// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"context"
	"strings"
	"sync"
	"testing"

	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	toolscache "k8s.io/client-go/tools/cache"

	k8s "github.com/kognition-ai/kognition/api/k8s/v1alpha1"
)

func newKnowledgeBase(namespace, name, uid string) *k8s.KnowledgeBase {
	return &k8s.KnowledgeBase{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name, UID: types.UID(uid)},
	}
}

func TestKnowledgeBaseUIDIndexProbeGatesOnSync(t *testing.T) {
	index := newKnowledgeBaseUIDIndex(zap.NewNop())

	if err := index.Probe(context.Background()); status.Code(err) != codes.Unavailable {
		t.Fatalf("expected Unavailable before MarkSynced, got %v", err)
	}
	index.MarkSynced()
	if err := index.Probe(context.Background()); err != nil {
		t.Fatalf("expected nil after MarkSynced, got %v", err)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := index.Probe(cancelled); status.Code(err) != codes.Canceled {
		t.Fatalf("expected Canceled from probe with cancelled context, got %v", err)
	}
}

func TestKnowledgeBaseUIDIndexLookupHonoursSyncAndScope(t *testing.T) {
	index := newKnowledgeBaseUIDIndex(zap.NewNop())
	index.upsert(newKnowledgeBase("tenant-a", "manuals", "kb-uid"))

	if _, err := index.Lookup("tenant-a", "kb-uid"); status.Code(err) != codes.Unavailable {
		t.Fatalf("Lookup before MarkSynced must return Unavailable, got %v", err)
	}
	index.MarkSynced()

	got, err := index.Lookup("tenant-a", "kb-uid")
	if err != nil {
		t.Fatalf("Lookup after upsert returned %v", err)
	}
	if got.Namespace != "tenant-a" || got.Name != "manuals" {
		t.Fatalf("Lookup returned wrong identity: %+v", got)
	}
	if _, err := index.Lookup("tenant-a", "different-uid"); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("Lookup of unknown UID must be PermissionDenied, got %v", err)
	}
	if _, err := index.Lookup("tenant-b", "kb-uid"); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("Lookup in wrong namespace must be PermissionDenied, got %v", err)
	}
}

func TestKnowledgeBaseUIDIndexUpsertRejectsInvalidObjects(t *testing.T) {
	index := newKnowledgeBaseUIDIndex(zap.NewNop())
	index.MarkSynced()

	// Wrong type — silently ignored.
	index.upsert("not a KnowledgeBase")
	// Nil KnowledgeBase pointer — ignored.
	index.upsert((*k8s.KnowledgeBase)(nil))
	// Missing fields — each combination must be a no-op.
	index.upsert(&k8s.KnowledgeBase{ObjectMeta: metav1.ObjectMeta{Namespace: "tenant-a", Name: "manuals"}}) // no UID
	index.upsert(&k8s.KnowledgeBase{ObjectMeta: metav1.ObjectMeta{Namespace: "tenant-a", UID: "u"}})        // no Name
	index.upsert(&k8s.KnowledgeBase{ObjectMeta: metav1.ObjectMeta{Name: "manuals", UID: "u"}})              // no Namespace

	if _, err := index.Lookup("tenant-a", "u"); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("no partial entries should be indexed, got %v", err)
	}
}

func TestKnowledgeBaseUIDIndexRemovePrunesAndHandlesTombstones(t *testing.T) {
	index := newKnowledgeBaseUIDIndex(zap.NewNop())
	index.MarkSynced()
	first := newKnowledgeBase("tenant-a", "manuals", "kb-first")
	second := newKnowledgeBase("tenant-a", "runbooks", "kb-second")
	index.upsert(first)
	index.upsert(second)

	// Tombstone path (informer resync loss).
	index.remove(toolscache.DeletedFinalStateUnknown{Key: "tenant-a/manuals", Obj: first})
	if _, err := index.Lookup("tenant-a", "kb-first"); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("tombstoned entry must be gone, got %v", err)
	}
	// Sibling entry survives so the namespace bucket remains.
	if got, err := index.Lookup("tenant-a", "kb-second"); err != nil || got.Name != "runbooks" {
		t.Fatalf("sibling entry lost: got %+v err %v", got, err)
	}

	// Direct remove empties the bucket; a subsequent lookup shows the map
	// pruned to no namespace bucket (still surfaces as PermissionDenied).
	index.remove(second)
	if _, err := index.Lookup("tenant-a", "kb-second"); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("second entry must be removed, got %v", err)
	}
	index.mu.RLock()
	_, bucketExists := index.entries["tenant-a"]
	index.mu.RUnlock()
	if bucketExists {
		t.Fatalf("empty namespace bucket must be pruned")
	}

	// Remove with an untyped tombstone payload — must not panic.
	index.remove(toolscache.DeletedFinalStateUnknown{Key: "tenant-a/manuals", Obj: "not a KnowledgeBase"})
	// Remove with a completely unknown value — must not panic.
	index.remove(42)
}

func TestKnowledgeBaseUIDIndexRegisterRejectsNilManager(t *testing.T) {
	index := newKnowledgeBaseUIDIndex(zap.NewNop())
	err := index.Register(context.Background(), nil)
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "manager") {
		t.Fatalf("expected manager-required error, got %v", err)
	}
	if index.synced.Load() {
		t.Fatal("Register must not toggle synced")
	}
}

func TestKnowledgeBaseUIDIndexConcurrentAccess(t *testing.T) {
	index := newKnowledgeBaseUIDIndex(zap.NewNop())
	index.MarkSynced()
	const writers = 8
	const iterations = 200

	var wg sync.WaitGroup
	for writer := 0; writer < writers; writer++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for iter := 0; iter < iterations; iter++ {
				index.upsert(newKnowledgeBase("tenant-a", "kb", "uid"))
				_, _ = index.Lookup("tenant-a", "uid")
			}
		}()
	}
	wg.Wait()
	got, err := index.Lookup("tenant-a", "uid")
	if err != nil || got.Name != "kb" {
		t.Fatalf("concurrent access left the index inconsistent: %+v err %v", got, err)
	}
}
