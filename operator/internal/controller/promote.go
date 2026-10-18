package controller

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	servingv1alpha1 "github.com/MuratAlkan06/ml-observability-system/operator/api/v1alpha1"
	"github.com/MuratAlkan06/ml-observability-system/operator/internal/gate"
	"github.com/MuratAlkan06/ml-observability-system/operator/internal/scrape"
)

// ActPromote's dispatch (docs/PLAN.md D43, D47). At a boundary where a window
// running at ActPromote has its promote verdict latched, the gate dispatches
// the Deploy workflow for the canary's tag — at most once per verdict — only
// after each of these holds, in this order, the ones that ask nothing of the
// network first:
//
//  1. at least dispatchFloor of the window's TTL remains (D47);
//  2. the clean run still holds at this boundary;
//  3. spec.canaryImageTag is a full commit SHA;
//  4. the pods the boundary read are deployment/api's and
//     deployment/api-canary's (#108, N1);
//  5. the canary pod read runs the image that tag renders and is not being
//     deleted, so the evidence is the dispatched tag's (canaryRuns);
//  6. the token reads;
//  7. no Deploy run is queued, waiting, pending, requested or in progress.
//
// The dispatch is then latched: the status write that records the attempt,
// its time and its sha lands before the dispatch is sent, and a latched
// verdict is never sent again. That write is optimistically locked to the
// ServingDeployment the reconcile read, so a spec changed since conflicts it
// and nothing is sent. A crash between the two leaves the latch and no run,
// and a human actuates. GitHub's answer is recorded after, in a second write:
// accepted, or refused or unanswered and not retried. A verdict that fails 1,
// 3, 4, 5, 6 or the runs' listing is not dispatched at all, and a human
// actuates it. One that fails 7 is deferred and checked again at each later
// boundary, where it waits too while 2 fails, until 1 fails; dispatched, it
// rests on the clean run whole at that boundary, a canary replaced since
// included, and its Event names that run (#60). Every way a dispatch does not
// happen lands on a human or on waiting, never on a promote.

// dispatchFloor is the TTL a verdict must have left to be dispatched (D47):
// 13 minutes, the longest way back on record (deploy run 36517554863, its
// approval wait included), and 2 to spare. It is compiled, not a setting.
const dispatchFloor = 15 * time.Minute

// Event reasons of the promote dispatch, on transitions only: the verdict
// dispatched, its dispatch failed, deferred, a deferral ended in a dispatch,
// or not dispatched.
const (
	eventReasonGatePromoteDispatched     = "GatePromoteDispatched"
	eventReasonGatePromoteDispatchFailed = "GatePromoteDispatchFailed"
	eventReasonGatePromoteDeferred       = "GatePromoteDeferred"
	eventReasonGatePromoteDeferralEnded  = "GatePromoteDeferralEnded"
	eventReasonGatePromoteNotDispatched  = "GatePromoteNotDispatched"
)

// dispatchPlan is a latched dispatch, sent once the status write carrying its
// latch has landed.
type dispatchPlan struct {
	token, sha string
}

