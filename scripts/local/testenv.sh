#!/usr/bin/env bash
# Local test environments on kind / k3d / k3s (Colima) on a Mac. Driven by the
# root Makefile (`make help`); every command can also be run directly:
#   scripts/local/testenv.sh <command> [env]
set -euo pipefail
HERE=$(cd "$(dirname "$0")" && pwd)
# shellcheck source=lib.sh
. "$HERE/lib.sh"
# shellcheck source=cluster.sh
. "$HERE/cluster.sh"
# shellcheck source=images.sh
. "$HERE/images.sh"
# shellcheck source=render.sh
. "$HERE/render.sh"
# shellcheck source=simulate.sh
. "$HERE/simulate.sh"
# shellcheck source=argocd.sh
. "$HERE/argocd.sh"

# Remove this run's temporary directory (only ever a mktemp testenv.* dir; kept for DRY_RUN).
cleanup_work() {
  [ "$DRY_RUN" = 1 ] && return 0
  case "$WORK" in
    */testenv.??????) [ -d "$WORK" ] && rm -r -- "$WORK" ;;
  esac
  return 0
}
trap cleanup_work EXIT

need_env() { [ -n "${ENV:-}" ] || die "set ENV (one of: $(list_envs | tr '\n' ' '))"; env_meta "$ENV"; }

cmd=${1:-help}
case "$cmd" in
  tools)          install_tools "$CLUSTER" ;;
  cluster-install) cluster_install "${ENV:-}" ;;
  cluster-create) [ "$CLUSTER" != auto ] || { [ -n "${ENV:-}" ] && env_meta "$ENV" && CLUSTER=$RECOMMENDED || CLUSTER=k3d; }; cluster_create "$CLUSTER" ;;
  cluster-delete) cluster_delete "$CLUSTER" ;;
  check)          need_env; preflight "$ENV"; ok "ready for: make up ENV=$ENV" ;;
  up)             need_env; [ "${SKIP_PREFLIGHT:-0}" = 1 ] && { ensure_tool yq; detect_context; } || preflight "$ENV"; env_up "$ENV" ;;
  down)           need_env; detect_context; api_ok || die "cluster '$CTX' not reachable"; server_is_local || [ "$ALLOW_REMOTE" = 1 ] || die "refusing to tear down on non-local context '$CTX' ($SERVER); ALLOW_REMOTE=1 to override"; env_down "$ENV" ;;
  render)         need_env; ensure_tool helm; ensure_tool yq; detect_context; [ -n "$KIND_OF" ] || KIND_OF=${CLUSTER/auto/k3d}; [ "$CLUSTER" != auto ] && KIND_OF=$CLUSTER; DRY_RUN=1; env_up "$ENV" ;;
  status)         ensure_tool yq; if [ -n "${ENV:-}" ]; then status_env "$ENV"; else status_all; fi ;;
  envs)           for e in $(list_envs); do env_meta "$e"; printf '%-22s cpu~%-3s mem~%-4s arch=%-6s clusters=%-18s %s\n' "$e" "$REQ_CPU" "${REQ_MEM}Gi" "$ARCH" "$(echo $SUPPORTED | tr ' ' ',')" "$( [ -n "$DEPS" ] && echo "needs: $DEPS")"; done ;;
  simulator)      ensure_tool yq; detect_context; api_ok || die "no reachable cluster"; sim_deploy ;;
  simulator-delete) ensure_tool yq; detect_context; sim_undeploy ;;
  sim)            ensure_tool yq; detect_context; sim_run "${TYPE:-}" "${ARGS:-}" ;;
  sim-suite)      need_env; ensure_tool yq; detect_context; sim_suite "$ENV" ;;
  simctl)         detect_context; sim_ready; shift; simctl "$@" ;;
  argocd)         ensure_tool helm; detect_context; api_ok || die "no reachable cluster"
                  server_is_local || [ "$ALLOW_REMOTE" = 1 ] || die "context '$CTX' is not a local cluster"
                  argocd_install ;;
  ui)             detect_context
                  [ "${ENV:-observability}" = observability ] || die "ui supports ENV=observability"
                  log "Grafana: http://localhost:3000 (admin / testenv)"
                  kubectl -n observability port-forward svc/kube-prometheus-stack-grafana 3000:80 ;;
  *) die "unknown command '$cmd' (see: make help)" ;;
esac
