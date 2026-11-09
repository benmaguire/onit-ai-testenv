# runai-sim simulations

Failure and recovery simulations for the
[runai-sim environment](../../environments/runai-sim/README.md): KWOK virtual
GPU nodes (Run:ai fake-gpu-operator) scheduled by KAI Scheduler.

They run on the same controller as nvidia-sim
([simulations/controller](../controller/README.md), same API, `simctl`, hub and
agents). Enable it with `components.simulator.enabled: true` in
`environments/runai-sim/values.yaml`, **or** in nvidia-sim's: only one per
cluster. Full parameters for each type are in the [catalog](../controller/CATALOG.md#runai-runai-sim).

## Available

| Type | What happens | What it tests |
|---|---|---|
| [`kwok-node-down`](../controller/CATALOG.md#kwok-node-down) | The virtual node's KWOK heartbeat stops: NotReady after ~40s, unreachable taint, pods evicted after 300s by default | GPU node loss: capacity drops out of queues, KAI reschedules evicted workloads, gang jobs restart together, alerting on node loss |
| [`fgo-gpu-utilization`](../controller/CATALOG.md#fgo-gpu-utilization) | FGO's synthetic GPU utilization for the matched pods changes (e.g. `0`, `95-100`) | idle-GPU detection/reclaim, consolidation, utilization dashboards, autoscaling on GPU metrics |
| [`node-cordon`](../controller/CATALOG.md#node-cordon) | Virtual nodes become unschedulable (`--node-selector type=kwok`) | capacity shrink without eviction; pending workloads and queue fairness |
| [`pod-kill`](../controller/CATALOG.md#pod-kill) | Workload pods are deleted | workload restarts, gang re-admission (kill one pod of `distributed-job`) |
| [`network-isolate`](../controller/CATALOG.md#network-isolate) | NetworkPolicy deny for pods | of limited use: pods on virtual nodes don't really run. Useful against the real controllers (KAI, FGO) |

GPU types that go through Mokka (`gpu-*`) and `dynamo-*` types target
nvidia-sim's Mokka node and Dynamo graph; they do not apply to virtual nodes.

## Examples

```bash
# Lose one h100 node for 10 minutes; watch KAI reschedule
simctl inject kwok-node-down --node-selector run.ai/simulated-gpu-node-pool=h100 --count 1 \
  --duration 10m --label testrun=node-loss --wait recovered
kubectl get pods -n runai-sim -o wide -w

# Make team-a's training look idle (idle-GPU reclaim policies)
simctl inject fgo-gpu-utilization --target-namespace runai-sim --pod-selector app=train-whole-gpu \
  --param range=0 --duration 30m

# Shrink the fleet: cordon both a100 nodes, then kill inference pods
simctl inject node-cordon --node-selector run.ai/simulated-gpu-node-pool=a100 --duration 15m
simctl inject pod-kill --target-namespace runai-sim --pod-selector app=inference --wait recovered
```

## Notes

- `kwok-node-down` only touches nodes annotated `kwok.x-k8s.io/node=fake`.
  Real nodes are refused even when named. Recovery is "node Ready again".
  Whether the evicted workloads came back is up to their controllers and KAI,
  so check that in your test.
- Argo CD manages the virtual Node objects but ignores the KWOK annotation,
  taints and `unschedulable` (see `environments/runai-sim/templates/app-nodes.yaml`),
  so these simulations aren't reverted by self-heal.

## Planned

- **Queue quota / limit changes** (shrink a project's quota mid-run). The sample
  queues are Argo-managed, so that needs queues outside Argo CD or an
  ignoreDifferences decision first.
- **GPU XID / health on virtual nodes**: FGO marks GPUs by topology ConfigMap
  status; driving that needs a closer look at FGO's status API.
