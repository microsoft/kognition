// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package runtimeapproval

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	api "github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/store"
	"k8s.io/apimachinery/pkg/util/validation"
)

const (
	WorkflowExecutorBindingFormat = "kognition-workflow-executor-action-v1"
	WorkflowExecutorBindingDomain = "kognition.dev/workflow-executor/action/v1"
	WorkflowExecutorInitiate      = "initiate"
	maxExecutorPayloadBytes       = 1 << 20
)

type WorkflowExecutorAction struct {
	Executor               api.ControlPlaneRoleSubject  `json:"executor"`
	Initiator              api.ControlPlaneRoleSubject  `json:"initiator"`
	Workflow               api.RuntimeApprovalReference `json:"workflow"`
	WorkflowRevisionDigest string                       `json:"workflowRevisionDigest"`
	ExecutionBinding       api.RuntimeApprovalReference `json:"executionBinding"`
	Target                 api.RuntimeApprovalReference `json:"target"`
	Service                api.RuntimeApprovalReference `json:"service"`
	Activation             store.WorkflowActivationKey  `json:"activation"`
	OwnerEpoch             int64                        `json:"ownerEpoch"`
	BindingDigest          string                       `json:"bindingDigest"`
	InvocationID           string                       `json:"invocationID"`
	Operation              string                       `json:"operation"`
	ExpiresAt              time.Time                    `json:"expiresAt"`
	Payload                json.RawMessage              `json:"payload"`
}

func ValidateWorkflowExecutorAction(action WorkflowExecutorAction, now time.Time) error {
	if now.IsZero() || action.ExpiresAt.IsZero() || !action.ExpiresAt.After(now) || action.ExpiresAt.Sub(now) > maxApprovalTTL {
		return fmt.Errorf("executor action requires a live expiry bounded to 24h")
	}
	if err := validateExecutorActionIdentity(action); err != nil {
		return err
	}
	if len(action.Payload) == 0 || len(action.Payload) > maxExecutorPayloadBytes {
		return fmt.Errorf("executor action payload must be bounded to 1 MiB")
	}
	if _, err := CanonicalizeJSON(action.Payload); err != nil {
		return fmt.Errorf("canonicalize executor action payload: %w", err)
	}
	return nil
}

func validateExecutorActionIdentity(action WorkflowExecutorAction) error {
	for _, principal := range []api.ControlPlaneRoleSubject{action.Executor, action.Initiator} {
		if principal.Method == "" || len(principal.Method) > 64 || principal.Method == "anonymous" || principal.Method == "token" ||
			!utf8.ValidString(principal.Method) || !utf8.ValidString(principal.Issuer) || !utf8.ValidString(principal.Subject) ||
			strings.TrimSpace(principal.Method) != principal.Method || !ValidDecisionActor(principal.Issuer, principal.Subject) ||
			strings.TrimSpace(principal.Issuer) != principal.Issuer || strings.TrimSpace(principal.Subject) != principal.Subject ||
			strings.ContainsAny(principal.Method+principal.Issuer+principal.Subject, "\x00\r\n\t") {
			return fmt.Errorf("executor action requires canonical non-bootstrap executor and initiator principals")
		}
	}
	if !store.ValidWorkflowActivationKey(action.Activation) || action.OwnerEpoch < 1 ||
		len(validation.IsDNS1123Label(action.Activation.Workspace)) != 0 ||
		action.InvocationID == "" || len(action.InvocationID) > 128 || strings.TrimSpace(action.InvocationID) != action.InvocationID ||
		!utf8.ValidString(action.InvocationID) ||
		strings.ContainsAny(action.InvocationID, "\x00\r\n\t") || action.Operation != WorkflowExecutorInitiate ||
		!validInstanceConfigurationDigest(action.WorkflowRevisionDigest) || !validInstanceConfigurationDigest(action.BindingDigest) {
		return fmt.Errorf("executor action requires exact activation, owner epoch, invocation, operation, and Workflow revision")
	}
	for _, reference := range []struct {
		name      string
		kind      string
		namespace string
		value     api.RuntimeApprovalReference
	}{
		{"workflow", "Workflow", action.Activation.Workspace, action.Workflow},
		{"executionBinding", "WorkflowExecutionBinding", action.Activation.Workspace, action.ExecutionBinding},
		{"target", "Agent", action.Target.Namespace, action.Target},
		{"service", "AgentService", action.Target.Namespace, action.Service},
	} {
		if reference.value.Scope != "Namespaced" || reference.value.ConfigurationDigest != "" &&
			(reference.name != "target" && reference.name != "service" || !validInstanceConfigurationDigest(reference.value.ConfigurationDigest)) ||
			!utf8.ValidString(reference.value.UID) ||
			len(validation.IsDNS1123Label(reference.namespace)) != 0 ||
			len(validation.IsDNS1123Subdomain(reference.value.Name)) != 0 ||
			strings.ContainsAny(reference.value.UID, "\x00\r\n\t /\\*") ||
			strings.TrimSpace(reference.value.UID) != reference.value.UID {
			return fmt.Errorf("executor action %s must identify an exact namespaced incarnation", reference.name)
		}
		if err := validateReference(reference.name, reference.kind, reference.namespace, reference.value); err != nil {
			return err
		}
	}
	return nil
}

