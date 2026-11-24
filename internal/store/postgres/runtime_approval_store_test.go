// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package postgres

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"go.uber.org/zap"

	api "github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/apiserver"
	"github.com/kognition-ai/kognition/internal/auth"
	"github.com/kognition-ai/kognition/internal/controlplaneauth"
	"github.com/kognition-ai/kognition/internal/runtimeapproval"
	"github.com/kognition-ai/kognition/internal/runtimeapproval/conformancetest"
	"github.com/kognition-ai/kognition/internal/store"
)

func TestExecutorTargetReadsCurrentPostgresRevision(t *testing.T) {
	storage := newTestStore(t)
	ctx := context.Background()
	proposal, payload := conformancetest.ExecutorApprovalProposal(t, runtimeApprovalTestKeys(t), "target-reader")
	action, err := runtimeapproval.WorkflowExecutorActionFromApproval(proposal, payload)
	if err != nil {
		t.Fatal(err)
	}
	revision := int64(1)
	agent := api.Agent{ObjectMeta: api.ObjectMeta{Name: action.Target.Name, Namespace: action.Target.Namespace},
		Spec: api.AgentSpec{Runtime: &api.AgentRuntimeSpec{Class: "native"}}, Status: api.AgentStatus{Phase: api.PhaseReady, RevisionCount: 1,
			Runtime: &api.AgentRuntimeStatus{Ready: "True", ReadyReplicas: 1, AvailableReplicas: 1}}}
	service := api.AgentService{ObjectMeta: api.ObjectMeta{Name: action.Service.Name, Namespace: action.Service.Namespace},
		Spec:   api.AgentServiceSpec{AgentRef: api.LocalObjectReference{Name: action.Target.Name}, RevisionPin: &revision},
		Status: api.AgentServiceStatus{Phase: api.PhaseReady, ReadyInstances: 1, ActiveRevision: 1}}
	for kind, resource := range map[string]any{"Agent": agent, "AgentService": service} {
		data, err := json.Marshal(resource)
		if err != nil {
			t.Fatal(err)
		}
		created, err := storage.CreateAndGet(ctx, kind, "team-a", "analyst", data)
		if err != nil {
			t.Fatal(err)
		}
		uid, version := resourceIdentity(t, created)
		if kind == "Agent" {
			action.Target.UID, action.Target.ResourceVersion = uid, version
		} else {
			action.Service.UID, action.Service.ResourceVersion = uid, version
		}
	}
	allow := func(context.Context, runtimeapproval.WorkflowExecutorAction) error { return nil }
	if err := runtimeapproval.AuthorizeWorkflowExecutorTarget(ctx, storage, action, allow); err != nil {
		t.Fatalf("current exact target rejected: %v", err)
	}
	executorContext := auth.WithIdentity(ctx, &auth.Identity{Method: action.Executor.Method, Issuer: action.Executor.Issuer, Subject: action.Executor.Subject})
	initiatorContext := auth.WithIdentity(ctx, &auth.Identity{Method: action.Initiator.Method, Issuer: action.Initiator.Issuer, Subject: action.Initiator.Subject})
	role := api.ControlPlaneRole{ObjectMeta: api.ObjectMeta{Name: "executor", Namespace: "control"},
		Spec: api.ControlPlaneRoleSpec{ExecutionRules: []api.ControlPlaneExecutionRule{{Effect: "allow", Operation: api.ControlPlaneAgentInvoke,
			Workspace: action.Target.Namespace, ResourceName: action.Target.Name, ResourceUID: action.Target.UID}}}}
	binding := api.ControlPlaneRoleBinding{ObjectMeta: api.ObjectMeta{Name: "executor", Namespace: "control"},
		Spec: api.ControlPlaneRoleBindingSpec{RoleRef: api.LocalObjectReference{Name: "executor"}, Subjects: []api.ControlPlaneRoleSubject{action.Executor, action.Initiator}}}
	for kind, resource := range map[string]any{"ControlPlaneRole": role, "ControlPlaneRoleBinding": binding} {
		data, err := json.Marshal(resource)
		if err != nil {
			t.Fatal(err)
		}
		if err := storage.Create(ctx, kind, "control", "executor", data); err != nil {
			t.Fatal(err)
		}
	}
	if err := storage.AuthorizeWorkflowExecutorTarget(executorContext, action); err != nil {
		t.Fatalf("authorized executor target rejected: %v", err)
	}
	foreign := action
	foreign.Target.Namespace, foreign.Service.Namespace = "team-b", "team-b"
	if err := storage.AuthorizeWorkflowExecutorTarget(executorContext, foreign); !errors.Is(err, controlplaneauth.ErrForbidden) {
		t.Fatalf("foreign target proceeded without a reference grant: %v", err)
	}
	if err := storage.AuthorizeWorkflowExecutorTarget(initiatorContext, action); !errors.Is(err, controlplaneauth.ErrForbidden) {
		t.Fatalf("initiator used executor permission: %v", err)
	}
	for _, invalidContext := range []context.Context{ctx, auth.WithIdentity(ctx, auth.Anonymous()),
		auth.WithIdentity(ctx, &auth.Identity{Method: "token", Issuer: action.Executor.Issuer, Subject: action.Executor.Subject})} {
		if err := storage.AuthorizeWorkflowExecutorTarget(invalidContext, action); err == nil {
			t.Fatal("unverified or shared-token principal initiated a target")
		}
	}
	role.Spec.ExecutionRules = nil
	roleData, err := json.Marshal(role)
	if err != nil {
		t.Fatal(err)
	}
	if err := storage.Update(ctx, "ControlPlaneRole", "control", "executor", roleData, 1); err != nil {
		t.Fatal(err)
	}
	if err := storage.AuthorizeWorkflowExecutorTarget(executorContext, action); !errors.Is(err, controlplaneauth.ErrForbidden) {
		t.Fatalf("revoked executor permission remained usable: %v", err)
	}
	data, err := json.Marshal(agent)
	if err != nil {
		t.Fatal(err)
	}
	if err := storage.Update(ctx, "Agent", "team-a", "analyst", data, action.Target.ResourceVersion); err != nil {
		t.Fatal(err)
	}
	if err := runtimeapproval.AuthorizeWorkflowExecutorTarget(ctx, storage, action, allow); err == nil {
		t.Fatal("accepted target survived a live resource revision change")
	}
	if _, err := storage.GetWorkflowExecutorResource(ctx, "Agent", "team-b", "analyst"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("foreign namespace read = %v", err)
	}
	if _, err := storage.GetWorkflowExecutorResource(ctx, "Model", "team-a", "analyst"); err == nil {
		t.Fatal("executor reader accepted an unsupported resource kind")
	}
}

