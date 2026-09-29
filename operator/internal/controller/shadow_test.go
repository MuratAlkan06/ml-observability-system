package controller

import (
	"context"
	"fmt"
	"sync"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv1 "k8s.io/api/autoscaling/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/config"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	servingv1alpha1 "github.com/MuratAlkan06/ml-observability-system/operator/api/v1alpha1"
)

// consumerStatefulSetName is the stack's other StatefulSet: readable by the
// operator, never writable by it (D33 addendum of O2).
const consumerStatefulSetName = "consumer"

func getStatefulSet(namespace, name string) *appsv1.StatefulSet {
	sts := &appsv1.StatefulSet{}
	Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, sts)).To(Succeed())
	return sts
}

// createStatefulSet creates a one-replica StatefulSet, the stack's shape for
// both of its StatefulSets, with the status the StatefulSet controller
// reports once that pod runs.
func createStatefulSet(namespace, name string) *appsv1.StatefulSet {
	labels := map[string]string{"app": name}
	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Labels: labels},
		Spec: appsv1.StatefulSetSpec{
			Replicas:    new(int32(1)),
			ServiceName: name,
			Selector:    &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{Containers: []corev1.Container{{
					Name:  name,
					Image: "example.com/" + name + ":test",
				}}},
			},
		},
	}
	Expect(k8sClient.Create(ctx, sts)).To(Succeed())
	markStatefulSetObserved(namespace, name)
	return getStatefulSet(namespace, name)
}

// markStatefulSetObserved writes the status the StatefulSet controller
// reports once the pods for the current scale exist, or are gone. envtest runs
// no StatefulSet controller, so without this the status never moves.
func markStatefulSetObserved(namespace, name string) {
	sts := getStatefulSet(namespace, name)
	n := replicasOf(sts.Spec.Replicas)
	sts.Status = appsv1.StatefulSetStatus{
		ObservedGeneration: sts.Generation,
		Replicas:           n,
		ReadyReplicas:      n,
		CurrentReplicas:    n,
		UpdatedReplicas:    n,
		AvailableReplicas:  n,
	}
	Expect(k8sClient.Status().Update(ctx, sts)).To(Succeed())
}

// handScaleShadow sets the shadow scorer's scale the way `kubectl scale`
// does: on the scale subresource, as an admin — not as the operator.
func handScaleShadow(namespace string, replicas int32) {
	target := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: shadowStatefulSetName}}
	body := fmt.Sprintf(`{"spec":{"replicas":%d}}`, replicas)
	Expect(k8sClient.SubResource("scale").Patch(ctx, target, client.RawPatch(types.MergePatchType, []byte(body)),
		client.WithSubResourceBody(&autoscalingv1.Scale{}))).To(Succeed())
}

// createWindowServingDeployment creates a ServingDeployment whose spec
// requests a canary window, the shape a host-side patch gives it (D32).
func createWindowServingDeployment(namespace string) *servingv1alpha1.ServingDeployment {
	sd := &servingv1alpha1.ServingDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: servingDeploymentName, Namespace: namespace},
		Spec: servingv1alpha1.ServingDeploymentSpec{
			ImageTag:       validImageTag,
			CanaryImageTag: canaryImageTag,
			CanaryReplicas: 1,
		},
	}
	Expect(k8sClient.Create(ctx, sd)).To(Succeed())
	return sd
}

// setCanarySpec rewrites sd's canary fields the way a host-side patch does.
func setCanarySpec(sd *servingv1alpha1.ServingDeployment, tag string, replicas int32) {
	latest := getServingDeployment(sd)
	latest.Spec.CanaryImageTag = tag
	latest.Spec.CanaryReplicas = replicas
	Expect(k8sClient.Update(ctx, latest)).To(Succeed())
}

// canaryImageTag is the tag a test window's canary runs; any 40-hex value
// other than the stable's would do.
const canaryImageTag = "fedcba9876543210fedcba9876543210fedcba98"

