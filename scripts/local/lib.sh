# shellcheck shell=bash
# Shared helpers for scripts/local/testenv.sh. Compatible with macOS bash 3.2.

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
CLUSTER=${CLUSTER:-auto}            # auto | kind | k3d | k3s
CLUSTER_NAME=${CLUSTER_NAME:-testenv}
AUTO_INSTALL=${AUTO_INSTALL:-ask}   # ask | yes | no
ALLOW_REMOTE=${ALLOW_REMOTE:-0}
CPUS=${CPUS:-6}
MEMORY=${MEMORY:-16}                # GiB, for the container VM
DISK=${DISK:-100}                   # GiB
WAIT_TIMEOUT=${WAIT_TIMEOUT:-300s}
DRY_RUN=${DRY_RUN:-0}

if [ -t 1 ]; then B=$'\033[1m'; R=$'\033[31m'; G=$'\033[32m'; Y=$'\033[33m'; C=$'\033[36m'; N=$'\033[0m'; else B='' R='' G='' Y='' C='' N=''; fi
log()  { printf '%s==>%s %s\n' "$C" "$N" "$*" >&2; }
ok()   { printf '  %s✓%s %s\n' "$G" "$N" "$*" >&2; }
warn() { printf '  %s!%s %s\n' "$Y" "$N" "$*" >&2; }
bad()  { printf '  %s✗%s %s\n' "$R" "$N" "$*" >&2; }
die()  { printf '\n%sERROR:%s %s\n' "$R$B" "$N" "$*" >&2; exit 1; }
have() { command -v "$1" >/dev/null 2>&1; }

# confirm "question" -> 0 for yes. Honors AUTO_INSTALL=yes|no; needs a TTY to ask.
confirm() {
  case "$AUTO_INSTALL" in
    yes|y|1|true) return 0 ;;
    no|n|0|false) return 1 ;;
  esac
  if [ ! -r /dev/tty ] || ! (exec </dev/tty) 2>/dev/null; then
    warn "cannot ask \"$1\" without a terminal (set AUTO_INSTALL=yes to accept, AUTO_INSTALL=no to refuse)"
    return 1
  fi
  local a
  printf '%s?%s %s [y/N] ' "$Y" "$N" "$1" >/dev/tty
  read -r a </dev/tty || return 1
  case "$a" in y|Y|yes|YES) return 0 ;; esac
  return 1
}

is_mac() { [ "$(uname -s)" = Darwin ]; }
host_arch() { case "$(uname -m)" in arm64|aarch64) echo arm64 ;; x86_64|amd64) echo amd64 ;; *) uname -m ;; esac; }

# --- environment metadata ----------------------------------------------------------
# env_meta <env>: sets REQ_CPU REQ_MEM ARCH DEPS SUPPORTED RECOMMENDED NOTE
# HW_MODES (simulated / real) CONFLICTS MIN_MINOR (Kubernetes 1.x)
env_meta() {
  REQ_CPU=1; REQ_MEM=1; ARCH=any; DEPS=""; SUPPORTED="kind k3d k3s other"; RECOMMENDED=k3d; NOTE=""
  HW_MODES="simulated"; CONFLICTS=""; MIN_MINOR=28
  case "$1" in
    qa) ;;
    observability) REQ_CPU=2; REQ_MEM=3 ;;
    runai-sim) REQ_CPU=2; REQ_MEM=3 ;;
    nvidia-triton-cpu) REQ_CPU=3; REQ_MEM=6; HW_MODES="simulated real"; NOTE="pulls a ~20 GB Triton image: give the container VM DISK>=60" ;;
    nvidia-sim)
      REQ_CPU=4; REQ_MEM=8; SUPPORTED="kind k3s"; RECOMMENDED=kind; HW_MODES="simulated real"; CONFLICTS="nvidia-dra-sim"
      NOTE="Mokka configures containerd on the node: kind (systemd in the node container) or k3s in a VM with host prep. k3d runs k3s without systemd, so the containerd setup cannot run there." ;;
    nvidia-dynamo-disagg)
      REQ_CPU=5; REQ_MEM=10; DEPS="nvidia-sim"; SUPPORTED="kind k3s"; RECOMMENDED=kind; HW_MODES="simulated real" ;;
    nvidia-dra-sim)
      REQ_CPU=3; REQ_MEM=6; SUPPORTED="kind k3s"; RECOMMENDED=kind; HW_MODES="simulated real"; MIN_MINOR=34
      CONFLICTS="nvidia-sim nvidia-dynamo-disagg nvidia-nvsentinel"
      NOTE="DRA (resource.k8s.io/v1) needs Kubernetes 1.34+; one Mokka per cluster, so not together with nvidia-sim." ;;
    nvidia-nvsentinel)
      REQ_CPU=6; REQ_MEM=10; DEPS="nvidia-sim"; SUPPORTED="kind k3s"; RECOMMENDED=kind; HW_MODES="simulated real"
      NOTE="cert-manager is installed cluster-wide; skip if your cluster already has it (components.certManager.enabled: false)." ;;
    nvidia-nim)
      REQ_CPU=4; REQ_MEM=16; DEPS="nvidia-sim"; HW_MODES="real"
      NOTE="needs NGC secrets ngc-secret and ngc-api-secret in namespace nvidia-nim (environments/nvidia-nim/README.md)." ;;
    network-kne)
      REQ_CPU=6; REQ_MEM=8; ARCH=amd64; RECOMMENDED=kind
      NOTE="Cisco XRd and meshnet are x86_64-only, and the XRd image must be imported (see environments/network-kne/README.md)." ;;
    nvidia-vllm)
      REQ_CPU=3; REQ_MEM=6; HW_MODES="simulated real"
      if [ "$(env_frontend "$1")" = dynamo ]; then
        REQ_CPU=5; REQ_MEM=10; DEPS="nvidia-sim"; SUPPORTED="kind k3s"; RECOMMENDED=kind
        NOTE="frontend dynamo: Dynamo frontend + workers; the Dynamo platform comes from nvidia-sim."
      else
        [ "$(env_hardware "$1")" = real ] && DEPS="nvidia-sim"
        NOTE="frontend inference-gateway installs Gateway API + Envoy Gateway CRDs cluster-wide (k3s: Traefik may already own Gateway API CRDs)."
      fi ;;
    dc-ot-sim) REQ_CPU=1; REQ_MEM=1 ;;
    *) [ -f "$ROOT/environments/$1/env.yaml" ] || die "unknown environment '$1' (have: $(list_envs | tr '\n' ' '))" ;;
  esac
}

