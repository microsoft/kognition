// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap/zaptest"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/observability"
	"github.com/kognition-ai/kognition/internal/store"
)

// fakeWorkflowToolInvoker records invocations and returns a configured output or error.
type fakeWorkflowToolInvoker struct {
	calls  []fakeToolInvocation
	output map[string]any
	err    error
	// failN, when > 0, fails the first failN calls with err and then returns
	// output — used to exercise retry recovery.
	failN int
	// block, when true, waits for context cancellation and returns ctx.Err() —
	// used to exercise per-step timeout.
	block bool
}

type fakeToolInvocation struct {
	Namespace   string
	Principal   string
	ToolName    string
	Input       map[string]any
	SpanContext observability.SpanContext
}

type conflictOnToolCompletionStore struct {
	store.Store
	injected bool
}

func (s *conflictOnToolCompletionStore) Update(ctx context.Context, kind, namespace, name string, data []byte, resourceVersion int64) error {
	if (kind == "Workflow" || kind == "WorkflowRun") && !s.injected && strings.Contains(string(data), workflowCompletedToolsKey) {
		raw, err := s.Store.Get(ctx, kind, namespace, name)
		if err != nil {
			return err
		}
		var metadata *v1alpha1.ObjectMeta
		var conditions *[]v1alpha1.Condition
		var concurrent any
		if kind == "Workflow" {
			workflow := &v1alpha1.Workflow{}
			concurrent = workflow
			metadata = &workflow.ObjectMeta
			conditions = &workflow.Status.Conditions
		} else {
			run := &v1alpha1.WorkflowRun{}
			concurrent = run
			metadata = &run.ObjectMeta
			conditions = &run.Status.Conditions
		}
		if err := json.Unmarshal(raw, concurrent); err != nil {
			return err
		}
		if metadata.Annotations == nil {
			metadata.Annotations = map[string]string{}
		}
		metadata.Annotations["example.com/concurrent"] = "preserved"
		*conditions = append(*conditions, v1alpha1.Condition{Type: "Audited", Status: "True", Reason: "ConcurrentObserver"})
		updated, err := json.Marshal(concurrent)
		if err != nil {
			return err
		}
		if err := s.Store.Update(ctx, kind, namespace, name, updated, resourceVersion); err != nil {
			return err
		}
		s.injected = true
		return store.ErrConflict
	}
	return s.Store.Update(ctx, kind, namespace, name, data, resourceVersion)
}

func TestWorkflowReconciler_ToolStep_ConflictPreservesConcurrentStateWithoutRepeat(t *testing.T) {
	base := newMemStore()
	s := &conflictOnToolCompletionStore{Store: base}
	invoker := &fakeWorkflowToolInvoker{output: map[string]any{"ok": true}}
	wf := makeToolStepWorkflow("default", "wf-tool-conflict")
	wf.Spec.EntryPoint = "fetch"
	wf.Spec.Edges = []v1alpha1.WorkflowEdge{{From: "fetch", To: "fetch"}}
	wf.Spec.Budgets = &v1alpha1.WorkflowBudget{MaxIterations: 1}
	storeWorkflow(t, base, wf)

	r := NewWorkflowReconciler(s, zaptest.NewLogger(t))
	r.SetToolInvoker(invoker)
	req := Request{Namespace: "default", Name: wf.ObjectMeta.Name, Kind: "Workflow"}
	for attempt := 0; attempt < 6; attempt++ {
		mustReconcile(t, r, req)
		if getWorkflow(t, base, "default", wf.ObjectMeta.Name).Status.Phase == v1alpha1.PhaseReady {
			break
		}
	}

	stored := getWorkflow(t, base, "default", wf.ObjectMeta.Name)
	if stored.Status.Phase != v1alpha1.PhaseReady {
		t.Fatalf("workflow phase = %q, want Ready", stored.Status.Phase)
	}
	if len(invoker.calls) != 1 {
		t.Fatalf("tool invocations = %d, want 1", len(invoker.calls))
	}
	if condition := findCondition(stored.Status.Conditions, "Audited"); condition == nil || condition.Reason != "ConcurrentObserver" {
		t.Fatalf("concurrent condition was lost: condition=%#v all=%#v", condition, stored.Status.Conditions)
	}
	if got := stored.ObjectMeta.Annotations["example.com/concurrent"]; got != "preserved" {
		t.Fatalf("concurrent annotation = %q, want preserved", got)
	}
	if !strings.Contains(stored.ObjectMeta.Annotations[workflowCompletedToolsKey], "fetch@1") {
		t.Fatalf("completion annotation = %q, want fetch@1", stored.ObjectMeta.Annotations[workflowCompletedToolsKey])
	}
}

