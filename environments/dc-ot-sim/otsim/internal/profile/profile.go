// Package profile loads region and protocol-map profiles from the embedded
// defaults, overridden or extended by files in an optional directory
// (<dir>/regions/*.yaml, <dir>/modbus/*.yaml, <dir>/snmp/*.yaml).
package profile

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"sigs.k8s.io/yaml"

	"github.com/benmaguire/onit-ai-testenv/environments/dc-ot-sim/otsim/internal/site"
	"github.com/benmaguire/onit-ai-testenv/environments/dc-ot-sim/otsim/profiles"
)

// ModbusEntry is one register, coil or discrete input.
type ModbusEntry struct {
	Addr     uint16  `json:"addr"`
	Point    string  `json:"point,omitempty"`
	Const    *int64  `json:"const,omitempty"`
	Field    string  `json:"field,omitempty"`  // vendor | model | serial | name | location (strings)
	String   string  `json:"string,omitempty"` // literal string
	Type     string  `json:"type,omitempty"`   // uint16 | int16 | uint32 | int32 | float32 | string (registers)
	Len      int     `json:"len,omitempty"`    // string length in registers
	Scale    float64 `json:"scale,omitempty"`  // raw = value * scale
	Quantity string  `json:"quantity,omitempty"`
	Writable bool    `json:"writable,omitempty"`
}

// ModbusMap is a device's Modbus data model.
type ModbusMap struct {
	Name        string        `json:"name"`
	Description string        `json:"description"`
	RegionUnits bool          `json:"regionUnits,omitempty"`
	Holding     []ModbusEntry `json:"holding,omitempty"`
	Input       []ModbusEntry `json:"input,omitempty"`
	Coils       []ModbusEntry `json:"coils,omitempty"`
	Discrete    []ModbusEntry `json:"discrete,omitempty"`
}

// SNMPObject is one scalar object.
type SNMPObject struct {
	OID   string  `json:"oid"`
	Type  string  `json:"type"` // integer | gauge | counter | timeticks | string
	Point string  `json:"point,omitempty"`
	Field string  `json:"field,omitempty"`
	Const *int64  `json:"const,omitempty"`
	Scale float64 `json:"scale,omitempty"`
}

// SNMPSensor expands into ENTITY-MIB / ENTITY-SENSOR-MIB table rows.
type SNMPSensor struct {
	Name         string `json:"name"`
	Point        string `json:"point"`
	Type         int    `json:"type"`      // entPhySensorType
	Precision    int    `json:"precision"` // decimal places
	UnitsDisplay string `json:"unitsDisplay"`
}

// SNMPMap is a device's SNMP data model.
type SNMPMap struct {
	Name        string       `json:"name"`
	Description string       `json:"description"`
	Objects     []SNMPObject `json:"objects,omitempty"`
	Sensors     []SNMPSensor `json:"sensors,omitempty"`
}

// Set holds every loaded profile.
type Set struct {
	Regions map[string]*site.Region
	Modbus  map[string]*ModbusMap
	SNMP    map[string]*SNMPMap
}

// Load reads the embedded profiles and then dir (if non-empty).
func Load(dir string) (*Set, error) {
	s := &Set{Regions: map[string]*site.Region{}, Modbus: map[string]*ModbusMap{}, SNMP: map[string]*SNMPMap{}}
	if err := s.loadFS(profiles.FS); err != nil {
		return nil, err
	}
	if dir != "" {
		if _, err := os.Stat(dir); err == nil {
			if err := s.loadFS(os.DirFS(dir)); err != nil {
				return nil, err
			}
		}
	}
	return s, nil
}

func (s *Set) loadFS(fsys fs.FS) error {
	for _, kind := range []string{"regions", "modbus", "snmp"} {
		files, _ := fs.Glob(fsys, kind+"/*.yaml")
		// Also <kind>__<name>.yaml at the top level (ConfigMap keys cannot contain "/").
		flat, _ := fs.Glob(fsys, kind+"__*.yaml")
		files = append(files, flat...)
		for _, f := range files {
			b, err := fs.ReadFile(fsys, f)
			if err != nil {
				return err
			}
			name := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(f), kind+"__"), ".yaml")
			var target interface{}
			switch kind {
			case "regions":
				r := &site.Region{}
				target, s.Regions[name] = r, r
			case "modbus":
				m := &ModbusMap{}
				target, s.Modbus[name] = m, m
			case "snmp":
				m := &SNMPMap{}
				target, s.SNMP[name] = m, m
			}
			if err := yaml.UnmarshalStrict(b, target); err != nil {
				return fmt.Errorf("%s: %w", f, err)
			}
			if m, ok := target.(*ModbusMap); ok {
				if err := m.Check(); err != nil {
					return fmt.Errorf("%s: %w", f, err)
				}
			}
		}
	}
	return nil
}

// Width is the number of 16-bit registers an entry occupies.
func (e ModbusEntry) Width() (int, error) {
	switch e.Type {
	case "", "uint16", "int16":
		return 1, nil
	case "uint32", "int32", "float32":
		return 2, nil
	case "string":
		if e.Len < 1 {
			return 0, fmt.Errorf("string register at %d needs len >= 1", e.Addr)
		}
		return e.Len, nil
	}
	return 0, fmt.Errorf("register at %d: unknown type %q (uint16, int16, uint32, int32, float32, string)", e.Addr, e.Type)
}

