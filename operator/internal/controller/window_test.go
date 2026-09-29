package controller

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	servingv1alpha1 "github.com/MuratAlkan06/ml-observability-system/operator/api/v1alpha1"
)

// laterCanaryImageTag and thirdCanaryImageTag are further 40-hex tags: a
// canary tag changed mid-window, and one set on an expired window.
const (
	laterCanaryImageTag = "00112233445566778899aabbccddeeff00112233"
	thirdCanaryImageTag = "33221100ffeeddccbbaa99887766554433221100"
)

// windowStart is the fake clock's reading when a spec's window opens.
var windowStart = time.Date(2026, time.September, 29, 12, 0, 0, 0, time.UTC)

// testClock is a settable clock for the reconciler's Now.
type testClock struct{ t time.Time }

func (c *testClock) now() time.Time { return c.t }

// newClockedReconciler is newRecordingReconciler on a fake clock set to
// windowStart.
func newClockedReconciler() (*ServingDeploymentReconciler, *recordingClient, *testClock) {
	r, writes := newRecordingReconciler()
	clock := &testClock{t: windowStart}
	r.Now = clock.now
	return r, writes, clock
}

func reconcileResult(r *ServingDeploymentReconciler, sd *servingv1alpha1.ServingDeployment) reconcile.Result {
	result, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(sd)})
	Expect(err).NotTo(HaveOccurred())
	return result
}

// getCanaryDeployment returns deployment/api-canary, or nil if there is none.
func getCanaryDeployment(namespace string) *appsv1.Deployment {
	dep := &appsv1.Deployment{}
	err := k8sClient.Get(ctx, types.NamespacedName{Namespace: namespace, Name: canaryDeploymentName}, dep)
	if apierrors.IsNotFound(err) {
		return nil
	}
	Expect(err).NotTo(HaveOccurred())
	return dep
}

// markCanaryObserved writes the status the Deployment controller reports for
// deployment/api-canary once its pods match its spec, with terminating pods
// still counted apart. envtest runs no Deployment controller.
func markCanaryObserved(namespace string, terminating int32) {
	dep := getCanaryDeployment(namespace)
	Expect(dep).NotTo(BeNil())
	n := replicasOf(dep.Spec.Replicas)
	dep.Status = rolledOutStatus(dep.Generation)
	dep.Status.Replicas, dep.Status.UpdatedReplicas = n, n
	dep.Status.ReadyReplicas, dep.Status.AvailableReplicas = n, n
	dep.Status.TerminatingReplicas = new(terminating)
	Expect(k8sClient.Status().Update(ctx, dep)).To(Succeed())
}

// steadyStack creates the stack's StatefulSets and a ServingDeployment in a
// fresh namespace, and reconciles it to steady with deployment/api rolled
// out and Available.
func steadyStack() (string, *servingv1alpha1.ServingDeployment, *ServingDeploymentReconciler, *recordingClient,
	*testClock) {
	ns := newTestNamespace()
	createStatefulSet(ns, shadowStatefulSetName)
	createStatefulSet(ns, consumerStatefulSetName)
	sd := createServingDeployment(ns)
	r, writes, clock := newClockedReconciler()
	Expect(reconcileServingDeployment(r, sd)).To(Succeed())
	markRolledOut(ns)
	Expect(reconcileServingDeployment(r, sd)).To(Succeed())
	return ns, sd, r, writes, clock
}

// openWindowFully opens a window on a steady stack and walks it to the
// window state: shadow observed at 0, canary created and observed running.
func openWindowFully(ns string, sd *servingv1alpha1.ServingDeployment, r *ServingDeploymentReconciler) {
	setCanarySpec(sd, canaryImageTag, 1)
	Expect(reconcileServingDeployment(r, sd)).To(Succeed())
	markStatefulSetObserved(ns, shadowStatefulSetName)
	Expect(reconcileServingDeployment(r, sd)).To(Succeed())
	markCanaryObserved(ns, 0)
	Expect(reconcileServingDeployment(r, sd)).To(Succeed())
	Expect(getCanaryDeployment(ns).Spec.Replicas).To(HaveValue(BeEquivalentTo(1)))
}

