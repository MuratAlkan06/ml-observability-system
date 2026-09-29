package controller

import (
	"context"
	"errors"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	servingv1alpha1 "github.com/MuratAlkan06/ml-observability-system/operator/api/v1alpha1"
)

// Event reasons the reconciler records on a ServingDeployment.
const (
	eventReasonAdopted        = "Adopted"
	eventReasonCreated        = "Created"
	eventReasonImageUpdated   = "ImageUpdated"
	eventReasonAdoptionFailed = "AdoptionFailed"
)

// ServingDeploymentReconciler reconciles a ServingDeployment object
type ServingDeploymentReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// Recorder must write core/v1 Events: those are what the operator's Role
	// grants create and patch on (docs/PLAN.md D33).
	Recorder record.EventRecorder
}

// The operator's whole grant: one namespaced Role in mlobs, no ClusterRole and
// no write on CRDs (docs/PLAN.md D33). controller-gen renders these markers
// into deploy/k3s/manifests/02-operator-rbac.yaml; `make verify-manifests`
// fails if the two disagree.
// +kubebuilder:rbac:groups=serving.mlobs.dev,namespace=mlobs,resources=servingdeployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=serving.mlobs.dev,namespace=mlobs,resources=servingdeployments/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=serving.mlobs.dev,namespace=mlobs,resources=servingdeployments/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps,namespace=mlobs,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",namespace=mlobs,resources=events,verbs=create;patch
// +kubebuilder:rbac:groups=coordination.k8s.io,namespace=mlobs,resources=leases,verbs=get;list;watch;create;update;patch;delete

// Reconcile drives deployment/api, the stable api Deployment in the
// ServingDeployment's namespace, to the image spec.imageTag names, and records
// the generation it did so for in status.observedGeneration. It reconciles the
// stable path only; the canary window lands in O2 (docs/PLAN.md D31, D32).
func (r *ServingDeploymentReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	sd := &servingv1alpha1.ServingDeployment{}
	if err := r.Get(ctx, req.NamespacedName, sd); err != nil {
		// A ServingDeployment that is gone needs nothing from here:
		// deployment/api goes with it, garbage-collected through its
		// ownerReference (D36).
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !sd.DeletionTimestamp.IsZero() {
		// While the ServingDeployment is being deleted, a deployment/api the
		// garbage collector has already removed must stay removed: D36's
		// rollback waits on exactly that removal.
		return ctrl.Result{}, nil
	}

	if _, err := r.reconcileStable(ctx, sd); err != nil {
		return ctrl.Result{}, err
	}

	if sd.Status.ObservedGeneration != sd.Generation {
		base := sd.DeepCopy()
		sd.Status.ObservedGeneration = sd.Generation
		if err := r.Status().Patch(ctx, sd, client.MergeFrom(base)); err != nil {
			return ctrl.Result{}, fmt.Errorf("writing the status of servingdeployment/%s: %w", sd.Name, err)
		}
	}
	return ctrl.Result{}, nil
}

// adoptionError reports a deployment/api the operator cannot take over: one
// controlled by another owner, or one without the api container.
type adoptionError struct{ reason string }

func (e *adoptionError) Error() string {
	return fmt.Sprintf("cannot adopt deployment/%s: %s", stableDeploymentName, e.reason)
}

// reconcileStable makes deployment/api exist in the ServingDeployment's
// namespace, be controlled by it, and run stableImage(spec.imageTag). It
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
	image := stableImage(sd.Spec.ImageTag)

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
// adopted, so a drifted image is put back without waiting for a resync.
func (r *ServingDeploymentReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&servingv1alpha1.ServingDeployment{}).
		Owns(&appsv1.Deployment{}).
		Named("servingdeployment").
		Complete(r)
}
