# network-kne

Network emulation with [KNE](https://github.com/openconfig/kne) (Kubernetes
Network Emulation, openconfig) on single-node k3s. A basic topology of
**2 Cisco XRd routers** running OSPF + iBGP, with a Linux host behind each,
plus gNMI streaming telemetry into Prometheus.

```text
 host1 192.168.1.10/24 ── eth2 ┌────┐ eth1   192.168.12.0/30   eth1 ┌────┐ eth2 ── 192.168.2.10/24 host2
                               │ r1 │════════════════════════════════│ r2 │
               Gi0/0/0/1 .1    └────┘ Gi0/0/0/0 .1          .2 Gi0/0/0/0 └────┘ Gi0/0/0/1 .1
                Lo0 1.1.1.1    OSPF area 0 (p2p) + iBGP AS 65000 (loopbacks)    Lo0 2.2.2.2
```

## Components

| Order | Component | Source | Namespace | Argo CD Application |
|---|---|---|---|---|
| 0 | **meshnet** CNI: creates the point-to-point links between KNE pods (gRPC wire mode) | vendored from KNE v0.3.4 ([meshnet/base](meshnet/base/)), overlays for k3s CNI paths ([meshnet/k3s](meshnet/k3s/)) and standard paths ([meshnet/kind](meshnet/kind/)) | `meshnet` | `network-kne-meshnet` |
| 1 | **Topology** ([topology/](topology/)): ConfigMap with the KNE topology and router configs. A Job runs `kne create` on every sync (`kne delete` when down), and a PostDelete Job removes the topology namespace | `kne-runner` image ([runner/](runner/)) | `network-kne` (runner), `xrd-lab` (routers + hosts, created by KNE) | `network-kne-topology` |
| 2 | **Telemetry** ([telemetry/](telemetry/)): gnmic subscribes to interface counters / oper-status and BGP neighbor state on both routers, exports Prometheus metrics (`gnmic_*`) | `ghcr.io/openconfig/gnmic:0.49.1` | `network-kne` | `network-kne-telemetry` |

Topology source: [topology/files/topology.pb.txt](topology/files/topology.pb.txt)
and [router.config](topology/files/router.config) (templated per router).
It's checked against KNE v0.3.4's topology proto. Router services are **ClusterIP
only**: a KNE LoadBalancer service on k3s's ServiceLB would bind node ports,
including port 22.

## Prerequisites

1. **The Cisco XRd control-plane image.** It's not public: download
   `xrd-control-plane` from Cisco (software.cisco.com, needs a Cisco account
   and entitlement) and either import it on the node, or push it to a registry the
   node can pull from and set `components.topology.values.routerImage`:

   ```bash
   sudo environments/network-kne/scripts/k3s-host-prep.sh \
     --image xrd-control-plane-container-x64.dockerv1.tgz --tag ios-xr/xrd-control-plane:24.4.2
   ```

2. **Host prep** (same script, also without `--image`): sets the inotify
   sysctls XRd needs (`fs.inotify.max_user_instances=64000`,
   `max_user_watches=524288`) and checks k3s's CNI directories. `--check` only
   reports.
3. **k3s from October 2024 or later** (CNI binaries in `/var/lib/rancher/k3s/data/cni`).
   On older k3s, change the meshnet hostPaths in [meshnet/k3s/kustomization.yaml](meshnet/k3s/kustomization.yaml).
4. **x86_64 node** with ~2 CPU / 2 GiB per XRd (KNE's defaults) plus the hosts:
   about 5 CPU / 5 GiB free.
5. **The `kne-runner` image**: published by `.github/workflows/kne-runner.yaml`
   (push to `main` or run it manually) as `ghcr.io/benmaguire/onit-ai-testenv/kne-runner:v0.3.4`.
6. Argo CD 2.10+ (PostDelete hook for cleanup).

## Lifecycle

Starts disabled: `scripts/envctl.sh up network-kne --push`.

| | What runs |
|---|---|
| `state: up` | meshnet, topology (r1, r2, host1, host2), gnmic |
| `state: down` | topology deleted (`kne delete`), gnmic at 0; meshnet kept |
| `enabled: "false"` | everything removed. The PostDelete Job deletes the `xrd-lab` namespace |

Changing the topology or router configs in git recreates the topology on the
next sync. The runner compares a hash of the files with an annotation on the
`xrd-lab` namespace.

## Verify

```bash
kubectl -n network-kne logs job/kne-reconcile            # kne create output
kubectl -n xrd-lab get pods,svc                          # r1 r2 host1 host2, service-*-clusterip
kubectl -n xrd-lab exec host1 -- ping -c3 192.168.2.10   # end to end through both routers
kubectl -n xrd-lab exec -it r1 -- /pkg/bin/xr_cli.sh     # XR CLI
#   show ospf neighbor ; show bgp ipv4 unicast summary ; show ip route
kubectl -n network-kne port-forward svc/gnmic 9804 & curl -s localhost:9804/metrics | grep gnmic_interfaces | head
```

The router password is a lab default (`components.topology.values.routerPassword`, also used by gnmic). Change it.

## Simulations

[simulations/network-kne](../../simulations/network-kne/README.md): link down
and flap, latency/jitter/loss/bandwidth on router links, router crash, all
measured with host-to-host ping probes and gNMI counters (`--observe network`).

## Troubleshooting

| Symptom | Fix |
|---|---|
| `kne-reconcile` fails: `failed to create meshnet topologies` | meshnet not ready: `kubectl -n meshnet get pods`. The Job retries |
| Router pods `ErrImagePull` | XRd image not imported or `routerImage` mismatch: `sudo k3s ctr -n k8s.io images ls \| grep xrd` |
| Pods stuck in `Init` | meshnet can't wire links: check the meshnet pod logs and that `/var/lib/rancher/k3s/agent/etc/cni/net.d/00-meshnet.conflist` exists |
| XRd crash-loops | host prep not applied (inotify limits), or not enough CPU/memory |
| Router pods Running, ping fails | XR still booting (a few minutes), or check `show ospf neighbor` |
| gnmic has no metrics | gNMI on XR is `grpc no-tls` port 57400. Check credentials and `kubectl -n network-kne logs deploy/gnmic` |
