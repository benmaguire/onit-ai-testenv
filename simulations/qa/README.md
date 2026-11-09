# qa simulations

The [qa environment](../../environments/qa/README.md) is only a namespace
for now, so it has **no environment-specific simulations**.

When QA workloads are added, the generic types served by the simulation
controller can be pointed at them. To use them, deploy the controller (agent
overlay) on the cluster: [../controller/README.md#deploy](../controller/README.md#deploy).
GPU and `dynamo-*` types need the nvidia-sim stack (Mokka, Dynamo); elsewhere
they end `Failed` at injection because their targets do not exist.

| Type | Use in QA |
|---|---|
| [`pod-kill`](../controller/CATALOG.md#pod-kill) | kill QA service pods: `--target-namespace qa --pod-selector app=<svc>` |
| [`network-isolate`](../controller/CATALOG.md#network-isolate) | partition a QA service (ingress, egress or both) |
| [`node-cordon`](../controller/CATALOG.md#node-cordon) | make rescheduling impossible during a kill |
| [generic faults](../faults/README.md) | latency, jitter, loss, dependency/DNS/service outages, CPU/memory stress against QA services |

```bash
simctl inject pod-kill --target-namespace qa --pod-selector app=api --count 1 --wait recovered
```

## Planned

List QA-specific scenarios here as QA workloads land (e.g. dependency
outages, slow responses), with the Injector that implements each one.
