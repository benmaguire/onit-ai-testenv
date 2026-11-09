# nvidia-sim simulations

For the [nvidia-sim environment](../../environments/nvidia-sim/README.md):
Mokka mock GPUs, GPU Operator, and a Dynamo mocker graph (`mocker-agg` in
namespace `nvidia-sim`).

| Area | Types | Metrics profile |
|---|---|---|
| GPU failures (Mokka `nvml-mock-ctl`) | `gpu-xid`, `gpu-lost`, `gpu-fallen-off-bus`, `gpu-ecc-uncorrectable` | `gpu`, `kubernetes` |
| GPU health / telemetry | `gpu-overheat`, `gpu-power-throttle`, `gpu-throttle`, `gpu-clocks-degraded`, `gpu-utilization`, `gpu-memory-pressure` | `gpu` |
| GPU interconnect | `gpu-nvlink-errors`, `gpu-sram-ecc`, `gpu-fabric-degraded`, `gpu-set` | `gpu` |
| GPU stack | `gpu-device-plugin-restart` | `gpu`, `kubernetes` |
| Dynamo | `dynamo-worker-kill`, `dynamo-frontend-kill`, `dynamo-worker-isolate`, `dynamo-frontend-isolate`, `dynamo-operator-restart` | `dynamo`, `kubernetes` |
| Generic | all [generic faults](../faults/README.md), e.g. `net-latency` on the decode worker, `cpu-stress` on the frontend | `dynamo`, `kubernetes` |

GPU faults can target the GPU a pod holds: `--graph mocker-agg --component decode`.
Full parameters are in the [catalog](../controller/CATALOG.md).

```bash
FE=http://mocker-agg-frontend.nvidia-sim.svc:8000/v1/models

# GPU falls off the bus under the worker, measured end to end
simctl inject gpu-fallen-off-bus --graph mocker-agg --component decode --duration 2m \
  --observe gpu --observe dynamo --observe kubernetes --baseline 2m --after 2m \
  --probe-http models=$FE --wait observed

# Worker killed: how long until the graph serves again?
simctl inject dynamo-worker-kill --baseline 1m --after 1m --probe-http models=$FE --wait observed

# Slow worker: 150ms extra latency on the decode worker's responses
simctl inject net-latency --target-namespace nvidia-sim \
  --pod-selector nvidia.com/dynamo-component-type=decode --param delay=150ms --duration 3m \
  --observe dynamo --baseline 2m --after 1m --wait observed
```

The environment runs no load generator, so Dynamo request metrics only move
with traffic. Use [nvidia-dynamo-disagg](../nvidia-dynamo-disagg/README.md)
(which has one), or send requests during the run. `gpu-*` faults reach Dynamo
only indirectly, because the mocker doesn't use the GPU (see the
[controller limits](../controller/README.md)).
