# nvidia-sim simulations

Failure and recovery simulations for the
[nvidia-sim environment](../../environments/nvidia-sim/README.md) (Mokka mock
GPUs + GPU Operator + Dynamo mocker graph), run by a Kubernetes controller
with a `Simulation` CRD and an HTTP API. Test runners inject a fault, observe
how the stack behaves, and get a verified recovery, locally or on remote
clusters through a central hub.

- **What can be simulated:** [CATALOG.md](CATALOG.md) (25 types, generated from the code). The same controller also serves the [runai-sim](../runai-sim/README.md) types
- **How it works:** [docs/architecture.md](docs/architecture.md)
- **API reference:** [docs/api.md](docs/api.md)

## Simulations at a glance

| Area | Types | Mechanism |
|---|---|---|
| GPU failures | `gpu-xid`, `gpu-lost`, `gpu-fallen-off-bus`, `gpu-ecc-uncorrectable` | Mokka `nvml-mock-ctl fail` (NVML errors + Xid events) |
| GPU health / telemetry | `gpu-overheat`, `gpu-power-throttle`, `gpu-throttle`, `gpu-clocks-degraded`, `gpu-utilization`, `gpu-memory-pressure` | Mokka overrides read by nvidia-smi, device plugin, GFD, dcgm-exporter |
| GPU interconnect | `gpu-nvlink-errors`, `gpu-sram-ecc`, `gpu-fabric-degraded` | Mokka NVLink / SRAM ECC / fabric health |
| GPU escape hatch | `gpu-set` | any Mokka config field |
| GPU stack | `gpu-device-plugin-restart` | kill the device plugin |
| Dynamo | `dynamo-worker-kill`, `dynamo-frontend-kill`, `dynamo-worker-isolate`, `dynamo-frontend-isolate`, `dynamo-operator-restart` | pod kill / NetworkPolicy partition (cf. Dynamo's fault-tolerance tests) |
| Kubernetes | `pod-kill`, `network-isolate`, `node-cordon` | generic |
| Run:ai (runai-sim) | `kwok-node-down`, `fgo-gpu-utilization` | KWOK heartbeat / fake-gpu-operator annotations |

GPU simulations can target **the GPU a pod holds**: with
`--graph mocker-agg --component decode` the controller asks the worker
which GPU UUID it has and maps it to the device index on its node. Only that
GPU is faulted.

Each Simulation goes through these phases:
`Pending → (Waiting) → Active → Recovering → Recovered` (or `Failed`).
Recovery happens after `duration`, when you ask for it, or when the
Simulation is deleted (a finalizer reverts the fault first). `Recovered` means
**verified**: the GPU answers again, or the killed pods were replaced and are
Ready. `status.data.timeToRecover` records how long that took.

## Deploy

The controller image is built by `.github/workflows/simulator.yaml` and
pushed to `ghcr.io/benmaguire/onit-ai-testenv/simulator`. Push a tag
`simulator-v0.1.0` to publish `:0.1.0` (the tag the manifests use). Pushes to
`main` publish `:latest` and `:sha-…`.

**1. Create the API token** (every mode needs one):

```bash
kubectl create namespace sim-system
kubectl -n sim-system create secret generic simulator-api-token \
  --from-literal=token="$(openssl rand -hex 32)"
```

**2a. With Argo CD (as part of nvidia-sim):** set
`components.simulator.enabled: true` in
[environments/nvidia-sim/values.yaml](../../environments/nvidia-sim/values.yaml).
It deploys [deploy/overlays/agent](deploy/overlays/agent) as the child
Application `nvidia-sim-simulator`. Choose another overlay with
`components.simulator.path`.

**2b. By hand:**

```bash
kubectl apply --server-side -k simulations/nvidia-sim/deploy/overlays/agent
```

| Overlay | Use |
|---|---|
| [`agent`](deploy/overlays/agent) | This cluster only. API on ClusterIP `testenv-simulator.sim-system:8443`; use port-forward or in-cluster runners. |
| [`agent-remote`](deploy/overlays/agent-remote) | A cluster managed by a hub. TLS on NodePort 30843; self-registers with the hub. Edit names/URLs. |
| [`hub`](deploy/overlays/hub) | Central controller: an agent for its own cluster plus the registry and router for remote ones. TLS on NodePort 30843. |

## Use it

```bash
kubectl -n sim-system port-forward svc/testenv-simulator 8443 &
export SIMCTL_SERVER=http://localhost:8443
export SIMCTL_TOKEN=$(kubectl -n sim-system get secret simulator-api-token -o jsonpath='{.data.token}' | base64 -d)

simctl catalog                       # what can be simulated
simctl catalog gpu-xid               # params, effect, recovery
simctl inject gpu-xid --graph mocker-agg --component decode \
  --param xid=79 --param mode=fallen_off_bus --duration 2m --label testrun=demo --wait recovered
simctl inject dynamo-worker-kill --wait recovered      # status shows timeToRecover
simctl inject gpu-overheat --gpu 0 --param temperature=97   # held until:
simctl recover <name> --wait
simctl list --selector testrun=demo
simctl recover-all                   # emergency stop
```

`simctl` is in the image (`/usr/local/bin/simctl`) or `make build`
(`bin/simctl`). The same works with `kubectl`:

```bash
kubectl apply -f simulations/nvidia-sim/examples/worker-kill.yaml
kubectl -n sim-system get sims -w
kubectl -n sim-system patch sim overheat-held --type merge -p '{"spec":{"recover":true}}'
```

From Go test code (see [pkg/client](pkg/client/client.go)):

```go
c := client.New(os.Getenv("SIM_SERVER"), os.Getenv("SIM_TOKEN"))
c.Cluster = "lab-2" // optional: through a hub
sim, err := c.Create(ctx, client.CreateRequest{Labels: map[string]string{"testrun": runID},
    Spec: simv1.SimulationSpec{Type: "dynamo-worker-kill", Target: simv1.Target{Count: 1}}})
sim, err = c.WaitFor(ctx, sim.Namespace, sim.Name, simv1.PhaseRecovered)
```

[examples/failover-test.sh](examples/failover-test.sh) is a complete
test-runner flow: baseline, fault, observe, recover, re-check, with a trap
that recovers everything if the test aborts.

## Multi-cluster: hub and remote agents

1. **Hub** (central cluster): create the Secrets `simulator-api-token`, `simulator-registration-token` and
   `simulator-tls` (`kubectl -n sim-system create secret tls simulator-tls --cert … --key …`), then
   apply `deploy/overlays/hub`.
2. **Each remote cluster**: create `simulator-api-token`, `simulator-tls`
   and `simulator-hub-registration` (holding the hub's *registration* token),
   set the cluster name / hub URL / advertise URL in
   `deploy/overlays/agent-remote`, and apply it. The agent registers every
   minute. The hub stores its token as a Secret and probes it every 30s.
   Agents the hub cannot be reached *from* can instead be added statically
   with a [SimulationCluster](examples/simulationcluster.yaml).
3. **Runners** talk only to the hub:

```bash
export SIMCTL_SERVER=https://sim-hub.example.internal:30843 SIMCTL_TOKEN=<hub api token>
simctl clusters                                   # local + registered agents, reachability
simctl --cluster lab-2 inject dynamo-worker-kill --wait recovered
simctl fleet --selector testrun=nightly           # everything, everywhere
simctl recover-all --fleet                        # emergency stop on every cluster
```

The hub's own cluster is reachable as `--cluster central` (its
`SIM_CLUSTER_NAME`). Tokens: runners use the **hub API token**; agents use
the **registration token**, which can only register. The hub uses each
**agent's API token** to call it.

## Safety

- One fault per Simulation, with the exact targets recorded in `status.targets`. Recovery undoes only those.
- Finalizer: deleting a Simulation, or removing the env through Argo CD, reverts its fault before the object goes.
- `recover-all` (per cluster or `--fleet`) for an emergency stop. Use a `testrun` label and a shell `trap` in runners.
- Simulations are processed one at a time (`nvml-mock-ctl` edits a shared per-node file).
- **Overlapping GPU simulations:** types whose recovery is `nvml-mock-ctl reset --gpu` (overheat, power, clocks,
  utilization, memory, gpu-set) clear *every* override on that GPU, including another active simulation's.
  Don't overlap them on the same GPU.
- **Argo CD self-heal** reverts manual changes to Argo-managed objects. Faults here only delete pods, add
  NetworkPolicies or change node/GPU state. None of these is an Argo-managed spec, so Argo CD doesn't undo them.
- Kernel-log Xids need `nodeAgent.kernelLog.enabled` on Mokka (privileged; see the environment values).
  Without it, Xids are delivered through NVML events only.

## Development

```bash
make test        # vet + unit tests (fake clients, fake pod exec, real HTTP for hub routing)
make all         # regenerate deepcopy, CRDs, RBAC, CATALOG.md; build; test
make run-agent   # run against your current kubeconfig, no auth
make docker-build IMG=...
```

```text
api/v1alpha1/        Simulation, SimulationCluster types (+ generated deepcopy)
cmd/simulator/       controller + API binary (--mode=agent|hub)
cmd/simctl/          CLI for test runners
internal/inject/     simulation types: gpu.go (Mokka), k8s.go (pods, network, nodes)
internal/controller/ Simulation reconciler (phases, finalizer, TTL)
internal/api/        HTTP API, hub registry/router/fleet, agent registrar
internal/kube/       pod exec
pkg/client/          Go client + API types
pkg/catalog/         catalog entry types
hack/gencatalog/     generates CATALOG.md
deploy/              CRDs, RBAC, kustomize base + overlays (agent, agent-remote, hub)
examples/            Simulation YAMLs, SimulationCluster, test-runner script
```

## Limits

- Nothing here has been run against a live cluster yet. The unit tests use
  fake Kubernetes clients and a fake `nvml-mock-ctl`, so test the first
  deployment end to end.
- Mokka simulates what software *reads* (NVML, nvidia-smi, DCGM). It cannot
  simulate CUDA or performance, and the Dynamo mocker does not use the GPU, so a
  GPU fault reaches Dynamo indirectly: through the device plugin, health
  checks, and the pod restarts or evictions your remediation triggers.
- `gpu-nvlink-errors` / `gpu-fabric-degraded` need an NVLink-capable Mokka
  profile (`gpu.profile`), and fabric health needs gb200/gb300.
