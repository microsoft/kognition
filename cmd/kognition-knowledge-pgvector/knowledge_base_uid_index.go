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

// knowledgeBaseUIDIndex maps (namespace, KnowledgeBase UID) → KnowledgeBase name
// using the controller-runtime shared cache. It exists because the KnowledgeScope
// carries the KnowledgeBase UID but not its name, and every query must be
// authorized against the committed BackendBinding on the live KnowledgeBase.
type knowledgeBaseUIDIndex struct {
	logger  *zap.Logger
	mu      sync.RWMutex
	entries map[string]map[string]string
	synced  atomic.Bool
}

func newKnowledgeBaseUIDIndex(logger *zap.Logger) *knowledgeBaseUIDIndex {
	return &knowledgeBaseUIDIndex{
		logger:  logger,
		entries: make(map[string]map[string]string),
	}
}

// Register attaches an event handler to the KnowledgeBase informer so the index
// stays in sync with the cache. It must be called before mgr.Start.
func (index *knowledgeBaseUIDIndex) Register(ctx context.Context, mgr ctrl.Manager) error {
	if mgr == nil {
		return fmt.Errorf("controller-runtime manager is required")
	}
	informer, err := mgr.GetCache().GetInformer(ctx, &k8s.KnowledgeBase{})
	if err != nil {
		return fmt.Errorf("obtain KnowledgeBase informer: %w", err)
	}
	if _, err := informer.AddEventHandler(toolscache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj any) { index.upsert(obj) },
		UpdateFunc: func(_, obj any) { index.upsert(obj) },
		DeleteFunc: index.remove,
	}); err != nil {
		return fmt.Errorf("register KnowledgeBase event handler: %w", err)
	}
	return nil
}

// MarkSynced records that the shared cache has completed its initial sync. The
// caller is responsible for invoking WaitForCacheSync before calling this.
func (index *knowledgeBaseUIDIndex) MarkSynced() {
	index.synced.Store(true)
}

// Probe is used by both the pluginsdk knowledge server and the admin /readyz
// endpoint. It returns a gRPC status error so pluginsdk can surface it directly.
func (index *knowledgeBaseUIDIndex) Probe(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return status.FromContextError(err).Err()
	}
	if !index.synced.Load() {
		return status.Error(codes.Unavailable, "KnowledgeBase informer has not synced")
	}
	return nil
}

// Lookup resolves a (namespace, UID) pair back to the live KnowledgeBase's
// namespaced name. Callers must treat missing entries as an authorization
// failure: the scope refers to a KnowledgeBase that no longer exists (or never
// did) in the watched namespaces.
func (index *knowledgeBaseUIDIndex) Lookup(namespace, uid string) (types.NamespacedName, error) {
	if !index.synced.Load() {
		return types.NamespacedName{}, status.Error(codes.Unavailable, "KnowledgeBase informer has not synced")
	}
	index.mu.RLock()
	defer index.mu.RUnlock()
	if bucket, ok := index.entries[namespace]; ok {
		if name, ok := bucket[uid]; ok {
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
	namespace, uid, name := base.Namespace, string(base.UID), base.Name
	index.mu.Lock()
	defer index.mu.Unlock()
	bucket := index.entries[namespace]
	if bucket == nil {
		bucket = make(map[string]string)
		index.entries[namespace] = bucket
	}
	bucket[uid] = name
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
	namespace, uid := base.Namespace, string(base.UID)
	index.mu.Lock()
	defer index.mu.Unlock()
	if bucket, ok := index.entries[namespace]; ok {
		delete(bucket, uid)
		if len(bucket) == 0 {
			delete(index.entries, namespace)
		}
	}
}
