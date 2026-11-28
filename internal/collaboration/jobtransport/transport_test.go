// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package jobtransport

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	api "github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/collaboration"
	"github.com/kognition-ai/kognition/internal/collaboration/a2atransport"
	"github.com/kognition-ai/kognition/internal/collaboration/driver"
	"github.com/kognition-ai/kognition/internal/controller"
	"github.com/kognition-ai/kognition/internal/store"
)

type fakeStore struct {
	store.Store
	jobs map[string][]byte
}

func newFakeStore() *fakeStore { return &fakeStore{jobs: map[string][]byte{}} }

func (f *fakeStore) Create(_ context.Context, kind, ns, name string, data []byte) error {
	if kind != "AgentJob" {
		return errors.New("unexpected kind " + kind)
	}
	if _, ok := f.jobs[ns+"/"+name]; ok {
		return store.ErrAlreadyExists
	}
	f.jobs[ns+"/"+name] = data
	return nil
}

func (f *fakeStore) Get(_ context.Context, _, ns, name string) ([]byte, error) {
	data, ok := f.jobs[ns+"/"+name]
	if !ok {
		return nil, store.ErrNotFound
	}
	return data, nil
}

func (f *fakeStore) Delete(_ context.Context, _, ns, name string) error {
	if _, ok := f.jobs[ns+"/"+name]; !ok {
		return store.ErrNotFound
	}
	delete(f.jobs, ns+"/"+name)
	return nil
}

func (f *fakeStore) setStatus(t *testing.T, ns, name string, status api.AgentJobStatus) {
	t.Helper()
	var job api.AgentJob
	if err := json.Unmarshal(f.jobs[ns+"/"+name], &job); err != nil {
		t.Fatal(err)
	}
	job.Status = status
	data, err := json.Marshal(job)
	if err != nil {
		t.Fatal(err)
	}
	f.jobs[ns+"/"+name] = data
}

func testRun() controller.WorkflowCollaborationRun {
	return controller.WorkflowCollaborationRun{
		Namespace: "work", OwnerKind: "WorkflowRun", RunName: "run-1", RunUID: "uid-1", Step: "collab", Attempt: 1,
		Spec: &api.WorkflowCollaboration{Members: []api.CollaborationMember{
			{Name: "researcher", AgentRef: api.AgentReference{Name: "research-agent"}},
			{Name: "writer", AgentRef: api.AgentReference{Name: "summarizer-agent"}},
			{Name: "remote", AgentRef: api.AgentReference{Name: "other", Namespace: "elsewhere"}},
			{Name: "pinned", AgentRef: api.AgentReference{Name: "pinned-agent"}, ServiceRef: &api.LocalObjectReference{Name: "svc"}},
		}},
	}
}

func startMsg(recipient string) collaboration.Message {
	return collaboration.Message{
		DispatchID: "d1", Kind: collaboration.DispatchStart, TaskID: "t1", Recipient: recipient, Turn: 1,
		Goal: collaboration.Content{Text: "summarize the findings"}, Allowance: 1000,
		AllowedOperations: []collaboration.OperationKind{collaboration.OpFinalize},
	}
}

func newTransport(t *testing.T, st *fakeStore, authorize Authorizer) *Transport {
	t.Helper()
	tr, err := New(Config{Store: st, Run: testRun(), Authorize: authorize})
	if err != nil {
		t.Fatal(err)
	}
	return tr
}

func TestNewRequiresIdentity(t *testing.T) {
	run := testRun()
	if _, err := New(Config{Run: run}); err == nil {
		t.Fatal("missing store accepted")
	}
	run.OwnerKind = ""
	if _, err := New(Config{Store: newFakeStore(), Run: run}); err == nil {
		t.Fatal("missing owner kind accepted")
	}
	run = testRun()
	run.Spec = nil
	if _, err := New(Config{Store: newFakeStore(), Run: run}); err == nil {
		t.Fatal("missing spec accepted")
	}
}

