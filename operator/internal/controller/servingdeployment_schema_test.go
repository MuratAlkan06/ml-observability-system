package controller

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	servingv1alpha1 "github.com/MuratAlkan06/ml-observability-system/operator/api/v1alpha1"
)

// validImageTag has the shape of a full commit SHA: 40 lowercase hex
// characters. The value itself is arbitrary.
const validImageTag = "0123456789abcdef0123456789abcdef01234567"

// These run against the CRD envtest installs, so they assert what the API
// server enforces from the schema — the only validation in v0, which has no
// webhooks (docs/PLAN.md D33).
var _ = Describe("ServingDeployment CRD schema", func() {
	newServingDeployment := func(name string, spec servingv1alpha1.ServingDeploymentSpec) *servingv1alpha1.ServingDeployment {
		return &servingv1alpha1.ServingDeployment{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
			Spec:       spec,
		}
	}

	It("accepts 40-hex image tags with canaryReplicas at its maximum of 1", func() {
		sd := newServingDeployment("schema-accepts", servingv1alpha1.ServingDeploymentSpec{
			ImageTag:       validImageTag,
			CanaryImageTag: validImageTag,
			CanaryReplicas: 1,
		})
		Expect(k8sClient.Create(ctx, sd)).To(Succeed())
		DeferCleanup(func() { Expect(k8sClient.Delete(ctx, sd)).To(Succeed()) })
	})

	DescribeTable("rejects a spec the schema does not admit",
		func(spec servingv1alpha1.ServingDeploymentSpec) {
			err := k8sClient.Create(ctx, newServingDeployment("schema-rejects", spec))
			Expect(err).To(Satisfy(apierrors.IsInvalid))
		},
		Entry("empty imageTag", servingv1alpha1.ServingDeploymentSpec{}),
		Entry("imageTag as a 7-character short SHA",
			servingv1alpha1.ServingDeploymentSpec{ImageTag: validImageTag[:7]}),
		Entry("imageTag in uppercase hex",
			servingv1alpha1.ServingDeploymentSpec{ImageTag: "0123456789ABCDEF0123456789ABCDEF01234567"}),
		Entry("imageTag of 41 characters",
			servingv1alpha1.ServingDeploymentSpec{ImageTag: validImageTag + "8"}),
		Entry("imageTag as a moving tag",
			servingv1alpha1.ServingDeploymentSpec{ImageTag: "latest"}),
		Entry("canaryImageTag as a 7-character short SHA",
			servingv1alpha1.ServingDeploymentSpec{ImageTag: validImageTag, CanaryImageTag: validImageTag[:7]}),
		Entry("canaryReplicas 2, above the maximum of 1",
			servingv1alpha1.ServingDeploymentSpec{ImageTag: validImageTag, CanaryReplicas: 2}),
		Entry("canaryReplicas -1, below the minimum of 0",
			servingv1alpha1.ServingDeploymentSpec{ImageTag: validImageTag, CanaryReplicas: -1}),
	)

	DescribeTable("admits spec.gate.mode as exactly the ordered enum Observe, ActRollback, ActPromote, and "+
		"forCanaryImageTag as a 40-hex tag (D42)",
		func(gate servingv1alpha1.GateSpec, valid bool) {
			sd := newServingDeployment("schema-gate", servingv1alpha1.ServingDeploymentSpec{
				ImageTag: validImageTag, Gate: &gate,
			})
			err := k8sClient.Create(ctx, sd)
			if !valid {
				Expect(err).To(Satisfy(apierrors.IsInvalid))
				return
			}
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Delete(ctx, sd)).To(Succeed())
		},
		Entry("Observe, with no tag", servingv1alpha1.GateSpec{Mode: servingv1alpha1.GateModeObserve}, true),
		Entry("ActRollback for a tag",
			servingv1alpha1.GateSpec{Mode: servingv1alpha1.GateModeActRollback, ForCanaryImageTag: validImageTag}, true),
		Entry("ActPromote for a tag",
			servingv1alpha1.GateSpec{Mode: servingv1alpha1.GateModeActPromote, ForCanaryImageTag: validImageTag}, true),
		Entry("no mode", servingv1alpha1.GateSpec{}, false),
		Entry("observe, in lowercase", servingv1alpha1.GateSpec{Mode: "observe"}, false),
		Entry("a mode outside the enum", servingv1alpha1.GateSpec{Mode: "Act"}, false),
		Entry("a mode that promotes and will not abort, which is not offered", servingv1alpha1.GateSpec{Mode: "Promote"},
			false),
		Entry("forCanaryImageTag as a 7-character short SHA",
			servingv1alpha1.GateSpec{Mode: servingv1alpha1.GateModeActRollback, ForCanaryImageTag: validImageTag[:7]},
			false),
	)

	DescribeTable("validates status.gate: its verdicts, reasons, trips and arming are enums, and its ring holds "+
		"at most 50 records",
		func(mutate func(*servingv1alpha1.GateStatus), valid bool) {
			sd := newServingDeployment("schema-status-gate", servingv1alpha1.ServingDeploymentSpec{ImageTag: validImageTag})
			Expect(k8sClient.Create(ctx, sd)).To(Succeed())
			DeferCleanup(func() { Expect(k8sClient.Delete(ctx, sd)).To(Succeed()) })
			st := schemaGateStatus()
			mutate(st)
			sd.Status.Gate = st
			err := k8sClient.Status().Update(ctx, sd)
			if valid {
				Expect(err).NotTo(HaveOccurred())
			} else {
				Expect(err).To(Satisfy(apierrors.IsInvalid))
			}
		},
		Entry("a status.gate as the operator writes it", func(*servingv1alpha1.GateStatus) {}, true),
		Entry("50 records", func(st *servingv1alpha1.GateStatus) { st.Records = schemaRecords(50) }, true),
		Entry("51 records", func(st *servingv1alpha1.GateStatus) { st.Records = schemaRecords(51) }, false),
		Entry("a record's verdict outside the vocabulary",
			func(st *servingv1alpha1.GateStatus) { st.Records[0].Verdict = "Slow" }, false),
		Entry("a record's reason outside the vocabulary", func(st *servingv1alpha1.GateStatus) {
			st.Records[0].Reasons = []servingv1alpha1.GateReason{"LatencyRatio", "Slow"}
		}, false),
		Entry("lastVerdict outside the vocabulary", func(st *servingv1alpha1.GateStatus) { st.LastVerdict = "Ok" }, false),
		Entry("a trip outside the vocabulary",
			func(st *servingv1alpha1.GateStatus) { st.RollbackVerdict.Trip = "Line4WindowState" }, false),
		Entry("an arming outside the vocabulary",
			func(st *servingv1alpha1.GateStatus) { st.Arming = "Disarmed" }, false),
		Entry("a mode outside the enum", func(st *servingv1alpha1.GateStatus) { st.Mode = "Act" }, false),
	)
})

