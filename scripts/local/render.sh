# shellcheck shell=bash
# Renders an environment from the local working tree and applies / deletes it
# without Argo CD: the env chart is rendered with the same values the
# ApplicationSet passes, and every child Argo CD Application it produces
# (app-of-apps) is expanded and applied in sync-wave order.

# One work directory per run (created here, not in a subshell, so it is shared).
WORK=$(mktemp -d "${TMPDIR:-/tmp}/testenv.XXXXXX")
work_dir() { echo "$WORK"; }

# local_values <env>: extra -f flags for the current cluster type.
local_values() {
  local f="$ROOT/local/values/$1/$KIND_OF.yaml"
  [ -f "$f" ] && printf -- '-f\n%s\n' "$f"
  f="$ROOT/local/values/$1/all.yaml"
  [ -f "$f" ] && printf -- '-f\n%s\n' "$f"
  return 0
}

# render_parent <env> <state> <out>
render_parent() {
  local env=$1 state=$2 out=$3 ns args=()
  ns=$(env_ns "$env")
  while IFS= read -r a; do [ -n "$a" ] && args+=("$a"); done < <(local_values "$env")
  if [ -n "${FRONTEND:-}" ]; then args+=(--set "frontend=$FRONTEND"); fi
  # Published images instead of the locally built ones (images.sh skips the build).
  case "$env" in
    dc-ot-sim) [ -n "${OTSIM_IMAGE:-}" ] && args+=(--set "image=$OTSIM_IMAGE") ;;
    network-kne) [ -n "${KNE_RUNNER_IMAGE:-}" ] && args+=(--set "components.topology.values.runner.image=$KNE_RUNNER_IMAGE") ;;
  esac
  helm template "$env" "$ROOT/environments/$env" -n "$ns" \
    --set "state=$state" --set "hardware=$(env_hardware "$env")" --set "global.envName=$env" --set "global.project=testenvs" \
    --set "global.repoURL=local" --set "global.targetRevision=local" \
    ${args[@]+"${args[@]}"} > "$out" || die "helm template environments/$env failed"
}

