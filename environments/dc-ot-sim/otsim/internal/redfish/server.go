// Package redfish serves the site through the DMTF Redfish facility model
// (Redfish 2023.3+ DCIM schemas): PowerEquipment/RackPDUs and TransferSwitches
// (PowerDistribution, Circuit, PowerDistributionMetrics), ThermalEquipment/CDUs
// (CoolingUnit, CoolantConnector, LeakDetection/LeakDetector) and Facilities.
// Schema versions follow Redfish 2025.4 (DSP0268); check a strict client with
// the DMTF Redfish Service Validator. Read-only except the CDU supply
// temperature setpoint (PATCH CoolantConnector SupplyTemperatureControlCelsius).
package redfish

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/benmaguire/onit-ai-testenv/environments/dc-ot-sim/otsim/internal/profile"
	"github.com/benmaguire/onit-ai-testenv/environments/dc-ot-sim/otsim/internal/site"
)

// Requests counts protocol requests (device, protocol) for metrics.
var Requests = func(device, protocol string) {}

const root = "/redfish/v1"

type obj = map[string]any

// Handler serves the Redfish tree.
type Handler struct{ Site *site.Site }

func ref(path string) obj { return obj{"@odata.id": path} }

func reading(v float64) obj { return obj{"Reading": v} }

func (h *Handler) devices(kind string) []site.DeviceConfig {
	var out []site.DeviceConfig
	for _, d := range h.Site.Devices() {
		if d.Protocols["redfish"] != "" && d.Type == kind {
			out = append(out, d)
		}
	}
	return out
}

func collection(path, typ, name string, ids []string) obj {
	members := []obj{}
	for _, id := range ids {
		members = append(members, ref(path+"/"+id))
	}
	return obj{"@odata.id": path, "@odata.type": "#" + typ + "Collection." + typ + "Collection",
		"Name": name, "Members": members, "Members@odata.count": len(members)}
}

func ids(ds []site.DeviceConfig) []string {
	out := make([]string, len(ds))
	for i, d := range ds {
		out[i] = d.ID
	}
	return out
}

func (h *Handler) status(snap site.Snapshot, id string) obj {
	health := "OK"
	for _, a := range snap.Alarms {
		if a.Device == id {
			if a.Severity == "critical" {
				health = "Critical"
			} else if health == "OK" {
				health = "Warning"
			}
		}
	}
	return obj{"State": "Enabled", "Health": health}
}

func identity(d site.DeviceConfig) obj {
	return obj{"Manufacturer": profile.Identity(d, "vendor"), "Model": profile.Identity(d, "model"),
		"SerialNumber": profile.Identity(d, "serial"), "UserLabel": d.Name}
}

func merge(a, b obj) obj {
	for k, v := range b {
		a[k] = v
	}
	return a
}

// ServeHTTP implements the resource tree.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p := strings.TrimSuffix(r.URL.Path, "/")
	if p == "/redfish" {
		writeJSON(w, obj{"v1": root + "/"})
		return
	}
	if !strings.HasPrefix(p, root) {
		http.NotFound(w, r)
		return
	}
	parts := strings.Split(strings.TrimPrefix(strings.TrimPrefix(p, root), "/"), "/")
	if len(parts) == 1 && parts[0] == "" {
		parts = nil
	}
	// Device-scoped requests honour comms-loss.
	if len(parts) >= 3 {
		dev := parts[2]
		Requests(dev, "redfish")
		if h.Site.CommsDown(dev, "redfish") {
			http.Error(w, `{"error":{"code":"Base.1.0.ServiceUnavailable","message":"device not responding"}}`, http.StatusServiceUnavailable)
			return
		}
	}
	if r.Method == http.MethodPatch {
		h.patch(w, r, parts)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD, PATCH")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	snap := h.Site.Snapshot()
	res, ok := h.resource(snap, parts)
	if !ok {
		http.NotFound(w, r)
		return
	}
	writeJSON(w, res)
}

