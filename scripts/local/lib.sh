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
env_meta() {
  REQ_CPU=1; REQ_MEM=1; ARCH=any; DEPS=""; SUPPORTED="kind k3d k3s other"; RECOMMENDED=k3d; NOTE=""
  case "$1" in
    qa) ;;
    observability) REQ_CPU=2; REQ_MEM=3 ;;
    runai-sim) REQ_CPU=2; REQ_MEM=3 ;;
    nvidia-triton-cpu) REQ_CPU=3; REQ_MEM=6; NOTE="pulls a ~20 GB Triton image: give the container VM DISK>=60" ;;
    nvidia-sim)
      REQ_CPU=4; REQ_MEM=8; SUPPORTED="kind k3s"; RECOMMENDED=kind
      NOTE="Mokka configures containerd on the node: kind (systemd in the node container) or k3s in a VM with host prep. k3d runs k3s without systemd, so the containerd setup cannot run there." ;;
    nvidia-dynamo-disagg)
      REQ_CPU=5; REQ_MEM=10; DEPS="nvidia-sim"; SUPPORTED="kind k3s"; RECOMMENDED=kind ;;
    network-kne)
      REQ_CPU=6; REQ_MEM=8; ARCH=amd64; RECOMMENDED=kind
      NOTE="Cisco XRd and meshnet are x86_64-only, and the XRd image must be imported (see environments/network-kne/README.md)." ;;
    *) [ -f "$ROOT/environments/$1/env.yaml" ] || die "unknown environment '$1' (have: $(list_envs | tr '\n' ' '))" ;;
  esac
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
