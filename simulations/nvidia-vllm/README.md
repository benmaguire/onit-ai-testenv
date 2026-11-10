# nvidia-vllm simulations

For the [nvidia-vllm environment](../../environments/nvidia-vllm/README.md).
Entries in [suite.txt](suite.txt) are prefixed by frontend:

```bash
make sim-suite ENV=nvidia-vllm ONLY='^gw-'     # frontend: inference-gateway
make sim-suite ENV=nvidia-vllm ONLY='^dyn-'    # frontend: dynamo
```

| Question | Simulation | Watch |
|---|---|---|
| Does the EPP stop sending traffic to a dead pod? | `pod-kill` on `app=vllm-pool` (count 1) | probe success through the Gateway, `vllm_running` per pod |
| Does it route around a slow pod? | `net-latency` 300 ms on one `app=vllm-pool` pod | `vllm_ttft_p95`, probe latency, the slow pod's `vllm_waiting` |
| What happens without the picker? | `pod-kill` / `network-isolate` on `llm-d-router-gateway=vllm-pool-epp` | probe success: the gateway's fail-open/fail-closed behaviour |
| Proxy saturation | `cpu-stress` on the Envoy proxy pod | probe latency |
| Dynamo worker loss / slow worker / frontend loss | `dynamo-worker-kill`, `net-latency` (`--graph vllm-dynamo --component worker`), `dynamo-frontend-kill` | `dynamo` profile |

Metrics profiles: **`vllm`** (`vllm_running`, `vllm_waiting`,
`vllm_kv_cache_usage_max`, `vllm_success_per_s`, `vllm_ttft_p95`,
`vllm_itl_p95`; needs the `vllm` ServiceMonitor in observability), `dynamo`,
`kubernetes`, `probes`. The environment runs no load generator, so routing
differences only show up under traffic. Run a client (e.g. `vllm bench serve`
or `guidellm`) against the Gateway during the window.

The llm-d simulator can also inject its own failures. Set
`simulator.extraArgs` in `vllm/values.yaml` (e.g.
`[--failure-injection-rate, "10", --failure-types, rate_limit]`) to test client
retries.
