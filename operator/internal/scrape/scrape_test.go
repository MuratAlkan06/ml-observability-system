package scrape

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/MuratAlkan06/ml-observability-system/operator/internal/gate"
)

// The grid the reads must fit well inside (D41, D45).
const gridInterval = 60 * time.Second

// The pod IPs the specs give the window's pods.
const (
	stableIP = "10.42.0.11"
	canaryIP = "10.42.0.12"
	otherIP  = "10.42.0.13"
)

const (
	// appLabel is the label the stack names its workloads' pods by, and
	// apiApp its value on every api pod, the stable's and the canary's.
	appLabel = "app"
	apiApp   = "api"
	// hashLabel is the label a Deployment's ReplicaSet stamps on its pods.
	hashLabel = "pod-template-hash"
	// apiContainer is the api pod's serving container.
	apiContainer = "api"
)

var (
	// The labels a Deployment's pods carry: the stable's are deployment/api's,
	// the canary's deployment/api-canary's, each with its ReplicaSet's hash.
	stableLabels = map[string]string{appLabel: apiApp, hashLabel: "6f7c9d8b5"}
	canaryLabels = map[string]string{appLabel: apiApp, "role": "canary", hashLabel: "7b8d9c6f4"}

	// readySince is when a pod's Ready condition last turned True, unless a
	// spec says otherwise.
	readySince = time.Date(2026, time.October, 12, 12, 1, 30, 0, time.UTC)
)

// stub is every pod's /metrics: one HTTP server standing in for all the pod
// IPs. Its client dials the server whatever address a read asks for, and the
// server tells the pods apart by the Host the request names — the pod IP and
// port the read built — so a read that goes anywhere else is seen.
type stub struct {
	server   *httptest.Server
	client   *http.Client
	mu       sync.Mutex
	answers  map[string][]http.HandlerFunc
	served   map[string]int
	requests []string
}

func newStub() *stub {
	st := &stub{answers: map[string][]http.HandlerFunc{}, served: map[string]int{}}
	st.server = httptest.NewServer(http.HandlerFunc(st.serve))
	target := st.server.Listener.Addr().String()
	var dialer net.Dialer
	transport := &http.Transport{
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, network, target)
		},
	}
	st.client = &http.Client{Transport: transport}
	DeferCleanup(func() {
		transport.CloseIdleConnections()
		st.server.CloseClientConnections()
		st.server.Close()
	})
	return st
}

// answer has the pod at ip answer its reads in turn, the last answer
// repeating.
func (st *stub) answer(ip string, answers ...http.HandlerFunc) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.answers[net.JoinHostPort(ip, "8000")] = answers
}

func (st *stub) serve(w http.ResponseWriter, r *http.Request) {
	st.mu.Lock()
	st.requests = append(st.requests, r.Method+" "+r.Host+r.URL.Path)
	answers, n := st.answers[r.Host], st.served[r.Host]
	st.served[r.Host]++
	st.mu.Unlock()
	if len(answers) == 0 {
		http.NotFound(w, r)
		return
	}
	answers[min(n, len(answers)-1)](w, r)
}

// seen is the requests the stub has served, "<method> <host:port><path>".
func (st *stub) seen() []string {
	st.mu.Lock()
	defer st.mu.Unlock()
	return append([]string(nil), st.requests...)
}

// metricsRequest is the request a read of the pod at ip makes.
func metricsRequest(ip string) string { return "GET " + ip + ":8000/metrics" }

// servePage answers with a /metrics page, as the api's route does.
func servePage(text string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		_, _ = io.WriteString(w, text)
	}
}

func serveStatus(code int) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(code) }
}

// hang never answers: it returns once the reader has given up on the
// request, and says so on abandoned when that is not nil.
func hang(abandoned chan<- struct{}) http.HandlerFunc {
	return func(_ http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
		if abandoned != nil {
			abandoned <- struct{}{}
		}
	}
}

// hangMidPage answers its headers and the first line of a page, then never
// finishes it.
func hangMidPage(w http.ResponseWriter, r *http.Request) {
	_, _ = io.WriteString(w, `mlobs_http_requests_total{endpoint="/predict",method="POST",status="200"} 10.0`+"\n")
	w.(http.Flusher).Flush()
	<-r.Context().Done()
}

