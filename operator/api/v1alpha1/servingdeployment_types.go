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
	// ConditionCanaryActive is True while a canary window is open and
	// unexpired: the spec names a canary image tag and at least one canary
	// replica, and fewer than 45 minutes have passed since the condition last
	// turned True. It turns True on the reconcile that starts opening the
	// window by pausing the shadow scorer, and its lastTransitionTime is the
	// window's TTL clock (D37 addendum of O2).
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
	// ReasonWindowOpen: CanaryActive is True; the window is open and its TTL
	// has not run out.
	ReasonWindowOpen = "WindowOpen"
	// ReasonNoCanary: CanaryActive is False because the spec requests no
	// canary window.
	ReasonNoCanary = "NoCanary"
	// ReasonWindowExpired: CanaryActive is False because the window's TTL ran
	// out. The operator closed it in the cluster and left the spec alone; the
	// condition stays latched here, and no window opens, until the spec passes
	// through the closed shape the close-window patch leaves (D32
	// clarification of O2).
	ReasonWindowExpired = "WindowExpired"
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
	// window and is never rendered by the pipeline (docs/PLAN.md D32). The
	// operator never writes it: a window that reaches its 45-minute TTL is
	// closed in the cluster, and this field stays until the close-window patch
	// clears it.
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

	// gate opts the canary windows into the metric-driven gate (docs/PLAN.md
	// D42). Absent, the operator is v0: it reads no pod's metrics, writes no
	// status.gate and emits no gate Event, and promote and rollback stay
	// human. Like the canary fields it is set by the host-side patch that
	// opens a window and is never rendered by the pipeline (D32), so a
	// pipeline deploy leaves it as it is.
	// +optional
	Gate *GateSpec `json:"gate,omitempty"`
}

// GateMode is the gate's mode, an ordered lattice in which each mode strictly
// contains the one before (docs/PLAN.md D42): Observe records every interval
// and acts on nothing; ActRollback adds the autonomous breach rollback;
// ActPromote adds the promote dispatch and still rolls back. A mode that
// promotes but will not abort is not offered.
// +kubebuilder:validation:Enum=Observe;ActRollback;ActPromote
type GateMode string

const (
	// GateModeObserve records every interval in status.gate and acts on
	// nothing.
	GateModeObserve GateMode = "Observe"
	// GateModeActRollback adds the autonomous breach rollback to Observe.
	GateModeActRollback GateMode = "ActRollback"
	// GateModeActPromote adds the promote dispatch to ActRollback.
	GateModeActPromote GateMode = "ActPromote"
)

// GateSpec is the gate a ServingDeployment opts into (docs/PLAN.md D42).
type GateSpec struct {
	// mode is the gate's mode. In this version of the operator every mode
	// runs as Observe: ActRollback and ActPromote are accepted, and a window
	// that asks for one is observed and says so in an Event, until the slices
	// that give them their acting behavior land.
	// +required
	Mode GateMode `json:"mode"`

	// forCanaryImageTag binds an acting mode to one canary: ActRollback and
	// ActPromote are armed only while it equals spec.canaryImageTag, and
	// otherwise the gate runs as Observe and says so in an Event. Consent is
	// given per canary tag, so a gate left in the spec does not arm itself for
	// the next window's canary (D42). Observe needs no tag.
	// +optional
	// +kubebuilder:validation:Pattern=`^[0-9a-f]{40}$`
	ForCanaryImageTag string `json:"forCanaryImageTag,omitempty"`
}

// GateArming is how the gate armed the mode spec.gate asks for, as of the
// last boundary it read.
// +kubebuilder:validation:Enum=Armed;TagMismatch;NotYetActive
type GateArming string

const (
	// GateArmingArmed: the gate runs the mode spec.gate asks for.
	GateArmingArmed GateArming = "Armed"
	// GateArmingTagMismatch: an acting mode whose forCanaryImageTag is not
	// spec.canaryImageTag runs as Observe (D42).
	GateArmingTagMismatch GateArming = "TagMismatch"
	// GateArmingNotYetActive: an acting mode, armed for the canary's tag, that
	// this version of the operator does not act in yet runs as Observe.
	GateArmingNotYetActive GateArming = "NotYetActive"
)

