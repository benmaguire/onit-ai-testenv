# nvidia-dynamo-disagg simulations

For the [nvidia-dynamo-disagg environment](../../environments/nvidia-dynamo-disagg/README.md):
a disaggregated Dynamo graph `mocker-disagg` (frontend with KV router and
request migration, 1 prefill + 2 decode workers on mock GPUs) under continuous
load from `loadgen`.

Use `--observe dynamo --observe kubernetes`. The loadgen means request rate,
errors and latency always have a baseline.

| Scenario | Simulation | What to look at |
|---|---|---|
| Decode worker dies mid-generation | `dynamo-worker-kill --target-namespace nvidia-dynamo-disagg --graph mocker-disagg --component decode --count 1` | `dynamo_errors_per_s` should stay ~0 with `DYN_MIGRATION_LIMIT=3` (requests migrate); `timeToRecover` |
| Prefill worker dies | `pod-kill --target-namespace nvidia-dynamo-disagg --pod-selector nvidia.com/dynamo-component-type=prefill` | TTFT p95 during the gap; errors if no prefill is available |
| Slow prefill → decode KV transfer | `net-latency --pod-selector nvidia.com/dynamo-component-type=prefill --param delay=50ms --param jitter=20ms` | TTFT p95 rises, ITL steady |
| Lossy link to one decode worker | `net-packet-loss --pod-selector nvidia.com/dynamo-component-type=decode --count 1 --param loss=5` | KV router should shift load; ITL p95 |
| Frontend partitioned | `dynamo-frontend-isolate --target-namespace nvidia-dynamo-disagg --graph mocker-disagg --param direction=egress` | all requests fail; recovery time |
| GPU under a decode worker lost | `gpu-lost --target-namespace nvidia-dynamo-disagg --graph mocker-disagg --component decode --count 1` | device plugin marks GPU unhealthy; worker restarts / reschedules |
| CPU-starved frontend | `cpu-stress --target-namespace nvidia-dynamo-disagg --pod-selector nvidia.com/dynamo-component-type=frontend --param workers=2` | request latency p95 |

```bash
simctl inject dynamo-worker-kill --target-namespace nvidia-dynamo-disagg --graph mocker-disagg \
  --component decode --count 1 --observe dynamo --observe kubernetes --baseline 2m --after 2m \
  --probe-http models=http://mocker-disagg-frontend.nvidia-dynamo-disagg.svc:8000/v1/models \
  --label testrun=disagg-1 --wait observed
simctl metrics <name> > disagg-1.csv
```

Note: `dynamo-*` types default to the nvidia-sim graph. Always pass
`--target-namespace nvidia-dynamo-disagg --graph mocker-disagg` here.
