# API reference

Base URL: the agent or hub API (`:8443`, plain HTTP unless `--tls-cert-file`
is set). Every call except `/healthz` and `/readyz` needs
`Authorization: Bearer <token>`. Errors are `{"error": "..."}` with a 4xx/5xx status.
Simulations are JSON-encoded `sim.testenv.onit.ai/v1alpha1` objects.

## Agent (and hub, for its own cluster)

| Method | Path | Body / query | Response |
|---|---|---|---|
| GET | `/healthz`, `/readyz` | — | `ok` (no auth) |
| GET | `/version` | — | `{name, version, mode, cluster, environment, activeSimulations}` |
| GET | `/api/v1/catalog` | — | `[catalog entry]`: type, category, summary, effect, recovery, targets, params, requires, references |
| GET | `/api/v1/simulations` | `?namespace=&labelSelector=&phase=` | `SimulationList` |
| POST | `/api/v1/simulations` | `CreateRequest` | `201 Simulation`; `400` on unknown type / invalid params (validated before creation) |
| GET | `/api/v1/simulations/{ns}/{name}` | — | `Simulation` |
| POST | `/api/v1/simulations/{ns}/{name}/recover` | — | `202 Simulation` (sets `spec.recover`) |
| DELETE | `/api/v1/simulations/{ns}/{name}` | — | `204`; the fault is reverted by the finalizer |
| POST | `/api/v1/recover-all` | `?namespace=&labelSelector=` | `202 {"recovered": ["ns/name", ...]}` |

`CreateRequest`:

```json
{
  "name": "optional, default <type>-xxxxx",
  "namespace": "optional, default the server's namespace (sim-system)",
  "labels": {"testrun": "nightly-42"},
  "spec": {
    "type": "gpu-xid",
    "target": {"graph": "mocker-agg", "component": "decode", "namespace": "nvidia-sim"},
    "params": {"xid": "79", "mode": "fallen_off_bus"},
    "delay": "10s",
    "duration": "2m",
    "recoveryTimeout": "5m",
    "ttlAfterFinished": "1h"
  }
}
```

`spec.target` fields: `node`, `nodeSelector`, `namespace`, `podSelector`,
`graph`, `component`, `gpu` (index | `all` | UUID), `count`. Which ones a type
uses is in its catalog entry. Durations use Go syntax (`90s`, `2m`, `1h30m`).

Status fields of interest: `phase`, `message`, `injectedAt`, `recoverAt`,
`recoveryStartedAt`, `recoveredAt`, `targets[]` (`kind`, `namespace`, `name`,
`node`, `gpu`, `detail`), `data` (`timeToRecover`, `recoveryDuration`, and
injector state), `conditions` (`Injected`, `Recovered`).

## Hub only

| Method | Path | Notes |
|---|---|---|
| GET | `/api/v1/clusters` | `[{name, endpoint, environment, local, reachable, agentVersion, activeSimulations, lastSeen, message}]` |
| POST | `/api/v1/clusters/register` | Agent self-registration: `{name, endpoint, token, environment, caBundle, insecureSkipTLSVerify}`. Accepts the registration token **or** the API token. |
| DELETE | `/api/v1/clusters/{cluster}` | Remove a cluster and its token Secret |
| ANY | `/api/v1/clusters/{cluster}/{path}` | Routed to that cluster's `/api/v1/{path}` (`.../version` → `/version`). The hub's own name is served locally. |
| GET | `/api/v1/fleet/simulations` | `?labelSelector=` → `[{cluster, result: [Simulation], error}]` |
| POST | `/api/v1/fleet/recover-all` | `?labelSelector=` → `[{cluster, result: {recovered}, error}]` |

## curl

```bash
H="Authorization: Bearer $TOKEN"
curl -sH "$H" $SERVER/api/v1/catalog | jq -r '.[].type'
curl -sH "$H" -H 'Content-Type: application/json' $SERVER/api/v1/clusters/lab-2/simulations \
  -d '{"labels":{"testrun":"x"},"spec":{"type":"dynamo-worker-kill","target":{"count":1}}}'
curl -sH "$H" "$SERVER/api/v1/clusters/lab-2/simulations?labelSelector=testrun=x" | jq '.items[].status.phase'
curl -sXPOST -H "$H" "$SERVER/api/v1/fleet/recover-all?labelSelector=testrun=x"
```