func TestExecutorProtectedPostgresRecords(t *testing.T) {
	storage := newTestStore(t)
	ctx := context.Background()
	proposal, payload := conformancetest.ExecutorApprovalProposal(t, runtimeApprovalTestKeys(t), "protected-records")
	action, err := runtimeapproval.WorkflowExecutorActionFromApproval(proposal, payload)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Add(-time.Second)
	authority := runtimeapproval.WorkflowExecutorAuthority{PolicyVersion: runtimeapproval.WorkflowExecutorPolicyVersion, Reference: action.ExecutionBinding, Workflow: action.Workflow,
		WorkflowRevisionDigest: action.WorkflowRevisionDigest, Executor: action.Executor, Enabled: true,
		NotBefore: now.Add(-time.Minute), ExpiresAt: now.Add(time.Hour), ControllerUIDs: []string{"controller-uid"}, ApprovedBy: action.Initiator,
		Limits: runtimeapproval.WorkflowExecutorLimits{MaxEffects: 8, MaxPayloadBytes: 32768, MaxDurationSeconds: 3600, MaxMembers: 8, MaxTurns: 100, MaxMessages: 500, MaxDelegations: 100, MaxHandoffDepth: 5, MaxParallelTasks: 8, ContentRetentionSeconds: 604800, ReplayRetentionSeconds: 7776000},
		Targets: []runtimeapproval.WorkflowExecutorTarget{{Agent: action.Target, Service: action.Service,
			InitialDecision: api.RuntimeApprovalDecisionPending, Operation: runtimeapproval.WorkflowExecutorInitiate}}}
	workflow := api.Workflow{ObjectMeta: api.ObjectMeta{Name: action.Workflow.Name, Namespace: action.Workflow.Namespace}, Spec: api.WorkflowSpec{Steps: []api.WorkflowStep{{Name: "review", Type: api.WorkflowStepTypeCollaboration, Collaboration: &api.WorkflowCollaboration{Strategy: api.CollaborationStrategySupervisor, Protocol: api.CollaborationProtocolA2A, Members: []api.CollaborationMember{{Name: "analyst", AgentRef: api.AgentReference{Name: action.Target.Name}, ServiceRef: &api.LocalObjectReference{Name: action.Service.Name}}}, Supervisor: &api.CollaborationSupervisor{Coordinator: "analyst"}, Limits: api.CollaborationLimits{MaxTurns: 100, MaxMessages: 500, MaxDelegations: 100, MaxDelegationDepth: 5, MaxParallelTasks: 8, MaxTokens: 4096}}}}}}
	workflowJSON, err := json.Marshal(workflow)
	if err != nil {
		t.Fatal(err)
	}
	workflowJSON, err = storage.CreateAndGet(ctx, "Workflow", workflow.Namespace, workflow.Name, workflowJSON)
	if err != nil {
		t.Fatal(err)
	}
	revision := int64(1)
	for kind, value := range map[string]any{
		"Agent":        api.Agent{ObjectMeta: api.ObjectMeta{Name: action.Target.Name, Namespace: action.Target.Namespace}, Spec: api.AgentSpec{Runtime: &api.AgentRuntimeSpec{Class: "native"}}, Status: api.AgentStatus{Phase: api.PhaseReady, RevisionCount: 1, Runtime: &api.AgentRuntimeStatus{Ready: "True", ReadyReplicas: 1, AvailableReplicas: 1}}},
		"AgentService": api.AgentService{ObjectMeta: api.ObjectMeta{Name: action.Service.Name, Namespace: action.Service.Namespace}, Spec: api.AgentServiceSpec{AgentRef: api.LocalObjectReference{Name: action.Target.Name}, RevisionPin: &revision}, Status: api.AgentServiceStatus{Phase: api.PhaseReady, ReadyInstances: 1, ActiveRevision: 1}},
	} {
		encoded, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		reference := &action.Target
		if kind == "AgentService" {
			reference = &action.Service
		}
		encoded, err = storage.CreateAndGet(ctx, kind, reference.Namespace, reference.Name, encoded)
		if err != nil {
			t.Fatal(err)
		}
		reference.UID, reference.ResourceVersion = resourceIdentity(t, encoded)
		reference.ConfigurationDigest, err = runtimeapproval.WorkflowExecutorSpecDigest(encoded)
		if err != nil {
			t.Fatal(err)
		}
	}
	authority.Targets[0].Agent, authority.Targets[0].Service = action.Target, action.Service
	authority.Workflow.UID, authority.Workflow.ResourceVersion = resourceIdentity(t, workflowJSON)
	authority.WorkflowRevisionDigest, err = runtimeapproval.WorkflowExecutorSpecDigest(workflowJSON)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"uid", "version", "digest"} {
		forged := authority
		switch field {
		case "uid":
			forged.Workflow.UID = "invented"
		case "version":
			forged.Workflow.ResourceVersion++
		case "digest":
			forged.WorkflowRevisionDigest = strings.Repeat("d", 64)
		}
		if _, err := storage.CreateWorkflowExecutorAuthority(ctx, forged); !errors.Is(err, store.ErrConflict) {
			t.Fatalf("binding with invented Workflow %s accepted: %v", field, err)
		}
	}
	created, err := storage.CreateWorkflowExecutorAuthority(ctx, authority)
	if err != nil {
		t.Fatal(err)
	}
	if created.Reference.UID == authority.Reference.UID || created.Reference.ResourceVersion != 1 {
		t.Fatal("binding trusted caller-assigned persistence identity")
	}
	if _, err := storage.CreateWorkflowExecutorAuthority(ctx, authority); !errors.Is(err, store.ErrAlreadyExists) {
		t.Fatalf("binding recreation: %v", err)
	}
	if _, err := storage.GetWorkflowExecutorAuthority(ctx, authority.Reference); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("caller-assigned binding UID accepted: %v", err)
	}
	retained, err := storage.GetWorkflowExecutorAuthority(ctx, created.Reference)
	if err != nil || !reflect.DeepEqual(created, retained) {
		t.Fatalf("binding round trip: %+v %v", retained, err)
	}
	run := api.WorkflowRun{ObjectMeta: api.ObjectMeta{Name: "protected-run", Namespace: action.Workflow.Namespace, Annotations: map[string]string{runtimeapproval.WorkflowRunAcceptanceRequiredAnnotation: "true"}},
		Spec: api.WorkflowRunSpec{WorkflowRef: api.LocalObjectReference{Name: action.Workflow.Name}}}
	raw, err := json.Marshal(run)
	if err != nil {
		t.Fatal(err)
	}
	raw, err = storage.CreateAndGet(ctx, "WorkflowRun", run.Namespace, run.Name, raw)
	if err != nil {
		t.Fatal(err)
	}
	uid, version := resourceIdentity(t, raw)
	acceptance := runtimeapproval.WorkflowRunAcceptance{Workspace: run.Namespace, RunUID: uid,
		Run:      api.RuntimeApprovalReference{Kind: "WorkflowRun", Scope: "Namespaced", Namespace: run.Namespace, Name: run.Name, UID: uid, ResourceVersion: version},
		Workflow: created.Workflow, WorkflowRevisionDigest: created.WorkflowRevisionDigest, ExecutionBinding: created.Reference,
		Executor: created.Executor, Initiator: action.Initiator, BindingDigest: action.BindingDigest, RequestDigest: strings.Repeat("a", 64),
		AcceptedAt: now, ExpiresAt: now.Add(30 * time.Minute)}
	acceptance.RequestDigest, err = runtimeapproval.WorkflowExecutorSpecDigest(raw)
	if err != nil {
		t.Fatal(err)
	}
	acceptance.BindingDigest, err = runtimeapproval.WorkflowRunExecutionScopeDigest(acceptance, created)
	if err != nil {
		t.Fatal(err)
	}
	ctx = auth.WithIdentity(ctx, &auth.Identity{Method: action.Initiator.Method, Issuer: action.Initiator.Issuer, Subject: action.Initiator.Subject})
	useRole := api.ControlPlaneRole{ObjectMeta: api.ObjectMeta{Name: "workflow-use", Namespace: run.Namespace}, Spec: api.ControlPlaneRoleSpec{ExecutionRules: []api.ControlPlaneExecutionRule{{Effect: "allow", Operation: api.ControlPlaneWorkflowUse, Workspace: run.Namespace, ResourceName: created.Workflow.Name, ResourceUID: created.Workflow.UID, RevisionDigest: created.WorkflowRevisionDigest}}}}
	useBinding := api.ControlPlaneRoleBinding{ObjectMeta: api.ObjectMeta{Name: useRole.Name, Namespace: run.Namespace}, Spec: api.ControlPlaneRoleBindingSpec{RoleRef: api.LocalObjectReference{Name: useRole.Name}, Subjects: []api.ControlPlaneRoleSubject{action.Initiator}}}
	for kind, value := range map[string]any{"ControlPlaneRole": useRole, "ControlPlaneRoleBinding": useBinding} {
		encoded, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if err := storage.Create(ctx, kind, run.Namespace, useRole.Name, encoded); err != nil {
			t.Fatal(err)
		}
	}
	for _, field := range []string{"request", "scope"} {
		forged := acceptance
		if field == "request" {
			forged.RequestDigest = strings.Repeat("d", 64)
		} else {
			forged.BindingDigest = strings.Repeat("d", 64)
		}
		if _, err := storage.CreateWorkflowRunAcceptance(ctx, forged); !errors.Is(err, store.ErrConflict) {
			t.Fatalf("forged %s digest accepted: %v", field, err)
		}
	}
	if _, err := storage.CreateWorkflowRunAcceptance(ctx, acceptance); err != nil {
		t.Fatal(err)
	}
	if actual, err := storage.CreateWorkflowRunAcceptance(ctx, acceptance); err != nil || !reflect.DeepEqual(actual, acceptance) {
		t.Fatalf("exact acceptance retry: %+v %v", actual, err)
	}
	forged := acceptance
	forged.Initiator.Subject = "other-submitter"
	if _, err := storage.CreateWorkflowRunAcceptance(ctx, forged); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("acceptance initiator substitution: %v", err)
	}
	atomicRun := run
	atomicRun.Name = "atomic-submission"
	for _, scope := range []string{"root", "spec"} {
		unknownRun := atomicRun
		unknownRun.Name = "unknown-submission-" + scope
		encoded, err := json.Marshal(unknownRun)
		if err != nil {
			t.Fatal(err)
		}
		var document map[string]any
		if err := json.Unmarshal(encoded, &document); err != nil {
			t.Fatal(err)
		}
		if scope == "spec" {
			document["spec"].(map[string]any)["unexpected"] = "discarded"
		} else {
			document["unexpected"] = "discarded"
		}
		encoded, err = json.Marshal(document)
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := storage.CreateAcceptedWorkflowRun(ctx, created.Reference, encoded, 60); !errors.Is(err, store.ErrConflict) {
			t.Fatalf("unknown %s submission field was accepted: %v", scope, err)
		}
		if _, err := storage.Get(ctx, "WorkflowRun", run.Namespace, unknownRun.Name); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("invalid %s submission left a public run: %v", scope, err)
		}
	}
	atomicJSON, err := json.Marshal(atomicRun)
	if err != nil {
		t.Fatal(err)
	}
	savedRun, savedAcceptance, err := storage.CreateAcceptedWorkflowRun(ctx, created.Reference, atomicJSON, 60)
	if err != nil || savedRun.UID == "" || savedRun.UID != savedAcceptance.RunUID {
		t.Fatalf("atomic accepted submission: %+v %+v %v", savedRun, savedAcceptance, err)
	}
	if _, _, err := storage.CreateAcceptedWorkflowRun(ctx, created.Reference, atomicJSON, 60); !errors.Is(err, store.ErrAlreadyExists) {
		t.Fatalf("same-name submission replay: %v", err)
	}
	if err := storage.Delete(ctx, "ControlPlaneRoleBinding", run.Namespace, useRole.Name); err != nil {
		t.Fatal(err)
	}
	atomicRun.Name = "revoked-submission"
	atomicJSON, err = json.Marshal(atomicRun)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := storage.CreateAcceptedWorkflowRun(ctx, created.Reference, atomicJSON, 60); err == nil {
		t.Fatal("final acceptance ignored Workflow/use revocation")
	}
	if _, err := storage.Get(ctx, "WorkflowRun", run.Namespace, atomicRun.Name); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("failed acceptance left a public orphan: %v", err)
	}
	testAcceptedExecutorInitiationLifecycle(t, storage, created, savedAcceptance, payload)
	testWorkflowSubmissionRevocationRaces(t, storage, ctx, created, workflowJSON)
	for _, kind := range []string{workflowExecutorAuthorityKind, workflowRunAcceptanceKind, workflowActivationRecordKind} {
		if err := storage.Create(ctx, kind, run.Namespace, "forged", raw); !errors.Is(err, store.ErrConflict) {
			t.Fatalf("generic create reached %s: %v", kind, err)
		}
		if _, err := storage.Get(ctx, kind, run.Namespace, "forged"); !errors.Is(err, store.ErrConflict) {
			t.Fatalf("generic read reached %s: %v", kind, err)
		}
		if _, err := storage.List(ctx, kind, store.ListOptions{}); !errors.Is(err, store.ErrConflict) {
			t.Fatalf("generic list reached %s: %v", kind, err)
		}
		if _, err := storage.Watch(ctx, kind, store.ListOptions{}); !errors.Is(err, store.ErrConflict) {
			t.Fatalf("generic watch reached %s: %v", kind, err)
		}
		if err := storage.CreateRevision(ctx, kind, run.Namespace, "forged", 1, []byte(`{}`), raw); !errors.Is(err, store.ErrConflict) {
			t.Fatalf("generic revision write reached %s: %v", kind, err)
		}
		if _, err := storage.ListRevisions(ctx, kind, run.Namespace, "forged"); !errors.Is(err, store.ErrConflict) {
			t.Fatalf("generic revision list reached %s: %v", kind, err)
		}
		if _, err := storage.GetRevision(ctx, kind, run.Namespace, "forged", 1); !errors.Is(err, store.ErrConflict) {
			t.Fatalf("generic revision read reached %s: %v", kind, err)
		}
		if err := storage.AppendEvent(ctx, kind, run.Namespace, "forged", store.InstanceEvent{}); !errors.Is(err, store.ErrConflict) {
			t.Fatalf("generic event write reached %s: %v", kind, err)
		}
		if _, err := storage.ListEvents(ctx, kind, run.Namespace, "forged"); !errors.Is(err, store.ErrConflict) {
			t.Fatalf("generic event read reached %s: %v", kind, err)
		}
		if err := storage.Update(ctx, kind, run.Namespace, "forged", raw, 1); !errors.Is(err, store.ErrConflict) {
			t.Fatalf("generic update reached %s: %v", kind, err)
		}
		if err := storage.Delete(ctx, kind, run.Namespace, "forged"); !errors.Is(err, store.ErrConflict) {
			t.Fatalf("generic delete reached %s: %v", kind, err)
		}
	}
	revoked := created
	revoked.Enabled = false
	updated, err := storage.UpdateWorkflowExecutorAuthority(ctx, revoked)
	if err != nil || updated.Reference.ResourceVersion != 2 || updated.Enabled {
		t.Fatalf("binding revocation: %+v %v", updated, err)
	}
	if _, err := storage.GetWorkflowExecutorAuthority(ctx, created.Reference); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("revoked binding retained old version: %v", err)
	}
	if _, err := storage.UpdateWorkflowExecutorAuthority(ctx, revoked); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("stale binding overwrite: %v", err)
	}
}

