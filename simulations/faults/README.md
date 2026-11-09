# Generic fault injection

Faults that work on **any workload** in any environment. Each one is a
`Simulation` type ([catalog](../controller/CATALOG.md#generic-faults-any-workload)),
applied from an ephemeral container in each target pod, so it affects only
those pods, and is reverted and verified at the end.

| Fault | Type | Key params | Typical question |
|---|---|---|---|
| Latency | `net-latency` | `delay`, `jitter`, `destinations`, `ports` | Do timeouts and retries behave when a dependency gets slow? |
| Jitter | `net-jitter` | `delay`, `jitter`, `distribution`, `correlation` | Do streaming clients (SSE token streams, gRPC) cope with variable latency? |
| Packet loss | `net-packet-loss` | `loss`, `lossCorrelation` (bursts) | Does throughput collapse? Do health checks flap? |
| Corruption | `net-corruption` | `corrupt` | Checksums, retransmits |
| Duplication | `net-duplication` | `duplicate` | Idempotency of UDP or at-least-once protocols |
| Reordering | `net-reorder` | `delay`, `reorder` | Protocols that assume order |
| Bandwidth | `net-bandwidth` | `rate` | Large payloads (model downloads, long generations) on a thin link |
| Everything | `net-degraded` | any combination | A bad WAN or congested cross-zone link |
| Dependency outage | `dependency-outage` | `destinations` (`svc/<name>`, IP, CIDR), `ports`, `mode` reject/drop | Database / cache / model store / downstream API is down, as the caller sees it |
| DNS outage | `dns-outage` | `mode` | Name resolution fails while existing connections keep working |
| Service outage | `service-outage` | `service` | A Service stops answering everyone (its pods stay Ready) |
| CPU pressure | `cpu-stress` | `workers`, `load` | Throttling and latency under a noisy neighbour in the pod |
| Memory pressure | `memory-stress` | `size` | Page-cache pressure, then OOM kills at the pod limit |
| Crash | `container-kill` | `signal` (TERM, INT, QUIT, HUP: one the app handles) | Restart in place: readiness, warm-up, in-flight requests. For an abrupt kill use `pod-kill` (SIGKILL cannot reach PID 1 from inside the pod) |
| Pod loss | `pod-kill` | `gracePeriodSeconds`, `count` | Rescheduling, PDBs, leader election |
| Partition | `network-isolate` | `direction` | The pod is cut off (ingress, egress or both) |

## Scoping

- **Which pods:** `--target-namespace` + `--pod-selector` (or `--graph`/`--component` for Dynamo), plus `--count`.
- **Which traffic:** `--param destinations=svc/db,10.0.0.0/8 --param ports=5432` limits a `net-*` fault to those
  destinations (Services resolve to ClusterIP and endpoint IPs). Without it, all egress is impaired.
- **Direction:** netem shapes **egress**. Slow a service's *responses* by targeting the service's pods; slow
  requests *to* it by targeting the callers with `destinations=svc/<it>`.
- **Interface:** `--param interface=eth1` for multi-interface pods (KNE routers).

## Recipes

```bash
# p95 latency budget: 200ms ±50ms between api and its database, measured
simctl inject net-latency --target-namespace app --pod-selector app=api \
  --param delay=200ms --param jitter=50ms --param destinations=svc/postgres --param ports=5432 \
  --duration 5m --baseline 2m --after 2m --observe kubernetes \
  --probe-http http://api.app:8080/healthz --wait observed

# Flaky network: 3% loss in bursts plus 30ms jitter
simctl inject net-degraded --target-namespace app --pod-selector app=api \
  --param delay=20ms --param jitter=30ms --param loss=3 --param lossCorrelation=50 --duration 10m

# The cache is down (fast failures) vs. black-holed (timeouts)
simctl inject dependency-outage --target-namespace app --pod-selector app=api --param destinations=svc/redis --param mode=reject
simctl inject dependency-outage --target-namespace app --pod-selector app=api --param destinations=svc/redis --param mode=drop

# The whole service is down for its clients
simctl inject service-outage --target-namespace app --param service=api --duration 2m \
  --probe-http http://api.app:8080/healthz --baseline 1m --after 1m --wait observed

# CPU and memory pressure
simctl inject cpu-stress --target-namespace app --pod-selector app=api --param workers=2 --param load=80 --duration 5m
simctl inject memory-stress --target-namespace app --pod-selector app=api --param size=900Mi --duration 2m
```

## Requirements and limits

- Ephemeral containers (Kubernetes ≥ 1.25) and the `nicolaka/netshoot:v0.16`
  image pullable by the node (`--tools-image` to use a mirror).
- Network faults need `NET_ADMIN` (and `NET_RAW` for iptables). The
  restricted and baseline Pod Security profiles reject that, so label test
  namespaces `pod-security.kubernetes.io/enforce=privileged`.
- One `tc` fault per pod at a time. A second overlapping `net-*` simulation
  on the same pod is refused.
- IPv4 only for destination filters. Ingress shaping isn't supported (target
  the other side instead).
- `service-outage` and `network-isolate` need a CNI that enforces
  NetworkPolicy (k3s does by default).
