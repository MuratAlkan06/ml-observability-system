package controller

import (
	"context"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv1 "k8s.io/api/autoscaling/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	servingv1alpha1 "github.com/MuratAlkan06/ml-observability-system/operator/api/v1alpha1"
)

// shadowStatefulSetName is the shadow scorer's StatefulSet: the one object in
// the namespace whose replica count the operator may write, and only through
// the scale subresource (docs/PLAN.md D33 addendum of O2).
const shadowStatefulSetName = "shadow-scorer"

// The only two writes the operator ever sends the shadow scorer: its replica
// count, as one of these two literal merge patches on the scale subresource.
// Nothing computes a replica value for it, so no other value can be sent; a
// test pins both bodies byte for byte (D33 addendum).
const (
	shadowPauseBody  = `{"spec":{"replicas":0}}`
	shadowResumeBody = `{"spec":{"replicas":1}}`
)

// shadowScalePatch returns the patch that pauses the shadow scorer (scale 0)
// or resumes it (scale 1).
func shadowScalePatch(pause bool) client.Patch {
	body := shadowResumeBody
	if pause {
		body = shadowPauseBody
	}
	return client.RawPatch(types.MergePatchType, []byte(body))
}

// shadowReplicas is the literal a shadowScalePatch(pause) body carries.
func shadowReplicas(pause bool) int32 {
	if pause {
		return 0
	}
	return 1
}

// shadowState is what one reconcile knows about the shadow scorer: the
// StatefulSet as it read it, and the scale it wrote, if it wrote one.
type shadowState struct {
	// sts is the StatefulSet as read at the start of the reconcile, or nil
	// when the namespace holds none.
	sts *appsv1.StatefulSet
	// scaledTo is the replica count this reconcile wrote, or nil. A write
	// bumps the StatefulSet's generation, so after one the status read
	// before it describes the previous scale.
	scaledTo *int32
}

// replicasOf reads a replica count the way the API server defaults an absent
// one.
func replicasOf(replicas *int32) int32 {
	if replicas == nil {
		return 1
	}
	return *replicas
}

// shadowGone reports whether sts is observed at zero replicas with no pods
// left: its spec asks for none, its status describes that spec, and the
// status counts no pods. The StatefulSet controller counts a terminating pod
// in status.replicas, so zero means the pods are gone, not merely going. A
// namespace with no shadow StatefulSet has no shadow pods.
func shadowGone(sts *appsv1.StatefulSet) bool {
	if sts == nil {
		return true
	}
	return replicasOf(sts.Spec.Replicas) == 0 &&
		sts.Status.ObservedGeneration >= sts.Generation &&
		sts.Status.Replicas == 0
}

// getShadow returns the shadow scorer's StatefulSet in namespace, or nil if
// there is none.
func (r *ServingDeploymentReconciler) getShadow(ctx context.Context, namespace string) (*appsv1.StatefulSet, error) {
	sts := &appsv1.StatefulSet{}
	err := r.Get(ctx, types.NamespacedName{Namespace: namespace, Name: shadowStatefulSetName}, sts)
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("getting statefulset/%s: %w", shadowStatefulSetName, err)
	}
	return sts, nil
}

// setShadowScale brings the observed shadow scorer to scale 0 (pause) or 1
// (resume), writing only when it is elsewhere. A missing StatefulSet is left
// missing: the operator cannot create one, and has nothing to pause.
func (r *ServingDeploymentReconciler) setShadowScale(
	ctx context.Context, sd *servingv1alpha1.ServingDeployment, shadow shadowState, pause bool,
) (shadowState, error) {
	want := shadowReplicas(pause)
	if shadow.sts == nil || replicasOf(shadow.sts.Spec.Replicas) == want {
		return shadow, nil
	}
	// The target carries a name and a namespace and nothing else: the
	// request is the literal body on statefulsets/<name>/scale, and no field
	// of the observed object can reach it.
	target := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Namespace: sd.Namespace, Name: shadowStatefulSetName}}
	if err := r.SubResource("scale").Patch(ctx, target, shadowScalePatch(pause),
		client.WithSubResourceBody(&autoscalingv1.Scale{})); err != nil {
		return shadow, fmt.Errorf("scaling statefulset/%s to %d: %w", shadowStatefulSetName, want, err)
	}
	shadow.scaledTo = &want
	logf.FromContext(ctx).Info("Scaled the shadow scorer", "statefulset", shadowStatefulSetName, "replicas", want)
	r.Recorder.Eventf(sd, corev1.EventTypeNormal, eventReasonShadowScaled,
		"statefulset/%s scaled to %d", shadowStatefulSetName, want)
	return shadow, nil
}

// shadowPausedCondition computes ShadowPaused from the shadow scorer as this
// reconcile observed it, never from what it asked for (D37 addendum of O2).
func shadowPausedCondition(shadow shadowState) (metav1.ConditionStatus, string, string) {
	switch {
	case shadow.sts == nil:
		return metav1.ConditionFalse, servingv1alpha1.ReasonShadowNotFound,
			fmt.Sprintf("the namespace holds no statefulset/%s", shadowStatefulSetName)
	case shadow.scaledTo != nil:
		return metav1.ConditionFalse, servingv1alpha1.ReasonShadowRunning,
			fmt.Sprintf("statefulset/%s was just scaled to %d and is not yet observed there",
				shadowStatefulSetName, *shadow.scaledTo)
	case shadowGone(shadow.sts):
		return metav1.ConditionTrue, servingv1alpha1.ReasonShadowScaledToZero,
			fmt.Sprintf("statefulset/%s is observed at 0 replicas with no pods", shadowStatefulSetName)
	default:
		return metav1.ConditionFalse, servingv1alpha1.ReasonShadowRunning,
			fmt.Sprintf("statefulset/%s requests %d replica(s) and reports %d pod(s)", shadowStatefulSetName,
				replicasOf(shadow.sts.Spec.Replicas), shadow.sts.Status.Replicas)
	}
}
