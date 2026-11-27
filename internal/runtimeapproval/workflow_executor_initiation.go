// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package runtimeapproval

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	api "github.com/kognition-ai/kognition/api/v1alpha1"
)

const WorkflowExecutorInitiationDomain = "kognition.dev/workflow-executor/initiation/v1"
const WorkflowExecutorInitiationFormat = "kognition-workflow-executor-initiation-v1"
const WorkflowExecutorInitiationTTL = time.Minute

type WorkflowExecutorInitiationCapability struct {
	Executor      api.ControlPlaneRoleSubject `json:"executor"`
	ControllerUID string                      `json:"controllerUID"`
	ActionDigest  string                      `json:"actionDigest"`
	IssuedAt      time.Time                   `json:"issuedAt"`
	ExpiresAt     time.Time                   `json:"expiresAt"`
	Binding       api.RuntimeActionBinding    `json:"binding"`
}

type WorkflowExecutorInitiationConfig struct {
	Keys                 BindingKeyStore
	Coordinator          *WorkflowExecutorCoordinator
	CurrentExecutor      func(context.Context) (api.ControlPlaneRoleSubject, error)
	CurrentControllerUID func(context.Context) (string, error)
	Clock                func() (time.Time, error)
}

type WorkflowExecutorInitiationService struct {
	config WorkflowExecutorInitiationConfig
}

func NewWorkflowExecutorInitiationService(config WorkflowExecutorInitiationConfig) (*WorkflowExecutorInitiationService, error) {
	if config.Keys == nil || config.Coordinator == nil || config.CurrentExecutor == nil || config.CurrentControllerUID == nil || config.Clock == nil {
		return nil, ErrWorkflowDispatchUnsupported
	}
	return &WorkflowExecutorInitiationService{config: config}, nil
}

func executorInitiationActionDigest(action WorkflowExecutorAction) (string, error) {
	encoded, err := canonicalExecutorAction(action)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}

func executorInitiationTag(key BindingKey, capability WorkflowExecutorInitiationCapability) (string, error) {
	capability.Binding.Tag = ""
	encoded, err := json.Marshal(capability)
	if err != nil {
		return "", err
	}
	canonical, err := CanonicalizeJSON(encoded)
	if err != nil {
		return "", err
	}
	mac := hmac.New(sha256.New, key.Material)
	_, _ = mac.Write([]byte(WorkflowExecutorInitiationDomain))
	_, _ = mac.Write([]byte{0})
	_, _ = mac.Write(canonical)
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), nil
}

func (service *WorkflowExecutorInitiationService) current(ctx context.Context, action WorkflowExecutorAction) (string, time.Time, error) {
	if service == nil || service.config.Keys == nil || service.config.Coordinator == nil || service.config.CurrentExecutor == nil || service.config.CurrentControllerUID == nil || service.config.Clock == nil {
		return "", time.Time{}, ErrWorkflowDispatchUnsupported
	}
	if err := ctx.Err(); err != nil {
		return "", time.Time{}, err
	}
	executor, err := service.config.CurrentExecutor(ctx)
	if err != nil || executor != action.Executor {
		return "", time.Time{}, fmt.Errorf("initiation requires its authenticated executor")
	}
	controllerUID, err := service.config.CurrentControllerUID(ctx)
	if err != nil || controllerUID == "" || len(controllerUID) > 128 {
		return "", time.Time{}, fmt.Errorf("initiation requires its verified controller incarnation")
	}
	now, err := service.config.Clock()
	if err != nil {
		return "", time.Time{}, err
	}
	if err := ValidateWorkflowExecutorAction(action, now); err != nil {
		return "", time.Time{}, err
	}
	return controllerUID, now, nil
}

func (service *WorkflowExecutorInitiationService) Issue(ctx context.Context, action WorkflowExecutorAction) (WorkflowExecutorInitiationCapability, error) {
	controllerUID, now, err := service.current(ctx, action)
	if err != nil {
		return WorkflowExecutorInitiationCapability{}, err
	}
	if _, err := service.config.Coordinator.config.Authorizer.AuthorizeWorkflowExecutorAction(ctx, action); err != nil {
		return WorkflowExecutorInitiationCapability{}, err
	}
	key, _, err := ValidatedBindingKeys(ctx, service.config.Keys)
	if err != nil {
		return WorkflowExecutorInitiationCapability{}, err
	}
	digest, err := executorInitiationActionDigest(action)
	if err != nil {
		return WorkflowExecutorInitiationCapability{}, err
	}
	expires := now.Add(WorkflowExecutorInitiationTTL).UTC()
	if expires.After(action.ExpiresAt) {
		expires = action.ExpiresAt
	}
	capability := WorkflowExecutorInitiationCapability{Executor: action.Executor, ControllerUID: controllerUID, ActionDigest: digest, IssuedAt: now.UTC(), ExpiresAt: expires,
		Binding: api.RuntimeActionBinding{Format: WorkflowExecutorInitiationFormat, Domain: WorkflowExecutorInitiationDomain, Algorithm: BindingAlgorithmV1, Epoch: key.Epoch, KeyVersion: key.Version}}
	capability.Binding.Tag, err = executorInitiationTag(key, capability)
	if err != nil {
		return WorkflowExecutorInitiationCapability{}, err
	}
	if err := service.Verify(ctx, action, capability); err != nil {
		return WorkflowExecutorInitiationCapability{}, err
	}
	return capability, nil
}

