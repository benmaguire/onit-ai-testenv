// Package snmp is a small SNMPv2c agent (GET, GETNEXT, GETBULK) serving
// device points through standard MIBs: MIB-II system, UPS-MIB (RFC 1628) and
// ENTITY-MIB / ENTITY-SENSOR-MIB (RFC 6933 / RFC 3433), as described by the
// device's SNMP profile. Read-only: SET returns notWritable.
package snmp

import (
	"fmt"
	"log"
	"math"
	"net"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gosnmp/gosnmp"

	"github.com/benmaguire/onit-ai-testenv/environments/dc-ot-sim/otsim/internal/profile"
	"github.com/benmaguire/onit-ai-testenv/environments/dc-ot-sim/otsim/internal/site"
)

// Requests counts protocol requests (device, protocol) for metrics.
var Requests = func(device, protocol string) {}

// EnterpriseOID is the sysObjectID prefix (an example/private arc; real
// devices report their vendor's enterprise number).
const EnterpriseOID = "1.3.6.1.4.1.99999.1"

type entry struct {
	oid []int
	get func() gosnmp.SnmpPDU
}

// Agent serves one device (or, on the shared port, the device named by the community).
type Agent struct {
	site      *site.Site
	tables    map[string][]entry // device -> sorted entries
	community string
	started   time.Time
}

// New builds OID tables for every device with an snmp profile.
func New(st *site.Site, set *profile.Set, community string) *Agent {
	a := &Agent{site: st, tables: map[string][]entry{}, community: community, started: time.Now()}
	for i, d := range st.Devices() {
		name, ok := d.Protocols["snmp"]
		if !ok {
			continue
		}
		a.tables[d.ID] = a.build(d, i, set.SNMP[name])
	}
	return a
}

