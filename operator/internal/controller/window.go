package controller

import (
	"context"
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	servingv1alpha1 "github.com/MuratAlkan06/ml-observability-system/operator/api/v1alpha1"
)

// canaryWindowTTL is how long a canary window stays open (docs/PLAN.md D37).
// XADD MAXLEN ~ 50000 at the frozen 5 rps is a trim horizon of about 2.8 h,
// and the TTL stays at or below a third of it: a shadow paused past the
// horizon takes a permanent, silent gap. The clock is CanaryActive's last
// False→True lastTransitionTime, kept in status, so an operator restart does
// not reset it (D37 addendum of O2).
const canaryWindowTTL = 45 * time.Minute

// canaryRequested reports whether spec asks for a canary window: a canary
// image tag and at least one canary replica, the shape a human's host-side
// patch gives it (docs/PLAN.md D32).
func canaryRequested(spec servingv1alpha1.ServingDeploymentSpec) bool {
	return spec.CanaryImageTag != "" && spec.CanaryReplicas >= 1
}

// canaryCleared reports whether spec is in the closed shape apply.sh's
// constant close-window patch leaves: no canary image tag and no canary
// replicas (D32). An expired window's latch releases only once a reconcile
// observes this shape.
func canaryCleared(spec servingv1alpha1.ServingDeploymentSpec) bool {
	return spec.CanaryImageTag == "" && spec.CanaryReplicas == 0
}

// window is one reconcile's decision about the canary window: whether to
// drive the stack toward the window state or toward steady, and the
// CanaryActive condition that goes with it.
type window struct {
	// open is whether the stack is driven toward the window state (canary up,
	// shadow at 0); otherwise it is driven toward steady (canary at 0,
	// shadow at 1). It is also CanaryActive's status.
	open bool
	// reason is CanaryActive's reason.
	reason string
	// deadline is when an open window expires.
	deadline time.Time
	// expiredNow marks the reconcile that finds an open window past its
	// deadline and closes it.
	expiredNow bool
	// progress completes CanaryActive's message with what the stack is
	// waiting on, if anything.
	progress string
}

// message is CanaryActive's message: a fixed sentence per reason, then the
// progress. Nothing in it is read back from the previous condition, so it
// cannot grow across reconciles.
func (w window) message() string {
	var head string
	switch w.reason {
	case servingv1alpha1.ReasonWindowOpen:
		head = "canary window open until " + w.deadline.UTC().Format(time.RFC3339)
	case servingv1alpha1.ReasonWindowExpired:
		head = fmt.Sprintf("the canary window reached its %s TTL and was closed; the spec keeps its canary "+
			"fields until the close-window patch clears them, and no window opens before it does", canaryWindowTTL)
	default:
		head = "no canary window is requested"
	}
	if w.progress == "" {
		return head
	}
	return head + "; " + w.progress
}

// decideWindow decides the window for spec, given the CanaryActive condition
// on record (nil if none) and the time now.
//
// A window opens when the spec requests one and none is latched; its clock
// starts now. It stays open while the spec requests it and the clock is
// inside canaryWindowTTL — a changed canaryImageTag does not restart it,
// because CanaryActive does not transition. It closes when the spec stops
// requesting it, or at the deadline. An expired window latches: it stays
// closed, whatever the canary fields say, until the spec is observed in the
// cleared shape (D32 clarification and D37 addendum of O2). The operator
// never writes the spec, so the latch lives in the condition alone.
func decideWindow(spec servingv1alpha1.ServingDeploymentSpec, prev *metav1.Condition, now time.Time) window {
	latched := prev != nil && prev.Status == metav1.ConditionFalse &&
		prev.Reason == servingv1alpha1.ReasonWindowExpired
	switch {
	case latched && !canaryCleared(spec):
		return window{reason: servingv1alpha1.ReasonWindowExpired}
	case !canaryRequested(spec):
		return window{reason: servingv1alpha1.ReasonNoCanary}
	case prev != nil && prev.Status == metav1.ConditionTrue:
		deadline := prev.LastTransitionTime.Add(canaryWindowTTL)
		if !now.Before(deadline) {
			return window{reason: servingv1alpha1.ReasonWindowExpired, expiredNow: true}
		}
		return window{open: true, reason: servingv1alpha1.ReasonWindowOpen, deadline: deadline}
	default:
		// lastTransitionTime is stored to the second, so the deadline is
		// counted from the stored value.
		return window{
			open:     true,
			reason:   servingv1alpha1.ReasonWindowOpen,
			deadline: now.Truncate(time.Second).Add(canaryWindowTTL),
		}
	}
}

