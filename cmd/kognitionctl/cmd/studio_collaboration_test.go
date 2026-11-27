// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cmd

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/collaboration"
)

func collabTestSpec() *v1alpha1.WorkflowCollaboration {
	return &v1alpha1.WorkflowCollaboration{
		Strategy: v1alpha1.CollaborationStrategySupervisor,
		Members: []v1alpha1.CollaborationMember{
			{Name: "writer", AgentRef: v1alpha1.AgentReference{Name: "wfteam-writer"}},
			{Name: "analyst", AgentRef: v1alpha1.AgentReference{Name: "wfteam-analyst"}},
		},
		Limits:     v1alpha1.CollaborationLimits{MaxTurns: 8, MaxDelegations: 2, MaxTokens: 9000},
		Supervisor: &v1alpha1.CollaborationSupervisor{Coordinator: "writer"},
	}
}

func collabTestState() collaboration.State {
	return collaboration.State{
		Version:  1,
		Strategy: v1alpha1.CollaborationStrategySupervisor,
		Phase:    collaboration.PhaseSucceeded,
		Outcome: &collaboration.Outcome{
			Phase:  collaboration.PhaseSucceeded,
			Result: &collaboration.Content{Text: "done", Origins: []string{"executor", "writer"}},
		},
		Counters: collaboration.Counters{Turns: 1, Messages: 1, TokensUsed: 1356},
		Tasks: []collaboration.Task{{
			ID: "t1", Recipient: "writer", Final: true, State: collaboration.TaskSucceeded, Turns: 1,
			Goal: collaboration.Content{Text: "write a paper"}, Result: &collaboration.Content{Text: "done"},
		}},
		Dispatches: []collaboration.Dispatch{{
			ID: "t1.1", Kind: collaboration.DispatchStart, TaskID: "t1", Recipient: "writer", Turn: 1, Status: collaboration.DispatchReported,
		}},
	}
}

func TestBuildCollabStepSeparatesAnsweringFromIdleMembers(t *testing.T) {
	step := collabRunStep{name: "prepare-answer", attempt: 1, collaboration: collabTestSpec()}
	jobs := map[string]*collabJob{"t1.1": {Name: "wfcj-x", Agent: "wfteam-writer", Phase: "Ready", TotalTokens: 1356}}

	got := buildCollabStep(step, collabTestState(), jobs)

	if !got.Available || got.FinalBy != "writer" || got.Counters.Delegations != 0 || got.Limits.MaxDelegations != 2 {
		t.Fatalf("summary = %+v", got)
	}
	byAlias := map[string]collabMember{}
	for _, m := range got.Members {
		byAlias[m.Alias] = m
	}
	writer, analyst := byAlias["writer"], byAlias["analyst"]
	if !writer.Involved || !writer.Final || !writer.Contributed || writer.Role != "coordinator" || writer.Tokens != 1356 || writer.Agent != "wfteam-writer" {
		t.Fatalf("writer = %+v", writer)
	}
	if analyst.Involved || analyst.Final || analyst.Contributed || analyst.Role != "specialist" {
		t.Fatalf("analyst = %+v", analyst)
	}
	if len(got.Dispatches) != 1 || got.Dispatches[0].Job == nil || got.Dispatches[0].Job.Name != "wfcj-x" {
		t.Fatalf("dispatches = %+v", got.Dispatches)
	}
}

func TestCollabExcerptTruncatesByRune(t *testing.T) {
	long := strings.Repeat("é", collabTextLimit+5)
	got := collabExcerpt(long)
	if !got.Truncated || got.Chars != collabTextLimit+5 || len([]rune(got.Text)) != collabTextLimit {
		t.Fatalf("excerpt = truncated %v chars %d runes %d", got.Truncated, got.Chars, len([]rune(got.Text)))
	}
	if short := collabExcerpt("abc"); short.Truncated || short.Text != "abc" {
		t.Fatalf("short = %+v", short)
	}
}

func TestCollabRoleByStrategy(t *testing.T) {
	spec := &v1alpha1.WorkflowCollaboration{
		Strategy: v1alpha1.CollaborationStrategyParallel,
		Parallel: &v1alpha1.CollaborationParallel{
			Branches:    []v1alpha1.CollaborationBranch{{Member: "a"}},
			Synthesizer: "s",
		},
	}
	for alias, want := range map[string]string{"a": "branch", "s": "synthesizer", "z": "member"} {
		if got := collabRole(spec, alias); got != want {
			t.Errorf("parallel %s = %q, want %q", alias, got, want)
		}
	}
	if got := collabRole(collabTestSpec(), "analyst"); got != "specialist" {
		t.Errorf("supervisor analyst = %q", got)
	}
}

