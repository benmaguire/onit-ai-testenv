# network-kne simulations

For the [network-kne environment](../../environments/network-kne/README.md):
a KNE topology `xrd-lab` with Cisco XRd routers r1 and r2 (OSPF + iBGP), and
Linux hosts host1 and host2:

```text
host1 192.168.1.10 ─ eth2 r1 eth1 ══ 192.168.12.0/30 ══ eth1 r2 eth2 ─ 192.168.2.10 host2
```

Router pods are labelled `app=r1` / `app=r2` in namespace `xrd-lab`. Linux
interface `eth1` is XR `GigabitEthernet0/0/0/0` (the r1–r2 link) and `eth2`
is `Gi0/0/0/1` (the host link).

## Network simulations

| Scenario | Simulation | Expect |
|---|---|---|
| Core link down | `link-down --pod-selector app=r1 --param interface=eth1` | OSPF/BGP adjacency drops; host1↔host2 ping fails; recovers when the link is up |
| Link flapping | `link-flap --pod-selector app=r1 --param interface=eth1 --param downSeconds=5 --param upSeconds=20` | repeated reconvergence; ping loss in bursts; BGP session flaps |
| High-latency core link | `net-latency --pod-selector app=r1 --param interface=eth1 --param delay=80ms --param jitter=10ms` | ping RTT +80ms one way; protocols stay up |
| Lossy core link | `net-packet-loss --pod-selector app=r1 --param interface=eth1 --param loss=10` | ping loss; OSPF/BGP survive moderate loss |
| Congested link | `net-bandwidth --pod-selector app=r1 --param interface=eth1 --param rate=1mbit` | throughput (iperf3 from the hosts) capped |
| Degraded WAN | `net-degraded ... --param delay=40ms --param jitter=15ms --param loss=1 --param rate=10mbit` | everything at once |
| Access link down | `link-down --pod-selector app=r1 --param interface=eth2` | host1 isolated; r1–r2 stays up |
| Router crash | `container-kill --pod-selector app=r1` | XR restarts in place (pod and links kept); measures reboot + reconvergence. Untested: XRd's PID 1 must exit on the chosen `signal` (try TERM, then INT) |
| Host misbehaviour | any generic fault on `app=host1` | e.g. `cpu-stress`, `net-duplication` from the host |

Do **not** use `pod-kill` on routers: KNE pods are bare pods, so nothing
recreates them. Use `container-kill`, or `scripts/envctl.sh down/up network-kne` to rebuild.

## Measuring

- **Ping probe across the network** (the end-to-end view):
  `--probe-exec 'xrd-lab/app=host1:ping -c1 -W1 192.168.2.10'`
- **Router telemetry** (gnmic → Prometheus): `--observe network` gives
  interface packets/errors and established BGP sessions.
- `--observe kubernetes` shows router container restarts.

```bash
simctl inject link-flap --target-namespace xrd-lab --pod-selector app=r1 --param interface=eth1 \
  --param downSeconds=5 --param upSeconds=25 --duration 5m \
  --probe-exec 'xrd-lab/app=host1:ping -c1 -W1 192.168.2.10' --probe-interval 1s \
  --observe network --baseline 1m --after 2m --label testrun=flap-1 --wait observed
simctl metrics <name> > flap-1.csv     # per-second ping results + router counters
```

The probe report's `LONGEST-OUTAGE` in the fault window is the worst
reconvergence time, and `AFTER` should be back at 100% success.

[examples/](examples/) has the same simulations as YAML.

## Notes

- `link-*` and `net-*` act on the Linux side of the XRd interface (the
  pod's network namespace), which XR sees as carrier loss or a degraded link,
  as with a cable or optics fault.
- netem shapes egress: `net-*` on `r1 eth1` affects r1→r2. Apply it to both
  routers for symmetric impairment.
- One `tc` fault and one link fault per router pod at a time.
