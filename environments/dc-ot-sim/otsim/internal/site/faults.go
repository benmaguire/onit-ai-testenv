package site

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// Fault is an injected OT fault.
type Fault struct {
	ID      string             `json:"id"`
	Type    string             `json:"type"`
	Device  string             `json:"device,omitempty"`
	Params  map[string]float64 `json:"params,omitempty"`
	Point   string             `json:"point,omitempty"`    // sensor-stuck / sensor-drift
	Proto   string             `json:"protocol,omitempty"` // comms-loss: modbus | snmp | redfish | all
	Started time.Time          `json:"started"`
	Expires *time.Time         `json:"expires,omitempty"`
}

// FaultType documents a fault for the catalog.
type FaultType struct {
	Type        string            `json:"type"`
	Description string            `json:"description"`
	DeviceTypes []string          `json:"deviceTypes,omitempty"` // empty = site-wide
	Params      map[string]string `json:"params,omitempty"`
}

// FaultTypes is the fault catalog.
var FaultTypes = []FaultType{
	{Type: "utility-outage", Description: "Mains supply lost: UPS goes on battery, generator starts, ATS transfers; restores and retransfers on recovery."},
	{Type: "utility-sag", Description: "Mains voltage sags below tolerance (UPS rides through on battery).", Params: map[string]string{"voltagePct": "supply voltage as % of nominal (default 80)"}},
	{Type: "frequency-deviation", Description: "Mains frequency deviates (out of tolerance beyond ±1 Hz).", Params: map[string]string{"deltaHz": "offset from nominal (e.g. -1.5)"}},
	{Type: "generator-fail-to-start", Description: "Generator does not start on demand: on a utility outage the UPS runs until its battery is depleted.", DeviceTypes: []string{"generator"}},
	{Type: "ups-battery-degraded", Description: "Battery capacity reduced (ageing): shorter runtime on battery.", DeviceTypes: []string{"ups"}, Params: map[string]string{"capacityPct": "remaining capacity (default 30)"}},
	{Type: "ups-failure", Description: "UPS output fails: the racks it feeds lose power.", DeviceTypes: []string{"ups"}},
	{Type: "pdu-breaker-trip", Description: "Rack PDU breaker opens: the rack loses power.", DeviceTypes: []string{"rack-pdu"}},
	{Type: "pdu-overload", Description: "Rack load rises; past the breaker rating the breaker trips.", DeviceTypes: []string{"rack-pdu"}, Params: map[string]string{"multiplier": "load multiplier (default 1.6)"}},
	{Type: "cdu-pump-failure", Description: "CDU pump fails: N+1 keeps flow with redundancy lost; pump=0 fails all pumps (no flow, return temperature climbs).", DeviceTypes: []string{"cdu"}, Params: map[string]string{"pump": "pump number, 0 = all (default 1)"}},
	{Type: "cdu-leak", Description: "Coolant leak at a CDU: leak sensors alarm, reservoir drains, low level stops the pumps.", DeviceTypes: []string{"cdu"}, Params: map[string]string{"litersPerMinute": "leak rate (default 5)"}},
	{Type: "leak", Description: "Water detected by a leak sensor (no plant effect).", DeviceTypes: []string{"leak-sensor"}},
	{Type: "crah-failure", Description: "CRAH stops: less air cooling capacity, rack inlet temperatures rise.", DeviceTypes: []string{"crah"}},
	{Type: "chiller-trip", Description: "Chiller trips: chilled/facility water warms, CDU and CRAH supply temperatures follow.", DeviceTypes: []string{"chiller"}},
	{Type: "high-ambient", Description: "Heatwave: outdoor temperature rises, chiller COP falls.", Params: map[string]string{"deltaC": "added to the region's ambient (default 10)"}},
	{Type: "it-load-surge", Description: "IT load surge (e.g. a training job ramps up).", Params: map[string]string{"multiplier": "IT load multiplier (default 1.3)"}},
	{Type: "sensor-stuck", Description: "A point reports a frozen value over every protocol (the plant is unaffected).", Params: map[string]string{"value": "value reported (default: current value)"}},
	{Type: "sensor-drift", Description: "A point drifts away from the truth at a rate per minute.", Params: map[string]string{"ratePerMinute": "drift per simulated minute (default 0.5)"}},
	{Type: "comms-loss", Description: "A device stops answering on a protocol (modbus, snmp, redfish or all): DCIM/BMS loses it while the plant keeps running."},
}

// FaultRequest is how faults are injected.
type FaultRequest struct {
	Type            string             `json:"type"`
	Device          string             `json:"device,omitempty"`
	Params          map[string]float64 `json:"params,omitempty"`
	Point           string             `json:"point,omitempty"`
	Protocol        string             `json:"protocol,omitempty"`
	DurationSeconds float64            `json:"durationSeconds,omitempty"`
}

