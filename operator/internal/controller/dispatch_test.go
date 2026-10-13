package controller

import (
	"crypto/tls"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/config"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	servingv1alpha1 "github.com/MuratAlkan06/ml-observability-system/operator/api/v1alpha1"
)

// The promote dispatch's GitHub client and its shipping (docs/PLAN.md D43;
// the client discipline of #116).

var _ = Describe("The promote dispatch's GitHub client (D43)", func() {
	It("is wired by SetupWithManager to GitHub's API and the Secret's mounted file, with no client to set, so "+
		"its calls go through dispatchClient; and it keeps no token, reading the file at each use", func() {
		mgr, err := ctrl.NewManager(cfg, ctrl.Options{
			Scheme:     k8sClient.Scheme(),
			Metrics:    metricsserver.Options{BindAddress: "0"},
			Cache:      cache.Options{DefaultNamespaces: map[string]cache.Config{metav1.NamespaceDefault: {}}},
			Controller: config.Controller{SkipNameValidation: new(true)},
		})
		Expect(err).NotTo(HaveOccurred())
		r := &ServingDeploymentReconciler{Client: mgr.GetClient(), Scheme: mgr.GetScheme(), Recorder: &record.FakeRecorder{}}
		Expect(r.SetupWithManager(mgr)).To(Succeed())

		Expect(r.dispatcher).To(Equal(&dispatcher{
			baseURL: "https://api.github.com", tokenPath: "/var/run/secrets/mlobs/dispatch/token",
		}))
		Expect(r.dispatcher.httpClient()).To(BeIdenticalTo(dispatchClient))
		visible := reflect.VisibleFields(reflect.TypeFor[dispatcher]())
		fields := make([]string, 0, len(visible))
		for _, field := range visible {
			fields = append(fields, field.Name)
		}
		Expect(fields).To(Equal([]string{"baseURL", "tokenPath", "client"}), "no field to keep a token in")
	})

	It("goes through no proxy whatever the environment names, follows no redirect, keeps no connection, "+
		"bounds the headers it reads, asks for TLS 1.2 at least, and has no timeout but each exchange's", func() {
		Expect(dispatchClient).NotTo(BeIdenticalTo(http.DefaultClient))
		transport, ok := dispatchClient.Transport.(*http.Transport)
		Expect(ok).To(BeTrue(), "dispatchClient has a transport of its own")
		Expect(transport).NotTo(BeIdenticalTo(http.DefaultTransport))
		Expect(transport.Proxy).To(BeNil(), "no proxy, whatever HTTPS_PROXY says: the token goes to GitHub alone")
		Expect(transport.DisableKeepAlives).To(BeTrue())
		Expect(transport.MaxResponseHeaderBytes).To(BeEquivalentTo(maxGitHubHeaderBytes))
		Expect(transport.TLSClientConfig).To(HaveField("MinVersion", BeEquivalentTo(tls.VersionTLS12)))
		Expect(transport.TLSClientConfig.InsecureSkipVerify).To(BeFalse())
		Expect(dispatchClient.Timeout).To(BeZero(), "each exchange's context carries dispatchTimeout")
		Expect(dispatchClient.CheckRedirect(nil, nil)).To(MatchError(http.ErrUseLastResponse))
	})

	It("targets deploy.yml as it stands: a workflow_dispatch whose input is sha, in the deploy concurrency group, "+
		"never cancelled for a newer run", func() {
		data, err := os.ReadFile(filepath.Join("..", "..", "..", ".github", "workflows", deployWorkflow))
		Expect(err).NotTo(HaveOccurred())
		workflow := string(data)
		Expect(workflow).To(ContainSubstring("\non:\n  workflow_dispatch:\n    inputs:\n      sha:\n"))
		Expect(workflow).To(ContainSubstring("\nconcurrency:\n  group: deploy\n  cancel-in-progress: false\n"))
		Expect(workflow).To(ContainSubstring(`if ! [[ "$sha" =~ ^[0-9a-f]{40}$ ]]; then`),
			"the workflow's own check of the input, which the operator's matches")
		Expect(commitSHA.String()).To(Equal("^[0-9a-f]{40}$"))
	})

	DescribeTable("reads the GitHub API from its test-only knob, an http or https URL with a host and nothing "+
		"past its path, and refuses anything else",
		func(value *string, want, wantErr string) {
			api, err := githubAPIFrom(func(name string) (string, bool) {
				Expect(name).To(Equal(githubAPIEnv))
				if value == nil {
					return "", false
				}
				return *value, true
			})
			if wantErr != "" {
				Expect(err).To(MatchError(ContainSubstring(wantErr)))
				return
			}
			Expect(err).NotTo(HaveOccurred())
			Expect(api).To(Equal(want))
		},
		Entry("unset: GitHub's", nil, "https://api.github.com", ""),
		Entry("the e2e's stand-in", new("http://github-stub.mlobs.svc.cluster.local:8000/cgi-bin/github"),
			"http://github-stub.mlobs.svc.cluster.local:8000/cgi-bin/github", ""),
		Entry("a trailing slash, dropped", new("https://ghe.example/api/v3/"), "https://ghe.example/api/v3", ""),
		Entry("another scheme", new("ftp://github.example"), "", "an http or https URL"),
		Entry("no host", new("http:///repos"), "", "an http or https URL"),
		Entry("a user", new("https://token@api.github.com"), "", "no user"),
		Entry("a query", new("https://api.github.com/?x=1"), "", "query"),
		Entry("not a URL", new("api.github.com"), "", "an http or https URL"),
	)

	DescribeTable("refuses to dispatch a sha that is not a 40-character lowercase hex commit SHA, before any "+
		"request is made",
		func(sha string) {
			gh := newGitHubStub()
			d := &dispatcher{baseURL: gh.server.URL, tokenPath: "unused"}
			code, err := d.dispatch(ctx, tokenA, sha)
			Expect(err).To(MatchError(ContainSubstring("is not a 40-character lowercase hex commit SHA")))
			Expect(code).To(BeZero())
			Expect(gh.all()).To(BeEmpty())
		},
		Entry("a branch name", "main"),
		Entry("a 7-character short SHA", canaryImageTag[:7]),
		Entry("uppercase hex", strings.ToUpper(canaryImageTag)),
		Entry("41 characters", canaryImageTag+"0"),
		Entry("a newline after the SHA", canaryImageTag+"\n"),
		Entry("empty", ""),
	)

	It("reads the token at each use: trimmed as the kubelet projects it, bounded, and refused when absent, "+
		"empty, oversized or holding a character no token has; no error carries it", func() {
		path := filepath.Join(GinkgoT().TempDir(), "token")
		d := &dispatcher{tokenPath: path}
		_, err := d.token()
		Expect(err).To(MatchError(fs.ErrNotExist), "the Secret absent")
		Expect(d.tokenPresent()).To(BeFalse())

		writeToken(path, tokenA)
		Expect(d.token()).To(Equal(tokenA))
		Expect(d.tokenPresent()).To(BeTrue())

		for _, bad := range []string{"", "\n", tokenA + " " + tokenB, tokenA + "\n" + tokenB, strings.Repeat("a", maxTokenBytes+1)} {
			Expect(os.WriteFile(path, []byte(bad), 0o600)).To(Succeed())
			_, err := d.token()
			Expect(err).To(HaveOccurred(), "%q", bad)
			Expect(err.Error()).NotTo(ContainSubstring(tokenA))
			Expect(d.tokenPresent()).To(BeFalse())
		}
		Expect(os.WriteFile(path, []byte(strings.Repeat("a", maxTokenBytes)), 0o600)).To(Succeed())
		Expect(d.tokenPresent()).To(BeTrue(), "at the bound")

		var none *dispatcher
		_, err = none.token()
		Expect(err).To(MatchError(errNoDispatcher))
	})

	DescribeTable("fails a runs list whose answer it cannot trust: a page with no total_count, a body past "+
		"its bound, or a status other than 200",
		func(answer http.HandlerFunc, want string) {
			server := httptest.NewServer(answer)
			DeferCleanup(server.Close)
			d := &dispatcher{baseURL: server.URL}
			_, err := d.pendingRun(ctx, tokenA)
			Expect(err).To(MatchError(ContainSubstring(want)))
			Expect(err.Error()).NotTo(ContainSubstring(tokenA))
		},
		Entry("no total_count", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = fmt.Fprint(w, `{"workflow_runs":[]}`)
		}), "no total_count"),
		Entry("past the body bound", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = fmt.Fprintf(w, `{"total_count":0,"pad":"%s"}`, strings.Repeat("p", maxGitHubBodyBytes))
		}), "too large"),
		Entry("a 404, a workflow renamed", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNotFound)
		}), "GitHub answered 404 Not Found"),
		Entry("a 301, which is not followed", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "/elsewhere", http.StatusMovedPermanently)
		}), "GitHub answered 301 Moved Permanently"),
	)

	It("bounds each exchange at dispatchTimeout, the five runs lists together and the dispatch alone: a GitHub "+
		"that never answers costs a reconcile that long, and never wedges it", func(spec SpecContext) {
		// The handler reads the request through, so the server sees the client
		// hang up, and is released at cleanup whatever it saw.
		released := make(chan struct{})
		server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			_, _ = io.Copy(io.Discard, r.Body)
			select {
			case <-r.Context().Done():
			case <-released:
			}
		}))
		DeferCleanup(func() {
			close(released)
			server.CloseClientConnections()
			server.Close()
		})
		d := &dispatcher{baseURL: server.URL}
		for _, exchange := range []func() error{
			func() error { _, err := d.pendingRun(spec, tokenA); return err },
			func() error { _, err := d.dispatch(spec, tokenA, canaryImageTag); return err },
		} {
			start := time.Now()
			err := exchange()
			elapsed := time.Since(start)
			Expect(err).To(MatchError(ContainSubstring("context deadline exceeded")))
			Expect(elapsed).To(BeNumerically(">=", dispatchTimeout))
			Expect(elapsed).To(BeNumerically("<", dispatchTimeout+2*time.Second))
		}
	}, SpecTimeout(3*dispatchTimeout))
})

