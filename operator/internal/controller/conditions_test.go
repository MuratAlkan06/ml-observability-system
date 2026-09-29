package controller

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	servingv1alpha1 "github.com/MuratAlkan06/ml-observability-system/operator/api/v1alpha1"
)

// markRolledOut writes the status the Deployment controller reports once
// deployment/api's current spec has rolled out and is Available. envtest runs
// no Deployment controller, so without this the status stays empty.
func markRolledOut(namespace string) {
	dep := getStable(namespace)
	dep.Status = rolledOutStatus(dep.Generation)
	Expect(k8sClient.Status().Update(ctx, dep)).To(Succeed())
}

// rolledOutStatus is a one-replica Deployment's status once generation has
// rolled out and is Available.
func rolledOutStatus(generation int64) appsv1.DeploymentStatus {
	now := metav1.Now()
	return appsv1.DeploymentStatus{
		ObservedGeneration: generation,
		Replicas:           1,
		UpdatedReplicas:    1,
		ReadyReplicas:      1,
		AvailableReplicas:  1,
		Conditions: []appsv1.DeploymentCondition{{
			Type:               appsv1.DeploymentAvailable,
			Status:             corev1.ConditionTrue,
			Reason:             "MinimumReplicasAvailable",
			LastUpdateTime:     now,
			LastTransitionTime: now,
		}},
	}
}

func condition(sd *servingv1alpha1.ServingDeployment, conditionType string) metav1.Condition {
	c := meta.FindStatusCondition(sd.Status.Conditions, conditionType)
	Expect(c).NotTo(BeNil(), "no %s condition", conditionType)
	return *c
}

// backdateConditions moves every condition's lastTransitionTime to past, so a
// later stamp is distinguishable from it without depending on the clock.
func backdateConditions(sd *servingv1alpha1.ServingDeployment, past metav1.Time) {
	latest := getServingDeployment(sd)
	for i := range latest.Status.Conditions {
		latest.Status.Conditions[i].LastTransitionTime = past
	}
	Expect(k8sClient.Status().Update(ctx, latest)).To(Succeed())
}