func testWorkflowSubmissionRevocationRaces(t *testing.T, storage *Store, authenticated context.Context, authority runtimeapproval.WorkflowExecutorAuthority, workflowJSON []byte) {
	t.Helper()
	for _, boundary := range []string{"binding", "source-use"} {
		for _, first := range []string{"revocation", "acceptance"} {
			t.Run(boundary+"-"+first+"-first", func(t *testing.T) {
				ctx, cancel := context.WithTimeout(authenticated, 30*time.Second)
				defer cancel()
				name := "race-" + boundary + "-" + first
				var workflow api.Workflow
				if err := json.Unmarshal(workflowJSON, &workflow); err != nil {
					t.Fatal(err)
				}
				workflow.Name, workflow.UID, workflow.ResourceVersion = name, "", 0
				encoded, err := json.Marshal(workflow)
				if err != nil {
					t.Fatal(err)
				}
				encoded, err = storage.CreateAndGet(ctx, "Workflow", workflow.Namespace, name, encoded)
				if err != nil {
					t.Fatal(err)
				}
				binding := authority
				binding.Reference.Name, binding.Workflow.Name = name, name
				binding.Workflow.UID, binding.Workflow.ResourceVersion = resourceIdentity(t, encoded)
				binding.WorkflowRevisionDigest, err = runtimeapproval.WorkflowExecutorSpecDigest(encoded)
				if err != nil {
					t.Fatal(err)
				}
				binding, err = storage.CreateWorkflowExecutorAuthority(ctx, binding)
				if err != nil {
					t.Fatal(err)
				}
				initiator, err := runtimeapproval.AuthenticatedWorkflowExecutor(ctx)
				if err != nil {
					t.Fatal(err)
				}
				role := api.ControlPlaneRole{ObjectMeta: api.ObjectMeta{Name: name, Namespace: workflow.Namespace}, Spec: api.ControlPlaneRoleSpec{ExecutionRules: []api.ControlPlaneExecutionRule{{Effect: "allow", Operation: api.ControlPlaneWorkflowUse, Workspace: workflow.Namespace, ResourceName: name, ResourceUID: binding.Workflow.UID, RevisionDigest: binding.WorkflowRevisionDigest}}}}
				roleBinding := api.ControlPlaneRoleBinding{ObjectMeta: role.ObjectMeta, Spec: api.ControlPlaneRoleBindingSpec{RoleRef: api.LocalObjectReference{Name: name}, Subjects: []api.ControlPlaneRoleSubject{initiator}}}
				for kind, value := range map[string]any{"ControlPlaneRole": role, "ControlPlaneRoleBinding": roleBinding} {
					data, err := json.Marshal(value)
					if err != nil {
						t.Fatal(err)
					}
					if err := storage.Create(ctx, kind, workflow.Namespace, name, data); err != nil {
						t.Fatal(err)
					}
				}
				run := api.WorkflowRun{ObjectMeta: api.ObjectMeta{Name: name, Namespace: workflow.Namespace, Annotations: map[string]string{runtimeapproval.WorkflowRunAcceptanceRequiredAnnotation: "true"}}, Spec: api.WorkflowRunSpec{WorkflowRef: api.LocalObjectReference{Name: name}}}
				runJSON, err := json.Marshal(run)
				if err != nil {
					t.Fatal(err)
				}
				holder, err := storage.pool.Begin(ctx)
				if err != nil {
					t.Fatal(err)
				}
				var workers sync.WaitGroup
				defer func() {
					cancel()
					_ = holder.Rollback(context.Background())
					workers.Wait()
				}()
				kind := workflowExecutorAuthorityKind
				if boundary == "source-use" {
					kind = "ControlPlaneRole"
				}
				var holderPID int
				if err := holder.QueryRow(ctx, `SELECT pg_backend_pid() FROM resources WHERE kind=$1 AND namespace=$2 AND name=$3 AND deleted_at IS NULL FOR UPDATE`, kind, workflow.Namespace, name).Scan(&holderPID); err != nil {
					t.Fatal(err)
				}
				waitForBlocked := func(expected int) {
					t.Helper()
					ticker := time.NewTicker(10 * time.Millisecond)
					defer ticker.Stop()
					for {
						var blocked int
						if err := storage.pool.QueryRow(ctx, `WITH RECURSIVE waiters(pid) AS (
							SELECT $1::integer
							UNION
							SELECT activity.pid FROM pg_stat_activity activity
							JOIN waiters ON waiters.pid=ANY(pg_blocking_pids(activity.pid))
							WHERE activity.datname=current_database()
						) SELECT count(*)-1 FROM waiters`, holderPID).Scan(&blocked); err != nil {
							t.Fatal(err)
						}
						if blocked >= expected {
							return
						}
						select {
						case <-ctx.Done():
							t.Fatalf("expected %d blocked operations, observed %d: %v", expected, blocked, ctx.Err())
						case <-ticker.C:
						}
					}
				}
				var acceptedRun api.WorkflowRun
				var acceptance runtimeapproval.WorkflowRunAcceptance
				var acceptanceErr, revocationErr error
				accept := func() {
					acceptedRun, acceptance, acceptanceErr = storage.CreateAcceptedWorkflowRun(ctx, binding.Reference, runJSON, 60)
				}
				revoke := func() {
					if boundary == "binding" {
						revoked := binding
						revoked.Enabled = false
						_, revocationErr = storage.UpdateWorkflowExecutorAuthority(ctx, revoked)
						return
					}
					role.Spec.ExecutionRules = nil
					data, err := json.Marshal(role)
					if err != nil {
						revocationErr = err
						return
					}
					revocationErr = storage.Update(ctx, "ControlPlaneRole", workflow.Namespace, name, data, 1)
				}
				operations := []func(){revoke, accept}
				if first == "acceptance" {
					operations = []func(){accept, revoke}
				}
				for index, operation := range operations {
					workers.Add(1)
					go func() { defer workers.Done(); operation() }()
					waitForBlocked(index + 1)
				}
				if err := holder.Commit(ctx); err != nil {
					t.Fatal(err)
				}
				workers.Wait()
				if revocationErr != nil {
					t.Fatalf("revocation failed: %v", revocationErr)
				}
				if first == "revocation" {
					var databaseError *pgconn.PgError
					if !errors.Is(acceptanceErr, store.ErrConflict) && !errors.Is(acceptanceErr, controlplaneauth.ErrForbidden) && !(errors.As(acceptanceErr, &databaseError) && databaseError.Code == "40001") {
						t.Fatalf("revocation-first acceptance error: %v", acceptanceErr)
					}
					if acceptedRun.UID != "" || acceptance.RunUID != "" {
						t.Fatal("failed submission returned accepted identities")
					}
					if _, err := storage.Get(ctx, "WorkflowRun", run.Namespace, name); !errors.Is(err, store.ErrNotFound) {
						t.Fatalf("revocation-first submission left a public run: %v", err)
					}
					var count int
					if err := storage.pool.QueryRow(ctx, `SELECT count(*) FROM resources WHERE kind=$1 AND namespace=$2 AND data->'run'->>'name'=$3 AND deleted_at IS NULL`, workflowRunAcceptanceKind, run.Namespace, name).Scan(&count); err != nil || count != 0 {
						t.Fatalf("revocation-first submission left acceptance records: %d %v", count, err)
					}
				} else {
					if acceptanceErr != nil || acceptedRun.UID == "" || acceptedRun.UID != acceptance.RunUID {
						t.Fatalf("acceptance-first submission: %+v %+v %v", acceptedRun, acceptance, acceptanceErr)
					}
					retained, err := storage.GetWorkflowRunAcceptance(ctx, run.Namespace, acceptedRun.UID)
					if err != nil || !reflect.DeepEqual(retained, acceptance) || retained.Initiator != initiator {
						t.Fatalf("revocation changed accepted attribution: %+v %v", retained, err)
					}
				}
				run.Name += "-later"
				runJSON, err = json.Marshal(run)
				if err != nil {
					t.Fatal(err)
				}
				if _, _, err := storage.CreateAcceptedWorkflowRun(ctx, binding.Reference, runJSON, 60); err == nil {
					t.Fatal("new submission accepted after revocation")
				}
				if _, err := storage.Get(ctx, "WorkflowRun", run.Namespace, run.Name); !errors.Is(err, store.ErrNotFound) {
					t.Fatalf("post-revocation submission left a public run: %v", err)
				}
			})
		}
	}
}

