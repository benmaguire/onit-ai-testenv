#!/usr/bin/env bash
# One-time host preparation for the network-kne environment on k3s.
#   sudo ./k3s-host-prep.sh [--check] [--image /path/xrd-control-plane-container-x64.dockerv1.tgz --tag ios-xr/xrd-control-plane:24.4.2]
#
#  1. sysctls Cisco XRd needs (inotify instances/watches; per XRd instance)
#  2. checks k3s' CNI directories exist where meshnet is configured to install
#  3. optionally imports the XRd image into k3s containerd (it is not public)
set -euo pipefail
CHECK=false IMAGE="" TAG="ios-xr/xrd-control-plane:24.4.2"
while [ $# -gt 0 ]; do
  case "$1" in
    --check) CHECK=true ;;
    --image) IMAGE=$2; shift ;;
    --tag) TAG=$2; shift ;;
    -h|--help) sed -n '2,9p' "$0"; exit 0 ;;
    *) echo "unknown flag $1" >&2; exit 2 ;;
  esac
  shift
done
log()  { printf '\033[1;34m==>\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33mWARN\033[0m %s\n' "$*" >&2; }
[ "$EUID" -eq 0 ] || { echo "run as root" >&2; exit 1; }
command -v k3s >/dev/null || { echo "k3s not found" >&2; exit 1; }

SYSCTL=/etc/sysctl.d/90-xrd.conf
want() { # key value
  cur=$(sysctl -n "$1" 2>/dev/null || echo 0)
  if [ "$cur" -lt "$2" ]; then
    if $CHECK; then warn "$1=$cur (XRd wants >= $2)"; else echo "$1 = $2" >> "$SYSCTL.new"; fi
  else log "$1=$cur ok"; fi
}
rm -f "$SYSCTL.new"
want fs.inotify.max_user_instances 64000
want fs.inotify.max_user_watches 524288
if [ -f "$SYSCTL.new" ]; then mv "$SYSCTL.new" "$SYSCTL"; sysctl --system >/dev/null; log "wrote $SYSCTL"; fi

for d in /var/lib/rancher/k3s/data/cni /var/lib/rancher/k3s/agent/etc/cni/net.d; do
  if [ -d "$d" ]; then log "$d present"; else warn "$d missing: k3s older than the October 2024 releases? meshnet's paths in meshnet/k3s/kustomization.yaml must match containerd's CNI bin_dir/conf_dir (grep -A3 'cni' /var/lib/rancher/k3s/agent/etc/containerd/config.toml)"; fi
done
grep -E 'bin_dirs?|conf_dir' /var/lib/rancher/k3s/agent/etc/containerd/config.toml 2>/dev/null | sed 's/^/    containerd: /' || true

if [ -n "$IMAGE" ] && ! $CHECK; then
  log "importing $IMAGE"
  out=$(k3s ctr -n k8s.io images import "$IMAGE" 2>&1 | tee /dev/stderr)
  ref=$(echo "$out" | sed -n 's/^unpacking \([^ ]*\) .*/\1/p' | head -1)
  if [ -n "$ref" ] && [ "$ref" != "$TAG" ]; then
    k3s ctr -n k8s.io images tag --force "$ref" "docker.io/$TAG" >/dev/null && log "tagged $ref as $TAG"
  fi
fi
if k3s ctr -n k8s.io images ls -q | grep -q "$TAG"; then log "router image $TAG present"; else warn "router image $TAG not found in k3s containerd (set routerImage or import it with --image)"; fi
log "done"
