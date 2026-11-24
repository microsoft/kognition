package postgres

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kognition-ai/kognition/internal/store"
	"github.com/kognition-ai/kognition/internal/store/workflowactivationtest"
)

func TestPostgresWorkflowActivationStoreContract(test *testing.T) {
	workflowactivationtest.Run(test, func(test *testing.T) (store.WorkflowActivationStore, store.WorkflowActivationStore) {
		first := newTestStore(test)
		second := &Store{pool: first.pool}
		return first, second
	})
}

func TestPostgresWorkflowActivationCreateRetainsOneRecord(test *testing.T) {
	storage := newTestStore(test)
	ctx := context.Background()
	spec := store.WorkflowActivationSpec{
		Key:           store.WorkflowActivationKey{Workspace: "work", RunUID: "run", Step: "team", ActivationID: "visit", Attempt: 1},
		BindingDigest: strings.Repeat("a", 64), Actors: []string{strings.Repeat("b", 64)}, MaxEffects: 2, MaxBytes: 16384, ExpiresAt: time.Now().Add(time.Hour),
	}
	var workers sync.WaitGroup
	for range 8 {
		workers.Go(func() {
			gate, err := storage.CreateWorkflowActivation(ctx, spec)
			if err != nil || gate.Version != 1 {
				test.Errorf("identical creation: version=%d error=%v", gate.Version, err)
			}
		})
	}
	workers.Wait()
	var count int
	if err := storage.pool.QueryRow(ctx, `SELECT count(*) FROM resources WHERE kind=$1`, workflowActivationRecordKind).Scan(&count); err != nil || count != 1 {
		test.Fatalf("activation records=%d error=%v", count, err)
	}
	if _, err := storage.pool.Exec(ctx, `UPDATE resources SET deleted_at=clock_timestamp() WHERE kind=$1`, workflowActivationRecordKind); err != nil {
		test.Fatal(err)
	}
	if _, err := storage.CreateWorkflowActivation(ctx, spec); !errors.Is(err, store.ErrConflict) {
		test.Fatalf("soft-deleted activation was recreated: %v", err)
	}
}

func TestPostgresWorkflowActivationRejectsGenericWrites(test *testing.T) {
	storage := newTestStore(test)
	ctx := context.Background()
	data := []byte(`{}`)
	spec := store.WorkflowActivationSpec{
		Key:           store.WorkflowActivationKey{Workspace: "work", RunUID: "run", Step: "team", ActivationID: "visit", Attempt: 1},
		BindingDigest: strings.Repeat("a", 64), Actors: []string{strings.Repeat("b", 64)}, MaxEffects: 2, MaxBytes: 16384, ExpiresAt: time.Now().Add(time.Hour),
	}
	created, err := storage.CreateWorkflowActivation(ctx, spec)
	if err != nil {
		test.Fatal(err)
	}
	name := workflowActivationRecordName(spec.Key)
	checks := map[string]func() error{
		"create": func() error { return storage.Create(ctx, workflowActivationRecordKind, "work", name, data) },
		"create-and-get": func() error {
			_, err := storage.CreateAndGet(ctx, workflowActivationRecordKind, "work", name, data)
			return err
		},
		"update": func() error { return storage.Update(ctx, workflowActivationRecordKind, "work", name, data, 1) },
		"update-and-get": func() error {
			_, err := storage.UpdateAndGet(ctx, workflowActivationRecordKind, "work", name, data, 1)
			return err
		},
		"delete": func() error { return storage.Delete(ctx, workflowActivationRecordKind, "work", name) },
	}
	for name, check := range checks {
		test.Run(name, func(test *testing.T) {
			if err := check(); !errors.Is(err, store.ErrConflict) {
				test.Fatalf("generic write returned %v, want ErrConflict", err)
			}
		})
	}
	retained, err := storage.GetWorkflowActivation(ctx, spec.Key)
	if err != nil || retained.Version != created.Version || retained.Spec.BindingDigest != created.Spec.BindingDigest {
		test.Fatalf("retained activation = %+v, error = %v", retained, err)
	}
}