// GateVerdict classifies one interval, in the decision engine's vocabulary
// (operator/internal/gate; docs/PLAN.md D44, D45). Breach and Unevaluable
// count toward line 3's two in a row and Clean toward the clean run; Warmup
// resets line 3's counter and adds nothing to the clean run; Pending counts
// toward neither.
// +kubebuilder:validation:Enum=Pending;Warmup;Clean;Breach;Unevaluable
type GateVerdict string

// GateReason explains an interval's verdict, from the decision engine's fixed
// vocabulary (D42): the canary's state, the line that tripped, or the cause
// that left line 3 unevaluated, OperatorInterrupted among them (D45).
// +kubebuilder:validation:Enum=CanaryNotReady;CanaryWarmup;CanaryOOMKilled;CanaryErrors;LatencyRatio;OperatorInterrupted;StableReadFailed;CanaryReadFailed;StableNoBaseline;CanaryNoBaseline;StablePodChanged;CanaryPodChanged;StableCounterReset;CanaryCounterReset;StableBelowFloor;CanaryBelowFloor;StableNoP95;CanaryNoP95;NoRatio
type GateReason string

// GateTrip names the line of the O4 abort table that tripped.
// +kubebuilder:validation:Enum=Line5CanaryOOMKilled;Line2CanaryErrors;Line3CanaryLatency
type GateTrip string

// GateStatus is the gate's evidence for the window it observes, kept in
// status beside the window's clock so that it survives an operator restart
// (docs/PLAN.md D42, D45). It changes only when the gate reads a boundary:
// between boundaries it is byte for byte what the last boundary left.
type GateStatus struct {
	// windowStart anchors the interval grid: CanaryActive's
	// lastTransitionTime for the window observed. Boundary n is windowStart
	// plus n intervals, and interval n runs from boundary n to boundary n+1.
	// +required
	WindowStart metav1.Time `json:"windowStart"`

	// intervalSeconds is the grid's interval: 60, the O4 interval.
	// +required
	// +kubebuilder:validation:Minimum=1
	IntervalSeconds int32 `json:"intervalSeconds"`

	// mode is the mode the gate ran its last boundary as.
	// +required
	Mode GateMode `json:"mode"`

	// arming says how the gate armed spec.gate.mode at its last boundary.
	// +required
	Arming GateArming `json:"arming"`

	// nextBoundary is the boundary the gate reads next. Each boundary it
	// reads, or finds missed, records the interval that ends there.
	// +required
	// +kubebuilder:validation:Minimum=1
	NextBoundary int32 `json:"nextBoundary"`

	// breachRun is line 3's counter: the consecutive intervals with a latency
	// breach or unevaluable.
	// +required
	// +kubebuilder:validation:Minimum=0
	BreachRun int32 `json:"breachRun"`

	// cleanRun is the consecutive clean intervals; the promote verdict comes
	// at 11 (D44).
	// +required
	// +kubebuilder:validation:Minimum=0
	CleanRun int32 `json:"cleanRun"`

	// canaryReadySeen is whether a canary pod has been Ready at a boundary of
	// the window. Until one has, intervals are pending (D45).
	// +required
	CanaryReadySeen bool `json:"canaryReadySeen"`

	// canaryPod is the canary pod last read: a new one resets the clean run
	// (D44).
	// +optional
	CanaryPod *GatePod `json:"canaryPod,omitempty"`

	// totals counts the window's recorded intervals by verdict.
	// +required
	Totals GateTotals `json:"totals"`

	// lastVerdict is the verdict of the last interval recorded.
	// +optional
	LastVerdict GateVerdict `json:"lastVerdict,omitempty"`

	// rollbackVerdict is the first interval whose verdict tripped a line of
	// the table. In Observe it is recorded and nothing acts on it: the window
	// stays open until its TTL or a close.
	// +optional
	RollbackVerdict *GateRollbackVerdict `json:"rollbackVerdict,omitempty"`

	// promoteVerdict is the first interval that completed 11 consecutive
	// clean intervals (D44). In Observe it is recorded and nothing is
	// dispatched.
	// +optional
	PromoteVerdict *GatePromoteVerdict `json:"promoteVerdict,omitempty"`

	// records are the window's intervals, oldest first: a ring of 50 slots,
	// sized to the 45-minute TTL's 45 intervals (D42).
	// +optional
	// +listType=atomic
	// +kubebuilder:validation:MaxItems=50
	Records []GateRecord `json:"records,omitempty"`
}

