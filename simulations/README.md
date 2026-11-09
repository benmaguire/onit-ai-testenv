# Simulations

Failure and recovery simulations to run **during tests** against the
environments in [`../environments`](../environments), with measurements
before, during and after each fault. There's one directory per environment,
saying which simulations make sense there and how to run them. All of them run
on one controller.

| Directory | What |
|---|---|
| [controller/](controller/README.md) | The Go controller: `Simulation` CRD, HTTP API, `simctl`, Go client, hub + remote agents. [Catalog of all 42 types](controller/CATALOG.md) |
| [faults/](faults/README.md) | Generic faults for any workload: latency, jitter, packet loss, corruption, reordering, bandwidth, dependency / DNS / service outages, CPU / memory stress, container crashes |
| [controller/docs/observability.md](controller/docs/observability.md) | **How to monitor and extract metrics**: baseline / fault / recovery / after windows, Prometheus profiles, probes, reports, CSV export, Grafana |

| Environment | Simulations | Guide |
|---|---|---|
| `nvidia-sim` | GPU faults through Mokka (Xid, lost GPU, ECC, thermal, power, NVLink, fabric…), Dynamo worker/frontend/operator faults | [nvidia-sim/](nvidia-sim/README.md) |
| `nvidia-dynamo-disagg` | prefill/decode kills (request migration), KV-router and prefill/decode network impairment, GPU faults on workers | [nvidia-dynamo-disagg/](nvidia-dynamo-disagg/README.md) |
| `nvidia-triton-cpu` | inference server under latency/jitter/loss, CPU/memory pressure, replica loss vs PDB/HPA, crash restarts | [nvidia-triton-cpu/](nvidia-triton-cpu/README.md) |
| `runai-sim` | virtual GPU node failure, fake GPU utilization, cordon/kill on the virtual fleet | [runai-sim/](runai-sim/README.md) |
| `network-kne` | router link down / flap / latency / loss, router crash, measured with ping probes and gNMI counters | [network-kne/](network-kne/README.md) |
| `qa` | the generic faults once QA has workloads | [qa/](qa/README.md) |
| `observability` | none: it is what measures the others | [../environments/observability](../environments/observability/README.md) |

## How it works

```mermaid
flowchart LR
    runner["test runner<br/>(simctl / Go client / curl)"] -- HTTPS + token --> hub
    subgraph central[central cluster]
      hub["simulator --mode=hub"]
      prom1[(Prometheus)]
    end
    subgraph lab[test cluster]
      agent[simulator --mode=agent]
      prom2[(Prometheus)]
      pods[workloads / routers / GPUs]
    end
    hub -- "/api/v1/clusters/lab/..." --> agent
    agent -. self-register .-> hub
    agent -- inject / revert / verify --> pods
    agent -- probes --> pods
    agent -- baseline/fault/after queries --> prom2
```

1. A runner creates a `Simulation` (API, `simctl`, or `kubectl apply`), optionally with `observe`.
2. The controller measures a **baseline**, injects the fault, holds it, reverts it, and **verifies** recovery.
3. It measures the **after** window and stores a report: per-window stats and change against the baseline, plus `timeToRecover`.
4. The runner reads the report (`simctl report`) and raw data (`simctl metrics > run.csv`), and asserts on them.

## Adding simulations for an environment

1. Write `simulations/<env>/README.md`: what to simulate there, which targets,
   which metrics profile and probe show the effect.
2. If a new kind of fault is needed, add an `Injector` in
   `controller/internal/inject` and register it in `All()`. If it needs new
   metrics, add a profile in `controller/internal/observe/profiles.go` or the
   profiles ConfigMap.
3. Run `make -C simulations/controller all` so the catalog and generated files match.
