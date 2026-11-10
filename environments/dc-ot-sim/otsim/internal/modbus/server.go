// Package modbus serves device points over Modbus TCP: one listener per device
// (so each device has its own endpoint, as real devices do) and a gateway
// listener that routes by unit ID (device index + 1), like a Modbus gateway.
package modbus

import (
	"encoding/binary"
	"fmt"
	"log"
	"math"
	"time"

	mb "github.com/simonvetter/modbus"

	"github.com/benmaguire/onit-ai-testenv/environments/dc-ot-sim/otsim/internal/profile"
	"github.com/benmaguire/onit-ai-testenv/environments/dc-ot-sim/otsim/internal/site"
)

// Requests counts protocol requests (device, protocol) for metrics.
var Requests = func(device, protocol string) {}

// CommsLossDelay is how long a "comms-loss" device stalls before answering
// with an exception (longer than typical client timeouts).
var CommsLossDelay = 15 * time.Second

type device struct {
	cfg site.DeviceConfig
	m   *profile.ModbusMap
}

// Handler implements mb.RequestHandler for one device or a gateway.
type Handler struct {
	site    *site.Site
	region  *site.Region
	devices map[uint8]*device // unit ID -> device (gateway); single entry keyed 0 for a device listener
	gateway bool
}

// Devices returns the modbus-enabled devices with their maps and unit IDs.
func Devices(st *site.Site, set *profile.Set) map[uint8]*device {
	out := map[uint8]*device{}
	for i, d := range st.Devices() {
		if name, ok := d.Protocols["modbus"]; ok {
			out[uint8(i+1)] = &device{cfg: d, m: set.Modbus[name]}
		}
	}
	return out
}

// Serve starts the gateway on gatewayURL and one listener per device at
// basePort+index. It returns the servers so callers can stop them.
func Serve(st *site.Site, set *profile.Set, gatewayURL, host string, basePort int) ([]*mb.ModbusServer, error) {
	devs := Devices(st, set)
	var servers []*mb.ModbusServer
	start := func(url string, h *Handler) error {
		srv, err := mb.NewServer(&mb.ServerConfiguration{URL: url, Timeout: 5 * time.Minute, MaxClients: 32,
			Logger: log.New(log.Writer(), "modbus: ", log.LstdFlags)}, h)
		if err != nil {
			return err
		}
		if err := srv.Start(); err != nil {
			return fmt.Errorf("modbus %s: %w", url, err)
		}
		servers = append(servers, srv)
		return nil
	}
	if gatewayURL != "" {
		if err := start(gatewayURL, &Handler{site: st, region: st.Region(), devices: devs, gateway: true}); err != nil {
			return nil, err
		}
	}
	if basePort > 0 {
		for uid, d := range devs {
			url := fmt.Sprintf("tcp://%s:%d", host, basePort+int(uid)-1)
			if err := start(url, &Handler{site: st, region: st.Region(), devices: map[uint8]*device{0: d}}); err != nil {
				return nil, err
			}
		}
	}
	return servers, nil
}

func (h *Handler) device(unit uint8) (*device, error) {
	var d *device
	if h.gateway {
		d = h.devices[unit]
		if d == nil {
			return nil, mb.ErrGWTargetFailedToRespond
		}
	} else {
		d = h.devices[0]
	}
	Requests(d.cfg.ID, "modbus")
	if h.site.CommsDown(d.cfg.ID, "modbus") {
		time.Sleep(CommsLossDelay)
		return nil, mb.ErrGWTargetFailedToRespond
	}
	return d, nil
}

func width(e profile.ModbusEntry) uint16 {
	switch e.Type {
	case "uint32", "int32", "float32":
		return 2
	case "string":
		return uint16(e.Len)
	}
	return 1
}

func (h *Handler) value(d *device, e profile.ModbusEntry, regionUnits bool) float64 {
	if e.Const != nil {
		return float64(*e.Const)
	}
	v, _ := h.site.Point(d.cfg.ID, e.Point)
	if regionUnits && e.Quantity != "" {
		v = profile.ToDisplay(h.region, e.Quantity, v)
	}
	if e.Scale != 0 {
		v *= e.Scale
	}
	return v
}

// encode returns the registers of one entry.
func (h *Handler) encode(d *device, e profile.ModbusEntry) []uint16 {
	if e.Type == "string" {
		s := e.String
		if e.Field != "" {
			s = profile.Identity(d.cfg, e.Field)
		}
		b := make([]byte, int(e.Len)*2)
		copy(b, s)
		regs := make([]uint16, e.Len)
		for i := range regs {
			regs[i] = binary.BigEndian.Uint16(b[i*2:])
		}
		return regs
	}
	v := math.Round(h.value(d, e, d.m.RegionUnits))
	switch e.Type {
	case "int16":
		return []uint16{uint16(int16(clamp(v, math.MinInt16, math.MaxInt16)))}
	case "uint32":
		u := uint32(clamp(v, 0, math.MaxUint32))
		return []uint16{uint16(u >> 16), uint16(u)}
	case "int32":
		u := uint32(int32(clamp(v, math.MinInt32, math.MaxInt32)))
		return []uint16{uint16(u >> 16), uint16(u)}
	case "float32":
		u := math.Float32bits(float32(h.value(d, e, d.m.RegionUnits)))
		return []uint16{uint16(u >> 16), uint16(u)}
	}
	return []uint16{uint16(clamp(v, 0, math.MaxUint16))}
}