// canaryGone reports whether dep, the canary Deployment, is observed at zero
// replicas with no pods left, terminating ones included. A missing canary has
// no pods. terminatingReplicas is beta and on by default in the pinned 1.36
// band; where it is not reported, status.replicas, which excludes terminating
// pods, is the only signal there is.
func canaryGone(dep *appsv1.Deployment) bool {
	if dep == nil {
		return true
	}
	var terminating int32
	if dep.Status.TerminatingReplicas != nil {
		terminating = *dep.Status.TerminatingReplicas
	}
	return replicasOf(dep.Spec.Replicas) == 0 &&
		dep.Status.ObservedGeneration >= dep.Generation &&
		dep.Status.Replicas == 0 &&
		terminating == 0
}

// reconcileWindow drives the canary and the shadow scorer toward w, in the
// order D26's memory bar needs (D37 addendum of O2): opening, the shadow is
// scaled to 0 and observed gone before the canary comes up; closing, the
// canary is scaled to 0 and observed gone before the shadow returns to 1. The
// canary is only scaled down once the stable Deployment is Available, so a
// promotion — or any close — leaves no serving gap (D32). Each call takes at
// most one step and returns; the watches on the shadow scorer and on the
// owned Deployments bring the next reconcile when the step is observed.
func (r *ServingDeploymentReconciler) reconcileWindow(
	ctx context.Context, sd *servingv1alpha1.ServingDeployment, stable *appsv1.Deployment, w *window,
) (shadowState, error) {
	sts, err := r.getShadow(ctx, sd.Namespace)
	if err != nil {
		return shadowState{}, err
	}
	shadow := shadowState{sts: sts}
	canary, err := r.getCanary(ctx, sd)
	if err != nil {
		return shadow, err
	}
	if w.open {
		return r.openWindow(ctx, sd, shadow, canary, w)
	}
	return r.closeWindow(ctx, sd, stable, shadow, canary, w)
}

// openWindow takes the next step toward the window state.
func (r *ServingDeploymentReconciler) openWindow(
	ctx context.Context, sd *servingv1alpha1.ServingDeployment, shadow shadowState, canary *appsv1.Deployment,
	w *window,
) (shadowState, error) {
	shadow, err := r.setShadowScale(ctx, sd, shadow, true)
	if err != nil {
		return shadow, err
	}
	if shadow.scaledTo != nil || !shadowGone(shadow.sts) {
		w.progress = fmt.Sprintf("pausing statefulset/%s before deployment/%s starts",
			shadowStatefulSetName, canaryDeploymentName)
		return shadow, nil
	}
	image := r.apiImage(sd.Spec.CanaryImageTag)
	if err := r.ensureCanary(ctx, sd, canary, image); err != nil {
		return shadow, err
	}
	w.progress = fmt.Sprintf("deployment/%s runs %s with the shadow scorer paused", canaryDeploymentName, image)
	return shadow, nil
}

// closeWindow takes the next step toward the steady state.
func (r *ServingDeploymentReconciler) closeWindow(
	ctx context.Context, sd *servingv1alpha1.ServingDeployment, stable *appsv1.Deployment, shadow shadowState,
	canary *appsv1.Deployment, w *window,
) (shadowState, error) {
	if canary != nil && replicasOf(canary.Spec.Replicas) != 0 {
		if !stableAvailable(stable) {
			w.progress = fmt.Sprintf("deployment/%s keeps serving until deployment/%s is Available",
				canaryDeploymentName, stableDeploymentName)
			return shadow, nil
		}
		if err := r.scaleCanaryDown(ctx, sd, canary); err != nil {
			return shadow, err
		}
		w.progress = fmt.Sprintf("deployment/%s scaled to 0", canaryDeploymentName)
		return shadow, nil
	}
	if !canaryGone(canary) {
		w.progress = fmt.Sprintf("waiting for the pods of deployment/%s to be gone before statefulset/%s resumes",
			canaryDeploymentName, shadowStatefulSetName)
		return shadow, nil
	}
	return r.setShadowScale(ctx, sd, shadow, false)
}