func (f *fakeWorkflowToolInvoker) InvokeTool(ctx context.Context, ns, principal, name string, input map[string]any) (map[string]any, error) {
	spanContext, _ := observability.SpanContextFromContext(ctx)
	f.calls = append(f.calls, fakeToolInvocation{Namespace: ns, Principal: principal, ToolName: name, Input: input, SpanContext: spanContext})
	if f.block {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if f.failN > 0 {
		if len(f.calls) <= f.failN {
			return nil, f.err
		}
		return f.output, nil
	}
	if f.err != nil {
		return nil, f.err
	}
	return f.output, nil
}

func TestWorkflowRunReconciler_ToolStepTracingExportsRunAndStepSpans(t *testing.T) {
	es := newEventMemStore()
	tracer := observability.NewTracer(observability.TracerConfig{SampleRate: 1.0})
	collector := observability.NewCollector()
	exporter := &workflowSpanExporter{}
	collector.RegisterSpanExporter(exporter)
	invoker := &fakeWorkflowToolInvoker{output: map[string]any{"status": "ok"}}
	reconciler := NewWorkflowRunReconciler(es, zaptest.NewLogger(t))
	reconciler.SetToolInvoker(invoker)
	reconciler.SetTracing(tracer, collector)

	wf := makeToolStepWorkflow("default", "traced-wf")
	storeWorkflow(t, es.memStore, wf)
	run := makeWorkflowRun("default", "traced-run", wf.ObjectMeta.Name)
	storeWorkflowRun(t, es, run)

	driveRunToCompletion(t, reconciler, es, "default", run.ObjectMeta.Name)

	runSpans := map[observability.SpanID]*observability.Span{}
	var stepSpans []*observability.Span
	for _, span := range exporter.spans {
		switch span.Name {
		case observability.SpanNameWorkflowRun:
			runSpans[span.Context.SpanID] = span
		case observability.SpanNameWorkflowStep:
			stepSpans = append(stepSpans, span)
		}
	}
	if len(stepSpans) != 1 {
		t.Fatalf("workflow step spans = %d, want 1; spans=%+v", len(stepSpans), exporter.spans)
	}
	stepSpan := stepSpans[0]
	runSpan := runSpans[stepSpan.Context.ParentID]
	if runSpan == nil || stepSpan == nil {
		t.Fatalf("missing run or step span: %+v", exporter.spans)
	}
	if runSpan.StatusCode != observability.StatusOK || stepSpan.StatusCode != observability.StatusOK {
		t.Fatalf("span statuses = run %v step %v, want OK", runSpan.StatusCode, stepSpan.StatusCode)
	}
	if stepSpan.Context.TraceID != runSpan.Context.TraceID {
		t.Fatalf("step trace id = %x, want run trace id %x", stepSpan.Context.TraceID, runSpan.Context.TraceID)
	}
	if stepSpan.Context.ParentID != runSpan.Context.SpanID {
		t.Fatalf("step parent = %x, want run span %x", stepSpan.Context.ParentID, runSpan.Context.SpanID)
	}
	if len(invoker.calls) != 1 {
		t.Fatalf("tool invocations = %d, want 1", len(invoker.calls))
	}
	if invoker.calls[0].SpanContext.TraceID != stepSpan.Context.TraceID || invoker.calls[0].SpanContext.SpanID != stepSpan.Context.SpanID {
		t.Fatalf("invoker span context = %+v, want step context %+v", invoker.calls[0].SpanContext, stepSpan.Context)
	}
	storedRun := getWorkflowRun(t, es, "default", run.ObjectMeta.Name)
	if storedRun.ObjectMeta.Annotations[observability.AnnotationTraceID] != observability.HexTraceID(runSpan.Context.TraceID) {
		t.Fatalf("stored trace id annotation = %q, want %q", storedRun.ObjectMeta.Annotations[observability.AnnotationTraceID], observability.HexTraceID(runSpan.Context.TraceID))
	}
	if storedRun.ObjectMeta.Annotations[observability.AnnotationRootSpanID] != observability.HexSpanID(runSpan.Context.SpanID) {
		t.Fatalf("stored root span annotation = %q, want %q", storedRun.ObjectMeta.Annotations[observability.AnnotationRootSpanID], observability.HexSpanID(runSpan.Context.SpanID))
	}
}

func TestWorkflowRunReconciler_ToolStepTracingCreatesIndependentRootTraces(t *testing.T) {
	es := newEventMemStore()
	tracer := observability.NewTracer(observability.TracerConfig{SampleRate: 1.0})
	collector := observability.NewCollector()
	exporter := &workflowSpanExporter{}
	collector.RegisterSpanExporter(exporter)
	reconciler := NewWorkflowRunReconciler(es, zaptest.NewLogger(t))
	reconciler.SetToolInvoker(&fakeWorkflowToolInvoker{output: map[string]any{"status": "ok"}})
	reconciler.SetTracing(tracer, collector)

	wf := makeToolStepWorkflow("default", "traced-wf")
	storeWorkflow(t, es.memStore, wf)
	for _, name := range []string{"traced-run-a", "traced-run-b"} {
		run := makeWorkflowRun("default", name, wf.ObjectMeta.Name)
		storeWorkflowRun(t, es, run)
		driveRunToCompletion(t, reconciler, es, "default", name)
	}

	rootTraceIDs := map[observability.TraceID]bool{}
	for _, span := range exporter.spans {
		if span.Name == observability.SpanNameWorkflowRun {
			rootTraceIDs[span.Context.TraceID] = true
		}
	}
	if len(rootTraceIDs) != 2 {
		t.Fatalf("workflow run root trace ids = %d unique values, want 2; spans=%+v", len(rootTraceIDs), exporter.spans)
	}
}

func TestWorkflowRunReconciler_AgentStepTracingExportsDispatchSpan(t *testing.T) {
	es := newEventMemStore()
	tracer := observability.NewTracer(observability.TracerConfig{SampleRate: 1.0})
	collector := observability.NewCollector()
	exporter := &workflowSpanExporter{}
	collector.RegisterSpanExporter(exporter)
	reconciler := NewWorkflowRunReconciler(es, zaptest.NewLogger(t))
	reconciler.SetTracing(tracer, collector)

	wf := v1alpha1.Workflow{
		TypeMeta:   v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "Workflow"},
		ObjectMeta: v1alpha1.ObjectMeta{Name: "agent-wf", Namespace: "default", UID: "wf-agent-uid"},
		Spec: v1alpha1.WorkflowSpec{
			Steps: []v1alpha1.WorkflowStep{{Name: "plan", AgentRef: v1alpha1.LocalObjectReference{Name: "planner"}}},
		},
		Status: v1alpha1.WorkflowStatus{Phase: v1alpha1.PhasePending},
	}
	storeWorkflow(t, es.memStore, wf)
	run := makeWorkflowRun("default", "agent-run", wf.ObjectMeta.Name)
	storeWorkflowRun(t, es, run)

	driveRunToCompletion(t, reconciler, es, "default", run.ObjectMeta.Name)

	runSpans := map[observability.SpanID]*observability.Span{}
	var stepSpans []*observability.Span
	for _, span := range exporter.spans {
		switch span.Name {
		case observability.SpanNameWorkflowRun:
			runSpans[span.Context.SpanID] = span
		case observability.SpanNameWorkflowStep:
			stepSpans = append(stepSpans, span)
		}
	}
	if len(stepSpans) != 1 {
		t.Fatalf("workflow step spans = %d, want 1; spans=%+v", len(stepSpans), exporter.spans)
	}
	stepSpan := stepSpans[0]
	runSpan := runSpans[stepSpan.Context.ParentID]
	if runSpan == nil || stepSpan == nil {
		t.Fatalf("missing run or step span: %+v", exporter.spans)
	}
	if stepSpan.StatusCode != observability.StatusOK {
		t.Fatalf("step span status = %v, want OK", stepSpan.StatusCode)
	}
	if stepSpan.Context.TraceID != runSpan.Context.TraceID {
		t.Fatalf("step trace id = %x, want run trace id %x", stepSpan.Context.TraceID, runSpan.Context.TraceID)
	}
	if stepSpan.Context.ParentID != runSpan.Context.SpanID {
		t.Fatalf("step parent = %x, want run span %x", stepSpan.Context.ParentID, runSpan.Context.SpanID)
	}
	if got := stepSpan.Attributes[observability.AttrStepName]; got != "plan" {
		t.Fatalf("step span name attr = %v, want plan", got)
	}

	jobs := listWorkflowJobs(t, es.memStore, "default")
	if len(jobs) != 1 {
		t.Fatalf("jobs = %d, want 1", len(jobs))
	}
	if got := jobs[0].ObjectMeta.Annotations[observability.AnnotationSpanID]; got != observability.HexSpanID(stepSpan.Context.SpanID) {
		t.Fatalf("job span annotation = %q, want step span %q", got, observability.HexSpanID(stepSpan.Context.SpanID))
	}
	if got := jobs[0].ObjectMeta.Annotations[observability.AnnotationParentSpanID]; got != observability.HexSpanID(runSpan.Context.SpanID) {
		t.Fatalf("job parent span annotation = %q, want run span %q", got, observability.HexSpanID(runSpan.Context.SpanID))
	}
}

