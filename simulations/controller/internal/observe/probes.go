package observe

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	simv1 "github.com/benmaguire/onit-ai-testenv/simulations/controller/api/v1alpha1"
	"github.com/benmaguire/onit-ai-testenv/simulations/controller/internal/kube"
)

// Sample is one probe result.
type Sample struct {
	T       time.Time     `json:"t"`
	OK      bool          `json:"ok"`
	Latency time.Duration `json:"latencyNs"`
	Error   string        `json:"error,omitempty"`
}

const maxSamples = 200_000

var (
	probeDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "sim_probe_duration_seconds",
		Help:    "Latency of simulation probes.",
		Buckets: prometheus.ExponentialBuckets(0.001, 2, 16),
	}, []string{"namespace", "simulation", "probe"})
	probeTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "sim_probe_total",
		Help: "Simulation probe results.",
	}, []string{"namespace", "simulation", "probe", "result"})
	simActive = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "sim_simulation_active",
		Help: "1 while a simulation's fault is injected (Active or Recovering).",
	}, []string{"namespace", "simulation", "type", "cluster"})
	simPhase = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "sim_simulation_phase",
		Help: "1 for the simulation's current phase.",
	}, []string{"namespace", "simulation", "type", "phase"})
)

// Collectors to register with the metrics registry.
func Collectors() []prometheus.Collector {
	return []prometheus.Collector{probeDuration, probeTotal, simActive, simPhase}
}

var phases = []simv1.Phase{simv1.PhasePending, simv1.PhaseWaiting, simv1.PhaseBaselining, simv1.PhaseActive,
	simv1.PhaseRecovering, simv1.PhaseRecovered, simv1.PhaseFailed}

// RecordPhase exports the simulation phase as metrics (Grafana annotations).
func RecordPhase(sim *simv1.Simulation, cluster string) {
	for _, p := range phases {
		v := 0.0
		if sim.Status.Phase == p {
			v = 1
		}
		simPhase.WithLabelValues(sim.Namespace, sim.Name, sim.Spec.Type, string(p)).Set(v)
	}
	active := 0.0
	if sim.Status.Phase == simv1.PhaseActive || sim.Status.Phase == simv1.PhaseRecovering {
		active = 1
	}
	simActive.WithLabelValues(sim.Namespace, sim.Name, sim.Spec.Type, cluster).Set(active)
}

// ForgetPhase removes a deleted simulation's series.
func ForgetPhase(namespace, name string) {
	simPhase.DeletePartialMatch(prometheus.Labels{"namespace": namespace, "simulation": name})
	simActive.DeletePartialMatch(prometheus.Labels{"namespace": namespace, "simulation": name})
	probeDuration.DeletePartialMatch(prometheus.Labels{"namespace": namespace, "simulation": name})
	probeTotal.DeletePartialMatch(prometheus.Labels{"namespace": namespace, "simulation": name})
}

type probeRun struct {
	cancel  context.CancelFunc
	mu      sync.Mutex
	samples map[string][]Sample
}

// ProbeManager runs simulation probes in the background. Samples are kept in
// memory (lost if the controller restarts; the Prometheus metrics persist).
type ProbeManager struct {
	Client client.Client
	Exec   kube.Executor
	Log    logr.Logger
	HTTP   *http.Client

	mu   sync.Mutex
	runs map[string]*probeRun
}

func key(ns, name string) string { return ns + "/" + name }

