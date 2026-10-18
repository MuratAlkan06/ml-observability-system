package controller

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	servingv1alpha1 "github.com/MuratAlkan06/ml-observability-system/operator/api/v1alpha1"
	"github.com/MuratAlkan06/ml-observability-system/operator/internal/scrape"
)

// Event reasons the reconciler records on a ServingDeployment.
const (
	eventReasonAdopted          = "Adopted"
	eventReasonCreated          = "Created"
	eventReasonImageUpdated     = "ImageUpdated"
	eventReasonAdoptionFailed   = "AdoptionFailed"
	eventReasonShadowScaled     = "ShadowScaled"
	eventReasonCanaryCreated    = "CanaryCreated"
	eventReasonCanaryUpdated    = "CanaryUpdated"
	eventReasonCanaryScaledDown = "CanaryScaledDown"
	eventReasonWindowExpired    = "WindowExpired"
)

// conflictRequeueAfter is how soon a reconcile whose status write the
// optimistic lock refused runs again, if nothing brings it sooner: the write
// that conflicted it brings its own watch event, so this is a floor, well
// inside the half interval a gate boundary's read has to land in.
const conflictRequeueAfter = time.Second

// ServingDeploymentReconciler reconciles a ServingDeployment object
type ServingDeploymentReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// Recorder must write core/v1 Events: those are what the operator's Role
	// grants create and patch on (docs/PLAN.md D33).
	Recorder record.EventRecorder
	// Now is the reconciler's clock, read once per reconcile, and again after
	// a gate boundary's reads: to judge a promote dispatch's floor and to
	// schedule the next boundary from when they ended; nil means time.Now.
	// Tests set it, so the 45-minute window TTL and the gate's grid are
	// exercised without waiting on them.
	Now func() time.Time
	// ImagePrefix is the registry and owner of the api images the operator
	// writes, as <ImagePrefix>/mlobs-api:<tag>; empty means
	// DefaultImagePrefix. The operator's --image-prefix sets it.
	ImagePrefix string

	// scraper reads the window's two pods at the gate's boundaries.
	// SetupWithManager sets it to a scrape.Scraper whose Reader is the
	// manager's uncached APIReader and nothing else, so its /metrics reads go
	// through package scrape's own client (D41); this package's tests set a
	// scripted one.
	scraper boundaryReader
	// gateInterval is the gate's grid interval; zero means gateGridInterval.
	// SetupWithManager sets it from the test-only knob, gateIntervalEnv.
	gateInterval time.Duration
	// baselines are the gate's last boundary reads, in memory alone (D45).
	baselines gateBaselines
	// dispatcher makes ActPromote's GitHub calls (D43). SetupWithManager sets
	// it to read the token from dispatchTokenPath and call githubAPI, unless
	// the test-only knob, githubAPIEnv, moves the API; this package's tests
	// set one of their own. nil has no token, so ActPromote is TokenAbsent.
	dispatcher *dispatcher
}

// now reads the reconciler's clock.
func (r *ServingDeploymentReconciler) now() time.Time {
	if r.Now == nil {
		return time.Now()
	}
	return r.Now()
}

// apiImage renders tag as an api image reference under the reconciler's
// prefix.
func (r *ServingDeploymentReconciler) apiImage(tag string) string {
	prefix := r.ImagePrefix
	if prefix == "" {
		prefix = DefaultImagePrefix
	}
	return apiImage(prefix, tag)
}