type executorLifecycleAuthenticator struct{ principal api.ControlPlaneRoleSubject }

func (authenticator executorLifecycleAuthenticator) Authenticate(_ context.Context, token string) (*auth.Identity, error) {
	if token != "executor-fixture" {
		return nil, errors.New("unverified executor fixture credential")
	}
	return &auth.Identity{Method: authenticator.principal.Method, Issuer: authenticator.principal.Issuer, Subject: authenticator.principal.Subject}, nil
}

func testAcceptedExecutorInitiationLifecycle(t *testing.T, storage *Store, authority runtimeapproval.WorkflowExecutorAuthority, acceptance runtimeapproval.WorkflowRunAcceptance, payload []byte) {
	t.Helper()
	keys := runtimeApprovalTestKeys(t)
	storage.ConfigureRuntimeApprovalBindingKeys(keys)
	ctx := auth.WithIdentity(context.Background(), &auth.Identity{Method: authority.Executor.Method, Issuer: authority.Executor.Issuer, Subject: authority.Executor.Subject})
	target := authority.Targets[0]
	role := api.ControlPlaneRole{ObjectMeta: api.ObjectMeta{Name: "executor-invoke", Namespace: acceptance.Workspace}, Spec: api.ControlPlaneRoleSpec{ExecutionRules: []api.ControlPlaneExecutionRule{{Effect: "allow", Operation: api.ControlPlaneAgentInvoke, Workspace: target.Agent.Namespace, ResourceName: target.Agent.Name, ResourceUID: target.Agent.UID}}}}
	binding := api.ControlPlaneRoleBinding{ObjectMeta: role.ObjectMeta, Spec: api.ControlPlaneRoleBindingSpec{RoleRef: api.LocalObjectReference{Name: role.Name}, Subjects: []api.ControlPlaneRoleSubject{authority.Executor}}}
	for kind, value := range map[string]any{"ControlPlaneRole": role, "ControlPlaneRoleBinding": binding} {
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if err := storage.Create(ctx, kind, role.Namespace, role.Name, raw); err != nil {
			t.Fatal(err)
		}
	}
	clock := func() (time.Time, error) { return storage.WorkflowExecutorClock(ctx) }
	currentExecutor := runtimeapproval.AuthenticatedWorkflowExecutor
	currentController := func(context.Context) (string, error) { return authority.ControllerUIDs[0], nil }
	authorizer, err := runtimeapproval.NewLiveWorkflowExecutorAuthorizer(runtimeapproval.WorkflowExecutorAuthorityConfig{Reader: storage, CurrentExecutor: currentExecutor, CurrentControllerUID: currentController, Clock: clock})
	if err != nil {
		t.Fatal(err)
	}
	action := runtimeapproval.WorkflowExecutorAction{Executor: authority.Executor, Initiator: acceptance.Initiator, Workflow: authority.Workflow, WorkflowRevisionDigest: authority.WorkflowRevisionDigest, ExecutionBinding: authority.Reference,
		Target: target.Agent, Service: target.Service, Activation: store.WorkflowActivationKey{Workspace: acceptance.Workspace, RunUID: acceptance.RunUID, Step: "review", ActivationID: "accepted-visit", Attempt: 1}, OwnerEpoch: 1, BindingDigest: acceptance.BindingDigest, InvocationID: "accepted-initiation", Operation: runtimeapproval.WorkflowExecutorInitiate, ExpiresAt: acceptance.ExpiresAt, Payload: payload}
	now, err := clock()
	if err != nil {
		t.Fatal(err)
	}
	proposed, err := runtimeapproval.BuildWorkflowExecutorApproval(ctx, keys, action, api.RuntimeApprovalDecisionPending, now)
	if err != nil {
		t.Fatal(err)
	}
	actor, err := runtimeapproval.WorkflowApprovalActorDigest(proposed)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := storage.CreateWorkflowActivation(ctx, store.WorkflowActivationSpec{Key: action.Activation, BindingDigest: action.BindingDigest, Actors: []string{actor}, MaxEffects: authority.Limits.MaxEffects, MaxBytes: authority.Limits.MaxPayloadBytes, ExpiresAt: acceptance.ExpiresAt}); err != nil {
		t.Fatal(err)
	}
	dispatch, err := runtimeapproval.NewWorkflowDispatchGate(runtimeapproval.WorkflowDispatchConfig{Activations: storage, Approvals: storage, Keys: keys, Clock: clock, Eligibility: func(context.Context, api.RuntimeApproval) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	coordinator, err := runtimeapproval.NewWorkflowExecutorCoordinator(runtimeapproval.WorkflowExecutorCoordinatorConfig{Store: storage, Keys: keys, Authorizer: authorizer, Dispatch: dispatch, Clock: clock, TTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	service, err := runtimeapproval.NewWorkflowExecutorInitiationService(runtimeapproval.WorkflowExecutorInitiationConfig{Keys: keys, Coordinator: coordinator, CurrentExecutor: currentExecutor, CurrentControllerUID: currentController, Clock: clock})
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int64
	server := apiserver.New(apiserver.Config{}, storage, zap.NewNop(), apiserver.WithAuthenticator(executorLifecycleAuthenticator{principal: authority.Executor}, nil),
		apiserver.WithWorkflowExecutorInitiation(service, func(context.Context, runtimeapproval.WorkflowExecutorAction) error {
			calls.Add(1)
			return errors.New("target acknowledgement lost")
		}))
	httpServer := httptest.NewServer(server)
	t.Cleanup(func() {
		httpServer.Close()
		if err := server.Shutdown(context.Background()); err != nil {
			t.Errorf("shutdown initiation HTTP fixture: %v", err)
		}
	})
	client := httpServer.Client()
	client.Timeout = 5 * time.Second
	post := func(route string, input, output any, status int) {
		t.Helper()
		raw, err := json.Marshal(input)
		if err != nil {
			t.Fatal(err)
		}
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, httpServer.URL+"/internal/workflow-execution/namespaces/"+acceptance.Workspace+"/"+route, bytes.NewReader(raw))
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Authorization", "Bearer executor-fixture")
		response, err := client.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		if response.StatusCode != status {
			t.Fatalf("%s returned %d, want %d", route, response.StatusCode, status)
		}
		if err := json.NewDecoder(response.Body).Decode(output); err != nil {
			t.Fatal(err)
		}
	}
	var capability runtimeapproval.WorkflowExecutorInitiationCapability
	undeclared := action
	undeclared.Activation.Step = "undeclared"
	var denied map[string]any
	post("initiation-capabilities", map[string]any{"action": undeclared}, &denied, http.StatusForbidden)
	if calls.Load() != 0 {
		t.Fatal("undeclared Workflow step contacted a target")
	}
	post("initiation-capabilities", map[string]any{"action": action}, &capability, http.StatusCreated)
	var resolved runtimeapproval.Gate3Result
	post("initiations/resolve", map[string]any{"action": action, "capability": capability}, &resolved, http.StatusOK)
	if resolved.State != runtimeapproval.Gate3StatePending {
		t.Fatalf("accepted HTTP initiation did not preserve pending review: %+v", resolved)
	}
	var result runtimeapproval.WorkflowDispatchResult
	post("initiations/dispatch", map[string]any{"action": action, "capability": capability, "approval": resolved.Approval}, &result, http.StatusConflict)
	if result.TargetCalled || calls.Load() != 0 {
		t.Fatalf("pending HTTP initiation contacted target: %+v", result)
	}
	approved, err := storage.DecideRuntimeApproval(ctx, acceptance.Workspace, action.InvocationID, store.RuntimeApprovalDecision{Decision: api.RuntimeApprovalDecisionApproved, ExpectedResourceVersion: resolved.Approval.ResourceVersion, Issuer: "reviewer-issuer", Subject: "reviewer"})
	if err != nil {
		t.Fatal(err)
	}
	var ready runtimeapproval.Gate3Result
	post("initiations/resolve", map[string]any{"action": action, "capability": capability}, &ready, http.StatusOK)
	if ready.State != runtimeapproval.Gate3StateReady || !ready.Approval.Status.DecisionEvidenceDelivered || ready.Approval.UID != approved.UID {
		t.Fatalf("HTTP review did not preserve exact owner and durable audit: %+v", ready)
	}
	token, err := runtimeapproval.GeneratePreparationToken()
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := storage.PrepareRuntimeApproval(ctx, acceptance.Workspace, action.InvocationID, ready.Approval.ResourceVersion, token, ready.Approval.Spec.ExpiresAt)
	if err != nil {
		t.Fatal(err)
	}
	now, err = clock()
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := storage.ClaimRuntimeApprovalExecution(ctx, acceptance.Workspace, action.InvocationID, prepared.ResourceVersion, token, api.RuntimeApprovalClaimant{WorkloadUID: "executor-gateway-uid", ProcessStartNonce: "executor-process-0001", ClaimedAt: now, LastHeartbeat: now})
	if err != nil {
		t.Fatal(err)
	}
	post("initiations/dispatch", map[string]any{"action": action, "capability": capability, "approval": claimed}, &result, http.StatusConflict)
	if !result.DispatchCommitted || !result.TargetCalled || calls.Load() != 1 {
		t.Fatalf("claimed HTTP target failure lost committed evidence: %+v calls=%d", result, calls.Load())
	}
	var retry runtimeapproval.WorkflowDispatchResult
	post("initiations/dispatch", map[string]any{"action": action, "capability": capability, "approval": claimed}, &retry, http.StatusConflict)
	if retry.TargetCalled || calls.Load() != 1 {
		t.Fatalf("HTTP lost acknowledgement replayed target: %+v calls=%d", retry, calls.Load())
	}
}

func TestRuntimeApprovalCreateOrGetExactBindingAndInitialDecision(t *testing.T) {
	st := newTestStore(t)
	st.ConfigureRuntimeApprovalBindingKeys(runtimeApprovalTestKeys(t))
	ctx := context.Background()

	pending := runtimeApprovalFixture(t, "invocation-pending", api.RuntimeApprovalDecisionPending)
	created, first, err := st.CreateOrGetRuntimeApproval(ctx, pending, runtimeApprovalPayload, 15*time.Minute)
	if err != nil || !created {
		t.Fatalf("first CreateOrGetRuntimeApproval() = created %v, err %v", created, err)
	}
	if first.Status.DecisionState != api.RuntimeApprovalDecisionPending || first.Status.ExecutionState != api.RuntimeApprovalExecutionNotStarted {
		t.Fatalf("first state = %s/%s", first.Status.DecisionState, first.Status.ExecutionState)
	}
	if first.Name != runtimeapproval.CanonicalName("team-a", "invocation-pending") || first.UID == "" || first.ResourceVersion != 1 {
		t.Fatalf("store identity not assigned: %#v", first.ObjectMeta)
	}

	created, replay, err := st.CreateOrGetRuntimeApproval(ctx, pending, runtimeApprovalPayload, time.Minute)
	if err != nil || created {
		t.Fatalf("identical replay = created %v, err %v", created, err)
	}
	if replay.UID != first.UID || !replay.Spec.CreatedAt.Equal(first.Spec.CreatedAt) || !replay.Spec.ExpiresAt.Equal(first.Spec.ExpiresAt) {
		t.Fatalf("identical replay did not preserve store fields: %#v", replay)
	}
	loosened := pending
	loosened.Status = runtimeapproval.InitialStatus(api.RuntimeApprovalDecisionNotRequired)
	if _, _, err := st.CreateOrGetRuntimeApproval(ctx, loosened, runtimeApprovalPayload, time.Minute); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("policy-loosening replay error = %v, want conflict", err)
	}

	conflict := pending
	conflict.Spec.Binding.Tag = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	if _, _, err := st.CreateOrGetRuntimeApproval(ctx, conflict, runtimeApprovalPayload, 15*time.Minute); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("changed binding error = %v, want conflict", err)
	}

	notRequired := runtimeApprovalFixture(t, "invocation-not-required", api.RuntimeApprovalDecisionNotRequired)
	created, result, err := st.CreateOrGetRuntimeApproval(ctx, notRequired, runtimeApprovalPayload, 15*time.Minute)
	if err != nil || !created || result.Status.DecisionState != api.RuntimeApprovalDecisionNotRequired {
		t.Fatalf("NotRequired create = created %v, state %q, err %v", created, result.Status.DecisionState, err)
	}
	if result.Status.DecisionAudit == nil || result.Status.DecidedAt == nil || result.Status.DecisionAudit.Payload.Decision != api.RuntimeApprovalDecisionNotRequired {
		t.Fatalf("NotRequired decision evidence = %#v", result.Status)
	}
}

func TestRuntimeApprovalCompactionPreservesReplayUntilRetention(t *testing.T) {
	st := newTestStore(t)
	st.ConfigureRuntimeApprovalBindingKeys(runtimeApprovalTestKeys(t))
	ctx := context.Background()
	proposal := runtimeApprovalFixture(t, "compacted-replay", api.RuntimeApprovalDecisionNotRequired)
	created, approval, err := st.CreateOrGetRuntimeApproval(ctx, proposal, runtimeApprovalPayload, 15*time.Minute)
	if err != nil || !created {
		t.Fatalf("create RuntimeApproval: created=%v err=%v", created, err)
	}
	approval.Status.DecisionEvidenceDelivered = true
	approval.Status.ExecutionState = api.RuntimeApprovalExecutionCompleted
	approval.Status.ExecutionDisposition = "Succeeded"
	data, err := json.Marshal(approval)
	if err != nil {
		t.Fatalf("marshal terminal RuntimeApproval: %v", err)
	}
	if _, err := testPool.Exec(ctx, `UPDATE runtime_approvals SET decision_state = $3, execution_state = $4,
		execution_disposition = $5, data = $6, updated_at = transaction_timestamp()
		WHERE workspace = $1 AND invocation_id = $2`, "team-a", proposal.Spec.InvocationID,
		approval.Status.DecisionState, approval.Status.ExecutionState, approval.Status.ExecutionDisposition, data); err != nil {
		t.Fatalf("make RuntimeApproval compactable: %v", err)
	}

	compacted, err := st.CompactRuntimeApproval(ctx, "team-a", proposal.Spec.InvocationID, 90*24*time.Hour)
	if err != nil || !compacted {
		t.Fatalf("CompactRuntimeApproval() = (%v, %v), want (true, nil)", compacted, err)
	}
	created, replay, err := st.CreateOrGetRuntimeApproval(ctx, proposal, runtimeApprovalPayload, 15*time.Minute)
	if err != nil || created {
		t.Fatalf("compacted replay = created %v, err %v", created, err)
	}
	if replay.UID != approval.UID || replay.Status.DecisionState != api.RuntimeApprovalDecisionNotRequired || replay.Status.ExecutionState != api.RuntimeApprovalExecutionCompleted {
		t.Fatalf("compacted replay lost terminal identity/state: %#v", replay)
	}
	changed := proposal
	changed.Spec.Operation = "changed-invoke"
	binding, err := runtimeapproval.Bind(ctx, runtimeApprovalTestKeys(t), runtimeapproval.ActionEnvelopeFromApproval(changed, runtimeApprovalPayload))
	if err != nil {
		t.Fatalf("bind changed proposal: %v", err)
	}
	changed.Spec.Binding = binding
	if _, _, err := st.CreateOrGetRuntimeApproval(ctx, changed, runtimeApprovalPayload, 15*time.Minute); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("changed compacted replay error = %v, want conflict", err)
	}

	pending := runtimeApprovalFixture(t, "not-compactable", api.RuntimeApprovalDecisionPending)
	if created, _, err := st.CreateOrGetRuntimeApproval(ctx, pending, runtimeApprovalPayload, 15*time.Minute); err != nil || !created {
		t.Fatalf("create pending RuntimeApproval: created=%v err=%v", created, err)
	}
	if compacted, err := st.CompactRuntimeApproval(ctx, "team-a", pending.Spec.InvocationID, 90*24*time.Hour); err != nil || compacted {
		t.Fatalf("compact pending RuntimeApproval = (%v, %v), want (false, nil)", compacted, err)
	}

	retired, err := st.RetireRuntimeApprovalTombstones(ctx, time.Now().UTC().Add(91*24*time.Hour))
	if err != nil || retired != 1 {
		t.Fatalf("RetireRuntimeApprovalTombstones() = (%d, %v), want (1, nil)", retired, err)
	}
	if _, err := st.GetRuntimeApprovalByInvocation(ctx, "team-a", proposal.Spec.InvocationID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("get retired RuntimeApproval error = %v, want not found", err)
	}
}