func (service *WorkflowExecutorInitiationService) Verify(ctx context.Context, action WorkflowExecutorAction, capability WorkflowExecutorInitiationCapability) error {
	controllerUID, now, err := service.current(ctx, action)
	if err != nil {
		return err
	}
	if capability.Executor != action.Executor || capability.ControllerUID != controllerUID || capability.IssuedAt.IsZero() || capability.IssuedAt.After(now) ||
		!capability.ExpiresAt.After(now) || !capability.ExpiresAt.After(capability.IssuedAt) || capability.ExpiresAt.Sub(capability.IssuedAt) > WorkflowExecutorInitiationTTL ||
		capability.ExpiresAt.After(action.ExpiresAt) || capability.Binding.Domain != WorkflowExecutorInitiationDomain || capability.Binding.Format != WorkflowExecutorInitiationFormat ||
		capability.Binding.Algorithm != BindingAlgorithmV1 || capability.Binding.Epoch < 1 || capability.Binding.KeyVersion < 1 {
		return fmt.Errorf("initiation capability has invalid scope, expiry, or authentication domain")
	}
	digest, err := executorInitiationActionDigest(action)
	if err != nil || digest != capability.ActionDigest {
		return fmt.Errorf("initiation action changed its exact signed scope")
	}
	key, err := service.config.Keys.ByVersion(ctx, capability.Binding.KeyVersion)
	if err != nil {
		return err
	}
	if key.Version != capability.Binding.KeyVersion || key.Epoch != capability.Binding.Epoch || key.Domain != BindingDomainV1 || key.Algorithm != BindingAlgorithmV1 ||
		len(key.Material) < sha256.Size || key.Status != BindingKeyActive && key.Status != BindingKeyVerifyOnly {
		return ErrBindingKeyUnavailable
	}
	tag, err := executorInitiationTag(key, capability)
	if err != nil {
		return err
	}
	if err := validateBindingTag(capability.Binding.Tag); err != nil {
		return err
	}
	if !hmac.Equal([]byte(tag), []byte(capability.Binding.Tag)) {
		return fmt.Errorf("initiation capability signature conflict")
	}
	return ctx.Err()
}

func (service *WorkflowExecutorInitiationService) boundedContext(ctx context.Context, capability WorkflowExecutorInitiationCapability) (context.Context, context.CancelFunc, error) {
	now, err := service.config.Clock()
	if err != nil {
		return nil, nil, err
	}
	if now.Before(capability.IssuedAt) || !now.Before(capability.ExpiresAt) {
		return nil, nil, fmt.Errorf("initiation capability expired or its clock moved backwards")
	}
	bounded, cancel := context.WithTimeout(ctx, capability.ExpiresAt.Sub(now))
	return bounded, cancel, nil
}

func (service *WorkflowExecutorInitiationService) Resolve(ctx context.Context, action WorkflowExecutorAction, capability WorkflowExecutorInitiationCapability) (Gate3Result, error) {
	if err := service.Verify(ctx, action, capability); err != nil {
		return Gate3Result{}, err
	}
	bounded, cancel, err := service.boundedContext(ctx, capability)
	if err != nil {
		return Gate3Result{}, err
	}
	defer cancel()
	result, err := service.config.Coordinator.Resolve(bounded, action)
	if err != nil {
		return result, err
	}
	if err := service.Verify(bounded, action, capability); err != nil {
		result.State = ""
		return result, err
	}
	return result, nil
}

func (service *WorkflowExecutorInitiationService) Dispatch(ctx context.Context, action WorkflowExecutorAction, capability WorkflowExecutorInitiationCapability, approval api.RuntimeApproval, target func(context.Context) error) (WorkflowDispatchResult, error) {
	if target == nil {
		return WorkflowDispatchResult{}, ErrWorkflowDispatchUnsupported
	}
	if err := service.Verify(ctx, action, capability); err != nil {
		return WorkflowDispatchResult{}, err
	}
	retained, err := WorkflowExecutorActionFromApproval(approval, action.Payload)
	if err != nil {
		return WorkflowDispatchResult{}, err
	}
	digest, err := executorInitiationActionDigest(retained)
	if err != nil || digest != capability.ActionDigest {
		return WorkflowDispatchResult{}, fmt.Errorf("claimed approval does not own the signed initiation")
	}
	bounded, cancel, err := service.boundedContext(ctx, capability)
	if err != nil {
		return WorkflowDispatchResult{}, err
	}
	defer cancel()
	return service.config.Coordinator.Dispatch(bounded, approval, action.Payload, func(ctx context.Context) error {
		if err := service.Verify(ctx, action, capability); err != nil {
			return err
		}
		return target(ctx)
	})
}
