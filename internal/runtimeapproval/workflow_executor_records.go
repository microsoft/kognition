// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package runtimeapproval

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode/utf8"

	api "github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/store"
	"k8s.io/apimachinery/pkg/util/validation"
)

const WorkflowExecutorRecordMaxBytes = 1 << 20

const WorkflowExecutorPolicyVersion = "kognition-workflow-executor-policy-v1"

const WorkflowExecutorPrincipalsEnvironmentVariable = "KOGNITION_WORKFLOW_EXECUTOR_PRINCIPALS"

func WorkflowExecutorPrincipalsFromJSON(value string) ([]api.ControlPlaneRoleSubject, error) {
	if value == "" {
		return nil, nil
	}
	var principals []api.ControlPlaneRoleSubject
	if err := decodeWorkflowExecutorRecord([]byte(value), &principals); err != nil {
		return nil, err
	}
	if len(principals) == 0 || len(principals) > 16 {
		return nil, fmt.Errorf("executor configuration requires one to sixteen explicit principals")
	}
	seen := map[api.ControlPlaneRoleSubject]bool{}
	for _, principal := range principals {
		if executorRecordPrincipal(principal) != nil || seen[principal] {
			return nil, fmt.Errorf("executor principals must be exact, authenticated and unique")
		}
		seen[principal] = true
	}
	return principals, nil
}

const WorkflowRunAcceptanceRequiredAnnotation = "kognition.dev/workflow-execution-required"

func WorkflowExecutorBindingObjectName(namespace, name string) string {
	digest := sha256.Sum256([]byte(namespace + "\x00" + name))
	return "kognition-workflow-executor-binding-" + hex.EncodeToString(digest[:])
}

type WorkflowExecutorBindingStore interface {
	CreateWorkflowExecutorAuthority(context.Context, WorkflowExecutorAuthority) (WorkflowExecutorAuthority, error)
	UpdateWorkflowExecutorAuthority(context.Context, WorkflowExecutorAuthority) (WorkflowExecutorAuthority, error)
	GetWorkflowExecutorAuthority(context.Context, api.RuntimeApprovalReference) (WorkflowExecutorAuthority, error)
	CreateWorkflowRunAcceptance(context.Context, WorkflowRunAcceptance) (WorkflowRunAcceptance, error)
	GetWorkflowRunAcceptance(context.Context, string, string) (WorkflowRunAcceptance, error)
}

func decodeWorkflowExecutorRecord(raw []byte, target any) error {
	if len(raw) == 0 || len(raw) > WorkflowExecutorRecordMaxBytes {
		return fmt.Errorf("executor record exceeds its content bound")
	}
	_, err := CanonicalizeJSON(raw)
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return fmt.Errorf("executor record has trailing content")
	}
	return nil
}

func DecodeWorkflowExecutorAuthority(raw []byte) (WorkflowExecutorAuthority, error) {
	var authority WorkflowExecutorAuthority
	if err := decodeWorkflowExecutorRecord(raw, &authority); err != nil {
		return authority, err
	}
	return authority, ValidateWorkflowExecutorAuthority(authority)
}

