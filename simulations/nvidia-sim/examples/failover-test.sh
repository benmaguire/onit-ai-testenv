#!/usr/bin/env bash
# Example test-runner flow: baseline request, kill the worker's GPU, check the
# frontend's behaviour during the fault, recover, check again.
#   SIMCTL_SERVER=https://hub:30843 SIMCTL_TOKEN=... SIMCTL_CLUSTER=lab-2 \
#   FRONTEND=http://localhost:8000 ./failover-test.sh
set -euo pipefail
: "${FRONTEND:=http://localhost:8000}"
RUN="failover-$(date +%s)"

chat() {
  curl -s -o /dev/null -w '%{http_code}' --max-time 20 "$FRONTEND/v1/chat/completions" \
    -H 'Content-Type: application/json' \
    -d '{"model":"Qwen/Qwen3-0.6B","messages":[{"role":"user","content":"hi"}],"max_tokens":8}'
}

trap 'simctl recover-all --selector testrun=$RUN >/dev/null || true' EXIT   # never leave a fault behind

echo "baseline: HTTP $(chat)"

SIM=$(simctl inject gpu-lost --graph mocker-agg --component decode \
        --label testrun="$RUN" --ttl 1h)
simctl wait "$SIM" --for active --timeout 2m
echo "during fault: HTTP $(chat)"

simctl recover "$SIM" --wait --timeout 5m
echo "after recovery: HTTP $(chat)"
simctl get "$SIM"