func TestRuntimeApprovalDecisionUsesDatabaseTimeAtExpiryBoundary(t *testing.T) {
	st := newTestStore(t)
	st.ConfigureRuntimeApprovalBindingKeys(runtimeApprovalTestKeys(t))
	ctx := context.Background()
	tests := []struct {
		name     string
		interval string
		want     string
	}{
		{name: "before", interval: "1 minute", want: api.RuntimeApprovalDecisionApproved},
		{name: "equal", interval: "0 seconds", want: api.RuntimeApprovalDecisionExpired},
		{name: "after", interval: "-1 second", want: api.RuntimeApprovalDecisionExpired},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			invocationID := "expiry-boundary-" + test.name
			_, approval, err := st.CreateOrGetRuntimeApproval(ctx, runtimeApprovalFixture(t, invocationID, api.RuntimeApprovalDecisionPending), runtimeApprovalPayload, time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := testPool.Exec(ctx, `WITH boundary AS (SELECT clock_timestamp() + $1::interval AS expires_at) UPDATE runtime_approvals SET expires_at = boundary.expires_at, data = jsonb_set(data, '{spec,expiresAt}', to_jsonb(boundary.expires_at)) FROM boundary WHERE workspace = 'team-a' AND invocation_id = $2`, test.interval, invocationID); err != nil {
				t.Fatal(err)
			}
			result, err := st.DecideRuntimeApproval(ctx, "team-a", invocationID, store.RuntimeApprovalDecision{
				Decision: api.RuntimeApprovalDecisionApproved, ExpectedResourceVersion: approval.ResourceVersion,
				Issuer: "https://issuer.example", Subject: "operator-1",
			})
			if err != nil || result.Status.DecisionState != test.want {
				t.Fatalf("decision = %q, err %v; want %q", result.Status.DecisionState, err, test.want)
			}
		})
	}
}

func TestRuntimeApprovalDecisionSamplesTimeAfterLockWait(t *testing.T) {
	st := newTestStore(t)
	st.ConfigureRuntimeApprovalBindingKeys(runtimeApprovalTestKeys(t))
	ctx := context.Background()
	invocationID := "expiry-after-lock-wait"
	_, approval, err := st.CreateOrGetRuntimeApproval(ctx, runtimeApprovalFixture(t, invocationID, api.RuntimeApprovalDecisionPending), runtimeApprovalPayload, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	holder, err := testPool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Rollback(ctx)
	logicalKey := "team-a\x1f" + invocationID
	if _, err := holder.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, logicalKey); err != nil {
		t.Fatal(err)
	}
	baseline := waitingAdvisoryLockCount(t, ctx)
	result := make(chan api.RuntimeApproval, 1)
	errResult := make(chan error, 1)
	go func() {
		decided, decideErr := st.DecideRuntimeApproval(ctx, "team-a", invocationID, store.RuntimeApprovalDecision{Decision: api.RuntimeApprovalDecisionApproved, ExpectedResourceVersion: approval.ResourceVersion, Issuer: "https://issuer.example", Subject: "operator-1"})
		result <- decided
		errResult <- decideErr
	}()
	waitForAdvisoryLockWaiters(t, ctx, baseline+1)
	if _, err := testPool.Exec(ctx, `WITH boundary AS (SELECT clock_timestamp() + interval '100 milliseconds' AS expires_at) UPDATE runtime_approvals SET expires_at = boundary.expires_at, data = jsonb_set(data, '{spec,expiresAt}', to_jsonb(boundary.expires_at)) FROM boundary WHERE workspace = 'team-a' AND invocation_id = $1`, invocationID); err != nil {
		t.Fatal(err)
	}
	if _, err := testPool.Exec(ctx, `SELECT pg_sleep(0.2)`); err != nil {
		t.Fatal(err)
	}
	if err := holder.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-errResult; err != nil {
		t.Fatal(err)
	}
	if decided := <-result; decided.Status.DecisionState != api.RuntimeApprovalDecisionExpired {
		t.Fatalf("decision after lock wait = %q, want Expired", decided.Status.DecisionState)
	}
}

