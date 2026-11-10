# nvidia-nim

**NVIDIA NIM** microservices served by the
[NIM Operator](https://docs.nvidia.com/nim-operator/latest/). A `NIMCache`
downloads the model's optimized engine to a PVC, and a `NIMService` runs it
behind an OpenAI-compatible API. **Real hardware only:** NIMs run real
TensorRT-LLM/vLLM engines on real NVIDIA GPUs, and need an NGC account.

Depends on [nvidia-sim](../nvidia-sim/README.md) with `hardware: real`, which
supplies the GPU Operator. Any cluster that exposes `nvidia.com/gpu` works.
See [docs/real-hardware.md](../../docs/real-hardware.md).

## What it deploys

| Component | Details |
|---|---|
| `nim-operator` | `k8s-nim-operator` 3.1.4 |
| `nim` | Namespace `nvidia-nim`; `NIMCache llama-3-1-8b-instruct` (50 GiB PVC, default StorageClass); `NIMService llama-3-1-8b-instruct` (1 GPU, Service :8000) |

Pick the model in [nim/values.yaml](nim/values.yaml) (`model.image`, `tag`,
`gpus`, `profile`) from the [NGC NIM catalog](https://catalog.ngc.nvidia.com/containers?filters=nvidia_nim).
Choose a NIM and profile that fit your GPU's memory.

## Secrets (create before deploying)

```bash
kubectl create namespace nvidia-nim
kubectl -n nvidia-nim create secret docker-registry ngc-secret \
  --docker-server=nvcr.io --docker-username='$oauthtoken' --docker-password="$NGC_API_KEY"
kubectl -n nvidia-nim create secret generic ngc-api-secret --from-literal=NGC_API_KEY="$NGC_API_KEY"
```

They stay out of git. The chart only refers to them by name (`pullSecret`, `authSecret`).

## Lifecycle

Starts disabled, and the chart refuses to render unless `hardware: real`:
`scripts/envctl.sh up nvidia-nim --push`, or locally
`make up ENV=nvidia-nim HARDWARE=real ALLOW_REMOTE=1` against a GPU cluster.

| | What runs |
|---|---|
| `state: up` | NIMService at its replica count (the cache job runs first) |
| `state: down` | NIMService at 0 (the cache PVC is kept) |
| `enabled: "false"` | everything deleted, including the cache PVC |

## Verify

```bash
kubectl -n nvidia-nim get nimcache,nimservice,pods
kubectl -n nvidia-nim port-forward svc/llama-3-1-8b-instruct 8000 &
curl -s localhost:8000/v1/models | jq
curl -s localhost:8000/v1/chat/completions -H 'Content-Type: application/json' \
  -d '{"model":"meta/llama-3.1-8b-instruct","messages":[{"role":"user","content":"hi"}],"max_tokens":16}'
```