func ValidateWorkflowExecutorAuthority(authority WorkflowExecutorAuthority) error {
	reference := authority.Reference
	if reference.UID == "" && reference.ResourceVersion == 0 {
		reference.UID, reference.ResourceVersion = "unpersisted-configuration", 1
	}
	if err := executorRecordReference(reference, "WorkflowExecutionBinding", authority.Workflow.Namespace); err != nil {
		return err
	}
	if err := executorRecordReference(authority.Workflow, "Workflow", authority.Reference.Namespace); err != nil {
		return err
	}
	if authority.PolicyVersion != WorkflowExecutorPolicyVersion || !validInstanceConfigurationDigest(authority.WorkflowRevisionDigest) ||
		executorRecordPrincipal(authority.Executor) != nil || executorRecordPrincipal(authority.ApprovedBy) != nil ||
		authority.Executor == authority.ApprovedBy || authority.NotBefore.IsZero() ||
		!authority.ExpiresAt.After(authority.NotBefore) || authority.ExpiresAt.Sub(authority.NotBefore) > maxApprovalTTL ||
		authority.Limits.MaxEffects < 1 || authority.Limits.MaxEffects > store.WorkflowGateMaxEffects ||
		authority.Limits.MaxPayloadBytes < 1 || authority.Limits.MaxPayloadBytes > maxExecutorPayloadBytes ||
		authority.Limits.MaxDurationSeconds < 1 || authority.Limits.MaxDurationSeconds > int64(maxApprovalTTL/time.Second) ||
		authority.Limits.MaxMembers < 1 || authority.Limits.MaxMembers > 8 ||
		authority.Limits.MaxTurns < 1 || authority.Limits.MaxTurns > 100 ||
		authority.Limits.MaxMessages < 1 || authority.Limits.MaxMessages > 500 ||
		authority.Limits.MaxDelegations < 1 || authority.Limits.MaxDelegations > 100 ||
		authority.Limits.MaxHandoffDepth < 1 || authority.Limits.MaxHandoffDepth > 5 ||
		authority.Limits.MaxParallelTasks < 1 || authority.Limits.MaxParallelTasks > authority.Limits.MaxMembers ||
		authority.Limits.ContentRetentionSeconds < 1 || authority.Limits.ContentRetentionSeconds > 7*86400 ||
		authority.Limits.ReplayRetentionSeconds != 90*86400 ||
		len(authority.Targets) == 0 || len(authority.Targets) > authority.Limits.MaxMembers || len(authority.ControllerUIDs) == 0 || len(authority.ControllerUIDs) > 16 {
		return fmt.Errorf("executor authority requires exact identities, operator attribution, and finite limits")
	}
	controllers := map[string]bool{}
	for _, controller := range authority.ControllerUIDs {
		if controller == "" || len(controller) > 128 || strings.TrimSpace(controller) != controller ||
			!utf8.ValidString(controller) || strings.ContainsAny(controller, "\x00\r\n\t /\\*") || controllers[controller] {
			return fmt.Errorf("executor authority requires unique controller incarnations")
		}
		controllers[controller] = true
	}
	targets := map[api.RuntimeApprovalReference]bool{}
	for _, target := range authority.Targets {
		if executorTargetRecordReference(target.Agent, "Agent", target.Agent.Namespace) != nil ||
			executorTargetRecordReference(target.Service, "AgentService", target.Agent.Namespace) != nil || targets[target.Agent] ||
			target.Operation != WorkflowExecutorInitiate ||
			(target.InitialDecision != api.RuntimeApprovalDecisionPending && target.InitialDecision != api.RuntimeApprovalDecisionNotRequired) {
			return fmt.Errorf("executor authority requires unique exact targets and supported initiation policies")
		}
		targets[target.Agent] = true
	}
	return nil
}

func DecodeWorkflowRunAcceptance(raw []byte) (WorkflowRunAcceptance, error) {
	var acceptance WorkflowRunAcceptance
	if err := decodeWorkflowExecutorRecord(raw, &acceptance); err != nil {
		return acceptance, err
	}
	return acceptance, ValidateWorkflowRunAcceptance(acceptance)
}

func ValidateWorkflowRunAcceptance(acceptance WorkflowRunAcceptance) error {
	if acceptance.RunUID != acceptance.Run.UID || executorRecordReference(acceptance.Run, "WorkflowRun", acceptance.Workspace) != nil ||
		executorRecordReference(acceptance.Workflow, "Workflow", acceptance.Workspace) != nil ||
		executorRecordReference(acceptance.ExecutionBinding, "WorkflowExecutionBinding", acceptance.Workspace) != nil ||
		executorRecordPrincipal(acceptance.Executor) != nil || executorRecordPrincipal(acceptance.Initiator) != nil ||
		!validInstanceConfigurationDigest(acceptance.WorkflowRevisionDigest) || !validInstanceConfigurationDigest(acceptance.BindingDigest) ||
		!validInstanceConfigurationDigest(acceptance.RequestDigest) || acceptance.AcceptedAt.IsZero() ||
		!acceptance.ExpiresAt.After(acceptance.AcceptedAt) || acceptance.ExpiresAt.Sub(acceptance.AcceptedAt) > maxApprovalTTL {
		return fmt.Errorf("run acceptance requires immutable run identity, exact scope, authenticated principals, and bounded lifetime")
	}
	return nil
}

