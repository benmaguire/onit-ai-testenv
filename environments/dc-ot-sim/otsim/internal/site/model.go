package site

import (
	"fmt"
	"math"
	"math/rand"
	"sort"
	"sync"
	"time"
)

// Device is one simulated OT device: computed points plus writable settings.
type Device struct {
	Config DeviceConfig
	Index  int // position in the site config (for per-device ports)
	// Points are the device's current values (measurements, states, setpoints).
	Points map[string]float64
	// Settings are writable setpoints (written via Modbus/Redfish/API); they
	// also appear in Points.
	Settings map[string]float64
}

// Alarm is an active alarm.
type Alarm struct {
	Device   string `json:"device"`
	Name     string `json:"name"`
	Severity string `json:"severity"` // warning | critical
	Message  string `json:"message"`
}

// Site is the running model. All methods are safe for concurrent use.
type Site struct {
	mu      sync.RWMutex
	cfg     *Config
	region  *Region
	devices []*Device
	byID    map[string]*Device
	faults  map[string]*Fault
	nextID  int
	rng     *rand.Rand
	simTime time.Time // model clock (advances TimeScale x real time)
	alarms  []Alarm
	kpi     map[string]float64

	// internal state
	upsCharge      map[string]float64 // percent
	genState       map[string]string  // stopped | starting | running | cooldown
	genTimer       map[string]float64 // seconds in current state
	utilityLostFor float64
	utilityOKFor   float64
	atsSource      string             // utility | generator | none
	temps          map[string]float64 // lagged temperatures by key
	reservoir      map[string]float64 // CDU coolant litres
	rackPowered    map[int]bool
	energyKWh      map[string]float64
}

// New builds a site from config and region.
func New(cfg *Config, region *Region, start time.Time) *Site {
	s := &Site{
		cfg: cfg, region: region, byID: map[string]*Device{}, faults: map[string]*Fault{},
		rng: rand.New(rand.NewSource(42)), simTime: start,
		upsCharge: map[string]float64{}, genState: map[string]string{}, genTimer: map[string]float64{},
		atsSource: "utility", temps: map[string]float64{}, reservoir: map[string]float64{},
		rackPowered: map[int]bool{}, energyKWh: map[string]float64{}, kpi: map[string]float64{},
	}
	for i, dc := range cfg.Devices {
		d := &Device{Config: dc, Index: i, Points: map[string]float64{}, Settings: map[string]float64{}}
		switch dc.Type {
		case "ups":
			s.upsCharge[dc.ID] = 100
		case "generator":
			s.genState[dc.ID] = "stopped"
		case "cdu":
			d.Settings["supply_setpoint_c"] = dc.Params["supplySetpointC"]
			d.Settings["pump_speed_pct"] = 80
			for p := 1; p <= int(dc.Params["pumps"]); p++ {
				d.Settings[fmt.Sprintf("pump%d_enable", p)] = 1
			}
			s.reservoir[dc.ID] = dc.Params["reservoirL"]
		case "crah":
			d.Settings["supply_air_setpoint_c"] = dc.Params["supplyAirSetpointC"]
			d.Settings["enable"] = 1
		case "chiller":
			d.Settings["chilled_water_setpoint_c"] = dc.Params["chilledWaterSetpointC"]
			d.Settings["enable"] = 1
		case "rack-pdu":
			s.rackPowered[dc.Rack] = true
		}
		s.devices = append(s.devices, d)
		s.byID[dc.ID] = d
	}
	s.tickLocked(0)
	return s
}

// Region returns the site's region profile.
func (s *Site) Region() *Region { return s.region }

// Config returns the site config.
func (s *Site) Config() *Config { return s.cfg }

// Devices returns device IDs in config order.
func (s *Site) Devices() []DeviceConfig {
	out := make([]DeviceConfig, len(s.devices))
	for i, d := range s.devices {
		out[i] = d.Config
	}
	return out
}

// Point reads one point (ok=false if unknown).
func (s *Site) Point(device, point string) (float64, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	d, ok := s.byID[device]
	if !ok {
		return 0, false
	}
	if v, ok := s.stuckValue(device, point); ok {
		return v, true
	}
	v, ok := d.Points[point]
	return v, ok
}