// promote takes the gate's promote verdict, latched on st, one step at a
// boundary where the window runs at ActPromote: b is the boundary's read,
// promoting whether the interval it ended carried the promote decision — the
// clean run whole — and deadline the window's TTL. It writes on st what it
// decided, returns its Events, and puts on step the dispatch to send once the
// latch is written. A verdict already latched, or turned down, is left as it
// is: at most once.
func (r *ServingDeploymentReconciler) promote(
	ctx context.Context, sd *servingv1alpha1.ServingDeployment, st *servingv1alpha1.GateStatus, b scrape.Boundary,
	promoting bool, deadline time.Time, step *gateStep,
) []gateEvent {
	pv := st.PromoteVerdict
	deferred := pv.Dispatch != nil && pv.Dispatch.State == servingv1alpha1.GateDispatchDeferred
	if pv.Dispatch != nil && !deferred {
		return nil
	}
	notDispatched := func(reason servingv1alpha1.GateDispatchReason, why string) []gateEvent {
		pv.Dispatch = &servingv1alpha1.GateDispatch{State: servingv1alpha1.GateDispatchNotDispatched, Reason: reason}
		return []gateEvent{{corev1.EventTypeWarning, eventReasonGatePromoteNotDispatched, fmt.Sprintf(
			"the promote verdict of interval %d is not dispatched: %s; it stays recorded, and a human actuates it",
			pv.Interval, why)}}
	}

	// The clock is read here, after the boundary's reads, so the floor is
	// judged as late as the dispatch could be sent.
	now := r.now()
	tag := sd.Spec.CanaryImageTag
	if remaining := deadline.Sub(now); remaining < dispatchFloor {
		return notDispatched(servingv1alpha1.GateDispatchBelowFloor, fmt.Sprintf(
			"%s of the window's TTL remain, under the %s dispatch floor (D47)", remaining.Round(time.Second), dispatchFloor))
	}
	if !promoting {
		// Only a deferred verdict meets a broken clean run: it waits for the
		// run to be whole again, or for the floor.
		return nil
	}
	if !commitSHA.MatchString(tag) {
		return notDispatched(servingv1alpha1.GateDispatchTagInvalid, fmt.Sprintf(
			"spec.canaryImageTag %q is not a 40-character lowercase hex commit SHA", tag))
	}
	if err := r.verifyOwners(ctx, sd, b); err != nil {
		return notDispatched(servingv1alpha1.GateDispatchOwnerChainUnverified, err.Error())
	}
	if err := canaryRuns(b.CanaryPod, r.apiImage(tag)); err != nil {
		return notDispatched(servingv1alpha1.GateDispatchCanaryPodStale, err.Error())
	}
	token, err := r.dispatcher.token()
	if err != nil {
		return notDispatched(servingv1alpha1.GateDispatchTokenAbsent, err.Error())
	}
	pending, err := r.dispatcher.pendingRun(ctx, token)
	if err != nil {
		return notDispatched(servingv1alpha1.GateDispatchRunsUnreadable, fmt.Sprintf(
			"the Deploy runs could not be listed, so a queued human deploy cannot be ruled out: %v", err))
	}
	if pending != "" {
		if deferred {
			return nil
		}
		pv.Dispatch = &servingv1alpha1.GateDispatch{
			State: servingv1alpha1.GateDispatchDeferred, Reason: servingv1alpha1.GateDispatchRunPending,
		}
		return []gateEvent{{corev1.EventTypeWarning, eventReasonGatePromoteDeferred, fmt.Sprintf(
			"the promote verdict of interval %d for canary tag %s is deferred: a Deploy run is %s, and a dispatch "+
				"beside it would evict it or queue behind it; the gate checks again at each boundary, and "+
				"dispatches once no run is pending while the clean run holds and %s of the TTL remain",
			pv.Interval, tag, pending, dispatchFloor)}}
	}

	pv.Dispatch = &servingv1alpha1.GateDispatch{
		State:       servingv1alpha1.GateDispatchAttempted,
		AttemptedAt: new(metav1.NewTime(now.Truncate(time.Second))),
		SHA:         tag,
	}
	step.dispatch = &dispatchPlan{token: token, sha: tag}
	logf.FromContext(ctx).Info("The gate latched the promote dispatch; it is sent once the latch is written",
		"interval", pv.Interval, "sha", tag)
	if deferred && len(st.Records) > 0 {
		// The audit pointer (#60): a deferred verdict is dispatched on the
		// clean run whole at this boundary, which a canary replaced since its
		// interval, and its tag moved, makes another pod's than the one
		// deferred. The verdict's record stays its first interval's (D43), so
		// the Event names the run the dispatch rests on: the interval this
		// boundary ended, the ring's last record, and the tag the canary pod
		// read runs (canaryRuns).
		return []gateEvent{{corev1.EventTypeNormal, eventReasonGatePromoteDeferralEnded, fmt.Sprintf(
			"the deferred promote verdict of interval %d is dispatched on the clean run interval %d completed: %d "+
				"consecutive clean intervals, read from a canary pod running tag %s",
			pv.Interval, st.Records[len(st.Records)-1].Index, st.CleanRun, tag)}}
	}
	return nil
}

