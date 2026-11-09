// Package v1alpha1 contains the API types for the test-environment simulation
// controller: Simulation (a fault to inject and recover) and SimulationCluster
// (a remote agent a hub can manage simulations on).
//
// +kubebuilder:object:generate=true
// +groupName=sim.testenv.onit.ai
package v1alpha1

import (
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/scheme"
)

var (
	// GroupVersion is the API group and version of these types.
	GroupVersion = schema.GroupVersion{Group: "sim.testenv.onit.ai", Version: "v1alpha1"}

	// SchemeBuilder registers the types with a runtime.Scheme.
	SchemeBuilder = &scheme.Builder{GroupVersion: GroupVersion}

	// AddToScheme adds the types in this group-version to a scheme.
	AddToScheme = SchemeBuilder.AddToScheme
)