func TestSendCreatesOwnedAgentJobOnce(t *testing.T) {
	st := newFakeStore()
	tr := newTransport(t, st, nil)
	msg := startMsg("researcher")
	if err := tr.Send(context.Background(), msg); err != nil {
		t.Fatal(err)
	}
	if err := tr.Send(context.Background(), msg); err != nil {
		t.Fatalf("repeated send must be idempotent: %v", err)
	}
	if len(st.jobs) != 1 {
		t.Fatalf("jobs = %d, want 1", len(st.jobs))
	}
	name := JobName(testRun(), "d1")
	var job api.AgentJob
	if err := json.Unmarshal(st.jobs["work/"+name], &job); err != nil {
		t.Fatal(err)
	}
	if job.Spec.AgentRef == nil || job.Spec.AgentRef.Name != "research-agent" {
		t.Fatalf("agentRef = %+v", job.Spec.AgentRef)
	}
	if !strings.Contains(job.Spec.Input, "summarize the findings") {
		t.Fatalf("input = %q", job.Spec.Input)
	}
	if len(job.OwnerReferences) != 1 || job.OwnerReferences[0].Kind != "WorkflowRun" || job.OwnerReferences[0].Name != "run-1" || job.OwnerReferences[0].UID != "uid-1" {
		t.Fatalf("owner = %+v", job.OwnerReferences)
	}
	if _, stepJob := job.Labels["kognition.dev/workflow-step"]; stepJob {
		t.Fatal("collaboration job must not carry the workflow-step label")
	}
	if job.Labels[LabelMember] != "researcher" || job.Annotations[AnnotationDispatch] != "d1" {
		t.Fatalf("labels/annotations = %v %v", job.Labels, job.Annotations)
	}
	if other := JobName(testRun(), "d2"); other == name {
		t.Fatal("distinct dispatches must not share a job")
	}
}

func TestObserveMapsJobPhases(t *testing.T) {
	st := newFakeStore()
	tr := newTransport(t, st, nil)
	msg := startMsg("researcher")
	ctx := context.Background()

	if obs, err := tr.Observe(ctx, msg); err != nil || obs.Settled {
		t.Fatalf("before send: %+v %v", obs, err)
	}
	if err := tr.Send(ctx, msg); err != nil {
		t.Fatal(err)
	}
	name := JobName(testRun(), "d1")
	for _, phase := range []api.Phase{api.PhasePending, api.PhaseRunning} {
		st.setStatus(t, "work", name, api.AgentJobStatus{Phase: phase})
		if obs, err := tr.Observe(ctx, msg); err != nil || obs.Settled {
			t.Fatalf("%s: %+v %v", phase, obs, err)
		}
	}

	st.setStatus(t, "work", name, api.AgentJobStatus{Phase: api.PhaseReady, Output: "the answer", TokenUsage: &api.TokenUsage{TotalTokens: 321}})
	obs, err := tr.Observe(ctx, msg)
	if err != nil {
		t.Fatal(err)
	}
	if !obs.Settled || obs.Actor != "researcher" || obs.Status != collaboration.ReportCompleted {
		t.Fatalf("completed: %+v", obs)
	}
	if len(obs.Operations) != 1 || obs.Operations[0].Kind != collaboration.OpFinalize || obs.Operations[0].Result != "the answer" {
		t.Fatalf("operations = %+v", obs.Operations)
	}
	if !obs.TokensKnown || obs.TokensUsed != 321 {
		t.Fatalf("tokens = %d known=%t", obs.TokensUsed, obs.TokensKnown)
	}

	st.setStatus(t, "work", name, api.AgentJobStatus{Phase: api.PhaseReady, Output: "the answer"})
	if obs, _ := tr.Observe(ctx, msg); obs.TokensKnown {
		t.Fatal("absent usage must stay unknown so the engine charges the allowance")
	}

	for _, phase := range []api.Phase{api.PhaseFailed, api.PhaseTimedOut} {
		st.setStatus(t, "work", name, api.AgentJobStatus{Phase: phase})
		obs, err := tr.Observe(ctx, msg)
		if err != nil || !obs.Settled || obs.Status != collaboration.ReportFailed || obs.Failure == "" {
			t.Fatalf("%s: %+v %v", phase, obs, err)
		}
	}
}

func TestObserveOversizedJobOutputPreservesUsage(t *testing.T) {
	st := newFakeStore()
	tr := newTransport(t, st, nil)
	msg := startMsg("researcher")
	msg.Allowance = 5000
	ctx := context.Background()
	if err := tr.Send(ctx, msg); err != nil {
		t.Fatal(err)
	}
	st.setStatus(t, "work", JobName(testRun(), msg.DispatchID), api.AgentJobStatus{
		Phase:      api.PhaseReady,
		Output:     strings.Repeat("x", collaboration.MaxContentBytes+1),
		TokenUsage: &api.TokenUsage{TotalTokens: 2133},
	})
	obs, err := tr.Observe(ctx, msg)
	if err != nil {
		t.Fatal(err)
	}
	if !obs.Settled || obs.Status != collaboration.ReportFailed || !strings.HasPrefix(obs.Failure, a2atransport.FailureInvalidPayload) {
		t.Fatalf("oversized output was not rejected accurately: %+v", obs)
	}
	if !obs.TokensKnown || obs.TokensUsed != 2133 {
		t.Fatalf("reported usage was discarded: %+v", obs)
	}
}

