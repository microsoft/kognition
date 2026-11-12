// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"

	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/apimachinery/pkg/types"
	toolscache "k8s.io/client-go/tools/cache"
	ctrl "sigs.k8s.io/controller-runtime"

	k8s "github.com/kognition-ai/kognition/api/k8s/v1alpha1"
)

type knowledgeBaseUIDIndex struct {
	logger  *zap.Logger
	mu      sync.RWMutex
	entries map[string]map[string]string
	synced  atomic.Bool
}

func newKnowledgeBaseUIDIndex(logger *zap.Logger) *knowledgeBaseUIDIndex {
	return &knowledgeBaseUIDIndex{logger: logger, entries: make(map[string]map[string]string)}
}

func (index *knowledgeBaseUIDIndex) Register(ctx context.Context, manager ctrl.Manager) error {
	if manager == nil {
		return fmt.Errorf("controller-runtime manager is required")
	}
	informer, err := manager.GetCache().GetInformer(ctx, &k8s.KnowledgeBase{})
	if err != nil {
		return fmt.Errorf("obtain KnowledgeBase informer: %w", err)
	}
	if _, err := informer.AddEventHandler(toolscache.ResourceEventHandlerFuncs{
		AddFunc: func(obj any) { index.upsert(obj) }, UpdateFunc: func(_, obj any) { index.upsert(obj) }, DeleteFunc: index.remove,
	}); err != nil {
		return fmt.Errorf("register KnowledgeBase event handler: %w", err)
	}
	return nil
}

func (index *knowledgeBaseUIDIndex) MarkSynced() { index.synced.Store(true) }

func (index *knowledgeBaseUIDIndex) Probe(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return status.FromContextError(err).Err()
	}
	if !index.synced.Load() {
		return status.Error(codes.Unavailable, "KnowledgeBase informer has not synced")
	}
	return nil
}

func (index *knowledgeBaseUIDIndex) Lookup(namespace, uid string) (types.NamespacedName, error) {
	if !index.synced.Load() {
		return types.NamespacedName{}, status.Error(codes.Unavailable, "KnowledgeBase informer has not synced")
	}
	index.mu.RLock()
	defer index.mu.RUnlock()
	if bucket, found := index.entries[namespace]; found {
		if name, found := bucket[uid]; found {
			return types.NamespacedName{Namespace: namespace, Name: name}, nil
		}
	}
	return types.NamespacedName{}, status.Error(codes.PermissionDenied, "knowledge base does not match committed binding")
}

func (index *knowledgeBaseUIDIndex) upsert(obj any) {
	base, ok := obj.(*k8s.KnowledgeBase)
	if !ok || base == nil || base.UID == "" || base.Namespace == "" || base.Name == "" {
		return
	}
	index.mu.Lock()
	defer index.mu.Unlock()
	bucket := index.entries[base.Namespace]
	if bucket == nil {
		bucket = make(map[string]string)
		index.entries[base.Namespace] = bucket
	}
	bucket[string(base.UID)] = base.Name
}

func (index *knowledgeBaseUIDIndex) remove(obj any) {
	base, ok := obj.(*k8s.KnowledgeBase)
	if !ok {
		tombstone, ok := obj.(toolscache.DeletedFinalStateUnknown)
		if !ok {
			return
		}
		base, ok = tombstone.Obj.(*k8s.KnowledgeBase)
		if !ok || base == nil {
			return
		}
	}
	if base.UID == "" || base.Namespace == "" {
		return
	}
	index.mu.Lock()
	defer index.mu.Unlock()
	if bucket, found := index.entries[base.Namespace]; found {
		delete(bucket, string(base.UID))
		if len(bucket) == 0 {
			delete(index.entries, base.Namespace)
		}
	}
}
