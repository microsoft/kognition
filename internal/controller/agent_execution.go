// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/store"
)

// ResolveExecutionAgent loads the Agent configuration selected for an
// instance. A pinned revision supplies immutable execution configuration while
// the current Agent identity and status remain authoritative for policy and
// lifecycle checks.
func ResolveExecutionAgent(ctx context.Context, s store.Store, namespace string, instance *v1alpha1.AgentInstance) (*v1alpha1.Agent, error) {
	if instance == nil || instance.Spec.AgentRef.Name == "" {
		return nil, fmt.Errorf("agent instance must reference an Agent")
	}
	agent, err := getStoredResource[v1alpha1.Agent](ctx, s, "Agent", namespace, instance.Spec.AgentRef.Name)
	if err != nil {
		return nil, err
	}
	if instance.Spec.AgentRevision == nil {
		return agent, nil
	}
	if *instance.Spec.AgentRevision < 1 {
		return nil, fmt.Errorf("invalid Agent revision %d", *instance.Spec.AgentRevision)
	}
	revision, err := s.GetRevision(ctx, "Agent", namespace, agent.ObjectMeta.Name, *instance.Spec.AgentRevision)
	if err != nil {
		return nil, fmt.Errorf("get Agent revision %d for %q: %w", *instance.Spec.AgentRevision, agent.ObjectMeta.Name, err)
	}
	var snapshot v1alpha1.Agent
	if err := json.Unmarshal(revision.Data, &snapshot); err != nil {
		return nil, fmt.Errorf("unmarshal Agent revision %d for %q: %w", *instance.Spec.AgentRevision, agent.ObjectMeta.Name, err)
	}
	snapshot.ObjectMeta = agent.ObjectMeta
	snapshot.Status = agent.Status
	return &snapshot, nil
}