// sendDispatch sends the dispatch whose latch the status write before it has
// just landed — never before, so a crash between the two leaves the latch and
// no run (D43) — and records GitHub's answer in a second status write. Nothing
// sends it again, whatever that write does: its Event is recorded either way,
// and a latch the write leaves at Attempted is the human's to read in Actions.
func (r *ServingDeploymentReconciler) sendDispatch(
	ctx context.Context, sd *servingv1alpha1.ServingDeployment, plan *dispatchPlan,
) error {
	code, sendErr := r.dispatcher.dispatch(ctx, plan.token, plan.sha)
	base := sd.DeepCopy()
	pv := sd.Status.Gate.PromoteVerdict
	pv.Dispatch.StatusCode = int32(code)
	log := logf.FromContext(ctx)
	if sendErr == nil {
		pv.Dispatch.State = servingv1alpha1.GateDispatchDispatched
		log.Info("The gate dispatched the promote verdict", "interval", pv.Interval, "sha", plan.sha, "statusCode", code)
		r.Recorder.Event(sd, corev1.EventTypeNormal, eventReasonGatePromoteDispatched, fmt.Sprintf(
			"interval %d completed %d consecutive clean intervals, the promote verdict: in %s the gate dispatched "+
				"%s on %s with sha %s at %s, and GitHub accepted it (%d); the pipeline promotes, its guards and "+
				"approval unchanged, and its close-window patch closes the window; the run's URL is recorded by hand",
			pv.Interval, gate.PromoteAfter, servingv1alpha1.GateModeActPromote, deployWorkflow, deployRef, plan.sha,
			stamp(pv.Dispatch.AttemptedAt.Time), code))
	} else {
		pv.Dispatch.State = servingv1alpha1.GateDispatchFailed
		log.Info("The gate's promote dispatch failed; it is not retried", "interval", pv.Interval, "sha", plan.sha,
			"error", sendErr.Error())
		r.Recorder.Event(sd, corev1.EventTypeWarning, eventReasonGatePromoteDispatchFailed, fmt.Sprintf(
			"the dispatch of %s with sha %s for the promote verdict of interval %d failed: %v; it is not retried, "+
				"the verdict stays latched, and a human actuates it", deployWorkflow, plan.sha, pv.Interval, sendErr))
	}
	if err := r.Status().Patch(ctx, sd, client.MergeFrom(base)); err != nil {
		return fmt.Errorf("recording the promote dispatch's answer on servingdeployment/%s: %w", sd.Name, err)
	}
	return nil
}

// verifyOwners checks the owner chain of the two pods the boundary b read, the
// stable's and the canary's, against the Deployments sd controls (ownerChain).
func (r *ServingDeploymentReconciler) verifyOwners(
	ctx context.Context, sd *servingv1alpha1.ServingDeployment, b scrape.Boundary,
) error {
	stable := &appsv1.Deployment{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: sd.Namespace, Name: stableDeploymentName}, stable); err != nil {
		return fmt.Errorf("deployment/%s is unreadable: %w", stableDeploymentName, err)
	}
	if err := ownerChain(b.StablePod, stable, sd); err != nil {
		return fmt.Errorf("the stable pod read: %w", err)
	}
	canary, err := r.getCanary(ctx, sd)
	if err != nil {
		return err
	}
	if err := ownerChain(b.CanaryPod, canary, sd); err != nil {
		return fmt.Errorf("the canary pod read: %w", err)
	}
	return nil
}

