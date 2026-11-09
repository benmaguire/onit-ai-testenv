# shellcheck shell=bash
# Tool installation, container runtime, local cluster lifecycle and preflight.

brew_formula() {
  case "$1" in
    kubectl) echo kubernetes-cli ;;
    docker) echo docker ;;
    *) echo "$1" ;;
  esac
}

ensure_tool() {
  local t=$1 why=${2:-}
  if have "$t"; then ok "$t $(tool_version "$t")"; return 0; fi
  bad "$t is not installed${why:+ ($why)}"
  if ! have brew; then
    die "$t is missing and Homebrew is not installed. Install Homebrew (https://brew.sh) and re-run, or install $t yourself."
  fi
  if confirm "Install $t with Homebrew (brew install $(brew_formula "$t"))?"; then
    brew install "$(brew_formula "$t")" || die "brew install $(brew_formula "$t") failed"
    ok "installed $t"
  else
    die "$t is required. Install it with: brew install $(brew_formula "$t")   (or run: make tools)"
  fi
}

tool_version() {
  case "$1" in
    kubectl) kubectl version --client -o yaml 2>/dev/null | yq '.clientVersion.gitVersion' 2>/dev/null || true ;;
    helm) helm version --short 2>/dev/null ;;
    yq) yq --version 2>/dev/null | awk '{print $NF}' ;;
    kind) kind version 2>/dev/null | awk '{print $2}' ;;
    k3d) k3d version 2>/dev/null | head -1 | awk '{print $NF}' ;;
    colima) colima version 2>/dev/null | head -1 | awk '{print $NF}' ;;
    docker) docker version --format '{{.Client.Version}}' 2>/dev/null ;;
  esac
}

# install_tools [cluster-type]: everything `make up` needs for that cluster type.
install_tools() {
  local type=${1:-$CLUSTER}
  [ "$type" = auto ] && type=k3d
  log "Tools for $type clusters"
  ensure_tool kubectl
  ensure_tool helm
  ensure_tool yq "YAML processing"
  case "$type" in
    kind) ensure_tool docker "container CLI"; ensure_tool kind ;;
    k3d) ensure_tool docker "container CLI"; ensure_tool k3d ;;
    k3s) ensure_tool colima "VM running k3s" ;;
  esac
}

# --- container runtime -----------------------------------------------------------------
ensure_docker() {
  if docker_ok; then ok "Docker daemon: $(docker info --format '{{.Name}}, {{.NCPU}} CPUs, {{.MemTotal}}' 2>/dev/null | awk -F', ' '{printf "%s, %s, %.1f GiB", $1, $2, $3/1073741824}')"; return 0; fi
  bad "Docker daemon not reachable (kind and k3d run Kubernetes nodes as containers)"
  local app=""
  [ -d /Applications/OrbStack.app ] && app=OrbStack
  [ -z "$app" ] && [ -d /Applications/Docker.app ] && app=Docker
  if [ -n "$app" ]; then
    if confirm "Start $app?"; then
      open -a "$app"
      log "waiting for the Docker daemon"
      local i=0
      while ! docker_ok; do i=$((i+1)); [ $i -gt 90 ] && die "$app did not start within 3 minutes"; sleep 2; done
      ok "Docker daemon up"; return 0
    fi
    die "$app is installed but not running. Start it (open -a $app), or let this script start it: AUTO_INSTALL=yes"
  fi
  if have colima; then
    if confirm "Start Colima (colima start --cpu $CPUS --memory $MEMORY --disk $DISK)?"; then
      colima start --cpu "$CPUS" --memory "$MEMORY" --disk "$DISK" || die "colima start failed"
      ok "Colima started"; return 0
    fi
    die "no Docker daemon. Start one (colima start / Docker Desktop / OrbStack) and re-run"
  fi
  if have brew && confirm "No Docker runtime found. Install Colima + Docker CLI with Homebrew and start it ($CPUS CPU, ${MEMORY} GiB, ${DISK} GiB disk)?"; then
    brew install colima docker || die "brew install colima docker failed"
    colima start --cpu "$CPUS" --memory "$MEMORY" --disk "$DISK" || die "colima start failed"
    ok "Colima started"; return 0
  fi
  die "no Docker runtime. Install one of: Colima (brew install colima docker), OrbStack, Docker Desktop — or run: make cluster-install"
}