func BindWorkflowExecutorAction(ctx context.Context, keys BindingKeyStore, action WorkflowExecutorAction, now time.Time) (api.RuntimeActionBinding, error) {
	if err := ValidateWorkflowExecutorAction(action, now); err != nil {
		return api.RuntimeActionBinding{}, err
	}
	key, _, err := ValidatedBindingKeys(ctx, keys)
	if err != nil {
		return api.RuntimeActionBinding{}, err
	}
	encoded, err := canonicalExecutorAction(action)
	if err != nil {
		return api.RuntimeActionBinding{}, err
	}
	return api.RuntimeActionBinding{
		Format: WorkflowExecutorBindingFormat, Algorithm: BindingAlgorithmV1, Domain: WorkflowExecutorBindingDomain,
		Epoch: key.Epoch, KeyVersion: key.Version, Tag: executorActionTag(key, encoded),
	}, nil
}

func VerifyWorkflowExecutorAction(ctx context.Context, keys BindingKeyStore, action WorkflowExecutorAction, binding api.RuntimeActionBinding) error {
	if binding.Format != WorkflowExecutorBindingFormat || binding.Domain != WorkflowExecutorBindingDomain ||
		binding.Algorithm != BindingAlgorithmV1 || binding.Epoch < 1 || binding.KeyVersion < 1 || keys == nil {
		return fmt.Errorf("unsupported executor action binding")
	}
	if err := validateExecutorActionIdentity(action); err != nil {
		return err
	}
	if action.ExpiresAt.IsZero() || len(action.Payload) == 0 || len(action.Payload) > maxExecutorPayloadBytes {
		return fmt.Errorf("executor action requires a bounded payload and retained expiry")
	}
	key, err := keys.ByVersion(ctx, binding.KeyVersion)
	if err != nil {
		return fmt.Errorf("load executor action binding key: %w", err)
	}
	if key.Version != binding.KeyVersion || key.Epoch != binding.Epoch || key.Domain != BindingDomainV1 ||
		key.Algorithm != BindingAlgorithmV1 || len(key.Material) < sha256.Size ||
		(key.Status != BindingKeyActive && key.Status != BindingKeyVerifyOnly) {
		return ErrBindingKeyUnavailable
	}
	encoded, err := canonicalExecutorAction(action)
	if err != nil {
		return err
	}
	if err := validateBindingTag(binding.Tag); err != nil {
		return err
	}
	if !hmac.Equal([]byte(binding.Tag), []byte(executorActionTag(key, encoded))) {
		return fmt.Errorf("executor action binding conflict")
	}
	return nil
}

func canonicalExecutorAction(action WorkflowExecutorAction) ([]byte, error) {
	payload, err := CanonicalizeJSON(action.Payload)
	if err != nil {
		return nil, err
	}
	action.Payload = payload
	action.ExpiresAt = action.ExpiresAt.UTC().Truncate(time.Second)
	encoded, err := json.Marshal(struct {
		Format string `json:"format"`
		WorkflowExecutorAction
	}{WorkflowExecutorBindingFormat, action})
	if err != nil {
		return nil, err
	}
	return CanonicalizeJSON(encoded)
}

