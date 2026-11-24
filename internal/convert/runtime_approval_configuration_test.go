// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package convert

import (
	"reflect"
	"strings"
	"testing"
	"time"

	api "github.com/kognition-ai/kognition/api/v1alpha1"
)

func TestRuntimeApprovalConfigurationDigestRoundTrip(t *testing.T) {
	approval := &api.RuntimeApproval{
		Spec: api.RuntimeApprovalSpec{
			AgentInstanceRef: api.RuntimeApprovalReference{
				Kind: "AgentInstance", Scope: "Namespaced", Namespace: "team-a", Name: "instance",
				UID: "instance-uid", ResourceVersion: 17, ConfigurationDigest: strings.Repeat("a", 64),
			},
		},
	}
	converted := RuntimeApprovalToK8s(approval)
	if converted.Spec.AgentInstanceRef.ConfigurationDigest != approval.Spec.AgentInstanceRef.ConfigurationDigest {
		t.Fatal("Kubernetes conversion lost the configuration digest")
	}
	restored := RuntimeApprovalFromK8s(converted)
	if !reflect.DeepEqual(restored.Spec.AgentInstanceRef, approval.Spec.AgentInstanceRef) {
		t.Fatal("RuntimeApproval reference did not round-trip")
	}
}

func TestRuntimeApprovalWorkflowContextRoundTripAndIsolation(t *testing.T) {
	context := &api.WorkflowActionContext{Workspace: "origin", RunUID: "run-uid", Step: "team", ActivationID: "visit-1",
		Attempt: 2, OwnerEpoch: 3, BindingDigest: strings.Repeat("a", 64)}
	approval := &api.RuntimeApproval{Spec: api.RuntimeApprovalSpec{WorkflowContext: context}}
	converted := RuntimeApprovalToK8s(approval)
	restored := RuntimeApprovalFromK8s(converted)
	if !reflect.DeepEqual(restored.Spec.WorkflowContext, context) {
		t.Fatal("Workflow activation binding did not round-trip")
	}
	copy := converted.DeepCopy()
	copy.Spec.WorkflowContext.OwnerEpoch++
	if converted.Spec.WorkflowContext.OwnerEpoch != 3 {
		t.Fatal("deep copy aliases the original Workflow context")
	}
	restored.Spec.WorkflowContext.OwnerEpoch++
	if context.OwnerEpoch != 3 || converted.Spec.WorkflowContext.OwnerEpoch != 3 {
		t.Fatal("converted Workflow context aliases the canonical input")
	}
}

func TestRuntimeApprovalExecutorActorRoundTripAndIsolation(t *testing.T) {
	context := api.WorkflowActionContext{Workspace: "origin", RunUID: "run-uid", Step: "team", ActivationID: "visit-1",
		Attempt: 1, OwnerEpoch: 1, BindingDigest: strings.Repeat("a", 64)}
	approval := &api.RuntimeApproval{Spec: api.RuntimeApprovalSpec{ActorKind: api.RuntimeApprovalActorExecutor,
		WorkflowContext: &context, ExecutorAction: &api.WorkflowExecutorActionReference{
			Executor:               api.ControlPlaneRoleSubject{Method: "mtls", Issuer: "executor-ca", Subject: "executor"},
			Initiator:              api.ControlPlaneRoleSubject{Method: "oidc", Issuer: "issuer", Subject: "initiator"},
			WorkflowRevisionDigest: strings.Repeat("a", 64), Context: context, ExpiresAt: time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC),
		}}}
	converted := RuntimeApprovalToK8s(approval)
	restored := RuntimeApprovalFromK8s(converted)
	if restored.Spec.ActorKind != api.RuntimeApprovalActorExecutor || !reflect.DeepEqual(restored.Spec.ExecutorAction, approval.Spec.ExecutorAction) {
		t.Fatal("executor action binding did not round-trip")
	}
	copy := converted.DeepCopy()
	copy.Spec.ExecutorAction.Executor.Subject = "replacement"
	restored.Spec.ExecutorAction.Initiator.Subject = "replacement"
	if converted.Spec.ExecutorAction.Executor.Subject != "executor" || approval.Spec.ExecutorAction.Initiator.Subject != "initiator" {
		t.Fatal("executor action conversion or deep copy aliases source identity")
	}
}
