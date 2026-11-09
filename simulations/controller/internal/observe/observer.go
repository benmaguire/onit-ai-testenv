package observe

import (
	"bytes"
	"context"
	"encoding/csv"
	"fmt"
	"io"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"text/template"
	"time"

	simv1 "github.com/benmaguire/onit-ai-testenv/simulations/controller/api/v1alpha1"
)

// Observer measures simulations. Zero values are usable (no Prometheus, no probes).
type Observer struct {
	Prom     *Prom
	Profiles Profiles
	Probes   *ProbeManager
	Now      func() time.Time
}

func (o *Observer) now() time.Time {
	if o.Now != nil {
		return o.Now()
	}
	return time.Now()
}

// HasLiveBaseline: probes need a real baseline period before injection.
func HasLiveBaseline(sim *simv1.Simulation) bool {
	ob := sim.Spec.Observe
	return ob != nil && ob.Baseline != nil && ob.Baseline.Duration > 0 && len(ob.Probes) > 0
}

// Vars for query templates.
type Vars struct {
	Namespace, Pods, Nodes, Simulation, Rate string
}

func regexOf(names []string) string {
	if len(names) == 0 {
		return ".+"
	}
	q := make([]string, len(names))
	for i, n := range names {
		q[i] = regexp.QuoteMeta(n)
	}
	return strings.Join(q, "|")
}

// VarsFor derives template variables from the simulation's recorded targets.
func VarsFor(sim *simv1.Simulation) Vars {
	ns := sim.Spec.Target.Namespace
	if d := sim.Status.Data["namespace"]; d != "" {
		ns = d
	}
	var pods, nodes []string
	for _, t := range sim.Status.Targets {
		switch t.Kind {
		case "Pod":
			pods = append(pods, t.Name)
			if ns == "" {
				ns = t.Namespace
			}
		case "Node":
			nodes = append(nodes, t.Name)
		case "GPU":
			nodes = append(nodes, t.Node)
		}
		if t.Node != "" && t.Kind == "Pod" {
			nodes = append(nodes, t.Node)
		}
	}
	if ns == "" {
		ns = sim.Namespace
	}
	sort.Strings(nodes)
	return Vars{Namespace: ns, Pods: regexOf(pods), Nodes: regexOf(dedupe(nodes)), Simulation: sim.Name, Rate: "1m"}
}

func dedupe(s []string) []string {
	var out []string
	for i, v := range s {
		if i == 0 || v != s[i-1] {
			out = append(out, v)
		}
	}
	return out
}

// Queries resolves profiles + inline queries and renders their templates.
func (o *Observer) Queries(sim *simv1.Simulation) ([]simv1.MetricQuery, []error) {
	ob := sim.Spec.Observe
	if ob == nil {
		return nil, nil
	}
	var qs []simv1.MetricQuery
	var errs []error
	for _, name := range ob.Profiles {
		p, ok := o.Profiles[name]
		if !ok {
			errs = append(errs, fmt.Errorf("unknown metrics profile %q (have %s)", name, strings.Join(o.Profiles.Names(), ", ")))
			continue
		}
		qs = append(qs, p...)
	}
	qs = append(qs, ob.Queries...)
	vars := VarsFor(sim)
	out := make([]simv1.MetricQuery, 0, len(qs))
	for _, q := range qs {
		t, err := template.New(q.Name).Option("missingkey=error").Parse(q.Query)
		if err != nil {
			errs = append(errs, fmt.Errorf("query %s: %w", q.Name, err))
			continue
		}
		var b bytes.Buffer
		if err := t.Execute(&b, vars); err != nil {
			errs = append(errs, fmt.Errorf("query %s: %w", q.Name, err))
			continue
		}
		q.Query = b.String()
		out = append(out, q)
	}
	return out, errs
}

// ValidateSpec checks an observe block before the simulation starts.
func (o *Observer) ValidateSpec(sim *simv1.Simulation) error {
	ob := sim.Spec.Observe
	if ob == nil {
		return nil
	}
	if _, errs := o.Queries(sim); len(errs) > 0 {
		return errs[0]
	}
	seen := map[string]bool{}
	for _, p := range ob.Probes {
		if p.Name == "" || seen[p.Name] {
			return fmt.Errorf("probe names must be unique and non-empty")
		}
		seen[p.Name] = true
		switch p.Type {
		case "http":
			if !strings.HasPrefix(p.URL, "http://") && !strings.HasPrefix(p.URL, "https://") {
				return fmt.Errorf("probe %s: http needs url", p.Name)
			}
		case "tcp":
			if !strings.Contains(p.Address, ":") {
				return fmt.Errorf("probe %s: tcp needs address host:port", p.Name)
			}
		case "exec":
			if p.Exec == nil || len(p.Exec.Command) == 0 || p.Exec.Namespace == "" || (p.Exec.Pod == "" && len(p.Exec.PodSelector) == 0) {
				return fmt.Errorf("probe %s: exec needs namespace, pod or podSelector, and command", p.Name)
			}
		default:
			return fmt.Errorf("probe %s: type must be http, tcp or exec", p.Name)
		}
	}
	return nil
}

func step(sim *simv1.Simulation, w Window) time.Duration {
	s := 15 * time.Second
	if ob := sim.Spec.Observe; ob != nil && ob.Step != nil && ob.Step.Duration > 0 {
		s = ob.Step.Duration
	}
	if half := w.End.Sub(w.Start) / 2; half < s {
		s = half
	}
	if s < time.Second {
		s = time.Second
	}
	return s
}