func TestWorkflowRunReconciler_AgentStepTracingKeepsOneTraceAcrossReconciles(t *testing.T) {
	es := newEventMemStore()
	tracer := observability.NewTracer(observability.TracerConfig{SampleRate: 1.0})
	collector := observability.NewCollector()
	exporter := &workflowSpanExporter{}
	collector.RegisterSpanExporter(exporter)
	reconciler := NewWorkflowRunReconciler(es, zaptest.NewLogger(t))
	reconciler.SetTracing(tracer, collector)

	wf := v1alpha1.Workflow{
		TypeMeta:   v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "Workflow"},
		ObjectMeta: v1alpha1.ObjectMeta{Name: "agent-chain", Namespace: "default", UID: "wf-agent-chain-uid"},
		Spec: v1alpha1.WorkflowSpec{
			EntryPoint: "research",
			Steps: []v1alpha1.WorkflowStep{
				{Name: "research", AgentRef: v1alpha1.LocalObjectReference{Name: "researcher"}},
				{Name: "summarize", AgentRef: v1alpha1.LocalObjectReference{Name: "summarizer"}},
			},
			Edges: []v1alpha1.WorkflowEdge{{From: "research", To: "summarize"}},
		},
		Status: v1alpha1.WorkflowStatus{Phase: v1alpha1.PhasePending},
	}
	storeWorkflow(t, es.memStore, wf)
	run := makeWorkflowRun("default", "agent-chain-run", wf.ObjectMeta.Name)
	storeWorkflowRun(t, es, run)

	driveRunToCompletion(t, reconciler, es, "default", run.ObjectMeta.Name)

	var runSpans []*observability.Span
	var stepSpans []*observability.Span
	for _, span := range exporter.spans {
		switch span.Name {
		case observability.SpanNameWorkflowRun:
			runSpans = append(runSpans, span)
		case observability.SpanNameWorkflowStep:
			stepSpans = append(stepSpans, span)
		}
	}
	if len(runSpans) != 1 {
		t.Fatalf("workflow run spans = %d, want 1; spans=%+v", len(runSpans), exporter.spans)
	}
	if len(stepSpans) != 2 {
		t.Fatalf("workflow step spans = %d, want 2; spans=%+v", len(stepSpans), exporter.spans)
	}
	runSpan := runSpans[0]
	for _, stepSpan := range stepSpans {
		if stepSpan.Context.TraceID != runSpan.Context.TraceID {
			t.Fatalf("step %q trace id = %x, want run trace id %x", stepSpan.Attributes[observability.AttrStepName], stepSpan.Context.TraceID, runSpan.Context.TraceID)
		}
		if stepSpan.Context.ParentID != runSpan.Context.SpanID {
			t.Fatalf("step %q parent = %x, want run span %x", stepSpan.Attributes[observability.AttrStepName], stepSpan.Context.ParentID, runSpan.Context.SpanID)
		}
	}

	storedRun := getWorkflowRun(t, es, "default", run.ObjectMeta.Name)
	runTraceID := storedRun.ObjectMeta.Annotations[observability.AnnotationTraceID]
	runRootSpanID := storedRun.ObjectMeta.Annotations[observability.AnnotationRootSpanID]
	if runTraceID != observability.HexTraceID(runSpan.Context.TraceID) {
		t.Fatalf("stored run trace id = %q, want %q", runTraceID, observability.HexTraceID(runSpan.Context.TraceID))
	}
	jobs := listWorkflowJobs(t, es.memStore, "default")
	if len(jobs) != 2 {
		t.Fatalf("jobs = %d, want 2", len(jobs))
	}
	for _, job := range jobs {
		if got := job.ObjectMeta.Annotations[observability.AnnotationTraceID]; got != runTraceID {
			t.Fatalf("job %q trace id = %q, want run trace id %q", job.ObjectMeta.Name, got, runTraceID)
		}
		if got := job.ObjectMeta.Annotations[observability.AnnotationParentSpanID]; got != runRootSpanID {
			t.Fatalf("job %q parent span = %q, want run root span %q", job.ObjectMeta.Name, got, runRootSpanID)
		}
	}
}

func makeToolStepWorkflow(ns, name string) v1alpha1.Workflow {
	return v1alpha1.Workflow{
		TypeMeta: v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "Workflow"},
		ObjectMeta: v1alpha1.ObjectMeta{
			Name:      name,
			Namespace: ns,
			UID:       "wf-tool-uid",
		},
		Spec: v1alpha1.WorkflowSpec{
			Steps: []v1alpha1.WorkflowStep{
				{
					Name:    "fetch",
					Type:    v1alpha1.WorkflowStepTypeTool,
					ToolRef: v1alpha1.LocalObjectReference{Name: "http-get"},
					Input:   `{"url":"https://example.com"}`,
				},
			},
		},
		Status: v1alpha1.WorkflowStatus{Phase: v1alpha1.PhasePending},
	}
}