# env_hardware <env>: HARDWARE if set, else env.yaml's hardware (default simulated).
env_hardware() {
  if [ -n "${HARDWARE:-}" ]; then echo "$HARDWARE"; return; fi
  local h
  h=$(yq '.hardware // "simulated"' "$ROOT/environments/$1/env.yaml")
  echo "${h:-simulated}"
}

# env_frontend <env>: FRONTEND if set, else the env's values.yaml frontend (nvidia-vllm).
env_frontend() {
  if [ -n "${FRONTEND:-}" ]; then echo "$FRONTEND"; return; fi
  yq '.frontend // ""' "$ROOT/environments/$1/values.yaml" 2>/dev/null
}

list_envs() { for f in "$ROOT"/environments/*/env.yaml; do basename "$(dirname "$f")"; done; }

env_ns() { yq '.namespace' "$ROOT/environments/$1/env.yaml"; }

# --- cluster detection --------------------------------------------------------------
CTX=""; SERVER=""; KIND_OF=""

detect_context() {
  CTX=$(kubectl config current-context 2>/dev/null || true)
  SERVER=""
  KIND_OF=""
  [ -n "$CTX" ] || return 0
  SERVER=$(kubectl config view --minify -o jsonpath='{.clusters[0].cluster.server}' 2>/dev/null || true)
  case "$CTX" in
    kind-*) KIND_OF=kind ;;
    k3d-*) KIND_OF=k3d ;;
    colima*|rancher-desktop|orbstack) KIND_OF=k3s ;;
    docker-desktop|minikube) KIND_OF=other ;;
    *) KIND_OF=unknown ;;
  esac
}

# server_is_local: loopback / VM-local API server addresses.
server_is_local() {
  local host
  host=$(printf '%s' "$SERVER" | sed -E 's#^[a-z]+://##; s#^\[##; s#\](:[0-9]+)?$##; s#:[0-9]+$##; s#/.*$##')
  case "$host" in
    127.*|localhost|0.0.0.0|::1|host.docker.internal|kubernetes.docker.internal|*.local) return 0 ;;
    192.168.5.*|192.168.64.*|192.168.105.*|192.168.106.*|198.19.249.*|172.17.*|172.18.*|172.19.*) return 0 ;;
  esac
  return 1
}

docker_ok() { have docker && docker info >/dev/null 2>&1; }

api_ok() { kubectl get --raw /readyz --request-timeout=5s >/dev/null 2>&1; }

cluster_kind_ctx() { # kind for a CLUSTER value -> context name
  case "$1" in
    kind) echo "kind-$CLUSTER_NAME" ;;
    k3d) echo "k3d-$CLUSTER_NAME" ;;
    k3s) echo "colima-$CLUSTER_NAME" ;;
  esac
}