func ValidateWorkflowRunAcceptanceAuthority(acceptance WorkflowRunAcceptance, authority WorkflowExecutorAuthority, now time.Time) error {
	if ValidateWorkflowRunAcceptance(acceptance) != nil || ValidateWorkflowExecutorAuthority(authority) != nil || now.IsZero() ||
		!authority.Enabled || acceptance.ExecutionBinding != authority.Reference || acceptance.Workflow != authority.Workflow ||
		acceptance.WorkflowRevisionDigest != authority.WorkflowRevisionDigest || acceptance.Executor != authority.Executor ||
		acceptance.AcceptedAt.Before(authority.NotBefore) || acceptance.AcceptedAt.After(now) ||
		!acceptance.ExpiresAt.After(now) || acceptance.ExpiresAt.After(authority.ExpiresAt) ||
		acceptance.ExpiresAt.Sub(acceptance.AcceptedAt) > time.Duration(authority.Limits.MaxDurationSeconds)*time.Second {
		return store.ErrConflict
	}
	return nil
}

func WorkflowExecutorSpecDigest(resource []byte) (string, error) {
	if len(resource) > WorkflowExecutorRecordMaxBytes {
		return "", fmt.Errorf("executor definition exceeds its bound")
	}
	if _, err := CanonicalizeJSON(resource); err != nil {
		return "", err
	}
	var object struct {
		Spec json.RawMessage `json:"spec"`
	}
	if err := json.Unmarshal(resource, &object); err != nil {
		return "", err
	}
	canonical, err := CanonicalizeJSON(object.Spec)
	if err != nil || bytes.Equal(canonical, []byte("null")) {
		return "", fmt.Errorf("executor definition requires a valid spec")
	}
	digest := sha256.Sum256(canonical)
	return hex.EncodeToString(digest[:]), nil
}

func ValidateWorkflowExecutorAuthorityDefinition(authority WorkflowExecutorAuthority, workflowJSON []byte) error {
	var workflow api.Workflow
	if len(workflowJSON) > WorkflowExecutorRecordMaxBytes || json.Unmarshal(workflowJSON, &workflow) != nil || !executorResourceMatches(workflow.ObjectMeta, authority.Workflow) {
		return fmt.Errorf("binding requires the exact live Workflow incarnation and revision")
	}
	digest, err := WorkflowExecutorSpecDigest(workflowJSON)
	if err != nil || digest != authority.WorkflowRevisionDigest {
		return fmt.Errorf("binding Workflow digest does not match its persisted definition")
	}
	return nil
}

func WorkflowRunExecutionScopeDigest(acceptance WorkflowRunAcceptance, authority WorkflowExecutorAuthority) (string, error) {
	encoded, err := json.Marshal(struct {
		Run           api.RuntimeApprovalReference `json:"run"`
		Authority     WorkflowExecutorAuthority    `json:"authority"`
		Initiator     api.ControlPlaneRoleSubject  `json:"initiator"`
		RequestDigest string                       `json:"requestDigest"`
	}{acceptance.Run, authority, acceptance.Initiator, acceptance.RequestDigest})
	if err != nil {
		return "", err
	}
	canonical, err := CanonicalizeJSON(encoded)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(canonical)
	return hex.EncodeToString(digest[:]), nil
}

func executorRecordReference(reference api.RuntimeApprovalReference, kind, namespace string) error {
	if reference.Scope != "Namespaced" || reference.ConfigurationDigest != "" ||
		len(validation.IsDNS1123Label(namespace)) != 0 || len(validation.IsDNS1123Subdomain(reference.Name)) != 0 ||
		!utf8.ValidString(reference.UID) || strings.TrimSpace(reference.UID) != reference.UID ||
		strings.ContainsAny(reference.UID, "\x00\r\n\t /\\*") {
		return fmt.Errorf("executor record requires an exact namespaced reference")
	}
	return validateReference(kind, kind, namespace, reference)
}