// TC-0232: tool step executes synchronously via WorkflowToolInvoker and creates no AgentJob.
func TestWorkflowReconciler_ToolStep_ExecutesViaInvokerWithoutAgentJob(t *testing.T) {
	s := newMemStore()
	r := NewWorkflowReconciler(s, zaptest.NewLogger(t))
	invoker := &fakeWorkflowToolInvoker{output: map[string]any{"status": "ok", "body": "hello"}}
	r.SetToolInvoker(invoker)

	wf := makeToolStepWorkflow("default", "wf-tool")
	storeWorkflow(t, s, wf)

	req := Request{Namespace: "default", Name: "wf-tool", Kind: "Workflow"}
	mustReconcile(t, r, req)
	mustReconcile(t, r, req)

	if jobs := listWorkflowJobs(t, s, "default"); len(jobs) != 0 {
		t.Fatalf("expected no AgentJobs for tool step, got %d", len(jobs))
	}
	if len(invoker.calls) != 1 {
		t.Fatalf("expected 1 tool invocation, got %d", len(invoker.calls))
	}
	call := invoker.calls[0]
	if call.ToolName != "http-get" {
		t.Fatalf("invoked tool = %q, want http-get", call.ToolName)
	}
	if got, want := call.Principal, "workflow:wf-tool"; got != want {
		t.Fatalf("principal = %q, want %q", got, want)
	}
	if call.Input["url"] != "https://example.com" {
		t.Fatalf("input url = %v, want https://example.com", call.Input["url"])
	}

	stored := getWorkflow(t, s, "default", "wf-tool")
	if stored.Status.Phase != v1alpha1.PhaseReady {
		t.Fatalf("workflow phase = %q, want %q", stored.Status.Phase, v1alpha1.PhaseReady)
	}
	if len(stored.Status.StepStatuses) != 1 || stored.Status.StepStatuses[0].Phase != v1alpha1.PhaseReady {
		t.Fatalf("tool step status = %+v, want Ready", stored.Status.StepStatuses)
	}
	if !strings.Contains(stored.Status.StepStatuses[0].Output, "hello") {
		t.Fatalf("tool step output = %q, want to contain hello", stored.Status.StepStatuses[0].Output)
	}
}

func TestWorkflowReconciler_CyclicToolStep_ExecutesOncePerIterationWithoutAgentJob(t *testing.T) {
	s := newMemStore()
	r := NewWorkflowReconciler(s, zaptest.NewLogger(t))
	invoker := &fakeWorkflowToolInvoker{output: map[string]any{"status": "ok"}}
	r.SetToolInvoker(invoker)

	wf := makeToolStepWorkflow("default", "wf-cyclic-tool")
	wf.Spec.EntryPoint = "fetch"
	wf.Spec.Edges = []v1alpha1.WorkflowEdge{{From: "fetch", To: "fetch"}}
	wf.Spec.Budgets = &v1alpha1.WorkflowBudget{MaxIterations: 2}
	storeWorkflow(t, s, wf)

	req := Request{Namespace: "default", Name: "wf-cyclic-tool", Kind: "Workflow"}
	mustReconcile(t, r, req) // finalizer
	mustReconcile(t, r, req) // iteration 1
	mustReconcile(t, r, req) // iteration 2

	if jobs := listWorkflowJobs(t, s, "default"); len(jobs) != 0 {
		t.Fatalf("expected no AgentJobs for cyclic tool step, got %d", len(jobs))
	}
	if len(invoker.calls) != 2 {
		t.Fatalf("expected one invocation per iteration, got %d", len(invoker.calls))
	}

	stored := getWorkflow(t, s, "default", "wf-cyclic-tool")
	if stored.Status.Phase != v1alpha1.PhaseReady {
		t.Fatalf("workflow phase = %q, want Ready", stored.Status.Phase)
	}
	ss := findStepStatus(&stored, "fetch")
	if ss == nil || ss.Phase != v1alpha1.PhaseReady || ss.Attempts != 1 {
		t.Fatalf("tool step status = %+v, want Ready with one attempt in the final iteration", ss)
	}
	if len(stored.Status.CompletedSteps) != 2 {
		t.Fatalf("completed steps = %v, want one tool completion per iteration", stored.Status.CompletedSteps)
	}
}

func TestWorkflowReconciler_CyclicToolStep_OutputFlowsToDownstreamAgent(t *testing.T) {
	s := newMemStore()
	r := NewWorkflowReconciler(s, zaptest.NewLogger(t))
	invoker := &fakeWorkflowToolInvoker{output: map[string]any{"value": 42}}
	r.SetToolInvoker(invoker)

	wf := v1alpha1.Workflow{
		TypeMeta:   v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "Workflow"},
		ObjectMeta: v1alpha1.ObjectMeta{Name: "wf-cyclic-tool-chain", Namespace: "default", UID: "wf-cyclic-tool-chain-uid"},
		Spec: v1alpha1.WorkflowSpec{
			EntryPoint: "fetch",
			Steps: []v1alpha1.WorkflowStep{
				{Name: "fetch", Type: v1alpha1.WorkflowStepTypeTool, ToolRef: v1alpha1.LocalObjectReference{Name: "lookup"}},
				{Name: "summarize", AgentRef: v1alpha1.LocalObjectReference{Name: "summarizer"}, Input: `{"payload":{{ .Steps.fetch.Output }}}`},
			},
			Edges: []v1alpha1.WorkflowEdge{
				{From: "fetch", To: "summarize"},
				{From: "summarize", To: "fetch"},
			},
			Budgets: &v1alpha1.WorkflowBudget{MaxIterations: 1},
		},
		Status: v1alpha1.WorkflowStatus{Phase: v1alpha1.PhasePending},
	}
	storeWorkflow(t, s, wf)

	req := Request{Namespace: "default", Name: "wf-cyclic-tool-chain", Kind: "Workflow"}
	mustReconcile(t, r, req) // finalizer
	mustReconcile(t, r, req) // invoke tool and dispatch downstream agent

	jobs := listWorkflowJobs(t, s, "default")
	if len(jobs) != 1 {
		t.Fatalf("expected one downstream AgentJob, got %d", len(jobs))
	}
	if got := jobs[0].ObjectMeta.Labels[workflowStepLabel]; got != "summarize" {
		t.Fatalf("downstream job step = %q, want summarize", got)
	}
	if !strings.Contains(jobs[0].Spec.Input, `"value":42`) {
		t.Fatalf("downstream job input = %q, want tool output value=42", jobs[0].Spec.Input)
	}
	if len(invoker.calls) != 1 {
		t.Fatalf("expected one tool invocation, got %d", len(invoker.calls))
	}
}

