// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"context"
	"testing"

	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	toolscache "k8s.io/client-go/tools/cache"

	k8s "github.com/kognition-ai/kognition/api/k8s/v1alpha1"
)

func TestKnowledgeBaseUIDIndexLifecycle(t *testing.T) {
	index := newKnowledgeBaseUIDIndex(zap.NewNop())
	if err := index.Probe(context.Background()); status.Code(err) != codes.Unavailable {
		t.Fatalf("unexpected unsynced probe: %v", err)
	}
	base := &k8s.KnowledgeBase{ObjectMeta: metav1.ObjectMeta{Namespace: "tenant-a", Name: "documents", UID: types.UID("kb-uid")}}
	index.upsert(base)
	index.MarkSynced()
	key, err := index.Lookup("tenant-a", "kb-uid")
	if err != nil || key.Namespace != "tenant-a" || key.Name != "documents" {
		t.Fatalf("unexpected lookup result: %v, %v", key, err)
	}
	index.remove(toolscache.DeletedFinalStateUnknown{Obj: base})
	if _, err := index.Lookup("tenant-a", "kb-uid"); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("unexpected removed lookup: %v", err)
	}
}

func TestKnowledgeBaseUIDIndexRejectsInvalidObjectsAndContexts(t *testing.T) {
	index := newKnowledgeBaseUIDIndex(zap.NewNop())
	index.upsert("foreign")
	index.remove("foreign")
	index.MarkSynced()
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := index.Probe(cancelled); status.Code(err) != codes.Canceled {
		t.Fatalf("unexpected cancelled probe: %v", err)
	}
}