// clientWrite is one write a reconciler sent through a recordingClient.
type clientWrite struct {
	verb, kind, name, subresource, body string
}

// recordingClient passes every call through to the API server and records
// each write: the verb, the object's kind and name, the subresource if any,
// and the patch body if it is a patch.
type recordingClient struct {
	mu     sync.Mutex
	writes []clientWrite
}

func (rc *recordingClient) record(verb string, obj client.Object, subresource string, patch client.Patch) {
	gvk, err := apiutil.GVKForObject(obj, k8sClient.Scheme())
	Expect(err).NotTo(HaveOccurred())
	w := clientWrite{verb: verb, kind: gvk.Kind, name: obj.GetName(), subresource: subresource}
	if patch != nil {
		data, err := patch.Data(obj)
		Expect(err).NotTo(HaveOccurred())
		w.body = string(data)
	}
	rc.mu.Lock()
	defer rc.mu.Unlock()
	rc.writes = append(rc.writes, w)
}

// statefulSetWrites returns the recorded writes to any StatefulSet.
func (rc *recordingClient) statefulSetWrites() []clientWrite {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	var out []clientWrite
	for _, w := range rc.writes {
		if w.kind == "StatefulSet" {
			out = append(out, w)
		}
	}
	return out
}

// newRecordingReconciler returns a reconciler whose client records every
// write it sends, so a spec can assert on the whole set of writes rather than
// on the state they left behind.
func newRecordingReconciler() (*ServingDeploymentReconciler, *recordingClient) {
	underlying, err := client.NewWithWatch(cfg, client.Options{Scheme: k8sClient.Scheme()})
	Expect(err).NotTo(HaveOccurred())
	rc := &recordingClient{}
	intercepted := interceptor.NewClient(underlying, interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			rc.record("create", obj, "", nil)
			return c.Create(ctx, obj, opts...)
		},
		Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			rc.record("update", obj, "", nil)
			return c.Update(ctx, obj, opts...)
		},
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch,
			opts ...client.PatchOption) error {
			rc.record("patch", obj, "", patch)
			return c.Patch(ctx, obj, patch, opts...)
		},
		Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			rc.record("delete", obj, "", nil)
			return c.Delete(ctx, obj, opts...)
		},
		SubResourceCreate: func(ctx context.Context, c client.Client, sub string, obj, subObj client.Object,
			opts ...client.SubResourceCreateOption) error {
			rc.record("create", obj, sub, nil)
			return c.SubResource(sub).Create(ctx, obj, subObj, opts...)
		},
		SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object,
			opts ...client.SubResourceUpdateOption) error {
			rc.record("update", obj, sub, nil)
			return c.SubResource(sub).Update(ctx, obj, opts...)
		},
		SubResourcePatch: func(ctx context.Context, c client.Client, sub string, obj client.Object, patch client.Patch,
			opts ...client.SubResourcePatchOption) error {
			rc.record("patch", obj, sub, patch)
			return c.SubResource(sub).Patch(ctx, obj, patch, opts...)
		},
	})
	return &ServingDeploymentReconciler{
		Client:   intercepted,
		Scheme:   k8sClient.Scheme(),
		Recorder: record.NewFakeRecorder(64),
	}, rc
}

// expectOnlyShadowScaleWrites asserts that every StatefulSet write rc saw was
// a patch of statefulsets/shadow-scorer/scale carrying one of the two literal
// bodies, and returns those bodies in order.
func expectOnlyShadowScaleWrites(rc *recordingClient) []string {
	writes := rc.statefulSetWrites()
	bodies := make([]string, 0, len(writes))
	for _, w := range writes {
		Expect(w.verb).To(Equal("patch"), "a StatefulSet write other than a patch: %+v", w)
		Expect(w.subresource).To(Equal("scale"), "a StatefulSet write outside the scale subresource: %+v", w)
		Expect(w.name).To(Equal(shadowStatefulSetName), "a StatefulSet write to another object: %+v", w)
		Expect(w.body).To(BeElementOf(shadowPauseBody, shadowResumeBody), "a scale body other than 0 or 1: %+v", w)
		bodies = append(bodies, w.body)
	}
	return bodies
}

