package controller

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	servingv1alpha1 "github.com/MuratAlkan06/ml-observability-system/operator/api/v1alpha1"
)

// previousImageTag stands for the SHA the pipeline deployed before the
// ServingDeployment existed; validImageTag is the one the spec asks for.
const previousImageTag = "89abcdef0123456789abcdef0123456789abcdef"

// servingDeploymentName is the CR's name in these specs, the one apply.sh
// renders.
const servingDeploymentName = "api"

// Bounds for the one spec that waits on a running manager: a readiness poll
// inside a deadline, never a timing assertion (PRINCIPLES.md §5).
const (
	managerPollTimeout  = 20 * time.Second
	managerPollInterval = 100 * time.Millisecond
)

// newTestNamespace creates a namespace for one spec. deployment/api is a fixed
// name, so specs sharing a namespace would share the Deployment too. envtest
// runs no namespace controller, so the namespaces are left in place.
func newTestNamespace() string {
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "o1-"}}
	Expect(k8sClient.Create(ctx, ns)).To(Succeed())
	return ns.Name
}

func newTestReconciler() (*ServingDeploymentReconciler, *record.FakeRecorder) {
	recorder := record.NewFakeRecorder(32)
	return &ServingDeploymentReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Recorder: recorder}, recorder
}

// createServingDeployment creates a ServingDeployment asking for validImageTag.
func createServingDeployment(namespace string) *servingv1alpha1.ServingDeployment {
	sd := &servingv1alpha1.ServingDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: servingDeploymentName, Namespace: namespace},
		Spec:       servingv1alpha1.ServingDeploymentSpec{ImageTag: validImageTag},
	}
	Expect(k8sClient.Create(ctx, sd)).To(Succeed())
	return sd
}

func reconcileServingDeployment(r *ServingDeploymentReconciler, sd *servingv1alpha1.ServingDeployment) error {
	_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(sd)})
	return err
}

func stableKey(namespace string) types.NamespacedName {
	return types.NamespacedName{Namespace: namespace, Name: stableDeploymentName}
}

func getStable(namespace string) *appsv1.Deployment {
	dep := &appsv1.Deployment{}
	Expect(k8sClient.Get(ctx, stableKey(namespace), dep)).To(Succeed())
	return dep
}

func getServingDeployment(sd *servingv1alpha1.ServingDeployment) *servingv1alpha1.ServingDeployment {
	latest := &servingv1alpha1.ServingDeployment{}
	Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(sd), latest)).To(Succeed())
	return latest
}

// createLiveAPI creates deployment/api the way the pipeline left it before the
// operator existed, and returns it as the API server stored it. The pod
// template carries a restartedAt annotation the operator's own shape does not,
// so a template rebuilt from newStableDeployment would show.
func createLiveAPI(namespace, tag string) *appsv1.Deployment {
	dep := newStableDeployment(namespace, stableImage(tag))
	dep.Spec.Template.Annotations = map[string]string{"kubectl.kubernetes.io/restartedAt": "2026-09-28T00:00:00Z"}
	Expect(k8sClient.Create(ctx, dep)).To(Succeed())
	return getStable(namespace)
}

// expectControlledBy asserts dep carries sd's controller ownerReference with
// blockOwnerDeletion set.
func expectControlledBy(dep *appsv1.Deployment, sd *servingv1alpha1.ServingDeployment) {
	owner := metav1.GetControllerOf(dep)
	Expect(owner).NotTo(BeNil())
	Expect(owner.UID).To(Equal(sd.UID))
	Expect(owner.Kind).To(Equal("ServingDeployment"))
	Expect(owner.BlockOwnerDeletion).To(HaveValue(BeTrue()))
}

