package client

import (
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	simv1 "github.com/benmaguire/onit-ai-testenv/simulations/controller/api/v1alpha1"
)

// CreateRequest is the body of POST /api/v1/simulations.
type CreateRequest struct {
	// Name of the Simulation; empty generates "<type>-xxxxx".
	Name string `json:"name,omitempty"`
	// Namespace for the Simulation; empty uses the server default.
	Namespace string `json:"namespace,omitempty"`
	// Labels, e.g. {"testrun": "nightly-42"} to group a test run's simulations.
	Labels map[string]string    `json:"labels,omitempty"`
	Spec   simv1.SimulationSpec `json:"spec"`
}

// VersionInfo is returned by GET /version.
type VersionInfo struct {
	Name              string `json:"name"`
	Version           string `json:"version"`
	Mode              string `json:"mode"` // agent | hub
	Cluster           string `json:"cluster"`
	Environment       string `json:"environment,omitempty"`
	ActiveSimulations int    `json:"activeSimulations"`
}

// ClusterInfo is one entry of GET /api/v1/clusters (hub only).
type ClusterInfo struct {
	Name              string     `json:"name"`
	Endpoint          string     `json:"endpoint"`
	Environment       string     `json:"environment,omitempty"`
	Local             bool       `json:"local,omitempty"`
	Reachable         bool       `json:"reachable"`
	AgentVersion      string     `json:"agentVersion,omitempty"`
	ActiveSimulations int32      `json:"activeSimulations"`
	LastSeen          *time.Time `json:"lastSeen,omitempty"`
	Message           string     `json:"message,omitempty"`
}

// RegisterRequest is sent by an agent to POST /api/v1/clusters/register.
type RegisterRequest struct {
	Name                  string `json:"name"`
	Endpoint              string `json:"endpoint"`
	Token                 string `json:"token,omitempty"`
	Environment           string `json:"environment,omitempty"`
	CABundle              string `json:"caBundle,omitempty"`
	InsecureSkipTLSVerify bool   `json:"insecureSkipTLSVerify,omitempty"`
}

// RecoverAllResponse lists the Simulations asked to recover.
type RecoverAllResponse struct {
	Recovered []string `json:"recovered"`
}

// FleetResult is one cluster's part of a fleet-wide call (hub only).
type FleetResult[T any] struct {
	Cluster string `json:"cluster"`
	Result  T      `json:"result,omitempty"`
	Error   string `json:"error,omitempty"`
}

// ErrorResponse is the body of every non-2xx response.
type ErrorResponse struct {
	Error string `json:"error"`
}

// ReportWindow is one measured window.
type ReportWindow struct {
	Name  string `json:"name"`
	Start string `json:"start"`
	End   string `json:"end"`
}

// Report is returned by GET /api/v1/simulations/{ns}/{name}/report.
type Report struct {
	Cluster           string                   `json:"cluster"`
	Namespace         string                   `json:"namespace"`
	Name              string                   `json:"name"`
	Type              string                   `json:"type"`
	Phase             simv1.Phase              `json:"phase"`
	Message           string                   `json:"message,omitempty"`
	InjectedAt        *metav1.Time             `json:"injectedAt,omitempty"`
	RecoveryStartedAt *metav1.Time             `json:"recoveryStartedAt,omitempty"`
	RecoveredAt       *metav1.Time             `json:"recoveredAt,omitempty"`
	Targets           []simv1.AffectedTarget   `json:"targets,omitempty"`
	Data              map[string]string        `json:"data,omitempty"`
	Windows           []ReportWindow           `json:"windows,omitempty"`
	Observation       *simv1.ObservationStatus `json:"observation,omitempty"`
	// Live: measurements computed on request (observation not complete yet).
	Live bool `json:"live,omitempty"`
}