func TestRuntimeApprovalCreateSerializesBeforeBindingKeyActivation(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	if err := st.ActivateRuntimeApprovalBindingKeys(ctx, runtimeApprovalTestKeys(t)); err != nil {
		t.Fatal(err)
	}
	holder, err := testPool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Rollback(ctx)
	if _, err := holder.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, runtimeApprovalBindingEpochLock); err != nil {
		t.Fatal(err)
	}
	baseline := waitingAdvisoryLockCount(t, ctx)
	createResult := make(chan api.RuntimeApproval, 1)
	createErr := make(chan error, 1)
	go func() {
		_, created, err := st.CreateOrGetRuntimeApproval(ctx, runtimeApprovalFixture(t, "create-before-activation", api.RuntimeApprovalDecisionPending), runtimeApprovalPayload, time.Minute)
		createResult <- created
		createErr <- err
	}()
	waitForAdvisoryLockWaiters(t, ctx, baseline+1)
	activationErr := make(chan error, 1)
	go func() { activationErr <- st.ActivateRuntimeApprovalBindingKeys(ctx, runtimeApprovalRotatedTestKeys(t)) }()
	waitForAdvisoryLockWaiters(t, ctx, baseline+2)
	if err := holder.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-createErr; err != nil {
		t.Fatalf("queued create: %v", err)
	}
	if created := <-createResult; created.Spec.Binding.Epoch != 1 || created.Spec.Binding.KeyVersion != 1 {
		t.Fatalf("queued create binding = epoch %d version %d", created.Spec.Binding.Epoch, created.Spec.Binding.KeyVersion)
	}
	if err := <-activationErr; err != nil {
		t.Fatalf("queued activation: %v", err)
	}
	if err := st.RuntimeApprovalBindingKeysReady(ctx); err != nil {
		t.Fatalf("rotated key readiness: %v", err)
	}
}

