// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package runner

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/kubernetes/fake"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	k8stesting "k8s.io/client-go/testing"

	api "github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/collaboration"
	"github.com/kognition-ai/kognition/internal/collaboration/a2atransport"
	"github.com/kognition-ai/kognition/internal/collaboration/driver"
	"github.com/kognition-ai/kognition/internal/collaboration/driver/k8sledger"
	"github.com/kognition-ai/kognition/internal/controller"
)

var bg = context.Background()

var errBoom = errors.New("boom")

// newClient emulates API-server resourceVersion semantics that the fake lacks.
func newClient() *fake.Clientset {
	cs := fake.NewSimpleClientset()
	cs.PrependReactor("create", "configmaps", func(a k8stesting.Action) (bool, runtime.Object, error) {
		a.(k8stesting.CreateAction).GetObject().(metav1.Object).SetResourceVersion("1")
		return false, nil, nil
	})
	cs.PrependReactor("update", "configmaps", func(a k8stesting.Action) (bool, runtime.Object, error) {
		upd := a.(k8stesting.UpdateAction)
		obj := upd.GetObject().(metav1.Object)
		cur, err := cs.Tracker().Get(upd.GetResource(), upd.GetNamespace(), obj.GetName())
		if err != nil {
			return true, nil, err
		}
		rv := cur.(metav1.Object).GetResourceVersion()
		if obj.GetResourceVersion() != rv {
			return true, nil, apierrors.NewConflict(upd.GetResource().GroupResource(), obj.GetName(), errors.New("stale"))
		}
		n, _ := strconv.Atoi(rv)
		obj.SetResourceVersion(strconv.Itoa(n + 1))
		return false, nil, nil
	})
	return cs
}

// scripted is a Transport whose members answer from a fixed supervisor script.
type scripted struct {
	mu         sync.Mutex
	hold       bool // Observe reports nothing settled
	failMember bool
	sends      map[string]int
	sent       map[string]bool
}

func newScripted() *scripted {
	return &scripted{sends: map[string]int{}, sent: map[string]bool{}}
}

func (s *scripted) release() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hold = false
}

func (s *scripted) sendCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	total := 0
	for _, n := range s.sends {
		total += n
	}
	return total
}

func (s *scripted) Admit(context.Context, collaboration.Message) error { return nil }

func (s *scripted) Send(_ context.Context, m collaboration.Message) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sends[m.DispatchID]++
	s.sent[m.DispatchID] = true
	return nil
}

func (s *scripted) Observe(_ context.Context, m collaboration.Message) (driver.Observation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.sent[m.DispatchID] || s.hold {
		return driver.Observation{}, nil
	}
	done := func(ops ...collaboration.Operation) driver.Observation {
		return driver.Observation{Settled: true, Actor: m.Recipient, Status: collaboration.ReportCompleted,
			Operations: ops, TokensUsed: 10, TokensKnown: true}
	}
	switch {
	case m.Kind == collaboration.DispatchCancel:
		return driver.Observation{Settled: true, Actor: m.Recipient, Status: collaboration.ReportCancelled}, nil
	case m.Recipient == "lead" && m.Kind == collaboration.DispatchStart:
		return done(collaboration.Operation{Kind: collaboration.OpDelegate, OperationID: "d1", To: "a", Goal: "ga"}), nil
	case m.Recipient == "lead":
		var parts []string
		for _, r := range m.Results {
			parts = append(parts, r.Member+"="+r.Result.Text)
		}
		return done(collaboration.Operation{Kind: collaboration.OpFinalize, Result: strings.Join(parts, ",")}), nil
	case s.failMember:
		return driver.Observation{Settled: true, Actor: m.Recipient, Status: collaboration.ReportFailed,
			Failure: "member exploded", TokensUsed: 10, TokensKnown: true}, nil
	}
	return done(collaboration.Operation{Kind: collaboration.OpComplete, Result: "from-" + m.Recipient}), nil
}

