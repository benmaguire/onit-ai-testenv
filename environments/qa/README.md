# qa

A namespace for QA workloads on the single-node k3s cluster. For now it is
**just the namespace** plus guard rails; QA workloads get added here later.

## What it deploys

| Resource | Name | Purpose |
|---|---|---|
| `Namespace` | `qa` | Home for QA workloads. Managed as a resource, so removing the env deletes it. |
| `ResourceQuota` | `qa-quota` | Caps the namespace (4 CPU / 8Gi requested, 8 CPU / 16Gi limits, 50 pods, 10 PVCs) so QA cannot starve the node. |
| `LimitRange` | `qa-defaults` | Default requests/limits (50m/64Mi → 500m/512Mi) for containers that set none, required for the quota to admit them. |

All tunable in [values.yaml](values.yaml); set `resourceQuota.enabled` or
`limitRange.enabled` to `false` to drop either.

Argo CD Application: `testenv-qa` (created by the `testenvs` ApplicationSet).

## Lifecycle

| | Effect |
|---|---|
| `state: up` | namespace + guard rails |
| `state: down` | the same today (nothing to scale). Workloads added later scale to 0 if they use the `testenv.replicas` helper. |
| `enabled: "false"` | the `qa` namespace and **everything in it** is deleted |

```bash
scripts/envctl.sh down qa --push
scripts/envctl.sh up qa --push
scripts/envctl.sh remove qa --push
```

## Adding QA workloads

Put manifests in [templates/](templates/) with `namespace: {{ .Values.namespace }}`
and replica counts from the helper, so `state: down` scales them to zero:

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: my-service
  namespace: {{ .Values.namespace }}
  labels:
    {{- include "testenv.labels" . | nindent 4 }}
spec:
  replicas: {{ include "testenv.replicas" (list . 2) }}
  ...
```

Remember that the ResourceQuota counts them; raise it in `values.yaml` if needed.

## Verify

```bash
kubectl get ns qa --show-labels
kubectl -n qa describe resourcequota qa-quota
helm template qa environments/qa      # local render
```

## Leftovers after removal

None — everything is inside the namespace.