func executorActionTag(key BindingKey, encoded []byte) string {
	mac := hmac.New(sha256.New, key.Material)
	_, _ = mac.Write([]byte(WorkflowExecutorBindingDomain))
	_, _ = mac.Write([]byte{0})
	_, _ = mac.Write(encoded)
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func WorkflowExecutorActionFromApproval(approval api.RuntimeApproval, payload json.RawMessage) (WorkflowExecutorAction, error) {
	reference := approval.Spec.ExecutorAction
	if approval.Spec.ActorKind != api.RuntimeApprovalActorExecutor || reference == nil || approval.Spec.WorkflowContext == nil {
		return WorkflowExecutorAction{}, fmt.Errorf("executor approval requires its typed actor and activation binding")
	}
	if approval.Spec.AgentRef != (api.RuntimeApprovalReference{}) || approval.Spec.AgentIdentityRef != (api.RuntimeApprovalReference{}) ||
		approval.Spec.AgentInstanceRef != (api.RuntimeApprovalReference{}) || approval.Spec.AutonomyClassRef != (api.RuntimeApprovalReference{}) ||
		approval.Spec.TrustProfileRef != nil || approval.Spec.SessionRef != "" || len(approval.Spec.AuthorizationRefs) != 0 ||
		approval.Spec.Plane != api.RuntimeApprovalPlaneWorkflow || approval.Spec.Operation != WorkflowExecutorInitiate ||
		approval.Spec.Workspace != reference.Context.Workspace || *approval.Spec.WorkflowContext != reference.Context ||
		approval.Spec.TargetRef != reference.Service {
		return WorkflowExecutorAction{}, fmt.Errorf("executor approval cannot substitute Agent fields or change its exact action context")
	}
	return WorkflowExecutorAction{Executor: reference.Executor, Initiator: reference.Initiator, Workflow: reference.Workflow,
		WorkflowRevisionDigest: reference.WorkflowRevisionDigest, ExecutionBinding: reference.ExecutionBinding,
		Target: reference.Target, Service: reference.Service, Activation: workflowContextKey(reference.Context),
		OwnerEpoch: reference.Context.OwnerEpoch, BindingDigest: reference.Context.BindingDigest,
		InvocationID: approval.Spec.InvocationID, Operation: approval.Spec.Operation, ExpiresAt: reference.ExpiresAt,
		Payload: append(json.RawMessage(nil), payload...)}, nil
}

func BuildWorkflowExecutorApproval(ctx context.Context, keys BindingKeyStore, action WorkflowExecutorAction, initialDecision string, now time.Time) (api.RuntimeApproval, error) {
	action.ExpiresAt = action.ExpiresAt.UTC().Truncate(time.Second)
	status := InitialStatus(initialDecision)
	if err := ValidateInitialStatus(status); err != nil {
		return api.RuntimeApproval{}, err
	}
	binding, err := BindWorkflowExecutorAction(ctx, keys, action, now)
	if err != nil {
		return api.RuntimeApproval{}, err
	}
	context := api.WorkflowActionContext{Workspace: action.Activation.Workspace, RunUID: action.Activation.RunUID,
		Step: action.Activation.Step, ActivationID: action.Activation.ActivationID, Attempt: action.Activation.Attempt,
		OwnerEpoch: action.OwnerEpoch, BindingDigest: action.BindingDigest}
	return api.RuntimeApproval{ObjectMeta: api.ObjectMeta{Name: CanonicalName(context.Workspace, action.InvocationID), Namespace: context.Workspace},
		Spec: api.RuntimeApprovalSpec{ActorKind: api.RuntimeApprovalActorExecutor, Workspace: context.Workspace,
			InvocationID: action.InvocationID, InitialDecision: initialDecision, Plane: api.RuntimeApprovalPlaneWorkflow,
			Operation: action.Operation, TargetRef: action.Service, WorkflowContext: &context, Binding: binding,
			ExecutorAction: &api.WorkflowExecutorActionReference{Executor: action.Executor, Initiator: action.Initiator,
				Workflow: action.Workflow, WorkflowRevisionDigest: action.WorkflowRevisionDigest, ExecutionBinding: action.ExecutionBinding,
				Target: action.Target, Service: action.Service, Context: context, ExpiresAt: action.ExpiresAt.UTC()}}, Status: status}, nil
}

func RuntimeApprovalDeadline(approval api.RuntimeApproval, now time.Time, ttl time.Duration) (time.Time, error) {
	deadline := now.Add(ttl)
	if approval.Spec.ActorKind == api.RuntimeApprovalActorExecutor {
		if approval.Spec.ExecutorAction == nil || !approval.Spec.ExecutorAction.ExpiresAt.After(now) {
			return time.Time{}, fmt.Errorf("executor authority expired before approval creation")
		}
		if approval.Spec.ExecutorAction.ExpiresAt.Before(deadline) {
			deadline = approval.Spec.ExecutorAction.ExpiresAt
		}
	}
	return deadline.UTC().Truncate(time.Microsecond), nil
}
