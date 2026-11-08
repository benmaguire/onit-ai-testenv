# Architecture

## Flow

```mermaid
flowchart LR
    subgraph git[this repo, branch main]
      envyaml["environments/*/env.yaml<br/>enabled / state / targetRevision"]
      chart["environments/&lt;env&gt;/<br/>Helm chart (the specs)"]
    end
    appset[ApplicationSet<br/>testenvs] -- git files generator --> envyaml
    appset -- "enabled: \"true\" only" --> app["Application<br/>testenv-&lt;env&gt;"]
    app -- renders, passes state --> chart
    chart --> res[Kubernetes resources<br/>or child Applications]
```

1. [`argocd/applicationset.yaml`](../argocd/applicationset.yaml) uses a git
   **files generator** over `environments/*/env.yaml`, with a post-selector on
   `enabled: "true"`.
2. For each enabled environment it creates an Application `testenv-<name>`
   pointing at that environment's directory, rendered as a Helm chart, at the
   environment's `targetRevision`.
3. `state` from `env.yaml` and some `global.*` values (env name, repo URL,
   revision, project) are passed to the chart as Helm values.
4. The chart either contains the resources directly (`qa`) or is an
   **app-of-apps** rendering one child Application per component
   (`nvidia-sim`), each with its own namespace, source and sync options.

## Why these choices

| Choice | Reason |
|---|---|
| ApplicationSet over hand-written Applications | Adding an env = adding a directory. Disabling an env = one line. The ApplicationSet deletes Applications that drop out of the generator. |
| `env.yaml` separate from `values.yaml` | Lifecycle (on/off/scaled) changes often and is scriptable; the spec (`values.yaml`) changes rarely and is reviewed. |
| Every env is a Helm chart | One uniform way to pass `state` in, so "scale to zero" works the same in every env. |
| `resources-finalizer.argocd.argoproj.io` on every Application | Deleting an Application cascades to what it deployed, so removal frees the cluster. |
| Child Applications per component (nvidia-sim) | Each upstream chart wants its own namespace; components can be removed individually; ordering via sync waves. |
| `ServerSideApply=true` | Operator CRDs (GPU Operator, Dynamo) exceed the client-side apply annotation limit. |
| Automated sync with `prune` + `selfHeal` | Git is the source of truth. Manual edits in the cluster are reverted. |

## Revisions

- The ApplicationSet reads `env.yaml` files from `main` (`spec.generators[0].git.revision`).
- Each env deploys its specs from `targetRevision` in its `env.yaml`. To test a
  spec change from a branch, set `targetRevision: my-branch` on `main` (or point
  the ApplicationSet at your branch on a scratch cluster).

## Labels

| Label | On | Used for |
|---|---|---|
| `app.kubernetes.io/part-of: testenvs` | everything | finding all test env objects |
| `testenv.onit.ai/name: <env>` | Applications, namespaces, workloads | `envctl.sh status/sync/purge` |
| `testenv.onit.ai/component: <c>` | child Applications | per-component lookups |
| `testenv.onit.ai/state: up\|down` | namespaces / top-level workloads | seeing state from `kubectl` |

## Argo CD requirements

- Argo CD **2.10+** (ApplicationSet post-selectors with Go templates,
  `valuesObject`, `PostDelete` hooks). Developed against 2.14.
- The ApplicationSet controller enabled (default in Argo CD installs).
- Optional: the Application health customization in
  [`argocd/argocd-cm-application-health.yaml`](../argocd/argocd-cm-application-health.yaml),
  so child Application sync waves wait for the previous wave to be Healthy.
