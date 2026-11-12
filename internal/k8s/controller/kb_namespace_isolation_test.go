// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"strings"
	"testing"

	api "github.com/kognition-ai/kognition/api/v1alpha1"
	"go.uber.org/zap"
)

func TestKBIngestionRejectsInstallationNamespace(t *testing.T) {
	launcher := NewKBIngesterLauncher(nil, nil, zap.NewNop(), "ingester:test", "", "control")
	kb := &api.KnowledgeBase{}
	kb.ObjectMeta.Namespace = "control"
	if _, _, err := launcher.Ensure(context.Background(), kb, nil); err == nil || !strings.Contains(err.Error(), "installation namespace") {
		t.Fatalf("expected ingestion placement rejection, got %v", err)
	}
	if _, _, _, err := launcher.LaunchRun(context.Background(), kb, nil, &api.KnowledgeBaseIndexRun{}); err == nil || !strings.Contains(err.Error(), "installation namespace") {
		t.Fatalf("expected index run placement rejection, got %v", err)
	}
}
