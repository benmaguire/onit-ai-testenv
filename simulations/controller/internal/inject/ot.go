package inject

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	simv1 "github.com/benmaguire/onit-ai-testenv/simulations/controller/api/v1alpha1"
	"github.com/benmaguire/onit-ai-testenv/simulations/controller/pkg/catalog"
)

// OT faults are injected into the data centre OT simulator (otsim, the
// dc-ot-sim environment) through its admin API. The plant model then plays
// out the consequences (UPS on battery, temperatures rising, alarms) on every
// protocol it serves (Modbus TCP, SNMP, Redfish) and in its metrics.

const otDefaultNamespace = "dc-ot-sim"

// otFault mirrors one otsim fault type.
type otFault struct {
	name        string // otsim type; simulation type is "ot-" + name
	summary     string
	effect      string
	deviceTypes []string // empty = site-wide
	needsDevice bool
	params      []catalog.Param // numeric otsim params
	point       bool            // takes a point (sensor faults)
	protocol    bool            // takes a protocol (comms-loss)
	recovery    string
}

var otFaults = []otFault{
	{name: "utility-outage", summary: "Mains supply lost", effect: "The UPS goes on battery, the generator starts after its start delay and the ATS transfers to it; IT load stays up while the chain works.", recovery: "Mains restored; the ATS retransfers after its delay and the generator cools down and stops."},
	{name: "utility-sag", summary: "Mains voltage sag", effect: "Supply voltage drops below tolerance; the UPS rides through on battery.", params: []catalog.Param{{Name: "voltagePct", Description: "supply voltage as % of nominal", Default: "80"}}},
	{name: "frequency-deviation", summary: "Mains frequency deviation", effect: "Mains frequency moves off the region's nominal (50/60 Hz); beyond ±1 Hz the UPS treats the input as bad.", params: []catalog.Param{{Name: "deltaHz", Description: "offset from nominal in Hz", Default: "-1.5"}}},
	{name: "generator-fail-to-start", summary: "Generator fails to start", effect: "On a utility outage the generator alarms fail-to-start and the UPS runs until its battery is depleted. Combine with ot-utility-outage.", deviceTypes: []string{"generator"}},
	{name: "ups-battery-degraded", summary: "UPS battery capacity reduced", effect: "Shorter runtime on battery (ageing strings).", deviceTypes: []string{"ups"}, params: []catalog.Param{{Name: "capacityPct", Description: "remaining capacity %", Default: "30"}}},
	{name: "ups-failure", summary: "UPS output fails", effect: "The racks the UPS feeds lose power; IT load drops.", deviceTypes: []string{"ups"}},
	{name: "pdu-breaker-trip", summary: "Rack PDU breaker trips", effect: "The rack loses power; other racks are unaffected.", deviceTypes: []string{"rack-pdu"}},
	{name: "pdu-overload", summary: "Rack overload", effect: "Rack load rises; past the breaker rating the breaker trips.", deviceTypes: []string{"rack-pdu"}, params: []catalog.Param{{Name: "multiplier", Description: "load multiplier", Default: "1.6"}}},
	{name: "cdu-pump-failure", summary: "CDU pump failure", effect: "One pump: N+1 keeps flow, redundancy-lost alarm. pump=0: all pumps stop, no flow, return temperature climbs and alarms.", deviceTypes: []string{"cdu"}, params: []catalog.Param{{Name: "pump", Description: "pump number, 0 = all", Default: "1"}}},
	{name: "cdu-leak", summary: "Coolant leak at a CDU", effect: "Leak sensors alarm, the reservoir drains and a low level stops the pumps.", deviceTypes: []string{"cdu"}, params: []catalog.Param{{Name: "litersPerMinute", Description: "leak rate", Default: "5"}}},
	{name: "leak", summary: "Water detected by a leak sensor", effect: "Leak alarm only (no plant effect): tests BMS alarm handling.", deviceTypes: []string{"leak-sensor"}},
	{name: "crah-failure", summary: "CRAH unit stops", effect: "Less air cooling capacity; rack inlet temperatures rise (ASHRAE alarms).", deviceTypes: []string{"crah"}},
	{name: "chiller-trip", summary: "Chiller trips", effect: "Chilled/facility water warms; CDU and CRAH supply temperatures follow.", deviceTypes: []string{"chiller"}},
	{name: "high-ambient", summary: "Heatwave", effect: "Outdoor temperature rises and chiller efficiency falls; PUE and water use rise.", params: []catalog.Param{{Name: "deltaC", Description: "added to the region's ambient °C", Default: "10"}}},
	{name: "it-load-surge", summary: "IT load surge", effect: "IT load rises (e.g. a training job ramps); power and cooling follow.", params: []catalog.Param{{Name: "multiplier", Description: "IT load multiplier", Default: "1.3"}}},
	{name: "sensor-stuck", summary: "Sensor stuck", effect: "A point reports a frozen value on every protocol while the plant keeps changing: tests DCIM plausibility checks.", needsDevice: true, point: true, params: []catalog.Param{{Name: "value", Description: "value reported (default: the current value)"}}},
	{name: "sensor-drift", summary: "Sensor drift", effect: "A point drifts from the truth at a rate per simulated minute.", needsDevice: true, point: true, params: []catalog.Param{{Name: "ratePerMinute", Description: "drift per minute", Default: "0.5"}}},
	{name: "comms-loss", summary: "Device stops answering", effect: "The device stops responding on a protocol (timeouts for Modbus/SNMP, 503 for Redfish) while the plant keeps running: DCIM/BMS lose visibility.", needsDevice: true, protocol: true},
}

