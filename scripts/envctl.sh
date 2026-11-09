#!/usr/bin/env bash
# envctl — manage test environments the GitOps way.
#
# Lifecycle is declared in environments/<env>/env.yaml and applied by Argo CD.
# This script edits those switches, commits, and (with --push) pushes, so Argo CD
# picks the change up. It also has a few read-only / cleanup helpers.
#
# Usage: scripts/envctl.sh <command> [env] [--push] [--no-commit]
#
#   list                 show every environment and its switches
#   up <env>             enabled: "true", state: up    (deploy / scale back up)
#   down <env>           enabled: "true", state: down  (scale workloads to 0)
#   remove <env>         enabled: "false"               (Argo CD deletes the env)
#   status <env>         Argo CD Applications and pods for the env (kubectl)
#   sync <env>           ask Argo CD to refresh + sync the env now (kubectl)
#   purge <env> [--crds] delete leftovers after remove: env-labelled namespaces,
#                        stuck finalizers; --crds also deletes the env's CRDs
#
#   --push       git push after committing (Argo CD reads the remote)
#   --no-commit  only edit env.yaml
set -euo pipefail

ROOT=$(cd "$(dirname "$0")/.." && pwd)
ENVDIR="$ROOT/environments"
ARGO_NS=${ARGOCD_NAMESPACE:-argocd}

PUSH=false; COMMIT=true; CRDS=false; ARGS=()
for a in "$@"; do
  case "$a" in
    --push) PUSH=true ;;
    --no-commit) COMMIT=false ;;
    --crds) CRDS=true ;;
    -h|--help) sed -n '2,23p' "$0"; exit 0 ;;
    *) ARGS+=("$a") ;;
  esac
done
CMD=${ARGS[0]:-list}
ENV=${ARGS[1]:-}

die() { echo "error: $*" >&2; exit 1; }
need_env() {
  [[ -n "$ENV" ]] || die "missing environment name"
  [[ -f "$ENVDIR/$ENV/env.yaml" ]] || die "no such environment: $ENV ($(ls "$ENVDIR" | tr '\n' ' '))"
}
get()  { awk -v k="$1" 'index($0, k":")==1 {v=substr($0, length(k)+2); sub(/^ +/,"",v); sub(/ +#.*/,"",v); gsub(/"/,"",v); print v; exit}' "$2"; }
setk() { # setk <key> <value> <file>  (top-level scalar keys only)
  sed -i.bak -E "s|^$1:.*|$1: $2|" "$3" && rm -f "$3.bak"
}

commit() {
  local msg=$1 f="$ENVDIR/$ENV/env.yaml"
  $COMMIT || { echo "edited $f (not committed)"; return; }
  if git -C "$ROOT" diff --quiet -- "$f"; then echo "no change: $ENV already in that state"; return; fi
  git -C "$ROOT" add "$f"
  git -C "$ROOT" commit -q -m "$msg" && echo "committed: $msg"
  local branch rev
  branch=$(git -C "$ROOT" rev-parse --abbrev-ref HEAD)
  rev=$(get targetRevision "$f")
  [[ "$branch" == "$rev" ]] || echo "note: on branch '$branch' but Argo CD reads env.yaml from the ApplicationSet revision (main) and deploys '$rev'"
  if $PUSH; then git -C "$ROOT" push -q && echo "pushed; Argo CD will reconcile within its poll interval (run '$0 sync $ENV' to hurry it)"
  else echo "run 'git push' (or re-run with --push) for Argo CD to see it"; fi
}

case "$CMD" in
  list)
    printf '%-16s %-8s %-6s %-10s %s\n' ENV ENABLED STATE REVISION DESCRIPTION
    for f in "$ENVDIR"/*/env.yaml; do
      printf '%-16s %-8s %-6s %-10s %s\n' "$(get name "$f")" "$(get enabled "$f")" "$(get state "$f")" \
        "$(get targetRevision "$f")" "$(get description "$f")"
    done ;;
  up)
    need_env; f="$ENVDIR/$ENV/env.yaml"
    setk enabled '"true"' "$f"; setk state up "$f"; commit "testenv($ENV): up" ;;
  down)
    need_env; f="$ENVDIR/$ENV/env.yaml"
    setk enabled '"true"' "$f"; setk state down "$f"; commit "testenv($ENV): scale down to 0" ;;
  remove)
    need_env; f="$ENVDIR/$ENV/env.yaml"
    setk enabled '"false"' "$f"; commit "testenv($ENV): remove" ;;
  status)
    need_env
    kubectl -n "$ARGO_NS" get applications -l "testenv.onit.ai/name=$ENV" \
      -o custom-columns=APP:.metadata.name,SYNC:.status.sync.status,HEALTH:.status.health.status,REV:.status.sync.revision
    for ns in $(kubectl get ns -l "testenv.onit.ai/name=$ENV" -o name | cut -d/ -f2); do
      echo "--- namespace $ns"; kubectl -n "$ns" get pods -o wide 2>/dev/null || true
    done ;;
  sync)
    need_env
    for app in $(kubectl -n "$ARGO_NS" get applications -l "testenv.onit.ai/name=$ENV" -o name); do
      kubectl -n "$ARGO_NS" annotate "$app" argocd.argoproj.io/refresh=hard --overwrite >/dev/null
      kubectl -n "$ARGO_NS" patch "$app" --type merge \
        -p '{"operation":{"initiatedBy":{"username":"envctl"},"sync":{"syncStrategy":{"hook":{}}}}}' >/dev/null \
        && echo "sync requested: $app"
    done
    kubectl -n "$ARGO_NS" annotate applicationset testenvs argocd.argoproj.io/application-set-refresh=true --overwrite >/dev/null 2>&1 || true ;;
  purge)
    need_env
    [[ "$(get enabled "$ENVDIR/$ENV/env.yaml")" == "false" ]] || die "$ENV is still enabled; run '$0 remove $ENV --push' first"
    if kubectl -n "$ARGO_NS" get applications -l "testenv.onit.ai/name=$ENV" -o name | grep -q .; then
      echo "Argo CD Applications for $ENV still exist; waiting up to 5m for them to delete..."
      kubectl -n "$ARGO_NS" wait --for=delete applications -l "testenv.onit.ai/name=$ENV" --timeout=300s || true
    fi
    # Custom resources stuck on a finalizer whose operator is already gone.
    if [[ -x "$ENVDIR/$ENV/scripts/purge-extra.sh" ]]; then "$ENVDIR/$ENV/scripts/purge-extra.sh" "$CRDS"; fi
    for ns in $(kubectl get ns -l "testenv.onit.ai/name=$ENV" -o name | cut -d/ -f2); do
      echo "deleting leftover namespace $ns"; kubectl delete ns "$ns" --wait=false
    done
    echo "purge done" ;;
  *) die "unknown command: $CMD (see --help)" ;;
esac