func (h *Handler) resource(snap site.Snapshot, parts []string) (obj, bool) {
	cfg := h.Site.Config()
	pdus, atss, cdus := h.devices("rack-pdu"), h.devices("ats"), h.devices("cdu")
	switch {
	case len(parts) == 0:
		return obj{"@odata.id": root, "@odata.type": "#ServiceRoot.v1_17_0.ServiceRoot", "Id": "RootService",
			"Name": "otsim Redfish service (" + cfg.Site.Name + ")", "RedfishVersion": "1.21.0",
			"PowerEquipment": ref(root + "/PowerEquipment"), "ThermalEquipment": ref(root + "/ThermalEquipment"),
			"Facilities": ref(root + "/Facilities")}, true
	case len(parts) == 1 && parts[0] == "PowerEquipment":
		return obj{"@odata.id": root + "/PowerEquipment", "@odata.type": "#PowerEquipment.v1_2_3.PowerEquipment",
			"Id": "PowerEquipment", "Name": "Power equipment", "Status": obj{"State": "Enabled", "Health": "OK"},
			"RackPDUs": ref(root + "/PowerEquipment/RackPDUs"), "TransferSwitches": ref(root + "/PowerEquipment/TransferSwitches")}, true
	case len(parts) == 2 && parts[0] == "PowerEquipment" && parts[1] == "RackPDUs":
		return collection(root+"/PowerEquipment/RackPDUs", "PowerDistribution", "Rack PDUs", ids(pdus)), true
	case len(parts) == 2 && parts[0] == "PowerEquipment" && parts[1] == "TransferSwitches":
		return collection(root+"/PowerEquipment/TransferSwitches", "PowerDistribution", "Transfer switches", ids(atss)), true
	case len(parts) == 1 && parts[0] == "ThermalEquipment":
		return obj{"@odata.id": root + "/ThermalEquipment", "@odata.type": "#ThermalEquipment.v1_1_2.ThermalEquipment",
			"Id": "ThermalEquipment", "Name": "Thermal equipment", "Status": obj{"State": "Enabled", "Health": "OK"},
			"CDUs": ref(root + "/ThermalEquipment/CDUs")}, true
	case len(parts) == 2 && parts[0] == "ThermalEquipment" && parts[1] == "CDUs":
		return collection(root+"/ThermalEquipment/CDUs", "CoolingUnit", "Coolant distribution units", ids(cdus)), true
	case len(parts) == 1 && parts[0] == "Facilities":
		return collection(root+"/Facilities", "Facility", "Facilities", []string{cfg.Site.Name}), true
	case len(parts) == 2 && parts[0] == "Facilities" && parts[1] == cfg.Site.Name:
		health := "OK"
		if snap.KPI["alarms_critical"] > 0 {
			health = "Critical"
		} else if snap.KPI["alarms_warning"] > 0 {
			health = "Warning"
		}
		return obj{"@odata.id": root + "/Facilities/" + cfg.Site.Name, "@odata.type": "#Facility.v1_4_2.Facility",
			"Id": cfg.Site.Name, "Name": cfg.Site.Name, "FacilityType": "Room",
			"Status":       obj{"State": "Enabled", "Health": health},
			"PowerDomains": ref(root + "/PowerEquipment"),
			"Oem": obj{"otsim": obj{"Region": snap.Region, "PUE": snap.KPI["pue"], "ITPowerkW": snap.KPI["it_kw"],
				"FacilityPowerkW": snap.KPI["facility_kw"], "WUELitersPerkWh": snap.KPI["wue_l_per_kwh"]}}}, true
	}
	if len(parts) >= 3 && parts[0] == "PowerEquipment" && (parts[1] == "RackPDUs" || parts[1] == "TransferSwitches") {
		return h.powerDistribution(snap, parts)
	}
	if len(parts) >= 3 && parts[0] == "ThermalEquipment" && parts[1] == "CDUs" {
		return h.coolingUnit(snap, parts)
	}
	return nil, false
}

func find(ds []site.DeviceConfig, id string) (site.DeviceConfig, bool) {
	for _, d := range ds {
		if d.ID == id {
			return d, true
		}
	}
	return site.DeviceConfig{}, false
}

