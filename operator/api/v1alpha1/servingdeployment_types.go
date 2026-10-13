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
	// window's TTL clock (D37 addendum of O2). An acting gate that trips a
	// line turns it False before the TTL (D47).
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
	// ReasonBreachRolledBack: CanaryActive is False because the gate, at an
	// acting level, tripped a line of the O4 abort table; the message names
	// the line, and status.gate holds the evidence. The window is closed and
	// latched exactly as an expired one is: the spec is left alone, steady is
	// restored in the cluster, and no window opens until the spec passes
	// through the closed shape (D47).
	ReasonBreachRolledBack = "BreachRolledBack"
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
	// operator never writes it: a window that reaches its 45-minute TTL, or
	// that an acting gate rolls back, is closed in the cluster, and this field
	// stays until the close-window patch clears it.
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
	// pipeline deploy leaves it as it is. Removed from an open window, the
	// gate stops at once; the window's status.gate keeps its evidence, its
	// mode lowered to Observe, so a gate set again before that window closes
	// does not act in it.
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
	// mode is the gate's mode. ActRollback acts: a tripped line rolls the
	// window back (D47). ActPromote acts on the promote verdict as well: it
	// dispatches the Deploy workflow for the canary's tag, at most once per
	// verdict (D43), and still rolls back. With no dispatch token mounted it
	// runs as ActRollback, the level it contains, and a human actuates the
	// verdict. A window acts at the level sampled when it opens: a mode raised
	// mid-window takes effect from the next window, and one lowered, or its
	// tag mismatched, from the next boundary.
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
// +kubebuilder:validation:Enum=Armed;TagMismatch;TokenAbsent
type GateArming string

const (
	// GateArmingArmed: the mode spec.gate asks for is armed for the canary's
	// tag, and the gate runs it, from the next window if it was raised
	// mid-window; status.gate.mode is the level in effect.
	GateArmingArmed GateArming = "Armed"
	// GateArmingTagMismatch: an acting mode whose forCanaryImageTag is not
	// spec.canaryImageTag runs as Observe (D42).
	GateArmingTagMismatch GateArming = "TagMismatch"
	// GateArmingTokenAbsent: ActPromote, armed for the canary's tag, with no
	// dispatch token to read: the Secret the operator Deployment mounts it
	// from is absent, the off-season posture. It runs as ActRollback, the
	// level it contains, and the promote verdict is recorded for a human to
	// actuate (D43).
	GateArmingTokenAbsent GateArming = "TokenAbsent"
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

	// mode is the acting level the gate ran its last boundary at: sampled
	// when the window opened, lowered at a boundary by a lowered or
	// mismatched spec.gate and at once by its removal, and never raised
	// before the next window.
	// +required
	Mode GateMode `json:"mode"`

	// arming says how the gate armed spec.gate.mode at its last boundary:
	// Armed; TagMismatch, an acting mode armed for another canary tag, run as
	// Observe; or TokenAbsent, ActPromote with no dispatch token to read, run
	// as ActRollback.
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
	// the table. At Observe it is recorded and nothing acts on it: the window
	// stays open until its TTL or a close. At ActRollback and ActPromote it is
	// the trip that rolled the window back, CanaryActive False with
	// BreachRolledBack (D47).
	// +optional
	RollbackVerdict *GateRollbackVerdict `json:"rollbackVerdict,omitempty"`

	// promoteVerdict is the first interval that completed 11 consecutive
	// clean intervals (D44). Below ActPromote it is recorded and a human
	// actuates it (D43); at ActPromote its dispatch says what the gate did
	// with it.
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
	// rolledBackAt is when the gate rolled the window back on this trip: the
	// reconcile that latched CanaryActive False with BreachRolledBack, to the
	// second. Absent when the trip was recorded at Observe, which acts on
	// nothing.
	// +optional
	RolledBackAt *metav1.Time `json:"rolledBackAt,omitempty"`
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
	// dispatch is what the gate did with this verdict at ActPromote (D43,
	// D47), absent below it.
	// +optional
	Dispatch *GateDispatch `json:"dispatch,omitempty"`
}

// GateDispatchState is how far the gate took a promote verdict's dispatch
// (docs/PLAN.md D43, D47).
// +kubebuilder:validation:Enum=Deferred;NotDispatched;Attempted;Dispatched;Failed
type GateDispatchState string

