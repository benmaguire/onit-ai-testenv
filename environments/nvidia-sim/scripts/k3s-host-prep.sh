#!/usr/bin/env bash
# One-time host preparation for running the nvidia-sim environment on k3s.
#
# Why: Mokka normally sets up containerd itself (NVIDIA container runtime,
# CDI, NRI). k3s embeds its own containerd, which Mokka deliberately leaves
# alone, so on k3s that step is done here instead:
#   1. install the NVIDIA Container Toolkit binaries (no driver needed)
#   2. put nvidia-container-runtime in CDI mode (Mokka writes the CDI spec)
#   3. make sure k3s containerd has NRI and CDI on
#   4. restart k3s so it registers the "nvidia" runtime handler
#   5. make sure the "nvidia" RuntimeClass exists
#
# Run as root on the k3s node:   sudo ./k3s-host-prep.sh [--check] [--no-install] [--undo]
#   --check       only report status, change nothing
#   --no-install  skip installing nvidia-container-toolkit (already present)
#   --undo        remove the containerd template this script added and restart k3s
#
# Restarting k3s on a single-node cluster briefly takes the API server down;
# running pods keep running.
set -euo pipefail

K3S_CTRD_DIR=/var/lib/rancher/k3s/agent/etc/containerd
MARK="# added by onit-ai-testenv k3s-host-prep (nvidia-sim)"
CHECK=false; INSTALL=true; UNDO=false
for a in "$@"; do
  case "$a" in
    --check) CHECK=true ;;
    --no-install) INSTALL=false ;;
    --undo) UNDO=true ;;
    -h|--help) sed -n '2,22p' "$0"; exit 0 ;;
    *) echo "unknown flag: $a" >&2; exit 2 ;;
  esac
done

log()  { printf '\033[1;34m==>\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33mWARN\033[0m %s\n' "$*" >&2; }
die()  { printf '\033[1;31mERROR\033[0m %s\n' "$*" >&2; exit 1; }

[[ $EUID -eq 0 ]] || die "run as root"
command -v k3s >/dev/null || die "k3s not found on this host"

ctrd_major() { k3s ctr version 2>/dev/null | awk '/Server:/{s=1} s&&/Version:/{print $2; exit}' | sed 's/^v//' | cut -d. -f1; }
template_path() { if [[ "$(ctrd_major)" -ge 2 ]]; then echo "$K3S_CTRD_DIR/config-v3.toml.tmpl"; else echo "$K3S_CTRD_DIR/config.toml.tmpl"; fi; }

status() {
  log "k3s: $(k3s --version | head -1)"
  log "containerd major version: $(ctrd_major)"
  if command -v nvidia-container-runtime >/dev/null; then
    log "nvidia-container-runtime: $(command -v nvidia-container-runtime)"
    log "runtime mode: $(nvidia-ctk config 2>/dev/null | awk -F' = ' '/^ *mode *=/{print $2; exit}')"
  else
    warn "nvidia-container-runtime not installed"
  fi
  if grep -q 'nvidia' "$K3S_CTRD_DIR/config.toml" 2>/dev/null; then log "k3s containerd has an nvidia runtime handler"; else warn "k3s containerd has no nvidia runtime handler (restart k3s after installing the toolkit)"; fi
  if [[ -S /var/run/nri/nri.sock ]]; then log "NRI socket present (/var/run/nri/nri.sock)"; else warn "NRI socket missing: Mokka's NRI plugin will not register"; fi
  if k3s crictl info 2>/dev/null | grep -qiE '"enableCDI": *true'; then log "containerd CDI enabled"; else warn "containerd CDI not reported as enabled"; fi
  if k3s kubectl get runtimeclass nvidia >/dev/null 2>&1; then log "RuntimeClass nvidia exists"; else warn "RuntimeClass nvidia missing"; fi
}

if $CHECK; then status; exit 0; fi