func (h *Handler) powerDistribution(snap site.Snapshot, parts []string) (obj, bool) {
	kind := map[string]string{"RackPDUs": "rack-pdu", "TransferSwitches": "ats"}[parts[1]]
	d, ok := find(h.devices(kind), parts[2])
	if !ok {
		return nil, false
	}
	base := root + "/PowerEquipment/" + parts[1] + "/" + d.ID
	pt := snap.Devices[d.ID]
	switch {
	case len(parts) == 3:
		eq := map[string]string{"rack-pdu": "RackPDU", "ats": "TransferSwitch"}[kind]
		o := merge(obj{"@odata.id": base, "@odata.type": "#PowerDistribution.v1_6_0.PowerDistribution", "Id": d.ID,
			"Name": d.Name, "EquipmentType": eq, "Status": h.status(snap, d.ID)}, identity(d))
		if kind == "rack-pdu" {
			o["Metrics"] = ref(base + "/Metrics")
			o["Mains"] = ref(base + "/Mains")
			o["Location"] = obj{"Placement": obj{"Rack": fmt.Sprintf("%d", d.Rack)}}
		} else {
			o["TransferConfiguration"] = obj{"ActiveMainsId": map[float64]string{0: "", 1: "Utility", 2: "Generator"}[pt["source"]]}
		}
		return o, true
	case len(parts) == 4 && parts[3] == "Metrics" && kind == "rack-pdu":
		return obj{"@odata.id": base + "/Metrics", "@odata.type": "#PowerDistributionMetrics.v1_3_2.PowerDistributionMetrics",
			"Id": "Metrics", "Name": d.Name + " metrics", "PowerWatts": reading(pt["power_w"]), "EnergykWh": reading(pt["energy_kwh"])}, true
	case len(parts) == 4 && parts[3] == "Mains" && kind == "rack-pdu":
		return collection(base+"/Mains", "Circuit", "Mains", []string{"AC1"}), true
	case len(parts) == 5 && parts[3] == "Mains" && parts[4] == "AC1" && kind == "rack-pdu":
		breaker := "Normal"
		if pt["breaker_closed"] == 0 {
			breaker = "Tripped"
		}
		return obj{"@odata.id": base + "/Mains/AC1", "@odata.type": "#Circuit.v1_8_0.Circuit", "Id": "AC1", "Name": "Mains input",
			"CircuitType": "Mains", "PhaseWiringType": "ThreePhase5Wire", "BreakerState": breaker,
			"Status":  h.status(snap, d.ID),
			"Voltage": reading(pt["voltage_v"]), "FrequencyHz": reading(pt["frequency_hz"]), "PowerWatts": reading(pt["power_w"]),
			"CurrentAmps":          reading(pt["current_l1_a"]),
			"PolyPhaseCurrentAmps": obj{"Line1": reading(pt["current_l1_a"]), "Line2": reading(pt["current_l2_a"]), "Line3": reading(pt["current_l3_a"])},
			"EnergykWh":            reading(pt["energy_kwh"])}, true
	}
	return nil, false
}

func (h *Handler) leakSensors(cdu string) []site.DeviceConfig {
	var out []site.DeviceConfig
	for _, d := range h.Site.Devices() {
		if d.Type == "leak-sensor" && d.Location == cdu {
			out = append(out, d)
		}
	}
	return out
}

