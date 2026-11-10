// Package e2e exercises otsim's protocol servers with real clients.
package e2e

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gosnmp/gosnmp"
	"github.com/prometheus/client_golang/prometheus"
	mbc "github.com/simonvetter/modbus"

	"github.com/benmaguire/onit-ai-testenv/environments/dc-ot-sim/otsim/internal/api"
	"github.com/benmaguire/onit-ai-testenv/environments/dc-ot-sim/otsim/internal/modbus"
	"github.com/benmaguire/onit-ai-testenv/environments/dc-ot-sim/otsim/internal/profile"
	"github.com/benmaguire/onit-ai-testenv/environments/dc-ot-sim/otsim/internal/redfish"
	"github.com/benmaguire/onit-ai-testenv/environments/dc-ot-sim/otsim/internal/site"
	"github.com/benmaguire/onit-ai-testenv/environments/dc-ot-sim/otsim/internal/snmp"
	"github.com/benmaguire/onit-ai-testenv/environments/dc-ot-sim/otsim/internal/telegraf"
)

func freePort(t *testing.T, network string) int {
	t.Helper()
	if network == "udp" {
		c, err := net.ListenPacket("udp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		return c.LocalAddr().(*net.UDPAddr).Port
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func newSite(t *testing.T, region string) (*site.Site, *profile.Set) {
	t.Helper()
	cfg, err := site.LoadConfig("../../examples/site.yaml")
	if err != nil {
		t.Fatal(err)
	}
	set, err := profile.Load("")
	if err != nil {
		t.Fatal(err)
	}
	st := site.New(cfg, set.Regions[region], time.Date(2026, 11, 10, 14, 0, 0, 0, time.UTC))
	for i := 0; i < 300; i++ {
		st.Tick(1)
	}
	if err := set.Validate(st); err != nil {
		t.Fatal(err)
	}
	return st, set
}

func modbusClient(t *testing.T, port int, unit uint8) *mbc.ModbusClient {
	t.Helper()
	c, err := mbc.NewClient(&mbc.ClientConfiguration{URL: fmt.Sprintf("tcp://127.0.0.1:%d", port), Timeout: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Open(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	_ = c.SetUnitId(unit)
	return c
}

func unitOf(st *site.Site, id string) uint8 {
	for i, d := range st.Devices() {
		if d.ID == id {
			return uint8(i + 1)
		}
	}
	return 0
}

func TestModbusGatewayAndRegionUnits(t *testing.T) {
	st, set := newSite(t, "us")
	gw := freePort(t, "tcp")
	if _, err := modbus.Serve(st, set, fmt.Sprintf("tcp://127.0.0.1:%d", gw), "127.0.0.1", 0); err != nil {
		t.Fatal(err)
	}
	c := modbusClient(t, gw, unitOf(st, "cdu-1"))
	regs, err := c.ReadRegisters(0, 2, mbc.INPUT_REGISTER)
	if err != nil {
		t.Fatal(err)
	}
	supplyC, _ := st.Point("cdu-1", "supply_temp_c")
	gotF := float64(int16(regs[0])) / 10
	if want := supplyC*9/5 + 32; gotF < want-0.2 || gotF > want+0.2 {
		t.Fatalf("US region: supply %v °F, want %v (%.2f °C)", gotF, want, supplyC)
	}
	// Write the setpoint in °F (90 °F = 32.2 °C) through the holding register.
	if err := c.WriteRegister(0, 900); err != nil {
		t.Fatal(err)
	}
	if v, _ := st.Point("cdu-1", "supply_setpoint_c"); v < 32.1 || v > 32.3 {
		t.Fatalf("setpoint after write %v °C", v)
	}
	if err := c.WriteRegister(2, 1); err == nil {
		t.Fatal("write outside the map accepted")
	}
	if _, err := c.ReadRegisters(100, 1, mbc.INPUT_REGISTER); err == nil {
		t.Fatal("read outside the map accepted")
	}
	coils, err := c.ReadCoils(0, 2)
	if err != nil || !coils[0] || !coils[1] {
		t.Fatalf("pump enables %v %v", coils, err)
	}
	if err := c.WriteCoil(1, false); err != nil {
		t.Fatal(err)
	}
	if v, _ := st.Point("cdu-1", "pump2_enable"); v != 0 {
		t.Fatal("coil write ignored")
	}
}

func TestSunSpecMeterPerDevicePort(t *testing.T) {
	st, set := newSite(t, "au")
	base := freePort(t, "tcp")
	if _, err := modbus.Serve(st, set, "", "127.0.0.1", base); err != nil {
		t.Skipf("per-device ports unavailable: %v", err)
	}
	idx := int(unitOf(st, "meter-main")) - 1
	c := modbusClient(t, base+idx, 1)
	hdr, err := c.ReadRegisters(40000, 4, mbc.HOLDING_REGISTER)
	if err != nil {
		t.Fatal(err)
	}
	if hdr[0] != 0x5375 || hdr[1] != 0x6e53 || hdr[2] != 1 || hdr[3] != 66 {
		t.Fatalf("SunSpec header %x", hdr)
	}
	m, _ := c.ReadRegisters(40070, 18, mbc.HOLDING_REGISTER)
	if m[0] != 203 || m[1] != 105 {
		t.Fatalf("model 203 header %v", m[:2])
	}
	hz := float64(int16(m[16])) * 0.01 // Hz with Hz_SF = -2
	if hz < 49.9 || hz > 50.1 || int16(m[17]) != -2 {
		t.Fatalf("Hz %v SF %v", hz, int16(m[17]))
	}
	mn, _ := c.ReadRegisters(40004, 16, mbc.HOLDING_REGISTER)
	if s := regsToString(mn); s != "ExampleCo" {
		t.Fatalf("Mn %q", s)
	}
	// Comms loss: the device stops answering (client times out).
	modbus.CommsLossDelay = 3 * time.Second
	if _, err := st.AddFault(site.FaultRequest{Type: "comms-loss", Device: "meter-main", Protocol: "modbus"}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ReadRegisters(40000, 2, mbc.HOLDING_REGISTER); err == nil {
		t.Fatal("answered during comms-loss")
	}
}

func regsToString(r []uint16) string {
	var b []byte
	for _, x := range r {
		b = append(b, byte(x>>8), byte(x))
	}
	return strings.TrimRight(string(b), "\x00")
}

func TestSNMPUPSMIBAndEntitySensorWalk(t *testing.T) {
	st, set := newSite(t, "au")
	agent := snmp.New(st, set, "public")
	port := freePort(t, "udp")
	if err := agent.Serve(fmt.Sprintf("127.0.0.1:%d", port), ""); err != nil {
		t.Fatal(err)
	}
	g := &gosnmp.GoSNMP{Target: "127.0.0.1", Port: uint16(port), Community: "ups-1", Version: gosnmp.Version2c, Timeout: 2 * time.Second, Retries: 1}
	if err := g.Connect(); err != nil {
		t.Fatal(err)
	}
	defer g.Conn.Close()
	r, err := g.Get([]string{"1.3.6.1.2.1.33.1.2.4.0", "1.3.6.1.2.1.33.1.4.1.0", "1.3.6.1.2.1.33.1.1.1.0", "1.3.6.1.2.1.1.5.0", "1.3.6.1.2.1.33.9.9.0"})
	if err != nil {
		t.Fatal(err)
	}
	if gosnmp.ToBigInt(r.Variables[0].Value).Int64() != 100 || gosnmp.ToBigInt(r.Variables[1].Value).Int64() != 3 {
		t.Fatalf("charge %v source %v", r.Variables[0].Value, r.Variables[1].Value)
	}
	if string(r.Variables[2].Value.([]byte)) != "ExampleCo" || string(r.Variables[3].Value.([]byte)) != "ups-1" {
		t.Fatalf("identity %v %v", r.Variables[2].Value, r.Variables[3].Value)
	}
	if r.Variables[4].Type != gosnmp.NoSuchObject {
		t.Fatalf("unknown OID type %v", r.Variables[4].Type)
	}
	// Battery runs down on a utility outage and the MIB shows it.
	_, _ = st.AddFault(site.FaultRequest{Type: "utility-outage"})
	st.Tick(5)
	r, _ = g.Get([]string{"1.3.6.1.2.1.33.1.4.1.0"})
	if gosnmp.ToBigInt(r.Variables[0].Value).Int64() != 5 {
		t.Fatalf("upsOutputSource on battery = %v", r.Variables[0].Value)
	}
	// Walk a PDU's ENTITY-SENSOR-MIB.
	g.Community = "pdu-r1"
	var rows []gosnmp.SnmpPDU
	if err := g.BulkWalk("1.3.6.1.2.1.99.1.1.1", func(p gosnmp.SnmpPDU) error { rows = append(rows, p); return nil }); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 10*6 {
		t.Fatalf("entPhySensorTable rows %d, want 60", len(rows))
	}
	// Wrong community: no answer.
	g.Community = "nope"
	g.Timeout, g.Retries = 300*time.Millisecond, 0
	if _, err := g.Get([]string{"1.3.6.1.2.1.1.5.0"}); err == nil {
		t.Fatal("answered an unknown community")
	}
}

func TestRedfishCDUAndPDU(t *testing.T) {
	st, _ := newSite(t, "au")
	srv := httptest.NewServer(&redfish.Handler{Site: st})
	defer srv.Close()
	get := func(p string) map[string]any {
		resp, err := http.Get(srv.URL + p)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("GET %s: %s", p, resp.Status)
		}
		var m map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&m)
		return m
	}
	rootDoc := get("/redfish/v1")
	if rootDoc["ThermalEquipment"].(map[string]any)["@odata.id"] != "/redfish/v1/ThermalEquipment" {
		t.Fatalf("root %v", rootDoc)
	}
	cdus := get("/redfish/v1/ThermalEquipment/CDUs")
	if cdus["Members@odata.count"].(float64) != 1 {
		t.Fatalf("CDUs %v", cdus)
	}
	cdu := get("/redfish/v1/ThermalEquipment/CDUs/cdu-1")
	if cdu["EquipmentType"] != "CDU" || cdu["@odata.type"] != "#CoolingUnit.v1_5_0.CoolingUnit" {
		t.Fatalf("cdu %v", cdu)
	}
	conn := get("/redfish/v1/ThermalEquipment/CDUs/cdu-1/SecondaryCoolantConnectors/1")
	if conn["FlowLitersPerMinute"].(map[string]any)["Reading"].(float64) <= 0 {
		t.Fatalf("connector %v", conn)
	}
	req, _ := http.NewRequest(http.MethodPatch, srv.URL+"/redfish/v1/ThermalEquipment/CDUs/cdu-1/SecondaryCoolantConnectors/1",
		strings.NewReader(`{"SupplyTemperatureControlCelsius":{"SetPoint":28}}`))
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("PATCH %v %v", err, resp.Status)
	}
	if v, _ := st.Point("cdu-1", "supply_setpoint_c"); v != 28 {
		t.Fatalf("setpoint %v", v)
	}
	ld := get("/redfish/v1/ThermalEquipment/CDUs/cdu-1/LeakDetection/LeakDetectors")
	if ld["Members@odata.count"].(float64) != 2 {
		t.Fatalf("leak detectors %v", ld)
	}
	_, _ = st.AddFault(site.FaultRequest{Type: "cdu-leak", Device: "cdu-1"})
	st.Tick(1)
	if d := get("/redfish/v1/ThermalEquipment/CDUs/cdu-1/LeakDetection/LeakDetectors/leak-cdu-1"); d["DetectorState"] != "Critical" {
		t.Fatalf("detector %v", d)
	}
	mains := get("/redfish/v1/PowerEquipment/RackPDUs/pdu-r1/Mains/AC1")
	if mains["BreakerState"] != "Normal" || mains["Voltage"].(map[string]any)["Reading"].(float64) < 225 {
		t.Fatalf("mains %v", mains)
	}
	_, _ = st.AddFault(site.FaultRequest{Type: "comms-loss", Device: "pdu-r1", Protocol: "redfish"})
	resp, _ = http.Get(srv.URL + "/redfish/v1/PowerEquipment/RackPDUs/pdu-r1")
	if resp.StatusCode != 503 {
		t.Fatalf("comms-loss status %d", resp.StatusCode)
	}
}

func TestAdminAPIAndMetrics(t *testing.T) {
	st, set := newSite(t, "au")
	reg := prometheus.NewRegistry()
	reqs := api.NewMetrics(reg, st)
	srv := httptest.NewServer((&api.Server{Site: st, Profiles: set, Requests: reqs}).Handler(reg))
	defer srv.Close()
	resp, err := http.Post(srv.URL+"/api/v1/faults", "application/json", strings.NewReader(`{"type":"chiller-trip","device":"chiller-1","durationSeconds":60}`))
	if err != nil || resp.StatusCode != 201 {
		t.Fatalf("add fault %v %v", err, resp.Status)
	}
	var f site.Fault
	_ = json.NewDecoder(resp.Body).Decode(&f)
	resp, _ = http.Post(srv.URL+"/api/v1/faults", "application/json", strings.NewReader(`{"type":"chiller-trip","device":"ups-1"}`))
	if resp.StatusCode != 400 {
		t.Fatalf("bad fault status %d", resp.StatusCode)
	}
	st.Tick(1)
	resp, _ = http.Get(srv.URL + "/metrics")
	b, _ := io.ReadAll(resp.Body)
	for _, want := range []string{`otsim_site{kpi="pue"`, `otsim_point{device="cdu-1"`, `otsim_alarm{alarm="tripped",device="chiller-1",severity="critical"}`, `otsim_fault_active{device="chiller-1"`} {
		if !strings.Contains(string(b), want) {
			t.Fatalf("metrics missing %s", want)
		}
	}
	req, _ := http.NewRequest(http.MethodDelete, srv.URL+"/api/v1/faults/"+f.ID, nil)
	resp, _ = http.DefaultClient.Do(req)
	if resp.StatusCode != 204 {
		t.Fatalf("delete %d", resp.StatusCode)
	}
	req, _ = http.NewRequest(http.MethodPut, srv.URL+"/api/v1/devices/crah-1/settings", strings.NewReader(`{"supply_air_setpoint_c": 24}`))
	resp, _ = http.DefaultClient.Do(req)
	if resp.StatusCode != 200 {
		t.Fatalf("settings %d", resp.StatusCode)
	}
}

func TestTelegrafConfig(t *testing.T) {
	st, set := newSite(t, "au")
	conf := telegraf.Generate(st, set, telegraf.Options{HostTemplate: "otsim-{id}", ModbusPort: 502, SNMPPort: 161, Community: "public", RedfishURL: "http://otsim:8443"})
	for _, want := range []string{
		`controller = "tcp://otsim-cdu-1:502"`,
		`{ name = "supply_temp_c", byte_order = "AB", data_type = "INT16", scale = 0.1, address = [0] },`,
		`{ name = "energy_wh", byte_order = "ABCD", data_type = "UINT32", scale = 1.0, address = [40116, 40117] },`,
		`agents = ["udp://otsim-ups-1:161"]`,
		`oid = "1.3.6.1.2.1.33.1.2.4.0"`,
		`urls = ["http://otsim:8443/redfish/v1/ThermalEquipment/CDUs/cdu-1/SecondaryCoolantConnectors/1"]`,
		`[[outputs.prometheus_client]]`,
	} {
		if !strings.Contains(conf, want) {
			t.Fatalf("telegraf config missing %s", want)
		}
	}
	if strings.Count(conf, "[[inputs.snmp]]") != 17 { // ups + 16 PDUs
		t.Fatalf("snmp inputs %d", strings.Count(conf, "[[inputs.snmp]]"))
	}
}
