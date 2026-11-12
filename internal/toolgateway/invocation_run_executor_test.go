// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package toolgateway

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"go.uber.org/zap/zaptest"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/runtime"
	"github.com/kognition-ai/kognition/internal/store"
)

func TestInvocationRunExecutorCreatesPinnedShellRunAndReplays(t *testing.T) {
	created := 0
	st := &gatewayStore{data: map[string][]byte{}}
	st.onCreate = func(kind, namespace, name string, data []byte) error {
		if kind != toolInvocationRunKind {
			t.Fatalf("created kind = %q", kind)
		}
		created++
		var run v1alpha1.ToolInvocationRun
		if err := json.Unmarshal(data, &run); err != nil {
			return err
		}
		if run.Spec.InvocationID != "invoke-1" || run.Spec.ToolName != "shell" || run.Spec.ToolResourceVersion != 7 || run.Spec.Input["command"] != "status" {
			t.Fatalf("created run = %#v", run)
		}
		run.Status = v1alpha1.ToolInvocationRunStatus{Phase: v1alpha1.PhaseReady, Output: map[string]any{"status": "ok"}, ExecutionMode: "sandbox", DurationMillis: 12}
		updated, _ := json.Marshal(run)
		st.data[kind+"/"+namespace+"/"+name] = updated
		return nil
	}
	executor := NewInvocationRunExecutor(st, &gatewayExecutor{output: map[string]any{"fallback": true}})
	request := runtime.ToolExecutionRequest{InvocationID: "invoke-1", Tool: v1alpha1.Tool{ObjectMeta: v1alpha1.ObjectMeta{Name: "shell", Namespace: "team-a", ResourceVersion: 7}, Spec: v1alpha1.ToolSpec{Protocol: v1alpha1.ToolProtocolShell}}, Input: map[string]any{"command": "status"}}
	result, err := executor.Execute(context.Background(), request)
	if err != nil || result.Output["status"] != "ok" || result.ExecutionMode != "sandbox" || created != 1 {
		t.Fatalf("first result=%#v err=%v created=%d", result, err, created)
	}
	result, err = executor.Execute(context.Background(), request)
	if err != nil || result.Output["status"] != "ok" || created != 1 {
		t.Fatalf("replay result=%#v err=%v created=%d", result, err, created)
	}
}

func TestGatewayShellInvocationPreservesAuthorizationAndDurableResult(t *testing.T) {
	st := &gatewayStore{data: map[string][]byte{}}
	tool := v1alpha1.Tool{ObjectMeta: v1alpha1.ObjectMeta{Name: "shell", Namespace: "team-a", ResourceVersion: 7}, Spec: v1alpha1.ToolSpec{Protocol: v1alpha1.ToolProtocolShell}}
	storeGatewayObject(t, st, "Tool", "team-a", "shell", tool)
	st.onCreate = func(kind, namespace, name string, data []byte) error {
		var run v1alpha1.ToolInvocationRun
		if err := json.Unmarshal(data, &run); err != nil {
			return err
		}
		run.Status = v1alpha1.ToolInvocationRunStatus{Phase: v1alpha1.PhaseReady, Output: map[string]any{"status": "ok"}, ExecutionMode: "sandbox"}
		updated, _ := json.Marshal(run)
		st.data[kind+"/"+namespace+"/"+name] = updated
		return nil
	}
	ledger := &gatewayLedger{}
	gateway, err := New(st, &gatewayAuthorizer{}, &gatewayQuota{}, NewInvocationRunExecutor(st, &gatewayExecutor{}), zaptest.NewLogger(t), WithLedger(ledger))
	if err != nil {
		t.Fatal(err)
	}
	response, err := gateway.Invoke(context.Background(), Request{Namespace: "team-a", Principal: "planner", SubjectKind: "AgentInstance", SubjectName: "planner-r1", ToolName: "shell", ToolResourceVersion: 7, InvocationID: "shell-gateway", Input: map[string]any{"command": "status"}})
	if err != nil || response.Output["status"] != "ok" || response.ExecutionMode != "sandbox" || len(ledger.starts) != 1 || len(ledger.completions) != 1 {
		t.Fatalf("response=%#v err=%v ledger=%#v/%#v", response, err, ledger.starts, ledger.completions)
	}
}

func TestInvocationRunExecutorDelegatesNonShellTools(t *testing.T) {
	fallback := &gatewayExecutor{output: map[string]any{"updated": true}}
	result, err := NewInvocationRunExecutor(&gatewayStore{data: map[string][]byte{}}, fallback).Execute(context.Background(), runtime.ToolExecutionRequest{Tool: v1alpha1.Tool{Spec: v1alpha1.ToolSpec{Protocol: v1alpha1.ToolProtocolMCP}}})
	if err != nil || result.Output["updated"] != true || fallback.calls != 1 {
		t.Fatalf("result=%#v err=%v fallback=%d", result, err, fallback.calls)
	}
}

func TestInvocationRunExecutorRejectsUnexpectedRunPhase(t *testing.T) {
	st := &gatewayStore{data: map[string][]byte{}}
	st.onCreate = func(kind, namespace, name string, data []byte) error {
		var run v1alpha1.ToolInvocationRun
		if err := json.Unmarshal(data, &run); err != nil {
			return err
		}
		run.Status.Phase = v1alpha1.Phase("Unexpected")
		updated, _ := json.Marshal(run)
		st.data[kind+"/"+namespace+"/"+name] = updated
		return nil
	}
	executor := NewInvocationRunExecutor(st, nil)
	request := runtime.ToolExecutionRequest{InvocationID: "unexpected-phase", Tool: v1alpha1.Tool{ObjectMeta: v1alpha1.ObjectMeta{Name: "shell", Namespace: "team-a", ResourceVersion: 7}, Spec: v1alpha1.ToolSpec{Protocol: v1alpha1.ToolProtocolShell}}}
	_, err := executor.Execute(context.Background(), request)
	if err == nil || !strings.Contains(err.Error(), "unexpected phase") {
		t.Fatalf("err = %v, want unexpected phase", err)
	}
}

var _ store.Store