// Check rejects unknown register types, entries with nothing to serve, and
// overlapping registers or duplicate coil/discrete addresses: the usual
// mistakes when copying a vendor's register map.
func (m *ModbusMap) Check() error {
	for _, t := range []struct {
		kind string
		es   []ModbusEntry
	}{{"holding", m.Holding}, {"input", m.Input}} {
		used := map[int]string{}
		for _, e := range t.es {
			w, err := e.Width()
			if err != nil {
				return fmt.Errorf("%s %w", t.kind, err)
			}
			if e.Point == "" && e.Const == nil && e.Field == "" && e.String == "" {
				return fmt.Errorf("%s register %d: needs point, const, field or string", t.kind, e.Addr)
			}
			label := e.Point + e.Field + e.String
			for a := int(e.Addr); a < int(e.Addr)+w; a++ {
				if a > 0xFFFF {
					return fmt.Errorf("%s register %d (%s): runs past 65535", t.kind, e.Addr, label)
				}
				if prev, dup := used[a]; dup {
					return fmt.Errorf("%s register %d (%s) overlaps %s", t.kind, a, label, prev)
				}
				used[a] = fmt.Sprintf("%d (%s)", e.Addr, label)
			}
		}
	}
	for _, t := range []struct {
		kind string
		es   []ModbusEntry
	}{{"coil", m.Coils}, {"discrete input", m.Discrete}} {
		used := map[uint16]string{}
		for _, e := range t.es {
			if e.Point == "" {
				return fmt.Errorf("%s %d: needs a point", t.kind, e.Addr)
			}
			if prev, dup := used[e.Addr]; dup {
				return fmt.Errorf("%s %d (%s) duplicates %s", t.kind, e.Addr, e.Point, prev)
			}
			used[e.Addr] = e.Point
		}
	}
	return nil
}

// Names returns the sorted keys of a profile map.
func Names[T any](m map[string]T) []string {
	var n []string
	for k := range m {
		n = append(n, k)
	}
	sort.Strings(n)
	return n
}

// Validate checks that every device's protocol maps exist and refer to real points.
func (s *Set) Validate(st *site.Site) error {
	snap := st.Snapshot()
	for _, d := range st.Devices() {
		for proto, name := range d.Protocols {
			var points []string
			switch proto {
			case "modbus":
				m, ok := s.Modbus[name]
				if !ok {
					return fmt.Errorf("device %s: unknown modbus map %q (have %s)", d.ID, name, strings.Join(Names(s.Modbus), ", "))
				}
				for _, t := range [][]ModbusEntry{m.Holding, m.Input, m.Coils, m.Discrete} {
					for _, e := range t {
						if e.Point != "" {
							points = append(points, e.Point)
						}
					}
				}
			case "snmp":
				m, ok := s.SNMP[name]
				if !ok {
					return fmt.Errorf("device %s: unknown snmp map %q (have %s)", d.ID, name, strings.Join(Names(s.SNMP), ", "))
				}
				for _, o := range m.Objects {
					if o.Point != "" {
						points = append(points, o.Point)
					}
				}
				for _, x := range m.Sensors {
					points = append(points, x.Point)
				}
			case "redfish":
			default:
				return fmt.Errorf("device %s: unknown protocol %q", d.ID, proto)
			}
			for _, p := range points {
				if _, ok := snap.Devices[d.ID][p]; !ok {
					return fmt.Errorf("device %s (%s): %s map %s uses point %q, which a %s does not have", d.ID, d.Type, proto, name, p, d.Type)
				}
			}
		}
	}
	return nil
}

// Identity returns a device identity field.
func Identity(d site.DeviceConfig, field string) string {
	switch field {
	case "vendor":
		if d.Vendor != "" {
			return d.Vendor
		}
		return "otsim"
	case "model":
		if d.Model != "" {
			return d.Model
		}
		return "simulated " + d.Type
	case "serial":
		if d.Serial != "" {
			return d.Serial
		}
		return "SIM-" + strings.ToUpper(d.ID)
	case "name":
		return d.Name
	case "location":
		return d.Location
	}
	return ""
}

// ToDisplay converts an SI value to the region's display unit for a quantity.
func ToDisplay(r *site.Region, quantity string, v float64) float64 {
	switch {
	case quantity == "temperature" && r.Units.Temperature == "F":
		return v*9/5 + 32
	case quantity == "flow" && r.Units.Flow == "gpm":
		return v / 3.785411784
	case quantity == "pressure" && r.Units.Pressure == "psi":
		return v / 6.894757
	}
	return v
}

// FromDisplay is the inverse of ToDisplay (for writes).
func FromDisplay(r *site.Region, quantity string, v float64) float64 {
	switch {
	case quantity == "temperature" && r.Units.Temperature == "F":
		return (v - 32) * 5 / 9
	case quantity == "flow" && r.Units.Flow == "gpm":
		return v * 3.785411784
	case quantity == "pressure" && r.Units.Pressure == "psi":
		return v * 6.894757
	}
	return v
}