var _ = Describe("Reconciling the stable api Deployment", func() {
	It("adopts deployment/api in place: same object and name, pod template untouched but for the api image", func() {
		ns := newTestNamespace()
		before := createLiveAPI(ns, previousImageTag)
		sd := createServingDeployment(ns)
		r, recorder := newTestReconciler()

		Expect(reconcileServingDeployment(r, sd)).To(Succeed())

		after := getStable(ns)
		Expect(after.Name).To(Equal(stableDeploymentName))
		Expect(after.UID).To(Equal(before.UID), "adoption must keep the object, not replace it")
		expectControlledBy(after, sd)
		Expect(after.Labels).To(Equal(before.Labels))

		want := before.Spec.DeepCopy()
		want.Template.Spec.Containers[0].Image = stableImage(validImageTag)
		Expect(after.Spec).To(Equal(*want))

		Expect(recorder.Events).To(Receive(ContainSubstring("Normal Adopted adopted deployment/api in place")))
		Expect(recorder.Events).To(Receive(ContainSubstring(
			"Normal ImageUpdated deployment/api image " + stableImage(previousImageTag) + " -> " + stableImage(validImageTag))))
	})

	It("adopts a deployment/api already at spec.imageTag without changing its spec, so no rollout starts", func() {
		ns := newTestNamespace()
		before := createLiveAPI(ns, validImageTag)
		sd := createServingDeployment(ns)
		r, recorder := newTestReconciler()

		Expect(reconcileServingDeployment(r, sd)).To(Succeed())

		after := getStable(ns)
		expectControlledBy(after, sd)
		Expect(after.Spec).To(Equal(before.Spec))
		Expect(after.Generation).To(Equal(before.Generation), "an ownerReference alone must not bump the generation")
		Expect(recorder.Events).To(Receive(ContainSubstring("Normal Adopted")))
		Expect(recorder.Events).NotTo(Receive())
	})

	It("creates deployment/api in the 20-api.yaml shape, controlled by the ServingDeployment, when none exists", func() {
		ns := newTestNamespace()
		sd := createServingDeployment(ns)
		r, recorder := newTestReconciler()

		Expect(reconcileServingDeployment(r, sd)).To(Succeed())

		dep := getStable(ns)
		expectControlledBy(dep, sd)
		Expect(dep.Labels).To(Equal(map[string]string{"app": "api"}))
		Expect(dep.Spec.Strategy.Type).To(Equal(appsv1.RecreateDeploymentStrategyType))
		Expect(dep.Spec.Template.Spec.Containers).To(HaveLen(1))
		Expect(dep.Spec.Template.Spec.Containers[0].Image).To(Equal(stableImage(validImageTag)))
		Expect(recorder.Events).To(Receive(Equal(
			"Normal Created created deployment/api at image " + stableImage(validImageTag))))
	})

	It("puts a drifted api image back to spec.imageTag", func() {
		ns := newTestNamespace()
		sd := createServingDeployment(ns)
		r, recorder := newTestReconciler()
		Expect(reconcileServingDeployment(r, sd)).To(Succeed())
		Expect(recorder.Events).To(Receive(ContainSubstring("Normal Created")))

		drifted := getStable(ns)
		drifted.Spec.Template.Spec.Containers[0].Image = "example.com/elsewhere:latest"
		Expect(k8sClient.Update(ctx, drifted)).To(Succeed())

		Expect(reconcileServingDeployment(r, sd)).To(Succeed())

		Expect(getStable(ns).Spec.Template.Spec.Containers[0].Image).To(Equal(stableImage(validImageTag)))
		Expect(recorder.Events).To(Receive(Equal("Normal ImageUpdated deployment/api image " +
			"example.com/elsewhere:latest -> " + stableImage(validImageTag))))
	})

	It("converges status.observedGeneration on metadata.generation, again after a spec.imageTag change", func() {
		ns := newTestNamespace()
		sd := createServingDeployment(ns)
		r, _ := newTestReconciler()

		Expect(reconcileServingDeployment(r, sd)).To(Succeed())
		sd = getServingDeployment(sd)
		Expect(sd.Generation).To(Equal(int64(1)))
		Expect(sd.Status.ObservedGeneration).To(Equal(sd.Generation))

		sd.Spec.ImageTag = previousImageTag
		Expect(k8sClient.Update(ctx, sd)).To(Succeed())
		sd = getServingDeployment(sd)
		Expect(sd.Generation).To(Equal(int64(2)))
		Expect(sd.Status.ObservedGeneration).To(Equal(int64(1)), "not observed until reconciled")

		Expect(reconcileServingDeployment(r, sd)).To(Succeed())
		sd = getServingDeployment(sd)
		Expect(sd.Status.ObservedGeneration).To(Equal(int64(2)))
		Expect(getStable(ns).Spec.Template.Spec.Containers[0].Image).To(Equal(stableImage(previousImageTag)))
	})

	It("refuses to adopt a deployment/api another controller owns, and leaves it untouched", func() {
		ns := newTestNamespace()
		live := newStableDeployment(ns, stableImage(previousImageTag))
		live.OwnerReferences = []metav1.OwnerReference{{
			APIVersion: "example.com/v1",
			Kind:       "Widget",
			Name:       "someone-else",
			UID:        types.UID("11111111-1111-1111-1111-111111111111"),
			Controller: new(true),
		}}
		Expect(k8sClient.Create(ctx, live)).To(Succeed())
		before := getStable(ns)
		sd := createServingDeployment(ns)
		r, recorder := newTestReconciler()

		Expect(reconcileServingDeployment(r, sd)).To(BeAssignableToTypeOf(&adoptionError{}))

		after := getStable(ns)
		Expect(after.ResourceVersion).To(Equal(before.ResourceVersion), "a refused adoption must not write")
		Expect(recorder.Events).To(Receive(ContainSubstring("Warning AdoptionFailed cannot adopt deployment/api")))

		sd = getServingDeployment(sd)
		Expect(sd.Status.ObservedGeneration).To(Equal(sd.Generation))
		ready := meta.FindStatusCondition(sd.Status.Conditions, servingv1alpha1.ConditionReady)
		Expect(ready).NotTo(BeNil())
		Expect(ready.Status).To(Equal(metav1.ConditionFalse))
		Expect(ready.Reason).To(Equal(servingv1alpha1.ReasonAdoptionFailed))
		Expect(ready.Message).To(ContainSubstring("already owned by another Widget controller someone-else"))
	})

	It("does not recreate deployment/api while the ServingDeployment is being deleted", func() {
		ns := newTestNamespace()
		sd := createServingDeployment(ns)
		r, _ := newTestReconciler()
		Expect(reconcileServingDeployment(r, sd)).To(Succeed())

		// A finalizer holds the ServingDeployment in deletion the way a
		// foreground delete does while its dependents are collected.
		const hold = "test.mlobs.dev/hold"
		sd = getServingDeployment(sd)
		controllerutil.AddFinalizer(sd, hold)
		Expect(k8sClient.Update(ctx, sd)).To(Succeed())
		DeferCleanup(func() {
			latest := getServingDeployment(sd)
			controllerutil.RemoveFinalizer(latest, hold)
			Expect(k8sClient.Update(ctx, latest)).To(Succeed())
		})
		Expect(k8sClient.Delete(ctx, sd)).To(Succeed())
		// envtest runs no garbage collector; delete what it would have.
		Expect(k8sClient.Delete(ctx, getStable(ns))).To(Succeed())

		Expect(reconcileServingDeployment(r, sd)).To(Succeed())

		err := k8sClient.Get(ctx, stableKey(ns), &appsv1.Deployment{})
		Expect(apierrors.IsNotFound(err)).To(BeTrue(), "got %v", err)
	})

	It("puts a drifted image back, and turns Ready True once the rollout is Available, "+
		"through the watches of a running manager with no direct Reconcile call", func() {
		ns := newTestNamespace()
		mgr, err := ctrl.NewManager(cfg, ctrl.Options{
			Scheme:     k8sClient.Scheme(),
			Metrics:    metricsserver.Options{BindAddress: "0"},
			Cache:      cache.Options{DefaultNamespaces: map[string]cache.Config{ns: {}}},
			Controller: config.Controller{SkipNameValidation: new(true)},
		})
		Expect(err).NotTo(HaveOccurred())
		Expect((&ServingDeploymentReconciler{
			Client:   mgr.GetClient(),
			Scheme:   mgr.GetScheme(),
			Recorder: &record.FakeRecorder{},
		}).SetupWithManager(mgr)).To(Succeed())

		mgrCtx, stop := context.WithCancel(ctx)
		stopped := make(chan error, 1)
		go func() {
			defer GinkgoRecover()
			stopped <- mgr.Start(mgrCtx)
		}()
		DeferCleanup(func() {
			stop()
			Eventually(stopped).WithTimeout(managerPollTimeout).Should(Receive(BeNil()))
		})

		sd := createServingDeployment(ns)
		Eventually(func(g Gomega) {
			latest := &servingv1alpha1.ServingDeployment{}
			g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(sd), latest)).To(Succeed())
			g.Expect(latest.Status.ObservedGeneration).To(Equal(latest.Generation))
		}).WithTimeout(managerPollTimeout).WithPolling(managerPollInterval).Should(Succeed())

		drifted := getStable(ns)
		drifted.Spec.Template.Spec.Containers[0].Image = "example.com/elsewhere:latest"
		Expect(k8sClient.Update(ctx, drifted)).To(Succeed())

		Eventually(func(g Gomega) {
			dep := &appsv1.Deployment{}
			g.Expect(k8sClient.Get(ctx, stableKey(ns), dep)).To(Succeed())
			g.Expect(dep.Spec.Template.Spec.Containers[0].Image).To(Equal(stableImage(validImageTag)))
		}).WithTimeout(managerPollTimeout).WithPolling(managerPollInterval).Should(Succeed())

		// Ready depends on the Deployment's status, so a status change on the
		// owned Deployment must reach the reconciler through Owns() as well.
		markRolledOut(ns)
		Eventually(func(g Gomega) {
			latest := &servingv1alpha1.ServingDeployment{}
			g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(sd), latest)).To(Succeed())
			g.Expect(meta.IsStatusConditionTrue(latest.Status.Conditions, servingv1alpha1.ConditionReady)).To(BeTrue())
		}).WithTimeout(managerPollTimeout).WithPolling(managerPollInterval).Should(Succeed())
	})
})
