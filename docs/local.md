# Running environments locally (Mac)

The root [Makefile](../Makefile) loads any environment into a local **kind**,
**k3d** or **k3s** (Colima) cluster, runs its simulations, and tears it down.
It deploys from your **working tree**, without Argo CD and without pushing, so
you can iterate on an environment's specs before they go to `main`.

```bash
make help                                   # all targets
make envs                                   # environments + local requirements
make up ENV=nvidia-triton-cpu               # checks everything, offers to fix what's missing, deploys
make simulator                              # build + load + deploy the simulation controller
make sim-suite ENV=nvidia-triton-cpu        # run the env's simulations → reports/nvidia-triton-cpu/<run>/
make down ENV=nvidia-triton-cpu             # tear it down
make cluster-delete                         # remove the local cluster
```

## What `make up` checks (and offers to fix)

`make check ENV=<env>` runs the same preflight without deploying.

| Check | If it fails |
|---|---|
| `kubectl`, `helm`, `yq` installed | offers `brew install …` (or tells you the command) |
| a kubectl context exists and the API answers | context unreachable: diagnoses why. Docker/OrbStack/Colima not running → offers to start it. kind/k3d cluster stopped → offers to start it. No cluster at all → offers to create one of the env's recommended type |
| the context is **local** (loopback / local VM API address) | refuses, with the context and server, unless `ALLOW_REMOTE=1` |
| the cluster **type** suits the env | e.g. nvidia-sim on k3d → error explaining why, and the command to create a kind cluster |
| Kubernetes ≥ 1.28, node **architecture** | network-kne on Apple Silicon → error: Cisco XRd and meshnet are x86_64-only |
| allocatable **CPU / memory** ≥ the env's needs | error with how to give the VM more (`FORCE=1` to continue anyway) |
| **dependencies** (nvidia-dynamo-disagg → nvidia-sim) | deployed first automatically |
| env-specific | nvidia-sim on k3s (Colima): offers to run the host prep inside the VM. network-kne: warns if the XRd image is missing on the node |

Prompts need a terminal. In scripts or CI use `AUTO_INSTALL=yes` (accept all)
or `AUTO_INSTALL=no` (fail with instructions).

## Clusters

| `CLUSTER=` | Runs | Created with | Good for |
|---|---|---|---|
| `kind` | Kubernetes in a Docker container (systemd + containerd inside) | `kind create cluster` | **nvidia-sim**, **nvidia-dynamo-disagg** (Mokka configures the node's containerd itself), **network-kne** (KNE's own default) |
| `k3d` | k3s in a Docker container | `k3d cluster create` | qa, observability, runai-sim, nvidia-triton-cpu (fast, light) |
| `k3s` | k3s in a Colima VM (`colima start -p <name> --kubernetes --runtime containerd`) | Colima | closest to the target single-node k3s, including the host-prep scripts |

kind and k3d need a Docker daemon: Docker Desktop, OrbStack or Colima are
detected. If none is installed, the script offers `brew install colima docker`
and starts Colima with `CPUS` / `MEMORY` / `DISK` (default 6 / 16 GiB / 100 GiB).

```bash
make cluster-install                        # tools + runtime + k3d cluster "testenv"
make cluster-install CLUSTER=kind           # … kind
make cluster-install ENV=nvidia-sim         # … the type nvidia-sim needs (kind)
make cluster-install CLUSTER=k3s CPUS=8 MEMORY=24
make tools CLUSTER=kind                     # just the CLIs
make cluster-delete CLUSTER=kind            # asks first
```

Clusters created here map host ports 30300 (Grafana) and 30843 (simulation
API). An existing local cluster of a suitable type is used as is, whatever its
name.

## How `make up` deploys without Argo CD

1. Renders `environments/<env>` with Helm, with the values the ApplicationSet
   would pass, plus local overrides from `local/values/<env>/<cluster>.yaml`
   (e.g. kind: Mokka sets up containerd itself; meshnet uses standard CNI
   paths) and `local/values/<env>/all.yaml`.
2. Expands every child Argo CD Application (app-of-apps) in **sync-wave
   order**: upstream Helm charts (HTTP and OCI), local charts and kustomize
   directories from the working tree.
3. Applies each with server-side apply: CRDs first, then the rest, retrying
   while operators install their own CRDs and webhooks. It waits for
   Deployments/DaemonSets before the next wave (`WAIT=0` skips the wait,
   `WAIT_TIMEOUT=600s` extends it).
4. Helm test and delete hooks, and Argo CD `PostDelete` hooks, are skipped, as on
   an Argo CD sync. Hook Jobs are re-created on every `up`.
5. Images this repo builds (simulator, kne-runner) are built for the node
   architecture and loaded into the cluster (`kind load`, `k3d image import`,
   `nerdctl` in Colima), so nothing needs publishing.

`make render ENV=<env> CLUSTER=kind` writes all of this to a temp directory
without applying anything. `make down` deletes it in reverse wave order, then
the environment's namespaces (`PURGE=1` also deletes CRDs and strips stuck
finalizers).

To try the real GitOps path instead, `make argocd` installs Argo CD and the
ApplicationSet. It deploys from GitHub `main`, so changes must be pushed.

## Simulations

```bash
make simulator                                           # once per cluster
make sim-catalog                                         # types; TYPE=net-jitter for details
make sim TYPE=net-latency ARGS="--target-namespace nvidia-triton-cpu \
  --pod-selector app.kubernetes.io/name=triton-inference-server --param delay=150ms \
  --duration 2m --baseline 1m --after 1m --observe triton"
make sim-suite ENV=network-kne                           # simulations/network-kne/suite.txt
make sim-suite ENV=nvidia-triton-cpu ONLY='latency|crash'
make sim-list; make sim-report NAME=<name>; make sim-metrics NAME=<name> > run.csv
make sim-recover-all                                     # emergency stop
```

`simctl` runs inside the controller pod, so nothing else is needed on the Mac.
Bring up the [observability](../environments/observability/README.md)
environment first (`make up ENV=observability`) to get Prometheus metrics in
reports. Without it, reports have probe results only.

A suite run writes, for each simulation, `<name>.report.txt` /
`.report.json` / `.csv` and a `summary.tsv` (phase, time to recover) to
`reports/<env>/<run>/`. Ctrl-C recovers the run's simulations. Suites are plain
text (`name | type | simctl inject args`), one file per environment in
`simulations/<env>/suite.txt`.

## What has been validated

See [testing.md](testing.md). nvidia-triton-cpu plus a measured `net-latency`
simulation have run end to end on k3d (Apple Silicon); the rest is still to be run.

## Per-environment notes

| Environment | Locally |
|---|---|
| qa, observability, runai-sim | any cluster type |
| nvidia-triton-cpu | any; the Triton image is ~20 GB (Colima `DISK`, Docker Desktop disk limit) |
| nvidia-sim, nvidia-dynamo-disagg | kind (Mokka configures containerd via systemd in the node), or k3s in Colima with host prep. Not k3d |
| network-kne | x86_64 only (Intel Mac or an x86 VM). Import the Cisco XRd image into the node first (`kind load image-archive <xrd>.tgz --name testenv`). The inotify sysctls belong to the Docker VM |
