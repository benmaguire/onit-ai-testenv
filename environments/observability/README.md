# observability

Prometheus and Grafana for every test environment, and the metrics backend
for simulation **baseline / fault / recovery / after** measurements (see
[simulations/controller/docs/observability.md](../../simulations/controller/docs/observability.md)).

## Components

| Order | Component | Chart / version | Namespace | Argo CD Application |
|---|---|---|---|---|
| 0 | **kube-prometheus-stack**: Prometheus operator, Prometheus (7d retention, 15s scrape), Grafana (NodePort 30300), kube-state-metrics, node-exporter | `oci://ghcr.io/prometheus-community/charts/kube-prometheus-stack` `93.1.0` | `observability` | `observability-prometheus-stack` |
| 1 | **Monitors + dashboards** ([monitors/](monitors/)): what to scrape in each environment, and the *Test environments: simulations* dashboard | local chart | `observability` | `observability-monitors` |

| Monitor | Scrapes | Environment |
|---|---|---|
| `testenv-simulator` | simulation controller: `sim_probe_*`, `sim_simulation_active/phase` | any with the simulator |
| `dcgm-exporter` | GPU Operator dcgm-exporter (Mokka GPUs) and fake-gpu-operator's exporters | nvidia-sim, runai-sim |
| `dynamo-frontend`, `dynamo-workers` | Dynamo `:8000/metrics`, worker system port `:9090/metrics` | nvidia-sim, nvidia-dynamo-disagg |
| `triton` | Triton `:8002/metrics` | nvidia-triton-cpu |
| `kai-scheduler` | KAI components `:8080/metrics` | runai-sim |
| `gnmic` | router telemetry from gNMI | network-kne |

Switch any off under `components.monitors.values.targets` in [values.yaml](values.yaml).
Monitors without targets scrape nothing.

k3s-specific: scraping of kube-controller-manager, scheduler, proxy and etcd is
off, because k3s embeds them. The Prometheus operator's admission webhooks are
off (their Helm-hook cert jobs conflict with Argo CD). Alertmanager is off.

## Lifecycle

Starts disabled: `scripts/envctl.sh up observability --push`. Enable it
**before** the environments you test, so Prometheus history exists for
baselines.

| | What runs |
|---|---|
| `state: up` | everything |
| `state: down` | Grafana at 0. Prometheus keeps collecting so baselines stay available |
| `enabled: "false"` | removed (Prometheus data is on an emptyDir and is lost) |

## Use

```bash
# Grafana: http://<node-ip>:30300  (admin / testenv — change grafana.adminPassword)
kubectl -n observability port-forward svc/kube-prometheus-stack-prometheus 9090 &   # Prometheus
```

The simulation controller is pre-configured with
`--prometheus-url=http://kube-prometheus-stack-prometheus.observability.svc:9090`.
On a cluster without this environment, simulation probes still work and metric
queries report "no Prometheus".

## Resources

About 1 CPU / 2 GiB on a single node (Prometheus is limited to 2 GiB). Add
`prometheus.prometheusSpec.storageSpec` for persistent storage if baselines
must survive restarts.