func TestWorkflowReconciler_CyclicToolStep_RetryRecoversWithinIteration(t *testing.T) {
	s := newMemStore()
	r := NewWorkflowReconciler(s, zaptest.NewLogger(t))
	invoker := &fakeWorkflowToolInvoker{failN: 1, err: errors.New("transient"), output: map[string]any{"ok": true}}
	r.SetToolInvoker(invoker)

	wf := makeToolStepWorkflow("default", "wf-cyclic-tool-retry")
	wf.Spec.EntryPoint = "fetch"
	wf.Spec.Edges = []v1alpha1.WorkflowEdge{{From: "fetch", To: "fetch"}}
	wf.Spec.Budgets = &v1alpha1.WorkflowBudget{MaxIterations: 1}
	wf.Spec.Steps[0].Retry = &v1alpha1.WorkflowStepRetry{MaxAttempts: 2, Backoff: "0s"}
	storeWorkflow(t, s, wf)

	req := Request{Namespace: "default", Name: "wf-cyclic-tool-retry", Kind: "Workflow"}
	mustReconcile(t, r, req) // finalizer
	mustReconcile(t, r, req) // attempt 1 fails and schedules retry
	mustReconcile(t, r, req) // attempt 2 succeeds

	stored := getWorkflow(t, s, "default", "wf-cyclic-tool-retry")
	if stored.Status.Phase != v1alpha1.PhaseReady {
		t.Fatalf("workflow phase = %q, want Ready", stored.Status.Phase)
	}
	ss := findStepStatus(&stored, "fetch")
	if ss == nil || ss.Phase != v1alpha1.PhaseReady || ss.Attempts != 2 {
		t.Fatalf("tool step status = %+v, want Ready after two attempts", ss)
	}
	if len(invoker.calls) != 2 {
		t.Fatalf("expected two tool invocations, got %d", len(invoker.calls))
	}
}

func TestWorkflowReconciler_CyclicToolStep_TimeoutContinuesWithoutRedispatch(t *testing.T) {
	s := newMemStore()
	r := NewWorkflowReconciler(s, zaptest.NewLogger(t))
	invoker := &fakeWorkflowToolInvoker{block: true}
	r.SetToolInvoker(invoker)

	wf := makeToolStepWorkflow("default", "wf-cyclic-tool-timeout")
	wf.Spec.EntryPoint = "fetch"
	wf.Spec.Edges = []v1alpha1.WorkflowEdge{{From: "fetch", To: "fetch"}}
	wf.Spec.Budgets = &v1alpha1.WorkflowBudget{MaxIterations: 1}
	wf.Spec.Steps[0].Timeout = "1ms"
	wf.Spec.Steps[0].ContinueOnError = true
	storeWorkflow(t, s, wf)

	req := Request{Namespace: "default", Name: "wf-cyclic-tool-timeout", Kind: "Workflow"}
	mustReconcile(t, r, req) // finalizer
	mustReconcile(t, r, req) // timeout is ignored and workflow completes
	mustReconcile(t, r, req) // terminal reconciliation must not invoke again

	stored := getWorkflow(t, s, "default", "wf-cyclic-tool-timeout")
	if stored.Status.Phase != v1alpha1.PhaseReady {
		t.Fatalf("workflow phase = %q, want Ready", stored.Status.Phase)
	}
	ss := findStepStatus(&stored, "fetch")
	if ss == nil || ss.Phase != v1alpha1.PhaseFailed {
		t.Fatalf("tool step status = %+v, want Failed", ss)
	}
	if len(invoker.calls) != 1 {
		t.Fatalf("expected timed-out tool to be invoked once, got %d", len(invoker.calls))
	}
}

func TestWorkflowReconciler_CyclicToolStep_NilInvokerFailsTerminally(t *testing.T) {
	s := newMemStore()
	r := NewWorkflowReconciler(s, zaptest.NewLogger(t))
	wf := makeToolStepWorkflow("default", "wf-cyclic-tool-noinv")
	wf.Spec.EntryPoint = "fetch"
	wf.Spec.Edges = []v1alpha1.WorkflowEdge{{From: "fetch", To: "fetch"}}
	wf.Spec.Budgets = &v1alpha1.WorkflowBudget{MaxIterations: 1}
	storeWorkflow(t, s, wf)

	req := Request{Namespace: "default", Name: "wf-cyclic-tool-noinv", Kind: "Workflow"}
	mustReconcile(t, r, req) // finalizer
	result, err := r.reconcileDirect(context.Background(), req)
	if err != nil {
		t.Fatalf("reconcile cyclic tool without invoker: %v", err)
	}
	if result.Requeue || result.RequeueAfter > 0 {
		t.Fatalf("expected terminal result, got %+v", result)
	}

	stored := getWorkflow(t, s, "default", "wf-cyclic-tool-noinv")
	if stored.Status.Phase != v1alpha1.PhaseFailed {
		t.Fatalf("workflow phase = %q, want Failed", stored.Status.Phase)
	}
	if stored.Status.ActiveStep != "fetch@1" {
		t.Fatalf("active step = %q, want fetch@1", stored.Status.ActiveStep)
	}
}

func TestWorkflowReconciler_CyclicToolStep_InvokerErrorFailsTerminally(t *testing.T) {
	s := newMemStore()
	r := NewWorkflowReconciler(s, zaptest.NewLogger(t))
	invoker := &fakeWorkflowToolInvoker{err: errors.New("tool grant denied")}
	r.SetToolInvoker(invoker)

	wf := makeToolStepWorkflow("default", "wf-cyclic-tool-denied")
	wf.Spec.EntryPoint = "fetch"
	wf.Spec.Edges = []v1alpha1.WorkflowEdge{{From: "fetch", To: "fetch"}}
	wf.Spec.Budgets = &v1alpha1.WorkflowBudget{MaxIterations: 1}
	storeWorkflow(t, s, wf)
	req := Request{Namespace: "default", Name: wf.ObjectMeta.Name, Kind: "Workflow"}
	mustReconcile(t, r, req)

	result, err := r.reconcileDirect(context.Background(), req)
	if err != nil || result.Requeue || result.RequeueAfter != 0 {
		t.Fatalf("result = %+v, error = %v; want terminal tool failure", result, err)
	}
	stored := getWorkflow(t, s, "default", wf.ObjectMeta.Name)
	stepStatus := findStepStatus(&stored, "fetch")
	if stored.Status.Phase != v1alpha1.PhaseFailed || stored.Status.ActiveStep != "fetch@1" {
		t.Fatalf("workflow status = %+v, want failed at fetch@1", stored.Status)
	}
	if stepStatus == nil || stepStatus.Phase != v1alpha1.PhaseFailed || stepStatus.Attempts != 1 {
		t.Fatalf("step status = %+v, want failed first attempt", stepStatus)
	}
	if len(invoker.calls) != 1 {
		t.Fatalf("tool invocations = %d, want 1", len(invoker.calls))
	}
}

