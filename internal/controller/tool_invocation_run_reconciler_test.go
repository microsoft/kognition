// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap/zaptest"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/store"
)

type toolInvocationRunGetFaultStore struct {
	*memStore
	kind string
	err  error
}

func (s *toolInvocationRunGetFaultStore) Get(ctx context.Context, kind, namespace, name string) ([]byte, error) {
	if kind == s.kind {
		return nil, s.err
	}
	return s.memStore.Get(ctx, kind, namespace, name)
}

type stdioInvocationRunnerFunc func(context.Context, *v1alpha1.Tool, StdioInvocationTarget, map[string]any) (StdioInvocationResult, error)

func (fn stdioInvocationRunnerFunc) RunStdioInvocation(ctx context.Context, tool *v1alpha1.Tool, target StdioInvocationTarget, input map[string]any) (StdioInvocationResult, error) {
	return fn(ctx, tool, target, input)
}

func TestToolInvocationRunReconciler_WrappedRunNotFoundIsComplete(t *testing.T) {
	s := &toolInvocationRunGetFaultStore{
		memStore: newMemStore(),
		kind:     toolInvocationRunKind,
		err:      fmt.Errorf("wrapped missing run: %w", store.ErrNotFound),
	}
	reconciler := NewToolInvocationRunReconciler(s, zaptest.NewLogger(t))
	result, err := reconciler.Reconcile(context.Background(), Request{Kind: toolInvocationRunKind, Namespace: "team-a", Name: "missing"})
	if err != nil || result != (Result{}) {
		t.Fatalf("result = %+v, error = %v, want completed not-found reconciliation", result, err)
	}
}

