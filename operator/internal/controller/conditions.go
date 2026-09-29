package controller

import (
	"errors"
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	servingv1alpha1 "github.com/MuratAlkan06/ml-observability-system/operator/api/v1alpha1"
)

// setConditions writes Ready, CanaryActive and ShadowPaused on sd for
// sd.Generation (docs/PLAN.md D37 and its addenda). meta.SetStatusCondition
// moves a condition's lastTransitionTime when its status changes and only
// then, to now truncated to the second — the precision the field is stored
// at, so CanaryActive's stored value is exactly the clock the window's
// deadline was counted from.
//
// dep is the stable Deployment as reconcileStable last wrote or read it,
// reconcileErr is what reconcileStable returned, w is the window decided for
// this reconcile, and shadow is what reconcileWindow observed and wrote.
func setConditions(
	sd *servingv1alpha1.ServingDeployment, dep *appsv1.Deployment, reconcileErr error, w window,
	shadow shadowState, now time.Time,
) {
	stamp := metav1.NewTime(now.Truncate(time.Second))
	ready := metav1.Condition{
		Type:               servingv1alpha1.ConditionReady,
		ObservedGeneration: sd.Generation,
		LastTransitionTime: stamp,
	}
	switch {
	case reconcileErr != nil:
		ready.Status = metav1.ConditionFalse
		ready.Reason = servingv1alpha1.ReasonReconcileFailed
		if _, ok := errors.AsType[*adoptionError](reconcileErr); ok {
			ready.Reason = servingv1alpha1.ReasonAdoptionFailed
		}
		ready.Message = reconcileErr.Error()
	case stableAvailable(dep):
		ready.Status = metav1.ConditionTrue
		ready.Reason = servingv1alpha1.ReasonStableAvailable
		ready.Message = fmt.Sprintf("deployment/%s has rolled out %s and is Available",
			stableDeploymentName, stableImage(sd.Spec.ImageTag))
	default:
		ready.Status = metav1.ConditionFalse
		ready.Reason = servingv1alpha1.ReasonAwaitingRollout
		ready.Message = fmt.Sprintf("waiting for deployment/%s to roll out %s and become Available",
			stableDeploymentName, stableImage(sd.Spec.ImageTag))
	}
	meta.SetStatusCondition(&sd.Status.Conditions, ready)

	canaryStatus := metav1.ConditionFalse
	if w.open {
		canaryStatus = metav1.ConditionTrue
	}
	meta.SetStatusCondition(&sd.Status.Conditions, metav1.Condition{
		Type:               servingv1alpha1.ConditionCanaryActive,
		Status:             canaryStatus,
		ObservedGeneration: sd.Generation,
		LastTransitionTime: stamp,
		Reason:             w.reason,
		Message:            w.message(),
	})
	shadowStatus, shadowReason, shadowMessage := shadowPausedCondition(shadow)
	meta.SetStatusCondition(&sd.Status.Conditions, metav1.Condition{
		Type:               servingv1alpha1.ConditionShadowPaused,
		Status:             shadowStatus,
		ObservedGeneration: sd.Generation,
		LastTransitionTime: stamp,
		Reason:             shadowReason,
		Message:            shadowMessage,
	})
}

// stableAvailable reports whether dep has rolled out its current spec and is
// Available: the Deployment controller has observed dep's latest generation,
// every desired replica runs the current pod template and none of an older one
// remains (the test `kubectl rollout status` applies), and the Available
// condition is True. Without the first three, Ready could read True off an
// Available status that predates the operator's last image change.
func stableAvailable(dep *appsv1.Deployment) bool {
	if dep == nil || dep.Status.ObservedGeneration < dep.Generation {
		return false
	}
	desired := int32(1)
	if dep.Spec.Replicas != nil {
		desired = *dep.Spec.Replicas
	}
	status := dep.Status
	if status.UpdatedReplicas < desired || status.Replicas > status.UpdatedReplicas ||
		status.AvailableReplicas < status.UpdatedReplicas {
		return false
	}
	for _, c := range status.Conditions {
		if c.Type == appsv1.DeploymentAvailable {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}
