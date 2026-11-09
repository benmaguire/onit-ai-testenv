# nvidia-triton-cpu

**NVIDIA Triton Inference Server** hosting models on **CPU-only** nodes:
real Triton (HTTP/gRPC/metrics, dynamic batching, model repository polling,
health endpoints) serving Python-backend models, autoscaled and under load.
Use it to test AI-hosting concerns like ingress, autoscaling, rollouts,
disruption budgets, client retries and monitoring, without GPUs. It's
standalone and needs no other environment.

## What it deploys

| Resource | Details |
|---|---|
| Deployment `triton` | `nvcr.io/nvidia/tritonserver:26.10-py3`, 2 replicas, `--model-control-mode=poll`, readiness `/v2/health/ready`, 500m–2 CPU / 1–4 GiB |
| Models (ConfigMap `triton-models`) | `simulated_llm`: LLM-shaped Python model (prompt + `MAX_TOKENS` → text), latency `40ms + 2ms × tokens` per batch, dynamic batching up to 8, 2 CPU instances. `add_sub`: Triton's canonical example |
| Service `triton` | 8000 HTTP, 8001 gRPC, 8002 metrics |
| HPA | 2–4 replicas at 70% CPU (k3s ships metrics-server) |
| PDB | `minAvailable: 1` |
| Deployment `loadgen` | 4 clients calling `simulated_llm` and `add_sub` continuously |

The models are plain files in [models/](models/). Edit them (or the latency
parameters in `values.yaml`), and the ConfigMap checksum rolls the Deployment.

## Lifecycle

Starts disabled: `scripts/envctl.sh up nvidia-triton-cpu --push`.

| | What runs |
|---|---|
| `state: up` | Triton (HPA-managed) + loadgen |
| `state: down` | Triton and loadgen at 0 (HPA removed) |
| `enabled: "false"` | namespace and everything in it deleted |

The image is ~20 GB. The first start takes a while, and the node needs the disk space.

## Verify

```bash
kubectl -n nvidia-triton-cpu get pods,hpa,pdb
kubectl -n nvidia-triton-cpu port-forward svc/triton 8000 8002 &
curl -s localhost:8000/v2/health/ready -o /dev/null -w '%{http_code}\n'     # 200
curl -s localhost:8000/v2/models/simulated_llm/infer -H 'Content-Type: application/json' -d \
  '{"inputs":[{"name":"PROMPT","shape":[1,1],"datatype":"BYTES","data":["hello"]},{"name":"MAX_TOKENS","shape":[1,1],"datatype":"INT32","data":[8]}]}'
curl -s localhost:8002/metrics | grep nv_inference_request_success
```

## Simulations

[simulations/nvidia-triton-cpu](../../simulations/nvidia-triton-cpu/README.md):
network latency/jitter/loss on the server, CPU contention (HPA scale-out),
memory pressure, crashes, replica loss vs PDB, and service outage. Use
`--observe triton --observe kubernetes`.