func TestWorkflowReconciler_CyclicToolStep_FailureUpdateError(t *testing.T) {
	memory := newMemStore()
	wf := makeToolStepWorkflow("default", "wf-cyclic-tool-failure-update")
	wf.Spec.EntryPoint = "fetch"
	wf.Spec.Edges = []v1alpha1.WorkflowEdge{{From: "fetch", To: "fetch"}}
	wf.Spec.Budgets = &v1alpha1.WorkflowBudget{MaxIterations: 1}
	storeWorkflow(t, memory, wf)
	req := Request{Namespace: "default", Name: wf.ObjectMeta.Name, Kind: "Workflow"}
	mustReconcile(t, NewWorkflowReconciler(memory, zaptest.NewLogger(t)), req)
	wantErr := errors.New("workflow store unavailable")
	r := NewWorkflowReconciler(&faultStore{memStore: memory, updateErr: wantErr}, zaptest.NewLogger(t))
	r.SetToolInvoker(&fakeWorkflowToolInvoker{err: errors.New("tool grant denied")})

	_, err := r.reconcileDirect(context.Background(), req)
	if !errors.Is(err, wantErr) || !strings.Contains(err.Error(), "update cyclic failed workflow") {
		t.Fatalf("error = %v, want wrapped cyclic failure update error", err)
	}
}

func TestWorkflowReconciler_CyclicToolStep_CompletionTokenSupportsCommaInStepName(t *testing.T) {
	stepName := "fetch,primary"
	token := gateAnnotationToken(stepName, 1)
	encoded := appendRejectedToken("", token)
	if got := parseCSVTokens(encoded); len(got) != 1 || got[0] != token {
		t.Fatalf("encoded token %q parsed as %v, want %q", encoded, got, token)
	}
	wf := v1alpha1.Workflow{ObjectMeta: v1alpha1.ObjectMeta{Annotations: map[string]string{workflowCompletedToolsKey: encoded}}}
	if !cyclicToolCompleted(&wf, stepName, 1) {
		t.Fatalf("expected completion marker for comma-bearing step name; annotation=%q", encoded)
	}
}

func TestMergeOwnedFinalizerPreservesForeignFinalizers(t *testing.T) {
	t.Run("add owned finalizer", func(t *testing.T) {
		got := mergeOwnedFinalizer([]string{"example.com/foreign"}, []string{WorkflowRunFinalizer}, WorkflowRunFinalizer, false)
		if !hasString(got, "example.com/foreign") || !hasString(got, WorkflowRunFinalizer) {
			t.Fatalf("merged finalizers = %v, want foreign and owned finalizers", got)
		}
	})

	t.Run("remove only owned finalizer during deletion", func(t *testing.T) {
		got := mergeOwnedFinalizer([]string{"example.com/foreign", WorkflowRunFinalizer}, nil, WorkflowRunFinalizer, true)
		if !hasString(got, "example.com/foreign") || hasString(got, WorkflowRunFinalizer) {
			t.Fatalf("merged finalizers = %v, want only foreign finalizer", got)
		}
	})
}

func TestWorkflowRunReconciler_CyclicToolStep_ConflictDoesNotRepeatInvocation(t *testing.T) {
	base := newMemStore()
	s := &conflictOnToolCompletionStore{Store: base}
	invoker := &fakeWorkflowToolInvoker{output: map[string]any{"ok": true}}

	wf := makeToolStepWorkflow("default", "wf-run-cyclic-tool")
	wf.Spec.EntryPoint = "fetch"
	wf.Spec.Edges = []v1alpha1.WorkflowEdge{{From: "fetch", To: "fetch"}}
	wf.Spec.Budgets = &v1alpha1.WorkflowBudget{MaxIterations: 1}
	storeWorkflow(t, base, wf)
	run := makeWorkflowRun("default", "run-cyclic-tool", wf.ObjectMeta.Name)
	storeWorkflowRun(t, s, run)

	r := NewWorkflowRunReconciler(s, zaptest.NewLogger(t))
	r.SetToolInvoker(invoker)
	req := Request{Namespace: "default", Name: run.ObjectMeta.Name, Kind: "WorkflowRun"}
	for attempt := 0; attempt < 6; attempt++ {
		if _, err := r.Reconcile(context.Background(), req); err != nil {
			t.Fatalf("reconcile attempt %d: %v", attempt, err)
		}
		if getWorkflowRun(t, s, "default", run.ObjectMeta.Name).Status.Phase == v1alpha1.PhaseReady {
			break
		}
	}

	stored := getWorkflowRun(t, s, "default", run.ObjectMeta.Name)
	if stored.Status.Phase != v1alpha1.PhaseReady {
		t.Fatalf("workflow run phase = %q, want Ready", stored.Status.Phase)
	}
	if len(invoker.calls) != 1 {
		t.Fatalf("expected one tool invocation across conflict retry, got %d", len(invoker.calls))
	}
	if got := stored.ObjectMeta.Annotations["example.com/concurrent"]; got != "preserved" {
		t.Fatalf("concurrent annotation = %q, want preserved", got)
	}
	if !strings.Contains(stored.ObjectMeta.Annotations[workflowCompletedToolsKey], "fetch@1") {
		t.Fatalf("completion annotation = %q, want fetch@1", stored.ObjectMeta.Annotations[workflowCompletedToolsKey])
	}
}

// TC-0233: invoker error (e.g. permission denied from ToolGrant) marks step Failed and workflow Failed.
func TestWorkflowReconciler_ToolStep_InvokerErrorFailsStep(t *testing.T) {
	s := newMemStore()
	r := NewWorkflowReconciler(s, zaptest.NewLogger(t))
	invoker := &fakeWorkflowToolInvoker{err: errors.New("no ToolGrant permits invoke on tool http-get")}
	r.SetToolInvoker(invoker)

	wf := makeToolStepWorkflow("default", "wf-tool-denied")
	storeWorkflow(t, s, wf)

	req := Request{Namespace: "default", Name: "wf-tool-denied", Kind: "Workflow"}
	mustReconcile(t, r, req)
	mustReconcile(t, r, req)

	stored := getWorkflow(t, s, "default", "wf-tool-denied")
	if stored.Status.Phase != v1alpha1.PhaseFailed {
		t.Fatalf("workflow phase = %q, want Failed", stored.Status.Phase)
	}
	if len(stored.Status.StepStatuses) != 1 {
		t.Fatalf("expected 1 step status, got %d", len(stored.Status.StepStatuses))
	}
	ss := stored.Status.StepStatuses[0]
	if ss.Phase != v1alpha1.PhaseFailed {
		t.Fatalf("step phase = %q, want Failed", ss.Phase)
	}
	if ss.Output != "" {
		t.Fatalf("step output = %q, want empty (error must not leak into Output)", ss.Output)
	}
	evts, err := s.ListEvents(context.Background(), "Workflow", "default", "wf-tool-denied")
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	var foundFail bool
	for _, ev := range evts {
		if ev.Reason == "ToolStepFailed" && strings.Contains(ev.Message, "no ToolGrant permits") {
			foundFail = true
			break
		}
	}
	if !foundFail {
		t.Fatalf("expected ToolStepFailed event with permission denied message, got events: %+v", evts)
	}
}