// The operator's whole grant: one namespaced Role in mlobs, no ClusterRole and
// no write on CRDs (docs/PLAN.md D33). controller-gen renders these markers
// into deploy/k3s/manifests/02-operator-rbac.yaml; `make verify-manifests`
// fails if the two disagree.
// +kubebuilder:rbac:groups=serving.mlobs.dev,namespace=mlobs,resources=servingdeployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=serving.mlobs.dev,namespace=mlobs,resources=servingdeployments/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=serving.mlobs.dev,namespace=mlobs,resources=servingdeployments/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps,namespace=mlobs,resources=deployments,verbs=get;list;watch;create;update;patch;delete
//
// The shadow pause (D33 addendum of O2): read on the namespace's StatefulSets,
// because RBAC cannot name-scope list and watch, and one write — patch on the
// scale subresource of shadow-scorer alone. No update, no get on the scale,
// no write on any StatefulSet itself: the operator reads the scale from the
// StatefulSet and can change nothing but that one object's replica count.
// +kubebuilder:rbac:groups=apps,namespace=mlobs,resources=statefulsets,verbs=get;list;watch
// +kubebuilder:rbac:groups=apps,namespace=mlobs,resources=statefulsets/scale,resourceNames=shadow-scorer,verbs=patch
// +kubebuilder:rbac:groups="",namespace=mlobs,resources=events,verbs=create;patch
// +kubebuilder:rbac:groups=coordination.k8s.io,namespace=mlobs,resources=leases,verbs=get;list;watch;create;update;patch;delete
//
// Metric acquisition (D41): get and list on the namespace's pods, and no
// watch. The window's two api pods are read at interval boundaries only,
// through the manager's uncached APIReader (internal/scrape), so no informer
// runs and none needs the watch verb. The one grant serves three reads — the
// pods by label, the canary's Ready time, and OOMKilled from container status
// — and each pod's /metrics is read at its pod IP, not through pods/proxy,
// which this Role does not grant. RBAC cannot name-scope list, so the read
// covers the namespace's pods.
// +kubebuilder:rbac:groups="",namespace=mlobs,resources=pods,verbs=get;list

