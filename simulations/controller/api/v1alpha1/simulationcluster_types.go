package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// SecretKeyRef points at a key in a Secret.
type SecretKeyRef struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	// +kubebuilder:default=token
	// +optional
	Key string `json:"key,omitempty"`
}

// SimulationClusterSpec describes how the hub reaches a remote agent.
type SimulationClusterSpec struct {
	// Endpoint is the agent's API base URL, e.g. https://sim-agent.lab-2.example:8443.
	// +kubebuilder:validation:Pattern=`^https?://`
	Endpoint string `json:"endpoint"`
	// TokenSecretRef holds the bearer token the hub sends to the agent.
	// +optional
	TokenSecretRef *SecretKeyRef `json:"tokenSecretRef,omitempty"`
	// CABundle (PEM) to verify the agent's TLS certificate.
	// +optional
	CABundle string `json:"caBundle,omitempty"`
	// InsecureSkipTLSVerify disables TLS verification (test labs only).
	// +optional
	InsecureSkipTLSVerify bool `json:"insecureSkipTLSVerify,omitempty"`
	// Environment the remote cluster runs, e.g. nvidia-sim.
	// +optional
	Environment string `json:"environment,omitempty"`
	// +optional
	Description string `json:"description,omitempty"`
}

// SimulationClusterStatus is the hub's view of the agent.
type SimulationClusterStatus struct {
	// +optional
	Reachable bool `json:"reachable"`
	// +optional
	LastProbeTime *metav1.Time `json:"lastProbeTime,omitempty"`
	// +optional
	LastSeenTime *metav1.Time `json:"lastSeenTime,omitempty"`
	// +optional
	AgentVersion string `json:"agentVersion,omitempty"`
	// ActiveSimulations on the agent at the last probe.
	// +optional
	ActiveSimulations int32 `json:"activeSimulations,omitempty"`
	// +optional
	Message string `json:"message,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=simcluster;simclusters,categories=testenv
// +kubebuilder:printcolumn:name="Endpoint",type=string,JSONPath=`.spec.endpoint`
// +kubebuilder:printcolumn:name="Env",type=string,JSONPath=`.spec.environment`
// +kubebuilder:printcolumn:name="Reachable",type=boolean,JSONPath=`.status.reachable`
// +kubebuilder:printcolumn:name="Active",type=integer,JSONPath=`.status.activeSimulations`
// +kubebuilder:printcolumn:name="Last-Seen",type=date,JSONPath=`.status.lastSeenTime`

// SimulationCluster registers a remote simulation agent with a hub.
type SimulationCluster struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   SimulationClusterSpec   `json:"spec,omitempty"`
	Status SimulationClusterStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// SimulationClusterList is a list of SimulationClusters.
type SimulationClusterList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []SimulationCluster `json:"items"`
}

func init() {
	SchemeBuilder.Register(&SimulationCluster{}, &SimulationClusterList{})
}
