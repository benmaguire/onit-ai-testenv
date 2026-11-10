package site

import (
	"os"
	"strings"
	"testing"
	"time"

	"sigs.k8s.io/yaml"
)

func testSite(t *testing.T) *Site {
	t.Helper()
	b, err := os.ReadFile("../../examples/site.yaml")
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := ParseConfig(b)
	if err != nil {
		t.Fatal(err)
	}
	rb, err := os.ReadFile("../../profiles/regions/au.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var r Region
	if err := yaml.Unmarshal(rb, &r); err != nil {
		t.Fatal(err)
	}
	s := New(cfg, &r, time.Date(2026, 11, 10, 14, 0, 0, 0, time.UTC))
	run(s, 600) // settle
	return s
}

func run(s *Site, seconds int) {
	for i := 0; i < seconds; i++ {
		s.Tick(1)
	}
}

func pt(t *testing.T, s *Site, dev, p string) float64 {
	t.Helper()
	v, ok := s.Point(dev, p)
	if !ok {
		t.Fatalf("no point %s.%s", dev, p)
	}
	return v
}

func hasAlarm(s *Site, dev, name string) bool {
	for _, a := range s.Snapshot().Alarms {
		if a.Device == dev && a.Name == name {
			return true
		}
	}
	return false
}

func TestSteadyState(t *testing.T) {
	s := testSite(t)
	k := s.Snapshot().KPI
	if k["it_kw"] < 300 || k["it_kw"] > 500 {
		t.Fatalf("it_kw %v", k["it_kw"])
	}
	if k["pue"] < 1.05 || k["pue"] > 1.6 {
		t.Fatalf("pue %v out of a plausible range", k["pue"])
	}
	if k["alarms_critical"] != 0 {
		t.Fatalf("critical alarms at steady state: %+v", s.Snapshot().Alarms)
	}
	if v := pt(t, s, "utility", "frequency_hz"); v < 49.9 || v > 50.1 {
		t.Fatalf("AU frequency %v", v)
	}
	if in := k["rack_inlet_c"]; in < 18 || in > 27 {
		t.Fatalf("inlet %v outside ASHRAE recommended", in)
	}
	if r, s2 := pt(t, s, "cdu-1", "return_temp_c"), pt(t, s, "cdu-1", "supply_temp_c"); r <= s2 || r-s2 > 15 {
		t.Fatalf("CDU supply %v return %v", s2, r)
	}
}

func TestUtilityOutageGeneratorAndRetransfer(t *testing.T) {
	s := testSite(t)
	f, err := s.AddFault(FaultRequest{Type: "utility-outage"})
	if err != nil {
		t.Fatal(err)
	}
	run(s, 3)
	if pt(t, s, "ups-1", "on_battery") != 1 || pt(t, s, "ups-1", "output_source") != 5 {
		t.Fatal("UPS not on battery after utility loss")
	}
	if !hasAlarm(s, "ups-1", "on_battery") || !hasAlarm(s, "utility", "utility_lost") {
		t.Fatalf("alarms: %+v", s.Snapshot().Alarms)
	}
	run(s, 12)
	if pt(t, s, "gen-1", "running") != 1 || pt(t, s, "ats-1", "source") != 2 {
		t.Fatal("generator not carrying the load after its start delay")
	}
	if pt(t, s, "ups-1", "on_battery") != 0 {
		t.Fatal("UPS still on battery with generator running")
	}
	if s.Snapshot().KPI["it_kw"] == 0 {
		t.Fatal("IT load dropped")
	}
	s.RemoveFault(f.ID)
	run(s, 10)
	if pt(t, s, "ats-1", "source") != 2 {
		t.Fatal("retransferred before the retransfer delay")
	}
	run(s, 30)
	if pt(t, s, "ats-1", "source") != 1 {
		t.Fatal("not back on utility after retransfer delay")
	}
	run(s, 70)
	if pt(t, s, "gen-1", "running") != 0 {
		t.Fatal("generator still running after cooldown")
	}
}

func TestGeneratorFailDrainsBattery(t *testing.T) {
	s := testSite(t)
	if _, err := s.AddFault(FaultRequest{Type: "generator-fail-to-start", Device: "gen-1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddFault(FaultRequest{Type: "ups-battery-degraded", Device: "ups-1", Params: map[string]float64{"capacityPct": 5}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddFault(FaultRequest{Type: "utility-outage"}); err != nil {
		t.Fatal(err)
	}
	run(s, 30)
	if !hasAlarm(s, "gen-1", "fail_to_start") {
		t.Fatal("no fail-to-start alarm")
	}
	// 5 kWh at ~400 kW lasts well under two minutes.
	run(s, 120)
	if pt(t, s, "ups-1", "battery_charge_pct") > 0.01 || s.Snapshot().KPI["it_kw"] != 0 || !hasAlarm(s, "ups-1", "output_lost") {
		t.Fatalf("expected depleted battery and dropped load: charge %v it %v", pt(t, s, "ups-1", "battery_charge_pct"), s.Snapshot().KPI["it_kw"])
	}
}

func TestCDUPumpFailures(t *testing.T) {
	s := testSite(t)
	flow := pt(t, s, "cdu-1", "flow_lpm")
	f, _ := s.AddFault(FaultRequest{Type: "cdu-pump-failure", Device: "cdu-1", Params: map[string]float64{"pump": 1}})
	run(s, 60)
	if !hasAlarm(s, "cdu-1", "pump_redundancy_lost") || pt(t, s, "cdu-1", "flow_lpm") < flow*0.8 {
		t.Fatalf("N+1: flow %v (was %v), alarms %+v", pt(t, s, "cdu-1", "flow_lpm"), flow, s.Snapshot().Alarms)
	}
	s.RemoveFault(f.ID)
	if _, err := s.AddFault(FaultRequest{Type: "cdu-pump-failure", Device: "cdu-1", Params: map[string]float64{"pump": 0}}); err != nil {
		t.Fatal(err)
	}
	run(s, 300)
	if pt(t, s, "cdu-1", "flow_lpm") != 0 || !hasAlarm(s, "cdu-1", "no_flow") || !hasAlarm(s, "cdu-1", "high_return_temp") {
		t.Fatalf("no-flow: flow %v return %v alarms %+v", pt(t, s, "cdu-1", "flow_lpm"), pt(t, s, "cdu-1", "return_temp_c"), s.Snapshot().Alarms)
	}
	s.ClearFaults()
	run(s, 600)
	if s.Snapshot().KPI["alarms_critical"] != 0 {
		t.Fatalf("not recovered: %+v", s.Snapshot().Alarms)
	}
}

func TestLeakDrainsReservoirAndStopsPumps(t *testing.T) {
	s := testSite(t)
	if _, err := s.AddFault(FaultRequest{Type: "cdu-leak", Device: "cdu-1", Params: map[string]float64{"litersPerMinute": 60}}); err != nil {
		t.Fatal(err)
	}
	run(s, 5)
	if pt(t, s, "leak-cdu-1", "leak_detected") != 1 || !hasAlarm(s, "leak-cdu-1", "leak") {
		t.Fatal("leak sensor did not alarm")
	}
	run(s, 200)
	if pt(t, s, "cdu-1", "pumps_running") != 0 || pt(t, s, "cdu-1", "reservoir_level_pct") >= 15 {
		t.Fatalf("pumps %v level %v", pt(t, s, "cdu-1", "pumps_running"), pt(t, s, "cdu-1", "reservoir_level_pct"))
	}
}

func TestCRAHAndChillerFailuresRaiseTemperatures(t *testing.T) {
	s := testSite(t)
	inlet := s.Snapshot().KPI["rack_inlet_c"]
	_, _ = s.AddFault(FaultRequest{Type: "crah-failure", Device: "crah-1"})
	_, _ = s.AddFault(FaultRequest{Type: "crah-failure", Device: "crah-2"})
	run(s, 900)
	if s.Snapshot().KPI["rack_inlet_c"] < inlet+8 || !hasAlarm(s, "site", "inlet_temp_critical") {
		t.Fatalf("inlet %v (was %v)", s.Snapshot().KPI["rack_inlet_c"], inlet)
	}
	s.ClearFaults()
	chw := s.Snapshot().KPI["chilled_water_supply_c"]
	_, _ = s.AddFault(FaultRequest{Type: "chiller-trip", Device: "chiller-1"})
	run(s, 900)
	if s.Snapshot().KPI["chilled_water_supply_c"] < chw+5 {
		t.Fatalf("chilled water %v (was %v)", s.Snapshot().KPI["chilled_water_supply_c"], chw)
	}
}

func TestBreakerTripAndOverload(t *testing.T) {
	s := testSite(t)
	_, _ = s.AddFault(FaultRequest{Type: "pdu-breaker-trip", Device: "pdu-r3"})
	run(s, 2)
	if pt(t, s, "pdu-r3", "power_kw") != 0 || pt(t, s, "pdu-r3", "breaker_closed") != 0 || !hasAlarm(s, "pdu-r3", "breaker_open") {
		t.Fatal("breaker trip did not drop the rack")
	}
	if pt(t, s, "pdu-r4", "power_kw") == 0 {
		t.Fatal("other racks affected")
	}
	s.ClearFaults()
	_, _ = s.AddFault(FaultRequest{Type: "pdu-overload", Device: "pdu-r5", Params: map[string]float64{"multiplier": 5}})
	run(s, 2)
	if pt(t, s, "pdu-r5", "breaker_closed") != 0 {
		t.Fatal("5x overload did not trip the breaker")
	}
}

func TestSensorFaultsAndValidation(t *testing.T) {
	s := testSite(t)
	f, err := s.AddFault(FaultRequest{Type: "sensor-stuck", Device: "cdu-1", Point: "supply_temp_c", Params: map[string]float64{"value": 99}})
	if err != nil {
		t.Fatal(err)
	}
	if v, _ := s.Point("cdu-1", "supply_temp_c"); v != 99 {
		t.Fatalf("stuck value %v", v)
	}
	if s.Snapshot().Devices["cdu-1"]["supply_temp_c"] != 99 {
		t.Fatal("snapshot ignores stuck sensor")
	}
	s.RemoveFault(f.ID)
	if v, _ := s.Point("cdu-1", "supply_temp_c"); v == 99 {
		t.Fatal("still stuck")
	}
	if _, err := s.AddFault(FaultRequest{Type: "comms-loss", Device: "ups-1", Protocol: "snmp"}); err != nil || !s.CommsDown("ups-1", "snmp") || s.CommsDown("ups-1", "modbus") {
		t.Fatalf("comms-loss %v", err)
	}
	for _, bad := range []FaultRequest{
		{Type: "nope"},
		{Type: "cdu-pump-failure", Device: "ups-1"},
		{Type: "cdu-pump-failure"},
		{Type: "utility-outage", Device: "ups-1"},
		{Type: "sensor-stuck", Device: "cdu-1", Point: "nope"},
		{Type: "cdu-leak", Device: "cdu-1", Params: map[string]float64{"bogus": 1}},
	} {
		if _, err := s.AddFault(bad); err == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
	if err := s.SetPoint("cdu-1", "supply_setpoint_c", 80); err == nil || !strings.Contains(err.Error(), "5..50") {
		t.Fatalf("out-of-range setpoint accepted: %v", err)
	}
	if err := s.SetPoint("cdu-1", "flow_lpm", 1); err == nil {
		t.Fatal("measurement writable")
	}
}

func TestFaultExpiry(t *testing.T) {
	s := testSite(t)
	_, _ = s.AddFault(FaultRequest{Type: "high-ambient", DurationSeconds: 0.05})
	time.Sleep(80 * time.Millisecond)
	s.Tick(1)
	if len(s.Snapshot().Faults) != 0 {
		t.Fatal("fault did not expire")
	}
}
