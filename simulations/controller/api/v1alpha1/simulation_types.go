package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Phase is the lifecycle phase of a Simulation.
// +kubebuilder:validation:Enum=Pending;Waiting;Baselining;Active;Recovering;Recovered;Failed
type Phase string

const (
	// PhasePending: accepted, not yet processed.
	PhasePending Phase = "Pending"
	// PhaseWaiting: waiting for spec.delay to elapse before injecting.
	PhaseWaiting Phase = "Waiting"
	// PhaseBaselining: probes measure the healthy system before injection.
	PhaseBaselining Phase = "Baselining"
	// PhaseActive: the fault is injected.
	PhaseActive Phase = "Active"
	// PhaseRecovering: recovery has been applied and is being verified.
	PhaseRecovering Phase = "Recovering"
	// PhaseRecovered: the fault was reverted and the target verified healthy.
	PhaseRecovered Phase = "Recovered"
	// PhaseFailed: injection or recovery failed; see status.message.
	PhaseFailed Phase = "Failed"
)

// Finished reports whether the phase is terminal.
func (p Phase) Finished() bool { return p == PhaseRecovered || p == PhaseFailed }

// Target selects what a simulation acts on. Which fields matter depends on the
// simulation type; see the catalog (GET /api/v1/catalog or CATALOG.md).
type Target struct {
	// Node name. GPU and node simulations act on this node.
	// +optional
	Node string `json:"node,omitempty"`
	// NodeSelector selects nodes by label when Node is empty.
	// +optional
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`

	// Namespace of the target pods. Defaults to the Simulation's namespace.
	// +optional
	Namespace string `json:"namespace,omitempty"`
	// PodSelector selects target pods by label. For GPU simulations, the GPUs
	// allocated to the selected pods are targeted instead of Target.GPU.
	// +optional
	PodSelector map[string]string `json:"podSelector,omitempty"`

	// Graph is a DynamoGraphDeployment name; with Component it builds the pod
	// selector for Dynamo pods.
	// +optional
	Graph string `json:"graph,omitempty"`
	// Component is a Dynamo component type: frontend, decode, prefill, worker.
	// +optional
	Component string `json:"component,omitempty"`

	// GPU on the node: an index, "all", or a UUID. Default "all".
	// +optional
	GPU string `json:"gpu,omitempty"`

	// Count limits how many matched pods/nodes are affected. 0 means all.
	// +optional
	// +kubebuilder:validation:Minimum=0
	Count int32 `json:"count,omitempty"`
}

// SimulationSpec describes a fault to inject and how it recovers.
type SimulationSpec struct {
	// Type of simulation, e.g. gpu-xid, gpu-overheat, dynamo-worker-kill.
	// +kubebuilder:validation:MinLength=1
	Type string `json:"type"`

	// Target of the simulation.
	// +optional
	Target Target `json:"target,omitempty"`

	// Params are type-specific parameters (all strings), e.g. {"xid": "79"}.
	// +optional
	Params map[string]string `json:"params,omitempty"`

	// Delay before injecting.
	// +optional
	Delay *metav1.Duration `json:"delay,omitempty"`

	// Duration the fault stays injected before automatic recovery. Empty means
	// it stays until spec.recover is set to true or the Simulation is deleted.
	// +optional
	Duration *metav1.Duration `json:"duration,omitempty"`

	// Recover requests recovery now. Set by `simctl recover` / the API.
	// +optional
	Recover bool `json:"recover,omitempty"`

	// RecoveryTimeout bounds how long recovery verification may take
	// (e.g. waiting for killed pods to be Ready again). Default 5m.
	// +optional
	RecoveryTimeout *metav1.Duration `json:"recoveryTimeout,omitempty"`

	// Observe measures the system before, during and after the fault.
	// +optional
	Observe *ObserveSpec `json:"observe,omitempty"`

	// TTLAfterFinished deletes the Simulation this long after it Recovered or
	// Failed (and its observation completed). Empty keeps it.
	// +optional
	TTLAfterFinished *metav1.Duration `json:"ttlAfterFinished,omitempty"`
}

// AffectedTarget is something a simulation acted on, recorded so recovery can
// undo exactly what injection did.
type AffectedTarget struct {
	// Kind of target: Node, Pod, GPU, NetworkPolicy.
	Kind string `json:"kind"`
	// +optional
	Namespace string `json:"namespace,omitempty"`
	// +optional
	Name string `json:"name,omitempty"`
	// +optional
	Node string `json:"node,omitempty"`
	// GPU index on Node, for GPU targets.
	// +optional
	GPU string `json:"gpu,omitempty"`
	// +optional
	Detail string `json:"detail,omitempty"`
}

// SimulationStatus is the observed state of a Simulation.
type SimulationStatus struct {
	// +optional
	Phase Phase `json:"phase,omitempty"`
	// +optional
	Message string `json:"message,omitempty"`
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// +optional
	InjectedAt *metav1.Time `json:"injectedAt,omitempty"`
	// RecoverAt is when automatic recovery is due (InjectedAt + Duration).
	// +optional
	RecoverAt *metav1.Time `json:"recoverAt,omitempty"`
	// +optional
	RecoveryStartedAt *metav1.Time `json:"recoveryStartedAt,omitempty"`
	// +optional
	RecoveredAt *metav1.Time `json:"recoveredAt,omitempty"`
	// Targets the simulation acted on.
	// +optional
	Targets []AffectedTarget `json:"targets,omitempty"`
	// Data is injector state needed for recovery (e.g. original values).
	// +optional
	Data map[string]string `json:"data,omitempty"`
	// Observation holds baseline / fault / recovery / after measurements.
	// +optional
	Observation *ObservationStatus `json:"observation,omitempty"`
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=sim;sims,categories=testenv
// +kubebuilder:printcolumn:name="Type",type=string,JSONPath=`.spec.type`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Injected",type=date,JSONPath=`.status.injectedAt`
// +kubebuilder:printcolumn:name="Recover-At",type=date,JSONPath=`.status.recoverAt`,priority=1
// +kubebuilder:printcolumn:name="Message",type=string,JSONPath=`.status.message`,priority=1
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// Simulation injects one failure into the environment and recovers from it.
type Simulation struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   SimulationSpec   `json:"spec,omitempty"`
	Status SimulationStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// SimulationList is a list of Simulations.
type SimulationList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Simulation `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Simulation{}, &SimulationList{})
}
