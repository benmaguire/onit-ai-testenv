# nvidia-dra-sim

GPUs allocated through **Kubernetes Dynamic Resource Allocation** (DRA,
`resource.k8s.io/v1`) instead of the device plugin. The NVIDIA DRA driver
publishes each GPU as a device in a `ResourceSlice`, and pods ask for GPUs
with `ResourceClaim`s, which can be whole GPUs or one shared between pods. Use
it to test DRA-aware schedulers, quota, claim lifecycle and GPU fault handling
on Mokka mock GPUs, or on real GPUs with `hardware: real`.

Requires **Kubernetes ≥ 1.34**, where DRA is GA (kind ≥ v0.30 node images, k3s ≥ v1.34).
It **can't run alongside nvidia-sim**, nvidia-dynamo-disagg or nvidia-nvsentinel,
for two reasons: the cluster has room for one Mokka, and the device plugin and
DRA would both hand out the same GPUs.

## What it deploys

| Component | Simulated (default) | Real (`hardware: real`) |
|---|---|---|
| `mokka` | nvml-mock 0.6.0, H100 profile, with the NRI plugin that injects the mock driver into claimed containers | — |
| `gpu-operator` | — | GPU Operator v26.7.2: toolkit (k3s containerd socket), CDI, DCGM; **device plugin off** |
| `dra-driver` | `dra-driver-nvidia-gpu` 0.5.1 with Mokka's driver root | `nvidia-dra-driver-gpu` 25.12.0, `nvidiaDriverRoot: /` (host driver) |
| `workloads` | `ResourceClaimTemplate single-gpu` and Deployment `gpu-claimer` (2 pods × 1 GPU each); `ResourceClaim shared-gpu` and Deployment `gpu-sharer` (2 pods sharing one GPU) | same |

ComputeDomains (multi-node NVLink, GB200/GB300) are disabled. Enable
`resources.computeDomains` on NVLink systems.

## Lifecycle

Starts disabled: `scripts/envctl.sh up nvidia-dra-sim --push`, or locally
`make up ENV=nvidia-dra-sim CLUSTER=kind` (k3d can't run Mokka; see
[nvidia-sim](../nvidia-sim/README.md)).

| | What runs |
|---|---|
| `state: up` | everything above |
| `state: down` | workloads at 0 replicas, so claims are released and GPUs return to the pool |
| `enabled: "false"` | namespaces and the driver deleted |

## Verify

```bash
kubectl get deviceclass                                  # gpu.nvidia.com (+ mig.nvidia.com on real MIG GPUs)
kubectl get resourceslices -o wide                       # one slice per node, a device per GPU
kubectl -n nvidia-dra-sim get resourceclaims             # allocated, reserved for pods
kubectl -n nvidia-dra-sim exec deploy/gpu-claimer -- nvidia-smi -L
kubectl -n nvidia-dra-sim exec deploy/gpu-sharer -- nvidia-smi -L   # same UUID in both sharer pods
```

## Simulations

[simulations/nvidia-dra-sim](../../simulations/nvidia-dra-sim/README.md):
GPU faults on the GPUs DRA allocated (`--pod-selector app=gpu-claimer`),
plus killing the claimers and the kubelet plugin.