// Reconcile drives deployment/api, the stable api Deployment in the
// ServingDeployment's namespace, to the image spec.imageTag names; decides the
// canary window from the spec, the CanaryActive condition on record and the
// clock; and takes the next step toward that window's state — the canary
// Deployment, deployment/api-canary, up or down, and the shadow scorer at 0 or
// 1, in the order reconcileWindow keeps. With spec.gate set and a window open
// it runs the gate, which reads the window's pods only at the boundaries of
// its interval grid and records each interval in status.gate (observe); a line
// tripped at an acting level rolls the window back, latched as an expired
// window is (D47). It then writes the status for the generation it acted on:
// the Ready, CanaryActive and ShadowPaused conditions,
// status.observedGeneration and status.gate. The status is written on failure
// too, so a refused adoption shows on the resource and not only in the
// operator's log. That write is locked to the ServingDeployment as read, so
// one decided from a stale read conflicts, is logged at Info and requeued, and
// is decided again (#121, F4; #129; #60); the Events of the transitions it
// carries are recorded, and a promote dispatch it latches is sent, only after
// it lands, and GitHub's answer is written after (D43). While a window is open
// the request is requeued for the earlier of its deadline and the gate's next
// boundary.
// The spec is never written (docs/PLAN.md D31, D32, D37 and their O2 rulings;
// D42, D43, D47).
func (r *ServingDeploymentReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	sd := &servingv1alpha1.ServingDeployment{}
	if err := r.Get(ctx, req.NamespacedName, sd); err != nil {
		// A ServingDeployment that is gone needs nothing from here:
		// deployment/api goes with it, garbage-collected through its
		// ownerReference (D36).
		r.baselines.forget(req.NamespacedName)
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !sd.DeletionTimestamp.IsZero() {
		// While the ServingDeployment is being deleted, a deployment/api the
		// garbage collector has already removed must stay removed: D36's
		// rollback waits on exactly that removal.
		r.baselines.forget(req.NamespacedName)
		return ctrl.Result{}, nil
	}

	now := r.now()
	dep, stableErr := r.reconcileStable(ctx, sd)
	prev := meta.FindStatusCondition(sd.Status.Conditions, servingv1alpha1.ConditionCanaryActive)
	w := decideWindow(sd.Spec, prev, now)
	// The window opens on this reconcile, where the gate samples its acting
	// level (#119).
	opened := w.open && (prev == nil || prev.Status != metav1.ConditionTrue)
	if w.reason == servingv1alpha1.ReasonBreachRolledBack && sd.Status.Gate != nil {
		w.rollback = sd.Status.Gate.RollbackVerdict
	}
	// The expiry's Event is held, as the gate's are, until the status write
	// that records the close lands (finishGate): a reconcile whose write
	// conflicts records none, and the one that decides again records it once
	// (#60, V1-S7).
	var held []gateEvent
	if w.expiredNow {
		logf.FromContext(ctx).Info("The canary window reached its TTL; closing it", "ttl", canaryWindowTTL)
		held = append(held, gateEvent{corev1.EventTypeNormal, eventReasonWindowExpired, fmt.Sprintf(
			"the canary window reached its %s TTL; closing it and leaving the spec's canary fields to the "+
				"close-window patch", canaryWindowTTL)})
	}
	shadow, windowErr := r.reconcileWindow(ctx, sd, dep, &w)
	if windowErr != nil {
		w.progress = "blocked: " + windowErr.Error()
	}

	base := sd.DeepCopy()
	setConditions(sd, dep, stableErr, r.apiImage(sd.Spec.ImageTag), w, shadow, now)
	sd.Status.ObservedGeneration = sd.Generation
	step, gateErr := r.observe(ctx, sd, w.open, opened, now)
	if step.rollBack {
		// The gate tripped a line at an acting level (D47). The window is
		// latched closed here, in the status write below, before anything in
		// the cluster moves; from the next reconcile, which that write brings,
		// decideWindow's latch restores steady exactly as for an expired
		// window. A failed write moves nothing, and is retried.
		w = window{reason: servingv1alpha1.ReasonBreachRolledBack, rollback: sd.Status.Gate.RollbackVerdict}
		setConditions(sd, dep, stableErr, r.apiImage(sd.Spec.ImageTag), w, shadow, now)
	}
	reconcileErr := joinErrors(stableErr, windowErr, gateErr)
	if !equality.Semantic.DeepEqual(base.Status, sd.Status) {
		// The write is optimistically locked to the object this reconcile
		// read, a cached read that can lag a write the API server holds: a
		// ServingDeployment written since, its tag pair above all, conflicts
		// it, and the retry decides again from what was written (#121, F4).
		// A promote dispatch's latch rides it; if it fails, nothing is sent.
		// Every write is locked, not only the latch's and the window's
		// transitions: a merge patch carries the conditions list whole, so
		// any write from a stale read puts back the CanaryActive it read,
		// over an open or a latch it never saw. Unlocked, a stale reconcile
		// could move an open window's anchor, leave status.gate the evidence
		// of another anchor for the gate to re-arm at Observe, or close a
		// window the next reconcile then opens again at the level spec.gate
		// asks by then (#129; D42, D47).
		patch := client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})
		if err := r.Status().Patch(ctx, sd, patch); err != nil {
			if apierrors.IsConflict(err) && reconcileErr == nil {
				// Refused by the lock, the write is a stale read's, not a
				// failure: its status, its held Events, its kept read and
				// any dispatch it latched go with it, and the write that
				// conflicted it brings the reconcile that decides again. A
				// workload write made before it stands until that reconcile
				// puts it back: at worst a replaced canary pod, an
				// unevaluable interval and a warmup, never a promote (#60,
				// R-D). It is logged at Info and requeued, not returned, so
				// a lagging cache logs no Reconciler error and takes no
				// backoff; a conflict beside another error is returned with
				// it, as before (#60, V1-S7).
				logf.FromContext(ctx).Info("The status write conflicted with a newer ServingDeployment; "+
					"deciding again from it", "error", err.Error())
				return ctrl.Result{RequeueAfter: conflictRequeueAfter}, nil
			}
			return ctrl.Result{}, errors.Join(reconcileErr,
				fmt.Errorf("writing the status of servingdeployment/%s: %w", sd.Name, err))
		}
	}
	r.finishGate(sd, step)
	r.recordHeld(sd, held)
	if step.dispatch != nil {
		reconcileErr = joinErrors(reconcileErr, r.sendDispatch(ctx, sd, step.dispatch))
	}
	if reconcileErr != nil {
		return ctrl.Result{}, reconcileErr
	}
	if w.open {
		// The TTL is enforced by this requeue, not by an event: nothing in
		// the cluster changes at the deadline. Neither does anything at a
		// gate boundary, which competes with it for the earlier requeue.
		after := w.deadline.Sub(now)
		if !step.next.IsZero() {
			after = min(after, max(step.next.Sub(r.now()), time.Millisecond))
		}
		return ctrl.Result{RequeueAfter: after}, nil
	}
	return ctrl.Result{}, nil
}

// joinErrors is errors.Join, except that a single non-nil error is returned
// as itself rather than wrapped, so its type survives for callers and tests.
func joinErrors(errs ...error) error {
	var nonNil []error
	for _, err := range errs {
		if err != nil {
			nonNil = append(nonNil, err)
		}
	}
	if len(nonNil) == 1 {
		return nonNil[0]
	}
	return errors.Join(nonNil...)
}