# filter_hooks: drop Helm test/delete hooks and Argo PostDelete hooks (Argo CD does not run them on sync either).
filter_hooks() {
  yq 'select(. != null)
      | select(((.metadata.annotations["helm.sh/hook"] // "") | test("test|delete")) | not)
      | select((.metadata.annotations["argocd.argoproj.io/hook"] // "") != "PostDelete")' -
}

# render_app <apps.json> <index> <out>: manifests for one child Application.
render_app() {
  local apps=$1 i=$2 out=$3 w repo chart path ver rel ns skip vals
  w=$(work_dir)
  q() { yq -p json -oy ".[$i] | ($1)" "$apps"; }
  repo=$(q '.spec.source.repoURL'); chart=$(q '.spec.source.chart // ""'); path=$(q '.spec.source.path // ""')
  ver=$(q '.spec.source.targetRevision // ""'); rel=$(q '.spec.source.helm.releaseName // .metadata.name')
  ns=$(q '.spec.destination.namespace'); skip=$(q '.spec.source.helm.skipCrds // false')
  vals="$w/values-$i.yaml"
  q '.spec.source.helm.valuesObject // {}' | yq -P > "$vals"
  local crds=--include-crds; [ "$skip" = true ] && crds=""
  if [ -n "$chart" ]; then
    if printf '%s' "$repo" | grep -q '://'; then
      helm template "$rel" "$chart" --repo "$repo" --version "$ver" -n "$ns" -f "$vals" $crds > "$out" \
        || die "helm template $chart from $repo $ver failed"
    else
      helm template "$rel" "oci://$repo/$chart" --version "$ver" -n "$ns" -f "$vals" $crds > "$out" \
        || die "helm template oci://$repo/$chart $ver failed"
    fi
  elif [ -f "$ROOT/$path/Chart.yaml" ]; then
    helm template "$rel" "$ROOT/$path" -n "$ns" -f "$vals" $crds > "$out" || die "helm template $path failed"
  elif [ -f "$ROOT/$path/kustomization.yaml" ]; then
    kubectl kustomize "$ROOT/$path" > "$out" || die "kustomize $path failed"
  else
    die "child Application $(q '.metadata.name'): cannot render source $repo $path"
  fi
}

ensure_ns() { # ensure_ns <ns> <env>
  [ "$DRY_RUN" = 1 ] && return 0
  kubectl get ns "$1" >/dev/null 2>&1 || kubectl create ns "$1" >/dev/null
  kubectl label ns "$1" --overwrite "testenv.onit.ai/name=$2" app.kubernetes.io/part-of=testenvs >/dev/null
}

# apply_file <file> <ns> <label>: CRDs first, then the rest, retrying while
# CRDs/webhooks created by operators at runtime come up.
apply_file() {
  local f=$1 ns=$2 label=$3 w crds rest n
  w=$(work_dir); crds="$w/$label.crds.yaml"; rest="$w/$label.rest.yaml"
  filter_hooks < "$f" > "$w/$label.filtered.yaml"
  yq 'select(.kind == "CustomResourceDefinition")' "$w/$label.filtered.yaml" > "$crds"
  yq 'select(.kind != "CustomResourceDefinition")' "$w/$label.filtered.yaml" > "$rest"
  n=$(yq ea '[.] | length' "$w/$label.filtered.yaml" 2>/dev/null || echo 0)
  if [ "$DRY_RUN" = 1 ]; then ok "[dry-run] $label: $n object(s) -> $w/$label.filtered.yaml"; return 0; fi
  if [ -s "$crds" ] && grep -q 'kind:' "$crds"; then
    kubectl apply --server-side --force-conflicts --field-manager=testenv-local -f "$crds" >/dev/null || die "$label: applying CRDs failed"
    kubectl wait --for=condition=Established --timeout=120s -f "$crds" >/dev/null 2>&1 || true
  fi
  # Jobs are immutable: delete hook Jobs so they re-run (Argo: BeforeHookCreation).
  local j jns
  for j in $(yq 'select(.kind == "Job") | (.metadata.namespace // "") + "/" + .metadata.name' "$rest" 2>/dev/null); do
    jns=${j%%/*}; [ -n "$jns" ] || jns=$ns
    kubectl -n "$jns" delete job "${j#*/}" --ignore-not-found >/dev/null 2>&1 || true
  done
  grep -q 'kind:' "$rest" 2>/dev/null || { ok "$label: $n object(s)"; return 0; }
  local try=1 out
  while :; do
    if out=$(kubectl apply --server-side --force-conflicts --field-manager=testenv-local -n "$ns" -f "$rest" 2>&1); then
      ok "$label: $n object(s) applied"; return 0
    fi
    if [ $try -ge 12 ]; then printf '%s\n' "$out" | tail -5 >&2; die "$label: apply failed after $try attempts"; fi
    warn "$label: not ready yet ($(printf '%s' "$out" | grep -m1 -oE 'no matches for kind "[A-Za-z]+"|failed calling webhook "[^"]+"|not found|connection refused' || echo retrying)), retry $try/12 in 10s"
    try=$((try+1)); sleep 10
  done
}

wait_ready() { # wait_ready <ns> <label>
  [ "$DRY_RUN" = 1 ] && return 0
  [ "${WAIT:-1}" = 0 ] && return 0
  local r failed=0
  for r in $(kubectl -n "$1" get deploy,ds,sts -o name 2>/dev/null); do
    kubectl -n "$1" rollout status "$r" --timeout="$WAIT_TIMEOUT" >/dev/null 2>&1 || { warn "$2: $r not ready within $WAIT_TIMEOUT"; failed=1; }
  done
  [ $failed = 0 ] && ok "$2: ready"
  return 0
}

# env_up <env>
env_up() {
  local env=$1 state=${STATE:-up} w ns parent apps count i name ans d
  env_meta "$env"
  for d in $DEPS; do
    if [ "$DRY_RUN" != 1 ] && ! kubectl get ns -l "testenv.onit.ai/name=$d" -o name 2>/dev/null | grep -q .; then
      log "Dependency $d"
      ( env_up "$d" )
    fi
  done
  env_meta "$env"
  log "Deploying ${B}$env${N} (state=$state, hardware=$(env_hardware "$env"), cluster=$KIND_OF, from the working tree)"
  local_images "$env"
  w=$(work_dir); ns=$(env_ns "$env"); parent="$w/$env.parent.yaml"; apps="$w/$env.apps.json"
  render_parent "$env" "$state" "$parent"
  yq ea -o=json '[select(.kind == "Application")] | sort_by((.metadata.annotations["argocd.argoproj.io/sync-wave"] // "0") | tonumber)' "$parent" > "$apps"
  yq 'select(.kind != "Application")' "$parent" > "$w/$env.direct.yaml"
  if grep -q 'kind:' "$w/$env.direct.yaml"; then
    ensure_ns "$ns" "$env"
    apply_file "$w/$env.direct.yaml" "$ns" "$env"
    label_env_namespaces "$w/$env.direct.yaml" "$env"
    wait_ready "$ns" "$env"
  fi
  count=$(yq -p json -oy 'length' "$apps")
  i=0
  while [ "$i" -lt "$count" ]; do
    name=$(yq -p json -oy ".[$i].metadata.name" "$apps"); ans=$(yq -p json -oy ".[$i].spec.destination.namespace" "$apps")
    render_app "$apps" "$i" "$w/$name.yaml"
    ensure_ns "$ans" "$env"
    apply_file "$w/$name.yaml" "$ans" "$name"
    label_env_namespaces "$w/$name.yaml" "$env"
    wait_ready "$ans" "$name"
    i=$((i+1))
  done
  [ "$DRY_RUN" = 1 ] && { ok "rendered into $w"; return 0; }
  ok "${B}$env${N} is up. Status: make status ENV=$env"
}

# label_env_namespaces <file> <env>: Namespaces a chart renders also get the env label (for teardown).
label_env_namespaces() {
  [ "$DRY_RUN" = 1 ] && return 0
  local n
  for n in $(yq 'select(.kind == "Namespace") | .metadata.name' "$1"); do
    kubectl label ns "$n" --overwrite "testenv.onit.ai/name=$2" >/dev/null 2>&1 || true
  done
}

# env_down <env>: delete what env_up applied, children in reverse wave order, then the env's namespaces.
env_down() {
  local env=$1 w parent apps count i name ans f dep
  env_meta "$env"
  for dep in $(dependents_of "$env"); do
    if kubectl get ns -l "testenv.onit.ai/name=$dep" -o name 2>/dev/null | grep -q .; then
      warn "$dep depends on $env and is still up (make down ENV=$dep first)"
    fi
  done
  log "Tearing down ${B}$env${N}"
  w=$(work_dir); parent="$w/$env.parent.yaml"; apps="$w/$env.apps.json"
  render_parent "$env" up "$parent"
  yq ea -o=json '[select(.kind == "Application")] | sort_by((.metadata.annotations["argocd.argoproj.io/sync-wave"] // "0") | tonumber) | reverse' "$parent" > "$apps"
  env_predown "$env"
  count=$(yq -p json -oy 'length' "$apps")
  i=0
  while [ "$i" -lt "$count" ]; do
    name=$(yq -p json -oy ".[$i].metadata.name" "$apps"); ans=$(yq -p json -oy ".[$i].spec.destination.namespace" "$apps")
    f="$w/$name.yaml"
    render_app "$apps" "$i" "$f"
    delete_file "$f" "$ans" "$name"
    i=$((i+1))
  done
  yq 'select(.kind != "Application")' "$parent" > "$w/$env.direct.yaml"
  delete_file "$w/$env.direct.yaml" "$(env_ns "$env")" "$env"
  local n
  for n in $(kubectl get ns -l "testenv.onit.ai/name=$env" -o name 2>/dev/null); do
    kubectl delete "$n" --wait=false >/dev/null 2>&1 && ok "deleting $n"
  done
  if [ "${WAIT:-1}" != 0 ]; then
    kubectl wait --for=delete ns -l "testenv.onit.ai/name=$env" --timeout="$WAIT_TIMEOUT" >/dev/null 2>&1 \
      || warn "some namespaces are still terminating (finalizers?): kubectl get ns -l testenv.onit.ai/name=$env; PURGE=1 make down ENV=$env strips stuck finalizers"
  fi
  [ "${PURGE:-0}" = 1 ] && purge_env "$env"
  ok "${B}$env${N} removed"
}

delete_file() { # delete_file <file> <ns> <label>
  local f=$1 ns=$2 label=$3 w keep
  w=$(work_dir); keep="$w/$label.del.yaml"
  if [ "${PURGE:-0}" = 1 ]; then yq 'select(. != null) | select(.kind != "Namespace")' "$f" > "$keep"
  else yq 'select(. != null) | select(.kind != "Namespace" and .kind != "CustomResourceDefinition")' "$f" > "$keep"; fi
  grep -q 'kind:' "$keep" 2>/dev/null || return 0
  kubectl delete -n "$ns" -f "$keep" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  ok "$label: deleted"
}

dependents_of() {
  local e
  for e in $(list_envs); do env_meta "$e"; case " $DEPS " in *" $1 "*) echo "$e" ;; esac; done
  env_meta "$1"
}

# env_predown <env>: things to do before deleting (e.g. graceful app-level teardown).
env_predown() {
  case "$1" in
    network-kne)
      # The KNE topology namespace was created by the kne runner, not by us.
      kubectl delete ns xrd-lab --ignore-not-found --wait=false >/dev/null 2>&1 && ok "deleting KNE topology namespace xrd-lab" ;;
    nvidia-sim|nvidia-dynamo-disagg)
      # Let the Dynamo operator remove this env's graph pods while it still runs.
      kubectl -n "$(env_ns "$1")" delete dgd --all --wait=true --timeout=120s >/dev/null 2>&1 || true ;;
  esac
}

purge_env() {
  log "Purging leftovers of $1"
  # Strip finalizers on custom resources left by operators that are gone (Dynamo), and CRDs.
  case "$1" in
    nvidia-sim) "$ROOT/environments/nvidia-sim/scripts/purge-extra.sh" true || true ;;
  esac
  local n
  for n in $(kubectl get ns -l "testenv.onit.ai/name=$1" -o name 2>/dev/null); do
    kubectl get "$n" -o json | yq -p json -oy -o json '.spec.finalizers = []' | kubectl replace --raw "/api/v1/namespaces/${n#namespace/}/finalize" -f - >/dev/null 2>&1 || true
  done
}

status_all() {
  detect_context
  log "Cluster: ${CTX:-none} ($SERVER, $KIND_OF)"
  api_ok || { bad "not reachable"; return 0; }
  local e ns
  printf '  %-22s %-8s %s\n' ENVIRONMENT STATUS NAMESPACES >&2
  for e in $(list_envs); do
    ns=$(kubectl get ns -l "testenv.onit.ai/name=$e" -o jsonpath='{range .items[*]}{.metadata.name} {end}' 2>/dev/null)
    if [ -n "$ns" ]; then printf '  %-22s %s%-8s%s %s\n' "$e" "$G" up "$N" "$ns" >&2
    else printf '  %-22s %-8s\n' "$e" - >&2; fi
  done
  if kubectl get deploy -n sim-system testenv-simulator >/dev/null 2>&1; then ok "simulation controller deployed (sim-system)"; else warn "simulation controller not deployed (make simulator)"; fi
}

status_env() {
  local env=$1 n
  for n in $(kubectl get ns -l "testenv.onit.ai/name=$env" -o jsonpath='{range .items[*]}{.metadata.name} {end}'); do
    log "namespace $n"
    kubectl -n "$n" get pods -o wide 2>/dev/null
  done
}
