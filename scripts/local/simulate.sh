# shellcheck shell=bash
# Simulation controller deployment and simulation runs (simctl runs inside the
# controller pod, so nothing but kubectl is needed on the Mac).

SIM_NS="sim-system"

sim_deploy() {
  local w img=${SIMULATOR_IMAGE:-$SIMULATOR_IMG} out
  log "Deploying the simulation controller"
  [ -n "${SIMULATOR_IMAGE:-}" ] || build_load "$ROOT/simulations/controller" "$SIMULATOR_IMG"
  w=$(work_dir); out="$w/simulator.yaml"
  kubectl kustomize "$ROOT/simulations/controller/deploy/overlays/agent" \
    | IMG="$img" SIM_CLUSTER="${CTX#*-}" yq '(select(.kind == "Deployment") | .spec.template.spec.containers[0].image) = env(IMG)
                    | (select(.kind == "Deployment") | .spec.template.spec.containers[0].imagePullPolicy) = "IfNotPresent"
                    | (select(.kind == "Deployment") | .spec.template.spec.containers[0].env[] | select(.name == "SIM_CLUSTER_NAME") | .value) = env(SIM_CLUSTER)' > "$out"
  [ "$DRY_RUN" = 1 ] && { ok "[dry-run] simulator manifests in $out"; return 0; }
  ensure_ns "$SIM_NS" simulator
  if ! kubectl -n "$SIM_NS" get secret simulator-api-token >/dev/null 2>&1; then
    kubectl -n "$SIM_NS" create secret generic simulator-api-token --from-literal=token="$(LC_ALL=C tr -dc 'a-f0-9' </dev/urandom | head -c 64)" >/dev/null
    ok "created API token Secret"
  fi
  apply_file "$out" "$SIM_NS" simulator
  kubectl -n "$SIM_NS" rollout restart deploy/testenv-simulator >/dev/null 2>&1 || true
  kubectl -n "$SIM_NS" rollout status deploy/testenv-simulator --timeout="$WAIT_TIMEOUT" >/dev/null || die "simulator did not become ready: kubectl -n $SIM_NS logs deploy/testenv-simulator"
  ok "simulation controller ready ($img)"
  kubectl get ns -l testenv.onit.ai/name=observability -o name | grep -q . \
    || warn "observability is not up: probes work, Prometheus metric queries will report 'no data' (make up ENV=observability)"
}

sim_undeploy() {
  local w; w=$(work_dir)
  kubectl -n "$SIM_NS" get sims -o name 2>/dev/null | grep -q . && { simctl recover-all >/dev/null 2>&1 || true; kubectl -n "$SIM_NS" delete sims --all --timeout=180s >/dev/null 2>&1 || true; }
  kubectl kustomize "$ROOT/simulations/controller/deploy/overlays/agent" > "$w/simulator.yaml"
  delete_file "$w/simulator.yaml" "$SIM_NS" simulator
  kubectl delete ns "$SIM_NS" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  ok "simulation controller removed"
}

sim_ready() {
  kubectl -n "$SIM_NS" get deploy testenv-simulator >/dev/null 2>&1 || {
    if confirm "The simulation controller is not deployed. Build and deploy it now?"; then sim_deploy
    else die "deploy it first: make simulator"; fi
  }
}

# simctl <args...>: run simctl inside the controller pod.
simctl() {
  local tok
  tok=$(kubectl -n "$SIM_NS" get secret simulator-api-token -o jsonpath='{.data.token}' | base64 --decode)
  kubectl -n "$SIM_NS" exec deploy/testenv-simulator -c simulator -- /usr/local/bin/simctl \
    --server http://127.0.0.1:8443 --token "$tok" -n "$SIM_NS" "$@"
}

# sim_run <type> <args string>
sim_run() {
  [ -n "$1" ] || die "set TYPE (see: make sim-catalog)"
  sim_ready
  local run
  run=local-$(date +%Y%m%d-%H%M%S)
  eval "set -- \"\$1\" $2"
  log "Running $1 (testrun=$run)"
  simctl inject "$@" --label "testrun=$run" --wait "${SIM_WAIT:-observed}"
}

# sim_suite <env>: run simulations/<env>/suite.txt, save reports under reports/<env>/<run>/.
sim_suite() {
  local env=$1 suite="$ROOT/simulations/$1/suite.txt" run dir line name type args n=0 failed=0
  [ -f "$suite" ] || die "no simulation suite for $env ($suite)"
  kubectl get ns -l "testenv.onit.ai/name=$env" -o name | grep -q . || die "$env is not up (make up ENV=$env)"
  sim_ready
  run=suite-$env-$(date +%Y%m%d-%H%M%S); dir="$ROOT/reports/$env/$run"; mkdir -p "$dir"
  log "Suite ${B}$env${N} -> ${dir#"$ROOT"/} (testrun=$run)"
  trap 'warn "interrupted: recovering"; simctl recover-all --selector "testrun='"$run"'" >/dev/null 2>&1; exit 130' INT TERM
  while IFS='|' read -r name type args; do
    case "$name" in ''|\#*) continue ;; esac
    name=$(echo "$name" | tr -d ' '); type=$(echo "$type" | tr -d ' ')
    [ -n "${ONLY:-}" ] && ! printf '%s' "$name" | grep -qE "$ONLY" && continue
    n=$((n+1))
    log "[$n] $name: $type $args"
    eval "set -- $args"
    if simctl inject "$type" --name "$name-$(date +%H%M%S)" --label "testrun=$run" --label "suite=$env" "$@" >"$dir/$name.inject.txt" 2>&1; then :; else
      bad "$name: inject failed: $(tail -2 "$dir/$name.inject.txt" | tr '\n' ' ')"; failed=$((failed+1)); continue
    fi
    local sim
    sim=$(grep -m1 -oE 'created [^/ ]+/[^ ]+' "$dir/$name.inject.txt" | sed 's#.*/##')
    [ -n "$sim" ] || sim=$(tail -1 "$dir/$name.inject.txt")
    simctl report "$sim" --wait --timeout "${SUITE_TIMEOUT:-30m}" > "$dir/$name.report.txt" 2>&1 || true
    simctl -o json report "$sim" > "$dir/$name.report.json" 2>/dev/null || true
    simctl metrics "$sim" > "$dir/$name.csv" 2>/dev/null || true
    local phase ttr
    phase=$(yq -p json -oy '.phase // "?"' "$dir/$name.report.json" 2>/dev/null || echo "?")
    ttr=$(yq -p json -oy '.data.timeToRecover // "-"' "$dir/$name.report.json" 2>/dev/null || echo "-")
    if [ "$phase" = Recovered ]; then ok "$name: $phase, timeToRecover $ttr"; else bad "$name: $phase"; failed=$((failed+1)); fi
    printf '%s\t%s\t%s\t%s\n' "$name" "$type" "$phase" "$ttr" >> "$dir/summary.tsv"
  done < "$suite"
  trap - INT TERM
  log "Suite done: $n simulation(s), $failed not recovered. Reports: ${dir#"$ROOT"/}"
  [ -f "$dir/summary.tsv" ] && column -t -s "$(printf '\t')" "$dir/summary.tsv" >&2
  [ $failed = 0 ]
}
