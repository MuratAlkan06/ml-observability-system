package controller

import (
	"context"

	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	servingv1alpha1 "github.com/MuratAlkan06/ml-observability-system/operator/api/v1alpha1"
)

// ServingDeploymentReconciler reconciles a ServingDeployment object
type ServingDeploymentReconciler struct {
	client.Client
	Scheme *runtime.Scheme
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

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the ServingDeployment object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.24.1/pkg/reconcile
func (r *ServingDeploymentReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	_ = logf.FromContext(ctx)

	// TODO(user): your logic here

	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *ServingDeploymentReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&servingv1alpha1.ServingDeployment{}).
		Named("servingdeployment").
		Complete(r)
}
