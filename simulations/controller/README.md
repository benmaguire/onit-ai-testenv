# Simulation controller

The Kubernetes controller and API that runs **failure and recovery
simulations** for every test environment in this repo. It provides a
`Simulation` CRD, an HTTP API, a `simctl` CLI and a Go client, plus a hub mode
that manages simulations on remote clusters.

- **What can be simulated:** [CATALOG.md](CATALOG.md) (42 types, generated from the code)
- **Monitoring / metrics / baselines:** [docs/observability.md](docs/observability.md)
- **Architecture:** [docs/architecture.md](docs/architecture.md) · **API:** [docs/api.md](docs/api.md)
- **Per-environment guides:** [../README.md](../README.md)

## Simulation types at a glance

| Category | Types | Mechanism |
|---|---|---|
| **Generic faults** (any workload) | `net-latency`, `net-jitter`, `net-packet-loss`, `net-corruption`, `net-duplication`, `net-reorder`, `net-bandwidth`, `net-degraded` | `tc netem` in the pod's network namespace, optionally only towards some destinations/ports |
| | `dependency-outage`, `dns-outage`, `service-outage` | `iptables` reject/drop in callers; NetworkPolicy in front of a Service |
| | `cpu-stress`, `memory-stress`, `container-kill` | load in the pod's cgroup; a signal the main process handles (TERM by default) for a restart in place |
| **Kubernetes** | `pod-kill`, `network-isolate`, `node-cordon` | API |
| **Network** (KNE routers) | `link-down`, `link-flap` (+ `net-*` with `--param interface=eth1`) | `ip link` on router interfaces |
| **GPU** (Mokka) | `gpu-xid`, `gpu-lost`, `gpu-fallen-off-bus`, `gpu-ecc-uncorrectable`, `gpu-overheat`, `gpu-power-throttle`, `gpu-throttle`, `gpu-clocks-degraded`, `gpu-utilization`, `gpu-memory-pressure`, `gpu-nvlink-errors`, `gpu-sram-ecc`, `gpu-fabric-degraded`, `gpu-set`, `gpu-device-plugin-restart` | `nvml-mock-ctl`, device plugin kill |
| **Dynamo** | `dynamo-worker-kill`, `dynamo-frontend-kill`, `dynamo-worker-isolate`, `dynamo-frontend-isolate`, `dynamo-operator-restart` | pod kill / NetworkPolicy |
| **Run:ai** (runai-sim) | `kwok-node-down`, `fgo-gpu-utilization` | KWOK heartbeat / fake-gpu-operator |
| **Measurement only** | `observe-only` | none: measures around your own action |

Every simulation goes `Pending → (Waiting) → (Baselining) → Active →
Recovering → Recovered` (or `Failed`). Recovery happens when `duration`
elapses, when you request it, or when the Simulation is deleted: a finalizer
reverts the fault first. `Recovered` means *verified*. With `observe`, the
controller also measures baseline / fault / recovery / after windows from
Prometheus and its own probes, and reports the change against the baseline.

## Deploy

The image is built by `.github/workflows/simulator.yaml` and published to
`ghcr.io/benmaguire/onit-ai-testenv/simulator`. Push the tag
`simulator-v0.1.0` for `:0.1.0`, the tag the manifests use.

```bash
kubectl create namespace sim-system
kubectl -n sim-system create secret generic simulator-api-token --from-literal=token="$(openssl rand -hex 32)"
```

Then either enable `components.simulator` in an environment's values (one
environment per cluster), or apply it directly:

```bash
kubectl apply --server-side -k simulations/controller/deploy/overlays/agent
```

| Overlay | Use |
|---|---|
| [`agent`](deploy/overlays/agent) | This cluster only (ClusterIP `testenv-simulator.sim-system:8443`) |
| [`agent-remote`](deploy/overlays/agent-remote) | A cluster managed by a hub: TLS NodePort 30843, self-registers |
| [`hub`](deploy/overlays/hub) | Central controller: agent for its own cluster plus registry/router for remote ones |

Notable flags (in `deploy/base/deployment.yaml`):

