package observe

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	simv1 "github.com/benmaguire/onit-ai-testenv/simulations/controller/api/v1alpha1"
)

var t0 = time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)

func mt(d time.Duration) *metav1.Time { return &metav1.Time{Time: t0.Add(d)} }

func observedSim() *simv1.Simulation {
	return &simv1.Simulation{
		ObjectMeta: metav1.ObjectMeta{Name: "lat", Namespace: "sim-system"},
		Spec: simv1.SimulationSpec{Type: "net-latency", Observe: &simv1.ObserveSpec{
			Profiles: []string{"kubernetes"},
			Queries:  []simv1.MetricQuery{{Name: "p95", Query: `histogram_quantile(0.95, x{pod=~"{{.Pods}}"})`, Unit: "s"}},
		}},
		Status: simv1.SimulationStatus{
			InjectedAt: mt(2 * time.Minute), RecoveryStartedAt: mt(4 * time.Minute), RecoveredAt: mt(5 * time.Minute),
			Data:        map[string]string{"namespace": "app"},
			Targets:     []simv1.AffectedTarget{{Kind: "Pod", Namespace: "app", Name: "api-0", Node: "n1"}, {Kind: "Pod", Namespace: "app", Name: "api-1", Node: "n1"}},
			Observation: &simv1.ObservationStatus{BaselineStart: mt(0), AfterEnd: mt(7 * time.Minute)},
		},
	}
}

func TestWindows(t *testing.T) {
	ws := Windows(observedSim(), t0.Add(time.Hour))
	got := ""
	for _, w := range ws {
		got += fmt.Sprintf("%s:%s-%s ", w.Name, w.Start.Sub(t0), w.End.Sub(t0))
	}
	if got != "baseline:0s-2m0s fault:2m0s-4m0s recovery:4m0s-5m0s after:5m0s-7m0s " {
		t.Fatal(got)
	}
	// Open fault window ends now.
	s := observedSim()
	s.Status.RecoveryStartedAt, s.Status.RecoveredAt = nil, nil
	ws = Windows(s, t0.Add(3*time.Minute))
	if len(ws) != 2 || ws[1].End != t0.Add(3*time.Minute) {
		t.Fatalf("%+v", ws)
	}
}

func TestTemplatesUseTargets(t *testing.T) {
	o := &Observer{Profiles: Builtin}
	qs, errs := o.Queries(observedSim())
	if len(errs) > 0 {
		t.Fatal(errs)
	}
	last := qs[len(qs)-1].Query
	if last != `histogram_quantile(0.95, x{pod=~"api-0|api-1"})` {
		t.Fatal(last)
	}
	if !strings.Contains(qs[0].Query, `namespace="app"`) {
		t.Fatal(qs[0].Query)
	}
	bad := observedSim()
	bad.Spec.Observe.Profiles = []string{"nope"}
	if err := o.ValidateSpec(bad); err == nil {
		t.Fatal("unknown profile accepted")
	}
}

func TestStatsAndChange(t *testing.T) {
	s, mean := StatsOf([]float64{1, 2, 3, 4, 100})
	if s.Samples != 5 || s.Mean != "22" || s.Max != "100" || s.Last != "100" || mean != 22 {
		t.Fatalf("%+v", s)
	}
	if ChangePercent(10, 15) != "50" || ChangePercent(0, 5) != "" || ChangePercent(0, 0) != "0" {
		t.Fatal("change percent")
	}
}

func TestProbeStatsOutage(t *testing.T) {
	var ss []Sample
	for i := 0; i < 10; i++ {
		ok := i < 3 || i > 6 // 4 failures in a row
		ss = append(ss, Sample{T: t0.Add(time.Duration(i) * time.Second), OK: ok, Latency: time.Duration(10+i) * time.Millisecond})
	}
	ps := ProbeStatsOf(ss)
	if ps.Count != 10 || ps.Failures != 4 || ps.SuccessRate != "0.6" || ps.LongestOutage != "4s" {
		t.Fatalf("%+v", ps)
	}
}

// fakeProm answers every range query with values that depend on the time:
// 1 before t0+2m, 5 during the fault, 1 after.
func fakeProm(t *testing.T) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		var start, end float64
		fmt.Sscan(r.Form.Get("start"), &start)
		fmt.Sscan(r.Form.Get("end"), &end)
		var vals [][2]any
		for ts := start; ts <= end; ts += 15 {
			v := "1"
			at := time.Unix(int64(ts), 0)
			if !at.Before(t0.Add(2*time.Minute)) && at.Before(t0.Add(4*time.Minute)) {
				v = "5"
			}
			vals = append(vals, [2]any{ts, v})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "success", "data": map[string]any{
			"resultType": "matrix", "result": []any{map[string]any{"metric": map[string]string{"pod": "api-0"}, "values": vals}}}})
	}))
}

func TestFinalizeAndExport(t *testing.T) {
	srv := fakeProm(t)
	defer srv.Close()
	o := &Observer{Prom: &Prom{URL: srv.URL}, Profiles: Profiles{}, Now: func() time.Time { return t0.Add(time.Hour) }}
	s := observedSim()
	s.Spec.Observe.Profiles = nil
	obs := o.Finalize(context.Background(), s)
	if len(obs.Metrics) != 1 {
		t.Fatalf("%+v", obs)
	}
	m := obs.Metrics[0]
	if m.Windows["baseline"].Mean != "1" || m.Windows["fault"].Mean != "5" || m.Windows["after"].Mean != "1" {
		t.Fatalf("%+v", m.Windows)
	}
	if m.FaultChangePercent != "400" || m.AfterChangePercent != "0" {
		t.Fatalf("change %s %s", m.FaultChangePercent, m.AfterChangePercent)
	}
	var buf bytes.Buffer
	if err := o.Export(context.Background(), s, 0).WriteCSV(&buf); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(buf.String(), "source,name,window,timestamp,value,ok,labels\nmetric,p95,baseline,") || !strings.Contains(buf.String(), ",fault,") {
		t.Fatal(buf.String()[:300])
	}
}

func TestProbeManagerHTTP(t *testing.T) {
	n := 0
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n++
		if n%2 == 0 {
			w.WriteHeader(503)
		}
	}))
	defer target.Close()
	pm := &ProbeManager{}
	s := observedSim()
	s.Spec.Observe.Probes = []simv1.Probe{{Name: "api", Type: "http", URL: target.URL, Interval: &metav1.Duration{Duration: 20 * time.Millisecond}}}
	pm.Ensure(s)
	pm.Ensure(s) // idempotent
	time.Sleep(150 * time.Millisecond)
	pm.Stop(s.Namespace, s.Name)
	got := pm.Samples(s.Namespace, s.Name)["api"]
	ok, fail := 0, 0
	for _, x := range got {
		if x.OK {
			ok++
		} else {
			fail++
		}
	}
	if ok == 0 || fail == 0 {
		t.Fatalf("ok=%d fail=%d", ok, fail)
	}
}
