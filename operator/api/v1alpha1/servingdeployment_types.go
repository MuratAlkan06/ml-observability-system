package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// Condition types the operator writes on a ServingDeployment, each with its
// lastTransitionTime; the operator is their sole writer (docs/PLAN.md D37).
const (
	// ConditionCanaryActive is True while a canary window is open: a canary
	// image tag is set and at least one canary replica is requested.
	ConditionCanaryActive = "CanaryActive"
	// ConditionShadowPaused is True while the shadow scorer is scaled to zero
	// for an open canary window.
	ConditionShadowPaused = "ShadowPaused"
)

// ServingDeploymentSpec defines the desired state of ServingDeployment.
//
// The resource moves image tags only. Model identity (model_name,
// model_revision, model_version) is frozen inside the image and is not part
// of the spec (docs/PLAN.md D31).
type ServingDeploymentSpec struct {
	// imageTag is the stable api image tag: the full 40-character lowercase
	// hex commit SHA the pipeline deploys. It is the only field the pipeline
	// renders (docs/PLAN.md D32).
	// +required
	// +kubebuilder:validation:Pattern=`^[0-9a-f]{40}$`
	ImageTag string `json:"imageTag"`

	// canaryImageTag is the canary api image tag, in the same 40-character
	// commit SHA form. It is set only by a host-side patch that opens a canary
	// window and is never rendered by the pipeline (docs/PLAN.md D32).
	// +optional
	// +kubebuilder:validation:Pattern=`^[0-9a-f]{40}$`
	CanaryImageTag string `json:"canaryImageTag,omitempty"`

	// canaryReplicas is the number of canary api pods: 0 or 1. Two torch api
	// pods and the shadow scorer do not fit on the host together, which is why
	// an open window pauses the shadow and holds the canary at one pod
	// (docs/PLAN.md D37).
	// +optional
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=1
	CanaryReplicas int32 `json:"canaryReplicas,omitempty"`
}

// ServingDeploymentStatus defines the observed state of ServingDeployment.
type ServingDeploymentStatus struct {
	// conditions carry the stack's state. The operator writes
	// CanaryActive and ShadowPaused (docs/PLAN.md D37).
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// observedGeneration is the metadata.generation the status was last
	// computed for.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status

// ServingDeployment is the Schema for the servingdeployments API
type ServingDeployment struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of ServingDeployment
	// +required
	Spec ServingDeploymentSpec `json:"spec"`

	// status defines the observed state of ServingDeployment
	// +optional
	Status ServingDeploymentStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// ServingDeploymentList contains a list of ServingDeployment
type ServingDeploymentList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []ServingDeployment `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &ServingDeployment{}, &ServingDeploymentList{})
		return nil
	})
}