// getCanary returns deployment/api-canary, or nil if there is none. One that
// exists but is not controlled by sd is an error: the operator never takes
// over a canary it did not create.
func (r *ServingDeploymentReconciler) getCanary(
	ctx context.Context, sd *servingv1alpha1.ServingDeployment,
) (*appsv1.Deployment, error) {
	dep := &appsv1.Deployment{}
	err := r.Get(ctx, types.NamespacedName{Namespace: sd.Namespace, Name: canaryDeploymentName}, dep)
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("getting deployment/%s: %w", canaryDeploymentName, err)
	}
	if !metav1.IsControlledBy(dep, sd) {
		return nil, fmt.Errorf("deployment/%s exists and is not controlled by servingdeployment/%s; "+
			"the operator leaves it alone and moves neither the canary nor the shadow scorer",
			canaryDeploymentName, sd.Name)
	}
	return dep, nil
}

// ensureCanary makes deployment/api-canary run image at spec.canaryReplicas,
// creating it, controlled by sd, if it is absent.
func (r *ServingDeploymentReconciler) ensureCanary(
	ctx context.Context, sd *servingv1alpha1.ServingDeployment, canary *appsv1.Deployment, image string,
) error {
	replicas := sd.Spec.CanaryReplicas
	if canary == nil {
		dep := newCanaryDeployment(sd.Namespace, image, replicas)
		if err := controllerutil.SetControllerReference(sd, dep, r.Scheme); err != nil {
			return fmt.Errorf("setting the controller reference on deployment/%s: %w", canaryDeploymentName, err)
		}
		if err := r.Create(ctx, dep); err != nil {
			return fmt.Errorf("creating deployment/%s: %w", canaryDeploymentName, err)
		}
		logf.FromContext(ctx).Info("Created the canary Deployment", "deployment", canaryDeploymentName,
			"image", image, "replicas", replicas)
		r.Recorder.Eventf(sd, corev1.EventTypeNormal, eventReasonCanaryCreated,
			"created deployment/%s at image %s with %d replica(s)", canaryDeploymentName, image, replicas)
		return nil
	}

	container := apiContainer(canary)
	if container == nil {
		return fmt.Errorf("deployment/%s has no container named %q", canaryDeploymentName, apiContainerName)
	}
	if container.Image == image && replicasOf(canary.Spec.Replicas) == replicas {
		return nil
	}
	base := canary.DeepCopy()
	container.Image = image
	canary.Spec.Replicas = new(replicas)
	if err := r.Patch(ctx, canary, client.StrategicMergeFrom(base, client.MergeFromWithOptimisticLock{})); err != nil {
		return fmt.Errorf("patching deployment/%s: %w", canaryDeploymentName, err)
	}
	logf.FromContext(ctx).Info("Updated the canary Deployment", "deployment", canaryDeploymentName,
		"image", image, "replicas", replicas)
	r.Recorder.Eventf(sd, corev1.EventTypeNormal, eventReasonCanaryUpdated,
		"deployment/%s now at image %s with %d replica(s)", canaryDeploymentName, image, replicas)
	return nil
}

// scaleCanaryDown scales deployment/api-canary to 0, leaving its template
// alone. The Deployment stays, at zero, until the next window reuses it.
func (r *ServingDeploymentReconciler) scaleCanaryDown(
	ctx context.Context, sd *servingv1alpha1.ServingDeployment, canary *appsv1.Deployment,
) error {
	base := canary.DeepCopy()
	canary.Spec.Replicas = new(int32(0))
	if err := r.Patch(ctx, canary, client.StrategicMergeFrom(base, client.MergeFromWithOptimisticLock{})); err != nil {
		return fmt.Errorf("scaling deployment/%s to 0: %w", canaryDeploymentName, err)
	}
	logf.FromContext(ctx).Info("Scaled the canary Deployment to 0", "deployment", canaryDeploymentName)
	r.Recorder.Eventf(sd, corev1.EventTypeNormal, eventReasonCanaryScaledDown,
		"deployment/%s scaled to 0", canaryDeploymentName)
	return nil
}
