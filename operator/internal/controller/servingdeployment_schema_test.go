package controller

import (
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
})