// predictPage is a /metrics page in prometheus-client's form for a pod that
// has served n /predict requests, all of them 200s and every one between 0.1 s
// and 0.25 s: two pods served alike have a p95 ratio of 1.
func predictPage(n float64) string {
	var b strings.Builder
	fmt.Fprintf(&b, "mlobs_http_requests_total{endpoint=\"/predict\",method=\"POST\",status=\"200\"} %.1f\n", n)
	for _, le := range []string{"0.1", "0.25", "0.5", "+Inf"} {
		c := n
		if le == "0.1" {
			c = 0
		}
		fmt.Fprintf(&b, "mlobs_http_request_duration_seconds_bucket{endpoint=\"/predict\",le=\"%s\"} %.1f\n", le, c)
	}
	return b.String()
}

func newNamespace() string {
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "scrape-"}}
	Expect(k8sClient.Create(ctx, ns)).To(Succeed())
	return ns.Name
}

// createPod creates a pod of the api's shape in ns under lbls and writes the
// status a kubelet reports for it once it runs: Running at ip, its init
// container done, its api container running, and Ready since readySince.
// Each mutate edits that status before it is written.
func createPod(ns, name string, lbls map[string]string, ip string, mutate ...func(*corev1.PodStatus)) *corev1.Pod {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: lbls},
		Spec: corev1.PodSpec{
			InitContainers: []corev1.Container{{Name: "wait-for-redis", Image: "example.com/busybox:test"}},
			Containers: []corev1.Container{{
				Name:  apiContainer,
				Image: "example.com/mlobs-api:test",
				Ports: []corev1.ContainerPort{{ContainerPort: MetricsPort}},
			}},
		},
	}
	Expect(k8sClient.Create(ctx, pod)).To(Succeed())
	pod.Status = corev1.PodStatus{
		Phase:  corev1.PodRunning,
		PodIP:  ip,
		PodIPs: []corev1.PodIP{{IP: ip}},
		Conditions: []corev1.PodCondition{{
			Type: corev1.PodReady, Status: corev1.ConditionTrue, LastTransitionTime: metav1.NewTime(readySince),
		}},
		InitContainerStatuses: []corev1.ContainerStatus{{
			Name:  "wait-for-redis",
			Image: "example.com/busybox:test",
			State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "Completed"}},
		}},
		ContainerStatuses: []corev1.ContainerStatus{{
			Name:  apiContainer,
			Image: "example.com/mlobs-api:test",
			Ready: true,
			State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
		}},
	}
	for _, m := range mutate {
		m(&pod.Status)
	}
	Expect(k8sClient.Status().Update(ctx, pod)).To(Succeed())
	return pod
}

func phase(p corev1.PodPhase) func(*corev1.PodStatus) {
	return func(s *corev1.PodStatus) { s.Phase = p }
}

// ready sets the Ready condition to status since t.
func ready(status corev1.ConditionStatus, t time.Time) func(*corev1.PodStatus) {
	return func(s *corev1.PodStatus) {
		s.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: status, LastTransitionTime: metav1.NewTime(t)}}
	}
}

// terminated sets a terminated state on the first container, or the first
// init container, current or last, with reason.
func terminated(init, last bool, reason string) func(*corev1.PodStatus) {
	return func(s *corev1.PodStatus) {
		cs := &s.ContainerStatuses[0]
		if init {
			cs = &s.InitContainerStatuses[0]
		}
		t := &corev1.ContainerStateTerminated{Reason: reason, ExitCode: 137}
		if last {
			cs.LastTerminationState = corev1.ContainerState{Terminated: t}
			cs.RestartCount = 1
		} else {
			cs.State = corev1.ContainerState{Terminated: t}
			cs.Ready = false
		}
	}
}

// identity is the gate's identity of pod.
func identity(pod *corev1.Pod) gate.Pod { return gate.Pod{Name: pod.Name, UID: string(pod.UID)} }

// withPod is sample read from pod.
func withPod(sample gate.Sample, pod *corev1.Pod) *gate.Sample {
	sample.Pod = identity(pod)
	return &sample
}