var _ = Describe("Writing the ServingDeployment's conditions", func() {
	past := metav1.NewTime(time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC))

	It("writes Ready, CanaryActive and ShadowPaused on the first reconcile, "+
		"each stamped with lastTransitionTime and the observed generation", func() {
		ns := newTestNamespace()
		createStatefulSet(ns, shadowStatefulSetName)
		sd := createServingDeployment(ns)
		r, _ := newTestReconciler()

		Expect(reconcileServingDeployment(r, sd)).To(Succeed())

		sd = getServingDeployment(sd)
		Expect(sd.Status.Conditions).To(HaveLen(3))
		for _, want := range []struct{ conditionType, reason string }{
			{servingv1alpha1.ConditionReady, servingv1alpha1.ReasonAwaitingRollout},
			{servingv1alpha1.ConditionCanaryActive, servingv1alpha1.ReasonNoCanary},
			{servingv1alpha1.ConditionShadowPaused, servingv1alpha1.ReasonShadowRunning},
		} {
			c := condition(sd, want.conditionType)
			Expect(c.Status).To(Equal(metav1.ConditionFalse), want.conditionType)
			Expect(c.Reason).To(Equal(want.reason), want.conditionType)
			Expect(c.LastTransitionTime.IsZero()).To(BeFalse(), want.conditionType)
			Expect(c.ObservedGeneration).To(Equal(sd.Generation), want.conditionType)
		}
	})

	It("flips Ready from False to True once deployment/api reports its rollout Available, "+
		"stamping a new lastTransitionTime on that transition only", func() {
		ns := newTestNamespace()
		sd := createServingDeployment(ns)
		r, _ := newTestReconciler()
		Expect(reconcileServingDeployment(r, sd)).To(Succeed())
		backdateConditions(sd, past)

		By("a reconcile with nothing changed keeps every timestamp")
		Expect(reconcileServingDeployment(r, sd)).To(Succeed())
		sd = getServingDeployment(sd)
		Expect(condition(sd, servingv1alpha1.ConditionReady).Status).To(Equal(metav1.ConditionFalse))
		for _, c := range sd.Status.Conditions {
			Expect(c.LastTransitionTime.Equal(&past)).To(BeTrue(), "%s moved without a transition", c.Type)
		}

		By("the rollout becoming Available flips Ready and stamps it")
		markRolledOut(ns)
		Expect(reconcileServingDeployment(r, sd)).To(Succeed())
		sd = getServingDeployment(sd)
		ready := condition(sd, servingv1alpha1.ConditionReady)
		Expect(ready.Status).To(Equal(metav1.ConditionTrue))
		Expect(ready.Reason).To(Equal(servingv1alpha1.ReasonStableAvailable))
		Expect(ready.LastTransitionTime.After(past.Time)).To(BeTrue())
		Expect(ready.ObservedGeneration).To(Equal(sd.Generation))
		Expect(sd.Status.ObservedGeneration).To(Equal(sd.Generation))
		for _, t := range []string{servingv1alpha1.ConditionCanaryActive, servingv1alpha1.ConditionShadowPaused} {
			unchanged := condition(sd, t)
			Expect(unchanged.LastTransitionTime.Equal(&past)).To(BeTrue(), "%s moved without a transition", t)
		}
	})

	It("turns Ready False on a spec.imageTag change until the new image has rolled out, "+
		"though deployment/api still reports the old rollout Available", func() {
		ns := newTestNamespace()
		sd := createServingDeployment(ns)
		r, _ := newTestReconciler()
		Expect(reconcileServingDeployment(r, sd)).To(Succeed())
		markRolledOut(ns)
		Expect(reconcileServingDeployment(r, sd)).To(Succeed())
		sd = getServingDeployment(sd)
		Expect(meta.IsStatusConditionTrue(sd.Status.Conditions, servingv1alpha1.ConditionReady)).To(BeTrue())

		By("bumping the generation: the Ready=True on record is for generation 1")
		sd.Spec.ImageTag = previousImageTag
		Expect(k8sClient.Update(ctx, sd)).To(Succeed())
		sd = getServingDeployment(sd)
		Expect(sd.Generation).To(Equal(int64(2)))
		Expect(sd.Status.ObservedGeneration).To(Equal(int64(1)),
			"D32's wait needs observedGeneration == generation, so the old Ready=True does not satisfy it")

		By("reconciling generation 2: the image moves and Ready goes False")
		Expect(reconcileServingDeployment(r, sd)).To(Succeed())
		sd = getServingDeployment(sd)
		Expect(sd.Status.ObservedGeneration).To(Equal(int64(2)))
		ready := condition(sd, servingv1alpha1.ConditionReady)
		Expect(ready.Status).To(Equal(metav1.ConditionFalse))
		Expect(ready.Reason).To(Equal(servingv1alpha1.ReasonAwaitingRollout))
		Expect(ready.ObservedGeneration).To(Equal(int64(2)))
		dep := getStable(ns)
		Expect(dep.Status.ObservedGeneration).To(BeNumerically("<", dep.Generation),
			"deployment/api's status still describes the previous image's rollout")

		By("the new image rolling out turns Ready True for generation 2")
		markRolledOut(ns)
		Expect(reconcileServingDeployment(r, sd)).To(Succeed())
		sd = getServingDeployment(sd)
		ready = condition(sd, servingv1alpha1.ConditionReady)
		Expect(ready.Status).To(Equal(metav1.ConditionTrue))
		Expect(ready.ObservedGeneration).To(Equal(int64(2)))
	})

	DescribeTable("stableAvailable is true only for a Deployment rolled out on its current generation and Available",
		func(mutate func(*appsv1.Deployment), want bool) {
			dep := &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{Generation: 3},
				Spec:       appsv1.DeploymentSpec{Replicas: new(int32(1))},
				Status:     rolledOutStatus(3),
			}
			mutate(dep)
			Expect(stableAvailable(dep)).To(Equal(want))
		},
		Entry("rolled out and Available", func(*appsv1.Deployment) {}, true),
		Entry("status observed an older generation", func(d *appsv1.Deployment) {
			d.Status.ObservedGeneration = 2
		}, false),
		Entry("no replica on the current template yet", func(d *appsv1.Deployment) {
			d.Status.UpdatedReplicas = 0
		}, false),
		Entry("a replica of an older template remains", func(d *appsv1.Deployment) {
			d.Status.Replicas = 2
		}, false),
		Entry("the updated replica is not available yet", func(d *appsv1.Deployment) {
			d.Status.AvailableReplicas = 0
		}, false),
		Entry("the Available condition is False", func(d *appsv1.Deployment) {
			d.Status.Conditions[0].Status = corev1.ConditionFalse
		}, false),
		Entry("no Available condition reported", func(d *appsv1.Deployment) {
			d.Status.Conditions = nil
		}, false),
	)
})
