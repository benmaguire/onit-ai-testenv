// Package catalog holds the descriptive types for the simulation catalog,
// shared by the server (which publishes it) and API clients.
package catalog

// Param documents one simulation parameter (spec.params key).
type Param struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Default     string `json:"default,omitempty"`
	Required    bool   `json:"required,omitempty"`
}

// Entry describes a simulation type.
type Entry struct {
	// Type is the value for spec.type.
	Type string `json:"type"`
	// Category groups entries: gpu, dynamo, kubernetes, network.
	Category string `json:"category"`
	// Summary is a one-line description.
	Summary string `json:"summary"`
	// Effect explains what the fault does and what a consumer observes.
	Effect string `json:"effect"`
	// Recovery explains how the fault is reverted and what "recovered" means.
	Recovery string `json:"recovery"`
	// Targets lists the spec.target fields the type uses.
	Targets []string `json:"targets"`
	// Params the type accepts.
	Params []Param `json:"params,omitempty"`
	// Requires lists environment prerequisites.
	Requires []string `json:"requires,omitempty"`
	// References are upstream documentation links.
	References []string `json:"references,omitempty"`
}
