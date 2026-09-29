package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// Condition types the operator writes on a ServingDeployment, each with its
// lastTransitionTime; the operator is their sole writer (docs/PLAN.md D37).
const (
	// ConditionReady is True while the stable Deployment the operator
	// reconciles has rolled out its current spec and is Available. Read it
	// together with status.observedGeneration == metadata.generation: that
	// pair is the wait target of apply.sh's deploy sequence (D32; the D37
	// addendum of O1).
	ConditionReady = "Ready"
	// ConditionCanaryActive is True while a canary window is open: a canary
	// image tag is set and at least one canary replica is requested.
	ConditionCanaryActive = "CanaryActive"
	// ConditionShadowPaused is True while the shadow scorer's StatefulSet, as
	// observed, is at zero replicas and reports no pods. It is computed from
	// that observation, never from what the operator asked for: a scale the
	// operator has just written does not make it True (D37 addendum of O2).
	ConditionShadowPaused = "ShadowPaused"
)

// Condition reasons.
const (
	// ReasonStableAvailable: Ready is True.
	ReasonStableAvailable = "StableAvailable"
	// ReasonAwaitingRollout: Ready is False because the stable Deployment has
	// not yet rolled out its current spec to an Available state.
	ReasonAwaitingRollout = "AwaitingRollout"
	// ReasonAdoptionFailed: Ready is False because the stable Deployment
	// exists but cannot be adopted.
	ReasonAdoptionFailed = "AdoptionFailed"
	// ReasonReconcileFailed: Ready is False because reconciling the stable
	// Deployment returned an error; the message carries it.
	ReasonReconcileFailed = "ReconcileFailed"
	// ReasonNoCanary: CanaryActive is False because the operator runs no
	// canary Deployment. Until the canary window lands (O2), always.
	ReasonNoCanary = "NoCanary"
	// ReasonShadowRunning: ShadowPaused is False because the shadow scorer's
	// StatefulSet requests replicas, still reports pods, or was scaled by the
	// reconcile that wrote the condition and is not yet observed at its new
	// scale.
	ReasonShadowRunning = "ShadowRunning"
	// ReasonShadowScaledToZero: ShadowPaused is True; the StatefulSet is
	// observed at zero replicas with no pods.
	ReasonShadowScaledToZero = "ShadowScaledToZero"
	// ReasonShadowNotFound: ShadowPaused is False because the namespace holds
	// no shadow scorer StatefulSet to pause.
	ReasonShadowNotFound = "ShadowNotFound"
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