func executorTargetRecordReference(reference api.RuntimeApprovalReference, kind, namespace string) error {
	if reference.ConfigurationDigest != "" && !validInstanceConfigurationDigest(reference.ConfigurationDigest) {
		return fmt.Errorf("executor target configuration digest is invalid")
	}
	reference.ConfigurationDigest = ""
	return executorRecordReference(reference, kind, namespace)
}

func executorRecordPrincipal(principal api.ControlPlaneRoleSubject) error {
	if principal.Method == "" || len(principal.Method) > 64 || principal.Method == "anonymous" || principal.Method == "token" ||
		!utf8.ValidString(principal.Method) || !ValidDecisionActor(principal.Issuer, principal.Subject) ||
		strings.TrimSpace(principal.Method) != principal.Method || strings.TrimSpace(principal.Issuer) != principal.Issuer ||
		strings.TrimSpace(principal.Subject) != principal.Subject || strings.ContainsAny(principal.Method+principal.Issuer+principal.Subject, "\x00\r\n\t") {
		return fmt.Errorf("executor record requires a canonical authenticated principal")
	}
	return nil
}

func ValidateWorkflowRunAcceptanceDefinitions(ctx context.Context, acceptance WorkflowRunAcceptance, authority WorkflowExecutorAuthority, workflowJSON, runJSON []byte) error {
	initiator, err := AuthenticatedWorkflowExecutor(ctx)
	if err != nil || initiator != acceptance.Initiator || initiator == authority.Executor {
		return fmt.Errorf("acceptance requires its authenticated original submitter")
	}
	return ValidateWorkflowRunAcceptanceEvidenceDefinitions(ctx, acceptance, authority, workflowJSON, runJSON)
}

func ValidateWorkflowRunAcceptanceEvidenceDefinitions(ctx context.Context, acceptance WorkflowRunAcceptance, authority WorkflowExecutorAuthority, workflowJSON, runJSON []byte) error {
	if acceptance.Initiator == authority.Executor || executorRecordPrincipal(acceptance.Initiator) != nil {
		return fmt.Errorf("acceptance requires independent original submitter evidence")
	}
	var workflow api.Workflow
	var run api.WorkflowRun
	if json.Unmarshal(workflowJSON, &workflow) != nil || json.Unmarshal(runJSON, &run) != nil ||
		!executorResourceMatches(workflow.ObjectMeta, acceptance.Workflow) || !executorResourceMatches(run.ObjectMeta, acceptance.Run) ||
		workflow.DeletionTimestamp != nil || run.DeletionTimestamp != nil || run.Status.CompletionTime != nil ||
		run.Status.StartTime != nil || run.Status.SnapshotSpec != nil || run.Status.ActiveStep != "" || len(run.Status.ActiveSteps) != 0 || len(run.Status.StepStatuses) != 0 ||
		run.Status.Phase != "" && run.Status.Phase != api.PhasePending ||
		run.Spec.WorkflowRef.Name != workflow.Name || run.Annotations[WorkflowRunAcceptanceRequiredAnnotation] != "true" {
		return fmt.Errorf("acceptance requires exact live protected Workflow and run definitions")
	}
	workflowDigest, workflowErr := WorkflowExecutorSpecDigest(workflowJSON)
	runDigest, runErr := WorkflowExecutorSpecDigest(runJSON)
	scopeDigest, scopeErr := WorkflowRunExecutionScopeDigest(acceptance, authority)
	if workflowErr != nil || runErr != nil || scopeErr != nil || workflowDigest != acceptance.WorkflowRevisionDigest ||
		runDigest != acceptance.RequestDigest || scopeDigest != acceptance.BindingDigest {
		return fmt.Errorf("acceptance digests do not match the live persisted execution scope")
	}
	return ctx.Err()
}

type WorkflowRunSubmissionStore interface {
	CreateAcceptedWorkflowRun(context.Context, api.RuntimeApprovalReference, []byte, int64) (api.WorkflowRun, WorkflowRunAcceptance, error)
}

type WorkflowRunSubmissionEvidenceAuthorizer interface {
	AuthorizeAcceptance(context.Context, []byte) (WorkflowRunAcceptance, error)
}
