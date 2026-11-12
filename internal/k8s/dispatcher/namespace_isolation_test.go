// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package dispatcher

import (
	"context"
	"strings"
	"testing"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"go.uber.org/zap"
)

func TestRuntimeRejectsInstallationNamespace(t *testing.T) {
	for _, override := range []string{"", "control"} {
		t.Run("override="+override, func(t *testing.T) {
			dispatcher := NewK8sDispatcher(nil, nil, zap.NewNop(), Config{ControllerNamespace: "control", AgentNamespace: override})
			agent := &v1alpha1.Agent{}
			agent.ObjectMeta.Namespace = "control"
			if err := dispatcher.EnsureAgentRuntime(context.Background(), agent); err == nil || !strings.Contains(err.Error(), "installation namespace") {
				t.Fatalf("expected placement rejection, got %v", err)
			}
			if _, err := dispatcher.EnsureAgentServiceRevisionWorkload(context.Background(), "control", "service", 1, agent); err == nil || !strings.Contains(err.Error(), "installation namespace") {
				t.Fatalf("expected revision placement rejection, got %v", err)
			}
		})
	}
}