// ownerChain checks that pod, read at a boundary, is a pod of dep, a
// Deployment sd controls (#108, N1): pod's controller is a ReplicaSet, named
// as the Deployment controller names dep's, dep's name and pod's own
// pod-template-hash label, and dep's selector matches pod.
//
// Its limit, stated plainly: the ReplicaSet itself is never read. The Role
// grants nothing on ReplicaSets, and this slice adds no verb, so the chain's
// middle link is inferred from the Deployment controller's naming convention,
// not verified: that the ReplicaSet exists, carries the uid pod's reference
// names, and is controlled by dep. A pod created by hand in mlobs, its labels
// and owner reference forged, passes; whoever can create pods there can do
// that. A get on replicasets would close it — the ReplicaSet's uid against
// the reference, and its controller against dep — and is a grant for a
// D-number to rule, not this check.
func ownerChain(pod *corev1.Pod, dep *appsv1.Deployment, sd *servingv1alpha1.ServingDeployment) error {
	switch {
	case pod == nil:
		return errors.New("no one Running pod was selected")
	case dep == nil:
		return fmt.Errorf("pod/%s's Deployment does not exist", pod.Name)
	case !metav1.IsControlledBy(dep, sd):
		return fmt.Errorf("deployment/%s is not controlled by servingdeployment/%s", dep.Name, sd.Name)
	}
	ref := metav1.GetControllerOf(pod)
	if ref == nil {
		return fmt.Errorf("pod/%s has no controller", pod.Name)
	}
	gv, err := schema.ParseGroupVersion(ref.APIVersion)
	if err != nil || gv.Group != appsv1.GroupName || ref.Kind != "ReplicaSet" {
		return fmt.Errorf("pod/%s is controlled by %s %s, not by a ReplicaSet", pod.Name, ref.APIVersion, ref.Kind)
	}
	hash := pod.Labels[appsv1.DefaultDeploymentUniqueLabelKey]
	if hash == "" {
		return fmt.Errorf("pod/%s has no %s label", pod.Name, appsv1.DefaultDeploymentUniqueLabelKey)
	}
	if strings.Contains(hash, "-") {
		// No hash the Deployment controller writes has one, and one would let
		// api-canary's ReplicaSet pass as api's under the hash canary-<hash>.
		return fmt.Errorf("pod/%s's %s label %q has a '-', which no generated hash has",
			pod.Name, appsv1.DefaultDeploymentUniqueLabelKey, hash)
	}
	if want := dep.Name + "-" + hash; ref.Name != want {
		return fmt.Errorf("pod/%s is controlled by replicaset/%s, not deployment/%s's replicaset/%s",
			pod.Name, ref.Name, dep.Name, want)
	}
	selector, err := metav1.LabelSelectorAsSelector(dep.Spec.Selector)
	if err != nil || !selector.Matches(labels.Set(pod.Labels)) {
		return fmt.Errorf("pod/%s does not match deployment/%s's selector", pod.Name, dep.Name)
	}
	return nil
}

// canaryRuns checks that pod, the canary pod a boundary read, is the one image
// runs: its api container's image is image, the one the dispatch's tag
// renders, and it is not being deleted. The reconcile drives
// deployment/api-canary to spec.canaryImageTag before the boundary is read,
// and a Recreate rollout leaves the old pod Running, and so the one read,
// while it terminates; ownerChain passes it, since its ReplicaSet is still the
// Deployment's. Without this check, a tag pair changed in a boundary's
// reconcile would dispatch the new tag on what the old image reported (#121,
// F1). The image compared is the pod spec's, which the ReplicaSet stamped from
// the template the tag rendered; the container status's is the runtime's
// report, which may name another tag of the same image, and is not compared.
func canaryRuns(pod *corev1.Pod, image string) error {
	switch {
	case pod == nil:
		return errors.New("no one Running canary pod was selected")
	case pod.DeletionTimestamp != nil:
		return fmt.Errorf("the canary pod read, pod/%s, is being deleted: what it reported may be an image "+
			"the canary no longer runs", pod.Name)
	}
	for _, c := range pod.Spec.Containers {
		if c.Name != apiContainerName {
			continue
		}
		if c.Image != image {
			return fmt.Errorf("the canary pod read, pod/%s, runs %s, not %s, the image spec.canaryImageTag "+
				"renders: what it reported is not the dispatched tag's", pod.Name, c.Image, image)
		}
		return nil
	}
	return fmt.Errorf("the canary pod read, pod/%s, has no container named %q", pod.Name, apiContainerName)
}