// Ensure starts the simulation's probes if they are not running.
func (m *ProbeManager) Ensure(sim *simv1.Simulation) {
	if sim.Spec.Observe == nil || len(sim.Spec.Observe.Probes) == 0 {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.runs == nil {
		m.runs = map[string]*probeRun{}
	}
	k := key(sim.Namespace, sim.Name)
	if _, ok := m.runs[k]; ok {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	run := &probeRun{cancel: cancel, samples: map[string][]Sample{}}
	m.runs[k] = run
	for _, p := range sim.Spec.Observe.Probes {
		go m.loop(ctx, run, sim.Namespace, sim.Name, p)
	}
}

// Stop ends the probes (samples are kept until Forget).
func (m *ProbeManager) Stop(ns, name string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if r, ok := m.runs[key(ns, name)]; ok {
		r.cancel()
	}
}

// Forget drops a simulation's samples.
func (m *ProbeManager) Forget(ns, name string) {
	m.Stop(ns, name)
	m.mu.Lock()
	delete(m.runs, key(ns, name))
	m.mu.Unlock()
}

// Samples returns a copy of the samples per probe.
func (m *ProbeManager) Samples(ns, name string) map[string][]Sample {
	m.mu.Lock()
	r, ok := m.runs[key(ns, name)]
	m.mu.Unlock()
	if !ok {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	out := map[string][]Sample{}
	for k, v := range r.samples {
		out[k] = append([]Sample(nil), v...)
	}
	return out
}

func (m *ProbeManager) loop(ctx context.Context, run *probeRun, ns, sim string, p simv1.Probe) {
	interval := 2 * time.Second
	if p.Interval != nil && p.Interval.Duration > 0 {
		interval = p.Interval.Duration
	}
	timeout := 5 * time.Second
	if p.Timeout != nil && p.Timeout.Duration > 0 {
		timeout = p.Timeout.Duration
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		pctx, cancel := context.WithTimeout(ctx, timeout)
		start := time.Now()
		err := m.probe(pctx, p)
		lat := time.Since(start)
		cancel()
		if ctx.Err() != nil {
			return
		}
		s := Sample{T: start, OK: err == nil, Latency: lat}
		result := "success"
		if err != nil {
			s.Error = err.Error()
			result = "failure"
		}
		probeDuration.WithLabelValues(ns, sim, p.Name).Observe(lat.Seconds())
		probeTotal.WithLabelValues(ns, sim, p.Name, result).Inc()
		run.mu.Lock()
		if len(run.samples[p.Name]) < maxSamples {
			run.samples[p.Name] = append(run.samples[p.Name], s)
		}
		run.mu.Unlock()
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (m *ProbeManager) probe(ctx context.Context, p simv1.Probe) error {
	switch p.Type {
	case "http":
		method := p.Method
		if method == "" {
			method = http.MethodGet
		}
		req, err := http.NewRequestWithContext(ctx, method, p.URL, bytes.NewBufferString(p.Body))
		if err != nil {
			return err
		}
		for k, v := range p.Headers {
			req.Header.Set(k, v)
		}
		if p.Body != "" && req.Header.Get("Content-Type") == "" {
			req.Header.Set("Content-Type", "application/json")
		}
		hc := m.HTTP
		if hc == nil {
			hc = http.DefaultClient
		}
		resp, err := hc.Do(req)
		if err != nil {
			return err
		}
		resp.Body.Close()
		if p.ExpectStatus != 0 && resp.StatusCode != p.ExpectStatus {
			return fmt.Errorf("status %d", resp.StatusCode)
		}
		if p.ExpectStatus == 0 && (resp.StatusCode < 200 || resp.StatusCode > 299) {
			return fmt.Errorf("status %d", resp.StatusCode)
		}
		return nil
	case "tcp":
		var d net.Dialer
		c, err := d.DialContext(ctx, "tcp", p.Address)
		if err != nil {
			return err
		}
		return c.Close()
	case "exec":
		if p.Exec == nil {
			return fmt.Errorf("exec probe without exec")
		}
		pod, container, err := m.execTarget(ctx, p.Exec)
		if err != nil {
			return err
		}
		_, stderr, err := m.Exec.Exec(ctx, p.Exec.Namespace, pod, container, p.Exec.Command)
		if err != nil {
			return fmt.Errorf("%v %s", err, strings.TrimSpace(stderr))
		}
		return nil
	}
	return fmt.Errorf("unknown probe type %q", p.Type)
}

func (m *ProbeManager) execTarget(ctx context.Context, e *simv1.ExecProbe) (string, string, error) {
	if e.Pod != "" {
		return e.Pod, e.Container, nil
	}
	var pl corev1.PodList
	if err := m.Client.List(ctx, &pl, client.InNamespace(e.Namespace), client.MatchingLabels(e.PodSelector)); err != nil {
		return "", "", err
	}
	sort.Slice(pl.Items, func(a, b int) bool { return pl.Items[a].Name < pl.Items[b].Name })
	for _, p := range pl.Items {
		if p.Status.Phase == corev1.PodRunning {
			return p.Name, e.Container, nil
		}
	}
	return "", "", fmt.Errorf("no running pod for exec probe in %s", e.Namespace)
}