func TestCollabRunStepsSkipsStepsWithoutAttempts(t *testing.T) {
	raw := []byte(`{"metadata":{"uid":"u1"},"status":{
		"stepStatuses":[{"name":"b","attempts":2}],
		"snapshotSpec":{"steps":[
			{"name":"a","collaboration":{"strategy":"Supervisor"}},
			{"name":"b","collaboration":{"strategy":"Supervisor"}},
			{"name":"c"}]}}}`)

	uid, steps, err := collabRunSteps(raw)
	if err != nil || uid != "u1" || len(steps) != 1 || steps[0].name != "b" || steps[0].attempt != 2 {
		t.Fatalf("uid=%q steps=%+v err=%v", uid, steps, err)
	}
}

func TestStudioRunCollaborationReadsLedgerAndJobs(t *testing.T) {
	const ns, runName, uid, step = "team", "run-1", "uid-1", "prepare-answer"

	spec, err := json.Marshal(collabTestSpec())
	if err != nil {
		t.Fatal(err)
	}
	var specMap map[string]any
	if err := json.Unmarshal(spec, &specMap); err != nil {
		t.Fatal(err)
	}
	run := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "kognition.dev/v1alpha1", "kind": "WorkflowRun",
		"metadata": map[string]any{"name": runName, "namespace": ns, "uid": uid},
		"status": map[string]any{
			"stepStatuses": []any{map[string]any{"name": step, "attempts": int64(1)}},
			"snapshotSpec": map[string]any{"steps": []any{map[string]any{"name": step, "collaboration": specMap}}},
		},
	}}
	raw, err := collaboration.Encode(collabTestState())
	if err != nil {
		t.Fatal(err)
	}
	ledger := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: collaboration.LedgerName(ns, uid, runName, step, 1), Namespace: ns},
		Data:       map[string]string{collaboration.LedgerStateKey: string(raw)},
	}
	job := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "kognition.dev/v1alpha1", "kind": "AgentJob",
		"metadata": map[string]any{"name": collaboration.JobName(ns, uid, step, 1, "t1.1"), "namespace": ns},
		"spec":     map[string]any{"agentRef": map[string]any{"name": "wfteam-writer"}},
		"status":   map[string]any{"phase": "Ready", "tokenUsage": map[string]any{"totalTokens": int64(1356)}},
	}}

	dyn := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme())
	dyn.PrependReactor("get", "workflowruns", func(k8stesting.Action) (bool, runtime.Object, error) { return true, run, nil })
	dyn.PrependReactor("get", "agentjobs", func(k8stesting.Action) (bool, runtime.Object, error) { return true, job, nil })
	prevMode, prevDyn, prevCore := k8sMode, k8sDynClient, k8sCoreClient
	k8sMode, k8sDynClient, k8sCoreClient = true, dyn, fake.NewSimpleClientset(ledger)
	t.Cleanup(func() { k8sMode, k8sDynClient, k8sCoreClient = prevMode, prevDyn, prevCore })

	mux := http.NewServeMux()
	registerStudioCollaborationAPI(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/workflowruns/"+runName+"/collaboration?namespace="+ns, nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var trace collabTrace
	if err := json.Unmarshal(rec.Body.Bytes(), &trace); err != nil {
		t.Fatal(err)
	}
	if len(trace.Steps) != 1 || !trace.Steps[0].Available || trace.Steps[0].FinalBy != "writer" {
		t.Fatalf("trace = %+v", trace)
	}
	if d := trace.Steps[0].Dispatches; len(d) != 1 || d[0].Job == nil || d[0].Job.TotalTokens != 1356 {
		t.Fatalf("dispatches = %+v", d)
	}
}

func TestStudioRunCollaborationOutsideKubernetesModeIsEmpty(t *testing.T) {
	prevMode := k8sMode
	k8sMode = false
	t.Cleanup(func() { k8sMode = prevMode })

	mux := http.NewServeMux()
	registerStudioCollaborationAPI(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/workflowruns/r/collaboration", nil))

	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != `{"steps":[]}` {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}
