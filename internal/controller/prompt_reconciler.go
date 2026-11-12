// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"go.uber.org/zap"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/lifecycle"
	"github.com/kognition-ai/kognition/internal/requestid"
	"github.com/kognition-ai/kognition/internal/store"
)

// PromptReconciler reconciles Prompt resources by tracking hash/version
// and propagating prompt drift to referencing agents.
type PromptReconciler struct {
	store  store.Store
	logger *zap.Logger
}

// NewPromptReconciler creates a new PromptReconciler.
func NewPromptReconciler(s store.Store, logger *zap.Logger) *PromptReconciler {
	return &PromptReconciler{
		store:  s,
		logger: logger.With(zap.String("reconciler", "prompt")),
	}
}

// Reconcile implements the Reconciler interface for Prompt resources.
func (r *PromptReconciler) Reconcile(ctx context.Context, req Request) (Result, error) {
	log := r.logger.With(zap.String("namespace", req.Namespace), zap.String("name", req.Name), requestid.ZapField(ctx))

	raw, err := r.store.Get(ctx, "Prompt", req.Namespace, req.Name)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			log.Debug("prompt not found, likely deleted")
			return Result{}, nil
		}
		return Result{}, fmt.Errorf("get prompt: %w", err)
	}

	var prompt v1alpha1.Prompt
	if err := json.Unmarshal(raw, &prompt); err != nil {
		return Result{}, fmt.Errorf("unmarshal prompt: %w", err)
	}

	hash := promptContentHash(prompt.Spec.ResolvedContent())
	version, _, err := r.ensureRevision(ctx, &prompt)
	if err != nil {
		return Result{}, fmt.Errorf("ensure prompt revision: %w", err)
	}

	statusChanged := prompt.Status.Hash != hash || prompt.Status.Version != version
	if statusChanged {
		prompt.Status.Hash = hash
		prompt.Status.Version = version
		if err := r.updatePrompt(ctx, &prompt); err != nil {
			return Result{}, fmt.Errorf("update prompt status: %w", err)
		}
	}

	// Propagate every established update until all referencing agents reflect it.
	// This retries partial failures after the prompt revision is already durable.
	if version > 1 {
		if err := r.markReferencingAgentsDrifted(ctx, &prompt); err != nil {
			return Result{}, fmt.Errorf("mark agents drifted: %w", err)
		}
	}

	return Result{}, nil
}

func (r *PromptReconciler) ensureRevision(ctx context.Context, prompt *v1alpha1.Prompt) (int64, bool, error) {
	specBytes, err := json.Marshal(prompt.Spec)
	if err != nil {
		return 0, false, fmt.Errorf("marshal prompt spec: %w", err)
	}

	revisions, err := r.store.ListRevisions(ctx, "Prompt", prompt.ObjectMeta.Namespace, prompt.ObjectMeta.Name)
	if err != nil {
		return 0, false, fmt.Errorf("list prompt revisions: %w", err)
	}

	newRevision := int64(1)
	if len(revisions) > 0 {
		latest := revisions[len(revisions)-1]
		if jsonBytesEqual(latest.Spec, specBytes) {
			return latest.Revision, false, nil
		}
		newRevision = latest.Revision + 1
	}

	promptBytes, err := json.Marshal(prompt)
	if err != nil {
		return 0, false, fmt.Errorf("marshal prompt: %w", err)
	}

	if err := r.store.CreateRevision(ctx, "Prompt", prompt.ObjectMeta.Namespace, prompt.ObjectMeta.Name, newRevision, specBytes, promptBytes); err != nil {
		return 0, false, fmt.Errorf("create prompt revision %d: %w", newRevision, err)
	}

	return newRevision, true, nil
}

