package controller

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	servingv1alpha1 "github.com/MuratAlkan06/ml-observability-system/operator/api/v1alpha1"
)

// ActPromote's dispatch against a stand-in for GitHub's REST API (docs/PLAN.md
// D43, D47; #108, N1), on the gate's harness: a fake clock, scripted /metrics,
// pods whose status and owners are written directly, and the dispatcher the
// operator runs, its token read from a file and its calls made through
// dispatchClient to an httptest server.

// The two paths the dispatch calls, written out: they are the contract with
// GitHub, not something to rebuild from the constants under test.
const (
	runsPath       = "/repos/MuratAlkan06/ml-observability-system/actions/workflows/deploy.yml/runs"
	dispatchesPath = "/repos/MuratAlkan06/ml-observability-system/actions/workflows/deploy.yml/dispatches"
)

// statusQuery is the runs list's status filter; statusSubresource is the
// subresource a status write goes to.
const (
	statusQuery       = "status"
	statusSubresource = "status"
)

// The tokens the specs mount: fine-grained PAT-shaped, and fake.
const (
	tokenA = "github_pat_11AAAAAAA0aaaaaaaaaaaa_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	tokenB = "github_pat_22CCCCCCC0cccccccccccc_ddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
)

// githubRequest is one request the stand-in served.
type githubRequest struct {
	method, path, status, auth, accept, version, contentType, body string
}

// githubStub stands in for GitHub: it answers a Deploy runs list with the
// count a spec sets for its status, and a dispatch with the status a spec
// sets, and records every request.
type githubStub struct {
	server *httptest.Server

	mu       sync.Mutex
	requests []githubRequest
	// pending is each status's run count; absent is 0.
	pending map[string]int
	// runsCode, when set, answers every runs list instead of a page.
	runsCode int
	// dispatchCode answers a dispatch; 0 is 204, GitHub's answer under the
	// pinned API version.
	dispatchCode int
	// hangUp drops a dispatch's connection with no answer.
	hangUp bool
	// onDispatch runs when a dispatch arrives, before it is answered.
	onDispatch func()
}

func newGitHubStub() *githubStub {
	gh := &githubStub{pending: map[string]int{}}
	gh.server = httptest.NewServer(http.HandlerFunc(gh.serve))
	DeferCleanup(gh.server.Close)
	return gh
}