func (h *Handler) coolingUnit(snap site.Snapshot, parts []string) (obj, bool) {
	d, ok := find(h.devices("cdu"), parts[2])
	if !ok {
		return nil, false
	}
	base := root + "/ThermalEquipment/CDUs/" + d.ID
	pt := snap.Devices[d.ID]
	connector := base + "/SecondaryCoolantConnectors/1"
	switch {
	case len(parts) == 3:
		return merge(obj{"@odata.id": base, "@odata.type": "#CoolingUnit.v1_5_0.CoolingUnit", "Id": d.ID, "Name": d.Name,
			"EquipmentType": "CDU", "Status": h.status(snap, d.ID),
			"Coolant":                    obj{"CoolantType": "Water"},
			"SecondaryCoolantConnectors": ref(base + "/SecondaryCoolantConnectors"),
			"LeakDetection":              ref(base + "/LeakDetection"),
			"CoolingCapacityWatts":       d.Params["capacityKW"] * 1000,
			"Oem":                        obj{"otsim": obj{"PumpsRunning": pt["pumps_running"], "ReservoirLevelPercent": pt["reservoir_level_pct"], "PumpPowerWatts": pt["pump_power_kw"] * 1000}}}, identity(d)), true
	case len(parts) == 4 && parts[3] == "SecondaryCoolantConnectors":
		return collection(base+"/SecondaryCoolantConnectors", "CoolantConnector", "Secondary coolant connectors", []string{"1"}), true
	case len(parts) == 5 && parts[3] == "SecondaryCoolantConnectors" && parts[4] == "1":
		return obj{"@odata.id": connector, "@odata.type": "#CoolantConnector.v1_3_0.CoolantConnector", "Id": "1", "Name": "Secondary loop",
			"Status":                          h.status(snap, d.ID),
			"SupplyTemperatureCelsius":        reading(pt["supply_temp_c"]),
			"ReturnTemperatureCelsius":        reading(pt["return_temp_c"]),
			"DeltaTemperatureCelsius":         reading(pt["return_temp_c"] - pt["supply_temp_c"]),
			"FlowLitersPerMinute":             reading(pt["flow_lpm"]),
			"DeltaPressurekPa":                reading(pt["delta_pressure_kpa"]),
			"HeatRemovedkW":                   reading(pt["heat_load_kw"]),
			"SupplyTemperatureControlCelsius": obj{"SetPoint": pt["supply_setpoint_c"], "ControlMode": "Automatic"}}, true
	case len(parts) == 4 && parts[3] == "LeakDetection":
		health := "OK"
		if pt["leak_detected"] == 1 {
			health = "Critical"
		}
		return obj{"@odata.id": base + "/LeakDetection", "@odata.type": "#LeakDetection.v1_2_0.LeakDetection", "Id": "LeakDetection",
			"Name": "Leak detection", "Status": obj{"State": "Enabled", "Health": health},
			"LeakDetectors": ref(base + "/LeakDetection/LeakDetectors")}, true
	case len(parts) == 5 && parts[3] == "LeakDetection" && parts[4] == "LeakDetectors":
		names := []string{"internal"}
		for _, s := range h.leakSensors(d.ID) {
			names = append(names, s.ID)
		}
		return collection(base+"/LeakDetection/LeakDetectors", "LeakDetector", "Leak detectors", names), true
	case len(parts) == 6 && parts[3] == "LeakDetection" && parts[4] == "LeakDetectors":
		state := pt["leak_detected"]
		name := "CDU internal leak detector"
		if parts[5] != "internal" {
			s, ok := find(h.leakSensors(d.ID), parts[5])
			if !ok {
				return nil, false
			}
			state, name = snap.Devices[s.ID]["leak_detected"], s.Name
		}
		ds := "OK"
		if state == 1 {
			ds = "Critical"
		}
		return obj{"@odata.id": base + "/LeakDetection/LeakDetectors/" + parts[5], "@odata.type": "#LeakDetector.v1_3_0.LeakDetector",
			"Id": parts[5], "Name": name, "LeakDetectorType": "Moisture", "DetectorState": ds,
			"Status": obj{"State": "Enabled", "Health": ds}}, true
	}
	return nil, false
}

func (h *Handler) patch(w http.ResponseWriter, r *http.Request, parts []string) {
	if !(len(parts) == 5 && parts[0] == "ThermalEquipment" && parts[1] == "CDUs" && parts[3] == "SecondaryCoolantConnectors" && parts[4] == "1") {
		http.Error(w, `{"error":{"code":"Base.1.0.OperationNotAllowed"}}`, http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		SupplyTemperatureControlCelsius *struct {
			SetPoint *float64 `json:"SetPoint"`
		} `json:"SupplyTemperatureControlCelsius"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body); err != nil || body.SupplyTemperatureControlCelsius == nil || body.SupplyTemperatureControlCelsius.SetPoint == nil {
		http.Error(w, `{"error":{"code":"Base.1.0.PropertyMissing","message":"SupplyTemperatureControlCelsius.SetPoint required"}}`, http.StatusBadRequest)
		return
	}
	if err := h.Site.SetPoint(parts[2], "supply_setpoint_c", *body.SupplyTemperatureControlCelsius.SetPoint); err != nil {
		http.Error(w, fmt.Sprintf(`{"error":{"code":"Base.1.0.PropertyValueNotInList","message":%q}}`, err.Error()), http.StatusBadRequest)
		return
	}
	res, _ := h.resource(h.Site.Snapshot(), parts)
	writeJSON(w, res)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("OData-Version", "4.0")
	_ = json.NewEncoder(w).Encode(v)
}
