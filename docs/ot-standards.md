# Data centre OT: standards, and adapting to hardware and countries

This page covers the standards behind the [dc-ot-sim](../environments/dc-ot-sim/README.md)
simulator (`otsim`), and how to adapt it to a vendor's hardware or to another
country. Data centre OT is the plant around the IT equipment: power (utility,
switchgear, ATS, generators, UPS, PDUs, meters), cooling (chillers, CRAH/CRAC,
CDUs and liquid loops), water, and leak/environmental sensing.

## There is no single standard

A data centre has no single OT standard. Instead, the protocols and data models
sit in layers, and most real sites mix several of them:

| Layer | Standards | What otsim does |
|---|---|---|
| Device/data model for DC equipment | **DMTF Redfish** DCIM schemas: `PowerDistribution` (rack PDUs, switchgear, transfer switches), `Circuit`, `PowerDistributionMetrics`, `CoolingUnit` (CDUs, CRAH/CRAC, heat exchangers), `CoolantConnector`, `LeakDetection`/`LeakDetector`, `Facility`, under `PowerEquipment` and `ThermalEquipment` (DSP0268, DSP2046; DMTF/OCP "Redfish for cooling units" profiles) | Redfish service on :8443: CDUs, rack PDUs, leak detectors, facility; PATCH of setpoints |
| Network management (IT-style) | **SNMP**: UPS-MIB (RFC 1628), ENTITY-MIB (RFC 6933), ENTITY-SENSOR-MIB (RFC 3433), plus vendor enterprise MIBs | SNMPv2c agent: UPS-MIB for UPSs, ENTITY-SENSOR-MIB tables for PDUs |
| Fieldbus / plant control | **Modbus TCP** (de facto for UPS, gensets, CDUs, chillers, meters; vendor register maps), **SunSpec** Modbus models (inverters, meters, storage) | Modbus TCP per device (and a gateway with one unit ID per device); SunSpec model 1 + 203 meter; generic vendor-style maps |
| Building automation | **BACnet** (ASHRAE 135 / ISO 16484-5) for chillers, CRAH and BMS | Not yet (planned); use the Modbus maps or add a BACnet front end |
| Industrial interoperability | **OPC UA** (IEC 62541), used by newer switchgear and BMS integrations | Not yet (planned) |
| Electrical/fire/building design rules | IEC 60364 / **AS/NZS 3000** / BS 7671 / **NFPA 70 (NEC)**, NFPA 75/76, **IEC 62040** (UPS), ANSI/TIA-942, **EN 50600 / ISO/IEC 22237** | Encoded as region parameters (voltage, frequency) and documentation, not enforced |
| Thermal envelope | **ASHRAE TC 9.9** (recommended 18–27 °C inlet; A1 allowable 15–32 °C), ASHRAE 90.4 | Inlet warning/critical alarms at 27/32 °C |
| Efficiency metrics | **ISO/IEC 30134** (PUE, WUE, REF, ERF, CUE), The Green Grid | PUE, WUE, carbon, water computed from the model and exported |

Practical consequences:

- **Use Redfish for anything new.** It is the only one of these with a
  standard model for DC power and cooling equipment, and NVIDIA's and OCP's
  liquid-cooled rack work (e.g. GB200 NVL72 CDUs) is converging on it.
- **SNMP and Modbus remain what most installed plant speaks.** Their *meaning*
  is in the device's MIB or register map, which differs by vendor and often by
  firmware version. That's why otsim keeps the protocol maps in data files,
  outside the code.
- **BMS, EPMS and DCIM tools** (Schneider EcoStruxure, Vertiv, Siemens
  Desigo, Nlyte, Sunbird and others) normalise all of these. The `dcim-collector`
  in dc-ot-sim (Telegraf polling Modbus, SNMP and Redfish) plays that role in
  tests.

## How otsim is layered

```text
site.yaml (devices, sizes)         region profile (country)       protocol profiles (vendor)
          \                                 |                                /
           +--------->  plant model (power path, cooling loops, water, alarms, faults)
                                            |
               Modbus TCP  |  SNMPv2c  |  Redfish  |  Prometheus  |  admin/fault API
```

- **The plant model** is in SI units and is independent of country and vendor.
  It covers the utility → ATS/generator → UPS → PDU path, and the IT load
  → CDU/CRAH → chiller → ambient heat-rejection chain.
- **A region profile** sets the country conventions: voltage and frequency,
  climate, display units, water use, grid carbon, applicable standards and
  reporting rules.
- **A protocol profile** maps model points to one vendor's wire format: Modbus
  registers (address, type, scale, units, writability), SNMP OIDs/sensor rows,
  or Redfish resources.

To adapt the simulator you change one of these three things. No code changes
are needed.

## Adapting to different hardware

1. **Get the vendor's map.** Use the Modbus register map or MIB from the
   product documentation, or a device's EDS/XML export if it has one.
