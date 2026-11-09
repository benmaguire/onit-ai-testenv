package observe

import (
	"math"
	"sort"
	"strconv"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	simv1 "github.com/benmaguire/onit-ai-testenv/simulations/controller/api/v1alpha1"
)

// Window is a named time range.
type Window struct {
	Name       string    `json:"name"`
	Start, End time.Time `json:"-"`
	StartStr   string    `json:"start"`
	EndStr     string    `json:"end"`
}

// Windows computes baseline / fault / recovery / after from status timestamps.
// Open windows end at now.
func Windows(sim *simv1.Simulation, now time.Time) []Window {
	st := sim.Status
	var out []Window
	add := func(name string, start, end time.Time) {
		if start.IsZero() || !end.After(start) {
			return
		}
		out = append(out, Window{Name: name, Start: start, End: end,
			StartStr: start.UTC().Format(time.RFC3339), EndStr: end.UTC().Format(time.RFC3339)})
	}
	var baselineStart time.Time
	if st.Observation != nil && st.Observation.BaselineStart != nil {
		baselineStart = st.Observation.BaselineStart.Time
	}
	injected := tm(st.InjectedAt)
	recStart := tm(st.RecoveryStartedAt)
	recovered := tm(st.RecoveredAt)
	afterEnd := time.Time{}
	if st.Observation != nil {
		afterEnd = tm(st.Observation.AfterEnd)
	}
	add("baseline", baselineStart, injected)
	faultEnd := recStart
	if faultEnd.IsZero() {
		faultEnd = recovered
	}
	if faultEnd.IsZero() {
		faultEnd = now
	}
	add("fault", injected, faultEnd)
	if !recStart.IsZero() {
		end := recovered
		if end.IsZero() {
			end = now
		}
		add("recovery", recStart, end)
	}
	if !recovered.IsZero() {
		end := afterEnd
		if end.IsZero() || end.After(now) {
			end = now
		}
		add("after", recovered, end)
	}
	return out
}

func tm(t *metav1.Time) time.Time {
	if t == nil {
		return time.Time{}
	}
	return t.Time
}

func fmtF(f float64) string {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return ""
	}
	return strconv.FormatFloat(f, 'g', 6, 64)
}

func quantile(sorted []float64, q float64) float64 {
	if len(sorted) == 0 {
		return math.NaN()
	}
	i := q * float64(len(sorted)-1)
	lo, hi := int(math.Floor(i)), int(math.Ceil(i))
	return sorted[lo] + (sorted[hi]-sorted[lo])*(i-float64(lo))
}

// StatsOf summarises values (NaN/Inf dropped).
func StatsOf(vals []float64) (simv1.Stats, float64) {
	var v []float64
	for _, x := range vals {
		if !math.IsNaN(x) && !math.IsInf(x, 0) {
			v = append(v, x)
		}
	}
	s := simv1.Stats{Samples: len(v)}
	if len(v) == 0 {
		return s, math.NaN()
	}
	last := v[len(v)-1]
	sorted := append([]float64(nil), v...)
	sort.Float64s(sorted)
	sum := 0.0
	for _, x := range v {
		sum += x
	}
	mean := sum / float64(len(v))
	s.Mean, s.Min, s.Max = fmtF(mean), fmtF(sorted[0]), fmtF(sorted[len(sorted)-1])
	s.P95, s.Last = fmtF(quantile(sorted, 0.95)), fmtF(last)
	return s, mean
}

// ChangePercent of b vs a.
func ChangePercent(a, b float64) string {
	if math.IsNaN(a) || math.IsNaN(b) {
		return ""
	}
	if a == 0 {
		if b == 0 {
			return "0"
		}
		return ""
	}
	return fmtF((b - a) / math.Abs(a) * 100)
}

// ProbeStatsOf summarises probe samples.
func ProbeStatsOf(samples []Sample) simv1.ProbeStats {
	ps := simv1.ProbeStats{Count: len(samples)}
	if len(samples) == 0 {
		return ps
	}
	var lat []float64
	run, longest := time.Duration(0), time.Duration(0)
	var runStart time.Time
	for i, s := range samples {
		if !s.OK {
			ps.Failures++
			if run == 0 {
				runStart = s.T
			}
			end := s.T
			if i+1 < len(samples) {
				end = samples[i+1].T
			}
			run = end.Sub(runStart)
			if run > longest {
				longest = run
			}
		} else {
			run = 0
			lat = append(lat, float64(s.Latency.Microseconds())/1000)
		}
	}
	ps.SuccessRate = fmtF(float64(ps.Count-ps.Failures) / float64(ps.Count))
	if longest > 0 {
		ps.LongestOutage = longest.Round(time.Millisecond).String()
	}
	if len(lat) > 0 {
		sort.Float64s(lat)
		sum := 0.0
		for _, x := range lat {
			sum += x
		}
		ps.LatencyMeanMs = fmtF(sum / float64(len(lat)))
		ps.LatencyP50Ms = fmtF(quantile(lat, 0.5))
		ps.LatencyP95Ms = fmtF(quantile(lat, 0.95))
		ps.LatencyP99Ms = fmtF(quantile(lat, 0.99))
		ps.LatencyMaxMs = fmtF(lat[len(lat)-1])
	}
	return ps
}

// InWindow filters samples to [w.Start, w.End).
func InWindow(samples []Sample, w Window) []Sample {
	var out []Sample
	for _, s := range samples {
		if !s.T.Before(w.Start) && s.T.Before(w.End) {
			out = append(out, s)
		}
	}
	return out
}