func (r *PromptReconciler) markReferencingAgentsDrifted(ctx context.Context, prompt *v1alpha1.Prompt) error {
	rawAgents, err := r.store.List(ctx, "Agent", store.ListOptions{Namespace: prompt.ObjectMeta.Namespace})
	if err != nil {
		return fmt.Errorf("list agents: %w", err)
	}

	for _, raw := range rawAgents {
		var agent v1alpha1.Agent
		if err := json.Unmarshal(raw, &agent); err != nil {
			continue
		}
		if agent.Spec.Instructions.Ref == nil || agent.Spec.Instructions.Ref.Name != prompt.ObjectMeta.Name {
			continue
		}

		lc := lifecycle.SetCondition(
			apiToLifecycleConditions(agent.Status.Conditions),
			"DriftDetected",
			lifecycle.ConditionTrue,
			"PromptChanged",
			fmt.Sprintf("prompt %s changed to version %d", prompt.ObjectMeta.Name, prompt.Status.Version),
		)
		desiredConditions := lifecycleToAPIConditions(lc)
		if conditionsMatch(
			restartLadderCondition(agent.Status.Conditions, "DriftDetected"),
			restartLadderCondition(desiredConditions, "DriftDetected"),
		) {
			continue
		}
		agent.Status.Conditions = desiredConditions

		if err := r.updateAgentWithRetry(ctx, &agent); err != nil {
			return fmt.Errorf("update drift condition for agent %s: %w", agent.ObjectMeta.Name, err)
		}
	}

	return nil
}

func (r *PromptReconciler) updatePrompt(ctx context.Context, prompt *v1alpha1.Prompt) error {
	const maxRetries = 5
	for attempt := 0; attempt < maxRetries; attempt++ {
		data, err := json.Marshal(prompt)
		if err != nil {
			return fmt.Errorf("marshal prompt: %w", err)
		}
		err = r.store.Update(ctx, "Prompt", prompt.ObjectMeta.Namespace, prompt.ObjectMeta.Name, data, prompt.ObjectMeta.ResourceVersion)
		if err == nil {
			return nil
		}
		if !errors.Is(err, store.ErrConflict) {
			return err
		}

		raw, getErr := r.store.Get(ctx, "Prompt", prompt.ObjectMeta.Namespace, prompt.ObjectMeta.Name)
		if getErr != nil {
			return fmt.Errorf("re-read prompt after conflict: %w", getErr)
		}
		var latest v1alpha1.Prompt
		if getErr := json.Unmarshal(raw, &latest); getErr != nil {
			return fmt.Errorf("unmarshal prompt after conflict: %w", getErr)
		}
		latest.Status.Hash = prompt.Status.Hash
		latest.Status.Version = prompt.Status.Version
		*prompt = latest
	}
	return store.ErrConflict
}

func promptContentHash(content string) string {
	sum := sha256.Sum256([]byte(content))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func (r *PromptReconciler) updateAgentWithRetry(ctx context.Context, agent *v1alpha1.Agent) error {
	const maxRetries = 5
	for attempt := 0; attempt < maxRetries; attempt++ {
		data, err := json.Marshal(agent)
		if err != nil {
			return fmt.Errorf("marshal agent: %w", err)
		}
		err = r.store.Update(ctx, "Agent", agent.ObjectMeta.Namespace, agent.ObjectMeta.Name, data, agent.ObjectMeta.ResourceVersion)
		if err == nil {
			return nil
		}
		if !errors.Is(err, store.ErrConflict) {
			return err
		}
		raw, getErr := r.store.Get(ctx, "Agent", agent.ObjectMeta.Namespace, agent.ObjectMeta.Name)
		if getErr != nil {
			return fmt.Errorf("re-read after conflict: %w", getErr)
		}
		var latest v1alpha1.Agent
		if getErr := json.Unmarshal(raw, &latest); getErr != nil {
			return fmt.Errorf("unmarshal after conflict: %w", getErr)
		}
		latest.Status.Conditions = mergeAgentCondition(latest.Status.Conditions, agent.Status.Conditions, "DriftDetected")
		*agent = latest
	}
	return store.ErrConflict
}