| Flag | Default | |
|---|---|---|
| `--prometheus-url` | `http://kube-prometheus-stack-prometheus.observability.svc:9090` | Prometheus for `observe`; empty = probes only |
| `--metrics-profiles-file` | ConfigMap `simulator-metrics-profiles` | extra/override metrics profiles |
| `--tools-image` | `nicolaka/netshoot:v0.16` | image for fault-injection ephemeral containers |
| `--mokka-namespace`, `--default-graph(-namespace)` | nvidia-sim defaults | GPU and Dynamo targeting |

## Use

```bash
kubectl -n sim-system port-forward svc/testenv-simulator 8443 &
export SIMCTL_SERVER=http://localhost:8443
export SIMCTL_TOKEN=$(kubectl -n sim-system get secret simulator-api-token -o jsonpath='{.data.token}' | base64 -d)

simctl catalog                                   # all types
simctl catalog net-jitter                        # params, effect, recovery
simctl profiles                                  # metrics profiles

# 100ms ± 40ms jitter on the Triton pods for 3 minutes, measured
simctl inject net-jitter --target-namespace nvidia-triton-cpu \
  --pod-selector app.kubernetes.io/name=triton-inference-server --duration 3m \
  --observe triton --observe kubernetes --baseline 2m --after 2m \
  --probe-http http://triton.nvidia-triton-cpu.svc:8000/v2/health/ready --wait observed

simctl list; simctl report <name>; simctl metrics <name> > run.csv
simctl recover-all                               # emergency stop
```

Go test code uses [pkg/client](pkg/client/client.go): `Create`, `WaitFor`,
`WaitObserved`, `Report`, `MetricsCSV`, `RecoverAll`, and fleet calls.
[examples/failover-test.sh](examples/failover-test.sh) shows a complete
test-runner flow.

## Multi-cluster

A hub (`deploy/overlays/hub`) runs the agent for its own cluster and routes
`/api/v1/clusters/<name>/...` to remote agents, which self-register
(`deploy/overlays/agent-remote`) or are added as `SimulationCluster` objects.
`simctl --cluster lab-2 ...` talks to a remote cluster through the hub.
`simctl fleet` and `simctl recover-all --fleet` act on every cluster. Reports
and exports are computed by the cluster that ran the simulation, against that
cluster's Prometheus. See [docs/architecture.md](docs/architecture.md).

## Safety

- One fault per Simulation. Its exact targets are recorded in `status.targets`, and recovery undoes only those.
- Finalizer: deleting a Simulation, or removing its environment, reverts the fault first.
- One `tc` fault and one link fault per pod at a time (enforced).
- `recover-all` (per cluster or `--fleet`) is the emergency stop. Label runs (`--label testrun=$RUN`) and `trap` in runners.
- Ephemeral containers stay in the pod spec after they exit, which is harmless. They need `NET_ADMIN` for
  network faults, which the restricted/baseline Pod Security profiles reject; label such namespaces
  `pod-security.kubernetes.io/enforce=privileged` for testing.
- `gpu-*` types that recover with `nvml-mock-ctl reset --gpu` clear every override on that GPU, so don't
  overlap them on one GPU.
- Network impairment is egress. To slow a service's responses, target the service's pods.

## Development

```bash
make test        # vet + unit tests
make all         # regenerate deepcopy, CRDs, RBAC, CATALOG.md; build; test
make run-agent   # against your current kubeconfig, no auth
```

```text
api/v1alpha1/        Simulation (+ observe), SimulationCluster
cmd/simulator/       controller + API (--mode=agent|hub)
cmd/simctl/          CLI
internal/inject/     simulation types: faults.go (generic), gpu.go (Mokka), k8s.go, runai.go, observeonly.go
internal/observe/    Prometheus client, profiles, probes, window summaries, export
internal/controller/ reconciler (phases, finalizer, baseline/after windows, TTL)
internal/api/        HTTP API, reports/exports, hub registry/router/fleet, registrar
internal/kube/       pod exec, ephemeral containers
pkg/client/          Go client + API types        pkg/catalog/  catalog types
hack/gencatalog/     generates CATALOG.md
deploy/              CRDs, RBAC, profiles ConfigMap, kustomize overlays
examples/            Simulation YAMLs, SimulationCluster, test-runner script
```

Limits: none of this has run against a live cluster yet. Unit tests use fake
clients, a fake `nvml-mock-ctl`, fake ephemeral containers and a fake
Prometheus, so test the first deployment end to end.