# --- cluster lifecycle --------------------------------------------------------------------
cluster_exists() {
  case "$1" in
    kind) have kind && kind get clusters 2>/dev/null | grep -qx "$CLUSTER_NAME" ;;
    k3d) have k3d && k3d cluster list -o json 2>/dev/null | yq -p json -oy -e ".[] | select(.name == \"$CLUSTER_NAME\")" >/dev/null 2>&1 ;;
    k3s) have colima && colima list -j 2>/dev/null | yq -p json -oy -e "select(.name == \"$CLUSTER_NAME\")" >/dev/null 2>&1 ;;
  esac
}

cluster_create() {
  local type=$1
  case "$type" in kind|k3d|k3s) ;; *) die "CLUSTER must be kind, k3d or k3s (got '$type')" ;; esac
  install_tools "$type"
  [ "$type" = k3s ] || ensure_docker
  if cluster_exists "$type"; then
    ok "$type cluster '$CLUSTER_NAME' already exists"
    cluster_start "$type"
    kubectl config use-context "$(cluster_kind_ctx "$type")" >/dev/null
    return 0
  fi
  log "Creating $type cluster '$CLUSTER_NAME'"
  case "$type" in
    kind)
      local cfg; cfg=$(mktemp)
      cat > "$cfg" <<KIND
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
nodes:
  - role: control-plane
    extraPortMappings:
      - {containerPort: 30300, hostPort: 30300}   # Grafana (observability)
      - {containerPort: 30843, hostPort: 30843}   # simulation API (hub / agent-remote)
KIND
      kind create cluster --name "$CLUSTER_NAME" --config "$cfg" --wait 180s || die "kind create cluster failed"
      rm -f "$cfg" ;;
    k3d)
      k3d cluster create "$CLUSTER_NAME" --servers 1 --agents 0 \
        -p "30300:30300@server:0" -p "30843:30843@server:0" \
        --k3s-arg "--disable=traefik@server:0" --wait || die "k3d cluster create failed" ;;
    k3s)
      # A dedicated Colima profile: k3s in a VM, containerd runtime (needed for Mokka/NRI).
      colima start -p "$CLUSTER_NAME" --kubernetes --runtime containerd \
        --cpu "$CPUS" --memory "$MEMORY" --disk "$DISK" || die "colima start --kubernetes failed" ;;
  esac
  kubectl config use-context "$(cluster_kind_ctx "$type")" >/dev/null
  ok "cluster ready: context $(cluster_kind_ctx "$type")"
}

cluster_start() {
  case "$1" in
    kind) local c; for c in $(docker ps -a --filter "label=io.x-k8s.kind.cluster=$CLUSTER_NAME" --format '{{.Names}}' 2>/dev/null); do docker start "$c" >/dev/null 2>&1 || true; done ;;
    k3d) k3d cluster start "$CLUSTER_NAME" >/dev/null 2>&1 || true ;;
    k3s) colima status -p "$CLUSTER_NAME" >/dev/null 2>&1 || colima start -p "$CLUSTER_NAME" ;;
  esac
}

cluster_delete() {
  local type=$1
  [ "$type" = auto ] && { detect_context; type=$KIND_OF; }
  case "$type" in kind|k3d|k3s) ;; *) die "set CLUSTER=kind|k3d|k3s (current context '$CTX' is not one this Makefile created)" ;; esac
  cluster_exists "$type" || { ok "no $type cluster '$CLUSTER_NAME'"; return 0; }
  confirm "Delete the $type cluster '$CLUSTER_NAME' and everything in it?" || die "not deleted"
  case "$type" in
    kind) kind delete cluster --name "$CLUSTER_NAME" ;;
    k3d) k3d cluster delete "$CLUSTER_NAME" ;;
    k3s) colima delete -p "$CLUSTER_NAME" -f ;;
  esac
  ok "deleted"
}

# cluster_install: tools + runtime + cluster, for ENV's recommended type unless CLUSTER is set.
cluster_install() {
  local type=$CLUSTER env=${1:-}
  if [ "$type" = auto ]; then
    if [ -n "$env" ]; then env_meta "$env"; type=$RECOMMENDED; else type=k3d; fi
  fi
  log "Installing a local $type cluster '$CLUSTER_NAME'"
  cluster_create "$type"
}

