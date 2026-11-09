# shellcheck shell=bash
# Optional: run the real GitOps path locally (Argo CD + the ApplicationSet).
# Argo CD deploys from GitHub, so changes must be pushed; `make up` (no Argo CD)
# deploys from the working tree instead.

ARGOCD_CHART_VERSION=${ARGOCD_CHART_VERSION:-9.1.0}

argocd_install() {
  log "Installing Argo CD (chart argo-cd $ARGOCD_CHART_VERSION) into namespace argocd"
  helm upgrade --install argocd argo-cd --repo https://argoproj.github.io/argo-helm --version "$ARGOCD_CHART_VERSION" \
    -n argocd --create-namespace --set 'configs.params.server\.insecure=true' --wait --timeout 10m >/dev/null \
    || die "helm install argo-cd failed (set ARGOCD_CHART_VERSION to a published version)"
  kubectl -n argocd patch configmap argocd-cm --type merge --patch-file "$ROOT/argocd/argocd-cm-application-health.yaml" >/dev/null
  kubectl apply -k "$ROOT/argocd" >/dev/null || die "applying argocd/ failed"
  ok "Argo CD installed; the ApplicationSet deploys every environment with enabled: \"true\" on main"
  local pw
  pw=$(kubectl -n argocd get secret argocd-initial-admin-secret -o jsonpath='{.data.password}' 2>/dev/null | base64 --decode)
  ok "UI: kubectl -n argocd port-forward svc/argocd-server 8080:80  ->  http://localhost:8080 (admin / $pw)"
}
