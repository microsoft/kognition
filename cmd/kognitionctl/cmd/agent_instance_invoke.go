// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

type interactiveAgentResult struct {
	SessionID       string
	Output          string
	TokenUsage      map[string]any
	ToolCalls       int64
	ToolCallsDetail []map[string]any
}

func invokeInteractiveAgentK8s(ctx context.Context, namespace, agentName, message, conversationID, createdBy string) (interactiveAgentResult, error) {
	revision, err := interactiveAgentRevision(namespace, agentName)
	if err != nil {
		return interactiveAgentResult{}, explainChatForbidden(err, namespace, "get", "agents", "read the Agent it will talk to")
	}
	instanceName := interactiveAgentInstanceName(agentName, createdBy)
	sessionContext := map[string]any{"input": message}
	if conversationID != "" {
		sessionContext["conversation_id"] = conversationID
	}
	obj := map[string]any{
		"apiVersion": "kognition.dev/v1alpha1",
		"kind":       "AgentInstance",
		"metadata": map[string]any{
			"name":      instanceName,
			"namespace": namespace,
			"labels": map[string]any{
				"kognition.dev/agent":      agentName,
				"kognition.dev/created-by": createdBy,
			},
		},
		"spec": map[string]any{
			"agentRef":       map[string]any{"name": agentName},
			"revision":       revision,
			"sessionContext": sessionContext,
		},
	}
	if _, err := k8sCreate(namespace, obj); err != nil {
		return interactiveAgentResult{}, explainChatForbidden(fmt.Errorf("create AgentInstance: %w", err), namespace, "create", "agentinstances", "start the conversation")
	}

	deadline := time.Now().Add(5 * time.Minute)
	for {
		select {
		case <-ctx.Done():
			return interactiveAgentResult{}, ctx.Err()
		case <-time.After(400 * time.Millisecond):
		}

		raw, err := k8sGet(namespace, "agentinstances", instanceName)
		if err != nil {
			if apierrors.IsForbidden(err) {
				return interactiveAgentResult{}, explainChatForbidden(fmt.Errorf("get AgentInstance status: %w", err), namespace, "get", "agentinstances", "read the answer")
			}
			if time.Now().After(deadline) {
				return interactiveAgentResult{}, fmt.Errorf("get AgentInstance status: %w", err)
			}
			continue
		}
		var instance struct {
			Status struct {
				Phase           string           `json:"phase"`
				SessionID       string           `json:"sessionID"`
				Output          string           `json:"output"`
				TokenUsage      map[string]any   `json:"tokenUsage"`
				ToolCalls       int64            `json:"toolCalls"`
				ToolCallsDetail []map[string]any `json:"toolCallsDetail"`
				Conditions      []struct {
					Type    string `json:"type"`
					Status  string `json:"status"`
					Reason  string `json:"reason"`
					Message string `json:"message"`
				} `json:"conditions"`
			} `json:"status"`
		}
		if err := json.Unmarshal(raw, &instance); err != nil {
			return interactiveAgentResult{}, fmt.Errorf("decode AgentInstance status: %w", err)
		}
		switch strings.ToLower(instance.Status.Phase) {
		case "pending":
			for _, condition := range instance.Status.Conditions {
				if condition.Type == "Ready" && condition.Status == "False" && condition.Reason == "ModelAccessDenied" {
					message := strings.TrimSpace(condition.Message)
					if message == "" {
						message = "model access denied"
					}
					return interactiveAgentResult{}, fmt.Errorf("%s", message)
				}
			}
		case "ready":
			return interactiveAgentResult{
				SessionID:       instance.Status.SessionID,
				Output:          instance.Status.Output,
				TokenUsage:      instance.Status.TokenUsage,
				ToolCalls:       instance.Status.ToolCalls,
				ToolCallsDetail: instance.Status.ToolCallsDetail,
			}, nil
		case "failed":
			message := "agent run failed"
			for index := len(instance.Status.Conditions) - 1; index >= 0; index-- {
				if strings.TrimSpace(instance.Status.Conditions[index].Message) != "" {
					message = instance.Status.Conditions[index].Message
					break
				}
			}
			return interactiveAgentResult{}, fmt.Errorf("%s", message)
		}
		if time.Now().After(deadline) {
			return interactiveAgentResult{}, fmt.Errorf("timed out waiting for agent response")
		}
	}
}

// explainChatForbidden turns a Forbidden failure on the interactive chat path into
// a message that names the missing permission, why chat needs it and the documented
// limitation. The original error is kept for errors.Is and apierrors checks. Other
// errors are returned unchanged. No credential is read or printed.
func explainChatForbidden(err error, namespace, verb, resource, purpose string) error {
	if err == nil || !apierrors.IsForbidden(err) {
		return err
	}
	return fmt.Errorf("you are not allowed to %s %s in namespace %q, which chat needs to %s. "+
		"Chat creates an AgentInstance for the Agent and polls it, and no shipped Kognition role grants that yet (ADR-0031, open decision OD-1); "+
		"ask your platform administrator for access. Check with: kognitionctl auth can-i %s %s -n %s: %w",
		verb, resource, namespace, purpose, verb, resource, namespace, err)
}

func interactiveAgentRevision(namespace, agentName string) (int64, error) {
	raw, err := k8sGet(namespace, "agents", agentName)
	if err != nil {
		return 0, fmt.Errorf("get Agent %q: %w", agentName, err)
	}
	var agent struct {
		Status struct {
			RevisionCount int64 `json:"revisionCount"`
		} `json:"status"`
	}
	if err := json.Unmarshal(raw, &agent); err != nil {
		return 0, fmt.Errorf("decode Agent %q: %w", agentName, err)
	}
	if agent.Status.RevisionCount < 1 {
		return 1, nil
	}
	return agent.Status.RevisionCount, nil
}

func interactiveAgentInstanceName(agentName, createdBy string) string {
	prefix := strings.ToLower(agentName + "-" + createdBy)
	if len(prefix) > 200 {
		prefix = prefix[:200]
	}
	return fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano())
}