var _ = Describe("Shipping the dispatch token (D43)", func() {
	It("mounts secret/operator-dispatch-token in the operator Deployment: optional, so the operator runs with it "+
		"absent; read-only, readable by the operator's user alone through its group; with no subPath, so a "+
		"rotation reaches the file; at the directory the operator reads its token from", func() {
		dep := &appsv1.Deployment{}
		decodeOne(manifest("03-operator.yaml"), testImagePrefix, validImageTag, "Deployment", "operator", dep)
		pod := dep.Spec.Template.Spec

		var secretVolumes []corev1.Volume
		for _, v := range pod.Volumes {
			if v.Secret != nil {
				secretVolumes = append(secretVolumes, v)
			}
		}
		Expect(secretVolumes).To(HaveLen(1))
		volume := secretVolumes[0]
		Expect(volume.Secret.SecretName).To(Equal(dispatchTokenSecret))
		Expect(volume.Secret.Optional).To(HaveValue(BeTrue()))
		Expect(volume.Secret.DefaultMode).To(HaveValue(BeEquivalentTo(0o440)))
		Expect(volume.Secret.Items).To(BeEmpty(), "the key token is the file token")

		Expect(pod.Containers).To(HaveLen(1))
		var mounts []corev1.VolumeMount
		for _, m := range pod.Containers[0].VolumeMounts {
			if m.Name == volume.Name {
				mounts = append(mounts, m)
			}
		}
		Expect(mounts).To(Equal([]corev1.VolumeMount{{
			Name: volume.Name, MountPath: filepath.Dir(dispatchTokenPath), ReadOnly: true,
		}}))
		Expect(filepath.Base(dispatchTokenPath)).To(Equal("token"))

		dockerfile, err := os.ReadFile(filepath.Join("..", "..", "Dockerfile"))
		Expect(err).NotTo(HaveOccurred())
		Expect(string(dockerfile)).To(ContainSubstring("\nUSER 65532:65532\n"))
		Expect(pod.SecurityContext.FSGroup).To(HaveValue(BeEquivalentTo(65532)),
			"the file, mode 0440 and owned by root, is read through the operator's group")
	})

	It("names the Secret nowhere but the operator Deployment, in the flattened manifests and the chart alike, and "+
		"grants no read of any Secret: a mount is not an API read", func() {
		chart := filepath.Join("..", "..", "..", "deploy", "helm", "mlobs-operator")
		var naming []string
		for _, root := range []string{filepath.Join(k3sDir, "manifests"), chart} {
			Expect(filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
				if err != nil || d.IsDir() {
					return err
				}
				data, err := os.ReadFile(path)
				Expect(err).NotTo(HaveOccurred())
				if strings.Contains(string(data), dispatchTokenSecret) {
					naming = append(naming, path)
				}
				return nil
			})).To(Succeed())
		}
		Expect(naming).To(ConsistOf(manifest("03-operator.yaml"), filepath.Join(chart, "templates", "03-operator.yaml")))

		role := &rbacv1.Role{}
		decodeOne(manifest("02-operator-rbac.yaml"), testImagePrefix, validImageTag, "Role", "operator", role)
		for _, rule := range role.Rules {
			Expect(rule.Resources).NotTo(ContainElement("secrets"), "%+v", rule)
		}
	})
})