const (
	// GateDispatchDeferred: a Deploy run was queued, waiting, pending,
	// requested or in progress, and nothing was sent, so as neither to evict
	// a pending run from deploy.yml's concurrency group nor to queue behind
	// one. The gate checks again at each later boundary while the window runs
	// at ActPromote, and dispatches at the first that finds no such run, its
	// clean run whole and 15 minutes of the TTL left.
	GateDispatchDeferred GateDispatchState = "Deferred"
	// GateDispatchNotDispatched: the gate does not dispatch this verdict, for
	// the reason recorded. The verdict stays recorded and a human actuates it.
	GateDispatchNotDispatched GateDispatchState = "NotDispatched"
	// GateDispatchAttempted: the latch, written before the dispatch is sent.
	// A verdict left here — the operator stopped between the latch and
	// GitHub's answer — is never dispatched again: whether a run exists is
	// the human's to read in Actions, and a human actuates.
	GateDispatchAttempted GateDispatchState = "Attempted"
	// GateDispatchDispatched: GitHub accepted the dispatch. Its answer names
	// no run, so the run's URL is recorded by hand on the slice issue.
	GateDispatchDispatched GateDispatchState = "Dispatched"
	// GateDispatchFailed: GitHub refused the dispatch or did not answer. It is
	// not retried; a human actuates the verdict.
	GateDispatchFailed GateDispatchState = "Failed"
)

// GateDispatchReason says why a promote verdict was deferred or not
// dispatched.
// +kubebuilder:validation:Enum=RunPending;BelowFloor;TagInvalid;OwnerChainUnverified;CanaryPodStale;TokenAbsent;RunsUnreadable
type GateDispatchReason string

const (
	// GateDispatchRunPending: a Deploy run was queued, waiting, pending,
	// requested or in progress (Deferred).
	GateDispatchRunPending GateDispatchReason = "RunPending"
	// GateDispatchBelowFloor: less than 15 minutes of the window's TTL
	// remained, the dispatch floor (D47).
	GateDispatchBelowFloor GateDispatchReason = "BelowFloor"
	// GateDispatchTagInvalid: spec.canaryImageTag is not a 40-character
	// lowercase hex commit SHA.
	GateDispatchTagInvalid GateDispatchReason = "TagInvalid"
	// GateDispatchOwnerChainUnverified: a pod the gate read is not shown to
	// be deployment/api's or deployment/api-canary's.
	GateDispatchOwnerChainUnverified GateDispatchReason = "OwnerChainUnverified"
	// GateDispatchCanaryPodStale: the canary pod the gate read does not run
	// the image spec.canaryImageTag renders, or is being deleted, so its
	// evidence is not shown to be the dispatched tag's.
	GateDispatchCanaryPodStale GateDispatchReason = "CanaryPodStale"
	// GateDispatchTokenAbsent: the dispatch token could not be read when the
	// dispatch was due.
	GateDispatchTokenAbsent GateDispatchReason = "TokenAbsent"
	// GateDispatchRunsUnreadable: the Deploy runs could not be listed, so a
	// queued human deploy could not be ruled out.
	GateDispatchRunsUnreadable GateDispatchReason = "RunsUnreadable"
)

// GateDispatch is what the gate did with a promote verdict at ActPromote: at
// most one workflow_dispatch of deploy.yml on main, whose sha input is the
// canary's tag (D43).
type GateDispatch struct {
	// state is how far the dispatch went.
	// +required
	State GateDispatchState `json:"state"`
	// reason says why a Deferred or NotDispatched verdict was not
	// dispatched.
	// +optional
	Reason GateDispatchReason `json:"reason,omitempty"`
	// attemptedAt is the latch's time, to the second. The status write that
	// carries it lands before the dispatch is sent, and nothing sends a
	// latched verdict again.
	// +optional
	AttemptedAt *metav1.Time `json:"attemptedAt,omitempty"`
	// sha is the dispatch's sha input: spec.canaryImageTag, as validated at
	// the attempt.
	// +optional
	// +kubebuilder:validation:Pattern=`^[0-9a-f]{40}$`
	SHA string `json:"sha,omitempty"`
	// statusCode is the HTTP status GitHub answered the dispatch with,
	// absent when no answer came.
	// +optional
	// +kubebuilder:validation:Minimum=100
	// +kubebuilder:validation:Maximum=599
	StatusCode int32 `json:"statusCode,omitempty"`
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
	// effectiveLevel is the acting level the interval was evaluated at, the
	// gate's mode at the boundary that recorded it: at ActRollback a tripped
	// line rolls the window back, and at Observe nothing acts. The operator
	// writes it on every record; one without it predates the field and was
	// evaluated at Observe.
	// +optional
	EffectiveLevel GateMode `json:"effectiveLevel,omitempty"`
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
