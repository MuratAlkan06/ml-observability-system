package controller

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/diff"
	"k8s.io/apimachinery/pkg/util/yaml"

	servingv1alpha1 "github.com/MuratAlkan06/ml-observability-system/operator/api/v1alpha1"
)

// k3sDir is deploy/k3s/, relative to this package, the directory `go test`
// runs it in.
var k3sDir = filepath.Join("..", "..", "..", "deploy", "k3s")

// testImagePrefix stands for a non-default IMAGE_PREFIX, the one the k3d
// rehearsals render.
const testImagePrefix = "docker.io/library"

// stableImage is the api image reference for tag under the default prefix,
// the one a reconciler with no ImagePrefix writes.
func stableImage(tag string) string {
	return apiImage(DefaultImagePrefix, tag)
}

// decodeDocuments decodes every YAML document in path, after substituting
// apply.sh's tokens the way it renders them under prefix.
func decodeDocuments(path, prefix, tag string) []*unstructured.Unstructured {
	raw, err := os.ReadFile(path)
	Expect(err).NotTo(HaveOccurred())
	rendered := strings.NewReplacer("IMAGE_PREFIX", prefix, "IMAGE_TAG", tag).Replace(string(raw))

	var docs []*unstructured.Unstructured
	decoder := yaml.NewYAMLOrJSONDecoder(strings.NewReader(rendered), 4096)
	for {
		doc := &unstructured.Unstructured{}
		err := decoder.Decode(&doc.Object)
		if errors.Is(err, io.EOF) {
			return docs
		}
		Expect(err).NotTo(HaveOccurred())
		if doc.Object != nil {
			docs = append(docs, doc)
		}
	}
}

// decodeOne decodes the single document of kind named name in path into out.
func decodeOne(path, prefix, tag, kind, name string, out any) {
	var found *unstructured.Unstructured
	for _, doc := range decodeDocuments(path, prefix, tag) {
		if doc.GetKind() == kind && doc.GetName() == name {
			Expect(found).To(BeNil(), "%s holds %s/%s twice", path, kind, name)
			found = doc
		}
	}
	Expect(found).NotTo(BeNil(), "%s holds no %s/%s", path, kind, name)
	Expect(runtime.DefaultUnstructuredConverter.FromUnstructured(found.Object, out)).To(Succeed())
}

func manifest(file string) string {
	return filepath.Join(k3sDir, "manifests", file)
}

var _ = Describe("The stable api Deployment the operator creates", func() {
	It("renders spec.imageTag as ghcr.io/muratalkan06/mlobs-api:<tag> by default, apply.sh's default image reference", func() {
		Expect(stableImage(validImageTag)).To(Equal("ghcr.io/muratalkan06/mlobs-api:" + validImageTag))
		Expect((&ServingDeploymentReconciler{}).apiImage(validImageTag)).To(Equal(stableImage(validImageTag)))

		applySh, err := os.ReadFile(filepath.Join(k3sDir, "apply.sh"))
		Expect(err).NotTo(HaveOccurred())
		Expect(string(applySh)).To(ContainSubstring(`IMAGE_PREFIX="${IMAGE_PREFIX:-` + DefaultImagePrefix + `}"`))
	})

	It("renders the api image under the reconciler's ImagePrefix when one is set", func() {
		r := &ServingDeploymentReconciler{ImagePrefix: testImagePrefix}
		Expect(r.apiImage(validImageTag)).To(Equal("docker.io/library/mlobs-api:" + validImageTag))
	})

	// newStableDeployment is copied, as YAML with its reasoning, into the
	// golden file (see its doc comment); this is what keeps the two equal now
	// that no manifest renders deployment/api.
	It("matches testdata/api-deployment.yaml field for field", func() {
		golden := &appsv1.Deployment{}
		decodeOne(filepath.Join("testdata", "api-deployment.yaml"), "", "", "Deployment", stableDeploymentName, golden)
		built := newStableDeployment(golden.Namespace, stableImage(validImageTag))

		Expect(built.Name).To(Equal(golden.Name))
		Expect(built.Labels).To(Equal(golden.Labels))
		Expect(built.Annotations).To(Equal(golden.Annotations))
		Expect(equality.Semantic.DeepEqual(built.Spec, golden.Spec)).To(BeTrue(),
			"newStableDeployment differs from testdata/api-deployment.yaml (-built +golden):\n%s",
			diff.Diff(built.Spec, golden.Spec))
	})
})