// adoptionError reports a deployment/api the operator cannot take over: one
// controlled by another owner, or one without the api container.
type adoptionError struct{ reason string }

func (e *adoptionError) Error() string {
	return fmt.Sprintf("cannot adopt deployment/%s: %s", stableDeploymentName, e.reason)
}

// reconcileStable makes deployment/api exist in the ServingDeployment's
// namespace, be controlled by it, and run r.apiImage(spec.imageTag). It
// returns the Deployment as last written or read.
//
// An existing deployment/api is adopted in place: the ServingDeployment's
// controller ownerReference is added and nothing else about it changes except
// the api container's image, so a Deployment already at spec.imageTag is
// adopted without a rollout (D31). An absent one is created from
// newStableDeployment.
func (r *ServingDeploymentReconciler) reconcileStable(
	ctx context.Context, sd *servingv1alpha1.ServingDeployment,
) (*appsv1.Deployment, error) {
	image := r.apiImage(sd.Spec.ImageTag)

	dep := &appsv1.Deployment{}
	err := r.Get(ctx, types.NamespacedName{Namespace: sd.Namespace, Name: stableDeploymentName}, dep)
	if apierrors.IsNotFound(err) {
		return r.createStable(ctx, sd, image)
	}
	if err != nil {
		return nil, fmt.Errorf("getting deployment/%s: %w", stableDeploymentName, err)
	}

	base := dep.DeepCopy()
	container := apiContainer(dep)
	if container == nil {
		return nil, r.adoptionFailed(sd, fmt.Sprintf("it has no container named %q", apiContainerName))
	}
	adopting := !metav1.IsControlledBy(dep, sd)
	if adopting {
		if err := controllerutil.SetControllerReference(sd, dep, r.Scheme); err != nil {
			if owned, ok := errors.AsType[*controllerutil.AlreadyOwnedError](err); ok {
				return nil, r.adoptionFailed(sd, owned.Error())
			}
			return nil, fmt.Errorf("setting the controller reference on deployment/%s: %w", stableDeploymentName, err)
		}
	}
	previous := container.Image
	if !adopting && previous == image {
		return dep, nil
	}
	container.Image = image

	// A strategic merge patch carries only what changed: the ownerReference,
	// merged into the list by uid, and the api container's image, merged by
	// container name. The rest of the Deployment, its pod template above all,
	// is not part of the request. The optimistic lock turns a concurrent write
	// into a conflict and a retry instead of a lost update.
	if err := r.Patch(ctx, dep, client.StrategicMergeFrom(base, client.MergeFromWithOptimisticLock{})); err != nil {
		return nil, fmt.Errorf("patching deployment/%s: %w", stableDeploymentName, err)
	}

	log := logf.FromContext(ctx)
	if adopting {
		log.Info("Adopted the stable Deployment in place", "deployment", stableDeploymentName)
		r.Recorder.Eventf(sd, corev1.EventTypeNormal, eventReasonAdopted,
			"adopted deployment/%s in place; name and pod template kept", stableDeploymentName)
	}
	if previous != image {
		log.Info("Set the stable image", "deployment", stableDeploymentName, "from", previous, "to", image)
		r.Recorder.Eventf(sd, corev1.EventTypeNormal, eventReasonImageUpdated,
			"deployment/%s image %s -> %s", stableDeploymentName, previous, image)
	}
	return dep, nil
}

// createStable creates deployment/api, controlled by sd, at image.
func (r *ServingDeploymentReconciler) createStable(
	ctx context.Context, sd *servingv1alpha1.ServingDeployment, image string,
) (*appsv1.Deployment, error) {
	dep := newStableDeployment(sd.Namespace, image)
	if err := controllerutil.SetControllerReference(sd, dep, r.Scheme); err != nil {
		return nil, fmt.Errorf("setting the controller reference on deployment/%s: %w", stableDeploymentName, err)
	}
	if err := r.Create(ctx, dep); err != nil {
		return nil, fmt.Errorf("creating deployment/%s: %w", stableDeploymentName, err)
	}
	logf.FromContext(ctx).Info("Created the stable Deployment", "deployment", stableDeploymentName, "image", image)
	r.Recorder.Eventf(sd, corev1.EventTypeNormal, eventReasonCreated,
		"created deployment/%s at image %s", stableDeploymentName, image)
	return dep, nil
}