// TC-0234: tool step output is consumable by downstream agent steps via .Steps.<name>.Output template.
func TestWorkflowReconciler_ToolStep_OutputFlowsToDownstreamAgent(t *testing.T) {
	s := newMemStore()
	r := NewWorkflowReconciler(s, zaptest.NewLogger(t))
	invoker := &fakeWorkflowToolInvoker{output: map[string]any{"value": 42}}
	r.SetToolInvoker(invoker)

	wf := v1alpha1.Workflow{
		TypeMeta:   v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "Workflow"},
		ObjectMeta: v1alpha1.ObjectMeta{Name: "wf-tool-chain", Namespace: "default", UID: "wf-chain-uid"},
		Spec: v1alpha1.WorkflowSpec{
			Steps: []v1alpha1.WorkflowStep{
				{
					Name:    "fetch",
					Type:    v1alpha1.WorkflowStepTypeTool,
					ToolRef: v1alpha1.LocalObjectReference{Name: "lookup"},
				},
				{
					Name:     "summarize",
					AgentRef: v1alpha1.LocalObjectReference{Name: "summarizer"},
					Input:    "value={{ .Steps.fetch.Output }}",
				},
			},
			Edges: []v1alpha1.WorkflowEdge{{From: "fetch", To: "summarize"}},
		},
		Status: v1alpha1.WorkflowStatus{Phase: v1alpha1.PhasePending},
	}
	storeWorkflow(t, s, wf)

	req := Request{Namespace: "default", Name: "wf-tool-chain", Kind: "Workflow"}
	// Reconcile until the downstream AgentJob exists or we give up.
	for i := 0; i < 5; i++ {
		mustReconcile(t, r, req)
		if jobs := listWorkflowJobs(t, s, "default"); len(jobs) >= 1 {
			break
		}
	}

	jobs := listWorkflowJobs(t, s, "default")
	if len(jobs) != 1 {
		t.Fatalf("expected 1 downstream AgentJob, got %d", len(jobs))
	}
	if got := jobs[0].ObjectMeta.Labels["kognition.dev/workflow-step"]; got != "summarize" {
		t.Fatalf("downstream job step = %q, want summarize", got)
	}
	if !strings.Contains(jobs[0].Spec.Input, `"value":42`) && !strings.Contains(jobs[0].Spec.Input, `\"value\":42`) {
		t.Fatalf("downstream job input = %q, want to contain tool output value=42", jobs[0].Spec.Input)
	}
}

// TC-0271: when no WorkflowToolInvoker is wired, a tool-step workflow terminal-fails
// (no infinite requeue) and surfaces a ConditionReady=False with reason ToolInvokerUnavailable.
func TestWorkflowReconciler_ToolStep_NilInvokerFailsWorkflowTerminally(t *testing.T) {
	s := newMemStore()
	r := NewWorkflowReconciler(s, zaptest.NewLogger(t))
	// Intentionally do NOT call r.SetToolInvoker.

	wf := makeToolStepWorkflow("default", "wf-tool-noinv")
	storeWorkflow(t, s, wf)

	req := Request{Namespace: "default", Name: "wf-tool-noinv", Kind: "Workflow"}
	// First reconcile adds the finalizer (Requeue:true). Second reconcile reaches the
	// tool-step branch which must terminal-fail with no requeue.
	if _, err := r.reconcileDirect(context.Background(), req); err != nil {
		t.Fatalf("Reconcile #1 returned error: %v", err)
	}
	res, err := r.reconcileDirect(context.Background(), req)
	if err != nil {
		t.Fatalf("Reconcile #2 returned error: %v", err)
	}
	if res.RequeueAfter != 0 || res.Requeue {
		t.Fatalf("expected terminal Result (no requeue), got %+v", res)
	}

	stored := getWorkflow(t, s, "default", "wf-tool-noinv")
	if stored.Status.Phase != v1alpha1.PhaseFailed {
		t.Fatalf("workflow phase = %q, want Failed", stored.Status.Phase)
	}
	var foundCond bool
	for _, c := range stored.Status.Conditions {
		if c.Type == "Ready" && c.Status == "False" && c.Reason == "ToolInvokerUnavailable" {
			foundCond = true
			break
		}
	}
	if !foundCond {
		t.Fatalf("expected ConditionReady=False reason=ToolInvokerUnavailable, got conditions=%+v", stored.Status.Conditions)
	}
	evts, err := s.ListEvents(context.Background(), "Workflow", "default", "wf-tool-noinv")
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	var foundEvt bool
	for _, ev := range evts {
		if ev.Reason == "ToolStepFailed" {
			foundEvt = true
			break
		}
	}
	if !foundEvt {
		t.Fatalf("expected ToolStepFailed event, got %+v", evts)
	}
}

// TC-0272: ContinueOnError on a failing tool step lets the workflow proceed (not terminal-failed).
func TestWorkflowReconciler_ToolStep_ContinueOnErrorDoesNotFailWorkflow(t *testing.T) {
	s := newMemStore()
	r := NewWorkflowReconciler(s, zaptest.NewLogger(t))
	invoker := &fakeWorkflowToolInvoker{err: errors.New("boom")}
	r.SetToolInvoker(invoker)

	wf := makeToolStepWorkflow("default", "wf-tool-coe")
	wf.Spec.Steps[0].ContinueOnError = true
	storeWorkflow(t, s, wf)

	req := Request{Namespace: "default", Name: "wf-tool-coe", Kind: "Workflow"}
	mustReconcile(t, r, req)
	mustReconcile(t, r, req)

	stored := getWorkflow(t, s, "default", "wf-tool-coe")
	if stored.Status.Phase == v1alpha1.PhaseFailed {
		t.Fatalf("workflow phase = Failed, want non-Failed (ContinueOnError honored); status=%+v", stored.Status)
	}
	if len(stored.Status.StepStatuses) != 1 || stored.Status.StepStatuses[0].Phase != v1alpha1.PhaseFailed {
		t.Fatalf("step status = %+v, want single Failed step", stored.Status.StepStatuses)
	}
}

