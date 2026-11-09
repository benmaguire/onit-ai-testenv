# Monitoring and metrics for simulations

Every simulation can be measured the same way: a **baseline** before the
fault, the **fault** window, the **recovery** window, and an **after** window
to check the system returned to normal. The controller collects the data
itself, so a test runner only asks for it and reads a report.

```text
 baseline            fault                 recovery        after
|-------------|=====================|~~~~~~~~~~~~~~~|-------------|
 healthy        fault injected        reverted,       back to normal?
 system         (duration / manual)   being verified
                ^ injectedAt          ^ recoveryStartedAt  ^ recoveredAt   ^ afterEnd
```

## Two sources of data

| Source | What | Needs |
|---|---|---|
| **Prometheus queries** (`observe.profiles`, `observe.queries`) | Anything Prometheus scrapes: pod CPU/memory/restarts, GPU (DCGM), Dynamo, Triton, KAI, router counters via gNMI, node metrics | the [observability environment](../../../environments/observability/README.md) (or any Prometheus: `--prometheus-url`) |
| **Probes** (`observe.probes`) | The controller itself sends HTTP requests, opens TCP connections, or runs a command in a pod (e.g. `ping`) every interval and records success and latency | nothing; probes also become Prometheus metrics (`sim_probe_*`) |

Probes are the user's view (is the endpoint up, how slow?). Prometheus is the
system's view (what did the components do?). Use both.

## Asking for it

```yaml
spec:
  type: net-latency
  target: {namespace: nvidia-triton-cpu, podSelector: {app.kubernetes.io/name: triton-inference-server}}
  params: {delay: 150ms, jitter: 30ms}
  duration: 3m
  observe:
    baseline: 2m          # probes run 2m before injecting; queries also look back 2m
    after: 2m             # keep measuring 2m after verified recovery
    profiles: [kubernetes, triton]
    queries:
      - name: loadgen_errors
        query: sum(rate(container_network_transmit_errors_total{namespace="{{.Namespace}}"}[{{.Rate}}]))
    probes:
      - name: infer
        type: http
        url: http://triton.nvidia-triton-cpu.svc:8000/v2/models/simulated_llm/ready
        interval: 2s
```

With `simctl`:

```bash
simctl inject net-latency --target-namespace nvidia-triton-cpu \
  --pod-selector app.kubernetes.io/name=triton-inference-server \
  --param delay=150ms --param jitter=30ms --duration 3m \
  --baseline 2m --after 2m --observe kubernetes --observe triton \
  --probe-http infer=http://triton.nvidia-triton-cpu.svc:8000/v2/models/simulated_llm/ready \
  --label testrun=$RUN --wait observed          # prints the report when done
```

- With probes and a baseline, the Simulation goes `Pending → Baselining → Active → ...`.
  Without probes, the baseline is a Prometheus look-back, so injection is immediate.
- The `Observed` condition and `status.observation.complete: true` are set once
  the after window has been measured. TTL deletion waits for that.
- Delete the Simulation and its probes stop; its `sim_*` series are removed.

## Reading results

**1. Report** (summary per window, with change vs baseline):

```bash
simctl report $NAME                 # table; live preview while it runs
simctl report $NAME --wait -o json  # for test assertions
```

Example output (values illustrative):

```text
METRIC              UNIT   BASELINE  FAULT    RECOVERY  AFTER   FAULT Δ%  AFTER Δ%
triton_request_ms   ms     46.2      198.4    51.0      46.9    329.4     1.5
triton_success_per_s req/s 38.1      12.6     36.9      38.0    -66.9     -0.3
PROBE infer  COUNT FAIL SUCCESS P50ms P95ms ... LONGEST-OUTAGE
  baseline   60    0    1       1.9   3.1
  fault      90    4    0.956   152   190         4s
```

The same data is in `kubectl get sim $NAME -o yaml` under
`status.observation`. `status.data.timeToRecover` is the time from injection
to verified healthy.

**2. Raw export** (every sample, every window, for notebooks or CI artefacts):

```bash
simctl metrics $NAME > $NAME.csv                 # columns: source,name,window,timestamp,value,ok,labels
curl -H "Authorization: Bearer $TOKEN" "$SERVER/api/v1/simulations/sim-system/$NAME/metrics"   # JSON
```

Probe samples are kept in the controller's memory until the Simulation is
deleted, so export before deleting (or rely on the `sim_probe_*` metrics in
Prometheus, which outlive the controller).

**3. Grafana**: the *Test environments: simulations* dashboard (observability
environment, `http://<node>:30300`, admin / `testenv`) shows active faults as
annotations, plus probes, Dynamo, Triton, GPU, Kubernetes and router panels.

## Built-in profiles

`simctl profiles` lists them with their queries. Templates get `{{.Namespace}}`,
`{{.Pods}}` (regex of the pods the simulation hit), `{{.Nodes}}`,
`{{.Simulation}}` and `{{.Rate}}` (1m).

| Profile | Measures |
|---|---|
| `kubernetes` | ready pods, restarts, target pods' CPU, CPU throttling, memory, network rx/tx |
| `gpu` | DCGM utilisation, temperature, power, framebuffer, last Xid, allocatable GPUs |
| `dynamo` | frontend request rate, error rate, latency / TTFT / ITL p95, in-flight |
| `triton` | success/failure rate, request and queue time |
| `kai` | pending/running pods, Ready virtual nodes |
| `network` | router interface packets/errors, established BGP sessions (gnmic) |
| `probes` | this simulation's probe success ratio and latency p95, from Prometheus |
| `node` | node CPU, available memory, NIC drops (from the `simulator-metrics-profiles` ConfigMap) |

Add your own in the `simulator-metrics-profiles` ConfigMap
(`deploy/base/metrics-profiles.yaml`); a profile with a built-in's name replaces it.
Queries should aggregate to one series (`sum`, `avg`, `max`); if several come
back, all their samples are summarised together.

## Measuring without a fault (`observe-only`)

`observe-only` injects nothing. It gives any action outside the controller
(a rollout, a model swap, a load test, a manual failover) the same baseline /
during / after report:

```bash
SIM=$(simctl inject observe-only --target-namespace nvidia-triton-cpu --observe triton \
        --probe-http http://triton.nvidia-triton-cpu.svc:8000/v2/health/ready --baseline 5m --after 5m)
simctl wait $SIM --for active          # baseline measured
kubectl -n nvidia-triton-cpu rollout restart deploy/triton   # your action
kubectl -n nvidia-triton-cpu rollout status deploy/triton
simctl recover $SIM                    # closes the "fault" window
simctl report $SIM --wait              # after window measured
```

It's also the way to record a plain baseline: run it with no action, then compare
its report with a later run's.

## Process for test runners

1. Pick the profile(s) for the component under test, and add a probe for the
   user-facing endpoint.
2. Inject with `--baseline` ≥ 2× your scrape interval (2m is a good default)
   and `--after` long enough to see the system settle.
3. Label every simulation with the run ID (`--label testrun=$RUN`).
4. Wait for `observed`, then assert on the JSON report: e.g. "probe success in
   the fault window ≥ 0.95", "AFTER Δ% of p95 latency < 10", "timeToRecover < 60s".
5. Save `simctl metrics $NAME` as a CI artefact.
6. `trap 'simctl recover-all --selector testrun=$RUN' EXIT` so an aborted run
   never leaves a fault behind.

Remote clusters: everything above works through the hub with `--cluster <name>`;
reports and exports are proxied to that cluster's controller, which queries that
cluster's Prometheus.