// adoptionFailed records why deployment/api cannot be adopted and returns it
// as an error, so the request is retried with backoff.
func (r *ServingDeploymentReconciler) adoptionFailed(sd *servingv1alpha1.ServingDeployment, reason string) error {
	err := &adoptionError{reason: reason}
	r.Recorder.Event(sd, corev1.EventTypeWarning, eventReasonAdoptionFailed, err.Error())
	return err
}

// apiContainer returns the api container of dep's pod template, or nil.
func apiContainer(dep *appsv1.Deployment) *corev1.Container {
	containers := dep.Spec.Template.Spec.Containers
	for i := range containers {
		if containers[i].Name == apiContainerName {
			return &containers[i]
		}
	}
	return nil
}

// SetupWithManager sets up the controller with the Manager. Owns() enqueues
// the owning ServingDeployment on any change to deployment/api once it is
// adopted, so a drifted image is put back without waiting for a resync. The
// shadow scorer is not owned — the operator holds no write on it beyond its
// scale — so a change to it enqueues every ServingDeployment in its namespace
// instead: a hand scale is put back, and a pause is seen through to its pods.
//
// It also wires the gate's reads. The Scraper gets the manager's APIReader
// and nothing else: the pods are read uncached, with no informer and so no
// watch (D41), and a Scraper made outside package scrape has no client to set,
// so its /metrics reads go through scrape's own, which carries none of the
// operator's credentials. The pods are not watched here either: the gate reads
// them at its boundaries, which the reconciler's requeue keeps. And the grid
// interval is the O4 interval unless the test-only knob, gateIntervalEnv,
// shortens it; a knob outside its bounds stops the operator here.
//
// It wires ActPromote's dispatch the same way: a dispatcher that reads its
// token from the Secret's mounted file at each use, with no client to set,
// so its GitHub calls go through dispatchClient and nothing of the manager's.
// The API is GitHub's unless the test-only knob, githubAPIEnv, points it at
// the e2e's stand-in; a malformed knob stops the operator here. An absent
// Secret stops nothing: ActPromote runs as TokenAbsent.
func (r *ServingDeploymentReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.scraper == nil {
		r.scraper = &scrape.Scraper{Reader: mgr.GetAPIReader()}
	}
	if r.dispatcher == nil {
		api, err := githubAPIFrom(os.LookupEnv)
		if err != nil {
			return err
		}
		if api != githubAPI {
			mgr.GetLogger().Info("The promote dispatch's GitHub API is moved by its test-only knob", "env", githubAPIEnv,
				"api", api)
		}
		r.dispatcher = &dispatcher{baseURL: api, tokenPath: dispatchTokenPath}
	}
	if r.gateInterval == 0 {
		interval, err := gateIntervalFrom(os.LookupEnv)
		if err != nil {
			return err
		}
		if interval != gateGridInterval {
			mgr.GetLogger().Info("The gate's grid is shortened by its test-only knob", "env", gateIntervalEnv,
				"interval", interval.String())
		}
		r.gateInterval = interval
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&servingv1alpha1.ServingDeployment{}).
		Owns(&appsv1.Deployment{}).
		Watches(&appsv1.StatefulSet{},
			handler.EnqueueRequestsFromMapFunc(r.servingDeploymentsInNamespace),
			builder.WithPredicates(predicate.NewPredicateFuncs(func(obj client.Object) bool {
				return obj.GetName() == shadowStatefulSetName
			}))).
		Named("servingdeployment").
		Complete(r)
}

// servingDeploymentsInNamespace maps an object to a request for every
// ServingDeployment in its namespace.
func (r *ServingDeploymentReconciler) servingDeploymentsInNamespace(
	ctx context.Context, obj client.Object,
) []reconcile.Request {
	list := &servingv1alpha1.ServingDeploymentList{}
	if err := r.List(ctx, list, client.InNamespace(obj.GetNamespace())); err != nil {
		logf.FromContext(ctx).Error(err, "Listing ServingDeployments to enqueue for a shadow scorer change",
			"namespace", obj.GetNamespace())
		return nil
	}
	requests := make([]reconcile.Request, 0, len(list.Items))
	for i := range list.Items {
		requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&list.Items[i])})
	}
	return requests
}