var _ = Describe("Reading the window's two pods at a boundary", func() {
	var (
		ns string
		st *stub
		s  *Scraper
	)

	BeforeEach(func() {
		ns = newNamespace()
		st = newStub()
		// k8sClient is a plain client.New, uncached: the kind of reader the
		// manager's APIReader is.
		s = &Scraper{Reader: k8sClient, HTTP: st.client}
	})

	It("lists each pod by watch-window.sh's selector and reads the one Running pod's /metrics at its pod IP on 8000", func() {
		stable := createPod(ns, "api-6f7c9d8b5-q2xkz", stableLabels, stableIP)
		canary := createPod(ns, "api-canary-7b8d9c6f4-m4tnw", canaryLabels, canaryIP)
		// Neither of these is read: an old stable pod that has stopped, and
		// another workload's pod.
		createPod(ns, "api-5d4c3b2a1-x7wq9", map[string]string{appLabel: apiApp, hashLabel: "5d4c3b2a1"}, otherIP,
			phase(corev1.PodSucceeded))
		createPod(ns, "redis-0", map[string]string{appLabel: "redis"}, "10.42.0.14")
		st.answer(stableIP, servePage(page("stable-start")))
		st.answer(canaryIP, servePage(page("canary-start")))
		st.answer(otherIP, serveStatus(http.StatusTeapot))

		b := s.ReadBoundary(ctx, ns)
		Expect(b.StableErr).NotTo(HaveOccurred())
		Expect(b.CanaryErr).NotTo(HaveOccurred())
		Expect(b.Current.Stable).To(Equal(withPod(parsed(page("stable-start")), stable)))
		Expect(b.Current.Canary).To(Equal(withPod(parsed(page("canary-start")), canary)))
		Expect(b.Current.Stable.Pod.UID).NotTo(BeEmpty())
		By("no read failed, so nothing is read again: the next interval's baseline is this read")
		Expect(b.Baseline).To(Equal(b.Current))
		Expect(st.seen()).To(Equal([]string{metricsRequest(stableIP), metricsRequest(canaryIP)}))
		Expect(b.CanaryReadyAt).To(HaveValue(BeTemporally("==", readySince)))
		Expect(b.CanaryOOMKilled).To(BeFalse())
	})

	It("holds its selectors, port and read bound to watch-window.sh's own", func() {
		data, err := os.ReadFile(filepath.Join("..", "..", "..", "deploy", "k3s", "watch-window.sh"))
		Expect(err).NotTo(HaveOccurred())
		script := string(data)
		Expect(script).To(ContainSubstring("declare -A pod_selector=([stable]='%s' [canary]='%s')",
			selectors[RoleStable], selectors[RoleCanary]))
		Expect(script).To(ContainSubstring(`open_forward "pod/${pods[0]}" "$port" %d /metrics`, MetricsPort))
		Expect(script).To(ContainSubstring("--request-timeout=%ds", int(ReadTimeout.Seconds())))
		Expect(script).To(ContainSubstring(`curl -fsS --max-time %d "http://127.0.0.1:${port}/metrics"`,
			int(ReadTimeout.Seconds())))
	})

	DescribeTable("a role without exactly one Running pod fails its read, the count in the reason",
		func(setup func(), failed Role, reason string) {
			setup()
			st.answer(stableIP, servePage(predictPage(100)))
			st.answer(canaryIP, servePage(predictPage(100)))

			b := s.ReadBoundary(ctx, ns)
			sample, err := b.Current.Stable, b.StableErr
			if failed == RoleCanary {
				sample, err = b.Current.Canary, b.CanaryErr
			}
			Expect(sample).To(BeNil())
			Expect(err).To(MatchError(reason))
			By("read once more for the next interval, and failed again: a nil baseline")
			if failed == RoleCanary {
				Expect(b.Baseline.Canary).To(BeNil())
			} else {
				Expect(b.Baseline.Stable).To(BeNil())
			}
		},
		Entry("no stable pod", func() {
			createPod(ns, "api-canary-7b8d9c6f4-m4tnw", canaryLabels, canaryIP)
		}, RoleStable, "0 Running stable pods, not 1"),
		Entry("two Running canary pods, mid-rollout", func() {
			createPod(ns, "api-6f7c9d8b5-q2xkz", stableLabels, stableIP)
			createPod(ns, "api-canary-7b8d9c6f4-m4tnw", canaryLabels, canaryIP)
			createPod(ns, "api-canary-7b8d9c6f4-z9hrp", canaryLabels, otherIP)
		}, RoleCanary, "2 Running canary pods, not 1"),
		Entry("a canary pod not yet Running", func() {
			createPod(ns, "api-6f7c9d8b5-q2xkz", stableLabels, stableIP)
			createPod(ns, "api-canary-7b8d9c6f4-m4tnw", canaryLabels, "", func(s *corev1.PodStatus) {
				*s = corev1.PodStatus{Phase: corev1.PodPending}
			})
		}, RoleCanary, "0 Running canary pods, not 1"),
	)

	It("takes the canary's Ready time as the latest Ready transition among its pods that are Ready, "+
		"whatever their phase, as canary_ready does", func() {
		createPod(ns, "api-6f7c9d8b5-q2xkz", stableLabels, stableIP, ready(corev1.ConditionTrue, readySince.Add(time.Hour)))
		createPod(ns, "api-canary-7b8d9c6f4-m4tnw", canaryLabels, canaryIP, ready(corev1.ConditionTrue, readySince))
		replacement := readySince.Add(4 * time.Minute)
		createPod(ns, "api-canary-7b8d9c6f4-z9hrp", canaryLabels, otherIP, ready(corev1.ConditionTrue, replacement))
		createPod(ns, "api-canary-7b8d9c6f4-k2lmn", canaryLabels, "10.42.0.15",
			phase(corev1.PodPending), ready(corev1.ConditionFalse, readySince.Add(5*time.Minute)))

		b := s.ReadBoundary(ctx, ns)
		Expect(b.CanaryErr).To(MatchError("2 Running canary pods, not 1"))
		Expect(b.CanaryReadyAt).To(HaveValue(BeTemporally("==", replacement)))
	})

	It("has no canary Ready time when no canary pod is Ready, nor when the canary pods cannot be listed", func() {
		createPod(ns, "api-canary-7b8d9c6f4-m4tnw", canaryLabels, canaryIP, ready(corev1.ConditionFalse, readySince))
		st.answer(canaryIP, servePage(predictPage(100)))
		b := s.ReadBoundary(ctx, ns)
		Expect(b.CanaryErr).NotTo(HaveOccurred())
		Expect(b.CanaryReadyAt).To(BeNil())

		canceled, cancelRead := context.WithCancel(ctx)
		cancelRead()
		b = s.ReadBoundary(canceled, ns)
		Expect(b.CanaryErr).To(MatchError(ContainSubstring("the canary pod list is unreadable")))
		Expect(b.CanaryReadyAt).To(BeNil())
		Expect(b.Current).To(Equal(gate.Samples{}))
		Expect(b.Baseline).To(Equal(gate.Samples{}))
	})

	DescribeTable("reads a canary OOM kill from its container status, current or last, init containers included",
		func(stableStatus, canaryStatus func(*corev1.PodStatus), want bool) {
			createPod(ns, "api-6f7c9d8b5-q2xkz", stableLabels, stableIP, stableStatus)
			createPod(ns, "api-canary-7b8d9c6f4-m4tnw", canaryLabels, canaryIP, canaryStatus)
			Expect(s.ReadBoundary(ctx, ns).CanaryOOMKilled).To(Equal(want))
		},
		Entry("last terminated OOMKilled, restarted since", phase(corev1.PodRunning),
			terminated(false, true, "OOMKilled"), true),
		Entry("terminated OOMKilled now, not yet restarted", phase(corev1.PodRunning),
			terminated(false, false, "OOMKilled"), true),
		Entry("an init container OOMKilled", phase(corev1.PodRunning), terminated(true, false, "OOMKilled"), true),
		Entry("a container that exited on an error is not an OOM kill", phase(corev1.PodRunning),
			terminated(false, true, "Error"), false),
		Entry("the stable's OOM kill is not the canary's (D46)", terminated(false, true, "OOMKilled"),
			phase(corev1.PodRunning), false),
	)

	// The specs whose stubs never answer run under a SpecTimeout and read
	// under its context, so a read that lost its own bound fails the spec
	// instead of hanging the suite.
	It("ends a /metrics read that never answers at ReadTimeout, under the 60 s grid, as a nil sample: "+
		"an unevaluable interval, and the read abandoned", func(spec SpecContext) {
		canary := createPod(ns, "api-canary-7b8d9c6f4-m4tnw", canaryLabels, canaryIP)
		abandoned := make(chan struct{}, 1)
		st.answer(canaryIP, hang(abandoned))

		start := time.Now()
		sample, pods, err := s.read(spec, ns, RoleCanary)
		elapsed := time.Since(start)
		Expect(sample).To(BeNil())
		Expect(err).To(MatchError(context.DeadlineExceeded))
		Expect(err).To(MatchError(ContainSubstring("canary pod/" + canary.Name + " /metrics unreadable")))
		Expect(pods).To(HaveLen(1), "the list was read; only /metrics failed")
		Expect(elapsed).To(BeNumerically(">=", ReadTimeout))
		Expect(elapsed).To(BeNumerically("<", ReadTimeout+2*time.Second))
		Expect(ReadTimeout).To(BeNumerically("<", gridInterval))
		Eventually(abandoned).Should(Receive(), "the stub never saw the read give up")

		By("the engine counts the failed read as an unevaluable interval")
		stablePod := gate.Pod{Name: "api-6f7c9d8b5-q2xkz", UID: "0d6f3b52-1c1e-4a8e-9d7a-2f4b6c8e0a11"}
		stableAt := func(n float64) *gate.Sample {
			sample := parsed(predictPage(n))
			sample.Pod = stablePod
			return &sample
		}
		at := readySince.Add(10 * time.Minute)
		rec := gate.Session{CanaryReadySeen: true, CanaryPod: identity(canary)}.Evaluate(gate.Interval{
			Start:         at,
			End:           at.Add(time.Minute),
			Baseline:      gate.Samples{Stable: stableAt(1000), Canary: withPod(parsed(predictPage(500)), canary)},
			Current:       gate.Samples{Stable: stableAt(1150), Canary: sample},
			CanaryReadyAt: &readySince,
		})
		Expect(rec.Verdict).To(Equal(gate.VerdictUnevaluable))
		Expect(rec.Reasons).To(Equal([]gate.Reason{gate.ReasonCanaryReadFailed}))
	}, SpecTimeout(gridInterval))

	It("always returns from a boundary: with both pods hung, it ends within four reads' time, every sample nil",
		func(spec SpecContext) {
			s.timeout = 300 * time.Millisecond
			createPod(ns, "api-6f7c9d8b5-q2xkz", stableLabels, stableIP)
			createPod(ns, "api-canary-7b8d9c6f4-m4tnw", canaryLabels, canaryIP)
			st.answer(stableIP, hangMidPage)
			st.answer(canaryIP, hang(nil))

			start := time.Now()
			b := s.ReadBoundary(spec, ns)
			Expect(time.Since(start)).To(BeNumerically("<", 4*s.timeout+2*time.Second))
			Expect(b.Current).To(Equal(gate.Samples{}))
			Expect(b.Baseline).To(Equal(gate.Samples{}))
			Expect(b.StableErr).To(MatchError(context.DeadlineExceeded))
			Expect(b.CanaryErr).To(MatchError(context.DeadlineExceeded))
			Expect(st.seen()).To(Equal([]string{
				metricsRequest(stableIP), metricsRequest(canaryIP), metricsRequest(stableIP), metricsRequest(canaryIP),
			}))
			By("the compiled bound: a boundary's four reads fit well inside the grid")
			Expect(4 * ReadTimeout).To(BeNumerically("<=", gridInterval/3))
		}, SpecTimeout(gridInterval))

	It("re-reads a failed pod right away for the next interval's baseline, "+
		"so that one failed read leaves one interval unevaluable rather than two", func() {
		stable := createPod(ns, "api-6f7c9d8b5-q2xkz", stableLabels, stableIP)
		canary := createPod(ns, "api-canary-7b8d9c6f4-m4tnw", canaryLabels, canaryIP)
		// Each pod serves 150 /predict requests a minute. The canary's read at
		// the second boundary fails once; the read right after it does not.
		st.answer(stableIP,
			servePage(predictPage(1000)), servePage(predictPage(1150)), servePage(predictPage(1300)))
		st.answer(canaryIP,
			servePage(predictPage(500)), serveStatus(http.StatusServiceUnavailable),
			servePage(predictPage(650)), servePage(predictPage(800)))

		first := s.ReadBoundary(ctx, ns)
		second := s.ReadBoundary(ctx, ns)
		third := s.ReadBoundary(ctx, ns)

		Expect(second.Current.Canary).To(BeNil())
		Expect(second.CanaryErr).To(MatchError(ContainSubstring("503 Service Unavailable")))
		Expect(second.Baseline.Canary).To(Equal(withPod(parsed(predictPage(650)), canary)))
		Expect(second.Baseline.Stable).To(Equal(second.Current.Stable))
		Expect(st.seen()).To(Equal([]string{
			metricsRequest(stableIP), metricsRequest(canaryIP), // the first boundary
			metricsRequest(stableIP), metricsRequest(canaryIP), metricsRequest(canaryIP), // the second, one retry
			metricsRequest(stableIP), metricsRequest(canaryIP), // the third
		}))
		Expect(third.Current.Stable).To(Equal(withPod(parsed(predictPage(1300)), stable)))

		// The two intervals the boundaries close, folded by the engine.
		at := readySince.Add(10 * time.Minute)
		intervals := func(secondBaseline gate.Samples) []gate.Interval {
			return []gate.Interval{
				{Start: at, End: at.Add(time.Minute), Baseline: first.Baseline, Current: second.Current,
					CanaryReadyAt: second.CanaryReadyAt},
				{Start: at.Add(time.Minute), End: at.Add(2 * time.Minute), Baseline: secondBaseline,
					Current: third.Current, CanaryReadyAt: third.CanaryReadyAt},
			}
		}
		fold := func(ivs []gate.Interval) ([]gate.Verdict, []gate.Decision) {
			session := gate.Session{CanaryReadySeen: true, CanaryPod: identity(canary)}
			verdicts := make([]gate.Verdict, 0, len(ivs))
			decisions := make([]gate.Decision, 0, len(ivs))
			for _, iv := range ivs {
				rec := session.Evaluate(iv)
				var d gate.Decision
				session, d = session.Fold(rec)
				verdicts = append(verdicts, rec.Verdict)
				decisions = append(decisions, d)
			}
			return verdicts, decisions
		}

		By("with the retry: one unevaluable interval, then a clean one, and nothing trips")
		verdicts, decisions := fold(intervals(second.Baseline))
		Expect(verdicts).To(Equal([]gate.Verdict{gate.VerdictUnevaluable, gate.VerdictClean}))
		Expect(decisions).To(Equal([]gate.Decision{{}, {}}))

		By("without it, the failed read would have cost the next interval its baseline too, and tripped line 3")
		verdicts, decisions = fold(intervals(second.Current))
		Expect(verdicts).To(Equal([]gate.Verdict{gate.VerdictUnevaluable, gate.VerdictUnevaluable}))
		Expect(decisions[1].Rollback).To(Equal(gate.TripCanaryLatency))
	})
})

