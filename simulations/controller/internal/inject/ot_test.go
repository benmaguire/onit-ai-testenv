package inject

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	simv1 "github.com/benmaguire/onit-ai-testenv/simulations/controller/api/v1alpha1"
)

// fakeOTSim implements the parts of otsim's admin API the injectors use.
type fakeOTSim struct {
	mu       sync.Mutex
	faults   map[string]otRequest
	critical int
	posted   []otRequest
}

func (f *fakeOTSim) handler() http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("GET /api/v1/status", func(w http.ResponseWriter, _ *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		var fs []map[string]string
		for id := range f.faults {
			fs = append(fs, map[string]string{"id": id})
		}
		var alarms []map[string]string
		for i := 0; i < f.critical; i++ {
			alarms = append(alarms, map[string]string{"device": "cdu-1", "name": "no_flow", "severity": "critical"})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"healthy": f.critical == 0,
			"kpi": map[string]float64{"alarms_critical": float64(f.critical)}, "faults": fs, "alarms": alarms})
	})
	m.HandleFunc("POST /api/v1/faults", func(w http.ResponseWriter, r *http.Request) {
		var req otRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.Type == "cdu-pump-failure" && req.Device != "cdu-1" {
			http.Error(w, `{"error":"fault cdu-pump-failure applies to cdu devices"}`, 400)
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		f.posted = append(f.posted, req)
		id := "f" + string(rune('0'+len(f.posted)))
		f.faults[id] = req
		f.critical++
		w.WriteHeader(201)
		_ = json.NewEncoder(w).Encode(map[string]string{"id": id, "type": req.Type})
	})
	m.HandleFunc("DELETE /api/v1/faults/{id}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if _, ok := f.faults[r.PathValue("id")]; !ok {
			http.Error(w, "no fault", 404)
			return
		}
		delete(f.faults, r.PathValue("id"))
		w.WriteHeader(204)
	})
	return m
}

func otInj(t *testing.T, typ string) Injector {
	t.Helper()
	i, ok := NewRegistry(All()...).Get(typ)
	if !ok {
		t.Fatalf("no type %s", typ)
	}
	return i
}

func TestOTInjectRecoverVerify(t *testing.T) {
	f := &fakeOTSim{faults: map[string]otRequest{}}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	env := &Env{OTSimURL: srv.URL}
	ctx := context.Background()

	inj := otInj(t, "ot-cdu-pump-failure")
	s := sim("ot-cdu-pump-failure", simv1.Target{}, map[string]string{"device": "cdu-1", "pump": "0"})
	if err := inj.Validate(s); err != nil {
		t.Fatal(err)
	}
	res, err := inj.Inject(ctx, env, s)
	if err != nil {
		t.Fatal(err)
	}
	if got := f.posted[0]; got.Type != "cdu-pump-failure" || got.Device != "cdu-1" || got.Params["pump"] != 0 {
		t.Fatalf("posted %+v", got)
	}
	if _, ok := f.posted[0].Params["pump"]; !ok {
		t.Fatal("explicit pump=0 not sent")
	}
	if res.Data["otFaultID"] != "f1" || res.Data["baselineCriticalAlarm"] != "0" || res.Targets[0].Kind != "OTFault" {
		t.Fatalf("result %+v", res)
	}
	s.Status.Data = res.Data
	if msg, err := inj.Recover(ctx, env, s); err != nil || !strings.Contains(msg, "removed") {
		t.Fatalf("recover %q %v", msg, err)
	}
	// Fault gone, but the plant still alarms: not recovered yet.
	if done, msg, err := inj.Verify(ctx, env, s); err != nil || done || !strings.Contains(msg, "cdu-1/no_flow") {
		t.Fatalf("verify while alarming: %v %q %v", done, msg, err)
	}
	f.mu.Lock()
	f.critical = 0
	f.mu.Unlock()
	if done, _, err := inj.Verify(ctx, env, s); err != nil || !done {
		t.Fatalf("verify after alarms cleared: %v %v", done, err)
	}
	// Recovering twice (fault already expired/cleared) is fine.
	if msg, err := inj.Recover(ctx, env, s); err != nil || !strings.Contains(msg, "already gone") {
		t.Fatalf("second recover %q %v", msg, err)
	}
}

func TestOTValidationAndErrors(t *testing.T) {
	f := &fakeOTSim{faults: map[string]otRequest{}}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	env := &Env{OTSimURL: srv.URL}

	for _, c := range []struct {
		typ    string
		params map[string]string
	}{
		{"ot-cdu-pump-failure", map[string]string{}},                                   // device required
		{"ot-cdu-pump-failure", map[string]string{"device": "cdu-1", "pump": "x"}},     // not a number
		{"ot-utility-outage", map[string]string{"device": "ups-1"}},                    // site-wide: no device param
		{"ot-comms-loss", map[string]string{"device": "ups-1", "protocol": "bacnet"}}, // bad protocol
		{"ot-sensor-stuck", map[string]string{"device": "cdu-1"}},                      // point required
	} {
		if err := otInj(t, c.typ).Validate(sim(c.typ, simv1.Target{}, c.params)); err == nil {
			t.Errorf("%s %v accepted", c.typ, c.params)
		}
	}
	// otsim's own validation errors surface from Inject.
	s := sim("ot-cdu-pump-failure", simv1.Target{}, map[string]string{"device": "ups-1"})
	if _, err := otInj(t, "ot-cdu-pump-failure").Inject(context.Background(), env, s); err == nil || !strings.Contains(err.Error(), "applies to cdu") {
		t.Fatalf("expected otsim error, got %v", err)
	}
	// Site-wide fault, comms-loss default protocol.
	s = sim("ot-comms-loss", simv1.Target{}, map[string]string{"device": "ups-1"})
	if _, err := otInj(t, "ot-comms-loss").Inject(context.Background(), env, s); err != nil || f.posted[len(f.posted)-1].Protocol != "all" {
		t.Fatalf("comms-loss: %v %+v", err, f.posted)
	}
}

func TestOTURL(t *testing.T) {
	s := sim("ot-high-ambient", simv1.Target{}, nil)
	p := newParams(otInjector{f: otFaults[0]}.Entry(), s)
	if u := otURL(&Env{}, s, p); u != "http://otsim.dc-ot-sim.svc:8080" {
		t.Fatalf("default %s", u)
	}
	s.Spec.Target.Namespace = "site-b"
	if u := otURL(&Env{}, s, p); u != "http://otsim.site-b.svc:8080" {
		t.Fatalf("namespace %s", u)
	}
	if u := otURL(&Env{OTSimURL: "http://x:1/"}, s, p); u != "http://x:1" {
		t.Fatalf("flag %s", u)
	}
}