func TestToolInvocationRunReconciler_WrappedTargetNotFound(t *testing.T) {
	for _, test := range []struct {
		name      string
		kind      string
		toolName  string
		className string
		want      string
	}{
		{name: "Tool", kind: toolKind, toolName: "missing-tool", want: `Tool "missing-tool" was not found`},
		{name: "ToolClass", kind: toolClassKind, toolName: "shell", className: "missing-class", want: `ToolClass "missing-class" was not found`},
	} {
		t.Run(test.name, func(t *testing.T) {
			baseStore := newMemStore()
			if test.className != "" {
				storeTool(t, baseStore, v1alpha1.Tool{
					ObjectMeta: v1alpha1.ObjectMeta{Name: test.toolName, Namespace: "team-a"},
					Spec:       v1alpha1.ToolSpec{Protocol: v1alpha1.ToolProtocolShell, ClassName: test.className},
					Status:     v1alpha1.ToolStatus{Phase: v1alpha1.PhaseReady},
				})
			}
			s := &toolInvocationRunGetFaultStore{
				memStore: baseStore,
				kind:     test.kind,
				err:      fmt.Errorf("wrapped missing target: %w", store.ErrNotFound),
			}
			reconciler := NewToolInvocationRunReconciler(s, zaptest.NewLogger(t))
			_, _, err := reconciler.resolveTarget(context.Background(), &v1alpha1.ToolInvocationRun{
				ObjectMeta: v1alpha1.ObjectMeta{Namespace: "team-a"},
				Spec: v1alpha1.ToolInvocationRunSpec{
					ToolName:            test.toolName,
					ToolResourceVersion: 1,
					InvocationID:        "invoke-1",
				},
			})
			if err == nil || err.Error() != test.want {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestToolInvocationRunReconciler_CompletesClassOwnedStdioInvocation(t *testing.T) {
	store := newMemStore()
	tool := v1alpha1.Tool{ObjectMeta: v1alpha1.ObjectMeta{Name: "shell", Namespace: "team-a"}, Spec: v1alpha1.ToolSpec{Protocol: v1alpha1.ToolProtocolShell, ClassName: "shell-class"}, Status: v1alpha1.ToolStatus{Phase: v1alpha1.PhaseReady}}
	storeTool(t, store, tool)
	tool = getTool(t, store, "team-a", "shell")
	storeToolClass(t, store, v1alpha1.ToolClass{ObjectMeta: v1alpha1.ObjectMeta{Name: "shell-class"}, Spec: v1alpha1.ToolClassSpec{ExecutionTier: v1alpha1.ExecutionTierPod, SandboxImage: "registry.example/shell:1", StdioCommand: []string{"/srv/mcp", "--stdio"}, SandboxDefaults: &v1alpha1.ToolSandboxSpec{Timeout: "4s", CPUMilli: 100, MemoryMB: 64}}, Status: v1alpha1.ToolClassStatus{Phase: v1alpha1.PhaseReady}})
	run := v1alpha1.ToolInvocationRun{ObjectMeta: v1alpha1.ObjectMeta{Name: "run", Namespace: "team-a"}, Spec: v1alpha1.ToolInvocationRunSpec{ToolName: "shell", ToolResourceVersion: tool.ObjectMeta.ResourceVersion, InvocationID: "invoke-1", Input: map[string]any{"command": "status"}}}
	data, _ := json.Marshal(run)
	if err := store.Create(context.Background(), toolInvocationRunKind, "team-a", "run", data); err != nil {
		t.Fatal(err)
	}
	calls := 0
	reconciler := NewToolInvocationRunReconciler(store, zaptest.NewLogger(t), WithStdioInvocationRunner(stdioInvocationRunnerFunc(func(_ context.Context, gotTool *v1alpha1.Tool, target StdioInvocationTarget, input map[string]any) (StdioInvocationResult, error) {
		calls++
		if gotTool.Name != "shell" || target.Image != "registry.example/shell:1" || len(target.Command) != 2 || target.Timeout != 4*time.Second || target.CPUMilli != 100 || target.MemoryMB != 64 || input["command"] != "status" {
			t.Fatalf("unexpected target: tool=%#v target=%#v input=%#v", gotTool, target, input)
		}
		return StdioInvocationResult{Output: map[string]any{"status": "ok"}, ExecutionMode: "sandbox"}, nil
	})))
	result, err := reconciler.Reconcile(context.Background(), Request{Kind: toolInvocationRunKind, Namespace: "team-a", Name: "run"})
	if err != nil || !result.Requeue || calls != 0 {
		t.Fatalf("first reconcile result=%#v err=%v calls=%d", result, err, calls)
	}
	result, err = reconciler.Reconcile(context.Background(), Request{Kind: toolInvocationRunKind, Namespace: "team-a", Name: "run"})
	if err != nil || result.Requeue || calls != 1 {
		t.Fatalf("second reconcile result=%#v err=%v calls=%d", result, err, calls)
	}
	stored := getToolInvocationRun(t, store, "team-a", "run")
	if stored.Status.Phase != v1alpha1.PhaseReady || stored.Status.Output["status"] != "ok" || stored.Status.ExecutionMode != "sandbox" {
		t.Fatalf("run status = %#v", stored.Status)
	}
}

func TestToolInvocationRunReconciler_FailsClosedForChangedToolOrUnavailableRunner(t *testing.T) {
	for _, test := range []struct {
		name          string
		versionOffset int64
		runner        StdioInvocationRunner
		wantCode      string
	}{
		{name: "changed Tool", versionOffset: 1, runner: stdioInvocationRunnerFunc(func(context.Context, *v1alpha1.Tool, StdioInvocationTarget, map[string]any) (StdioInvocationResult, error) {
			return StdioInvocationResult{}, nil
		}), wantCode: "ToolInvocationTargetUnavailable"},
		{name: "missing runner", runner: nil, wantCode: "StdioSandboxUnavailable"},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := newMemStore()
			tool := v1alpha1.Tool{ObjectMeta: v1alpha1.ObjectMeta{Name: "shell", Namespace: "team-a"}, Spec: v1alpha1.ToolSpec{Protocol: v1alpha1.ToolProtocolShell, ClassName: "class"}, Status: v1alpha1.ToolStatus{Phase: v1alpha1.PhaseReady}}
			storeTool(t, store, tool)
			tool = getTool(t, store, "team-a", "shell")
			storeToolClass(t, store, v1alpha1.ToolClass{ObjectMeta: v1alpha1.ObjectMeta{Name: "class"}, Spec: v1alpha1.ToolClassSpec{ExecutionTier: v1alpha1.ExecutionTierPod, SandboxImage: "registry.example/shell:1", StdioCommand: []string{"/srv/mcp"}}, Status: v1alpha1.ToolClassStatus{Phase: v1alpha1.PhaseReady}})
			run := v1alpha1.ToolInvocationRun{ObjectMeta: v1alpha1.ObjectMeta{Name: "run", Namespace: "team-a"}, Spec: v1alpha1.ToolInvocationRunSpec{ToolName: "shell", ToolResourceVersion: tool.ObjectMeta.ResourceVersion + test.versionOffset, InvocationID: "invoke"}}
			data, _ := json.Marshal(run)
			if err := store.Create(context.Background(), toolInvocationRunKind, "team-a", "run", data); err != nil {
				t.Fatal(err)
			}
			reconciler := NewToolInvocationRunReconciler(store, zaptest.NewLogger(t), WithStdioInvocationRunner(test.runner))
			if _, err := reconciler.Reconcile(context.Background(), Request{Kind: toolInvocationRunKind, Namespace: "team-a", Name: "run"}); err != nil {
				t.Fatal(err)
			}
			if _, err := reconciler.Reconcile(context.Background(), Request{Kind: toolInvocationRunKind, Namespace: "team-a", Name: "run"}); err != nil {
				t.Fatal(err)
			}
			stored := getToolInvocationRun(t, store, "team-a", "run")
			if stored.Status.Phase != v1alpha1.PhaseFailed || stored.Status.ErrorCode != test.wantCode {
				t.Fatalf("run status = %#v", stored.Status)
			}
		})
	}
}

func TestToolInvocationRunReconciler_RecordsRunnerFailure(t *testing.T) {
	store := newMemStore()
	tool := v1alpha1.Tool{ObjectMeta: v1alpha1.ObjectMeta{Name: "shell", Namespace: "team-a"}, Spec: v1alpha1.ToolSpec{Protocol: v1alpha1.ToolProtocolShell, ClassName: "class"}, Status: v1alpha1.ToolStatus{Phase: v1alpha1.PhaseReady}}
	storeTool(t, store, tool)
	tool = getTool(t, store, "team-a", "shell")
	storeToolClass(t, store, v1alpha1.ToolClass{ObjectMeta: v1alpha1.ObjectMeta{Name: "class"}, Spec: v1alpha1.ToolClassSpec{ExecutionTier: v1alpha1.ExecutionTierPod, SandboxImage: "registry.example/shell:1", StdioCommand: []string{"/srv/mcp"}}, Status: v1alpha1.ToolClassStatus{Phase: v1alpha1.PhaseReady}})
	run := v1alpha1.ToolInvocationRun{ObjectMeta: v1alpha1.ObjectMeta{Name: "run", Namespace: "team-a"}, Spec: v1alpha1.ToolInvocationRunSpec{ToolName: "shell", ToolResourceVersion: tool.ObjectMeta.ResourceVersion, InvocationID: "invoke"}}
	data, _ := json.Marshal(run)
	if err := store.Create(context.Background(), toolInvocationRunKind, "team-a", "run", data); err != nil {
		t.Fatal(err)
	}
	reconciler := NewToolInvocationRunReconciler(store, zaptest.NewLogger(t), WithStdioInvocationRunner(stdioInvocationRunnerFunc(func(context.Context, *v1alpha1.Tool, StdioInvocationTarget, map[string]any) (StdioInvocationResult, error) {
		return StdioInvocationResult{}, errors.New("server exited")
	})))
	_, _ = reconciler.Reconcile(context.Background(), Request{Kind: toolInvocationRunKind, Namespace: "team-a", Name: "run"})
	_, _ = reconciler.Reconcile(context.Background(), Request{Kind: toolInvocationRunKind, Namespace: "team-a", Name: "run"})
	stored := getToolInvocationRun(t, store, "team-a", "run")
	if stored.Status.Phase != v1alpha1.PhaseFailed || stored.Status.ErrorCode != "StdioInvocationFailed" || stored.Status.ErrorMessage != "server exited" {
		t.Fatalf("run status = %#v", stored.Status)
	}
}

func getToolInvocationRun(t *testing.T, store *memStore, namespace, name string) v1alpha1.ToolInvocationRun {
	t.Helper()
	raw, err := store.Get(context.Background(), toolInvocationRunKind, namespace, name)
	if err != nil {
		t.Fatal(err)
	}
	var run v1alpha1.ToolInvocationRun
	if err := json.Unmarshal(raw, &run); err != nil {
		t.Fatal(err)
	}
	return run
}

func TestToolInvocationRunReconcilerDockerFunctional(t *testing.T) {
	if os.Getenv("KOGNITION_DOCKER_STDIO_INTEGRATION") != "1" {
		t.Skip("set KOGNITION_DOCKER_STDIO_INTEGRATION=1 to run against Docker Engine")
	}
	runner, err := NewDockerStdioInvocationRunner()
	if err != nil {
		t.Fatalf("NewDockerStdioInvocationRunner: %v", err)
	}
	store := newMemStore()
	server := `import json, os, sys
assert os.geteuid() == 65532
for line in sys.stdin:
 request=json.loads(line)
 if request.get("method") == "initialize": print(json.dumps({"jsonrpc":"2.0","id":request["id"],"result":{"protocolVersion":"2025-03-26"}}), flush=True)
 elif request.get("method") == "tools/call": print(json.dumps({"jsonrpc":"2.0","id":request["id"],"result":{"content":[],"status":"ok"}}), flush=True); break`
	tool := v1alpha1.Tool{ObjectMeta: v1alpha1.ObjectMeta{Name: "shell", Namespace: "team-a"}, Spec: v1alpha1.ToolSpec{Protocol: v1alpha1.ToolProtocolShell, ClassName: "shell-class"}, Status: v1alpha1.ToolStatus{Phase: v1alpha1.PhaseReady}}
	storeTool(t, store, tool)
	tool = getTool(t, store, "team-a", "shell")
	storeToolClass(t, store, v1alpha1.ToolClass{ObjectMeta: v1alpha1.ObjectMeta{Name: "shell-class"}, Spec: v1alpha1.ToolClassSpec{ExecutionTier: v1alpha1.ExecutionTierPod, SandboxImage: "python:3.12-alpine", StdioCommand: []string{"/usr/local/bin/python", "-u", "-c", server}, SandboxDefaults: &v1alpha1.ToolSandboxSpec{Timeout: "10s", CPUMilli: 100, MemoryMB: 64}}, Status: v1alpha1.ToolClassStatus{Phase: v1alpha1.PhaseReady}})
	run := v1alpha1.ToolInvocationRun{ObjectMeta: v1alpha1.ObjectMeta{Name: "run", Namespace: "team-a"}, Spec: v1alpha1.ToolInvocationRunSpec{ToolName: "shell", ToolResourceVersion: tool.ObjectMeta.ResourceVersion, InvocationID: "invoke", Input: map[string]any{"command": "status"}}}
	data, _ := json.Marshal(run)
	if err := store.Create(context.Background(), toolInvocationRunKind, "team-a", "run", data); err != nil {
		t.Fatal(err)
	}
	reconciler := NewToolInvocationRunReconciler(store, zaptest.NewLogger(t), WithStdioInvocationRunner(runner))
	if _, err := reconciler.Reconcile(context.Background(), Request{Kind: toolInvocationRunKind, Namespace: "team-a", Name: "run"}); err != nil {
		t.Fatal(err)
	}
	if _, err := reconciler.Reconcile(context.Background(), Request{Kind: toolInvocationRunKind, Namespace: "team-a", Name: "run"}); err != nil {
		t.Fatal(err)
	}
	stored := getToolInvocationRun(t, store, "team-a", "run")
	if stored.Status.Phase != v1alpha1.PhaseReady || stored.Status.Output["status"] != "ok" || stored.Status.ExecutionMode != "sandbox" {
		t.Fatalf("run status = %#v", stored.Status)
	}
}

func TestResolveStdioInvocationTimeout(t *testing.T) {
	got, err := resolveStdioInvocationTimeout(&v1alpha1.ToolSandboxSpec{Timeout: "10s"}, nil, &v1alpha1.ToolClassResourceLimits{MaxTimeout: "15s"})
	if err != nil || got != 10*time.Second {
		t.Fatalf("class default timeout = %s err=%v", got, err)
	}
	got, err = resolveStdioInvocationTimeout(&v1alpha1.ToolSandboxSpec{Timeout: "10s"}, &v1alpha1.ToolSandboxSpec{Timeout: "3s"}, &v1alpha1.ToolClassResourceLimits{MaxTimeout: "15s"})
	if err != nil || got != 3*time.Second {
		t.Fatalf("Tool override timeout = %s err=%v", got, err)
	}
	_, err = resolveStdioInvocationTimeout(&v1alpha1.ToolSandboxSpec{Timeout: "10s"}, nil, &v1alpha1.ToolClassResourceLimits{MaxTimeout: "5s"})
	if err == nil || !strings.Contains(err.Error(), "exceeds ToolClass limit") {
		t.Fatalf("maximum timeout error = %v", err)
	}
}