var _ = Describe("The pod owner chain the promote dispatch checks (#108, N1)", func() {
	sd := &servingv1alpha1.ServingDeployment{ObjectMeta: metav1.ObjectMeta{
		Name: servingDeploymentName, Namespace: "mlobs", UID: "1f0e2d3c-4b5a-4968-8776-a5b4c3d2e1f0",
	}}
	controlled := func(dep *appsv1.Deployment) *appsv1.Deployment {
		dep.OwnerReferences = []metav1.OwnerReference{{
			APIVersion: servingv1alpha1.GroupVersion.String(),
			Kind:       reflect.TypeFor[servingv1alpha1.ServingDeployment]().Name(),
			Name:       sd.Name, UID: sd.UID, Controller: new(true),
		}}
		return dep
	}
	pod := func(name string, lbls map[string]string, hash string, owner *metav1.OwnerReference) *corev1.Pod {
		p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "mlobs", Labels: lbls}}
		if hash != "" {
			p.Labels[appsv1.DefaultDeploymentUniqueLabelKey] = hash
		}
		if owner != nil {
			p.OwnerReferences = []metav1.OwnerReference{*owner}
		}
		return p
	}
	owner := func(rs metav1.OwnerReference) *metav1.OwnerReference { return &rs }
	stableDep := func() *appsv1.Deployment { return controlled(newStableDeployment("mlobs", "x")) }
	canaryDep := func() *appsv1.Deployment { return controlled(newCanaryDeployment("mlobs", "x", 1)) }

	DescribeTable("traces a pod to its Deployment by its controller, a ReplicaSet named for the Deployment and "+
		"the pod's own pod-template-hash, and by the Deployment's selector, under a Deployment the "+
		"ServingDeployment controls",
		func(p *corev1.Pod, dep *appsv1.Deployment, want string) {
			err := ownerChain(p, dep, sd)
			if want == "" {
				Expect(err).NotTo(HaveOccurred())
				return
			}
			Expect(err).To(MatchError(want))
		},
		Entry("the stable's pod",
			pod("api-6f7c9d8b5-q2xkz", apiLabels(), "6f7c9d8b5", owner(replicaSetOwner("api-6f7c9d8b5"))),
			stableDep(), ""),
		Entry("the canary's pod",
			pod("api-canary-7b8d9c6f4-m4tnw", canaryLabels(), "7b8d9c6f4", owner(replicaSetOwner("api-canary-7b8d9c6f4"))),
			canaryDep(), ""),
		Entry("no pod selected", (*corev1.Pod)(nil), stableDep(), "no one Running pod was selected"),
		Entry("no Deployment",
			pod("api-6f7c9d8b5-q2xkz", apiLabels(), "6f7c9d8b5", owner(replicaSetOwner("api-6f7c9d8b5"))),
			(*appsv1.Deployment)(nil), "pod/api-6f7c9d8b5-q2xkz's Deployment does not exist"),
		Entry("a Deployment the ServingDeployment does not control",
			pod("api-6f7c9d8b5-q2xkz", apiLabels(), "6f7c9d8b5", owner(replicaSetOwner("api-6f7c9d8b5"))),
			newStableDeployment("mlobs", "x"), "deployment/api is not controlled by servingdeployment/api"),
		Entry("a bare pod, with no controller",
			pod("api-6f7c9d8b5-q2xkz", apiLabels(), "6f7c9d8b5", nil),
			stableDep(), "pod/api-6f7c9d8b5-q2xkz has no controller"),
		Entry("a pod a StatefulSet controls",
			pod("api-0", apiLabels(), "6f7c9d8b5", &metav1.OwnerReference{
				APIVersion: "apps/v1", Kind: "StatefulSet", Name: "api", UID: "u", Controller: new(true),
			}),
			stableDep(), "pod/api-0 is controlled by apps/v1 StatefulSet, not by a ReplicaSet"),
		Entry("a ReplicaSet of another API group",
			pod("api-6f7c9d8b5-q2xkz", apiLabels(), "6f7c9d8b5", &metav1.OwnerReference{
				APIVersion: "example.com/v1", Kind: "ReplicaSet", Name: "api-6f7c9d8b5", UID: "u", Controller: new(true),
			}),
			stableDep(), "pod/api-6f7c9d8b5-q2xkz is controlled by example.com/v1 ReplicaSet, not by a ReplicaSet"),
		Entry("the canary's pod, checked against the stable's Deployment, whose selector it matches",
			pod("api-canary-7b8d9c6f4-m4tnw", canaryLabels(), "7b8d9c6f4", owner(replicaSetOwner("api-canary-7b8d9c6f4"))),
			stableDep(), "pod/api-canary-7b8d9c6f4-m4tnw is controlled by replicaset/api-canary-7b8d9c6f4, not "+
				"deployment/api's replicaset/api-7b8d9c6f4"),
		Entry("a ReplicaSet of another Deployment, its name prefixed by the right one's",
			pod("api-evil-6f7c9d8b5-q2xkz", apiLabels(), "6f7c9d8b5", owner(replicaSetOwner("api-evil-6f7c9d8b5"))),
			stableDep(), "pod/api-evil-6f7c9d8b5-q2xkz is controlled by replicaset/api-evil-6f7c9d8b5, not "+
				"deployment/api's replicaset/api-6f7c9d8b5"),
		Entry("no pod-template-hash label",
			pod("api-6f7c9d8b5-q2xkz", apiLabels(), "", owner(replicaSetOwner("api-6f7c9d8b5"))),
			stableDep(), "pod/api-6f7c9d8b5-q2xkz has no pod-template-hash label"),
		Entry("a hash label that is not its ReplicaSet's",
			pod("api-6f7c9d8b5-q2xkz", apiLabels(), "5d4c3b2a1", owner(replicaSetOwner("api-6f7c9d8b5"))),
			stableDep(), "pod/api-6f7c9d8b5-q2xkz is controlled by replicaset/api-6f7c9d8b5, not deployment/api's "+
				"replicaset/api-5d4c3b2a1"),
		Entry("a pod outside its Deployment's selector",
			pod("api-canary-7b8d9c6f4-m4tnw", apiLabels(), "7b8d9c6f4", owner(replicaSetOwner("api-canary-7b8d9c6f4"))),
			canaryDep(), "pod/api-canary-7b8d9c6f4-m4tnw does not match deployment/api-canary's selector"),
		Entry("the canary's pod passed off as the stable's, its hash label carrying the canary's name: no "+
			"generated hash has a '-' (#121, F6)",
			pod("api-canary-7b8d9c6f4-m4tnw", canaryLabels(), "canary-7b8d9c6f4",
				owner(replicaSetOwner("api-canary-7b8d9c6f4"))),
			stableDep(), `pod/api-canary-7b8d9c6f4-m4tnw's pod-template-hash label "canary-7b8d9c6f4" has a '-', `+
				"which no generated hash has"),
	)

	DescribeTable("ties the canary pod read to the dispatched tag: its api container at the image the tag renders, "+
		"and not being deleted (#121, F1)",
		func(mutate func(p *corev1.Pod), want string) {
			p := pod("api-canary-7b8d9c6f4-m4tnw", canaryLabels(), "7b8d9c6f4",
				owner(replicaSetOwner("api-canary-7b8d9c6f4")))
			p.Spec.Containers = []corev1.Container{
				{Name: "sidecar", Image: "example.com/sidecar:1"},
				{Name: apiContainerName, Image: stableImage(canaryImageTag)},
			}
			if mutate != nil {
				mutate(p)
			}
			err := canaryRuns(p, stableImage(canaryImageTag))
			if want == "" {
				Expect(err).NotTo(HaveOccurred())
				return
			}
			Expect(err).To(MatchError(want))
		},
		Entry("its api container at the tag's image", nil, ""),
		Entry("another tag's image", func(p *corev1.Pod) { p.Spec.Containers[1].Image = stableImage(laterCanaryImageTag) },
			"the canary pod read, pod/api-canary-7b8d9c6f4-m4tnw, runs "+stableImage(laterCanaryImageTag)+", not "+
				stableImage(canaryImageTag)+", the image spec.canaryImageTag renders: what it reported is not the "+
				"dispatched tag's"),
		Entry("the tag's image under another prefix", func(p *corev1.Pod) {
			p.Spec.Containers[1].Image = apiImage(testImagePrefix, canaryImageTag)
		}, "the canary pod read, pod/api-canary-7b8d9c6f4-m4tnw, runs "+apiImage(testImagePrefix, canaryImageTag)+
			", not "+stableImage(canaryImageTag)+", the image spec.canaryImageTag renders: what it reported is not "+
			"the dispatched tag's"),
		Entry("being deleted", func(p *corev1.Pod) { p.DeletionTimestamp = new(metav1.Now()) },
			"the canary pod read, pod/api-canary-7b8d9c6f4-m4tnw, is being deleted: what it reported may be an "+
				"image the canary no longer runs"),
		Entry("no api container", func(p *corev1.Pod) { p.Spec.Containers = p.Spec.Containers[:1] },
			`the canary pod read, pod/api-canary-7b8d9c6f4-m4tnw, has no container named "api"`),
	)

	It("ties no pod at all to the tag", func() {
		Expect(canaryRuns(nil, stableImage(canaryImageTag))).To(MatchError("no one Running canary pod was selected"))
	})
})
