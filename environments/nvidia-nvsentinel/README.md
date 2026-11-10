# nvidia-nvsentinel

[**NVSentinel**](https://github.com/NVIDIA/nvsentinel), NVIDIA's open-source GPU
health monitoring and remediation for Kubernetes, running on top of
[nvidia-sim](../nvidia-sim/README.md). When a GPU fault appears (an Xid, a
double-bit ECC error, overheating, a lost GPU), NVSentinel records a health
event, quarantines the node (cordon + taint) and drains GPU workloads. Inject
the faults with Mokka through the simulation controller and watch the whole
detect → quarantine → drain → recover loop on a laptop.

Needs nvidia-sim, which supplies Mokka, the GPU Operator and DCGM (DCGM is
enabled for this environment).

## What it deploys

| Component | Details |
|---|---|
| `cert-manager` | v1.19.1 (NVSentinel's internal TLS). Cluster-wide: set `components.certManager.enabled: false` if the cluster already has one |
| `nvsentinel` | Chart v1.21.0 (`oci://ghcr.io/nvidia/nvsentinel`): GPU health monitor (DCGM at `nvidia-dcgm.nvidia-sim-gpu-operator.svc:5555`), fault quarantine, node drainer, labeler, metadata collector, MongoDB store (Percona operator, a single 2 GiB replica) |
| `workload` | Deployment `gpu-workload` in `nvidia-nvsentinel-workload`, holding 1 GPU so drains have something to evict |

Settings for a single node, which differ from upstream:
- The drainer only evicts in `nvidia-nvsentinel-workload` and `nvidia-sim`,
  so MongoDB and NVSentinel itself survive their own node's quarantine.
- Remediation (reboot/reset via janitor) is off. On a laptop the "repair" is
  the simulation's recovery, which resets the mock GPU.

`hardware: real` keeps the same stack and reads DCGM from the real GPU
Operator (nvidia-sim with `hardware: real`).

## Lifecycle

Starts disabled: `scripts/envctl.sh up nvidia-nvsentinel --push` after
nvidia-sim, or locally `make up ENV=nvidia-nvsentinel CLUSTER=kind` (deploys
nvidia-sim first).

| | What runs |
|---|---|
| `state: up` | everything above |
| `state: down` | workload at 0; NVSentinel keeps running |
| `enabled: "false"` | NVSentinel, MongoDB, workload deleted (cert-manager too, if this env installed it) |

## Verify

```bash
kubectl -n nvidia-nvsentinel get pods                 # gpu-health-monitor, fault-quarantine, node-drainer, mongodb-rs0-0, ...
make sim TYPE=gpu-xid ARGS="--gpu 0 --param xid=79 --param mode=fallen_off_bus --duration 3m"
kubectl get nodes                                     # SchedulingDisabled while quarantined
kubectl get node -o jsonpath='{.items[0].spec.taints}'
kubectl -n nvidia-nvsentinel-workload get pods        # evicted, Pending until the node is healthy
```

## Simulations

[simulations/nvidia-nvsentinel](../../simulations/nvidia-nvsentinel/README.md):
Xid 79, double-bit ECC errors, overheating at 142 °C and a lost GPU. Each
should cordon the node and drain the workload. Measure it with
`--observe gpu --observe kubernetes`.