// TC-0273: success path emits a ToolStepCompleted event with the step name as an attribute.
func TestWorkflowReconciler_ToolStep_EmitsCompletedEventOnSuccess(t *testing.T) {
	s := newMemStore()
	r := NewWorkflowReconciler(s, zaptest.NewLogger(t))
	invoker := &fakeWorkflowToolInvoker{output: map[string]any{"ok": true}}
	r.SetToolInvoker(invoker)

	wf := makeToolStepWorkflow("default", "wf-tool-evt")
	storeWorkflow(t, s, wf)

	req := Request{Namespace: "default", Name: "wf-tool-evt", Kind: "Workflow"}
	mustReconcile(t, r, req)
	mustReconcile(t, r, req)

	evts, err := s.ListEvents(context.Background(), "Workflow", "default", "wf-tool-evt")
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	var found bool
	for _, ev := range evts {
		if ev.Reason == "ToolStepCompleted" && strings.Contains(string(ev.Details), `"fetch"`) {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected ToolStepCompleted event with step=fetch in details, got %+v", evts)
	}
}

// Tool-step per-step retry recovers from a transient failure within MaxAttempts.
func TestWorkflowReconciler_ToolStepRetry_RecoversWithinMaxAttempts(t *testing.T) {
	s := newMemStore()
	r := NewWorkflowReconciler(s, zaptest.NewLogger(t))
	invoker := &fakeWorkflowToolInvoker{failN: 1, err: errors.New("transient"), output: map[string]any{"ok": true}}
	r.SetToolInvoker(invoker)

	wf := makeToolStepWorkflow("default", "wf-tool-retry")
	wf.Spec.Steps[0].Retry = &v1alpha1.WorkflowStepRetry{MaxAttempts: 3, Backoff: "1ms"}
	storeWorkflow(t, s, wf)

	req := Request{Namespace: "default", Name: "wf-tool-retry", Kind: "Workflow"}
	mustReconcile(t, r, req) // finalizer
	mustReconcile(t, r, req) // attempt 1 -> fail -> schedule retry

	stored := getWorkflow(t, s, "default", "wf-tool-retry")
	if stored.Status.Phase == v1alpha1.PhaseFailed {
		t.Fatalf("workflow should not be Failed during retry; status=%+v", stored.Status)
	}
	ss := findStepStatus(&stored, "fetch")
	if ss == nil || ss.NextRetryAt == nil {
		t.Fatalf("expected retry scheduled (NextRetryAt set), got %+v", stored.Status.StepStatuses)
	}

	time.Sleep(5 * time.Millisecond)
	mustReconcile(t, r, req) // attempt 2 -> succeed

	stored = getWorkflow(t, s, "default", "wf-tool-retry")
	if stored.Status.Phase != v1alpha1.PhaseReady {
		t.Fatalf("phase = %q, want Ready after retry success", stored.Status.Phase)
	}
	if len(invoker.calls) != 2 {
		t.Fatalf("expected 2 invocations (1 fail + 1 success), got %d", len(invoker.calls))
	}
	ss = findStepStatus(&stored, "fetch")
	if ss == nil || ss.Phase != v1alpha1.PhaseReady || ss.Attempts != 2 {
		t.Fatalf("step status = %+v, want Ready with 2 attempts", ss)
	}
}

// Tool-step retry that never succeeds fails the workflow after MaxAttempts.
func TestWorkflowReconciler_ToolStepRetry_ExhaustsAndFails(t *testing.T) {
	s := newMemStore()
	r := NewWorkflowReconciler(s, zaptest.NewLogger(t))
	invoker := &fakeWorkflowToolInvoker{err: errors.New("always")}
	r.SetToolInvoker(invoker)

	wf := makeToolStepWorkflow("default", "wf-tool-retry-exhaust")
	wf.Spec.Steps[0].Retry = &v1alpha1.WorkflowStepRetry{MaxAttempts: 2, Backoff: "1ms"}
	storeWorkflow(t, s, wf)

	req := Request{Namespace: "default", Name: "wf-tool-retry-exhaust", Kind: "Workflow"}
	mustReconcile(t, r, req) // finalizer
	mustReconcile(t, r, req) // attempt 1 -> fail -> schedule retry
	time.Sleep(5 * time.Millisecond)
	mustReconcile(t, r, req) // attempt 2 -> fail -> exhausted

	stored := getWorkflow(t, s, "default", "wf-tool-retry-exhaust")
	if stored.Status.Phase != v1alpha1.PhaseFailed {
		t.Fatalf("phase = %q, want Failed after retries exhausted", stored.Status.Phase)
	}
	if len(invoker.calls) != 2 {
		t.Fatalf("expected 2 invocations (MaxAttempts=2), got %d", len(invoker.calls))
	}
}

// Tool-step per-step timeout cancels a slow invocation and fails the step.
func TestWorkflowReconciler_ToolStepTimeout_FailsAfterDeadline(t *testing.T) {
	s := newMemStore()
	r := NewWorkflowReconciler(s, zaptest.NewLogger(t))
	invoker := &fakeWorkflowToolInvoker{block: true}
	r.SetToolInvoker(invoker)

	wf := makeToolStepWorkflow("default", "wf-tool-timeout")
	wf.Spec.Steps[0].Timeout = "5ms"
	storeWorkflow(t, s, wf)

	req := Request{Namespace: "default", Name: "wf-tool-timeout", Kind: "Workflow"}
	mustReconcile(t, r, req) // finalizer
	mustReconcile(t, r, req) // invoke blocks until the 5ms deadline -> fail

	stored := getWorkflow(t, s, "default", "wf-tool-timeout")
	if stored.Status.Phase != v1alpha1.PhaseFailed {
		t.Fatalf("phase = %q, want Failed after timeout", stored.Status.Phase)
	}
	ss := findStepStatus(&stored, "fetch")
	if ss == nil || ss.Phase != v1alpha1.PhaseFailed {
		t.Fatalf("step status = %+v, want Failed", ss)
	}
	if len(invoker.calls) != 1 {
		t.Fatalf("expected 1 invocation, got %d", len(invoker.calls))
	}
}