# --- preflight ------------------------------------------------------------------------------
cpu_m() { awk '{ if ($1 ~ /m$/) { sub(/m$/,"",$1); s+=$1 } else s+=$1*1000 } END { print s+0 }'; }
mem_gi() { awk '{ v=$1; if (v ~ /Ki$/) { sub(/Ki$/,"",v); v/=1048576 } else if (v ~ /Mi$/) { sub(/Mi$/,"",v); v/=1024 } else if (v ~ /Gi$/) { sub(/Gi$/,"",v) } else { v/=1073741824 }; s+=v } END { printf "%.1f", s }'; }

# preflight <env>: verify (and offer to fix) everything needed to run <env> locally.
preflight() {
  local env=$1
  env_meta "$env"
  log "Preflight for ${B}$env${N}"
  ensure_tool kubectl
  ensure_tool helm
  ensure_tool yq "YAML processing"

  detect_context
  if [ -n "$CTX" ] && ! api_ok; then
    bad "kubectl context '$CTX' ($SERVER) is not reachable"
    case "$KIND_OF" in
      kind|k3d)
        if ! docker_ok; then ensure_docker; fi
        if server_is_local; then
          local name=${CTX#*-}
          if [ "$name" = "$CLUSTER_NAME" ] || confirm "Start the stopped $KIND_OF cluster '$name'?"; then
            CLUSTER_NAME=$name cluster_start "$KIND_OF"
            local i=0; while ! api_ok; do i=$((i+1)); [ $i -gt 60 ] && break; sleep 2; done
          fi
        fi ;;
      k3s)
        case "$CTX" in colima-*) CLUSTER_NAME=${CTX#colima-} cluster_start k3s ;; esac ;;
    esac
  fi
  if [ -z "$CTX" ] || ! api_ok; then
    local want=$CLUSTER; [ "$want" = auto ] && want=$RECOMMENDED
    if [ -z "$CTX" ]; then bad "kubectl has no current context: no cluster configured"; fi
    if confirm "Create a local $want cluster '$CLUSTER_NAME' now (recommended for $env)?"; then
      cluster_create "$want"; detect_context
    else
      die "no reachable cluster. Create one with:  make cluster-install CLUSTER=$want   (or start yours and select it with kubectl config use-context)"
    fi
  fi
  api_ok || die "cluster '$CTX' still not reachable"
  ok "context $CTX ($SERVER)"

  if ! server_is_local && [ "$ALLOW_REMOTE" != 1 ]; then
    die "context '$CTX' points at $SERVER, which does not look like a local cluster.
       This Makefile only targets local kind / k3d / k3s clusters. Switch context
       (kubectl config use-context kind-$CLUSTER_NAME | k3d-$CLUSTER_NAME | colima-$CLUSTER_NAME),
       create one (make cluster-install), or set ALLOW_REMOTE=1 if you really mean it."
  fi
  if [ "$CLUSTER" != auto ] && [ "$KIND_OF" != "$CLUSTER" ]; then
    die "CLUSTER=$CLUSTER but the current context '$CTX' is a ${KIND_OF} cluster. Switch to $(cluster_kind_ctx "$CLUSTER") (make cluster-install CLUSTER=$CLUSTER) or drop CLUSTER."
  fi
  local kind_ok=0 s
  for s in $SUPPORTED; do [ "$s" = "$KIND_OF" ] && kind_ok=1; [ "$s" = other ] && [ "$KIND_OF" = unknown ] && kind_ok=1; done
  if [ $kind_ok = 0 ]; then
    die "$env does not run on a $KIND_OF cluster (supported: $SUPPORTED).
       $NOTE
       Create a suitable one:  make cluster-install CLUSTER=$RECOMMENDED CLUSTER_NAME=${env}"
  fi
  ok "cluster type $KIND_OF supported for $env"

  local minor
  minor=$(kubectl version -o yaml 2>/dev/null | yq '.serverVersion.minor' | tr -dc '0-9')
  [ -n "$minor" ] && [ "$minor" -lt 28 ] && die "Kubernetes 1.$minor is too old (need >= 1.28)"
  ok "Kubernetes $(kubectl version -o yaml 2>/dev/null | yq '.serverVersion.gitVersion')"

  local arches cpu mem nodes
  nodes=$(kubectl get nodes -l 'type!=kwok' -o name | wc -l | tr -d ' ')
  arches=$(kubectl get nodes -l 'type!=kwok' -o jsonpath='{range .items[*]}{.status.nodeInfo.architecture}{"\n"}{end}' | sort -u | tr '\n' ' ')
  if [ "$ARCH" != any ] && ! printf '%s' "$arches" | grep -qw "$ARCH"; then
    die "$env needs $ARCH nodes; this cluster has: $arches(host: $(host_arch)).
       $NOTE
       On Apple Silicon this environment cannot run locally; use an x86_64 machine or VM."
  fi
  cpu=$(kubectl get nodes -l 'type!=kwok' -o jsonpath='{range .items[*]}{.status.allocatable.cpu}{"\n"}{end}' | cpu_m)
  mem=$(kubectl get nodes -l 'type!=kwok' -o jsonpath='{range .items[*]}{.status.allocatable.memory}{"\n"}{end}' | mem_gi)
  ok "$nodes node(s), arch $arches, $((cpu/1000)) CPU, ${mem} GiB allocatable"
  local short=0
  [ $((cpu/1000)) -lt "$REQ_CPU" ] && short=1
  awk -v m="$mem" -v r="$REQ_MEM" 'BEGIN{exit !(m < r)}' && short=1
  if [ $short = 1 ]; then
    if [ "${FORCE:-0}" = 1 ]; then warn "$env wants ~$REQ_CPU CPU / ${REQ_MEM} GiB; continuing (FORCE=1)"
    else die "$env needs about $REQ_CPU CPU / ${REQ_MEM} GiB; the cluster has $((cpu/1000)) CPU / ${mem} GiB.
       Give the container VM more (e.g. colima stop && colima start --cpu 8 --memory 16, or Docker/OrbStack settings), or FORCE=1."; fi
  else ok "resources OK for $env (~$REQ_CPU CPU / ${REQ_MEM} GiB)"; fi
  [ -n "$NOTE" ] && warn "$NOTE"

  local d
  for d in $DEPS; do
    if kubectl get ns -l "testenv.onit.ai/name=$d" -o name 2>/dev/null | grep -q .; then ok "dependency $d is up"
    else warn "dependency $d is not up; 'make up' deploys it first"; fi
  done
  env_preflight "$env"
}

