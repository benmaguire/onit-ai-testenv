# nvidia-dra-sim simulations

For the [nvidia-dra-sim environment](../../environments/nvidia-dra-sim/README.md):
GPUs allocated by DRA. The GPU faults go through Mokka. With
`--pod-selector`, the controller resolves which GPU a pod holds, so a fault
lands on the device a ResourceClaim allocated.

| Simulation | Expectation |
|---|---|
| `gpu-xid` 79 on a `gpu-claimer` pod's GPU | the workload sees the GPU fall off the bus; the DRA driver keeps the allocation (no automatic remediation without NVSentinel) |
| `gpu-overheat` on the shared GPU | both `gpu-sharer` pods see the same GPU throttle |
| `pod-kill` on `gpu-claimer` | the claim from the template is released and a new one allocated for the replacement pod |
| `pod-kill` on the DRA kubelet plugin | existing allocations survive; new claims wait until it is back |

```bash
make sim-suite ENV=nvidia-dra-sim
kubectl -n nvidia-dra-sim get resourceclaims -w     # watch allocations during a run
```
