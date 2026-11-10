# nvidia-vllm

**vLLM** served in one of two ways, chosen with `frontend:` in [values.yaml](values.yaml):

| `frontend` | Request path | Routing decision |
|---|---|---|
| `inference-gateway` (default) | client → **Envoy Gateway** → `HTTPRoute` → **`InferencePool`** → vLLM pod | The **Endpoint Picker** (llm-d EPP) is called by Envoy over ext_proc for every request. It picks the pod from live vLLM metrics: queue depth, KV-cache use, prefix-cache affinity |
| `dynamo` | client → **Dynamo frontend** (OpenAI API) → Dynamo vLLM workers | Dynamo's **KV-aware router** (`--router-mode kv`) uses KV-cache events from the workers |

Both modes run without GPUs by default (`hardware: simulated`):
- **inference-gateway:** the [llm-d vLLM simulator](https://github.com/llm-d/llm-d-inference-sim)
  speaks vLLM's OpenAI API and exports vLLM's Prometheus metrics, with
  simulated TTFT, inter-token latency, KV cache and queueing. It needs no
  GPU, runs on amd64 and arm64, and needs no other environment.
- **dynamo:** Dynamo's mocker engine (needs nvidia-sim for the Dynamo operator).

`hardware: real` runs actual vLLM on NVIDIA GPUs instead: `vllm/vllm-openai` or
`dynamo.vllm`. See [docs/real-hardware.md](../../docs/real-hardware.md).

## What it deploys

### `frontend: inference-gateway`

| Component | Version | Details |
|---|---|---|
| `gateway-api-crds` | Envoy Gateway CRDs chart v1.10.0 | Gateway API (experimental channel) + Envoy Gateway CRDs, cluster-wide |
| `inference-crds` | GIE v1.6.2 + llm-d-router v1.0.0 | `InferencePool` (`inference.networking.k8s.io/v1`), `InferenceObjective` / `InferenceModelRewrite` (`llm-d.ai`) — [inference-crds/](inference-crds/) |
| `agent-router-crds`, `agent-router` | 1.2.1 | [Agent Router](https://theagentrouter.ai) (formerly Envoy AI Gateway): the Envoy Gateway extension server that wires the EPP into routes to an InferencePool. Namespace `envoy-ai-gateway-system` |
| `envoy-gateway` | v1.10.0 | Envoy Gateway with the extension-manager hooks, InferencePool as a backend resource, proxies in the Gateway's namespace |
| `model-servers` | llm-d-inference-sim v1.1.0 / vLLM v0.32.0 | Deployment `vllm-pool` (3 simulator pods, or 1 GPU pod) + Service `vllm-pool` :8000 — [vllm/](vllm/) |
| `endpoint-picker` | llm-d-router-gateway v1.0.0 | `InferencePool vllm-pool` (selects `app=vllm-pool`, port 8000) + EPP Deployment `vllm-pool-epp` (resources cut down for a laptop) |
| `gateway` | — | GatewayClass `testenv-inference-gateway`, EnvoyProxy (ClusterIP Service `inference-gateway`), Gateway `inference-gateway` :80, HTTPRoute `vllm-pool` → InferencePool, RBAC for Envoy Gateway to read InferencePools — [gateway/](gateway/) |

The versions follow Agent Router's v1.2.x compatibility matrix (Envoy Gateway
v1.9.2+, Gateway API v1.6, Inference Extension v1.6.2). The full EPP comes
from llm-d because, since GIE v1.6, the Kubernetes SIG repo ships only a
lightweight conformance EPP.

### `frontend: dynamo`

| Component | Details |
|---|---|
| `dynamo-graph` | `DynamoGraphDeployment vllm-dynamo`: `Frontend` (1, `dynamo.frontend --router-mode kv`, Service `vllm-dynamo-frontend:8000`) + `VllmWorker` (2 replicas: `dynamo.mocker` on a Mokka GPU, or `dynamo.vllm` on a real GPU) — [dynamo/](dynamo/) |

## Lifecycle

Starts disabled: `scripts/envctl.sh up nvidia-vllm --push`. Locally:

```bash
make up ENV=nvidia-vllm                                   # Inference Gateway + simulator (any cluster, any arch)
make up ENV=nvidia-vllm FRONTEND=dynamo CLUSTER=kind      # deploys nvidia-sim first
make up ENV=nvidia-vllm HARDWARE=real ALLOW_REMOTE=1      # real vLLM on a GPU cluster
```

| | What runs |
|---|---|
| `state: up` | everything for the selected frontend |
| `state: down` | model servers / workers / Envoy proxy at 0; controllers keep running |
| `enabled: "false"` | everything deleted; the Gateway API / Inference CRDs stay unless `make down PURGE=1` |

> **k3s:** k3s's Traefik may already install Gateway API CRDs. Both can manage
> them with server-side apply, or you can set `components.gatewayApiCrds.enabled: false`
> and keep k3s's, provided its Gateway API version supports `Gateway.spec.infrastructure.parametersRef`.

## Verify

```bash
# inference-gateway
kubectl -n nvidia-vllm get gateway,httproute,inferencepool,pods
kubectl -n nvidia-vllm port-forward svc/inference-gateway 8080:80 &
curl -s localhost:8080/v1/models | jq '.data[].id'
curl -s localhost:8080/v1/chat/completions -H 'Content-Type: application/json' \
  -d '{"model":"testenv/qwen3-0.6b-sim","messages":[{"role":"user","content":"hi"}],"max_tokens":32}'
# Scheduling decisions:
kubectl -n nvidia-vllm logs deploy/vllm-pool-epp | tail

# dynamo
kubectl -n nvidia-vllm get dynamographdeployment,pods
kubectl -n nvidia-vllm port-forward svc/vllm-dynamo-frontend 8000 &
curl -s localhost:8000/v1/chat/completions -H 'Content-Type: application/json' \
  -d '{"model":"Qwen/Qwen3-0.6B","messages":[{"role":"user","content":"hi"}],"max_tokens":32}'
```

With real hardware the inference-gateway model name is `Qwen/Qwen3-0.6B`
(`vllm.model`). To compare the routing approaches under the same load, deploy
the environment twice with each frontend and use the same client.

## Simulations

[simulations/nvidia-vllm](../../simulations/nvidia-vllm/README.md):
- killing or slowing one vLLM pod, to see whether the EPP routes around it;
- killing or isolating the EPP itself, to see how the gateway behaves without its picker;
- loading the Envoy proxy;
- the Dynamo equivalents: killing a worker or the frontend, or slowing a worker.

Use the `vllm` metrics profile (running/waiting requests, KV-cache use, TTFT
and ITL p95) with `--probe-http` through the Gateway.