var _ = Describe("Pausing the shadow scorer", func() {
	It("has exactly two scale writes: the literal merge patches for replicas 0 and 1", func() {
		for pause, want := range map[bool]string{
			true:  `{"spec":{"replicas":0}}`,
			false: `{"spec":{"replicas":1}}`,
		} {
			patch := shadowScalePatch(pause)
			Expect(patch.Type()).To(Equal(types.MergePatchType))
			data, err := patch.Data(nil)
			Expect(err).NotTo(HaveOccurred())
			Expect(string(data)).To(Equal(want))
			Expect(shadowReplicas(pause)).To(BeNumerically("==", map[bool]int{true: 0, false: 1}[pause]))
		}
	})

	It("pauses the shadow for a requested window through statefulsets/shadow-scorer/scale alone, "+
		"and reports ShadowPaused only once the StatefulSet is observed at zero with no pods", func() {
		ns := newTestNamespace()
		createStatefulSet(ns, shadowStatefulSetName)
		consumer := createStatefulSet(ns, consumerStatefulSetName)
		sd := createWindowServingDeployment(ns)
		r, writes := newRecordingReconciler()

		By("the first reconcile writes scale 0 and does not claim the pause")
		Expect(reconcileServingDeployment(r, sd)).To(Succeed())
		shadow := getStatefulSet(ns, shadowStatefulSetName)
		Expect(shadow.Spec.Replicas).To(HaveValue(BeZero()))
		paused := condition(getServingDeployment(sd), servingv1alpha1.ConditionShadowPaused)
		Expect(paused.Status).To(Equal(metav1.ConditionFalse))
		Expect(paused.Reason).To(Equal(servingv1alpha1.ReasonShadowRunning))

		By("while the status still describes the previous scale, it stays False and nothing more is written")
		Expect(shadow.Status.ObservedGeneration).To(BeNumerically("<", shadow.Generation))
		Expect(reconcileServingDeployment(r, sd)).To(Succeed())
		Expect(condition(getServingDeployment(sd), servingv1alpha1.ConditionShadowPaused).Status).
			To(Equal(metav1.ConditionFalse))

		By("while a pod is still counted — terminating — it stays False")
		shadow = getStatefulSet(ns, shadowStatefulSetName)
		shadow.Status.ObservedGeneration = shadow.Generation
		shadow.Status.Replicas = 1
		Expect(k8sClient.Status().Update(ctx, shadow)).To(Succeed())
		Expect(reconcileServingDeployment(r, sd)).To(Succeed())
		Expect(condition(getServingDeployment(sd), servingv1alpha1.ConditionShadowPaused).Status).
			To(Equal(metav1.ConditionFalse))

		By("observed at zero with no pods, it turns True")
		markStatefulSetObserved(ns, shadowStatefulSetName)
		Expect(reconcileServingDeployment(r, sd)).To(Succeed())
		paused = condition(getServingDeployment(sd), servingv1alpha1.ConditionShadowPaused)
		Expect(paused.Status).To(Equal(metav1.ConditionTrue))
		Expect(paused.Reason).To(Equal(servingv1alpha1.ReasonShadowScaledToZero))

		Expect(expectOnlyShadowScaleWrites(writes)).To(Equal([]string{shadowPauseBody}))
		Expect(getStatefulSet(ns, consumerStatefulSetName).ResourceVersion).To(Equal(consumer.ResourceVersion),
			"the consumer must never be written")
	})

	It("resumes the shadow once no window is requested, and puts a hand scale back in either direction", func() {
		ns := newTestNamespace()
		createStatefulSet(ns, shadowStatefulSetName)
		createStatefulSet(ns, consumerStatefulSetName)
		sd := createServingDeployment(ns)
		r, writes := newRecordingReconciler()

		By("steady: a hand scale to 0 is put back to 1")
		handScaleShadow(ns, 0)
		Expect(reconcileServingDeployment(r, sd)).To(Succeed())
		Expect(getStatefulSet(ns, shadowStatefulSetName).Spec.Replicas).To(HaveValue(BeEquivalentTo(1)))

		By("a hand scale above 1 is put back to 1 as well")
		handScaleShadow(ns, 3)
		Expect(reconcileServingDeployment(r, sd)).To(Succeed())
		Expect(getStatefulSet(ns, shadowStatefulSetName).Spec.Replicas).To(HaveValue(BeEquivalentTo(1)))

		By("a requested window: a hand scale back to 1 is put back to 0")
		setCanarySpec(sd, canaryImageTag, 1)
		Expect(reconcileServingDeployment(r, sd)).To(Succeed())
		Expect(getStatefulSet(ns, shadowStatefulSetName).Spec.Replicas).To(HaveValue(BeZero()))
		handScaleShadow(ns, 1)
		Expect(reconcileServingDeployment(r, sd)).To(Succeed())
		Expect(getStatefulSet(ns, shadowStatefulSetName).Spec.Replicas).To(HaveValue(BeZero()))

		Expect(expectOnlyShadowScaleWrites(writes)).To(Equal(
			[]string{shadowResumeBody, shadowResumeBody, shadowPauseBody, shadowPauseBody}))
	})

	It("reports ShadowNotFound, and writes to no StatefulSet, when the namespace holds no shadow scorer", func() {
		ns := newTestNamespace()
		consumer := createStatefulSet(ns, consumerStatefulSetName)
		sd := createWindowServingDeployment(ns)
		r, writes := newRecordingReconciler()

		Expect(reconcileServingDeployment(r, sd)).To(Succeed())

		paused := condition(getServingDeployment(sd), servingv1alpha1.ConditionShadowPaused)
		Expect(paused.Status).To(Equal(metav1.ConditionFalse))
		Expect(paused.Reason).To(Equal(servingv1alpha1.ReasonShadowNotFound))
		Expect(writes.statefulSetWrites()).To(BeEmpty())
		Expect(getStatefulSet(ns, consumerStatefulSetName).ResourceVersion).To(Equal(consumer.ResourceVersion))
	})

	It("puts a hand scale back through the StatefulSet watch of a running manager, with no direct Reconcile call", func() {
		ns := newTestNamespace()
		createStatefulSet(ns, shadowStatefulSetName)
		startManager(ns)

		sd := createServingDeployment(ns)
		Eventually(func(g Gomega) {
			latest := &servingv1alpha1.ServingDeployment{}
			g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(sd), latest)).To(Succeed())
			g.Expect(latest.Status.ObservedGeneration).To(Equal(latest.Generation))
		}).WithTimeout(managerPollTimeout).WithPolling(managerPollInterval).Should(Succeed())

		handScaleShadow(ns, 0)
		Eventually(func(g Gomega) {
			sts := &appsv1.StatefulSet{}
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: shadowStatefulSetName}, sts)).
				To(Succeed())
			g.Expect(sts.Spec.Replicas).To(HaveValue(BeEquivalentTo(1)))
		}).WithTimeout(managerPollTimeout).WithPolling(managerPollInterval).Should(Succeed())
	})
})

// startManager runs the reconciler under a manager whose cache covers
// namespace, until the spec ends.
func startManager(namespace string) {
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:     k8sClient.Scheme(),
		Metrics:    metricsserver.Options{BindAddress: "0"},
		Cache:      cache.Options{DefaultNamespaces: map[string]cache.Config{namespace: {}}},
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
}