// otInjector injects one otsim fault type.
type otInjector struct{ f otFault }

// OTInjectors returns the ot-* simulation types (dc-ot-sim).
func OTInjectors() []Injector {
	out := make([]Injector, 0, len(otFaults))
	for _, f := range otFaults {
		out = append(out, otInjector{f: f})
	}
	return out
}

func (o otInjector) Entry() catalog.Entry {
	f := o.f
	var ps []catalog.Param
	if f.needsDevice || len(f.deviceTypes) > 0 {
		d := "otsim device ID (GET /api/v1/devices)"
		if len(f.deviceTypes) > 0 {
			d = fmt.Sprintf("otsim device ID of a %s device", strings.Join(f.deviceTypes, "/"))
		}
		ps = append(ps, catalog.Param{Name: "device", Description: d, Required: true})
	}
	if f.point {
		ps = append(ps, catalog.Param{Name: "point", Description: "device point, e.g. supply_temp_c", Required: true})
	}
	if f.protocol {
		ps = append(ps, catalog.Param{Name: "protocol", Description: "modbus, snmp, redfish or all", Default: "all"})
	}
	ps = append(ps, f.params...)
	ps = append(ps, catalog.Param{Name: "otsimURL", Description: "otsim admin API (default http://otsim.<target.namespace>.svc:8080)"})
	rec := f.recovery
	if rec == "" {
		rec = "The fault is removed from otsim; the plant model returns to normal."
	}
	return catalog.Entry{
		Type: "ot-" + f.name, Category: "ot", Summary: f.summary + " (data centre OT)",
		Effect:   f.effect,
		Recovery: rec + " Recovered when the site has no more critical alarms than before injection.",
		Targets:  []string{"namespace"},
		Params:   ps,
		Requires: []string{"dc-ot-sim environment (otsim)"},
		References: []string{
			"https://www.dmtf.org/standards/redfish",
			"https://modbus.org/specs.php",
		},
	}
}

func (o otInjector) Validate(sim *simv1.Simulation) error {
	e := o.Entry()
	if err := checkKnownParams(e, sim); err != nil {
		return err
	}
	p := newParams(e, sim)
	if o.f.protocol {
		if err := oneOf("protocol", p.str("protocol"), "all", "modbus", "snmp", "redfish"); err != nil {
			return err
		}
	}
	_, err := o.request(p)
	return err
}

type otRequest struct {
	Type     string             `json:"type"`
	Device   string             `json:"device,omitempty"`
	Params   map[string]float64 `json:"params,omitempty"`
	Point    string             `json:"point,omitempty"`
	Protocol string             `json:"protocol,omitempty"`
}

func (o otInjector) request(p params) (otRequest, error) {
	r := otRequest{Type: o.f.name, Device: p.str("device"), Point: p.str("point")}
	if o.f.protocol {
		r.Protocol = p.str("protocol")
	}
	for _, d := range o.f.params {
		s := p.str(d.Name)
		if s == "" {
			continue
		}
		v, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return r, fmt.Errorf("param %q: %q is not a number", d.Name, s)
		}
		if r.Params == nil {
			r.Params = map[string]float64{}
		}
		r.Params[d.Name] = v
	}
	return r, nil
}