# env_preflight <env>: environment-specific checks and fixes.
env_preflight() {
  case "$1" in
    nvidia-sim|nvidia-dynamo-disagg)
      if [ "$KIND_OF" = k3s ] && ! kubectl get runtimeclass nvidia >/dev/null 2>&1; then
        warn "k3s needs the one-time host prep for Mokka (environments/nvidia-sim/scripts/k3s-host-prep.sh)"
        case "$CTX" in
          colima-*)
            if confirm "Run the host prep inside the Colima VM '${CTX#colima-}' now (restarts k3s)?"; then
              colima ssh -p "${CTX#colima-}" -- sudo bash -s < "$ROOT/environments/nvidia-sim/scripts/k3s-host-prep.sh" || die "host prep failed"
              local i=0; while ! api_ok; do i=$((i+1)); [ $i -gt 60 ] && die "k3s did not come back"; sleep 2; done
              ok "host prep done"
            else die "run the host prep on the k3s node first"; fi ;;
          *) die "run environments/nvidia-sim/scripts/k3s-host-prep.sh as root on the k3s node first" ;;
        esac
      fi ;;
    network-kne)
      local n img=""
      n=$(node_container)
      if [ -n "$n" ]; then img=$(docker exec "$n" crictl images 2>/dev/null | grep -i xrd || true); fi
      if [ -n "$img" ]; then ok "XRd image on the node: $(echo "$img" | head -1 | awk '{print $1":"$2}')"
      else warn "no XRd image found on the node; routers will not start until you import it (environments/network-kne/README.md, 'Router image'). For kind: kind load image-archive <xrd.tgz> --name $CLUSTER_NAME"; fi ;;
  esac
}

# node_container: the Docker container of the (first) node for kind/k3d, else empty.
node_container() {
  case "$KIND_OF" in
    kind) echo "${CTX#kind-}-control-plane" ;;
    k3d) echo "k3d-${CTX#k3d-}-server-0" ;;
  esac
}