// SetPoint writes a writable setting (ok=false if not writable).
func (s *Site) SetPoint(device, point string, v float64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.byID[device]
	if !ok {
		return fmt.Errorf("unknown device %q", device)
	}
	if _, ok := d.Settings[point]; !ok {
		return fmt.Errorf("%s: point %q is not writable", device, point)
	}
	if err := validSetting(point, v); err != nil {
		return err
	}
	d.Settings[point] = v
	d.Points[point] = v
	return nil
}

func validSetting(point string, v float64) error {
	switch {
	case point == "pump_speed_pct" && (v < 20 || v > 100):
		return fmt.Errorf("pump_speed_pct must be 20..100")
	case (point == "supply_setpoint_c" || point == "chilled_water_setpoint_c") && (v < 5 || v > 50):
		return fmt.Errorf("%s must be 5..50 °C", point)
	case point == "supply_air_setpoint_c" && (v < 15 || v > 32):
		return fmt.Errorf("supply_air_setpoint_c must be 15..32 °C")
	case (point == "enable" || len(point) > 7 && point[len(point)-7:] == "_enable") && v != 0 && v != 1:
		return fmt.Errorf("%s must be 0 or 1", point)
	}
	return nil
}

// Snapshot is a consistent copy of the site state.
type Snapshot struct {
	Site    string                        `json:"site"`
	Region  string                        `json:"region"`
	SimTime time.Time                     `json:"simTime"`
	KPI     map[string]float64            `json:"kpi"`
	Alarms  []Alarm                       `json:"alarms"`
	Devices map[string]map[string]float64 `json:"devices"`
	Types   map[string]string             `json:"types"`
	Faults  []Fault                       `json:"faults"`
}

// Snapshot returns all points, KPIs, alarms and faults.
func (s *Site) Snapshot() Snapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	snap := Snapshot{Site: s.cfg.Site.Name, Region: s.region.Name, SimTime: s.simTime,
		KPI: map[string]float64{}, Devices: map[string]map[string]float64{}, Types: map[string]string{}}
	for k, v := range s.kpi {
		snap.KPI[k] = v
	}
	snap.Alarms = append([]Alarm(nil), s.alarms...)
	for _, d := range s.devices {
		m := map[string]float64{}
		for k, v := range d.Points {
			if sv, ok := s.stuckValue(d.Config.ID, k); ok {
				v = sv
			}
			m[k] = v
		}
		snap.Devices[d.Config.ID] = m
		snap.Types[d.Config.ID] = d.Config.Type
	}
	for _, f := range s.faults {
		snap.Faults = append(snap.Faults, *f)
	}
	sort.Slice(snap.Faults, func(a, b int) bool { return snap.Faults[a].ID < snap.Faults[b].ID })
	return snap
}

// Run ticks the model until stop is closed.
func (s *Site) Run(stop <-chan struct{}) {
	t := time.NewTicker(time.Duration(s.cfg.TickSeconds * float64(time.Second)))
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			s.Tick(s.cfg.TickSeconds * s.cfg.TimeScale)
		}
	}
}

// Tick advances the model by dt simulated seconds.
func (s *Site) Tick(dt float64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tickLocked(dt)
}

// lag moves a stored temperature towards target with time constant tau (s).
func (s *Site) lag(key string, target, tau, dt float64) float64 {
	cur, ok := s.temps[key]
	if !ok || dt == 0 {
		s.temps[key] = target
		return target
	}
	cur += (target - cur) * (1 - math.Exp(-dt/tau))
	s.temps[key] = cur
	return cur
}

func round(v float64, places int) float64 {
	p := math.Pow(10, float64(places))
	return math.Round(v*p) / p
}