// indexOf returns the position of the first recorded write matching verb,
// kind and name, failing the spec if there is none.
func indexOf(rc *recordingClient, verb, kind, name, body string) int {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	for i, w := range rc.writes {
		if w.verb == verb && w.kind == kind && w.name == name && (body == "" || w.body == body) {
			return i
		}
	}
	Fail("no recorded " + verb + " of " + kind + "/" + name)
	return -1
}

func expectShadowReplicas(ns string, want int32) {
	Expect(getStatefulSet(ns, shadowStatefulSetName).Spec.Replicas).To(HaveValue(Equal(want)))
}

func expectCanaryReplicas(ns string, want int32) {
	canary := getCanaryDeployment(ns)
	Expect(canary).NotTo(BeNil())
	Expect(canary.Spec.Replicas).To(HaveValue(Equal(want)))
}

var _ = Describe("The canary window", func() {
	It("opens in order: the shadow is scaled to 0 and observed gone before deployment/api-canary exists", func() {
		ns, sd, r, writes, _ := steadyStack()
		setCanarySpec(sd, canaryImageTag, 1)

		By("the first reconcile pauses the shadow, opens the window and starts its clock, and creates no canary")
		Expect(reconcileServingDeployment(r, sd)).To(Succeed())
		expectShadowReplicas(ns, 0)
		Expect(getCanaryDeployment(ns)).To(BeNil())
		active := condition(getServingDeployment(sd), servingv1alpha1.ConditionCanaryActive)
		Expect(active.Status).To(Equal(metav1.ConditionTrue))
		Expect(active.Reason).To(Equal(servingv1alpha1.ReasonWindowOpen))
		Expect(active.LastTransitionTime.Time).To(BeTemporally("==", windowStart))

		By("no canary while the shadow's status is stale, or while it still counts a pod")
		Expect(reconcileServingDeployment(r, sd)).To(Succeed())
		Expect(getCanaryDeployment(ns)).To(BeNil())
		shadow := getStatefulSet(ns, shadowStatefulSetName)
		shadow.Status.ObservedGeneration = shadow.Generation
		shadow.Status.Replicas = 1
		Expect(k8sClient.Status().Update(ctx, shadow)).To(Succeed())
		Expect(reconcileServingDeployment(r, sd)).To(Succeed())
		Expect(getCanaryDeployment(ns)).To(BeNil())

		By("with the shadow observed gone, the canary is created at the canary tag, behind the shared selector")
		markStatefulSetObserved(ns, shadowStatefulSetName)
		Expect(reconcileServingDeployment(r, sd)).To(Succeed())
		canary := getCanaryDeployment(ns)
		Expect(canary).NotTo(BeNil())
		Expect(canary.Spec.Replicas).To(HaveValue(BeEquivalentTo(1)))
		Expect(canary.Spec.Template.Spec.Containers[0].Image).To(Equal(stableImage(canaryImageTag)))
		Expect(canary.Spec.Template.Labels).To(And(Equal(canaryLabels()), HaveKeyWithValue("role", "canary")))
		Expect(metav1.IsControlledBy(canary, getServingDeployment(sd))).To(BeTrue())
		latest := getServingDeployment(sd)
		Expect(condition(latest, servingv1alpha1.ConditionShadowPaused).Status).To(Equal(metav1.ConditionTrue))
		Expect(condition(latest, servingv1alpha1.ConditionCanaryActive).LastTransitionTime.Time).
			To(BeTemporally("==", windowStart), "the clock does not move while the window opens")

		Expect(indexOf(writes, "patch", "StatefulSet", shadowStatefulSetName, shadowPauseBody)).
			To(BeNumerically("<", indexOf(writes, "create", "Deployment", canaryDeploymentName, "")))
		expectOnlyShadowScaleWrites(writes)
	})

	It("closes in order: deployment/api-canary is scaled to 0 and observed gone before the shadow returns to 1", func() {
		ns, sd, r, writes, _ := steadyStack()
		openWindowFully(ns, sd, r)

		By("the close-window patch's shape closes the window and scales the canary to 0; the shadow stays at 0")
		setCanarySpec(sd, "", 0)
		Expect(reconcileServingDeployment(r, sd)).To(Succeed())
		expectCanaryReplicas(ns, 0)
		expectShadowReplicas(ns, 0)
		active := condition(getServingDeployment(sd), servingv1alpha1.ConditionCanaryActive)
		Expect(active.Status).To(Equal(metav1.ConditionFalse))
		Expect(active.Reason).To(Equal(servingv1alpha1.ReasonNoCanary))

		By("while the canary's status is stale, or a canary pod is still terminating, the shadow stays at 0")
		Expect(reconcileServingDeployment(r, sd)).To(Succeed())
		expectShadowReplicas(ns, 0)
		markCanaryObserved(ns, 1)
		Expect(reconcileServingDeployment(r, sd)).To(Succeed())
		expectShadowReplicas(ns, 0)

		By("with the canary observed gone, the shadow returns to 1")
		markCanaryObserved(ns, 0)
		Expect(reconcileServingDeployment(r, sd)).To(Succeed())
		expectShadowReplicas(ns, 1)
		Expect(condition(getServingDeployment(sd), servingv1alpha1.ConditionShadowPaused).Status).
			To(Equal(metav1.ConditionFalse))

		Expect(indexOf(writes, "patch", "Deployment", canaryDeploymentName, "")).
			To(BeNumerically("<", indexOf(writes, "patch", "StatefulSet", shadowStatefulSetName, shadowResumeBody)))
		expectOnlyShadowScaleWrites(writes)
	})

	It("promotes stable-first: deployment/api rolls to the promoted tag and is Available "+
		"before the canary goes to 0, and the canary is gone before the shadow resumes", func() {
		ns, sd, r, _, _ := steadyStack()
		openWindowFully(ns, sd, r)

		By("the pipeline deploys the canary's SHA: the CR apply moves imageTag, the close-window patch clears the rest")
		latest := getServingDeployment(sd)
		latest.Spec.ImageTag = canaryImageTag
		latest.Spec.CanaryImageTag = ""
		latest.Spec.CanaryReplicas = 0
		Expect(k8sClient.Update(ctx, latest)).To(Succeed())

		Expect(reconcileServingDeployment(r, sd)).To(Succeed())
		Expect(getStable(ns).Spec.Template.Spec.Containers[0].Image).To(Equal(stableImage(canaryImageTag)))
		expectCanaryReplicas(ns, 1)

		By("the canary keeps serving for as long as the stable rollout is incomplete")
		Expect(reconcileServingDeployment(r, sd)).To(Succeed())
		expectCanaryReplicas(ns, 1)
		Expect(condition(getServingDeployment(sd), servingv1alpha1.ConditionCanaryActive).Message).
			To(ContainSubstring("keeps serving until deployment/api is Available"))

		By("the stable Available at the promoted tag, the canary goes to 0; the shadow waits for its pods")
		markRolledOut(ns)
		Expect(reconcileServingDeployment(r, sd)).To(Succeed())
		expectCanaryReplicas(ns, 0)
		expectShadowReplicas(ns, 0)

		markCanaryObserved(ns, 0)
		Expect(reconcileServingDeployment(r, sd)).To(Succeed())
		expectShadowReplicas(ns, 1)
	})

	It("expires 45 minutes after CanaryActive turned True: a mid-window canaryImageTag change "+
		"does not restart the clock, and the operator restores steady without writing the spec", func() {
		ns, sd, r, writes, clock := steadyStack()
		setCanarySpec(sd, canaryImageTag, 1)
		Expect(reconcileResult(r, sd).RequeueAfter).To(Equal(canaryWindowTTL),
			"the reconciler schedules itself for the deadline")
		markStatefulSetObserved(ns, shadowStatefulSetName)
		Expect(reconcileServingDeployment(r, sd)).To(Succeed())
		markCanaryObserved(ns, 0)

		By("20 minutes in, the canary tag changes: the canary follows it, and the clock does not move")
		clock.t = windowStart.Add(20 * time.Minute)
		setCanarySpec(sd, laterCanaryImageTag, 1)
		Expect(reconcileResult(r, sd).RequeueAfter).To(Equal(25 * time.Minute))
		Expect(getCanaryDeployment(ns).Spec.Template.Spec.Containers[0].Image).
			To(Equal(stableImage(laterCanaryImageTag)))
		active := condition(getServingDeployment(sd), servingv1alpha1.ConditionCanaryActive)
		Expect(active.Status).To(Equal(metav1.ConditionTrue))
		Expect(active.LastTransitionTime.Time).To(BeTemporally("==", windowStart))
		markCanaryObserved(ns, 0)
		specBefore := getServingDeployment(sd)

		By("one second before the deadline, the window is still open")
		clock.t = windowStart.Add(canaryWindowTTL - time.Second)
		Expect(reconcileResult(r, sd).RequeueAfter).To(Equal(time.Second))
		expectCanaryReplicas(ns, 1)

		By("at the deadline, CanaryActive goes False with WindowExpired and the canary goes to 0")
		clock.t = windowStart.Add(canaryWindowTTL)
		Expect(reconcileResult(r, sd).RequeueAfter).To(BeZero())
		active = condition(getServingDeployment(sd), servingv1alpha1.ConditionCanaryActive)
		Expect(active.Status).To(Equal(metav1.ConditionFalse))
		Expect(active.Reason).To(Equal(servingv1alpha1.ReasonWindowExpired))
		Expect(active.LastTransitionTime.Time).To(BeTemporally("==", windowStart.Add(canaryWindowTTL)))
		expectCanaryReplicas(ns, 0)
		expectShadowReplicas(ns, 0)
		Expect(drainEvents(r)).To(ContainElement(ContainSubstring("Normal WindowExpired")))

		By("the canary observed gone, the shadow returns to 1; the spec still holds the window's fields")
		markCanaryObserved(ns, 0)
		Expect(reconcileServingDeployment(r, sd)).To(Succeed())
		expectShadowReplicas(ns, 1)
		after := getServingDeployment(sd)
		Expect(after.Generation).To(Equal(specBefore.Generation))
		Expect(after.Spec).To(Equal(specBefore.Spec))
		Expect(after.Spec.CanaryImageTag).To(Equal(laterCanaryImageTag))
		Expect(after.Spec.CanaryReplicas).To(BeEquivalentTo(1))
		for _, w := range writes.writes {
			if w.kind == "ServingDeployment" {
				Expect(w.subresource).To(Equal("status"), "the operator writes a ServingDeployment's status only: %+v", w)
			}
		}
	})

	It("latches an expired window: editing its canary fields does not reopen it, "+
		"the cleared shape releases it, and the next window starts a new clock", func() {
		ns, sd, r, writes, clock := steadyStack()
		openWindowFully(ns, sd, r)
		clock.t = windowStart.Add(canaryWindowTTL)
		Expect(reconcileServingDeployment(r, sd)).To(Succeed())
		markCanaryObserved(ns, 0)
		Expect(reconcileServingDeployment(r, sd)).To(Succeed())
		expectShadowReplicas(ns, 1)
		pausesBefore := len(writes.statefulSetWrites())

		By("a new canary tag on the expired window: still latched, still steady")
		clock.t = windowStart.Add(50 * time.Minute)
		setCanarySpec(sd, thirdCanaryImageTag, 1)
		Expect(reconcileResult(r, sd).RequeueAfter).To(BeZero())
		Expect(condition(getServingDeployment(sd), servingv1alpha1.ConditionCanaryActive).Reason).
			To(Equal(servingv1alpha1.ReasonWindowExpired))
		expectCanaryReplicas(ns, 0)
		expectShadowReplicas(ns, 1)

		By("replicas 0 with the tag still set is not the cleared shape: still latched")
		setCanarySpec(sd, thirdCanaryImageTag, 0)
		Expect(reconcileServingDeployment(r, sd)).To(Succeed())
		Expect(condition(getServingDeployment(sd), servingv1alpha1.ConditionCanaryActive).Reason).
			To(Equal(servingv1alpha1.ReasonWindowExpired))

		By("reopening without passing through the cleared shape: still latched")
		setCanarySpec(sd, thirdCanaryImageTag, 1)
		Expect(reconcileServingDeployment(r, sd)).To(Succeed())
		Expect(condition(getServingDeployment(sd), servingv1alpha1.ConditionCanaryActive).Status).
			To(Equal(metav1.ConditionFalse))
		Expect(writes.statefulSetWrites()).To(HaveLen(pausesBefore), "a latched window pauses nothing")

		By("the cleared shape releases the latch")
		setCanarySpec(sd, "", 0)
		Expect(reconcileServingDeployment(r, sd)).To(Succeed())
		Expect(condition(getServingDeployment(sd), servingv1alpha1.ConditionCanaryActive).Reason).
			To(Equal(servingv1alpha1.ReasonNoCanary))

		By("a new window opens on a new clock")
		clock.t = windowStart.Add(55 * time.Minute)
		setCanarySpec(sd, thirdCanaryImageTag, 1)
		Expect(reconcileResult(r, sd).RequeueAfter).To(Equal(canaryWindowTTL))
		active := condition(getServingDeployment(sd), servingv1alpha1.ConditionCanaryActive)
		Expect(active.Status).To(Equal(metav1.ConditionTrue))
		Expect(active.LastTransitionTime.Time).To(BeTemporally("==", windowStart.Add(55*time.Minute)))
		expectShadowReplicas(ns, 0)
	})

	It("keeps the TTL across an operator restart: a new reconciler reads the clock from status", func() {
		ns, sd, r, _, _ := steadyStack()
		openWindowFully(ns, sd, r)

		restarted, _, clock := newClockedReconciler()
		clock.t = windowStart.Add(canaryWindowTTL + time.Minute)
		Expect(reconcileServingDeployment(restarted, sd)).To(Succeed())

		active := condition(getServingDeployment(sd), servingv1alpha1.ConditionCanaryActive)
		Expect(active.Reason).To(Equal(servingv1alpha1.ReasonWindowExpired))
		expectCanaryReplicas(ns, 0)
	})

	It("moves neither the canary nor the shadow when a deployment/api-canary it does not control exists", func() {
		ns, sd, r, writes, _ := steadyStack()
		foreign := newCanaryDeployment(ns, "example.com/elsewhere:latest", 1)
		Expect(k8sClient.Create(ctx, foreign)).To(Succeed())
		setCanarySpec(sd, canaryImageTag, 1)

		err := reconcileServingDeployment(r, sd)
		Expect(err).To(MatchError(ContainSubstring("deployment/api-canary exists and is not controlled")))
		expectShadowReplicas(ns, 1)
		Expect(writes.statefulSetWrites()).To(BeEmpty())
		Expect(getCanaryDeployment(ns).Spec.Template.Spec.Containers[0].Image).To(Equal("example.com/elsewhere:latest"))
	})

	It("opens and closes through the watches of a running manager alone, with no direct Reconcile call", func() {
		ns := newTestNamespace()
		createStatefulSet(ns, shadowStatefulSetName)
		startManager(ns)
		sd := createServingDeployment(ns)
		poll := func(check func(g Gomega)) {
			Eventually(check).WithTimeout(managerPollTimeout).WithPolling(managerPollInterval).Should(Succeed())
		}
		poll(func(g Gomega) {
			g.Expect(k8sClient.Get(ctx, stableKey(ns), &appsv1.Deployment{})).To(Succeed())
		})
		markRolledOut(ns)

		setCanarySpec(sd, canaryImageTag, 1)
		poll(func(g Gomega) {
			g.Expect(getStatefulSet(ns, shadowStatefulSetName).Spec.Replicas).To(HaveValue(BeZero()))
		})
		markStatefulSetObserved(ns, shadowStatefulSetName)
		poll(func(g Gomega) { g.Expect(getCanaryDeployment(ns)).NotTo(BeNil()) })
		markCanaryObserved(ns, 0)

		setCanarySpec(sd, "", 0)
		poll(func(g Gomega) {
			g.Expect(getCanaryDeployment(ns).Spec.Replicas).To(HaveValue(BeZero()))
		})
		markCanaryObserved(ns, 0)
		poll(func(g Gomega) {
			g.Expect(getStatefulSet(ns, shadowStatefulSetName).Spec.Replicas).To(HaveValue(BeEquivalentTo(1)))
		})
	})

	DescribeTable("decideWindow",
		func(spec servingv1alpha1.ServingDeploymentSpec, prev *metav1.Condition, now time.Time,
			wantOpen bool, wantReason string, wantDeadline time.Time) {
			w := decideWindow(spec, prev, now)
			Expect(w.open).To(Equal(wantOpen))
			Expect(w.reason).To(Equal(wantReason))
			Expect(w.deadline).To(BeTemporally("==", wantDeadline))
		},
		Entry("the cleared shape: no window",
			servingv1alpha1.ServingDeploymentSpec{}, nil, windowStart,
			false, servingv1alpha1.ReasonNoCanary, time.Time{}),
		Entry("a tag with zero replicas is not a window",
			servingv1alpha1.ServingDeploymentSpec{CanaryImageTag: canaryImageTag}, nil, windowStart,
			false, servingv1alpha1.ReasonNoCanary, time.Time{}),
		Entry("a requested window with none open: opens on a clock counted from the whole second",
			windowSpec(canaryImageTag), canaryActive(metav1.ConditionFalse, servingv1alpha1.ReasonNoCanary, windowStart),
			windowStart.Add(1500*time.Millisecond),
			true, servingv1alpha1.ReasonWindowOpen, windowStart.Add(time.Second+canaryWindowTTL)),
		Entry("an open window inside its TTL keeps its deadline",
			windowSpec(laterCanaryImageTag), canaryActive(metav1.ConditionTrue, servingv1alpha1.ReasonWindowOpen, windowStart),
			windowStart.Add(44*time.Minute),
			true, servingv1alpha1.ReasonWindowOpen, windowStart.Add(canaryWindowTTL)),
		Entry("an open window at its deadline expires",
			windowSpec(canaryImageTag), canaryActive(metav1.ConditionTrue, servingv1alpha1.ReasonWindowOpen, windowStart),
			windowStart.Add(canaryWindowTTL),
			false, servingv1alpha1.ReasonWindowExpired, time.Time{}),
		Entry("an open window whose spec stops requesting it closes",
			servingv1alpha1.ServingDeploymentSpec{}, canaryActive(metav1.ConditionTrue, servingv1alpha1.ReasonWindowOpen, windowStart),
			windowStart.Add(time.Minute),
			false, servingv1alpha1.ReasonNoCanary, time.Time{}),
		Entry("an expired window stays latched while the spec requests one",
			windowSpec(thirdCanaryImageTag), canaryActive(metav1.ConditionFalse, servingv1alpha1.ReasonWindowExpired, windowStart),
			windowStart.Add(3*time.Hour),
			false, servingv1alpha1.ReasonWindowExpired, time.Time{}),
		Entry("an expired window stays latched on a half-cleared spec",
			servingv1alpha1.ServingDeploymentSpec{CanaryImageTag: thirdCanaryImageTag},
			canaryActive(metav1.ConditionFalse, servingv1alpha1.ReasonWindowExpired, windowStart), windowStart,
			false, servingv1alpha1.ReasonWindowExpired, time.Time{}),
		Entry("the cleared shape releases an expired window",
			servingv1alpha1.ServingDeploymentSpec{},
			canaryActive(metav1.ConditionFalse, servingv1alpha1.ReasonWindowExpired, windowStart), windowStart,
			false, servingv1alpha1.ReasonNoCanary, time.Time{}),
	)

	DescribeTable("canaryGone is true only for a canary observed at zero with no pods, terminating ones included",
		func(mutate func(*appsv1.Deployment), want bool) {
			dep := newCanaryDeployment("ns", stableImage(canaryImageTag), 0)
			dep.Generation = 4
			dep.Status = appsv1.DeploymentStatus{ObservedGeneration: 4, TerminatingReplicas: new(int32(0))}
			mutate(dep)
			Expect(canaryGone(dep)).To(Equal(want))
		},
		Entry("at zero, observed, nothing terminating", func(*appsv1.Deployment) {}, true),
		Entry("terminatingReplicas not reported", func(d *appsv1.Deployment) { d.Status.TerminatingReplicas = nil }, true),
		Entry("spec asks for a replica", func(d *appsv1.Deployment) { d.Spec.Replicas = new(int32(1)) }, false),
		Entry("status predates the spec", func(d *appsv1.Deployment) { d.Status.ObservedGeneration = 3 }, false),
		Entry("a pod still counted", func(d *appsv1.Deployment) { d.Status.Replicas = 1 }, false),
		Entry("a pod still terminating", func(d *appsv1.Deployment) { d.Status.TerminatingReplicas = new(int32(1)) }, false),
	)

	It("creates the canary as the stable's pod spec under the canary's labels, whose selector skips the stable's pods", func() {
		stable := newStableDeployment("ns", stableImage(validImageTag))
		canary := newCanaryDeployment("ns", stableImage(canaryImageTag), 1)

		Expect(canary.Name).To(Equal("api-canary"))
		Expect(canary.Spec.Strategy.Type).To(Equal(appsv1.RecreateDeploymentStrategyType))
		Expect(canary.Spec.Template.Spec.Containers[0].Ports[0].HostPort).To(BeZero())
		want := stable.Spec.Template.Spec.DeepCopy()
		want.Containers[0].Image = stableImage(canaryImageTag)
		want.Containers[0].Ports[0].HostPort = 0
		Expect(canary.Spec.Template.Spec).To(Equal(*want))

		canarySelector, err := metav1.LabelSelectorAsSelector(canary.Spec.Selector)
		Expect(err).NotTo(HaveOccurred())
		Expect(canarySelector.Matches(labels.Set(canary.Spec.Template.Labels))).To(BeTrue())
		Expect(canarySelector.Matches(labels.Set(stable.Spec.Template.Labels))).To(BeFalse(),
			"the canary must not select the stable's pods")
		sharedService := labels.SelectorFromSet(apiLabels())
		Expect(sharedService.Matches(labels.Set(canary.Spec.Template.Labels))).To(BeTrue(),
			"the shared api Service must reach the canary: the D34 split")
	})
})

// drainEvents returns every event r's FakeRecorder holds, emptying it.
func drainEvents(r *ServingDeploymentReconciler) []string {
	events := r.Recorder.(*record.FakeRecorder).Events
	var out []string
	for {
		select {
		case e := <-events:
			out = append(out, e)
		default:
			return out
		}
	}
}

func windowSpec(tag string) servingv1alpha1.ServingDeploymentSpec {
	return servingv1alpha1.ServingDeploymentSpec{ImageTag: validImageTag, CanaryImageTag: tag, CanaryReplicas: 1}
}

func canaryActive(status metav1.ConditionStatus, reason string, since time.Time) *metav1.Condition {
	return &metav1.Condition{
		Type:               servingv1alpha1.ConditionCanaryActive,
		Status:             status,
		Reason:             reason,
		LastTransitionTime: metav1.NewTime(since),
	}
}
