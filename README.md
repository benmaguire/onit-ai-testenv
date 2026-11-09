# onit-ai-testenv

Deployment specs for **ephemeral test environments**, deployed and removed on
demand by [Argo CD](https://argo-cd.readthedocs.io/). Each environment is a
self-contained directory under [`environments/`](environments/) with its own
documentation, specs, and a small `env.yaml` of lifecycle switches.

| Environment | What it is | Docs |
|---|---|---|
| `qa` | A guarded namespace for QA workloads (placeholder for now) | [environments/qa](environments/qa/README.md) |
| `nvidia-sim` | Simulated NVIDIA GPU node ([Mokka](https://github.com/NVIDIA/k8s-test-infra)) + GPU Operator + [NVIDIA Dynamo](https://docs.nvidia.com/dynamo/) serving a mocker inference graph — no real GPUs | [environments/nvidia-sim](environments/nvidia-sim/README.md) |
| `nvidia-dynamo-disagg` | Dynamo **disaggregated** serving (prefill + 2 decode workers, KV-aware routing, request migration) on mock GPUs, under load. Needs nvidia-sim. Starts disabled | [environments/nvidia-dynamo-disagg](environments/nvidia-dynamo-disagg/README.md) |
| `nvidia-triton-cpu` | **NVIDIA Triton Inference Server** on CPU: LLM-shaped and example Python models, dynamic batching, HPA, PDB, load. Starts disabled | [environments/nvidia-triton-cpu](environments/nvidia-triton-cpu/README.md) |
| `network-kne` | **KNE network emulation**: 2 Cisco XRd routers (OSPF + iBGP) and 2 hosts, gNMI telemetry. Needs the XRd image. Starts disabled | [environments/network-kne](environments/network-kne/README.md) |
| `observability` | **Prometheus + Grafana** for all environments, and the metrics backend for simulation baselines. Starts disabled | [environments/observability](environments/observability/README.md) |
| `runai-sim` | Run:ai-style GPU scheduling on non-GPU hardware: KAI Scheduler (open-source Run:ai scheduler) + Run:ai fake-gpu-operator on KWOK virtual GPU nodes (40 simulated GPUs). Starts disabled | [environments/runai-sim](environments/runai-sim/README.md) |

Target cluster: a **single-node k3s** cluster with Argo CD installed.

Failure/recovery **simulations** to run during tests live in
[`simulations/`](simulations/README.md), one directory per environment. A
controller ([simulations/controller](simulations/controller/README.md):
`Simulation` CRD, HTTP API, `simctl`, central hub for remote clusters) injects
42 kinds of fault: network latency, jitter, loss and outages, resource stress,
GPU, Dynamo, Run:ai and router link faults. It measures every one against a
baseline ([monitoring process](simulations/controller/docs/observability.md)).

## Layout

```text
argocd/                         Bootstrap, applied once per cluster
  kustomization.yaml            kubectl apply -k argocd/
  project.yaml                  AppProject "testenvs"
  repositories.yaml             Public Helm repos (Mokka OCI, NGC)
  applicationset.yaml           One Application per environments/*/env.yaml
  argocd-cm-application-health.yaml   optional patch: ordered app-of-apps sync
environments/
  <env>/
    env.yaml                    lifecycle switches: enabled, state, revision
    README.md                   what the env is, how to use and test it
    Chart.yaml values.yaml templates/   the env's specs (a Helm chart)
    scripts/                    env-specific helpers (optional)
simulations/
  README.md                     simulations per environment, how they run
  <env>/README.md               what can be simulated in that env
  controller/                   Go controller (agent/hub), simctl, CRDs, catalog, observability
  faults/                       generic network/service/resource faults
scripts/
  envctl.sh                     list / up / down / remove / status / sync / purge
docs/
  architecture.md               how the pieces fit together
  lifecycle.md                  deploy, scale to zero, remove, re-deploy
  adding-an-environment.md      template for new environments
```

## Quick start

```bash
# 1. once per cluster: register the project, repos and ApplicationSet
kubectl apply -k argocd/

# 2. (recommended) let app-of-apps sync waves wait for health
kubectl -n argocd patch configmap argocd-cm --type merge \
  --patch-file argocd/argocd-cm-application-health.yaml

# 3. see what is declared
scripts/envctl.sh list
```

Every environment with `enabled: "true"` in its `env.yaml` on `main` is
deployed automatically. Day-to-day lifecycle:

```bash
scripts/envctl.sh down nvidia-sim --push     # scale to 0, keep it installed
scripts/envctl.sh up nvidia-sim --push       # scale back up
scripts/envctl.sh remove nvidia-sim --push   # delete everything it deployed
scripts/envctl.sh status nvidia-sim
```

See [docs/lifecycle.md](docs/lifecycle.md) for details and
[docs/architecture.md](docs/architecture.md) for how it works.
