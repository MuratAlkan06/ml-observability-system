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
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/diff"
	"k8s.io/apimachinery/pkg/util/yaml"
)

// k3sDir is deploy/k3s/, relative to this package, the directory `go test`
// runs it in.
var k3sDir = filepath.Join("..", "..", "..", "deploy", "k3s")

// renderedManifestDeployment decodes the Deployment in one file under
// deploy/k3s/manifests/ after substituting the tokens the way apply.sh does
// under its default IMAGE_PREFIX.
func renderedManifestDeployment(file, tag string) *appsv1.Deployment {
	raw, err := os.ReadFile(filepath.Join(k3sDir, "manifests", file))
	Expect(err).NotTo(HaveOccurred())
	rendered := strings.NewReplacer("IMAGE_PREFIX", imagePrefix, "IMAGE_TAG", tag).Replace(string(raw))

	var found *appsv1.Deployment
	decoder := yaml.NewYAMLOrJSONDecoder(strings.NewReader(rendered), 4096)
	for {
		doc := &unstructured.Unstructured{}
		err := decoder.Decode(&doc.Object)
		if errors.Is(err, io.EOF) {
			break
		}
		Expect(err).NotTo(HaveOccurred())
		if doc.GetKind() != "Deployment" {
			continue
		}
		Expect(found).To(BeNil(), "%s holds more than one Deployment", file)
		found = &appsv1.Deployment{}
		Expect(runtime.DefaultUnstructuredConverter.FromUnstructured(doc.Object, found)).To(Succeed())
	}
	Expect(found).NotTo(BeNil(), "%s holds no Deployment", file)
	return found
}

var _ = Describe("The stable api Deployment the operator creates", func() {
	It("renders spec.imageTag as ghcr.io/muratalkan06/mlobs-api:<tag>, apply.sh's default image reference", func() {
		Expect(stableImage(validImageTag)).To(Equal("ghcr.io/muratalkan06/mlobs-api:" + validImageTag))

		applySh, err := os.ReadFile(filepath.Join(k3sDir, "apply.sh"))
		Expect(err).NotTo(HaveOccurred())
		Expect(string(applySh)).To(ContainSubstring(`IMAGE_PREFIX="${IMAGE_PREFIX:-` + imagePrefix + `}"`))
	})

	// newStableDeployment is a copy of the manifest's Deployment (see its doc
	// comment); this is what keeps the copy honest while both exist.
	It("matches the Deployment in deploy/k3s/manifests/20-api.yaml field for field", func() {
		manifest := renderedManifestDeployment("20-api.yaml", validImageTag)
		built := newStableDeployment(manifest.Namespace, stableImage(validImageTag))

		Expect(built.Name).To(Equal(manifest.Name))
		Expect(built.Labels).To(Equal(manifest.Labels))
		Expect(built.Annotations).To(Equal(manifest.Annotations))
		Expect(equality.Semantic.DeepEqual(built.Spec, manifest.Spec)).To(BeTrue(),
			"newStableDeployment differs from 20-api.yaml (-built +manifest):\n%s", diff.Diff(built.Spec, manifest.Spec))
	})
})