// AddFault validates and injects a fault.
func (s *Site) AddFault(req FaultRequest) (*Fault, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var ft *FaultType
	for i := range FaultTypes {
		if FaultTypes[i].Type == req.Type {
			ft = &FaultTypes[i]
		}
	}
	if ft == nil {
		var names []string
		for _, f := range FaultTypes {
			names = append(names, f.Type)
		}
		return nil, fmt.Errorf("unknown fault type %q (have %s)", req.Type, strings.Join(names, ", "))
	}
	for k := range req.Params {
		if _, ok := ft.Params[k]; !ok {
			return nil, fmt.Errorf("fault %s: unknown param %q", req.Type, k)
		}
	}
	needsDevice := len(ft.DeviceTypes) > 0 || req.Type == "sensor-stuck" || req.Type == "sensor-drift" || req.Type == "comms-loss"
	if needsDevice {
		d, ok := s.byID[req.Device]
		if !ok {
			return nil, fmt.Errorf("fault %s needs a device (have %s)", req.Type, strings.Join(s.idsOfTypes(ft.DeviceTypes), ", "))
		}
		if len(ft.DeviceTypes) > 0 && !contains(ft.DeviceTypes, d.Config.Type) {
			return nil, fmt.Errorf("fault %s applies to %s devices, %s is a %s", req.Type, strings.Join(ft.DeviceTypes, "/"), req.Device, d.Config.Type)
		}
		if req.Type == "sensor-stuck" || req.Type == "sensor-drift" {
			if _, ok := d.Points[req.Point]; !ok {
				return nil, fmt.Errorf("device %s has no point %q", req.Device, req.Point)
			}
		}
	} else if req.Device != "" {
		return nil, fmt.Errorf("fault %s is site-wide; omit device", req.Type)
	}
	if req.Type == "comms-loss" {
		if req.Protocol == "" {
			req.Protocol = "all"
		}
		if !contains([]string{"all", "modbus", "snmp", "redfish"}, req.Protocol) {
			return nil, fmt.Errorf("protocol must be all, modbus, snmp or redfish")
		}
	}
	if req.Params == nil {
		req.Params = map[string]float64{}
	}
	if req.Type == "sensor-stuck" {
		if _, ok := req.Params["value"]; !ok {
			req.Params["value"] = s.byID[req.Device].Points[req.Point]
		}
	}
	s.nextID++
	f := &Fault{ID: fmt.Sprintf("f%d", s.nextID), Type: req.Type, Device: req.Device, Params: req.Params,
		Point: req.Point, Proto: req.Protocol, Started: time.Now()}
	if req.DurationSeconds > 0 {
		e := f.Started.Add(time.Duration(req.DurationSeconds * float64(time.Second)))
		f.Expires = &e
	}
	s.faults[f.ID] = f
	return f, nil
}

// RemoveFault clears a fault.
func (s *Site) RemoveFault(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.faults[id]
	delete(s.faults, id)
	return ok
}

// ClearFaults removes every fault.
func (s *Site) ClearFaults() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := len(s.faults)
	s.faults = map[string]*Fault{}
	return n
}

// CommsDown reports whether a device is unreachable on a protocol.
func (s *Site) CommsDown(device, protocol string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, f := range s.faults {
		if f.Type == "comms-loss" && f.Device == device && (f.Proto == "all" || f.Proto == protocol) {
			return true
		}
	}
	return false
}

// expire drops faults past their duration (called from Tick).
func (s *Site) expire(now time.Time) {
	for id, f := range s.faults {
		if f.Expires != nil && now.After(*f.Expires) {
			delete(s.faults, id)
		}
	}
}

func (s *Site) faultOfType(t, device string) *Fault {
	for _, f := range s.faults {
		if f.Type == t && (device == "" || f.Device == device) {
			return f
		}
	}
	return nil
}

func (s *Site) faultParam(t, device, key string) float64 {
	f := s.faultOfType(t, device)
	if f == nil {
		return 0
	}
	defaults := map[string]float64{"high-ambient/deltaC": 10}
	return paramOr(f.Params, key, defaults[t+"/"+key])
}

func (s *Site) stuckValue(device, point string) (float64, bool) {
	for _, f := range s.faults {
		if f.Device != device || f.Point != point {
			continue
		}
		switch f.Type {
		case "sensor-stuck":
			return f.Params["value"], true
		case "sensor-drift":
			d := s.byID[device]
			mins := time.Since(f.Started).Minutes() * s.cfg.TimeScale
			return round(d.Points[point]+paramOr(f.Params, "ratePerMinute", 0.5)*mins, 3), true
		}
	}
	return 0, false
}

func paramOr(m map[string]float64, k string, def float64) float64 {
	if v, ok := m[k]; ok {
		return v
	}
	return def
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

func (s *Site) idsOfTypes(types []string) []string {
	var out []string
	for _, d := range s.devices {
		if len(types) == 0 || contains(types, d.Config.Type) {
			out = append(out, d.Config.ID)
		}
	}
	sort.Strings(out)
	return out
}
