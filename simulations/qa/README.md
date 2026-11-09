# qa simulations

The [qa environment](../../environments/qa/README.md) is only a namespace
for now, so it has **no environment-specific simulations**.

When QA workloads are added, the generic types served by the simulation
controller can be pointed at them. To use them, deploy the controller (agent
overlay) on the cluster: [../nvidia-sim/README.md#deploy](../nvidia-sim/README.md#deploy).
GPU and `dynamo-*` types need the nvidia-sim stack (Mokka, Dynamo); elsewhere
they end `Failed` at injection because their targets do not exist.

| Type | Use in QA |
|---|---|
| [`pod-kill`](../nvidia-sim/CATALOG.md#pod-kill) | kill QA service pods: `--target-namespace qa --pod-selector app=<svc>` |
| [`network-isolate`](../nvidia-sim/CATALOG.md#network-isolate) | partition a QA service (ingress, egress or both) |
| [`node-cordon`](../nvidia-sim/CATALOG.md#node-cordon) | make rescheduling impossible during a kill |

```bash
simctl inject pod-kill --target-namespace qa --pod-selector app=api --count 1 --wait recovered
```

## Planned

List QA-specific scenarios here as QA workloads land (e.g. dependency
outages, slow responses), with the Injector that implements each one.
