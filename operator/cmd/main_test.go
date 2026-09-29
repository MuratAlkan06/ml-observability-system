package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	. "github.com/onsi/gomega"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

// electionDeadline bounds the wait for the Lease: a readiness poll inside a
// deadline, not a timing assertion (PRINCIPLES.md §5).
const electionDeadline = 30 * time.Second

// Leader election is asserted as the acquisition of the Lease and nothing
// more: no failover, which would wait on lease durations and pod-kill timing
// and be flaky by construction (docs/PLAN.md D35).
func TestLeaderElectionAcquiresTheLeaseInMlobsUnderTheManagersIdentity(t *testing.T) {
	g := NewWithT(t)

	env := &envtest.Environment{}
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		env.BinaryAssetsDirectory = firstEnvtestBinaryDir()
	}
	cfg, err := env.Start()
	g.Expect(err).NotTo(HaveOccurred())
	t.Cleanup(func() { g.Expect(env.Stop()).To(Succeed()) })

	k8sClient, err := client.New(cfg, client.Options{Scheme: scheme})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(k8sClient.Create(t.Context(), &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: leaderElectionNamespace},
	})).To(Succeed())

	opts := ctrl.Options{Scheme: scheme, Metrics: metricsserver.Options{BindAddress: "0"}}
	setLeaderElection(&opts, true)
	mgr, err := ctrl.NewManager(cfg, opts)
	g.Expect(err).NotTo(HaveOccurred())

	mgrCtx, stop := context.WithCancel(context.Background())
	stopped := make(chan error, 1)
	go func() { stopped <- mgr.Start(mgrCtx) }()
	t.Cleanup(func() {
		stop()
		g.Eventually(stopped).WithTimeout(electionDeadline).Should(Receive())
	})

	g.Eventually(mgr.Elected()).WithTimeout(electionDeadline).Should(BeClosed())

	lease := &coordinationv1.Lease{}
	key := types.NamespacedName{Namespace: "mlobs", Name: leaderElectionID}
	g.Expect(k8sClient.Get(t.Context(), key, lease)).To(Succeed())
	// controller-runtime names a manager hostname_<uuid> when it takes a lock.
	hostname, err := os.Hostname()
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(lease.Spec.HolderIdentity).To(HaveValue(HavePrefix(hostname + "_")))
}

// --image-prefix heads the api image references the operator writes; apply.sh
// renders its IMAGE_PREFIX into it, docker.io/library in the k3d rehearsals.
func TestValidateImagePrefixAcceptsRegistryPrefixesAndRejectsTheRest(t *testing.T) {
	g := NewWithT(t)
	for _, ok := range []string{"ghcr.io/muratalkan06", "docker.io/library", "localhost:5000/mlobs"} {
		g.Expect(validateImagePrefix(ok)).To(Succeed(), ok)
	}
	for _, bad := range []string{"", "ghcr.io/muratalkan06/", "/ghcr.io", "ghcr.io/a b", "ghcr.io/x@sha256"} {
		g.Expect(validateImagePrefix(bad)).NotTo(Succeed(), bad)
	}
}

// firstEnvtestBinaryDir finds the envtest assets `make setup-envtest` puts
// under bin/k8s/, for runs outside make that leave KUBEBUILDER_ASSETS unset;
// the controller suite's getFirstFoundEnvTestBinaryDir does the same.
func firstEnvtestBinaryDir() string {
	basePath := filepath.Join("..", "bin", "k8s")
	entries, err := os.ReadDir(basePath)
	if err != nil {
		return ""
	}
	for _, entry := range entries {
		if entry.IsDir() {
			return filepath.Join(basePath, entry.Name())
		}
	}
	return ""
}
