# Adding an environment

1. **Create the directory** `environments/<name>/` (lowercase, DNS-safe; it
   becomes `testenv-<name>` in Argo CD).

2. **Add `env.yaml`** — keep these keys, they are required by the ApplicationSet
   (`missingkey=error`):

   ```yaml
   name: <name>
   description: one line
   enabled: "false"        # start disabled until the specs are reviewed
   state: up
   targetRevision: main
   namespace: <default namespace for the Application>
   ```

3. **Add a Helm chart** (`Chart.yaml`, `values.yaml`, `templates/`). It receives:

   | Value | From |
   |---|---|
   | `state` | `env.yaml` (`up` / `down`) |
   | `global.envName`, `global.project` | ApplicationSet |
   | `global.repoURL`, `global.targetRevision` | ApplicationSet (for app-of-apps children pointing back into this repo) |

   Give `values.yaml` defaults for all of them so `helm template` works locally.

   - **Simple env:** put resources in `templates/`. Render the `Namespace` as a
     resource (not `CreateNamespace=true`) so removal deletes it. Copy
     `environments/qa/templates/_helpers.tpl` for the `testenv.replicas` helper
     and use it for every replica count so `state: down` works.
   - **Env built from upstream charts:** make the chart an app-of-apps like
     `environments/nvidia-sim` — one child `Application` per component with a
     `resources-finalizer.argocd.argoproj.io` finalizer, a `sync-wave`, and the
     `testenv.onit.ai/name` label.

4. **Write `README.md`** covering: purpose, components and versions,
   prerequisites (cluster/host), what `up`/`down`/removed mean for this env,
   how to verify it works, troubleshooting, and leftovers after removal.

5. **Allow new sources** in `argocd/project.yaml` (`sourceRepos`) and, for a new
   Helm repo, add it to `argocd/repositories.yaml`.

6. **Check it renders:** `helm lint environments/<name>` and
   `helm template <name> environments/<name>`.

7. Merge, then `scripts/envctl.sh up <name> --push`.
