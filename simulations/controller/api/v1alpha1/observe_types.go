package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ObserveSpec asks the controller to measure the system around the fault:
// a baseline before injection, the fault window, the recovery window and an
// after window, from Prometheus queries and/or probes the controller runs.
type ObserveSpec struct {
	// Baseline window before injection. With probes the controller probes for
	// this long before injecting; Prometheus queries look back over it.
	// +optional
	Baseline *metav1.Duration `json:"baseline,omitempty"`
	// After window following verified recovery.
	// +optional
	After *metav1.Duration `json:"after,omitempty"`
	// Profiles are named query sets from the controller's metrics profiles
	// (e.g. kubernetes, gpu, dynamo, triton, kai, network, probes).
	// +optional
	Profiles []string `json:"profiles,omitempty"`
	// Queries are extra PromQL queries. They are Go templates with
	// {{.Namespace}}, {{.Pods}} (regex of target pods), {{.Nodes}},
	// {{.Simulation}}, {{.Rate}} (rate window).
	// +optional
	Queries []MetricQuery `json:"queries,omitempty"`
	// Probes the controller runs from the baseline start to the after end.
	// +optional
	Probes []Probe `json:"probes,omitempty"`
	// Step of Prometheus range queries (default 15s).
	// +optional
	Step *metav1.Duration `json:"step,omitempty"`
}

// MetricQuery is one PromQL expression; aggregate it to a single series.
type MetricQuery struct {
	Name  string `json:"name"`
	Query string `json:"query"`
	// +optional
	Unit string `json:"unit,omitempty"`
	// HigherIsBetter flips the sign convention for regressions in reports.
	// +optional
	HigherIsBetter bool `json:"higherIsBetter,omitempty"`
}

// Probe is a synthetic check run on an interval.
type Probe struct {
	Name string `json:"name"`
	// +kubebuilder:validation:Enum=http;tcp;exec
	Type string `json:"type"`
	// http: URL; tcp: host:port.
	// +optional
	URL string `json:"url,omitempty"`
	// +optional
	Address string `json:"address,omitempty"`
	// http method (default GET).
	// +optional
	Method string `json:"method,omitempty"`
	// +optional
	Body string `json:"body,omitempty"`
	// +optional
	Headers map[string]string `json:"headers,omitempty"`
	// Expected HTTP status (default any 2xx).
	// +optional
	ExpectStatus int `json:"expectStatus,omitempty"`
	// exec: command run in a pod (success = exit 0).
	// +optional
	Exec *ExecProbe `json:"exec,omitempty"`
	// Interval between probes (default 2s).
	// +optional
	Interval *metav1.Duration `json:"interval,omitempty"`
	// Timeout per probe (default 5s).
	// +optional
	Timeout *metav1.Duration `json:"timeout,omitempty"`
}

// ExecProbe runs a command in the first running pod matching the selector.
type ExecProbe struct {
	Namespace string `json:"namespace"`
	// +optional
	Pod string `json:"pod,omitempty"`
	// +optional
	PodSelector map[string]string `json:"podSelector,omitempty"`
	// +optional
	Container string   `json:"container,omitempty"`
	Command   []string `json:"command"`
}

// Stats summarises a metric over one window.
type Stats struct {
	Samples int `json:"samples"`
	// Values are decimal strings (CRDs avoid floats).
	// +optional
	Mean string `json:"mean,omitempty"`
	// +optional
	Min string `json:"min,omitempty"`
	// +optional
	Max string `json:"max,omitempty"`
	// +optional
	P95 string `json:"p95,omitempty"`
	// +optional
	Last string `json:"last,omitempty"`
}

// ProbeStats summarises a probe over one window.
type ProbeStats struct {
	Count    int `json:"count"`
	Failures int `json:"failures"`
	// +optional
	SuccessRate string `json:"successRate,omitempty"`
	// +optional
	LatencyMeanMs string `json:"latencyMeanMs,omitempty"`
	// +optional
	LatencyP50Ms string `json:"latencyP50Ms,omitempty"`
	// +optional
	LatencyP95Ms string `json:"latencyP95Ms,omitempty"`
	// +optional
	LatencyP99Ms string `json:"latencyP99Ms,omitempty"`
	// +optional
	LatencyMaxMs string `json:"latencyMaxMs,omitempty"`
	// LongestOutage is the longest run of consecutive failures.
	// +optional
	LongestOutage string `json:"longestOutage,omitempty"`
}

// MetricSummary is a query's stats per window.
type MetricSummary struct {
	Name string `json:"name"`
	// +optional
	Query string `json:"query,omitempty"`
	// +optional
	Unit string `json:"unit,omitempty"`
	// Windows: baseline, fault, recovery, after.
	// +optional
	Windows map[string]Stats `json:"windows,omitempty"`
	// FaultChangePercent: fault-window mean vs baseline mean.
	// +optional
	FaultChangePercent string `json:"faultChangePercent,omitempty"`
	// AfterChangePercent: after-window mean vs baseline mean (did it return to normal?).
	// +optional
	AfterChangePercent string `json:"afterChangePercent,omitempty"`
	// +optional
	Error string `json:"error,omitempty"`
}

// ProbeSummary is a probe's stats per window.
type ProbeSummary struct {
	Name string `json:"name"`
	// +optional
	Windows map[string]ProbeStats `json:"windows,omitempty"`
}

// ObservationStatus is the measured result.
type ObservationStatus struct {
	// Window boundaries.
	// +optional
	BaselineStart *metav1.Time `json:"baselineStart,omitempty"`
	// +optional
	AfterEnd *metav1.Time `json:"afterEnd,omitempty"`
	// Complete once every window was measured.
	// +optional
	Complete bool `json:"complete,omitempty"`
	// +optional
	Message string `json:"message,omitempty"`
	// +optional
	Metrics []MetricSummary `json:"metrics,omitempty"`
	// +optional
	Probes []ProbeSummary `json:"probes,omitempty"`
}