func otURL(env *Env, sim *simv1.Simulation, p params) string {
	if u := p.str("otsimURL"); u != "" {
		return strings.TrimRight(u, "/")
	}
	if env.OTSimURL != "" {
		return strings.TrimRight(env.OTSimURL, "/")
	}
	ns := sim.Spec.Target.Namespace
	if ns == "" || ns == sim.Namespace {
		// Simulations usually live in sim-system; the plant lives in dc-ot-sim.
		ns = otDefaultNamespace
	}
	return fmt.Sprintf("http://otsim.%s.svc:8080", ns)
}

func httpClient(env *Env) *http.Client {
	if env.HTTPClient != nil {
		return env.HTTPClient
	}
	return &http.Client{Timeout: 10 * time.Second}
}

func otCall(ctx context.Context, env *Env, method, url string, body any, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rd)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := httpClient(env).Do(req)
	if err != nil {
		return fmt.Errorf("otsim %s %s: %w", method, url, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 300 {
		return fmt.Errorf("otsim %s %s: %s: %s", method, url, resp.Status, strings.TrimSpace(string(b)))
	}
	if out != nil {
		return json.Unmarshal(b, out)
	}
	return nil
}

type otStatus struct {
	Healthy bool               `json:"healthy"`
	KPI     map[string]float64 `json:"kpi"`
	Faults  []struct {
		ID string `json:"id"`
	} `json:"faults"`
	Alarms []struct {
		Device   string `json:"device"`
		Name     string `json:"name"`
		Severity string `json:"severity"`
	} `json:"alarms"`
}

func (o otInjector) Inject(ctx context.Context, env *Env, sim *simv1.Simulation) (Result, error) {
	p := newParams(o.Entry(), sim)
	base := otURL(env, sim, p)
	req, err := o.request(p)
	if err != nil {
		return Result{}, err
	}
	var before otStatus
	if err := otCall(ctx, env, http.MethodGet, base+"/api/v1/status", nil, &before); err != nil {
		return Result{}, err
	}
	var f struct {
		ID string `json:"id"`
	}
	if err := otCall(ctx, env, http.MethodPost, base+"/api/v1/faults", req, &f); err != nil {
		return Result{}, err
	}
	target := req.Device
	if target == "" {
		target = "site"
	}
	return Result{
		Targets: []simv1.AffectedTarget{{Kind: "OTFault", Name: f.ID, Detail: fmt.Sprintf("%s on %s", req.Type, target)}},
		Data: map[string]string{
			"otsimURL":              base,
			"otFaultID":             f.ID,
			"baselineCriticalAlarm": strconv.Itoa(int(before.KPI["alarms_critical"])),
		},
		Message: fmt.Sprintf("otsim fault %s (%s) injected on %s", f.ID, req.Type, target),
	}, nil
}

func (o otInjector) Recover(ctx context.Context, env *Env, sim *simv1.Simulation) (string, error) {
	d := sim.Status.Data
	base, id := d["otsimURL"], d["otFaultID"]
	if base == "" || id == "" {
		return "nothing was injected", nil
	}
	err := otCall(ctx, env, http.MethodDelete, base+"/api/v1/faults/"+id, nil, nil)
	if err != nil && strings.Contains(err.Error(), "404") {
		return fmt.Sprintf("otsim fault %s already gone (expired or cleared)", id), nil
	}
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("otsim fault %s removed", id), nil
}

func (o otInjector) Verify(ctx context.Context, env *Env, sim *simv1.Simulation) (bool, string, error) {
	d := sim.Status.Data
	base := d["otsimURL"]
	if base == "" {
		return true, "nothing was injected", nil
	}
	var st otStatus
	if err := otCall(ctx, env, http.MethodGet, base+"/api/v1/status", nil, &st); err != nil {
		return false, "", err
	}
	for _, f := range st.Faults {
		if f.ID == d["otFaultID"] {
			return false, "fault still active in otsim", nil
		}
	}
	baseline, _ := strconv.Atoi(d["baselineCriticalAlarm"])
	crit := int(st.KPI["alarms_critical"])
	if crit > baseline {
		var names []string
		for _, a := range st.Alarms {
			if a.Severity == "critical" {
				names = append(names, a.Device+"/"+a.Name)
			}
		}
		sort.Strings(names)
		return false, fmt.Sprintf("%d critical alarm(s) still active (baseline %d): %s", crit, baseline, strings.Join(names, ", ")), nil
	}
	return true, fmt.Sprintf("site back to baseline (%d critical alarms)", crit), nil
}