// schemaGateStatus is a status.gate in the shape the operator writes, with a
// value in each enum-validated field.
func schemaGateStatus() *servingv1alpha1.GateStatus {
	at := metav1.NewTime(windowStart.Add(5 * time.Minute))
	return &servingv1alpha1.GateStatus{
		WindowStart:     metav1.NewTime(windowStart),
		IntervalSeconds: 60,
		Mode:            servingv1alpha1.GateModeObserve,
		Arming:          servingv1alpha1.GateArmingArmed,
		NextBoundary:    5,
		BreachRun:       2,
		CanaryReadySeen: true,
		CanaryPod:       &servingv1alpha1.GatePod{Name: "api-canary-7b8d9c6f4-m4tnw", UID: "0d6f3b52"},
		Totals:          servingv1alpha1.GateTotals{Clean: 2, Breach: 2},
		LastVerdict:     "Breach",
		RollbackVerdict: &servingv1alpha1.GateRollbackVerdict{Interval: 4, At: at, Trip: "Line3CanaryLatency"},
		Records:         schemaRecords(1),
	}
}

// schemaRecords is n records, each a latency breach.
func schemaRecords(n int) []servingv1alpha1.GateRecord {
	records := make([]servingv1alpha1.GateRecord, 0, n)
	for i := range n {
		start := windowStart.Add(time.Duration(i) * time.Minute)
		records = append(records, servingv1alpha1.GateRecord{
			Index:   int32(i),
			Start:   metav1.NewTime(start),
			End:     metav1.NewTime(start.Add(time.Minute)),
			Stable:  &servingv1alpha1.GatePodDelta{Requests: 150, P95Seconds: "0.2425"},
			Canary:  &servingv1alpha1.GatePodDelta{Requests: 150, P95Seconds: "0.4875"},
			Ratio:   "2.0103092783505154",
			Verdict: "Breach",
			Reasons: []servingv1alpha1.GateReason{"LatencyRatio"},
		})
	}
	return records
}