func TestCancelDeletesTargetJob(t *testing.T) {
	st := newFakeStore()
	tr := newTransport(t, st, nil)
	ctx := context.Background()
	start := startMsg("researcher")
	if err := tr.Send(ctx, start); err != nil {
		t.Fatal(err)
	}
	cancel := collaboration.Message{DispatchID: "c1", Kind: collaboration.DispatchCancel, TaskID: "t1", Recipient: "researcher", Target: &start}
	if err := tr.Send(ctx, cancel); err != nil {
		t.Fatal(err)
	}
	if len(st.jobs) != 0 {
		t.Fatalf("job not deleted: %d", len(st.jobs))
	}
	if err := tr.Send(ctx, cancel); err != nil {
		t.Fatalf("cancel of a missing job must succeed: %v", err)
	}
	obs, err := tr.Observe(ctx, cancel)
	if err != nil || !obs.Settled || obs.Status != collaboration.ReportCancelled || obs.Actor != "researcher" {
		t.Fatalf("cancel observation: %+v %v", obs, err)
	}
	if err := tr.Send(ctx, collaboration.Message{Kind: collaboration.DispatchCancel, Recipient: "researcher"}); err == nil {
		t.Fatal("cancel without a target accepted")
	}
}

func TestCancelIsConfirmedWhileTheJobIsTerminating(t *testing.T) {
	st := newFakeStore()
	tr := newTransport(t, st, nil)
	ctx := context.Background()
	start := startMsg("researcher")
	if err := tr.Send(ctx, start); err != nil {
		t.Fatal(err)
	}
	name := JobName(testRun(), "d1")
	var job api.AgentJob
	if err := json.Unmarshal(st.jobs["work/"+name], &job); err != nil {
		t.Fatal(err)
	}
	cancel := collaboration.Message{DispatchID: "c1", Kind: collaboration.DispatchCancel, TaskID: "t1", Recipient: "researcher", Target: &start}
	if obs, _ := tr.Observe(ctx, cancel); obs.Settled {
		t.Fatal("cancel confirmed while the job is still live")
	}
	now := time.Now()
	job.ObjectMeta.DeletionTimestamp = &now
	data, err := json.Marshal(job)
	if err != nil {
		t.Fatal(err)
	}
	st.jobs["work/"+name] = data
	obs, err := tr.Observe(ctx, cancel)
	if err != nil || !obs.Settled || obs.Status != collaboration.ReportCancelled {
		t.Fatalf("terminating job: %+v %v", obs, err)
	}
	if obs, _ := tr.Observe(ctx, start); obs.Settled && obs.Status == collaboration.ReportCancelled {
		t.Fatal("a terminating job must not settle a start dispatch as cancelled")
	}
}

func TestAdmitRefusesUnsupportedMembers(t *testing.T) {
	tr := newTransport(t, newFakeStore(), nil)
	for _, recipient := range []string{"remote", "pinned", "nobody"} {
		if err := tr.Admit(context.Background(), startMsg(recipient)); !errors.Is(err, driver.ErrRefused) {
			t.Fatalf("%s: err = %v", recipient, err)
		}
	}
	if err := tr.Admit(context.Background(), startMsg("researcher")); err != nil {
		t.Fatal(err)
	}
}

func TestAdmitAppliesCommunicationPolicyToMemberRequests(t *testing.T) {
	var asked [][3]string
	verdict, failure := true, error(nil)
	tr := newTransport(t, newFakeStore(), func(_ context.Context, ns, source, target string) (bool, error) {
		asked = append(asked, [3]string{ns, source, target})
		return verdict, failure
	})
	ctx := context.Background()

	if err := tr.Admit(ctx, startMsg("researcher")); err != nil || len(asked) != 0 {
		t.Fatalf("executor dispatch consulted policy: %v %v", err, asked)
	}
	delegated := startMsg("writer")
	delegated.Requester = "researcher"
	if err := tr.Admit(ctx, delegated); err != nil {
		t.Fatal(err)
	}
	if len(asked) != 1 || asked[0] != [3]string{"work", "research-agent", "summarizer-agent"} {
		t.Fatalf("asked = %v", asked)
	}
	verdict = false
	if err := tr.Admit(ctx, delegated); !errors.Is(err, driver.ErrRefused) {
		t.Fatalf("denied delegation: %v", err)
	}
	verdict, failure = true, errors.New("policy store down")
	if err := tr.Admit(ctx, delegated); err == nil || errors.Is(err, driver.ErrRefused) {
		t.Fatalf("policy failure must leave the dispatch undecided: %v", err)
	}
}

func TestFactoryBuildsTransport(t *testing.T) {
	tr, err := Factory{Store: newFakeStore()}.Transport(context.Background(), testRun(), nil)
	if err != nil || tr == nil {
		t.Fatalf("%v %v", tr, err)
	}
}