var _ = Describe("The manifests the operator's objects meet", func() {
	It("publishes the api on NodePort 8000 with externalTrafficPolicy Local, over the stable's pods and the canary's", func() {
		for _, doc := range decodeDocuments(manifest("20-api.yaml"), DefaultImagePrefix, validImageTag) {
			Expect(doc.GetKind()).To(Equal("Service"), "20-api.yaml renders no workload since O2")
		}
		svc := &corev1.Service{}
		decodeOne(manifest("20-api.yaml"), DefaultImagePrefix, validImageTag, "Service", "api", svc)
		Expect(svc.Spec.Type).To(Equal(corev1.ServiceTypeNodePort))
		Expect(svc.Spec.ExternalTrafficPolicy).To(Equal(corev1.ServiceExternalTrafficPolicyLocal))
		Expect(svc.Spec.Ports).To(HaveLen(1))
		Expect(svc.Spec.Ports[0].NodePort).To(BeEquivalentTo(8000))
		Expect(svc.Spec.Ports[0].Port).To(BeEquivalentTo(8000))

		selector := labels.SelectorFromSet(svc.Spec.Selector)
		Expect(selector.Matches(labels.Set(apiLabels()))).To(BeTrue(), "the api Service must reach the stable")
		Expect(selector.Matches(labels.Set(canaryLabels()))).To(BeTrue(), "the api Service must reach the canary: D34")
	})

	It("gives the canary its own ClusterIP Service, api-canary, over the canary's pods only", func() {
		svc := &corev1.Service{}
		decodeOne(manifest("20-api.yaml"), DefaultImagePrefix, validImageTag, "Service", "api-canary", svc)
		Expect(svc.Spec.Type).To(Equal(corev1.ServiceTypeClusterIP))
		Expect(svc.Spec.Ports).To(HaveLen(1))
		Expect(svc.Spec.Ports[0].Port).To(BeEquivalentTo(8000))

		selector := labels.SelectorFromSet(svc.Spec.Selector)
		Expect(selector.Matches(labels.Set(canaryLabels()))).To(BeTrue())
		Expect(selector.Matches(labels.Set(apiLabels()))).To(BeFalse(), "api-canary must not reach the stable")
	})

	It("renders the ServingDeployment with spec.imageTag alone (D32)", func() {
		sd := &servingv1alpha1.ServingDeployment{}
		decodeOne(manifest("40-servingdeployment.yaml"), DefaultImagePrefix, validImageTag,
			"ServingDeployment", servingDeploymentName, sd)
		Expect(sd.Namespace).To(Equal("mlobs"))
		Expect(sd.Spec).To(Equal(servingv1alpha1.ServingDeploymentSpec{ImageTag: validImageTag}))

		doc := decodeDocuments(manifest("40-servingdeployment.yaml"), DefaultImagePrefix, validImageTag)[0]
		spec, found, err := unstructured.NestedMap(doc.Object, "spec")
		Expect(err).NotTo(HaveOccurred())
		Expect(found).To(BeTrue())
		Expect(spec).To(HaveLen(1), "the pipeline renders no canary field, not even a zero one")
	})

	It("runs the operator as the operator ServiceAccount, on the rendered image, "+
		"passing apply.sh's image prefix on to the api images it writes", func() {
		dep := &appsv1.Deployment{}
		decodeOne(manifest("03-operator.yaml"), testImagePrefix, validImageTag, "Deployment", "operator", dep)
		pod := dep.Spec.Template.Spec
		Expect(pod.ServiceAccountName).To(Equal("operator"))
		Expect(pod.Containers).To(HaveLen(1))
		container := pod.Containers[0]
		Expect(container.Image).To(Equal(testImagePrefix + "/mlobs-operator:" + validImageTag))
		Expect(container.Args).To(ContainElements("--leader-elect", "--image-prefix="+testImagePrefix))
		Expect(container.Env).To(ContainElement(HaveField("Name", "WATCH_NAMESPACE")))
		for _, port := range container.Ports {
			Expect(port.HostPort).To(BeZero())
		}
	})

	It("leaves the shadow scorer's replica count to the operator: 22-shadow-scorer.yaml renders none", func() {
		sts := &appsv1.StatefulSet{}
		decodeOne(manifest("22-shadow-scorer.yaml"), DefaultImagePrefix, validImageTag,
			"StatefulSet", shadowStatefulSetName, sts)
		Expect(sts.Spec.Replicas).To(BeNil(), "a replicas line would reset the operator's scale on every apply")
	})
})
