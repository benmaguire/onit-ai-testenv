# Deploying onto real NVIDIA hardware

Every NVIDIA environment runs on mock GPUs by default (`hardware: simulated`).
`hardware: real` deploys the same environment onto real NVIDIA GPUs: the
manifests are the same, with the simulated pieces swapped out. Mokka is
removed, the GPU Operator runs the real container toolkit/DCGM/device
plugin, and the engines become real (vLLM, Triton with GPU metrics, NIM).

## Which environments support it

| Environment | `simulated` | `real` |
|---|---|---|
| `nvidia-sim` | Mokka + GPU Operator v26.3.3 (sim) + Dynamo mocker | GPU Operator v26.7.2: toolkit (k3s containerd), CDI, DCGM + exporter, device plugin, GFD, MIG manager. Dynamo graph switches to `dynamo.vllm` |
| `nvidia-dynamo-disagg` | mocker prefill/decode | `dynamo.vllm` prefill + decode with NIXL KV transfer (`kv_producer` / `kv_consumer`), 1 GPU each |
| `nvidia-triton-cpu` | CPU only | GPU limit + `--allow-gpu-metrics` |
| `nvidia-dra-sim` | Mokka + `dra-driver-nvidia-gpu` | GPU Operator (device plugin **off**) + `nvidia-dra-driver-gpu` 25.12.0 |
| `nvidia-nvsentinel` | DCGM from Mokka | DCGM from the real GPU Operator |
| `nvidia-vllm` | llm-d vLLM simulator / Dynamo mocker | `vllm/vllm-openai` v0.32.0 / `dynamo.vllm` |
| `nvidia-nim` | — (real only) | NIM Operator + NIMCache/NIMService |
| `runai-sim` | — (simulated only: KWOK + fake-gpu-operator) | — |

## The node

These environments target a single-node k3s with NVIDIA GPUs:

1. **Driver on the host** (recommended): install the NVIDIA datacenter driver
   matching your GPU and check `nvidia-smi`. The GPU Operator runs with
   `driver.enabled: false`. To have the operator install the driver instead,
   set `driver.enabled: true` in `components.gpuOperator.real.values` (and for
   DRA, `nvidiaDriverRoot: /run/nvidia/driver`).
2. **k3s containerd:** the toolkit is pointed at k3s's socket
   (`CONTAINERD_SOCKET=/run/k3s/containerd/containerd.sock`). k3s picks up the
   `nvidia` runtime the toolkit writes. Other distributions: remove that env
   var (stock containerd), or follow the GPU Operator docs for your runtime.
3. **Kubernetes ≥ 1.34** for `nvidia-dra-sim` (DRA GA).
4. Enough **disk** for model images and caches: vLLM and Dynamo runtimes are
   10–20 GB, plus model weights (`vllm.cacheSize`, the NIM cache PVC).

## Switching

**GitOps:** set `hardware: real` in the environment's `env.yaml`, and in its
dependencies' (nvidia-sim), then push. The ApplicationSet passes it to the chart.

**Locally** (from the working tree, against the GPU cluster's kube context):

```bash
make check ENV=nvidia-vllm HARDWARE=real ALLOW_REMOTE=1      # preflight only
make up    ENV=nvidia-vllm HARDWARE=real ALLOW_REMOTE=1      # deploys nvidia-sim (real) first if needed
```

`HARDWARE` also applies to the dependencies it deploys. The preflight refuses
to deploy real hardware onto a local Mac cluster. It requires `ALLOW_REMOTE=1`
for a remote context, and checks that some node advertises an NVIDIA GPU
(`nvidia.com/gpu.present`, NFD's `pci-10de` label, or allocatable
`nvidia.com/gpu`). It also checks Kubernetes version minimums and environment
conflicts, and refuses DRA while a device plugin is running.

## Models and secrets

- vLLM / Dynamo default to `Qwen/Qwen3-0.6B` (ungated, fits any GPU). For gated
  models, create a Secret with `HF_TOKEN` and set `hfTokenSecret`.
- NIM needs the NGC secrets described in
  [environments/nvidia-nim](../environments/nvidia-nim/README.md#secrets-create-before-deploying).
- Secrets never go in git. Values refer to them by name only.

## Simulations on real hardware

Fault simulations that go through Mokka (`gpu-*`) need mock GPUs, so they
don't apply on real hardware. Everything else still works: the Kubernetes and
generic network/CPU/memory faults, `dynamo-*`, `ot-*`, `observe-only` with
baselines, and the probes. NVSentinel reacts to real DCGM health events.

## References

- [GPU Operator on k3s / RKE2](https://docs.nvidia.com/datacenter/cloud-native/gpu-operator/latest/getting-started.html)
- [NVIDIA DRA driver](https://github.com/NVIDIA/k8s-dra-driver-gpu)
- [Mokka (nvml-mock) guides](https://github.com/NVIDIA/k8s-test-infra)
- [NIM Operator](https://docs.nvidia.com/nim-operator/latest/)
- [Dynamo Kubernetes deployment](https://docs.nvidia.com/dynamo/)