func clamp(v, lo, hi float64) float64 { return math.Max(lo, math.Min(hi, v)) }

// registers builds the requested window of a register table.
func (h *Handler) registers(d *device, table []profile.ModbusEntry, addr, qty uint16) ([]uint16, error) {
	if len(table) == 0 {
		return nil, mb.ErrIllegalFunction
	}
	lo, hi := uint32(table[0].Addr), uint32(0)
	img := map[uint32]uint16{}
	for _, e := range table {
		if uint32(e.Addr) < lo {
			lo = uint32(e.Addr)
		}
		end := uint32(e.Addr) + uint32(width(e))
		if end > hi {
			hi = end
		}
		for i, r := range h.encode(d, e) {
			img[uint32(e.Addr)+uint32(i)] = r
		}
	}
	if uint32(addr) < lo || uint32(addr)+uint32(qty) > hi {
		return nil, mb.ErrIllegalDataAddress
	}
	out := make([]uint16, qty)
	for i := range out {
		out[i] = img[uint32(addr)+uint32(i)] // gaps inside a block read as 0
	}
	return out, nil
}

func (h *Handler) bits(d *device, table []profile.ModbusEntry, addr, qty uint16) ([]bool, error) {
	if len(table) == 0 {
		return nil, mb.ErrIllegalFunction
	}
	img := map[uint16]bool{}
	var hi uint32
	lo := uint32(table[0].Addr)
	for _, e := range table {
		v, _ := h.site.Point(d.cfg.ID, e.Point)
		img[e.Addr] = v != 0
		if uint32(e.Addr) < lo {
			lo = uint32(e.Addr)
		}
		if uint32(e.Addr)+1 > hi {
			hi = uint32(e.Addr) + 1
		}
	}
	if uint32(addr) < lo || uint32(addr)+uint32(qty) > hi {
		return nil, mb.ErrIllegalDataAddress
	}
	out := make([]bool, qty)
	for i := range out {
		out[i] = img[addr+uint16(i)]
	}
	return out, nil
}

// HandleCoils implements reads and writes of coils.
func (h *Handler) HandleCoils(req *mb.CoilsRequest) ([]bool, error) {
	d, err := h.device(req.UnitId)
	if err != nil {
		return nil, err
	}
	if !req.IsWrite {
		return h.bits(d, d.m.Coils, req.Addr, req.Quantity)
	}
	for i, v := range req.Args {
		e := find(d.m.Coils, req.Addr+uint16(i))
		if e == nil || !e.Writable {
			return nil, mb.ErrIllegalDataAddress
		}
		if err := h.site.SetPoint(d.cfg.ID, e.Point, map[bool]float64{true: 1, false: 0}[v]); err != nil {
			return nil, mb.ErrIllegalDataValue
		}
	}
	return nil, nil
}

// HandleDiscreteInputs implements reads of discrete inputs.
func (h *Handler) HandleDiscreteInputs(req *mb.DiscreteInputsRequest) ([]bool, error) {
	d, err := h.device(req.UnitId)
	if err != nil {
		return nil, err
	}
	return h.bits(d, d.m.Discrete, req.Addr, req.Quantity)
}

// HandleHoldingRegisters implements reads and writes of holding registers.
func (h *Handler) HandleHoldingRegisters(req *mb.HoldingRegistersRequest) ([]uint16, error) {
	d, err := h.device(req.UnitId)
	if err != nil {
		return nil, err
	}
	if !req.IsWrite {
		return h.registers(d, d.m.Holding, req.Addr, req.Quantity)
	}
	for i := 0; i < len(req.Args); {
		e := find(d.m.Holding, req.Addr+uint16(i))
		if e == nil || !e.Writable {
			return nil, mb.ErrIllegalDataAddress
		}
		w := int(width(*e))
		if i+w > len(req.Args) {
			return nil, mb.ErrIllegalDataAddress
		}
		var raw float64
		switch e.Type {
		case "int16":
			raw = float64(int16(req.Args[i]))
		case "uint32":
			raw = float64(uint32(req.Args[i])<<16 | uint32(req.Args[i+1]))
		case "int32":
			raw = float64(int32(uint32(req.Args[i])<<16 | uint32(req.Args[i+1])))
		case "float32":
			raw = float64(math.Float32frombits(uint32(req.Args[i])<<16 | uint32(req.Args[i+1])))
		default:
			raw = float64(req.Args[i])
		}
		v := raw
		if e.Scale != 0 {
			v /= e.Scale
		}
		if d.m.RegionUnits && e.Quantity != "" {
			v = profile.FromDisplay(h.region, e.Quantity, v)
		}
		if err := h.site.SetPoint(d.cfg.ID, e.Point, v); err != nil {
			return nil, mb.ErrIllegalDataValue
		}
		i += w
	}
	return nil, nil
}

// HandleInputRegisters implements reads of input registers.
func (h *Handler) HandleInputRegisters(req *mb.InputRegistersRequest) ([]uint16, error) {
	d, err := h.device(req.UnitId)
	if err != nil {
		return nil, err
	}
	return h.registers(d, d.m.Input, req.Addr, req.Quantity)
}

func find(table []profile.ModbusEntry, addr uint16) *profile.ModbusEntry {
	for i := range table {
		if table[i].Addr == addr {
			return &table[i]
		}
	}
	return nil
}
