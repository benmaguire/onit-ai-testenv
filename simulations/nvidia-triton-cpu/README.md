# nvidia-triton-cpu simulations

For the [nvidia-triton-cpu environment](../../environments/nvidia-triton-cpu/README.md):
NVIDIA Triton Inference Server on CPU (`simulated_llm` and `add_sub` models)
behind Service `triton`, with an HPA (2–4 replicas), a PDB and a load
generator.

Use `--observe triton --observe kubernetes` with a probe on
`http://triton.nvidia-triton-cpu.svc:8000/v2/health/ready`.

| Scenario | Simulation | What to look at |
|---|---|---|
| Slow server (responses) | `net-latency --pod-selector app.kubernetes.io/name=triton-inference-server --param delay=100ms` | client-side probe latency; Triton's own request time stays flat (latency is outside the server) |
| Jittery / lossy network | `net-jitter`, `net-packet-loss --param loss=5` on the Triton pods | probe p95/p99, loadgen errors |
| Clients cut from server | `service-outage --param service=triton` | probe success → 0, recovery time |
| CPU contention | `cpu-stress --param workers=2 --param load=100` on Triton pods | `triton_queue_ms` and `triton_request_ms` rise; HPA scales out (`ready_pods`) |
| Memory pressure / OOM | `memory-stress --param size=3500Mi` (limit is 4Gi) | container restarts, readiness gaps |
| Crash and restart | `container-kill --count 1` | restart in place, model reload time (startup), PDB keeps one replica serving |
| Lose replicas | `pod-kill --count 1` | the other replica keeps serving; `timeToRecover` |
| Model repository / dependency unreachable | `dns-outage` on Triton pods | existing requests fine; anything resolving names fails |

```bash
simctl inject cpu-stress --target-namespace nvidia-triton-cpu \
  --pod-selector app.kubernetes.io/name=triton-inference-server --param workers=2 --duration 5m \
  --observe triton --observe kubernetes --baseline 2m --after 3m \
  --probe-http ready=http://triton.nvidia-triton-cpu.svc:8000/v2/health/ready --wait observed
```

The Triton image is large (≈20 GB), so pull it before timing-sensitive runs.
`pod-kill` and `container-kill` in a fresh node cache include that pull time.
