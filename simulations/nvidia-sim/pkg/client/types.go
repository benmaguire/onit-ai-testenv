package client

import (
	"time"

	simv1 "github.com/benmaguire/onit-ai-testenv/simulations/nvidia-sim/api/v1alpha1"
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
