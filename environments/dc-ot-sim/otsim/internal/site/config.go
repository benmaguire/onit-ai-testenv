// Package site models a data centre's OT plant (power path, cooling, water)
// as a set of devices whose points (measurements and setpoints) are updated
// by a simple coupled physics model on every tick.
package site

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"sigs.k8s.io/yaml"
)

// Config is the site description (site.yaml).
type Config struct {
	Site struct {
		Name   string `json:"name"`
		Region string `json:"region"` // regions/<name>.yaml
	} `json:"site"`
	TickSeconds float64 `json:"tickSeconds"`
	// TimeScale > 1 runs the model faster than real time (e.g. 60 = 1 h per minute).
	TimeScale float64 `json:"timeScale"`

	ITLoad struct {
		BaseKW     float64 `json:"baseKW"`
		DiurnalPct float64 `json:"diurnalPct"` // +/- swing over 24 h
		NoisePct   float64 `json:"noisePct"`
	} `json:"itLoad"`
	// LiquidFraction of IT heat removed by CDUs (direct liquid cooling); the rest goes to air (CRAHs).
	LiquidFraction float64 `json:"liquidFraction"`
	// MiscFraction of IT load added as lighting/controls/losses outside the UPS path.
	MiscFraction float64 `json:"miscFraction"`

	Devices []DeviceConfig `json:"devices"`
}

// DeviceConfig is one OT device.
type DeviceConfig struct {
	ID   string `json:"id"`
	Type string `json:"type"` // utility, ats, generator, ups, rack-pdu, power-meter, cdu, crah, chiller, leak-sensor
	Name string `json:"name,omitempty"`
	// Rack number for rack-pdu (1-based); location text for sensors.
	Rack     int    `json:"rack,omitempty"`
	Location string `json:"location,omitempty"`
	// Ratings / parameters (per type, see defaults()).
	Params map[string]float64 `json:"params,omitempty"`
	// Protocol maps exposed for this device: modbus, snmp, redfish (profile names).
	Protocols map[string]string `json:"protocols,omitempty"`
	// Identity strings (vendor/model/serial) reported over the protocols.
	Vendor string `json:"vendor,omitempty"`
	Model  string `json:"model,omitempty"`
	Serial string `json:"serial,omitempty"`
}

// Region is a country/region electrical, climate and units profile.
type Region struct {
	Name        string  `json:"name"`
	Description string  `json:"description"`
	VoltageLN   float64 `json:"voltageLN"` // nominal phase-to-neutral (V)
	VoltageLL   float64 `json:"voltageLL"` // nominal phase-to-phase (V)
	FrequencyHz float64 `json:"frequencyHz"`
	Units       struct {
		Temperature string `json:"temperature"` // C | F
		Flow        string `json:"flow"`        // lpm | gpm
		Pressure    string `json:"pressure"`    // kpa | psi
	} `json:"units"`
	Climate struct {
		DesignAmbientC float64 `json:"designAmbientC"`
		DiurnalSwingC  float64 `json:"diurnalSwingC"`
	} `json:"climate"`
	Cooling struct {
		// Evaporative heat rejection (cooling towers) consumes water; dry coolers don't.
		Evaporative     bool    `json:"evaporative"`
		WaterLPerKWhRej float64 `json:"waterLPerKWhRejected"`
	} `json:"cooling"`
	GridCarbonGPerKWh float64  `json:"gridCarbonGPerKWh"`
	Standards         []string `json:"standards"`
	Reporting         []string `json:"reporting"`
	Notes             string   `json:"notes,omitempty"`
}

// LoadConfig reads site.yaml.
func LoadConfig(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ParseConfig(b)
}

// ParseConfig parses and validates a site description.
func ParseConfig(b []byte) (*Config, error) {
	var c Config
	if err := yaml.UnmarshalStrict(b, &c); err != nil {
		return nil, fmt.Errorf("site config: %w", err)
	}
	if c.TickSeconds <= 0 {
		c.TickSeconds = 1
	}
	if c.TimeScale <= 0 {
		c.TimeScale = 1
	}
	if c.ITLoad.BaseKW <= 0 {
		c.ITLoad.BaseKW = 400
	}
	if c.LiquidFraction < 0 || c.LiquidFraction > 1 {
		return nil, fmt.Errorf("liquidFraction must be 0..1")
	}
	if c.MiscFraction == 0 {
		c.MiscFraction = 0.03
	}
	seen := map[string]bool{}
	for i := range c.Devices {
		d := &c.Devices[i]
		if d.ID == "" || strings.ContainsAny(d.ID, " /") {
			return nil, fmt.Errorf("device %d: invalid id %q", i, d.ID)
		}
		if seen[d.ID] {
			return nil, fmt.Errorf("duplicate device id %q", d.ID)
		}
		seen[d.ID] = true
		if _, ok := typeDefaults[d.Type]; !ok {
			return nil, fmt.Errorf("device %s: unknown type %q (have %s)", d.ID, d.Type, strings.Join(DeviceTypes(), ", "))
		}
		if d.Params == nil {
			d.Params = map[string]float64{}
		}
		for k, v := range typeDefaults[d.Type] {
			if _, ok := d.Params[k]; !ok {
				d.Params[k] = v
			}
		}
		if d.Name == "" {
			d.Name = d.ID
		}
	}
	return &c, nil
}

// DeviceTypes lists the supported device types.
func DeviceTypes() []string {
	var t []string
	for k := range typeDefaults {
		t = append(t, k)
	}
	sort.Strings(t)
	return t
}

// typeDefaults are the default parameters per device type.
var typeDefaults = map[string]map[string]float64{
	"utility":     {},
	"ats":         {"transferSeconds": 0.5, "retransferSeconds": 30},
	"generator":   {"ratedKW": 1500, "startDelaySeconds": 10, "fuelHours": 24, "cooldownSeconds": 60},
	"ups":         {"ratedKW": 600, "batteryKWh": 100, "efficiency": 0.96, "rechargeKW": 30},
	"rack-pdu":    {"breakerAmps": 63, "phases": 3, "powerFactor": 0.98},
	"power-meter": {},
	"cdu":         {"capacityKW": 400, "pumps": 2, "pumpFlowLPM": 600, "pumpKW": 5, "supplySetpointC": 32, "approachC": 3, "reservoirL": 200},
	"crah":        {"capacityKW": 150, "fanKW": 6, "supplyAirSetpointC": 22},
	"chiller":     {"capacityKW": 900, "copRated": 5.5, "chilledWaterSetpointC": 18},
	"leak-sensor": {},
}