func (gh *githubStub) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	gh.mu.Lock()
	gh.requests = append(gh.requests, githubRequest{
		method: r.Method, path: r.URL.Path, status: r.URL.Query().Get(statusQuery),
		auth: r.Header.Get("Authorization"), accept: r.Header.Get("Accept"),
		version: r.Header.Get("X-GitHub-Api-Version"), contentType: r.Header.Get("Content-Type"), body: string(body),
	})
	pending, runsCode, dispatchCode, hangUp, onDispatch := gh.pending[r.URL.Query().Get(statusQuery)], gh.runsCode,
		gh.dispatchCode, gh.hangUp, gh.onDispatch
	gh.mu.Unlock()

	switch {
	case r.Method == http.MethodGet && r.URL.Path == runsPath:
		if runsCode != 0 {
			w.WriteHeader(runsCode)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"total_count":%d,"workflow_runs":[]}`, pending)
	case r.Method == http.MethodPost && r.URL.Path == dispatchesPath:
		if onDispatch != nil {
			onDispatch()
		}
		if hangUp {
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				_ = conn.Close()
			}
			return
		}
		code := dispatchCode
		if code == 0 {
			code = http.StatusNoContent
		}
		if code >= 300 && code < 400 {
			// Followed, the dispatch would be sent again, here.
			w.Header().Set("Location", "/elsewhere")
		}
		w.WriteHeader(code)
	default:
		http.NotFound(w, r)
	}
}

// set changes the stand-in's answers under its lock.
func (gh *githubStub) set(change func(gh *githubStub)) {
	gh.mu.Lock()
	defer gh.mu.Unlock()
	change(gh)
}

// all is every request served so far.
func (gh *githubStub) all() []githubRequest {
	gh.mu.Lock()
	defer gh.mu.Unlock()
	return append([]githubRequest(nil), gh.requests...)
}

// served is the requests served so far, filtered to method.
func (gh *githubStub) served(method string) []githubRequest {
	var out []githubRequest
	for _, req := range gh.all() {
		if req.method == method {
			out = append(out, req)
		}
	}
	return out
}

func (gh *githubStub) dispatches() []githubRequest { return gh.served(http.MethodPost) }
func (gh *githubStub) runLists() []githubRequest   { return gh.served(http.MethodGet) }

// writeToken writes token to path as the kubelet projects a Secret's key, a
// trailing newline and all.
func writeToken(path, token string) {
	Expect(os.WriteFile(path, []byte(token+"\n"), 0o600)).To(Succeed())
}

// armDispatch gives h's reconciler the dispatcher the operator runs, its
// token tokenA in a file of the spec's own and its API the stand-in, and
// returns both.
func armDispatch(h *gateHarness) (*githubStub, string) {
	gh := newGitHubStub()
	tokenFile := filepath.Join(GinkgoT().TempDir(), "token")
	writeToken(tokenFile, tokenA)
	h.r.dispatcher = &dispatcher{baseURL: gh.server.URL, tokenPath: tokenFile}
	return gh, tokenFile
}

// newPromoteHarness is a gate armed in ActPromote for the canary's tag, its
// canary Ready since a minute before the window, so that interval 10 is the
// promote verdict and boundary 11 its dispatch, and its window open.
func newPromoteHarness() (*gateHarness, *githubStub, string) {
	h := newGateHarness(actPromote)
	gh, tokenFile := armDispatch(h)
	h.addCanary(at(windowStart.Add(-time.Minute)))
	h.open()
	return h, gh, tokenFile
}

// dispatchOf is a copy of status.gate's promote verdict's dispatch, nil
// without one, its time in UTC as the specs write theirs: the API server
// hands times back in the local zone.
func dispatchOf(st *servingv1alpha1.GateStatus) *servingv1alpha1.GateDispatch {
	if st.PromoteVerdict == nil || st.PromoteVerdict.Dispatch == nil {
		return nil
	}
	d := *st.PromoteVerdict.Dispatch
	if d.AttemptedAt != nil {
		d.AttemptedAt = new(metav1.NewTime(d.AttemptedAt.UTC()))
	}
	return &d
}

// dispatchBody is the body of the one dispatch: deploy.yml on main, sha tag.
func dispatchBody(tag string) string {
	return `{"inputs":{"sha":"` + tag + `"},"ref":"main"}`
}

// expectGitHubHeaders asserts req carried token and GitHub's headers.
func expectGitHubHeaders(req githubRequest, token string) {
	GinkgoHelper()
	Expect(req.auth).To(Equal("Bearer " + token))
	Expect(req.accept).To(Equal("application/vnd.github+json"))
	Expect(req.version).To(Equal("2022-11-28"))
}

// setTagPair moves the open window's canary to tag and arms spec.gate for it,
// in one host-side merge patch with no resourceVersion precondition: a human
// changing the canary of a window open under ActPromote.
func setTagPair(sd *servingv1alpha1.ServingDeployment, tag string) {
	body := fmt.Sprintf(`{"spec":{"canaryImageTag":%q,"gate":{"forCanaryImageTag":%q}}}`, tag, tag)
	target := &servingv1alpha1.ServingDeployment{ObjectMeta: metav1.ObjectMeta{Namespace: sd.Namespace, Name: sd.Name}}
	Expect(k8sClient.Patch(ctx, target, client.RawPatch(types.MergePatchType, []byte(body)))).To(Succeed())
}

// markDeleting deletes pod while a finalizer holds it, as a pod stays while
// its containers stop: its deletionTimestamp set, its status as it was.
// envtest has no kubelet to finish a deletion; the finalizer stands in for it,
// and is removed when the spec ends.
func markDeleting(pod *corev1.Pod) {
	latest := &corev1.Pod{}
	Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(pod), latest)).To(Succeed())
	latest.Finalizers = append(latest.Finalizers, "test.mlobs.dev/terminating")
	Expect(k8sClient.Update(ctx, latest)).To(Succeed())
	Expect(k8sClient.Delete(ctx, latest)).To(Succeed())
	DeferCleanup(func() {
		held := &corev1.Pod{}
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(pod), held)).To(Succeed())
		held.Finalizers = nil
		Expect(k8sClient.Update(ctx, held)).To(Succeed())
	})
	Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(pod), latest)).To(Succeed())
	Expect(latest.DeletionTimestamp).NotTo(BeNil())
	Expect(latest.Status.Phase).To(Equal(corev1.PodRunning))
}

// staleCanaryEvent is the Event of the verdict at interval 10 turned down
// because the canary pod read is not the dispatched tag's, for why.
func staleCanaryEvent(why string) string {
	return "Warning GatePromoteNotDispatched the promote verdict of interval 10 is not dispatched: the canary " +
		"pod read, pod/api-canary-7b8d9c6f4-m4tnw, " + why + "; it stays recorded, and a human actuates it"
}

// dispatchedEvent is the Event of a verdict at interval GitHub accepted the
// dispatch of, sent at sentAt.
func dispatchedEvent(interval int32, sentAt time.Time) string {
	return fmt.Sprintf("Normal GatePromoteDispatched interval %d completed 11 consecutive clean intervals, the "+
		"promote verdict: in ActPromote the gate dispatched deploy.yml on main with sha %s at %s, and GitHub "+
		"accepted it (204); the pipeline promotes, its guards and approval unchanged, and its close-window patch "+
		"closes the window; the run's URL is recorded by hand", interval, canaryImageTag, stamp(sentAt))
}

var _ = Describe("The gate in ActPromote: the promote dispatch (D43, D47)", func() {
	It("latches the verdict, then dispatches deploy.yml once, on main with the canary's 40-hex tag as its sha "+
		"input: the latch is in the API server before the dispatch reaches GitHub, and GitHub's 204 is "+
		"written after", func() {
		h, gh, _ := newPromoteHarness()
		Expect(h.gateEvents()).To(ConsistOf("Normal GateArmed the gate is armed in ActPromote for canary tag " +
			canaryImageTag + ": every 1m0s interval from 2026-09-29T12:00:00Z is recorded in status.gate, and a " +
			"tripped line rolls the window back, and the promote verdict is dispatched to deploy.yml at most once"))
		st := h.gate()
		Expect(st.Mode).To(Equal(levelActPromote))
		Expect(st.Arming).To(Equal(servingv1alpha1.GateArmingArmed))
		h.drainOpenEvents()
		writesOpen := writeCount(h.writes)

		By("ten clean intervals: no verdict, and nothing asked of GitHub")
		h.clean(10)
		Expect(h.gate().PromoteVerdict).To(BeNil())
		Expect(gh.all()).To(BeEmpty())

		By("the eleventh is the verdict: the latch lands, then the dispatch is sent")
		var atDispatch *servingv1alpha1.GateDispatch
		var readErr error
		gh.set(func(gh *githubStub) {
			gh.onDispatch = func() {
				latest := &servingv1alpha1.ServingDeployment{}
				if readErr = k8sClient.Get(ctx, client.ObjectKeyFromObject(h.sd), latest); readErr == nil {
					atDispatch = dispatchOf(latest.Status.Gate)
				}
			}
		})
		Expect(h.next(perInterval, 0, 0).RequeueAfter).To(Equal(time.Minute))
		Expect(readErr).NotTo(HaveOccurred())
		sentAt := h.boundaryTime(11)
		Expect(atDispatch).To(Equal(&servingv1alpha1.GateDispatch{
			State: servingv1alpha1.GateDispatchAttempted, AttemptedAt: new(metav1.NewTime(sentAt)), SHA: canaryImageTag,
		}), "what the API server held when the dispatch reached GitHub: the latch")

		st = h.gate()
		Expect(st.PromoteVerdict.Interval).To(BeEquivalentTo(10))
		Expect(st.PromoteVerdict.At.Time).To(BeTemporally("==", sentAt))
		Expect(dispatchOf(st)).To(Equal(&servingv1alpha1.GateDispatch{
			State:       servingv1alpha1.GateDispatchDispatched,
			AttemptedAt: new(metav1.NewTime(sentAt)),
			SHA:         canaryImageTag,
			StatusCode:  204,
		}))
		Expect(levels(st)).To(Equal(repeat(levelActPromote, 11)))

		By("before the dispatch, the Deploy runs in each pending status listed with the token; then one dispatch")
		lists := gh.runLists()
		Expect(lists).To(HaveLen(5))
		statuses := make([]string, 0, len(lists))
		for _, req := range lists {
			Expect(req.path).To(Equal(runsPath))
			expectGitHubHeaders(req, tokenA)
			statuses = append(statuses, req.status)
		}
		Expect(statuses).To(Equal([]string{"queued", "waiting", "pending", "requested", "in_progress"}))
		sent := gh.dispatches()
		Expect(sent).To(HaveLen(1))
		Expect(sent[0].path).To(Equal(dispatchesPath))
		expectGitHubHeaders(sent[0], tokenA)
		Expect(sent[0].contentType).To(Equal("application/json"))
		Expect(sent[0].body).To(MatchJSON(dispatchBody(canaryImageTag)))

		By("two status writes, the latch and then GitHub's answer, its state and status code alone, and no write " +
			"to anything else")
		h.writes.mu.Lock()
		last := h.writes.writes[len(h.writes.writes)-2:]
		h.writes.mu.Unlock()
		Expect(last[0].subresource).To(Equal(statusSubresource))
		Expect(last[0].body).To(ContainSubstring(`"state":"Attempted"`))
		Expect(last[1].subresource).To(Equal(statusSubresource))
		// The answer's write is the one status write left unlocked, its base
		// the API server's reply to the latch (D43, the clarification of
		// 2026-10-15): it carries GitHub's answer and nothing else.
		Expect(last[1].body).To(MatchJSON(
			`{"status":{"gate":{"promoteVerdict":{"dispatch":{"state":"Dispatched","statusCode":204}}}}}`))
		expectNoWorkloadWrites(h.writes, writesOpen)

		By("one Event says so; the window stays open for the pipeline's close")
		Expect(h.gateEvents()).To(ConsistOf(dispatchedEvent(10, sentAt)))
		expectOpen(h)
	})

	It("dispatches at most once per latched verdict: reconciles between boundaries, later boundaries and an "+
		"operator restart send nothing more, and leave the verdict as written", func() {
		h, gh, _ := newPromoteHarness()
		h.clean(11)
		Expect(gh.dispatches()).To(HaveLen(1))
		h.drainOpenEvents()
		written := dispatchOf(h.gate())
		requests := len(gh.all())

		for _, offset := range []time.Duration{time.Second, 30 * time.Second} {
			h.clock.t = h.boundaryTime(11).Add(offset)
			reconcileResult(h.r, h.sd)
		}
		h.clean(3)
		restarted, _, clock := newClockedReconciler()
		restarted.scraper, restarted.dispatcher = h.reads, h.r.dispatcher
		h.r, h.clock = restarted, clock
		h.clean(3)

		st := h.gate()
		Expect(st.PromoteVerdict.Interval).To(BeEquivalentTo(10))
		Expect(dispatchOf(st)).To(Equal(written))
		Expect(gh.all()).To(HaveLen(requests), "not a runs list, not a dispatch")
		Expect(h.gateEvents()).NotTo(ContainElement(ContainSubstring("GatePromote")))
	})

	It("never sends a verdict the operator died holding the latch of: killed once the latch landed and before "+
		"the dispatch, it leaves Attempted and no run, and the restarted operator sends nothing (D43)", func() {
		h, gh, _ := newPromoteHarness()
		h.clean(10)
		h.drainOpenEvents()
		h.writes.mu.Lock()
		h.writes.killAfter = func(w clientWrite) bool {
			return w.subresource == statusSubresource && strings.Contains(w.body, `"state":"Attempted"`)
		}
		h.writes.mu.Unlock()

		Expect(func() { h.next(perInterval, 0, 0) }).To(PanicWith(errOperatorKilled))
		st := h.gate()
		Expect(dispatchOf(st)).To(Equal(&servingv1alpha1.GateDispatch{
			State: servingv1alpha1.GateDispatchAttempted, AttemptedAt: new(metav1.NewTime(h.boundaryTime(11))),
			SHA: canaryImageTag,
		}))
		Expect(gh.dispatches()).To(BeEmpty(), "the latch landed and the dispatch was never sent")
		Expect(h.gateEvents()).To(BeEmpty(), "a killed operator records nothing")

		By("a restarted operator walks on: the latched verdict is never sent")
		restarted, _, clock := newClockedReconciler()
		restarted.scraper, restarted.dispatcher = h.reads, h.r.dispatcher
		h.r, h.clock = restarted, clock
		h.clean(4)
		Expect(gh.dispatches()).To(BeEmpty())
		Expect(dispatchOf(h.gate()).State).To(Equal(servingv1alpha1.GateDispatchAttempted))
		Expect(h.gateEvents()).NotTo(ContainElement(ContainSubstring("GatePromote")))
		expectOpen(h)
	})

	DescribeTable("dispatches only with 15 minutes of the TTL left at the verdict (D47): a canary Ready late moves "+
		"the verdict with it (D44), and one inside the floor stays recorded for a human, with nothing asked of "+
		"GitHub",
		func(readyBoundary, wantInterval int32, remaining time.Duration, dispatched bool) {
			h := newGateHarness(actPromote)
			gh, _ := armDispatch(h)
			canary := h.addCanary(nil)
			h.open()
			h.clean(int(readyBoundary - 1))
			updatePodStatus(canary, func(s *corev1.PodStatus) { setReady(s, at(h.boundaryTime(readyBoundary))) })
			for h.gate().PromoteVerdict == nil {
				h.next(perInterval, 0, 0)
			}
			st := h.gate()
			Expect(st.PromoteVerdict.Interval).To(Equal(wantInterval))
			Expect(windowStart.Add(canaryWindowTTL).Sub(h.boundaryTime(wantInterval + 1))).To(Equal(remaining))
			if dispatched {
				Expect(dispatchOf(st).State).To(Equal(servingv1alpha1.GateDispatchDispatched))
				Expect(gh.dispatches()).To(HaveLen(1))
				return
			}
			Expect(dispatchOf(st)).To(Equal(&servingv1alpha1.GateDispatch{
				State: servingv1alpha1.GateDispatchNotDispatched, Reason: servingv1alpha1.GateDispatchBelowFloor,
			}))
			Expect(gh.all()).To(BeEmpty(), "the floor is judged before anything is asked of GitHub")
			Expect(h.gateEvents()).To(ContainElement(fmt.Sprintf("Warning GatePromoteNotDispatched the promote "+
				"verdict of interval %d is not dispatched: 14m0s of the window's TTL remain, under the 15m0s "+
				"dispatch floor (D47); it stays recorded, and a human actuates it", wantInterval)))
			h.clean(3)
			Expect(gh.all()).To(BeEmpty())
			Expect(dispatchOf(h.gate()).State).To(Equal(servingv1alpha1.GateDispatchNotDispatched))
			expectOpen(h)
		},
		Entry("Ready at 19m: the verdict at 31m, 14 minutes left, not dispatched", int32(19), int32(30),
			14*time.Minute, false),
		Entry("Ready at 18m: the verdict at 30m, 15 minutes left, the floor itself, dispatched", int32(18), int32(29),
			15*time.Minute, true),
		Entry("Ready at 17m: the verdict at 29m, 16 minutes left, dispatched", int32(17), int32(28),
			16*time.Minute, true),
	)

	DescribeTable("defers the dispatch while a Deploy run is queued, waiting, pending, requested or in progress, "+
		"so as never to evict a human's pending run: the verdict latched Deferred, one Event, nothing sent; then "+
		"dispatched at the first boundary that finds none",
		func(status string) {
			h, gh, _ := newPromoteHarness()
			h.drainOpenEvents()
			gh.set(func(gh *githubStub) { gh.pending[status] = 1 })
			h.clean(11)
			Expect(dispatchOf(h.gate())).To(Equal(&servingv1alpha1.GateDispatch{
				State: servingv1alpha1.GateDispatchDeferred, Reason: servingv1alpha1.GateDispatchRunPending,
			}))
			Expect(gh.dispatches()).To(BeEmpty())
			Expect(gh.runLists()[len(gh.runLists())-1].status).To(Equal(status), "the list stops at the first run")
			Expect(h.gateEvents()).To(ConsistOf("Warning GatePromoteDeferred the promote verdict of interval 10 is " +
				"deferred: a Deploy run is " + status + ", and a dispatch beside it would evict it or queue behind " +
				"it; the gate checks again at each boundary, and dispatches once no run is pending while the clean " +
				"run holds and 15m0s of the TTL remain"))

			By("still pending at the next boundary: listed again, nothing written, no Event")
			before := h.gate()
			lists := len(gh.runLists())
			h.clean(1)
			Expect(gh.runLists()).To(HaveLen(lists + len(pendingRunStatuses[:indexOfStatus(status)+1])))
			Expect(gh.dispatches()).To(BeEmpty())
			Expect(h.gate().PromoteVerdict).To(Equal(before.PromoteVerdict))
			Expect(h.gateEvents()).To(BeEmpty())

			By("the run gone, the next boundary dispatches, once")
			gh.set(func(gh *githubStub) { gh.pending[status] = 0 })
			h.clean(1)
			Expect(gh.dispatches()).To(HaveLen(1))
			Expect(dispatchOf(h.gate())).To(Equal(&servingv1alpha1.GateDispatch{
				State: servingv1alpha1.GateDispatchDispatched, AttemptedAt: new(metav1.NewTime(h.boundaryTime(13))),
				SHA: canaryImageTag, StatusCode: 204,
			}))
			Expect(h.gateEvents()).To(ConsistOf(dispatchedEvent(10, h.boundaryTime(13))))
			h.clean(2)
			Expect(gh.dispatches()).To(HaveLen(1))
		},
		Entry("queued", "queued"),
		Entry("waiting, on the ec2-deploy reviewer", "waiting"),
		Entry("pending in deploy.yml's concurrency group, the run a dispatch would evict", "pending"),
		Entry("requested", "requested"),
		Entry("in progress, the deploy under way", "in_progress"),
	)

	It("keeps a deferred verdict waiting, asking nothing of GitHub, while a breach has broken its clean run, and "+
		"turns it down for a human at the floor", func() {
		h, gh, _ := newPromoteHarness()
		gh.set(func(gh *githubStub) { gh.pending["waiting"] = 1 })
		h.clean(11)
		Expect(dispatchOf(h.gate()).State).To(Equal(servingv1alpha1.GateDispatchDeferred))
		h.drainOpenEvents()

		By("one breach breaks the clean run: no list while it is broken")
		h.next(0, perInterval, 0)
		lists := len(gh.runLists())
		h.clean(10)
		Expect(gh.runLists()).To(HaveLen(lists), "the run not yet whole again: nothing listed")
		Expect(dispatchOf(h.gate()).State).To(Equal(servingv1alpha1.GateDispatchDeferred))

		By("whole again, and still a run waiting: deferred, with no new Event")
		h.clean(1)
		Expect(gh.runLists()).To(HaveLen(lists + 2))
		Expect(h.gateEvents()).NotTo(ContainElement(ContainSubstring("GatePromote")))

		By("at 31m, under the floor: turned down for a human, and never dispatched")
		for h.n < 31 {
			h.next(perInterval, 0, 0)
		}
		Expect(dispatchOf(h.gate())).To(Equal(&servingv1alpha1.GateDispatch{
			State: servingv1alpha1.GateDispatchNotDispatched, Reason: servingv1alpha1.GateDispatchBelowFloor,
		}))
		Expect(h.gateEvents()).To(ConsistOf(HavePrefix("Warning GatePromoteNotDispatched the promote verdict of " +
			"interval 10 is not dispatched: 14m0s of the window's TTL remain")))
		h.clean(2)
		Expect(gh.dispatches()).To(BeEmpty())
	})

	DescribeTable("leaves a dispatch GitHub refuses or never answers latched and Failed, with one Event, and "+
		"never retries it: the human actuates",
		func(answer func(gh *githubStub), code int32, why string) {
			h, gh, _ := newPromoteHarness()
			h.drainOpenEvents()
			gh.set(answer)
			h.clean(11)
			Expect(gh.dispatches()).To(HaveLen(1))
			Expect(dispatchOf(h.gate())).To(Equal(&servingv1alpha1.GateDispatch{
				State: servingv1alpha1.GateDispatchFailed, AttemptedAt: new(metav1.NewTime(h.boundaryTime(11))),
				SHA: canaryImageTag, StatusCode: code,
			}))
			Expect(h.gateEvents()).To(ConsistOf(And(
				HavePrefix("Warning GatePromoteDispatchFailed the dispatch of deploy.yml with sha "+canaryImageTag+
					" for the promote verdict of interval 10 failed: "),
				ContainSubstring(why),
				HaveSuffix("; it is not retried, the verdict stays latched, and a human actuates it"),
				Not(ContainSubstring(tokenA)))))

			By("later boundaries send nothing: no retry")
			gh.set(func(gh *githubStub) { gh.dispatchCode, gh.hangUp = 0, false })
			h.clean(4)
			Expect(gh.dispatches()).To(HaveLen(1))
			Expect(dispatchOf(h.gate()).State).To(Equal(servingv1alpha1.GateDispatchFailed))
			Expect(h.gateEvents()).To(BeEmpty())
			expectOpen(h)
		},
		Entry("401, a token expired or revoked", func(gh *githubStub) { gh.dispatchCode = 401 }, int32(401),
			"GitHub answered 401 Unauthorized"),
		Entry("403, a token without Actions write", func(gh *githubStub) { gh.dispatchCode = 403 }, int32(403),
			"GitHub answered 403 Forbidden"),
		Entry("422, a ref or input deploy.yml refuses", func(gh *githubStub) { gh.dispatchCode = 422 }, int32(422),
			"GitHub answered 422 Unprocessable Entity"),
		Entry("500", func(gh *githubStub) { gh.dispatchCode = 500 }, int32(500),
			"GitHub answered 500 Internal Server Error"),
		Entry("307, a redirect, which is not followed", func(gh *githubStub) { gh.dispatchCode = 307 }, int32(307),
			"GitHub answered 307 Temporary Redirect"),
		Entry("no answer: the connection dropped", func(gh *githubStub) { gh.hangUp = true }, int32(0), "EOF"),
	)

	DescribeTable("turns a verdict down for a human, sending nothing, when the Deploy runs cannot be listed: a "+
		"queued human deploy cannot be ruled out",
		func(answer func(gh *githubStub), why string) {
			h, gh, _ := newPromoteHarness()
			h.drainOpenEvents()
			gh.set(answer)
			h.clean(11)
			Expect(dispatchOf(h.gate())).To(Equal(&servingv1alpha1.GateDispatch{
				State: servingv1alpha1.GateDispatchNotDispatched, Reason: servingv1alpha1.GateDispatchRunsUnreadable,
			}))
			Expect(gh.dispatches()).To(BeEmpty())
			Expect(h.gateEvents()).To(ConsistOf(And(
				HavePrefix("Warning GatePromoteNotDispatched the promote verdict of interval 10 is not dispatched: "+
					"the Deploy runs could not be listed, so a queued human deploy cannot be ruled out: "),
				ContainSubstring(why))))
			lists := len(gh.runLists())
			gh.set(func(gh *githubStub) { gh.runsCode = 0 })
			h.clean(2)
			Expect(gh.all()).To(HaveLen(lists), "turned down: nothing more is asked")
		},
		Entry("401", func(gh *githubStub) { gh.runsCode = 401 }, "GitHub answered 401 Unauthorized"),
		Entry("500", func(gh *githubStub) { gh.runsCode = 500 }, "GitHub answered 500 Internal Server Error"),
	)

	It("turns a verdict down for a human, asking nothing of GitHub, when a pod it read is not shown to be its "+
		"Deployment's: the owner chain is checked before a promote acts on pod-reported signals (#108, N1)", func() {
		h := newGateHarness(actPromote)
		gh, _ := armDispatch(h)
		canary := h.addCanary(at(windowStart.Add(-time.Minute)))
		latest := &corev1.Pod{}
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(canary), latest)).To(Succeed())
		latest.OwnerReferences = nil
		Expect(k8sClient.Update(ctx, latest)).To(Succeed())
		h.open()
		h.drainOpenEvents()
		h.clean(11)
		Expect(h.gate().PromoteVerdict.Interval).To(BeEquivalentTo(10), "the verdict itself stands")
		Expect(dispatchOf(h.gate())).To(Equal(&servingv1alpha1.GateDispatch{
			State: servingv1alpha1.GateDispatchNotDispatched, Reason: servingv1alpha1.GateDispatchOwnerChainUnverified,
		}))
		Expect(gh.all()).To(BeEmpty())
		Expect(h.gateEvents()).To(ConsistOf("Warning GatePromoteNotDispatched the promote verdict of interval 10 is " +
			"not dispatched: the canary pod read: pod/api-canary-7b8d9c6f4-m4tnw has no controller; it stays " +
			"recorded, and a human actuates it"))
		h.clean(2)
		Expect(gh.all()).To(BeEmpty())
		expectOpen(h)
	})

	DescribeTable("turns a verdict down for a human, asking nothing of GitHub, when the canary pod it read is not "+
		"shown to run the tag it would dispatch: the dispatched sha is tied to the evidence (#121, F1)",
		func(race func(h *gateHarness, canary *corev1.Pod), why string) {
			h := newGateHarness(actPromote)
			gh, _ := armDispatch(h)
			canary := h.addCanary(at(windowStart.Add(-time.Minute)))
			h.open()
			h.clean(10)
			h.drainOpenEvents()

			race(h, canary)
			h.clean(1)
			st := h.gate()
			Expect(st.PromoteVerdict.Interval).To(BeEquivalentTo(10), "the verdict itself stands")
			Expect(st.Mode).To(Equal(levelActPromote))
			Expect(st.Arming).To(Equal(servingv1alpha1.GateArmingArmed), "the arming does not catch it")
			Expect(dispatchOf(st)).To(Equal(&servingv1alpha1.GateDispatch{
				State: servingv1alpha1.GateDispatchNotDispatched, Reason: servingv1alpha1.GateDispatchCanaryPodStale,
			}))
			Expect(gh.all()).To(BeEmpty(), "turned down before the token is read or a run listed")
			Expect(h.gateEvents()).To(ConsistOf(staleCanaryEvent(why)))
			Expect(apiContainer(getCanaryDeployment(h.ns)).Image).To(Equal(
				stableImage(getServingDeployment(h.sd).Spec.CanaryImageTag)),
				"deployment/api-canary runs the spec's tag: the pod read is what lags")

			h.clean(2)
			Expect(gh.all()).To(BeEmpty())
			Expect(dispatchOf(h.gate()).Reason).To(Equal(servingv1alpha1.GateDispatchCanaryPodStale))
			expectOpen(h)
		},
		Entry("the tag pair changed in the boundary's own reconcile: deployment/api-canary is moved to the new tag "+
			"before the read, and the old pod, Running while a Recreate rollout stops it, is the one read",
			func(h *gateHarness, _ *corev1.Pod) { setTagPair(h.sd, laterCanaryImageTag) },
			"runs "+stableImage(canaryImageTag)+", not "+stableImage(laterCanaryImageTag)+", the image "+
				"spec.canaryImageTag renders: what it reported is not the dispatched tag's"),
		Entry("the canary pod read is being deleted, its successor not yet Running",
			func(_ *gateHarness, canary *corev1.Pod) { markDeleting(canary) },
			"is being deleted: what it reported may be an image the canary no longer runs"),
	)

	It("sends nothing when the ServingDeployment is written between the boundary's read and the latch's write: "+
		"that write is locked to what was read, so a tag pair changed there conflicts it, and the retry inside "+
		"the boundary turns the verdict down for a human (#121, F4)", func() {
		h, gh, _ := newPromoteHarness()
		h.clean(10)
		h.drainOpenEvents()
		var raced bool
		h.writes.mu.Lock()
		h.writes.beforePatch = func(w clientWrite) {
			if !raced && w.subresource == statusSubresource && strings.Contains(w.body, `"state":"Attempted"`) {
				raced = true
				setTagPair(h.sd, laterCanaryImageTag)
			}
		}
		h.writes.mu.Unlock()

		By("the boundary's reconcile: the latch's write conflicts, and nothing lands or is sent")
		h.serve(perInterval, 0, 0)
		h.n++
		h.clock.t = h.boundaryTime(h.n)
		_, err := h.r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(h.sd)})
		Expect(raced).To(BeTrue(), "the tag pair was patched as the latch was sent")
		Expect(apierrors.IsConflict(err)).To(BeTrue(), "%v", err)
		Expect(h.gate().PromoteVerdict).To(BeNil(), "the latch, and the verdict with it, never landed")
		Expect(gh.dispatches()).To(BeEmpty())
		Expect(gh.runLists()).To(HaveLen(len(pendingRunStatuses)), "listed once, before the latch")
		Expect(h.gateEvents()).To(BeEmpty(), "nothing is recorded for a write that did not land")

		By("the retry, a second into the boundary: decided again from the new spec, and turned down")
		h.clock.t = h.boundaryTime(h.n).Add(time.Second)
		reconcileResult(h.r, h.sd)
		Expect(dispatchOf(h.gate())).To(Equal(&servingv1alpha1.GateDispatch{
			State: servingv1alpha1.GateDispatchNotDispatched, Reason: servingv1alpha1.GateDispatchCanaryPodStale,
		}))
		Expect(h.gateEvents()).To(ConsistOf(staleCanaryEvent("runs " + stableImage(canaryImageTag) + ", not " +
			stableImage(laterCanaryImageTag) + ", the image spec.canaryImageTag renders: what it reported is not " +
			"the dispatched tag's")))
		h.clean(2)
		Expect(gh.dispatches()).To(BeEmpty())
		Expect(gh.runLists()).To(HaveLen(len(pendingRunStatuses)))
		expectOpen(h)
	})

	It("never dispatches from a window opened at Observe, whatever a stale reconcile writes: spec.gate raised to "+
		"ActPromote mid-window, one served the ServingDeployment from before the window was requested would put "+
		"CanaryActive back to NoCanary, and the next boundary would open the window again armed to dispatch; its "+
		"write conflicts instead, and the verdict is recorded with nothing asked of GitHub (V1-S7)", func() {
		h := newGateHarness(observe)
		gh, _ := armDispatch(h)
		h.addCanary(at(windowStart.Add(-time.Minute)))
		beforeRequest := getServingDeployment(h.sd)
		h.open()
		h.clean(2)
		h.drainOpenEvents()

		By("spec.gate raised to ActPromote mid-window, then a reconcile served the object from before the request")
		setGate(h.sd, actPromote)
		h.clock.t = h.boundaryTime(2).Add(10 * time.Second)
		stale := h.reconcileReading(beforeRequest)

		By("twelve clean boundaries: the verdict at the ninth, and as many as a window opened again at the first " +
			"would need for its own")
		h.clean(12)

		By("nothing asked of GitHub, and the window on its own anchor, its verdict recorded at Observe")
		Expect(gh.dispatches()).To(BeEmpty(), "a dispatch from a window opened at Observe")
		Expect(gh.all()).To(BeEmpty())
		latest := getServingDeployment(h.sd)
		Expect(condition(latest, servingv1alpha1.ConditionCanaryActive).LastTransitionTime.Time).
			To(BeTemporally("==", windowStart))
		st := latest.Status.Gate
		Expect(st.WindowStart.Time).To(BeTemporally("==", windowStart))
		Expect(st.Mode).To(Equal(levelObserve))
		Expect(levels(st)).To(Equal(repeat(levelObserve, 14)))
		Expect(st.PromoteVerdict.Interval).To(BeEquivalentTo(10))
		Expect(st.PromoteVerdict.At.Time).To(BeTemporally("==", h.boundaryTime(11)))
		Expect(st.PromoteVerdict.Dispatch).To(BeNil(), "below ActPromote, no dispatch")
		Expect(h.gateEvents()).To(ConsistOf("Normal GateWouldPromote interval 10 completed 11 consecutive clean " +
			"intervals, the promote verdict: in Observe it is recorded and nothing is dispatched"))
		Expect(apierrors.IsConflict(stale)).To(BeTrue(), "the stale reconcile's write: %v", stale)
		expectOpen(h)
	})

	It("rolls back on a tripped line in ActPromote, the lattice (D42): the records at ActPromote, nothing asked "+
		"of GitHub without a verdict; and a trip after a dispatch names the run to reject (D47)", func() {
		h, gh, _ := newPromoteHarness()
		h.drainOpenEvents()
		h.clean(2)
		h.next(0, perInterval, 0)
		h.next(0, perInterval, 0)
		st := expectRolledBack(h, "Line3CanaryLatency", 3, h.boundaryTime(4))
		Expect(levels(st)).To(Equal(repeat(levelActPromote, 4)))
		Expect(gh.all()).To(BeEmpty())

		By("the next window, dispatched at boundary 11, then a breach two in a row")
		next := windowStart.Add(10 * time.Minute)
		h.reopen(next)
		h.drainOpenEvents()
		boundary := func(n int) time.Time { return next.Add(time.Duration(n) * time.Minute) }
		for n := 1; n <= 13; n++ {
			fast, slow := float64(perInterval), 0.0
			if n >= 12 {
				fast, slow = 0, perInterval
			}
			h.serve(fast, slow, 0)
			h.clock.t = boundary(n)
			reconcileResult(h.r, h.sd)
		}
		Expect(gh.dispatches()).To(HaveLen(1))
		active := condition(getServingDeployment(h.sd), servingv1alpha1.ConditionCanaryActive)
		Expect(active.Reason).To(Equal(servingv1alpha1.ReasonBreachRolledBack))
		Expect(h.gateEvents()).To(ContainElement(HaveSuffix("clears the canary fields; the promote dispatch sent at " +
			stamp(boundary(11)) + " may have a pending Deploy run: check Actions and reject it")))
	})

	It("reads the token at every use and keeps none: a Secret rotated on disk is the next dispatch's token, "+
		"with no restart", func() {
		h, gh, tokenFile := newPromoteHarness()
		h.clean(11)
		Expect(gh.dispatches()).To(HaveLen(1))
		expectGitHubHeaders(gh.dispatches()[0], tokenA)

		By("the Secret rotated: the file now holds token B; the same reconciler runs the next window")
		writeToken(tokenFile, tokenB)
		h.reopen(windowStart.Add(20 * time.Minute))
		start := windowStart.Add(20 * time.Minute)
		for n := 1; n <= 11; n++ {
			h.serve(perInterval, 0, 0)
			h.clock.t = start.Add(time.Duration(n) * time.Minute)
			reconcileResult(h.r, h.sd)
		}
		sent := gh.dispatches()
		Expect(sent).To(HaveLen(2))
		expectGitHubHeaders(sent[1], tokenB)
		for _, req := range gh.runLists()[5:] {
			expectGitHubHeaders(req, tokenB)
		}
	})

	DescribeTable("arms ActPromote as TokenAbsent while no token can be read — the Secret absent, the off-season "+
		"posture — and records the verdict for a human, asking nothing of GitHub (D43)",
		func(token *string) {
			h := newGateHarness(actPromote)
			gh, tokenFile := armDispatch(h)
			Expect(os.Remove(tokenFile)).To(Succeed())
			if token != nil {
				Expect(os.WriteFile(tokenFile, []byte(*token), 0o600)).To(Succeed())
			}
			h.addCanary(at(windowStart.Add(-time.Minute)))
			h.open()
			Expect(h.gate().Arming).To(Equal(servingv1alpha1.GateArmingTokenAbsent))
			Expect(h.gate().Mode).To(Equal(levelActRollback))
			h.drainOpenEvents()
			h.clean(12)
			st := h.gate()
			Expect(st.PromoteVerdict).NotTo(BeNil())
			Expect(st.PromoteVerdict.Interval).To(BeEquivalentTo(10))
			Expect(st.PromoteVerdict.At.Time).To(BeTemporally("==", h.boundaryTime(11)))
			Expect(st.PromoteVerdict.Dispatch).To(BeNil(), "below ActPromote, no dispatch")
			Expect(h.gateEvents()).To(ConsistOf("Normal GateWouldPromote interval 10 completed 11 consecutive clean " +
				"intervals, the promote verdict: in ActRollback it is recorded and nothing is dispatched"))
			Expect(gh.all()).To(BeEmpty())
		},
		Entry("no file: the Secret absent", nil),
		Entry("an empty file", new("")),
		Entry("whitespace only", new(" \n")),
		Entry("a value no token has: two lines", new("ghp_one\nghp_two")),
		Entry("a value no token has: a space", new("ghp one")),
	)

	It("lowers ActPromote at once when its token goes mid-window, and raises it back only from the next window "+
		"(#119)", func() {
		h, gh, tokenFile := newPromoteHarness()
		h.clean(3)
		h.drainOpenEvents()

		By("the Secret deleted: at the next boundary, TokenAbsent and ActRollback")
		Expect(os.Remove(tokenFile)).To(Succeed())
		h.clean(1)
		Expect(h.gate().Arming).To(Equal(servingv1alpha1.GateArmingTokenAbsent))
		Expect(h.gate().Mode).To(Equal(levelActRollback))
		Expect(h.gateEvents()).To(ConsistOf(HavePrefix("Warning GateDegraded spec.gate.mode ActPromote has no " +
			"dispatch token")))

		By("restored a boundary later: armed again, and still ActRollback until the window closes")
		writeToken(tokenFile, tokenA)
		h.clean(8)
		st := h.gate()
		Expect(st.Arming).To(Equal(servingv1alpha1.GateArmingArmed))
		Expect(st.Mode).To(Equal(levelActRollback))
		Expect(levels(st)).To(Equal(append(repeat(levelActPromote, 3), repeat(levelActRollback, 9)...)))
		Expect(st.PromoteVerdict).To(HaveField("Dispatch", BeNil()), "the verdict, at ActRollback, for a human")
		Expect(gh.all()).To(BeEmpty())

		By("the next window opens at ActPromote")
		h.reopen(windowStart.Add(30 * time.Minute))
		Expect(h.gate().Mode).To(Equal(levelActPromote))
	})

	It("never dispatches a tag that is not a full commit SHA: the API server's pattern refuses one in the spec, "+
		"and behind it the gate turns the verdict down and asks nothing of GitHub", func() {
		h := newGateHarness(actPromote)
		gh, _ := armDispatch(h)
		sd := getServingDeployment(h.sd)
		sd.Spec.CanaryImageTag = "v4.0.0"
		Expect(k8sClient.Update(ctx, sd.DeepCopy())).NotTo(Succeed(), "the CRD's pattern")

		st := &servingv1alpha1.GateStatus{PromoteVerdict: &servingv1alpha1.GatePromoteVerdict{Interval: 10}}
		var step gateStep
		events := h.r.promote(ctx, sd, st, h.reads.pods.ReadBoundary(ctx, h.ns), true,
			windowStart.Add(canaryWindowTTL), &step)
		Expect(dispatchOf(st)).To(Equal(&servingv1alpha1.GateDispatch{
			State: servingv1alpha1.GateDispatchNotDispatched, Reason: servingv1alpha1.GateDispatchTagInvalid,
		}))
		Expect(events).To(HaveLen(1))
		Expect(events[0].message).To(ContainSubstring(
			`spec.canaryImageTag "v4.0.0" is not a 40-character lowercase hex commit SHA`))
		Expect(step.dispatch).To(BeNil())
		Expect(gh.all()).To(BeEmpty())
	})
})

// indexOfStatus is status's place in pendingRunStatuses.
func indexOfStatus(status string) int {
	i := slices.Index(pendingRunStatuses, status)
	Expect(i).To(BeNumerically(">=", 0), "not a pending status: %s", status)
	return i
}