2. **Write a profile** in the same format as
   `environments/dc-ot-sim/otsim/profiles/modbus/generic-cdu.yaml`:

   ```yaml
   name: acme-cdu-x200
   description: ACME X200 CDU, firmware 3.x register map
   regionUnits: false          # true if the device follows site units (°F/GPM/psi in the US)
   holding:
     - {addr: 40010, point: supply_setpoint_c, type: int16, scale: 10, quantity: temperature, writable: true}
   input:
     - {addr: 30001, point: supply_temp_c, type: int16, scale: 10, quantity: temperature}
     - {addr: 30003, point: flow_lpm, type: float32, quantity: flow}
   coils:
     - {addr: 0, point: pump1_enable, writable: true}
   ```

   The supported types are int16, uint16, int32, uint32, float32 and string
   (identity). `quantity` turns on unit conversion. Set `const:` for fixed
   registers such as model IDs. SNMP profiles list scalar `objects` (OID, point,
   type, scale) and ENTITY-SENSOR `sensors` rows.
3. **Load it** without rebuilding: add it to `extraProfiles` in
   `environments/dc-ot-sim/values.yaml` (key `modbus/acme-cdu-x200.yaml`), and
   point the device at it: `protocols: {modbus: acme-cdu-x200}`.
4. **Check it**: `otsim validate --config site.yaml --profiles-dir ./profiles`
   rejects unknown points, overlapping registers and bad types. Then
   `otsim telegraf-config` generates a collector config that reads the new
   map back.

To model a different *topology*, edit `siteConfig.devices` instead (2N UPS,
more CDUs, a second generator, more racks). Device `params` set the sizes:
ratedKW, batteryKWh, capacityKW, pumps, breakerAmps.

## Adapting to different countries

Set `region:` in `environments/dc-ot-sim/values.yaml`. The built-in profiles are:

| Region | Supply (L-N / L-L) | Hz | Display units | Grid gCO₂/kWh | Reporting / rules encoded |
|---|---|---|---|---|---|
| `au` | 230/400 V | 50 | SI | 600 | NABERS Energy/Water for data centres; NGER; AS/NZS 3000 |
| `us` | 120/208 V | 60 | °F, gpm, psi | 370 | NEC/NFPA 70, NFPA 75; ENERGY STAR (voluntary) |
| `eu` | 230/400 V | 50 | SI | 250 | EED (EU) 2023/1791 Art. 12 + Delegated Reg. (EU) 2024/1364: sites ≥ 500 kW report PUE, WUE, ERF, REF yearly |
| `de` | 230/400 V | 50 | SI | 350 | EnEfG: PUE ≤ 1.2 for new sites from July 2026, existing ≤ 1.5 (2027) / ≤ 1.3 (2030); energy reuse; 100% renewable from 2027 |
| `nordics` | 230/400 V | 50 | SI | 40 | EED; district heat reuse; cold climate (free cooling) |
| `uk` | 230/400 V | 50 | SI | 150 | BS 7671; Climate Change Agreement (PUE); SECR |
| `sg` | 230/400 V | 50 | SI | 410 | SS 564 (green DC), Green Mark; hot humid climate |
| `in` | 230/415 V | 50 | SI | 700 | BEE reporting |
| `ae` | 230/400 V | 50 | SI | 420 | Very hot climate, high water use when evaporative |
| `jp-east` / `jp-west` | 115/200 V (100 V outlets) | 50 / 60 | SI | 450 | Energy Conservation Act; Japan's 50/60 Hz split |

Here's what a region changes:

- **Electrical:** nominal voltage and frequency, which UPS input tolerances and
  `frequency-deviation` are judged against. Breaker amps follow from kW at the
  local voltage, so a 17 kW rack draws more current at 208 V than at 400 V.
- **Climate:** design ambient and diurnal swing, which drive chiller COP,
  free cooling, PUE and (for evaporative cooling) water use and WUE.
- **Units:** only on maps with `regionUnits: true`. Redfish and SNMP stay SI,
  as their standards require.
- **Carbon and reporting:** grid intensity for carbon KPIs, plus the list of
  rules a report should cite.

To add a country, copy a profile to `regions/<name>.yaml` (or put it in
`extraProfiles`), adjust it, and set `region: <name>`. `otsim serve --region`
and `--profiles-dir` do the same outside Kubernetes.

> **Note:** The regulatory summaries are a starting point for test scenarios,
> not compliance advice. Check the current text of each rule (the EU and German
> thresholds above changed in 2023–2024) before using them for reporting.

## References

- DMTF Redfish: [specification DSP0266](https://www.dmtf.org/dsp/DSP0266), [schema DSP0268](https://www.dmtf.org/dsp/DSP0268), [resource and schema guide DSP2046](https://www.dmtf.org/dsp/DSP2046)
- [RFC 1628 UPS-MIB](https://www.rfc-editor.org/rfc/rfc1628), [RFC 3433 ENTITY-SENSOR-MIB](https://www.rfc-editor.org/rfc/rfc3433), [RFC 6933 ENTITY-MIB v4](https://www.rfc-editor.org/rfc/rfc6933)
- [Modbus application protocol](https://modbus.org/specs.php), [SunSpec information models](https://sunspec.org/specifications/)
- [ASHRAE TC 9.9 thermal guidelines](https://www.ashrae.org/technical-resources/bookstore/datacom-series)
- [EU Delegated Regulation 2024/1364 (data centre reporting)](https://eur-lex.europa.eu/eli/reg_del/2024/1364/oj), [Germany EnEfG](https://www.gesetze-im-internet.de/enefg/)
- [NABERS for data centres](https://www.nabers.gov.au/ratings/spaces-we-rate/data-centres)