type factory struct {
	mu      sync.Mutex
	tr      driver.Transport
	err     error
	runs    []controller.WorkflowCollaborationRun
	handles []a2atransport.Handles
}

func (f *factory) Transport(_ context.Context, run controller.WorkflowCollaborationRun, h a2atransport.Handles) (driver.Transport, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.runs = append(f.runs, run)
	f.handles = append(f.handles, h)
	return f.tr, f.err
}

func spec() *api.WorkflowCollaboration {
	c := &api.WorkflowCollaboration{
		Strategy: api.CollaborationStrategySupervisor,
		Protocol: api.CollaborationProtocolA2A,
		Limits: api.CollaborationLimits{MaxTurns: 20, MaxMessages: 100, MaxDelegations: 10, MaxDelegationDepth: 3,
			MaxParallelTasks: 4, MaxTokens: 100000, MaxContentBytes: 8192},
		Supervisor:          &api.CollaborationSupervisor{Coordinator: "lead"},
		AllowedInteractions: []api.CollaborationInteraction{{From: "lead", To: "a"}},
	}
	for _, m := range []string{"lead", "a"} {
		c.Members = append(c.Members, api.CollaborationMember{Name: m, AgentRef: api.AgentReference{Name: m}})
	}
	return c
}

func runFor() controller.WorkflowCollaborationRun {
	return controller.WorkflowCollaborationRun{Namespace: "ns", RunName: "run", RunUID: "uid-1", Step: "debate",
		Attempt: 1, Input: "investigate", Spec: spec()}
}

func newRunner(t *testing.T, cs *fake.Clientset, tr driver.Transport) (*Runner, *factory) {
	t.Helper()
	f := &factory{tr: tr}
	r, err := New(Config{
		ConfigMaps: func(ns string) corev1client.ConfigMapInterface { return cs.CoreV1().ConfigMaps(ns) },
		Transports: f,
	})
	if err != nil {
		t.Fatal(err)
	}
	return r, f
}

func advanceUntilDone(t *testing.T, r *Runner, run controller.WorkflowCollaborationRun) controller.WorkflowCollaborationProgress {
	t.Helper()
	for i := 0; i < 50; i++ {
		p, err := r.Advance(bg, run)
		if err != nil {
			t.Fatalf("Advance: %v", err)
		}
		if p.Done {
			return p
		}
	}
	t.Fatal("collaboration did not finish")
	return controller.WorkflowCollaborationProgress{}
}

func TestNewRequiresDependencies(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Fatal("empty config accepted")
	}
	if _, err := New(Config{Transports: &factory{}}); err == nil {
		t.Fatal("missing ConfigMaps accepted")
	}
	if _, err := New(Config{ConfigMaps: func(string) corev1client.ConfigMapInterface { return nil }}); err == nil {
		t.Fatal("missing transports accepted")
	}
}

func TestAdvanceRunsSupervisorToCompletion(t *testing.T) {
	cs := newClient()
	tr := newScripted()
	tr.hold = true
	r, f := newRunner(t, cs, tr)
	run := runFor()

	p, err := r.Advance(bg, run)
	if err != nil || p.Done {
		t.Fatalf("first pass = %+v, %v; want running", p, err)
	}
	cm, err := cs.CoreV1().ConfigMaps("ns").Get(bg, LedgerName(run), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("ledger not created: %v", err)
	}
	if cm.Labels[LabelComponent] != ComponentValue || cm.Labels[LabelRun] != hashLabel("uid-1") || cm.Labels[LabelStep] != hashLabel("debate") {
		t.Fatalf("labels = %v", cm.Labels)
	}
	for _, v := range cm.Labels {
		if len(validation.IsValidLabelValue(v)) > 0 {
			t.Fatalf("label value %q is invalid", v)
		}
	}

	tr.release()
	p = advanceUntilDone(t, r, run)
	if !p.Succeeded || p.Output != "a=from-a" {
		t.Fatalf("progress = %+v", p)
	}
	if len(f.runs) == 0 || f.runs[0].Step != "debate" || f.runs[0].Attempt != 1 || f.handles[0] == nil {
		t.Fatalf("factory received %+v", f.runs)
	}

	before := tr.sendCount()
	again, err := r.Advance(bg, run)
	if err != nil || !again.Done || !again.Succeeded || again.Output != "a=from-a" {
		t.Fatalf("terminal replay = %+v, %v", again, err)
	}
	if tr.sendCount() != before {
		t.Fatal("a terminal ledger caused another member contact")
	}
	for id, n := range tr.sends {
		if n != 1 {
			t.Fatalf("dispatch %s sent %d times", id, n)
		}
	}
}

