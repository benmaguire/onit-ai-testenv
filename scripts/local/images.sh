# shellcheck shell=bash
# Build images from this repo and make them available to the local cluster's
# container runtime (no registry needed).

SIMULATOR_IMG=docker.io/testenv/simulator:local
KNE_RUNNER_IMG=docker.io/testenv/kne-runner:local

# docker_build <context-dir> <image>: builds for the cluster nodes' architecture,
# with or without BuildKit (Homebrew's docker CLI may lack the buildx plugin).
docker_build() {
  local arch
  arch=$(kubectl get nodes -l 'type!=kwok' -o jsonpath='{.items[0].status.nodeInfo.architecture}' 2>/dev/null || true)
  [ -n "$arch" ] || arch=$(host_arch)
  docker build -q --platform "linux/$arch" \
    --build-arg "BUILDPLATFORM=linux/$(host_arch)" --build-arg TARGETOS=linux --build-arg "TARGETARCH=$arch" \
    -t "$2" "$1" >/dev/null || die "docker build $1 failed"
}

# build_load <context-dir> <image>
build_load() {
  local dir=$1 img=$2
  [ "$DRY_RUN" = 1 ] && { ok "[dry-run] would build $img from ${dir#"$ROOT"/}"; return 0; }
  log "Building $img from ${dir#"$ROOT"/} for the $KIND_OF cluster"
  case "$KIND_OF" in
    kind)
      docker_ok || ensure_docker
      docker_build "$dir" "$img"
      kind load docker-image "$img" --name "${CTX#kind-}" >/dev/null || die "kind load $img failed" ;;
    k3d)
      docker_ok || ensure_docker
      docker_build "$dir" "$img"
      k3d image import "$img" -c "${CTX#k3d-}" >/dev/null 2>&1 || die "k3d image import $img failed" ;;
    k3s)
      case "$CTX" in
        colima-*) colima nerdctl -p "${CTX#colima-}" -- --namespace k8s.io build -t "$img" "$dir" >/dev/null || die "nerdctl build in Colima failed" ;;
        *) die "cannot load images into '$CTX' automatically; push $img to a registry the cluster can pull from" ;;
      esac ;;
    *)
      docker_ok || ensure_docker
      docker_build "$dir" "$img"
      warn "$CTX: assuming the cluster shares the Docker image store (Docker Desktop / OrbStack)" ;;
  esac
  ok "$img available in the cluster"
}

# local_images <env>: images an environment needs that are built from this repo.
local_images() {
  case "$1" in
    network-kne)
      if [ -z "${KNE_RUNNER_IMAGE:-}" ]; then build_load "$ROOT/environments/network-kne/runner" "$KNE_RUNNER_IMG"; fi ;;
  esac
}