// DeviceIDs lists SNMP-enabled devices.
func (a *Agent) DeviceIDs() []string {
	var ids []string
	for id := range a.tables {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func parseOID(s string) []int {
	var out []int
	for _, p := range strings.Split(strings.Trim(s, "."), ".") {
		n, _ := strconv.Atoi(p)
		out = append(out, n)
	}
	return out
}

func oidString(o []int) string {
	parts := make([]string, len(o))
	for i, n := range o {
		parts[i] = strconv.Itoa(n)
	}
	return "." + strings.Join(parts, ".")
}

func cmp(a, b []int) int {
	for i := 0; i < len(a) && i < len(b); i++ {
		if a[i] != b[i] {
			if a[i] < b[i] {
				return -1
			}
			return 1
		}
	}
	return len(a) - len(b)
}

func (a *Agent) build(d site.DeviceConfig, index int, m *profile.SNMPMap) []entry {
	id := d.ID
	var es []entry
	add := func(oid string, f func() gosnmp.SnmpPDU) {
		es = append(es, entry{oid: parseOID(oid), get: f})
	}
	str := func(oid, v string) {
		add(oid, func() gosnmp.SnmpPDU { return gosnmp.SnmpPDU{Type: gosnmp.OctetString, Value: []byte(v)} })
	}
	integer := func(oid string, f func() int) {
		add(oid, func() gosnmp.SnmpPDU { return gosnmp.SnmpPDU{Type: gosnmp.Integer, Value: f()} })
	}
	point := func(p string, scale float64) func() int {
		return func() int {
			v, _ := a.site.Point(id, p)
			if scale != 0 {
				v *= scale
			}
			return int(math.Round(v))
		}
	}
	// MIB-II system group.
	str("1.3.6.1.2.1.1.1.0", fmt.Sprintf("otsim simulated %s %s %s", d.Type, profile.Identity(d, "vendor"), profile.Identity(d, "model")))
	add("1.3.6.1.2.1.1.2.0", func() gosnmp.SnmpPDU {
		return gosnmp.SnmpPDU{Type: gosnmp.ObjectIdentifier, Value: fmt.Sprintf("%s.%d", EnterpriseOID, index+1)}
	})
	add("1.3.6.1.2.1.1.3.0", func() gosnmp.SnmpPDU {
		return gosnmp.SnmpPDU{Type: gosnmp.TimeTicks, Value: uint32(time.Since(a.started) / (10 * time.Millisecond))}
	})
	str("1.3.6.1.2.1.1.4.0", "noc@example.com")
	str("1.3.6.1.2.1.1.5.0", d.Name)
	str("1.3.6.1.2.1.1.6.0", d.Location)
	for _, o := range m.Objects {
		o := o
		switch {
		case o.Type == "string":
			v := profile.Identity(d, o.Field)
			str(o.OID, v)
		case o.Const != nil:
			c := int(*o.Const)
			integer(o.OID, func() int { return c })
		default:
			typ := map[string]gosnmp.Asn1BER{"gauge": gosnmp.Gauge32, "counter": gosnmp.Counter32, "timeticks": gosnmp.TimeTicks}[o.Type]
			get := point(o.Point, o.Scale)
			if typ == 0 {
				integer(o.OID, get)
			} else {
				add(o.OID, func() gosnmp.SnmpPDU { return gosnmp.SnmpPDU{Type: typ, Value: uint32(max(0, get()))} })
			}
		}
	}
	// ENTITY-MIB entPhysicalTable + ENTITY-SENSOR-MIB entPhySensorTable rows.
	for i, s := range m.Sensors {
		s, n := s, i+1
		str(fmt.Sprintf("1.3.6.1.2.1.47.1.1.1.1.2.%d", n), s.Name)                              // entPhysicalDescr
		integer(fmt.Sprintf("1.3.6.1.2.1.47.1.1.1.1.5.%d", n), func() int { return 8 })         // entPhysicalClass: sensor
		integer(fmt.Sprintf("1.3.6.1.2.1.99.1.1.1.1.%d", n), func() int { return s.Type })      // entPhySensorType
		integer(fmt.Sprintf("1.3.6.1.2.1.99.1.1.1.2.%d", n), func() int { return 9 })           // entPhySensorScale: units
		integer(fmt.Sprintf("1.3.6.1.2.1.99.1.1.1.3.%d", n), func() int { return s.Precision }) // entPhySensorPrecision
		if s.Type == 12 {                                                                       // truthvalue: 1 true, 2 false
			integer(fmt.Sprintf("1.3.6.1.2.1.99.1.1.1.4.%d", n), func() int {
				v, _ := a.site.Point(id, s.Point)
				if v != 0 {
					return 1
				}
				return 2
			})
		} else {
			integer(fmt.Sprintf("1.3.6.1.2.1.99.1.1.1.4.%d", n), point(s.Point, math.Pow(10, float64(s.Precision)))) // entPhySensorValue
		}
		integer(fmt.Sprintf("1.3.6.1.2.1.99.1.1.1.5.%d", n), func() int { return 1 }) // entPhySensorOperStatus: ok
		str(fmt.Sprintf("1.3.6.1.2.1.99.1.1.1.6.%d", n), s.UnitsDisplay)              // entPhySensorUnitsDisplay
	}
	sort.Slice(es, func(i, j int) bool { return cmp(es[i].oid, es[j].oid) < 0 })
	return es
}

// Serve listens on addr. With device != "" it serves that device for the
// configured community; with device == "" the community selects the device.
func (a *Agent) Serve(addr, device string) error {
	conn, err := net.ListenPacket("udp", addr)
	if err != nil {
		return err
	}
	go func() {
		buf := make([]byte, 65535)
		for {
			n, from, err := conn.ReadFrom(buf)
			if err != nil {
				log.Printf("snmp %s: %v", addr, err)
				return
			}
			resp := a.Handle(append([]byte(nil), buf[:n]...), device)
			if resp != nil {
				_, _ = conn.WriteTo(resp, from)
			}
		}
	}()
	return nil
}

// Handle answers one request packet (nil = no answer).
func (a *Agent) Handle(pkt []byte, device string) []byte {
	g := &gosnmp.GoSNMP{Version: gosnmp.Version2c, Logger: gosnmp.NewLogger(log.New(nilWriter{}, "", 0))}
	req, err := g.SnmpDecodePacket(pkt)
	if err != nil || req.Version == gosnmp.Version3 {
		return nil
	}
	dev := device
	if dev == "" {
		dev = req.Community
	} else if req.Community != a.community {
		return nil // wrong community: real agents stay silent
	}
	table, ok := a.tables[dev]
	if !ok {
		return nil
	}
	Requests(dev, "snmp")
	if a.site.CommsDown(dev, "snmp") {
		return nil // no response -> client timeout
	}
	resp := &gosnmp.SnmpPacket{Version: req.Version, Community: req.Community, PDUType: gosnmp.GetResponse, RequestID: req.RequestID}
	switch req.PDUType {
	case gosnmp.GetRequest:
		for _, v := range req.Variables {
			resp.Variables = append(resp.Variables, a.get(table, v.Name))
		}
	case gosnmp.GetNextRequest:
		for _, v := range req.Variables {
			resp.Variables = append(resp.Variables, a.next(table, v.Name))
		}
	case gosnmp.GetBulkRequest:
		nonRep := int(req.NonRepeaters)
		for i, v := range req.Variables {
			if i < nonRep {
				resp.Variables = append(resp.Variables, a.next(table, v.Name))
				continue
			}
			name := v.Name
			for r := 0; r < int(req.MaxRepetitions) && r < 200; r++ {
				pdu := a.next(table, name)
				resp.Variables = append(resp.Variables, pdu)
				if pdu.Type == gosnmp.EndOfMibView {
					break
				}
				name = pdu.Name
			}
		}
	case gosnmp.SetRequest:
		resp.Error, resp.ErrorIndex = gosnmp.NotWritable, 1
		resp.Variables = req.Variables
	default:
		return nil
	}
	out, err := resp.MarshalMsg()
	if err != nil {
		log.Printf("snmp marshal: %v", err)
		return nil
	}
	return out
}

func (a *Agent) get(table []entry, name string) gosnmp.SnmpPDU {
	o := parseOID(name)
	for _, e := range table {
		if cmp(e.oid, o) == 0 {
			p := e.get()
			p.Name = oidString(e.oid)
			return p
		}
	}
	return gosnmp.SnmpPDU{Name: name, Type: gosnmp.NoSuchObject}
}

func (a *Agent) next(table []entry, name string) gosnmp.SnmpPDU {
	o := parseOID(name)
	for _, e := range table {
		if cmp(e.oid, o) > 0 {
			p := e.get()
			p.Name = oidString(e.oid)
			return p
		}
	}
	return gosnmp.SnmpPDU{Name: name, Type: gosnmp.EndOfMibView}
}

type nilWriter struct{}

func (nilWriter) Write(p []byte) (int, error) { return len(p), nil }