func TestAdvanceResumesFromDurableLedgerAfterRestart(t *testing.T) {
	cs := newClient()
	tr := newScripted()
	tr.hold = true
	first, _ := newRunner(t, cs, tr)
	run := runFor()
	if p, err := first.Advance(bg, run); err != nil || p.Done {
		t.Fatalf("first pass = %+v, %v", p, err)
	}
	sent := tr.sendCount()
	if sent == 0 {
		t.Fatal("no dispatch was sent")
	}

	tr.release()
	restarted, _ := newRunner(t, cs, tr)
	p := advanceUntilDone(t, restarted, run)
	if !p.Succeeded || p.Output != "a=from-a" {
		t.Fatalf("progress = %+v", p)
	}
	if n := tr.sends["t1.1"]; n != 1 {
		t.Fatalf("start dispatch sent %d times across restart", n)
	}
}

func TestAdvanceReportsMemberFailure(t *testing.T) {
	cs := newClient()
	tr := newScripted()
	tr.failMember = true
	r, _ := newRunner(t, cs, tr)
	p := advanceUntilDone(t, r, runFor())
	if p.Succeeded || p.Output != "" || p.Reason == "" {
		t.Fatalf("progress = %+v", p)
	}
}

func TestAdvanceKeepsAttemptsIsolated(t *testing.T) {
	cs := newClient()
	tr := newScripted()
	r, _ := newRunner(t, cs, tr)
	first := runFor()
	second := first
	second.Attempt = 2
	if p := advanceUntilDone(t, r, first); !p.Succeeded {
		t.Fatalf("first = %+v", p)
	}
	before := tr.sendCount()
	if p := advanceUntilDone(t, r, second); !p.Succeeded {
		t.Fatalf("second = %+v", p)
	}
	if tr.sendCount() <= before {
		t.Fatal("a retry attempt reused the previous attempt's terminal ledger")
	}
	list, err := cs.CoreV1().ConfigMaps("ns").List(bg, metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	ledgers := 0
	for _, cm := range list.Items {
		if !strings.HasSuffix(cm.Name, k8sledger.HandlesSuffix) {
			ledgers++
		}
	}
	if ledgers != 2 {
		t.Fatalf("ledgers = %d, want one per attempt", ledgers)
	}
}

func TestLedgerNameIsStableDistinctAndValid(t *testing.T) {
	base := runFor()
	name := LedgerName(base)
	if name != LedgerName(base) {
		t.Fatal("name is not deterministic")
	}
	variants := []controller.WorkflowCollaborationRun{base, base, base, base, base}
	variants[0].Attempt = 2
	variants[1].Step = "other"
	variants[2].RunUID = "uid-2"
	variants[3].RunName = "other"
	variants[4].Namespace = "other"
	seen := map[string]bool{name: true}
	for i, v := range variants {
		n := LedgerName(v)
		if seen[n] {
			t.Fatalf("variant %d collides: %s", i, n)
		}
		seen[n] = true
	}
	if errs := validation.IsDNS1123Subdomain(name + k8sledger.HandlesSuffix); len(errs) > 0 {
		t.Fatalf("name %q is not a valid ConfigMap name: %v", name, errs)
	}
	if !strings.HasPrefix(name, "wfc-") {
		t.Fatalf("name = %q", name)
	}
	input := runFor()
	input.Input = "different"
	if LedgerName(input) != name {
		t.Fatal("the activation input must not affect ledger identity")
	}
}

func TestAdvanceAppliesOwnerReferences(t *testing.T) {
	cs := newClient()
	tr := newScripted()
	tr.hold = true
	f := &factory{tr: tr}
	owner := metav1.OwnerReference{APIVersion: "kognition.dev/v1alpha1", Kind: "WorkflowRun", Name: "run", UID: "uid-1"}
	var got controller.WorkflowCollaborationRun
	r, err := New(Config{
		ConfigMaps: func(ns string) corev1client.ConfigMapInterface { return cs.CoreV1().ConfigMaps(ns) },
		Transports: f,
		Owners: func(run controller.WorkflowCollaborationRun) []metav1.OwnerReference {
			got = run
			return []metav1.OwnerReference{owner}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	run := runFor()
	if _, err := r.Advance(bg, run); err != nil {
		t.Fatal(err)
	}
	cm, err := cs.CoreV1().ConfigMaps("ns").Get(bg, LedgerName(run), metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(cm.OwnerReferences) != 1 || cm.OwnerReferences[0].UID != "uid-1" || cm.OwnerReferences[0].Kind != "WorkflowRun" {
		t.Fatalf("owners = %+v", cm.OwnerReferences)
	}
	if got.Step != "debate" {
		t.Fatalf("owners callback saw %+v", got)
	}
}

func TestFactoryHandlesAreDurable(t *testing.T) {
	cs := newClient()
	r, f := newRunner(t, cs, newScripted())
	run := runFor()
	if _, err := r.Advance(bg, run); err != nil {
		t.Fatal(err)
	}
	h := f.handles[0]
	if err := h.Put(bg, "t1.1", "remote-1"); err != nil {
		t.Fatal(err)
	}
	r2, f2 := newRunner(t, cs, newScripted())
	if _, err := r2.Advance(bg, run); err != nil {
		t.Fatal(err)
	}
	got, ok, err := f2.handles[0].Get(bg, "t1.1")
	if err != nil || !ok || got != "remote-1" {
		t.Fatalf("handle after restart = %q, %v, %v", got, ok, err)
	}
}

func TestAssembleFailures(t *testing.T) {
	cs := newClient()
	r, f := newRunner(t, cs, newScripted())

	noSpec := runFor()
	noSpec.Spec = nil
	if _, err := r.Advance(bg, noSpec); err == nil {
		t.Fatal("missing spec accepted")
	}
	if err := r.Stop(bg, noSpec, "x"); err == nil {
		t.Fatal("Stop accepted a missing spec")
	}

	invalid := runFor()
	invalid.Spec = &api.WorkflowCollaboration{}
	if _, err := r.Advance(bg, invalid); err == nil {
		t.Fatal("invalid definition accepted")
	}

	f.err = errBoom
	if _, err := r.Advance(bg, runFor()); !errors.Is(err, errBoom) {
		t.Fatalf("factory error = %v", err)
	}
	f.err = nil

	f.tr = nil
	if _, err := r.Advance(bg, runFor()); err == nil {
		t.Fatal("a nil transport was accepted")
	}

	noClient, err := New(Config{ConfigMaps: func(string) corev1client.ConfigMapInterface { return nil }, Transports: &factory{tr: newScripted()}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := noClient.Advance(bg, runFor()); err == nil {
		t.Fatal("a missing ConfigMap client was accepted")
	}
	if list, _ := cs.CoreV1().ConfigMaps("ns").List(bg, metav1.ListOptions{}); len(list.Items) != 0 {
		t.Fatalf("failed assembly left %d objects", len(list.Items))
	}
}

func TestAdvanceSurfacesLedgerFailures(t *testing.T) {
	create := newClient()
	create.PrependReactor("create", "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errBoom
	})
	r, _ := newRunner(t, create, newScripted())
	if _, err := r.Advance(bg, runFor()); !errors.Is(err, errBoom) {
		t.Fatalf("create failure = %v", err)
	}

	load := newClient()
	load.PrependReactor("get", "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errBoom
	})
	r, _ = newRunner(t, load, newScripted())
	if _, err := r.Advance(bg, runFor()); !errors.Is(err, errBoom) {
		t.Fatalf("load failure = %v", err)
	}
}

func TestStopCancelsRunningCollaboration(t *testing.T) {
	cs := newClient()
	tr := newScripted()
	tr.hold = true
	r, _ := newRunner(t, cs, tr)
	run := runFor()
	if p, err := r.Advance(bg, run); err != nil || p.Done {
		t.Fatalf("first pass = %+v, %v", p, err)
	}

	tr.release()
	if err := r.Stop(bg, run, "WorkflowFailed"); err != nil {
		t.Fatal(err)
	}
	p, err := r.Advance(bg, run)
	if err != nil || !p.Done || p.Succeeded {
		t.Fatalf("after stop = %+v, %v", p, err)
	}
	if p.Reason == "" {
		t.Fatal("a stopped collaboration must carry a reason")
	}
	if err := r.Stop(bg, run, "again"); err != nil {
		t.Fatalf("Stop on a terminal ledger: %v", err)
	}
}

func TestStopWithoutLedgerIsNoOp(t *testing.T) {
	cs := newClient()
	tr := newScripted()
	r, _ := newRunner(t, cs, tr)
	if err := r.Stop(bg, runFor(), "WorkflowFailed"); err != nil {
		t.Fatal(err)
	}
	if tr.sendCount() != 0 {
		t.Fatal("stopping an unstarted step contacted a member")
	}
	if list, _ := cs.CoreV1().ConfigMaps("ns").List(bg, metav1.ListOptions{}); len(list.Items) != 0 {
		t.Fatalf("stop created %d objects", len(list.Items))
	}
}

func TestStopSurfacesLedgerFailures(t *testing.T) {
	cs := newClient()
	tr := newScripted()
	tr.hold = true
	r, _ := newRunner(t, cs, tr)
	run := runFor()
	if _, err := r.Advance(bg, run); err != nil {
		t.Fatal(err)
	}

	load := newClient()
	load.PrependReactor("get", "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errBoom
	})
	failing, _ := newRunner(t, load, tr)
	if err := failing.Stop(bg, run, "x"); !errors.Is(err, errBoom) {
		t.Fatalf("stop load failure = %v", err)
	}

	// The first read serves the stop mutation; every later read fails.
	reads := 0
	cs.PrependReactor("get", "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) {
		reads++
		if reads > 1 {
			return true, nil, errBoom
		}
		return false, nil, nil
	})
	if err := r.Stop(bg, run, "x"); !errors.Is(err, errBoom) {
		t.Fatalf("drain failure = %v", err)
	}
}

func TestProgressMapping(t *testing.T) {
	ok := progress(collaboration.State{Phase: collaboration.PhaseSucceeded, Outcome: &collaboration.Outcome{
		Phase: collaboration.PhaseSucceeded, Result: &collaboration.Content{Text: "final"}}})
	if !ok.Done || !ok.Succeeded || ok.Output != "final" {
		t.Fatalf("succeeded = %+v", ok)
	}
	bare := progress(collaboration.State{Phase: collaboration.PhaseSucceeded})
	if !bare.Done || !bare.Succeeded || bare.Output != "" {
		t.Fatalf("succeeded without result = %+v", bare)
	}
	reasoned := progress(collaboration.State{Phase: collaboration.PhaseFailed, Outcome: &collaboration.Outcome{
		Phase: collaboration.PhaseFailed, Reason: "TokenBudgetExhausted"}})
	if !reasoned.Done || reasoned.Succeeded || reasoned.Reason != "TokenBudgetExhausted" {
		t.Fatalf("failed = %+v", reasoned)
	}
	unreasoned := progress(collaboration.State{Phase: collaboration.PhaseCancelled})
	if !unreasoned.Done || unreasoned.Succeeded || unreasoned.Reason != string(collaboration.PhaseCancelled) {
		t.Fatalf("cancelled without outcome = %+v", unreasoned)
	}
}