// operatorNamespace is the one namespace the operator's Role is shaped for
// (D33).
const operatorNamespace = "mlobs"

// applyOperatorRBAC creates every object in the flattened RBAC file apply.sh
// deploys — the ServiceAccount, the RoleBinding and controller-gen's Role —
// so the grant under test is the one that ships.
func applyOperatorRBAC() {
	f, err := os.Open(filepath.Join("..", "..", "..", "deploy", "k3s", "manifests", "02-operator-rbac.yaml"))
	Expect(err).NotTo(HaveOccurred())
	defer func() { _ = f.Close() }()
	decoder := utilyaml.NewYAMLOrJSONDecoder(f, 4096)
	kinds := map[string]bool{}
	for {
		var doc map[string]any
		err := decoder.Decode(&doc)
		if errors.Is(err, io.EOF) {
			break
		}
		Expect(err).NotTo(HaveOccurred())
		if len(doc) == 0 {
			continue // the file's comment head
		}
		obj := &unstructured.Unstructured{Object: doc}
		Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, obj))).To(Succeed())
		kinds[obj.GetKind()] = true
	}
	Expect(kinds).To(Equal(map[string]bool{"ServiceAccount": true, "RoleBinding": true, "Role": true}))
}

var _ = Describe("Reading the pods as the operator, under the Role that ships", func() {
	It("reads both pods through a manager's uncached APIReader, under a Role with get and list on pods "+
		"and neither watch nor pods/proxy (D41)", func() {
		Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx,
			&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: operatorNamespace}}))).To(Succeed())
		applyOperatorRBAC()

		operatorCfg := rest.CopyConfig(cfg)
		operatorCfg.Impersonate = rest.ImpersonationConfig{UserName: "system:serviceaccount:mlobs:operator"}
		asOperator, err := client.New(operatorCfg, client.Options{Scheme: scheme.Scheme})
		Expect(err).NotTo(HaveOccurred())

		By("the API server's answers, as e2e.sh's can-i asks them")
		for _, c := range []struct {
			verb, subresource string
			allowed           bool
		}{
			{"get", "", true}, {"list", "", true}, {"watch", "", false}, {"get", "proxy", false},
		} {
			review := &authorizationv1.SelfSubjectAccessReview{Spec: authorizationv1.SelfSubjectAccessReviewSpec{
				ResourceAttributes: &authorizationv1.ResourceAttributes{
					Namespace: operatorNamespace, Verb: c.verb, Resource: "pods", Subresource: c.subresource,
				},
			}}
			Expect(asOperator.Create(ctx, review)).To(Succeed())
			Expect(review.Status.Allowed).To(Equal(c.allowed), "can-i %s pods %s", c.verb, c.subresource)
		}

		By("a manager built as main.go builds it, as the operator, reads both pods through GetAPIReader()")
		stable := createPod(operatorNamespace, "api-6f7c9d8b5-q2xkz", stableLabels, stableIP)
		canary := createPod(operatorNamespace, "api-canary-7b8d9c6f4-m4tnw", canaryLabels, canaryIP)
		st := newStub()
		st.answer(stableIP, servePage(page("stable-end")))
		st.answer(canaryIP, servePage(page("canary-end")))
		mgr, err := ctrl.NewManager(operatorCfg, ctrl.Options{
			Scheme:  scheme.Scheme,
			Metrics: metricsserver.Options{BindAddress: "0"},
			Cache:   cache.Options{DefaultNamespaces: map[string]cache.Config{operatorNamespace: {}}},
		})
		Expect(err).NotTo(HaveOccurred())
		s := &Scraper{Reader: mgr.GetAPIReader(), HTTP: st.client}

		b := s.ReadBoundary(ctx, operatorNamespace)
		Expect(b.StableErr).NotTo(HaveOccurred())
		Expect(b.CanaryErr).NotTo(HaveOccurred())
		Expect(b.Current.Stable).To(Equal(withPod(parsed(page("stable-end")), stable)))
		Expect(b.Current.Canary).To(Equal(withPod(parsed(page("canary-end")), canary)))
		Expect(b.CanaryReadyAt).To(HaveValue(BeTemporally("==", readySince)))

		By("and the Role is namespaced: the same reads in another namespace are refused")
		other := newNamespace()
		b = s.ReadBoundary(ctx, other)
		Expect(apierrors.IsForbidden(b.StableErr)).To(BeTrue(), "got %v", b.StableErr)
		Expect(apierrors.IsForbidden(b.CanaryErr)).To(BeTrue(), "got %v", b.CanaryErr)
		Expect(b.Current).To(Equal(gate.Samples{}))
		Expect(st.seen()).To(HaveLen(2), "a refused list reads no /metrics")
	})
})
