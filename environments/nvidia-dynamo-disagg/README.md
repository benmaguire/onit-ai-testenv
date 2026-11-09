# nvidia-dynamo-disagg

**Disaggregated LLM serving** with NVIDIA Dynamo on mock GPUs: a frontend
with **KV-cache-aware routing** and **request migration**, 1 **prefill**
worker and 2 **decode** workers running Dynamo's mocker engine, under
continuous load. It's the production-style topology Dynamo targets
(separate prefill and decode pools), without GPUs.

Follows NVIDIA's [mocker on Kubernetes](https://docs.nvidia.com/dynamo/knowledge-base/concepts/simulation/kubernetes-mocker)
disaggregated example (`examples/backends/mocker/deploy/disagg.yaml`, v1.6.1).

## Requires nvidia-sim

This environment deploys only the graph and the load. It uses the
[nvidia-sim](../nvidia-sim/README.md) platform on the same cluster: Mokka mock
GPUs, the GPU Operator device plugin, and the Dynamo operator (which watches
all namespaces). nvidia-sim's `h100` profile has 8 mock GPUs. nvidia-sim's graph uses
1, and this one uses 3.

## What it deploys

| Resource | Details |
|---|---|
| `DynamoGraphDeployment` `mocker-disagg` | Frontend (`DYN_ROUTER_MODE=kv`, `DYN_MIGRATION_LIMIT=3`), `prefill` × 1 and `decode` × 2 (`dynamo.mocker --disaggregation-mode`, `Qwen/Qwen3-0.6B`, 1 mock GPU each) |
| Deployment `loadgen` | 4 concurrent clients sending chat completions with a shared system prompt (so the KV router has prefixes to reuse) |
| Namespace `nvidia-dynamo-disagg` | managed, so removed with the environment |

Endpoint: `http://mocker-disagg-frontend.nvidia-dynamo-disagg.svc:8000/v1/...`

## Lifecycle

Starts disabled: `scripts/envctl.sh up nvidia-dynamo-disagg --push` (after nvidia-sim).

| | What runs |
|---|---|
| `state: up` | graph + loadgen |
| `state: down` | all components and loadgen at 0 replicas (DGD kept) |
| `enabled: "false"` | namespace and everything in it deleted |

## Configure

[values.yaml](values.yaml): replicas per role, `frontend.routerMode`
(`kv` / `round-robin` / …), `frontend.migrationLimit`, `speedupRatio`,
`extraArgs` per role (any [mocker flag](https://docs.nvidia.com/dynamo/reference/components/mocker-cli-reference),
e.g. `--num-gpu-blocks-override`), and `loadgen.concurrency` / `maxTokens`.

## Verify

```bash
kubectl -n nvidia-dynamo-disagg get dgd,pods
kubectl -n nvidia-dynamo-disagg logs deploy/loadgen --tail=20      # silent = all HTTP 200
kubectl -n nvidia-dynamo-disagg port-forward svc/mocker-disagg-frontend 8000 &
curl -s localhost:8000/v1/models
curl -s localhost:8000/metrics | grep dynamo_frontend_requests_total
```

## Simulations

[simulations/nvidia-dynamo-disagg](../../simulations/nvidia-dynamo-disagg/README.md):
worker kills with request migration, slow KV transfer between prefill and
decode, lossy links to one decode worker, GPU loss, frontend partition. Use
`--observe dynamo`.
