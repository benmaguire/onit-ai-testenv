# nvidia-nvsentinel simulations

For the [nvidia-nvsentinel environment](../../environments/nvidia-nvsentinel/README.md).
Each simulation injects a GPU fault through Mokka. NVSentinel's GPU health
monitor should see it via DCGM, record a health event, quarantine the node
(cordon + taint) and drain the GPU workload. Recovering the simulation resets
the GPU, and NVSentinel then clears the quarantine.

| Simulation | Fault | Expected NVSentinel reaction |
|---|---|---|
| `xid-79-fallen-off` | `gpu-xid` 79 | fatal: quarantine + drain |
| `xid-48-dbe` | `gpu-ecc-uncorrectable` | fatal: quarantine + drain |
| `gpu-overheat-142` | `gpu-overheat` 142 °C | above the GPU's temperature limit: quarantine + drain |
| `gpu-lost` | `gpu-lost` | GPU missing: quarantine + drain |

What to compare across the windows (`--observe gpu --observe kubernetes`):
- `gpus_allocatable`, which drops while the node is quarantined;
- `ready_pods` in `nvidia-nvsentinel-workload`, which go Pending and then return;
- the time from injection to cordon, read from the node events:
  `kubectl get events -A --field-selector involvedObject.kind=Node`.

```bash
make sim-suite ENV=nvidia-nvsentinel
kubectl -n nvidia-nvsentinel logs deploy/fault-quarantine -f     # decisions as they happen
```
