#!/usr/bin/env bash
# Called by scripts/envctl.sh purge nvidia-sim [--crds] after Argo CD removed the env.
# Cleans what Argo CD does not track:
#  - Dynamo custom resources stuck on finalizers once the Dynamo operator is gone
#  - (with --crds) the Dynamo CRDs, which the operator installs at runtime, and
#    the GPU Operator / NFD CRDs
set -euo pipefail
DELETE_CRDS=${1:-false}

for crd in $(kubectl get crd -o name | grep -E '\.nvidia\.com$' | grep -iE 'dynamo' | cut -d/ -f2); do
  for r in $(kubectl get "$crd" -A -o jsonpath='{range .items[*]}{.metadata.namespace}/{.metadata.name}{"\n"}{end}' 2>/dev/null); do
    ns=${r%%/*}; name=${r#*/}
    echo "removing finalizers from $crd $ns/$name"
    kubectl -n "$ns" patch "$crd" "$name" --type merge -p '{"metadata":{"finalizers":null}}' || true
  done
done

if [[ "$DELETE_CRDS" == "true" ]]; then
  for crd in $(kubectl get crd -o name | grep -E 'dynamo|grove|nvidia\.com|nfd\.k8s-sigs\.io' | cut -d/ -f2); do
    echo "deleting CRD $crd"; kubectl delete crd "$crd" --wait=false
  done
fi