// Finalize measures every window and returns the observation.
func (o *Observer) Finalize(ctx context.Context, sim *simv1.Simulation) *simv1.ObservationStatus {
	obs := sim.Status.Observation.DeepCopy()
	if obs == nil {
		obs = &simv1.ObservationStatus{}
	}
	windows := Windows(sim, o.now())
	qs, errs := o.Queries(sim)
	obs.Metrics = nil
	for _, q := range qs {
		ms := simv1.MetricSummary{Name: q.Name, Query: q.Query, Unit: q.Unit, Windows: map[string]simv1.Stats{}}
		means := map[string]float64{}
		var qerrs []string
		for _, w := range windows {
			// End is exclusive so boundary samples are not counted twice.
			series, err := o.Prom.QueryRange(ctx, q.Query, w.Start, w.End.Add(-time.Millisecond), step(sim, w))
			if err != nil {
				qerrs = append(qerrs, w.Name+": "+err.Error())
				continue
			}
			var vals []float64
			for _, s := range series {
				for _, p := range s.Points {
					vals = append(vals, p.V)
				}
			}
			st, mean := StatsOf(vals)
			ms.Windows[w.Name] = st
			means[w.Name] = mean
		}
		if b, ok := means["baseline"]; ok {
			if f, ok := means["fault"]; ok {
				ms.FaultChangePercent = ChangePercent(b, f)
			}
			if a, ok := means["after"]; ok {
				ms.AfterChangePercent = ChangePercent(b, a)
			}
		}
		if len(qerrs) > 0 {
			ms.Error = strings.Join(dedupe(qerrs), "; ")
			if len(ms.Error) > 500 {
				ms.Error = ms.Error[:500]
			}
		}
		obs.Metrics = append(obs.Metrics, ms)
	}
	obs.Probes = nil
	if o.Probes != nil {
		samples := o.Probes.Samples(sim.Namespace, sim.Name)
		var names []string
		for n := range samples {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			ps := simv1.ProbeSummary{Name: n, Windows: map[string]simv1.ProbeStats{}}
			for _, w := range windows {
				ps.Windows[w.Name] = ProbeStatsOf(InWindow(samples[n], w))
			}
			obs.Probes = append(obs.Probes, ps)
		}
	}
	var msgs []string
	for _, e := range errs {
		msgs = append(msgs, e.Error())
	}
	obs.Message = strings.Join(msgs, "; ")
	return obs
}

// Export is the raw data behind a report.
type Export struct {
	Simulation string              `json:"simulation"`
	Namespace  string              `json:"namespace"`
	Windows    []Window            `json:"windows"`
	Metrics    []ExportedMetric    `json:"metrics"`
	Probes     map[string][]Sample `json:"probes,omitempty"`
}

// ExportedMetric is a query's raw series over the whole observation.
type ExportedMetric struct {
	Name   string   `json:"name"`
	Query  string   `json:"query"`
	Unit   string   `json:"unit,omitempty"`
	Series []Series `json:"series,omitempty"`
	Error  string   `json:"error,omitempty"`
}

// Export returns raw series from the baseline start to the after end (or now).
func (o *Observer) Export(ctx context.Context, sim *simv1.Simulation, stepOverride time.Duration) Export {
	windows := Windows(sim, o.now())
	ex := Export{Simulation: sim.Name, Namespace: sim.Namespace, Windows: windows}
	if len(windows) == 0 {
		return ex
	}
	all := Window{Start: windows[0].Start, End: windows[len(windows)-1].End}
	st := step(sim, all)
	if stepOverride > 0 {
		st = stepOverride
	}
	qs, _ := o.Queries(sim)
	for _, q := range qs {
		em := ExportedMetric{Name: q.Name, Query: q.Query, Unit: q.Unit}
		series, err := o.Prom.QueryRange(ctx, q.Query, all.Start, all.End, st)
		if err != nil {
			em.Error = err.Error()
		}
		em.Series = series
		ex.Metrics = append(ex.Metrics, em)
	}
	if o.Probes != nil {
		ex.Probes = o.Probes.Samples(sim.Namespace, sim.Name)
	}
	return ex
}

func windowOf(windows []Window, t time.Time) string {
	for _, w := range windows {
		if !t.Before(w.Start) && t.Before(w.End) {
			return w.Name
		}
	}
	return ""
}

// WriteCSV writes the export as rows:
// source,name,window,timestamp,value,labels (value is latency ms / ok for probes).
func (ex Export) WriteCSV(w io.Writer) error {
	cw := csv.NewWriter(w)
	_ = cw.Write([]string{"source", "name", "window", "timestamp", "value", "ok", "labels"})
	for _, m := range ex.Metrics {
		for _, s := range m.Series {
			var lbl []string
			for k, v := range s.Labels {
				lbl = append(lbl, k+"="+v)
			}
			sort.Strings(lbl)
			for _, p := range s.Points {
				if math.IsNaN(p.V) {
					continue
				}
				_ = cw.Write([]string{"metric", m.Name, windowOf(ex.Windows, p.T), p.T.UTC().Format(time.RFC3339Nano),
					strconv.FormatFloat(p.V, 'g', -1, 64), "", strings.Join(lbl, ";")})
			}
		}
	}
	var names []string
	for n := range ex.Probes {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		for _, s := range ex.Probes[n] {
			_ = cw.Write([]string{"probe", n, windowOf(ex.Windows, s.T), s.T.UTC().Format(time.RFC3339Nano),
				strconv.FormatFloat(float64(s.Latency.Microseconds())/1000, 'f', 3, 64), strconv.FormatBool(s.OK), s.Error})
		}
	}
	cw.Flush()
	return cw.Error()
}