// GatePod identifies a pod across reads.
type GatePod struct {
	// name is the pod's name.
	// +required
	Name string `json:"name"`
	// uid tells apart a pod recreated under the same name.
	// +required
	UID string `json:"uid"`
}

// GateTotals counts a window's recorded intervals by verdict.
type GateTotals struct {
	// pending counts the Pending intervals.
	// +required
	Pending int32 `json:"pending"`
	// warmup counts the Warmup intervals.
	// +required
	Warmup int32 `json:"warmup"`
	// clean counts the Clean intervals.
	// +required
	Clean int32 `json:"clean"`
	// breach counts the Breach intervals.
	// +required
	Breach int32 `json:"breach"`
	// unevaluable counts the Unevaluable intervals, the forfeited ones among
	// them.
	// +required
	Unevaluable int32 `json:"unevaluable"`
}

// GateRollbackVerdict is the interval at which a line of the table tripped.
type GateRollbackVerdict struct {
	// interval is the index of the interval that tripped it.
	// +required
	// +kubebuilder:validation:Minimum=0
	Interval int32 `json:"interval"`
	// at is the boundary that interval ends at.
	// +required
	At metav1.Time `json:"at"`
	// trip names the line that tripped.
	// +required
	Trip GateTrip `json:"trip"`
}

// GatePromoteVerdict is the interval that completed the clean run.
type GatePromoteVerdict struct {
	// interval is the index of that interval.
	// +required
	// +kubebuilder:validation:Minimum=0
	Interval int32 `json:"interval"`
	// at is the boundary that interval ends at.
	// +required
	At metav1.Time `json:"at"`
}

// GateRecord is one interval's evidence and verdict (D42).
type GateRecord struct {
	// index is the interval's place on the grid.
	// +required
	// +kubebuilder:validation:Minimum=0
	Index int32 `json:"index"`
	// start is the boundary the interval starts at.
	// +required
	Start metav1.Time `json:"start"`
	// end is the boundary the interval ends at.
	// +required
	End metav1.Time `json:"end"`
	// stable is the stable pod's change over the interval, absent where none
	// could be taken.
	// +optional
	Stable *GatePodDelta `json:"stable,omitempty"`
	// canary is the canary pod's change over the interval, absent where none
	// could be taken.
	// +optional
	Canary *GatePodDelta `json:"canary,omitempty"`
	// ratio is the canary's p95 over the stable's, as a decimal, absent where
	// there is none.
	// +optional
	Ratio string `json:"ratio,omitempty"`
	// verdict classifies the interval.
	// +required
	Verdict GateVerdict `json:"verdict"`
	// reasons explain the verdict, in the order watch-window.sh prints its
	// lines.
	// +optional
	// +listType=atomic
	Reasons []GateReason `json:"reasons,omitempty"`
}

// GatePodDelta is one pod's change over an interval: watch-window.sh's
// pod_delta.
type GatePodDelta struct {
	// requests is the /predict requests the pod served in the interval.
	// +required
	Requests int64 `json:"requests"`
	// errors is the 5xx among them.
	// +required
	Errors int64 `json:"errors"`
	// p95Seconds is histogram_quantile(0.95) over the bucket changes, as a
	// decimal, absent where they give none.
	// +optional
	P95Seconds string `json:"p95Seconds,omitempty"`
}

// ServingDeploymentStatus defines the observed state of ServingDeployment.
type ServingDeploymentStatus struct {
	// conditions carry the stack's state. The operator writes Ready,
	// CanaryActive and ShadowPaused, each with lastTransitionTime, and is
	// their sole writer (docs/PLAN.md D37 and its addenda).
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// observedGeneration is the metadata.generation the status was last
	// computed for.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// gate is the evidence of the gate spec.gate opts into, for the last
	// window it observed (docs/PLAN.md D42). The operator is its sole writer.
	// +optional
	Gate *GateStatus `json:"gate,omitempty"`
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
