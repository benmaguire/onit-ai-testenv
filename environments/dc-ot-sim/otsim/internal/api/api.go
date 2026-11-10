// Package api is otsim's admin API (status, points, setpoints, faults) and
// its Prometheus metrics.
package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/benmaguire/onit-ai-testenv/environments/dc-ot-sim/otsim/internal/profile"
	"github.com/benmaguire/onit-ai-testenv/environments/dc-ot-sim/otsim/internal/site"
)

// Server is the admin API.
type Server struct {
	Site     *site.Site
	Profiles *profile.Set
	Requests *prometheus.CounterVec
}

// NewMetrics registers otsim collectors and returns the request counter.
func NewMetrics(reg *prometheus.Registry, st *site.Site) *prometheus.CounterVec {
	reqs := prometheus.NewCounterVec(prometheus.CounterOpts{Name: "otsim_protocol_requests_total",
		Help: "Protocol requests answered (or dropped) per device."}, []string{"device", "protocol"})
	reg.MustRegister(reqs, &collector{site: st})
	return reqs
}

type collector struct{ site *site.Site }

var (
	pointDesc = prometheus.NewDesc("otsim_point", "Device point value (SI units).", []string{"device", "type", "point"}, nil)
	kpiDesc   = prometheus.NewDesc("otsim_site", "Site KPI (pue, it_kw, facility_kw, wue_l_per_kwh, rack_inlet_c, ...).", []string{"site", "region", "kpi"}, nil)
	alarmDesc = prometheus.NewDesc("otsim_alarm", "Active alarm (1).", []string{"device", "alarm", "severity"}, nil)
	faultDesc = prometheus.NewDesc("otsim_fault_active", "Injected fault (1).", []string{"id", "type", "device"}, nil)
)

func (c *collector) Describe(ch chan<- *prometheus.Desc) {
	ch <- pointDesc
	ch <- kpiDesc
	ch <- alarmDesc
	ch <- faultDesc
}

func (c *collector) Collect(ch chan<- prometheus.Metric) {
	snap := c.site.Snapshot()
	for dev, pts := range snap.Devices {
		for p, v := range pts {
			ch <- prometheus.MustNewConstMetric(pointDesc, prometheus.GaugeValue, v, dev, snap.Types[dev], p)
		}
	}
	for k, v := range snap.KPI {
		ch <- prometheus.MustNewConstMetric(kpiDesc, prometheus.GaugeValue, v, snap.Site, snap.Region, k)
	}
	for _, a := range snap.Alarms {
		ch <- prometheus.MustNewConstMetric(alarmDesc, prometheus.GaugeValue, 1, a.Device, a.Name, a.Severity)
	}
	for _, f := range snap.Faults {
		ch <- prometheus.MustNewConstMetric(faultDesc, prometheus.GaugeValue, 1, f.ID, f.Type, f.Device)
	}
}

// Handler builds the admin routes; metrics on /metrics from reg.
func (s *Server) Handler(reg *prometheus.Registry) http.Handler {
	m := http.NewServeMux()
	m.Handle("GET /metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
	m.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	m.HandleFunc("GET /api/v1/status", s.status)
	m.HandleFunc("GET /api/v1/region", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, 200, s.Site.Region()) })
	m.HandleFunc("GET /api/v1/regions", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, 200, profile.Names(s.Profiles.Regions)) })
	m.HandleFunc("GET /api/v1/devices", s.devices)
	m.HandleFunc("GET /api/v1/devices/{id}", s.device)
	m.HandleFunc("PUT /api/v1/devices/{id}/settings", s.settings)
	m.HandleFunc("GET /api/v1/fault-types", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, 200, site.FaultTypes) })
	m.HandleFunc("GET /api/v1/faults", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, 200, s.Site.Snapshot().Faults) })
	m.HandleFunc("POST /api/v1/faults", s.addFault)
	m.HandleFunc("DELETE /api/v1/faults/{id}", s.removeFault)
	m.HandleFunc("DELETE /api/v1/faults", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, 200, map[string]int{"cleared": s.Site.ClearFaults()})
	})
	return m
}

func (s *Server) status(w http.ResponseWriter, _ *http.Request) {
	snap := s.Site.Snapshot()
	writeJSON(w, 200, map[string]any{"site": snap.Site, "region": snap.Region, "simTime": snap.SimTime,
		"kpi": snap.KPI, "alarms": snap.Alarms, "faults": snap.Faults,
		"healthy": snap.KPI["alarms_critical"] == 0})
}

func (s *Server) devices(w http.ResponseWriter, _ *http.Request) {
	snap := s.Site.Snapshot()
	type row struct {
		ID        string            `json:"id"`
		Type      string            `json:"type"`
		Name      string            `json:"name"`
		Index     int               `json:"index"`
		Protocols map[string]string `json:"protocols,omitempty"`
		Points    int               `json:"points"`
	}
	var out []row
	for i, d := range s.Site.Devices() {
		out = append(out, row{d.ID, d.Type, d.Name, i, d.Protocols, len(snap.Devices[d.ID])})
	}
	writeJSON(w, 200, out)
}

func (s *Server) device(w http.ResponseWriter, r *http.Request) {
	snap := s.Site.Snapshot()
	id := r.PathValue("id")
	pts, ok := snap.Devices[id]
	if !ok {
		writeErr(w, 404, errors.New("unknown device "+id))
		return
	}
	var alarms []site.Alarm
	for _, a := range snap.Alarms {
		if a.Device == id {
			alarms = append(alarms, a)
		}
	}
	keys := make([]string, 0, len(pts))
	for k := range pts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	writeJSON(w, 200, map[string]any{"id": id, "type": snap.Types[id], "points": pts, "alarms": alarms})
}

func (s *Server) settings(w http.ResponseWriter, r *http.Request) {
	var body map[string]float64
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body); err != nil {
		writeErr(w, 400, err)
		return
	}
	var errs []string
	for k, v := range body {
		if err := s.Site.SetPoint(r.PathValue("id"), k, v); err != nil {
			errs = append(errs, err.Error())
		}
	}
	if len(errs) > 0 {
		writeErr(w, 400, errors.New(strings.Join(errs, "; ")))
		return
	}
	s.device(w, r)
}

func (s *Server) addFault(w http.ResponseWriter, r *http.Request) {
	var req site.FaultRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeErr(w, 400, err)
		return
	}
	f, err := s.Site.AddFault(req)
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	writeJSON(w, 201, f)
}

func (s *Server) removeFault(w http.ResponseWriter, r *http.Request) {
	if !s.Site.RemoveFault(r.PathValue("id")) {
		writeErr(w, 404, errors.New("no fault "+r.PathValue("id")))
		return
	}
	w.WriteHeader(204)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]string{"error": err.Error()})
}