if $UNDO; then
  for f in "$K3S_CTRD_DIR/config.toml.tmpl" "$K3S_CTRD_DIR/config-v3.toml.tmpl"; do
    if [[ -f "$f" ]] && grep -q "$MARK" "$f"; then log "removing $f"; rm -f "$f"; fi
  done
  log "restarting k3s"; systemctl restart k3s
  exit 0
fi

# 1. NVIDIA Container Toolkit -------------------------------------------------
if $INSTALL && ! command -v nvidia-ctk >/dev/null; then
  log "installing nvidia-container-toolkit"
  if command -v apt-get >/dev/null; then
    curl -fsSL https://nvidia.github.io/libnvidia-container/gpgkey \
      | gpg --dearmor -o /usr/share/keyrings/nvidia-container-toolkit-keyring.gpg
    curl -fsSL https://nvidia.github.io/libnvidia-container/stable/deb/nvidia-container-toolkit.list \
      | sed 's#deb https://#deb [signed-by=/usr/share/keyrings/nvidia-container-toolkit-keyring.gpg] https://#g' \
      > /etc/apt/sources.list.d/nvidia-container-toolkit.list
    apt-get update -qq && apt-get install -y -qq nvidia-container-toolkit
  elif command -v dnf >/dev/null; then
    curl -fsSL https://nvidia.github.io/libnvidia-container/stable/rpm/nvidia-container-toolkit.repo \
      > /etc/yum.repos.d/nvidia-container-toolkit.repo
    dnf install -y -q nvidia-container-toolkit
  else
    die "unsupported package manager; install nvidia-container-toolkit manually and re-run with --no-install"
  fi
fi
command -v nvidia-container-runtime >/dev/null || die "nvidia-container-runtime not on PATH"

# 2. CDI mode: resolve GPUs from the spec Mokka writes to /var/run/cdi ---------
log "setting nvidia-container-runtime to CDI mode"
nvidia-ctk config --in-place --set nvidia-container-runtime.mode=cdi
mkdir -p /var/run/cdi

# 3. NRI / CDI in k3s containerd ----------------------------------------------
# containerd 2.x (k3s >= v1.31) has NRI and CDI on by default. On 1.7 extend
# the k3s template (keeping k3s' own base config) to switch NRI on.
TMPL=$(template_path)
if [[ "$(ctrd_major)" -lt 2 ]] && ! grep -q 'io.containerd.nri.v1.nri' "$K3S_CTRD_DIR/config.toml" 2>/dev/null; then
  if [[ -f "$TMPL" ]] && ! grep -q "$MARK" "$TMPL"; then
    warn "$TMPL exists and was not written by this script; add the NRI table yourself:"
    printf '  [plugins."io.containerd.nri.v1.nri"]\n    disable = false\n    socket_path = "/var/run/nri/nri.sock"\n' >&2
  else
    log "writing $TMPL (enables NRI)"
    cat > "$TMPL" <<TOML
{{ template "base" . }}

$MARK
[plugins."io.containerd.nri.v1.nri"]
  disable = false
  socket_path = "/var/run/nri/nri.sock"
TOML
  fi
fi

# 4. Restart k3s so it detects nvidia-container-runtime ------------------------
log "restarting k3s (k3s registers the nvidia runtime handler on start)"
systemctl restart k3s
for _ in $(seq 1 60); do k3s kubectl get --raw /readyz >/dev/null 2>&1 && break; sleep 2; done

# 5. RuntimeClass used by GPU Operator operands -------------------------------
if ! k3s kubectl get runtimeclass nvidia >/dev/null 2>&1; then
  log "creating RuntimeClass nvidia"
  k3s kubectl apply -f - <<'YAML'
apiVersion: node.k8s.io/v1
kind: RuntimeClass
metadata:
  name: nvidia
handler: nvidia
YAML
fi

status
log "done. You can now enable the nvidia-sim environment in Argo CD."
