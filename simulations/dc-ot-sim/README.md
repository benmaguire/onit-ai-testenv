# dc-ot-sim simulations

For the [dc-ot-sim environment](../../environments/dc-ot-sim/README.md): the
data centre OT plant simulator (otsim). The `ot-*` types inject faults through
otsim's admin API (`http://otsim.dc-ot-sim.svc:8080`, or `--param otsimURL=` /
the controller's `--otsim-url`). The plant model then plays out the
consequences on every protocol: UPS on battery, temperatures rising, leak
alarms, devices going silent.

| Area | Types |
|---|---|
| Power | `ot-utility-outage`, `ot-utility-sag`, `ot-frequency-deviation`, `ot-generator-fail-to-start`, `ot-ups-battery-degraded`, `ot-ups-failure`, `ot-pdu-breaker-trip`, `ot-pdu-overload`, `ot-it-load-surge` |
| Cooling / water | `ot-cdu-pump-failure`, `ot-cdu-leak`, `ot-leak`, `ot-crah-failure`, `ot-chiller-trip`, `ot-high-ambient` |
| Monitoring path | `ot-sensor-stuck`, `ot-sensor-drift`, `ot-comms-loss` (modbus / snmp / redfish / all) |
| The collector itself | generic faults on `dcim-collector`, e.g. `net-latency`, `network-isolate` |

**Recovery** removes the fault. The simulation counts as recovered only once
the site has no more critical alarms than at injection, so the reports show
how long the *plant* took to come back (temperatures settling, the ATS
retransfer delay), not just when the API call returned.

**Metrics profile `ot`:** `ot_pue`, `ot_it_kw`, `ot_facility_kw`,
`ot_rack_inlet_c`, `ot_cdu_return_max_c`, `ot_ups_charge_min_pct`,
`ot_alarms_critical`, `ot_water_l_per_h`, and `dcim_scrape_errors` (Telegraf
gather errors: did the DCIM notice comms loss?).

```bash
# Outage with a generator that won't start: how long does the battery last?
simctl inject ot-generator-fail-to-start --param device=gen-1 --duration 5m --wait active
simctl inject ot-utility-outage --duration 4m --observe ot --baseline 1m --after 2m --wait observed

# Total CDU flow loss; recovered once return temperature and alarms clear
simctl inject ot-cdu-pump-failure --param device=cdu-1 --param pump=0 --duration 2m \
  --observe ot --baseline 30s --after 5m --wait observed

# Does the DCIM alarm on a UPS that stops answering SNMP?
simctl inject ot-comms-loss --param device=ups-1 --param protocol=snmp --duration 1m --observe ot --wait observed

make sim-suite ENV=dc-ot-sim                   # suite.txt
```

To combine OT faults with IT faults (a CDU failure while `nvidia-sim` serves
traffic, say), run the two simulations side by side. Each one is measured
separately, and the Grafana dashboards annotate both.