func waitingAdvisoryLockCount(t *testing.T, ctx context.Context) int {
	t.Helper()
	var count int
	if err := testPool.QueryRow(ctx, `SELECT count(*) FROM pg_locks WHERE locktype = 'advisory' AND NOT granted`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func waitForAdvisoryLockWaiters(t *testing.T, ctx context.Context, minimum int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for waitingAdvisoryLockCount(t, ctx) < minimum {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %d advisory lock waiters", minimum)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestRuntimeApprovalDecisionCASAndExpiryAreFirstWriterWins(t *testing.T) {
	st := newTestStore(t)
	st.ConfigureRuntimeApprovalBindingKeys(runtimeApprovalTestKeys(t))
	ctx := context.Background()
	_, approval, err := st.CreateOrGetRuntimeApproval(ctx, runtimeApprovalFixture(t, "decision-race", api.RuntimeApprovalDecisionPending), runtimeApprovalPayload, 15*time.Minute)
	if err != nil {
		t.Fatal(err)
	}

	decisions := []string{api.RuntimeApprovalDecisionApproved, api.RuntimeApprovalDecisionRejected}
	results := make(chan api.RuntimeApproval, len(decisions))
	errorsSeen := make(chan error, len(decisions))
	start := make(chan struct{})
	var wg sync.WaitGroup
	for _, value := range decisions {
		value := value
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			result, err := st.DecideRuntimeApproval(ctx, "team-a", "decision-race", store.RuntimeApprovalDecision{
				Decision: value, ExpectedResourceVersion: approval.ResourceVersion,
				Issuer: "https://issuer.example", Subject: "operator-1",
			})
			if err != nil {
				errorsSeen <- err
				return
			}
			results <- result
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	close(errorsSeen)
	for err := range errorsSeen {
		t.Fatalf("concurrent decision returned error: %v", err)
	}
	var winner string
	for result := range results {
		if winner == "" {
			winner = result.Status.DecisionState
		}
		if result.Status.DecisionState != winner {
			t.Fatalf("decision writers observed different terminal states: %q and %q", winner, result.Status.DecisionState)
		}
	}
	persisted, err := st.GetRuntimeApprovalByInvocation(ctx, "team-a", "decision-race")
	if err != nil || persisted.Status.DecisionState != winner || persisted.ResourceVersion != 2 {
		t.Fatalf("persisted winner = state %q rv %d, err %v", persisted.Status.DecisionState, persisted.ResourceVersion, err)
	}
	if persisted.Status.DecisionAudit == nil || persisted.Status.DecisionAudit.ID == "" || persisted.Status.DecisionAudit.Payload.Decision != winner {
		t.Fatalf("decision audit outbox not persisted atomically: %#v", persisted.Status.DecisionAudit)
	}
	if _, err := st.AcknowledgeRuntimeApprovalDecisionAudit(ctx, "team-a", "decision-race", persisted.ResourceVersion, "wrong-audit-id"); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("wrong decision audit ID error = %v, want conflict", err)
	}
	acknowledged, err := st.AcknowledgeRuntimeApprovalDecisionAudit(ctx, "team-a", "decision-race", persisted.ResourceVersion, persisted.Status.DecisionAudit.ID)
	if err != nil || !acknowledged.Status.DecisionEvidenceDelivered {
		t.Fatalf("acknowledge decision audit = %#v, %v", acknowledged.Status, err)
	}
	retried, err := st.AcknowledgeRuntimeApprovalDecisionAudit(ctx, "team-a", "decision-race", persisted.ResourceVersion, persisted.Status.DecisionAudit.ID)
	if err != nil || retried.ResourceVersion != acknowledged.ResourceVersion {
		t.Fatalf("idempotent acknowledgement retry = rv %d, err %v", retried.ResourceVersion, err)
	}

	_, expired, err := st.CreateOrGetRuntimeApproval(ctx, runtimeApprovalFixture(t, "expired-decision", api.RuntimeApprovalDecisionPending), runtimeApprovalPayload, 15*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := testPool.Exec(ctx, `UPDATE runtime_approvals SET expires_at = transaction_timestamp(), data = jsonb_set(data, '{spec,expiresAt}', to_jsonb(transaction_timestamp())) WHERE workspace = 'team-a' AND invocation_id = 'expired-decision'`); err != nil {
		t.Fatal(err)
	}
	expiredResult, err := st.DecideRuntimeApproval(ctx, "team-a", "expired-decision", store.RuntimeApprovalDecision{
		Decision: api.RuntimeApprovalDecisionApproved, ExpectedResourceVersion: expired.ResourceVersion,
		Issuer: "https://issuer.example", Subject: "operator-1",
	})
	if err != nil || expiredResult.Status.DecisionState != api.RuntimeApprovalDecisionExpired {
		t.Fatalf("decision at expiry = %q, err %v", expiredResult.Status.DecisionState, err)
	}
	if expiredResult.Status.DecidedByIssuer != "https://issuer.example" || expiredResult.Status.DecidedBySubject != "operator-1" {
		t.Fatalf("expired decision actor = %q/%q", expiredResult.Status.DecidedByIssuer, expiredResult.Status.DecidedBySubject)
	}
	retry, err := st.ExpireRuntimeApprovalDecision(ctx, "team-a", "expired-decision", expired.ResourceVersion)
	if err != nil || retry.Status.DecisionState != api.RuntimeApprovalDecisionExpired || retry.ResourceVersion != expiredResult.ResourceVersion {
		t.Fatalf("expiry retry changed winner: %#v, err %v", retry, err)
	}
}

func TestRuntimeApprovalDecisionActorBounds(t *testing.T) {
	st := newTestStore(t)
	st.ConfigureRuntimeApprovalBindingKeys(runtimeApprovalTestKeys(t))
	_, approval, err := st.CreateOrGetRuntimeApproval(context.Background(), runtimeApprovalFixture(t, "actor-bounds", api.RuntimeApprovalDecisionPending), runtimeApprovalPayload, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	_, err = st.DecideRuntimeApproval(context.Background(), "team-a", "actor-bounds", store.RuntimeApprovalDecision{Decision: api.RuntimeApprovalDecisionApproved, ExpectedResourceVersion: approval.ResourceVersion, Issuer: "https://issuer.example", Subject: string(make([]byte, 513))})
	if err == nil {
		t.Fatal("oversized decision actor was accepted")
	}
}

func TestRuntimeApprovalCreateRejectsCallerStatusAndUntrustedKeys(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	proposal := runtimeApprovalFixture(t, "status-forgery", api.RuntimeApprovalDecisionPending)
	proposal.Status.DecidedBySubject = "forged"
	if _, _, err := st.CreateOrGetRuntimeApproval(ctx, proposal, runtimeApprovalPayload, time.Minute); err == nil {
		t.Fatal("caller-owned decision status was accepted")
	}
	proposal.Status = runtimeapproval.InitialStatus(api.RuntimeApprovalDecisionPending)
	if _, _, err := st.CreateOrGetRuntimeApproval(ctx, proposal, runtimeApprovalPayload, time.Minute); !errors.Is(err, runtimeapproval.ErrBindingKeyUnavailable) {
		t.Fatalf("missing key store error = %v", err)
	}
	st.ConfigureRuntimeApprovalBindingKeys(runtimeApprovalTestKeys(t))
	proposal.Spec.Binding.KeyVersion = 2
	if _, _, err := st.CreateOrGetRuntimeApproval(ctx, proposal, runtimeApprovalPayload, time.Minute); err == nil {
		t.Fatal("stale key version was accepted for a new write")
	}
	forged := runtimeApprovalFixture(t, "forged-binding", api.RuntimeApprovalDecisionPending)
	forged.Spec.Binding.Tag = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	if _, _, err := st.CreateOrGetRuntimeApproval(ctx, forged, runtimeApprovalPayload, time.Minute); err == nil {
		t.Fatal("forged binding tag was accepted for a new write")
	}
}

func TestRuntimeApprovalPersistsLargePositiveKeyIdentifier(t *testing.T) {
	const version = int64(^uint64(0) >> 1)
	keys, err := runtimeapproval.NewStaticBindingKeyStore([]runtimeapproval.BindingKey{{
		Version: version, Algorithm: runtimeapproval.BindingAlgorithmV1, Domain: runtimeapproval.BindingDomainV1,
		Status: runtimeapproval.BindingKeyActive, Material: []byte("0123456789abcdef0123456789abcdef"),
	}})
	if err != nil {
		t.Fatal(err)
	}
	st := newTestStore(t)
	if err := st.ActivateRuntimeApprovalBindingKeys(context.Background(), keys); err != nil {
		t.Fatal(err)
	}
	proposal := runtimeApprovalFixture(t, "large-key-identifier", api.RuntimeApprovalDecisionPending)
	binding, err := runtimeapproval.Bind(context.Background(), keys, runtimeapproval.ActionEnvelopeFromApproval(proposal, runtimeApprovalPayload))
	if err != nil {
		t.Fatal(err)
	}
	proposal.Spec.Binding = binding
	created, approval, err := st.CreateOrGetRuntimeApproval(context.Background(), proposal, runtimeApprovalPayload, time.Minute)
	if err != nil || !created || approval.Spec.Binding.KeyVersion != version {
		t.Fatalf("large key identifier create = created %v, version %d, error %v", created, approval.Spec.Binding.KeyVersion, err)
	}
}

func TestRuntimeApprovalReplayAuthenticatesRetainedVerifyOnlyKey(t *testing.T) {
	st := newTestStore(t)
	st.ConfigureRuntimeApprovalBindingKeys(runtimeApprovalTestKeys(t))
	ctx := context.Background()
	proposal := runtimeApprovalFixture(t, "rotated-replay", api.RuntimeApprovalDecisionPending)
	if created, _, err := st.CreateOrGetRuntimeApproval(ctx, proposal, runtimeApprovalPayload, time.Minute); err != nil || !created {
		t.Fatalf("create before rotation = %v, %v", created, err)
	}
	if err := st.ActivateRuntimeApprovalBindingKeys(ctx, runtimeApprovalRotatedTestKeys(t)); err != nil {
		t.Fatal(err)
	}
	if created, _, err := st.CreateOrGetRuntimeApproval(ctx, proposal, runtimeApprovalPayload, time.Minute); err != nil || created {
		t.Fatalf("replay after rotation = %v, %v", created, err)
	}
	fresh := runtimeApprovalFixture(t, "verify-only-new-write", api.RuntimeApprovalDecisionPending)
	if _, _, err := st.CreateOrGetRuntimeApproval(ctx, fresh, runtimeApprovalPayload, time.Minute); err == nil {
		t.Fatal("verify-only key accepted for a new write")
	}
}

func TestRuntimeApprovalBindingKeyRotationRejectsRetainedEpochMutationAndSupportsThreeEpochs(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	if err := st.ActivateRuntimeApprovalBindingKeys(ctx, runtimeApprovalTestKeys(t)); err != nil {
		t.Fatal(err)
	}
	proposal := runtimeApprovalFixture(t, "three-epoch-replay", api.RuntimeApprovalDecisionPending)
	if created, _, err := st.CreateOrGetRuntimeApproval(ctx, proposal, runtimeApprovalPayload, time.Minute); err != nil || !created {
		t.Fatalf("create before rotations = %v, %v", created, err)
	}
	if err := st.ActivateRuntimeApprovalBindingKeys(ctx, runtimeApprovalRotatedTestKeys(t)); err != nil {
		t.Fatal(err)
	}
	mutated, err := runtimeapproval.NewStaticBindingKeyStoreAtEpoch(3, []runtimeapproval.BindingKey{
		{Version: 1, Epoch: 2, Algorithm: runtimeapproval.BindingAlgorithmV1, Domain: runtimeapproval.BindingDomainV1, Status: runtimeapproval.BindingKeyVerifyOnly, Material: []byte("0123456789abcdef0123456789abcdef")},
		{Version: 2, Epoch: 2, Algorithm: runtimeapproval.BindingAlgorithmV1, Domain: runtimeapproval.BindingDomainV1, Status: runtimeapproval.BindingKeyVerifyOnly, Material: []byte("abcdef0123456789abcdef0123456789")},
		{Version: 3, Algorithm: runtimeapproval.BindingAlgorithmV1, Domain: runtimeapproval.BindingDomainV1, Status: runtimeapproval.BindingKeyActive, Material: []byte("fedcba9876543210fedcba9876543210")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.ActivateRuntimeApprovalBindingKeys(ctx, mutated); err == nil {
		t.Fatal("retained key epoch mutation was accepted")
	}
	third, err := runtimeapproval.NewStaticBindingKeyStoreAtEpoch(3, []runtimeapproval.BindingKey{
		{Version: 1, Epoch: 1, Algorithm: runtimeapproval.BindingAlgorithmV1, Domain: runtimeapproval.BindingDomainV1, Status: runtimeapproval.BindingKeyVerifyOnly, Material: []byte("0123456789abcdef0123456789abcdef")},
		{Version: 2, Epoch: 2, Algorithm: runtimeapproval.BindingAlgorithmV1, Domain: runtimeapproval.BindingDomainV1, Status: runtimeapproval.BindingKeyVerifyOnly, Material: []byte("abcdef0123456789abcdef0123456789")},
		{Version: 3, Algorithm: runtimeapproval.BindingAlgorithmV1, Domain: runtimeapproval.BindingDomainV1, Status: runtimeapproval.BindingKeyActive, Material: []byte("fedcba9876543210fedcba9876543210")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.ActivateRuntimeApprovalBindingKeys(ctx, third); err != nil {
		t.Fatalf("activate third binding-key epoch: %v", err)
	}
	if created, approval, err := st.CreateOrGetRuntimeApproval(ctx, proposal, runtimeApprovalPayload, time.Minute); err != nil || created || approval.Spec.Binding.Epoch != 1 {
		t.Fatalf("epoch-one replay after third activation = created %v, epoch %d, error %v", created, approval.Spec.Binding.Epoch, err)
	}
}

func TestRuntimeApprovalBindingKeyReadinessRejectsMixedVersionsAndMaterial(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	if err := st.ActivateRuntimeApprovalBindingKeys(ctx, runtimeApprovalTestKeys(t)); err != nil {
		t.Fatal(err)
	}
	if err := st.RuntimeApprovalBindingKeysReady(ctx); err != nil {
		t.Fatalf("matching initial keys were not ready: %v", err)
	}
	if err := st.ActivateRuntimeApprovalBindingKeys(ctx, runtimeApprovalRotatedTestKeys(t)); err != nil {
		t.Fatal(err)
	}

	oldReplica := New(testPool, st.logger)
	oldReplica.ConfigureRuntimeApprovalBindingKeys(runtimeApprovalTestKeys(t))
	if err := oldReplica.RuntimeApprovalBindingKeysReady(ctx); !errors.Is(err, runtimeapproval.ErrBindingKeyUnavailable) {
		t.Fatalf("old-version readiness error = %v", err)
	}
	materialMismatch, err := runtimeapproval.NewStaticBindingKeyStoreAtEpoch(2, []runtimeapproval.BindingKey{
		{Version: 1, Epoch: 1, Algorithm: runtimeapproval.BindingAlgorithmV1, Domain: runtimeapproval.BindingDomainV1, Status: runtimeapproval.BindingKeyVerifyOnly, Material: []byte("0123456789abcdef0123456789abcdef")},
		{Version: 2, Algorithm: runtimeapproval.BindingAlgorithmV1, Domain: runtimeapproval.BindingDomainV1, Status: runtimeapproval.BindingKeyActive, Material: []byte("different-material-abcdef0123456789")},
	})
	if err != nil {
		t.Fatal(err)
	}
	mismatchedReplica := New(testPool, st.logger)
	mismatchedReplica.ConfigureRuntimeApprovalBindingKeys(materialMismatch)
	if err := mismatchedReplica.RuntimeApprovalBindingKeysReady(ctx); !errors.Is(err, runtimeapproval.ErrBindingKeyUnavailable) {
		t.Fatalf("material-mismatch readiness error = %v", err)
	}
}

func TestRuntimeApprovalDecideAndExpireRaceConvergesOnExpired(t *testing.T) {
	st := newTestStore(t)
	st.ConfigureRuntimeApprovalBindingKeys(runtimeApprovalTestKeys(t))
	ctx := context.Background()
	_, approval, err := st.CreateOrGetRuntimeApproval(ctx, runtimeApprovalFixture(t, "decide-expire-race", api.RuntimeApprovalDecisionPending), runtimeApprovalPayload, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := testPool.Exec(ctx, `UPDATE runtime_approvals SET expires_at = transaction_timestamp(), data = jsonb_set(data, '{spec,expiresAt}', to_jsonb(transaction_timestamp())) WHERE workspace = 'team-a' AND invocation_id = 'decide-expire-race'`); err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	results := make(chan api.RuntimeApproval, 2)
	errorsSeen := make(chan error, 2)
	go func() {
		<-start
		result, err := st.DecideRuntimeApproval(ctx, "team-a", "decide-expire-race", store.RuntimeApprovalDecision{Decision: api.RuntimeApprovalDecisionApproved, ExpectedResourceVersion: approval.ResourceVersion, Issuer: "https://issuer.example", Subject: "operator-1"})
		results <- result
		errorsSeen <- err
	}()
	go func() {
		<-start
		result, err := st.ExpireRuntimeApprovalDecision(ctx, "team-a", "decide-expire-race", approval.ResourceVersion)
		results <- result
		errorsSeen <- err
	}()
	close(start)
	for range 2 {
		if err := <-errorsSeen; err != nil {
			t.Fatalf("Decide/Expire race error: %v", err)
		}
		if result := <-results; result.Status.DecisionState != api.RuntimeApprovalDecisionExpired {
			t.Fatalf("Decide/Expire race state = %q", result.Status.DecisionState)
		}
	}
}

func TestRuntimeApprovalReadRejectsIndexedColumnDrift(t *testing.T) {
	st := newTestStore(t)
	st.ConfigureRuntimeApprovalBindingKeys(runtimeApprovalTestKeys(t))
	ctx := context.Background()
	proposal := runtimeApprovalFixture(t, "column-drift", api.RuntimeApprovalDecisionPending)
	if _, _, err := st.CreateOrGetRuntimeApproval(ctx, proposal, runtimeApprovalPayload, time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, err := testPool.Exec(ctx, `UPDATE runtime_approvals SET binding_tag = 'AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA' WHERE workspace = 'team-a' AND invocation_id = 'column-drift'`); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetRuntimeApprovalByInvocation(ctx, "team-a", "column-drift"); err == nil {
		t.Fatal("RuntimeApproval with mismatched indexed columns was returned")
	}
}

func TestPostgresStableUIDProjectionUniquenessAndAgentRecreation(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	if _, err := testPool.Exec(ctx, `DELETE FROM resources WHERE namespace = 'team-a' AND ((kind = 'Agent' AND name = 'agent-a') OR (kind = 'TrustProfile' AND name IN ('agent-a-profile', 'duplicate-profile')))`); err != nil {
		t.Fatal(err)
	}
	agent := api.Agent{TypeMeta: api.TypeMeta{APIVersion: api.APIVersion, Kind: "Agent"}, ObjectMeta: api.ObjectMeta{Name: "agent-a", Namespace: "team-a", UID: "forged"}}
	agentData, _ := json.Marshal(agent)
	createdData, err := st.CreateAndGet(ctx, "Agent", "team-a", "agent-a", agentData)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(createdData, &agent); err != nil || agent.UID == "" || agent.UID == "forged" {
		t.Fatalf("standalone Agent UID was not row-backed: %#v, err %v", agent.ObjectMeta, err)
	}
	firstUID := agent.UID
	profile := api.TrustProfile{
		TypeMeta:   api.TypeMeta{APIVersion: api.APIVersion, Kind: "TrustProfile"},
		ObjectMeta: api.ObjectMeta{Name: "agent-a-profile", Namespace: "team-a"},
		Spec:       api.TrustProfileSpec{AgentRef: api.LocalObjectReference{Name: "agent-a"}, AgentIncarnationUID: firstUID, InitialScore: 50},
	}
	profileData, _ := json.Marshal(profile)
	if _, err := st.CreateAndGet(ctx, "TrustProfile", "team-a", profile.Name, profileData); err != nil {
		t.Fatal(err)
	}
	duplicate := profile
	duplicate.Name = "duplicate-profile"
	duplicateData, _ := json.Marshal(duplicate)
	if _, err := st.CreateAndGet(ctx, "TrustProfile", "team-a", duplicate.Name, duplicateData); !errors.Is(err, store.ErrAlreadyExists) {
		t.Fatalf("duplicate TrustProfile error = %v, want already exists", err)
	}
	if err := st.Delete(ctx, "Agent", "team-a", "agent-a"); err != nil {
		t.Fatal(err)
	}
	agent.UID = ""
	agentData, _ = json.Marshal(agent)
	createdData, err = st.CreateAndGet(ctx, "Agent", "team-a", "agent-a", agentData)
	if err != nil {
		t.Fatalf("recreate Agent: %v", err)
	}
	var recreated api.Agent
	_ = json.Unmarshal(createdData, &recreated)
	if recreated.UID == firstUID {
		t.Fatalf("recreated Agent inherited UID %q", firstUID)
	}
	if _, err := runtimeapproval.NewResolver(st).Resolve(ctx, "team-a", "agent-a"); !errors.Is(err, runtimeapproval.ErrPolicyUnavailable) {
		t.Fatalf("resolver error = %v, want stale-profile rejection", err)
	}
}

var runtimeApprovalPayload = json.RawMessage(`{"argument":"sensitive"}`)

func TestRuntimeApprovalExecutorContract(t *testing.T) {
	storage := newTestStore(t)
	keys := runtimeApprovalTestKeys(t)
	storage.ConfigureRuntimeApprovalBindingKeys(keys)
	proposed, payload, terminal := conformancetest.RunExecutorApprovalContract(t, storage, keys)
	compacted, err := storage.CompactRuntimeApproval(context.Background(), terminal.Spec.Workspace, terminal.Spec.InvocationID, 90*24*time.Hour)
	if err != nil || !compacted {
		t.Fatalf("executor replay fence compaction: %v", err)
	}
	created, replay, err := storage.CreateOrGetRuntimeApproval(context.Background(), proposed, payload, time.Hour)
	if err != nil || created || replay.UID != terminal.UID || replay.Status.ExecutionState != api.RuntimeApprovalExecutionCompleted {
		t.Fatalf("compacted executor replay reopened: created=%v error=%v", created, err)
	}
	retained, err := storage.GetRuntimeApprovalByInvocation(context.Background(), proposed.Spec.Workspace, proposed.Spec.InvocationID)
	if err != nil || retained.Spec.ActorKind != api.RuntimeApprovalActorExecutor || retained.Spec.ExecutorAction == nil {
		t.Fatalf("executor tombstone read lost actor: %v", err)
	}
}

func TestRuntimeApprovalWorkflowContextPersistenceAndReplay(t *testing.T) {
	storage := newTestStore(t)
	keys := runtimeApprovalTestKeys(t)
	storage.ConfigureRuntimeApprovalBindingKeys(keys)
	proposed := runtimeApprovalFixture(t, "workflow-context", api.RuntimeApprovalDecisionPending)
	proposed.Spec.WorkflowContext = &api.WorkflowActionContext{Workspace: "origin", RunUID: "run-uid", Step: "team",
		ActivationID: "visit-1", Attempt: 1, OwnerEpoch: 1, BindingDigest: strings.Repeat("a", 64)}
	binding, err := runtimeapproval.Bind(context.Background(), keys, runtimeapproval.ActionEnvelopeFromApproval(proposed, runtimeApprovalPayload))
	if err != nil {
		t.Fatal(err)
	}
	proposed.Spec.Binding = binding
	created, first, err := storage.CreateOrGetRuntimeApproval(context.Background(), proposed, runtimeApprovalPayload, time.Minute)
	if err != nil || !created || !reflect.DeepEqual(first.Spec.WorkflowContext, proposed.Spec.WorkflowContext) {
		t.Fatalf("Workflow context creation: %v %+v %v", created, first.Spec.WorkflowContext, err)
	}
	retained, err := storage.GetRuntimeApprovalByInvocation(context.Background(), proposed.Spec.Workspace, proposed.Spec.InvocationID)
	if err != nil || !reflect.DeepEqual(retained.Spec.WorkflowContext, proposed.Spec.WorkflowContext) {
		t.Fatalf("Workflow context read: %+v %v", retained.Spec.WorkflowContext, err)
	}
	created, replay, err := storage.CreateOrGetRuntimeApproval(context.Background(), proposed, runtimeApprovalPayload, time.Minute)
	if err != nil || created || replay.UID != first.UID {
		t.Fatalf("exact Workflow replay: %v %+v %v", created, replay, err)
	}
	changed := proposed
	changedContext := *proposed.Spec.WorkflowContext
	changedContext.OwnerEpoch++
	changed.Spec.WorkflowContext = &changedContext
	if _, _, err := storage.CreateOrGetRuntimeApproval(context.Background(), changed, runtimeApprovalPayload, time.Minute); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("changed Workflow epoch accepted: %v", err)
	}
}

func runtimeApprovalFixture(t *testing.T, invocationID, decision string) api.RuntimeApproval {
	t.Helper()
	ref := func(kind, name, uid string) api.RuntimeApprovalReference {
		return api.RuntimeApprovalReference{Kind: kind, Scope: "Namespaced", Namespace: "team-a", Name: name, UID: uid, ResourceVersion: 1}
	}
	approval := api.RuntimeApproval{
		Spec: api.RuntimeApprovalSpec{
			Workspace: "team-a", InvocationID: invocationID, Plane: api.RuntimeApprovalPlaneTool, Operation: "invoke",
			AgentRef:         ref("Agent", "agent-a", "agent-uid"),
			AgentIdentityRef: ref("AgentIdentity", "agent-a", "identity-uid"),
			AgentInstanceRef: ref("AgentInstance", "agent-a-1", "instance-uid"),
			SessionRef:       "session-a", TargetRef: ref("Tool", "tool-a", "tool-uid"),
			AutonomyClassRef: api.RuntimeApprovalReference{Kind: "AutonomyClass", Scope: "Cluster", Name: "supervised", UID: "class-uid", ResourceVersion: 1},
		},
		Status: api.RuntimeApprovalStatus{DecisionState: decision, ExecutionState: api.RuntimeApprovalExecutionNotStarted},
	}
	binding, err := runtimeapproval.Bind(context.Background(), runtimeApprovalTestKeys(t), runtimeapproval.ActionEnvelopeFromApproval(approval, runtimeApprovalPayload))
	if err != nil {
		t.Fatal(err)
	}
	approval.Spec.Binding = binding
	return approval
}

func runtimeApprovalTestKeys(t *testing.T) runtimeapproval.BindingKeyStore {
	t.Helper()
	keys, err := runtimeapproval.NewStaticBindingKeyStore([]runtimeapproval.BindingKey{{
		Version: 1, Algorithm: runtimeapproval.BindingAlgorithmV1, Domain: runtimeapproval.BindingDomainV1,
		Status: runtimeapproval.BindingKeyActive, Material: []byte("0123456789abcdef0123456789abcdef"),
	}})
	if err != nil {
		t.Fatal(err)
	}
	return keys
}

func runtimeApprovalRotatedTestKeys(t *testing.T) runtimeapproval.BindingKeyStore {
	t.Helper()
	keys, err := runtimeapproval.NewStaticBindingKeyStoreAtEpoch(2, []runtimeapproval.BindingKey{
		{Version: 1, Epoch: 1, Algorithm: runtimeapproval.BindingAlgorithmV1, Domain: runtimeapproval.BindingDomainV1, Status: runtimeapproval.BindingKeyVerifyOnly, Material: []byte("0123456789abcdef0123456789abcdef")},
		{Version: 2, Algorithm: runtimeapproval.BindingAlgorithmV1, Domain: runtimeapproval.BindingDomainV1, Status: runtimeapproval.BindingKeyActive, Material: []byte("abcdef0123456789abcdef0123456789")},
	})
	if err != nil {
		t.Fatal(err)
	}
	return keys
}
