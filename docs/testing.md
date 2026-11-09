# What has been tested

A record of what has actually been run, against what, and what hasn't been
run yet. Update it when you validate something new.

## 2026-11-09: local run on a Mac (k3d)

**Setup:** macOS 26.4 on Apple Silicon (arm64), Docker Desktop 28.0.1 (14 CPU,
7.7 GiB for the VM), k3d v5.8.3 (k3s v1.31.5), kubectl v1.32.3, Helm v4.3.0,
yq v4.54.3. Cluster `testenv` was created by `make cluster-create CLUSTER=k3d`.
Nothing was installed through Homebrew for this run; yq was a standalone binary.

### Passed end to end

| Area | What was run | Result |
|---|---|---|
| Cluster creation | `make cluster-create CLUSTER=k3d` | k3d cluster `testenv` created, context switched to `k3d-testenv` |
| Preflight | `make check ENV=nvidia-triton-cpu` | all checks passed: tools, local context, cluster type, Kubernetes version, arm64, 14 CPU / 7.7 GiB vs ~3 CPU / 6 GiB, disk note |
| Environment deploy | `make up ENV=nvidia-triton-cpu` (no Argo CD, from the working tree) | 7 objects applied, readiness waited for. Triton 26.10-py3 (arm64) pulled and started, both models `READY` (`add_sub`, `simulated_llm`) |
| Inference | `simulated_llm` over HTTP from the loadgen pod | correct response (6 tokens) |
| Load and metrics | loadgen + Triton `:8002/metrics` | ~345 successful requests per model in the first minutes, no loadgen errors after Triton was ready |
| Autoscaling | HPA on Triton | reads CPU (2% of the 70% target), 2 replicas |
| Simulator image | `make simulator` | built with Docker for arm64, imported into k3d, deployed, leader lease acquired |
| In-pod simctl | `make sim-catalog` | all 42 simulation types listed |
| **Fault injection + observation** | `make sim-suite ENV=nvidia-triton-cpu ONLY=latency`: `net-latency` 100ms ± 20ms on both Triton pods, 2m fault, 1m baseline, 1m after, HTTP probe on `/v2/health/ready` | `Baselining → Active → Recovering → Recovered`. The ephemeral container installed `netem delay 100ms 20ms` (confirmed in its logs). Probe p50: **0.55 ms baseline → 103 ms fault → 0.43 ms after**, 0 failures out of 121 probes, `timeToRecover 2m2s`, recovery verified in 2 s. Report, JSON and CSV (121 probe samples tagged by window) saved under `reports/` |
| Revert on delete | `kubectl delete sims --all` | finalizers ran; health check back to 5 ms (from ~205 ms during the fault) |

Before this run, all of the following were already checked with unit tests and
dry runs: Helm lint of every chart; `make render` for all 7 environments on kind
and k3d; preflight failure paths (missing tools, no kubeconfig, unreachable
context with Docker stopped, remote API servers, unknown or missing `ENV`); suite
arguments against `simctl`'s flag parser; ShellCheck and macOS bash 3.2; the KNE
topology against KNE's own proto; and a static `kne_cli` build for linux/amd64.

### Bugs found by this run (fixed in code; the fixes have unit tests but have not been re-run live)

| Bug | Fix |
|---|---|
| `container-kill` with `SIGKILL` never restarted Triton (`restartCount 0`): the kernel ignores SIGKILL sent to a PID namespace's init from inside that namespace | Default signal is now `TERM` (also `INT`, `QUIT`, `HUP`), and `KILL` is rejected with an explanation. For abrupt kills use `pod-kill` |
| One-shot faults (`pod-kill`, `container-kill`, `dynamo-*-kill`, …) without `--duration` stayed `Active` until recovery was requested, so suites hung | One-shot types without a duration go straight to verifying recovery |
| `kubectl get sims -o wide` showed `<invalid>` for `RECOVER-AT` (future dates in a `date` column) | Column type changed to string |
| A configured probe with no samples yet was left out of the report (also made a unit test flaky) | Every configured probe is always reported |

### Not exercised by this run

- **Metrics through Prometheus:** the observability environment wasn't deployed, so the report's
  Prometheus metrics were errors as expected. Only the probes were measured.
- **Other environments:** nvidia-sim, nvidia-dynamo-disagg, runai-sim, observability, qa and network-kne.
  The suite's `crash-restart` was stopped and the other entries weren't run.
- **Other cluster types:** kind and k3s (Colima), and the interactive install prompts (Homebrew, Colima).
- **The GitOps path:** Argo CD and the ApplicationSet, and the hub / remote-agent mode.
- **network-kne:** cannot run on Apple Silicon (XRd and meshnet are x86_64-only). Needs the Cisco XRd image.
- **XRd crash:** whether `container-kill` restarts an XRd router depends on which signal XRd's PID 1 exits on.

### Left running

The k3d cluster `testenv` with `nvidia-triton-cpu` and the simulation controller.
The kubectl context is `k3d-testenv` (it was `k3d-devcluster` before).

```bash
make down ENV=nvidia-triton-cpu && make simulator-delete   # remove the environment and controller
make cluster-delete CLUSTER=k3d                            # remove the cluster
kubectl config use-context k3d-devcluster                  # previous context
```