func b2f(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

func (s *Site) ofType(t string) []*Device {
	var out []*Device
	for _, d := range s.devices {
		if d.Config.Type == t {
			out = append(out, d)
		}
	}
	return out
}

func (s *Site) alarm(dev, name, sev, msg string) {
	s.alarms = append(s.alarms, Alarm{Device: dev, Name: name, Severity: sev, Message: msg})
}

// tickLocked is the physics model. Order: load -> power path -> cooling -> KPIs -> alarms.
func (s *Site) tickLocked(dt float64) {
	s.simTime = s.simTime.Add(time.Duration(dt * float64(time.Second)))
	s.expire(time.Now())
	s.alarms = nil
	r := s.region
	hour := float64(s.simTime.Hour()) + float64(s.simTime.Minute())/60

	// --- ambient -------------------------------------------------------------------
	ambient := r.Climate.DesignAmbientC + r.Climate.DiurnalSwingC*math.Sin((hour-9)/24*2*math.Pi) + s.faultParam("high-ambient", "", "deltaC")

	// --- IT load -------------------------------------------------------------------
	cfg := s.cfg.ITLoad
	diurnal := 1 + cfg.DiurnalPct/100*math.Sin((hour-8)/24*2*math.Pi)
	noise := 1 + cfg.NoisePct/100*(s.rng.Float64()*2-1)
	surge := 1.0
	if f := s.faultOfType("it-load-surge", ""); f != nil {
		surge = paramOr(f.Params, "multiplier", 1.3)
	}
	pdus := s.ofType("rack-pdu")
	racks := len(pdus)
	if racks == 0 {
		racks = 1
	}
	perRack := cfg.BaseKW * diurnal * noise * surge / float64(racks)

	// --- utility, ATS, generator -----------------------------------------------------
	nominalV, nominalHz := r.VoltageLN, r.FrequencyHz
	utilityUp := s.faultOfType("utility-outage", "") == nil
	utilV := nominalV * (1 + 0.01*(s.rng.Float64()*2-1))
	if f := s.faultOfType("utility-sag", ""); f != nil {
		utilV = nominalV * paramOr(f.Params, "voltagePct", 80) / 100
	}
	utilHz := nominalHz + 0.02*(s.rng.Float64()*2-1) + s.faultParam("frequency-deviation", "", "deltaHz")
	if !utilityUp {
		utilV, utilHz = 0, 0
	}
	utilityOK := utilityUp && utilV > 0.85*nominalV && math.Abs(utilHz-nominalHz) < 1.0
	if utilityOK {
		s.utilityOKFor += dt
		s.utilityLostFor = 0
	} else {
		s.utilityLostFor += dt
		s.utilityOKFor = 0
	}
	genRunning := false
	var genKWCap float64
	for _, g := range s.ofType("generator") {
		id := g.Config.ID
		st := s.genState[id]
		s.genTimer[id] += dt
		switch st {
		case "stopped":
			if !utilityOK {
				st, s.genTimer[id] = "starting", 0
			}
		case "starting":
			if s.faultOfType("generator-fail-to-start", id) != nil {
				st = "failed"
			} else if s.genTimer[id] >= g.Config.Params["startDelaySeconds"] {
				st, s.genTimer[id] = "running", 0
			}
		case "running":
			if utilityOK && s.utilityOKFor >= s.atsRetransfer() {
				st, s.genTimer[id] = "cooldown", 0
			}
		case "cooldown":
			if !utilityOK {
				st, s.genTimer[id] = "running", 0
			} else if s.genTimer[id] >= g.Config.Params["cooldownSeconds"] {
				st, s.genTimer[id] = "stopped", 0
			}
		case "failed":
			if s.faultOfType("generator-fail-to-start", id) == nil {
				st, s.genTimer[id] = "stopped", 0
			}
		}
		s.genState[id] = st
		running := st == "running" || st == "cooldown"
		if running {
			genRunning = true
			genKWCap += g.Config.Params["ratedKW"]
		}
		g.Points["running"] = b2f(running)
		g.Points["state"] = map[string]float64{"stopped": 0, "starting": 1, "running": 2, "cooldown": 3, "failed": 4}[st]
		g.Points["output_voltage_v"] = round(b2f(running)*nominalV, 1)
		g.Points["output_frequency_hz"] = round(b2f(running)*nominalHz, 2)
		g.Points["run_seconds"] = round(s.genTimer[id]*b2f(running), 0)
		if st == "failed" {
			s.alarm(id, "fail_to_start", "critical", "generator failed to start")
		} else if running {
			s.alarm(id, "running", "warning", "generator running")
		}
	}
	switch {
	case utilityOK && (s.atsSource == "utility" || s.utilityOKFor >= s.atsRetransfer() || !genRunning):
		s.atsSource = "utility"
	case genRunning:
		s.atsSource = "generator"
	default:
		s.atsSource = "none"
	}
	inputAvailable := s.atsSource != "none"
	for _, a := range s.ofType("ats") {
		a.Points["source"] = map[string]float64{"none": 0, "utility": 1, "generator": 2}[s.atsSource]
		a.Points["utility_available"] = b2f(utilityOK)
		a.Points["generator_available"] = b2f(genRunning)
		if s.atsSource == "generator" {
			s.alarm(a.Config.ID, "on_generator", "warning", "ATS on generator")
		}
	}
	for _, u := range s.ofType("utility") {
		u.Points["voltage_v"] = round(utilV, 1)
		u.Points["voltage_ll_v"] = round(utilV*math.Sqrt(3), 1)
		u.Points["frequency_hz"] = round(utilHz, 2)
		u.Points["available"] = b2f(utilityOK)
		if !utilityOK {
			s.alarm(u.Config.ID, "utility_lost", "critical", "utility supply lost or out of tolerance")
		}
	}

	// --- UPS -> racks ------------------------------------------------------------------
	itKW := 0.0
	upsInKW := 0.0
	upsOut := true
	upss := s.ofType("ups")
	for _, u := range upss {
		id := u.Config.ID
		p := u.Config.Params
		capPct := 100.0
		if f := s.faultOfType("ups-battery-degraded", id); f != nil {
			capPct = paramOr(f.Params, "capacityPct", 30)
		}
		batKWh := p["batteryKWh"] * capPct / 100
		load := 0.0
		for _, pdu := range pdus {
			if s.rackPowered[pdu.Config.Rack] {
				load += perRack * s.rackMultiplier(pdu.Config.ID)
			}
		}
		load /= float64(len(upss))
		onBattery := !inputAvailable
		charge := s.upsCharge[id]
		if charge > capPct {
			charge = capPct
		}
		if onBattery {
			charge -= (load / p["efficiency"]) * dt / 3600 / math.Max(batKWh, 0.001) * 100
		} else if charge < capPct {
			charge += p["rechargeKW"] * dt / 3600 / math.Max(batKWh, 0.001) * 100
		}
		charge = math.Max(0, math.Min(capPct, charge))
		s.upsCharge[id] = charge
		output := !(onBattery && charge <= 0) && s.faultOfType("ups-failure", id) == nil
		if !output {
			upsOut = false
			load = 0
		}
		in := 0.0
		if !onBattery {
			in = load/p["efficiency"] + b2f(charge < capPct)*p["rechargeKW"]
		}
		upsInKW += in
		itKW += load
		runtimeMin := 0.0
		if load > 0 {
			runtimeMin = charge / 100 * batKWh / (load / p["efficiency"]) * 60
		}
		src := 3.0 // UPS-MIB upsOutputSource: normal
		switch {
		case !output:
			src = 2 // none
		case onBattery:
			src = 5 // battery
		}
		u.Points["input_voltage_v"] = round(b2f(!onBattery)*utilOr(s.atsSource == "generator", nominalV, utilV), 1)
		u.Points["input_frequency_hz"] = round(b2f(!onBattery)*utilOr(s.atsSource == "generator", nominalHz, utilHz), 2)
		u.Points["input_kw"] = round(in, 2)
		u.Points["output_voltage_v"] = round(b2f(output)*nominalV, 1)
		u.Points["output_frequency_hz"] = round(b2f(output)*nominalHz, 2)
		u.Points["output_kw"] = round(load, 2)
		u.Points["output_current_a"] = round(load*1000/(3*nominalV*0.98)*b2f(output), 1)
		u.Points["output_load_pct"] = round(load/p["ratedKW"]*100, 1)
		u.Points["output_source"] = src
		u.Points["on_battery"] = b2f(onBattery)
		u.Points["battery_charge_pct"] = round(charge, 1)
		u.Points["battery_runtime_min"] = round(runtimeMin, 1)
		u.Points["battery_seconds_on"] = round(b2f(onBattery)*s.utilityLostFor, 0)
		u.Points["battery_status"] = upsBatteryStatus(charge, onBattery)
		u.Points["battery_temp_c"] = round(25+0.5*math.Sin(hour), 1)
		u.Points["battery_voltage_v"] = round(480*(0.85+0.15*charge/100), 1)
		s.energyKWh[id] += in * dt / 3600
		if onBattery {
			s.alarm(id, "on_battery", "critical", fmt.Sprintf("on battery, %.0f%% charge, %.1f min remaining", charge, runtimeMin))
		}
		if !output {
			s.alarm(id, "output_lost", "critical", "UPS output lost: racks unpowered")
		}
		if capPct < 100 {
			s.alarm(id, "battery_degraded", "warning", fmt.Sprintf("battery capacity %.0f%%", capPct))
		}
		if u.Points["output_load_pct"] > 90 {
			s.alarm(id, "overload", "warning", "UPS load above 90%")
		}
	}
	if len(upss) == 0 {
		for _, pdu := range pdus {
			if s.rackPowered[pdu.Config.Rack] && inputAvailable {
				itKW += perRack * s.rackMultiplier(pdu.Config.ID)
			}
		}
		upsInKW = itKW
	}

	// --- rack PDUs (3-phase) --------------------------------------------------------------
	for _, pdu := range pdus {
		id := pdu.Config.ID
		p := pdu.Config.Params
		demand := perRack * s.rackMultiplier(id)
		demandA := demand * 1000 / (3 * nominalV * p["powerFactor"])
		// A breaker opens on a trip fault, or when an overload pushes the phase
		// current past its rating; it closes again when the fault is cleared.
		tripped := s.faultOfType("pdu-breaker-trip", id) != nil || demandA > p["breakerAmps"]
		powered := !tripped
		s.rackPowered[pdu.Config.Rack] = powered
		load := demand * b2f(powered && upsOut && inputAvailable)
		phaseA := load * 1000 / (3 * nominalV * p["powerFactor"])
		pdu.Points["power_kw"] = round(load, 2)
		pdu.Points["power_w"] = round(load*1000, 0)
		pdu.Points["voltage_v"] = round(b2f(load > 0 || (upsOut && inputAvailable && powered))*nominalV, 1)
		pdu.Points["frequency_hz"] = round(b2f(powered)*nominalHz, 2)
		for i, ph := range []string{"l1", "l2", "l3"} {
			pdu.Points["current_"+ph+"_a"] = round(phaseA*(1+0.03*float64(i-1)), 2)
		}
		pdu.Points["breaker_closed"] = b2f(powered)
		pdu.Points["load_pct"] = round(phaseA/p["breakerAmps"]*100, 1)
		s.energyKWh[id] += load * dt / 3600
		pdu.Points["energy_kwh"] = round(s.energyKWh[id], 3)
		if !powered {
			s.alarm(id, "breaker_open", "critical", fmt.Sprintf("rack %d unpowered", pdu.Config.Rack))
		} else if pdu.Points["load_pct"] > 80 {
			s.alarm(id, "high_load", "warning", "PDU phase current above 80% of breaker rating")
		}
	}

	// --- cooling: chiller -> facility water -> CDU (liquid) / CRAH (air) -----------------
	heatLiquid := itKW * s.cfg.LiquidFraction
	heatAir := itKW*(1-s.cfg.LiquidFraction) + itKW*s.cfg.MiscFraction
	coolingKW := 0.0
	// Chillers.
	chillers := s.ofType("chiller")
	chwCap, chwSet := 0.0, 18.0
	for _, c := range chillers {
		chwSet = c.Settings["chilled_water_setpoint_c"]
		running := c.Settings["enable"] == 1 && s.faultOfType("chiller-trip", c.Config.ID) == nil && inputAvailable
		if running {
			chwCap += c.Config.Params["capacityKW"]
		}
		c.Points["running"] = b2f(running)
		if !running && c.Settings["enable"] == 1 {
			s.alarm(c.Config.ID, "tripped", "critical", "chiller not running")
		}
	}
	totalHeat := heatLiquid + heatAir
	chwSupplyTarget := chwSet
	if len(chillers) > 0 && totalHeat > chwCap {
		// Not enough chiller capacity: facility water warms in proportion to the deficit.
		chwSupplyTarget = chwSet + 20*(totalHeat-chwCap)/math.Max(totalHeat, 1)
	}
	chwSupply := s.lag("chw_supply", chwSupplyTarget, 300, dt)
	for _, c := range chillers {
		cop := c.Config.Params["copRated"] * math.Max(0.4, 1-0.025*(ambient-25))
		share := 0.0
		if c.Points["running"] == 1 && chwCap > 0 {
			share = math.Min(totalHeat, chwCap) * c.Config.Params["capacityKW"] / chwCap
		}
		power := share / cop
		coolingKW += power
		c.Points["cooling_load_kw"] = round(share, 1)
		c.Points["power_kw"] = round(power, 1)
		c.Points["cop"] = round(cop*b2f(share > 0), 2)
		c.Points["chilled_water_supply_c"] = round(chwSupply, 2)
		c.Points["chilled_water_return_c"] = round(chwSupply+5*b2f(share > 0), 2)
		c.Points["chilled_water_setpoint_c"] = c.Settings["chilled_water_setpoint_c"]
		c.Points["enable"] = c.Settings["enable"]
		c.Points["ambient_c"] = round(ambient, 1)
	}
	// CDUs.
	cdus := s.ofType("cdu")
	maxReturn := 0.0
	for _, c := range cdus {
		id := c.Config.ID
		p := c.Config.Params
		running := 0
		failedPump := -1 // -1 none, 0 all pumps, n pump n
		if f := s.faultOfType("cdu-pump-failure", id); f != nil {
			failedPump = int(paramOr(f.Params, "pump", 1))
		}
		for i := 1; i <= int(p["pumps"]); i++ {
			key := fmt.Sprintf("pump%d", i)
			on := c.Settings[key+"_enable"] == 1 && inputAvailable && failedPump != 0 && failedPump != i
			c.Points[key+"_running"] = b2f(on)
			c.Points[key+"_enable"] = c.Settings[key+"_enable"]
			if on {
				running++
			}
		}
		// Leak: coolant lost from the secondary loop.
		leak := s.faultOfType("cdu-leak", id) != nil
		if leak {
			s.reservoir[id] = math.Max(0, s.reservoir[id]-paramOr(s.faultOfType("cdu-leak", id).Params, "litersPerMinute", 5)*dt/60)
		} else if s.reservoir[id] < p["reservoirL"] {
			s.reservoir[id] = math.Min(p["reservoirL"], s.reservoir[id]+20*dt/60) // auto top-up
		}
		levelPct := s.reservoir[id] / p["reservoirL"] * 100
		if levelPct < 15 {
			running = 0 // low-level interlock stops the pumps
		}
		// One pump at the set speed carries the design flow (N+1 redundancy).
		speed := c.Settings["pump_speed_pct"] / 100
		flow := math.Min(float64(running), 1) * p["pumpFlowLPM"] * speed * (1 + 0.15*math.Max(0, float64(running)-1))
		share := heatLiquid / math.Max(float64(len(cdus)), 1)
		setpoint := c.Settings["supply_setpoint_c"]
		supplyTarget := math.Max(setpoint, chwSupply+p["approachC"])
		if len(chillers) == 0 {
			supplyTarget = setpoint
		}
		supply := s.lag(id+"_supply", supplyTarget, 60, dt)
		dT := 0.0
		if flow > 0 {
			dT = share / (flow / 60 * 4.18) // kW / (kg/s * kJ/kgK)
		} else if share > 0 {
			dT = 40 // no flow: cold plates heat up fast
		}
		ret := s.lag(id+"_return", supply+dT, 45, dt)
		maxReturn = math.Max(maxReturn, ret)
		pumpKW := float64(running) * p["pumpKW"] * speed * speed * speed
		coolingKW += pumpKW
		c.Points["supply_temp_c"] = round(supply, 2)
		c.Points["return_temp_c"] = round(ret, 2)
		c.Points["supply_setpoint_c"] = setpoint
		c.Points["flow_lpm"] = round(flow, 1)
		c.Points["delta_pressure_kpa"] = round(120*(flow/math.Max(p["pumpFlowLPM"], 1))*(flow/math.Max(p["pumpFlowLPM"], 1)), 1)
		c.Points["heat_load_kw"] = round(share, 1)
		c.Points["pump_speed_pct"] = c.Settings["pump_speed_pct"]
		c.Points["pump_power_kw"] = round(pumpKW, 2)
		c.Points["reservoir_level_pct"] = round(levelPct, 1)
		c.Points["leak_detected"] = b2f(leak)
		c.Points["pumps_running"] = float64(running)
		c.Points["capacity_pct"] = round(share/p["capacityKW"]*100, 1)
		if running < int(p["pumps"]) && running > 0 {
			s.alarm(id, "pump_redundancy_lost", "warning", fmt.Sprintf("%d of %d pumps running", running, int(p["pumps"])))
		}
		if running == 0 && share > 0 {
			s.alarm(id, "no_flow", "critical", "no coolant flow")
		}
		if leak {
			s.alarm(id, "leak", "critical", "coolant leak detected")
		}
		if ret > 50 {
			s.alarm(id, "high_return_temp", "critical", fmt.Sprintf("secondary return %.1f °C", ret))
		}
	}
	// CRAHs and rack inlet temperatures (ASHRAE TC 9.9: recommended 18-27 °C, A1 allowable up to 32 °C).
	crahs := s.ofType("crah")
	airCap, airSet := 0.0, 22.0
	for _, c := range crahs {
		airSet = c.Settings["supply_air_setpoint_c"]
		running := c.Settings["enable"] == 1 && s.faultOfType("crah-failure", c.Config.ID) == nil && inputAvailable
		if running {
			airCap += c.Config.Params["capacityKW"]
		}
		c.Points["running"] = b2f(running)
		c.Points["enable"] = c.Settings["enable"]
		c.Points["supply_air_setpoint_c"] = airSet
		if !running && c.Settings["enable"] == 1 {
			s.alarm(c.Config.ID, "unit_failed", "critical", "CRAH not running")
		}
	}
	airSupplyTarget := math.Max(airSet, chwSupply+4)
	if len(chillers) == 0 {
		airSupplyTarget = airSet
	}
	deficit := 0.0
	if heatAir > airCap {
		deficit = (heatAir - airCap) / math.Max(heatAir, 1)
	}
	inletTarget := airSupplyTarget + 1.5 + 18*deficit
	inlet := s.lag("rack_inlet", inletTarget, 180, dt)
	for _, c := range crahs {
		share := 0.0
		if c.Points["running"] == 1 && airCap > 0 {
			share = math.Min(heatAir, airCap) * c.Config.Params["capacityKW"] / airCap
		}
		fan := c.Config.Params["fanKW"] * b2f(c.Points["running"] == 1) * (0.4 + 0.6*share/math.Max(c.Config.Params["capacityKW"], 1))
		coolingKW += fan
		c.Points["supply_air_temp_c"] = round(s.lag(c.Config.ID+"_sat", airSupplyTarget+b2f(c.Points["running"] == 0)*8, 90, dt), 2)
		c.Points["return_air_temp_c"] = round(inlet+12*b2f(share > 0), 2)
		c.Points["cooling_load_kw"] = round(share, 1)
		c.Points["fan_power_kw"] = round(fan, 2)
		c.Points["humidity_pct"] = round(45+3*math.Sin(hour/3), 1)
	}
	for i, pdu := range pdus {
		pdu.Points["inlet_temp_c"] = round(inlet+0.3*float64(i%4), 2)
		pdu.Points["outlet_temp_c"] = round(inlet+10*b2f(pdu.Points["power_kw"] > 0), 2)
		pdu.Points["humidity_pct"] = round(45+2*math.Sin(hour/2+float64(i)), 1)
	}
	if inlet > 32 {
		s.alarm("site", "inlet_temp_critical", "critical", fmt.Sprintf("rack inlet %.1f °C above ASHRAE A1 allowable (32 °C)", inlet))
	} else if inlet > 27 {
		s.alarm("site", "inlet_temp_high", "warning", fmt.Sprintf("rack inlet %.1f °C above ASHRAE recommended (27 °C)", inlet))
	}

	// --- leak sensors --------------------------------------------------------------------------
	for _, l := range s.ofType("leak-sensor") {
		detected := s.faultOfType("cdu-leak", l.Config.Location) != nil || s.faultOfType("leak", l.Config.ID) != nil
		l.Points["leak_detected"] = b2f(detected)
		l.Points["state"] = b2f(detected) * 2 // 0 OK, 2 critical
		if detected {
			s.alarm(l.Config.ID, "leak", "critical", "water detected at "+l.Config.Location)
		}
	}

	// --- facility totals and KPIs ----------------------------------------------------------------
	miscKW := itKW * s.cfg.MiscFraction
	facilityKW := upsInKW + coolingKW + miscKW
	if !inputAvailable {
		facilityKW = 0
	}
	for _, m := range s.ofType("power-meter") {
		kw := facilityKW
		v := utilOr(s.atsSource == "generator", nominalV, utilV)
		hz := utilOr(s.atsSource == "generator", nominalHz, utilHz)
		if !inputAvailable {
			v, hz = 0, 0
		}
		amps := kw * 1000 / math.Max(3*v*0.97, 1)
		s.energyKWh[m.Config.ID] += kw * dt / 3600
		m.Points["power_kw"] = round(kw, 2)
		m.Points["power_w"] = round(kw*1000, 0)
		m.Points["voltage_ln_v"] = round(v, 1)
		m.Points["voltage_ll_v"] = round(v*math.Sqrt(3), 1)
		m.Points["frequency_hz"] = round(hz, 2)
		m.Points["current_a"] = round(amps*3, 1)
		for _, ph := range []string{"l1", "l2", "l3"} {
			m.Points["current_"+ph+"_a"] = round(amps, 1)
			m.Points["voltage_"+ph+"_v"] = round(v, 1)
			m.Points["power_"+ph+"_w"] = round(kw*1000/3, 0)
		}
		m.Points["power_factor"] = 0.97
		m.Points["energy_kwh"] = round(s.energyKWh[m.Config.ID], 3)
		m.Points["energy_wh"] = math.Round(s.energyKWh[m.Config.ID] * 1000)
	}
	water := 0.0
	if r.Cooling.Evaporative {
		water = (totalHeat + coolingKW) * r.Cooling.WaterLPerKWhRej // L/h
	}
	s.kpi["it_kw"] = round(itKW, 2)
	s.kpi["facility_kw"] = round(facilityKW, 2)
	s.kpi["cooling_kw"] = round(coolingKW, 2)
	s.kpi["pue"] = round(facilityKW/math.Max(itKW, 0.001), 3)
	if itKW == 0 {
		s.kpi["pue"] = 0
	}
	s.kpi["water_l_per_h"] = round(water, 1)
	s.kpi["wue_l_per_kwh"] = round(water/math.Max(itKW, 0.001), 3)
	s.kpi["carbon_kg_per_h"] = round(facilityKW*r.GridCarbonGPerKWh/1000*b2f(s.atsSource == "utility"), 2)
	s.kpi["rack_inlet_c"] = round(inlet, 2)
	s.kpi["cdu_return_max_c"] = round(maxReturn, 2)
	s.kpi["chilled_water_supply_c"] = round(chwSupply, 2)
	s.kpi["ambient_c"] = round(ambient, 1)
	s.kpi["on_generator"] = b2f(s.atsSource == "generator")
	s.kpi["alarms_critical"] = 0
	s.kpi["alarms_warning"] = 0
	for _, a := range s.alarms {
		s.kpi["alarms_"+a.Severity]++
	}
	// Settings are visible as points too.
	for _, d := range s.devices {
		for k, v := range d.Settings {
			d.Points[k] = v
		}
	}
}

func (s *Site) atsRetransfer() float64 {
	for _, a := range s.ofType("ats") {
		return a.Config.Params["retransferSeconds"]
	}
	return 30
}

func (s *Site) rackMultiplier(pdu string) float64 {
	if f := s.faultOfType("pdu-overload", pdu); f != nil {
		return paramOr(f.Params, "multiplier", 1.6)
	}
	return 1
}

func utilOr(useNominal bool, nominal, actual float64) float64 {
	if useNominal {
		return nominal
	}
	return actual
}

// upsBatteryStatus follows UPS-MIB upsBatteryStatus: 2 normal, 3 low, 4 depleted.
func upsBatteryStatus(charge float64, onBattery bool) float64 {
	switch {
	case charge <= 0:
		return 4
	case onBattery && charge < 25:
		return 3
	default:
		return 2
	}
}
