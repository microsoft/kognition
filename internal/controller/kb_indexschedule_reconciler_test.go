// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"go.uber.org/zap/zaptest"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/store"
)

type concurrentIndexScheduleStatusStore struct {
	*memStore
	conflicted bool
	runCreates int
}

type persistentIndexScheduleStatusConflictStore struct {
	*memStore
	updates    int
	runCreates int
}

type indexScheduleDeleteFaultStore struct {
	*memStore
	failName string
	failed   bool
}

func (s *indexScheduleDeleteFaultStore) Delete(ctx context.Context, kind, namespace, name string) error {
	if kind == knowledgeBaseIndexRunKind && name == s.failName && !s.failed {
		s.failed = true
		return errors.New("injected child run delete failure")
	}
	return s.memStore.Delete(ctx, kind, namespace, name)
}

func (s *concurrentIndexScheduleStatusStore) Create(ctx context.Context, kind, namespace, name string, data []byte) error {
	if kind == knowledgeBaseIndexRunKind {
		s.runCreates++
	}
	return s.memStore.Create(ctx, kind, namespace, name, data)
}

func (s *concurrentIndexScheduleStatusStore) Update(ctx context.Context, kind, namespace, name string, data []byte, resourceVersion int64) error {
	if kind == knowledgeBaseIndexScheduleKind && !s.conflicted {
		var desired v1alpha1.KnowledgeBaseIndexSchedule
		if err := json.Unmarshal(data, &desired); err != nil {
			return err
		}
		if desired.Status.Phase == v1alpha1.PhaseReady && desired.Status.LastScheduleTime != "" {
			s.conflicted = true
			raw, err := s.memStore.Get(ctx, kind, namespace, name)
			if err != nil {
				return err
			}
			var latest v1alpha1.KnowledgeBaseIndexSchedule
			if err := json.Unmarshal(raw, &latest); err != nil {
				return err
			}
			latest.Spec.Suspend = boolPtr(true)
			latest.Status.Conditions = append(latest.Status.Conditions, v1alpha1.Condition{
				Type: "Audited", Status: "True", Reason: "ConcurrentAudit",
			})
			updated, err := json.Marshal(latest)
			if err != nil {
				return err
			}
			if err := s.memStore.Update(ctx, kind, namespace, name, updated, latest.ObjectMeta.ResourceVersion); err != nil {
				return err
			}
			return fmt.Errorf("injected concurrent conflict: %w", store.ErrConflict)
		}
	}
	return s.memStore.Update(ctx, kind, namespace, name, data, resourceVersion)
}

func (s *persistentIndexScheduleStatusConflictStore) Create(ctx context.Context, kind, namespace, name string, data []byte) error {
	if kind == knowledgeBaseIndexRunKind {
		s.runCreates++
	}
	return s.memStore.Create(ctx, kind, namespace, name, data)
}

func (s *persistentIndexScheduleStatusConflictStore) Update(ctx context.Context, kind, namespace, name string, data []byte, resourceVersion int64) error {
	if kind == knowledgeBaseIndexScheduleKind {
		var desired v1alpha1.KnowledgeBaseIndexSchedule
		if err := json.Unmarshal(data, &desired); err != nil {
			return err
		}
		if desired.Status.Phase == v1alpha1.PhaseReady && desired.Status.LastScheduleTime != "" {
			s.updates++
			return fmt.Errorf("injected persistent conflict: %w", store.ErrConflict)
		}
	}
	return s.memStore.Update(ctx, kind, namespace, name, data, resourceVersion)
}

func TestKnowledgeBaseIndexScheduleReconciler_MissedDeadlinePersistsNextTick(t *testing.T) {
	s := newMemStore()
	now := time.Date(2026, time.January, 2, 12, 2, 30, 0, time.UTC)
	deadline := int64(30)
	sched := makeIndexSchedule("default", "refresh", "docs", "* * * * *", now.Add(-150*time.Second))
	sched.Spec.StartingDeadlineSeconds = &deadline
	storeIndexSchedule(t, s, sched)
	storeScheduleRun(t, s, sched, "active", v1alpha1.PhaseRunning, now.Add(-time.Minute), "")
	r := NewKnowledgeBaseIndexScheduleReconciler(s, zaptest.NewLogger(t))
	r.now = func() time.Time { return now }

	result, err := r.Reconcile(context.Background(), Request{Namespace: "default", Name: "refresh"})
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if result.RequeueAfter != 30*time.Second {
		t.Fatalf("RequeueAfter = %v, want 30s", result.RequeueAfter)
	}
	stored := getIndexSchedule(t, s, "default", "refresh")
	if stored.Status.LastScheduleTime != now.Format(time.RFC3339) {
		t.Fatalf("LastScheduleTime = %q, want %q", stored.Status.LastScheduleTime, now.Format(time.RFC3339))
	}
	wantNext := now.Add(30 * time.Second).Format(time.RFC3339)
	if stored.Status.NextScheduleTime != wantNext {
		t.Fatalf("NextScheduleTime = %q, want %q", stored.Status.NextScheduleTime, wantNext)
	}
	if stored.Status.Phase != v1alpha1.PhaseReady || len(stored.Status.Active) != 1 || stored.Status.Active[0].Name != "active" {
		t.Fatalf("status after missed tick = %+v", stored.Status)
	}
	if runs := listIndexRuns(t, s, "default"); len(runs) != 1 || runs[0].ObjectMeta.Name != "active" {
		t.Fatalf("runs after missed tick = %+v, want only existing active run", runNames(runs))
	}
}

func TestKnowledgeBaseIndexScheduleReconciler_InvalidAndSuspended(t *testing.T) {
	tests := []struct {
		name      string
		schedule  string
		suspend   bool
		wantPhase v1alpha1.Phase
	}{
		{name: "invalid", schedule: "not cron", wantPhase: v1alpha1.PhaseFailed},
		{name: "suspended", schedule: "* * * * *", suspend: true, wantPhase: v1alpha1.PhaseSuspended},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newMemStore()
			now := time.Date(2026, time.January, 2, 12, 0, 30, 0, time.UTC)
			sched := makeIndexSchedule("default", tt.name, "docs", tt.schedule, now)
			if tt.suspend {
				sched.Spec.Suspend = boolPtr(true)
			}
			storeIndexSchedule(t, s, sched)
			r := NewKnowledgeBaseIndexScheduleReconciler(s, zaptest.NewLogger(t))
			r.now = func() time.Time { return now }

			result, err := r.Reconcile(context.Background(), Request{Namespace: "default", Name: tt.name})
			if err != nil {
				t.Fatalf("Reconcile() error = %v", err)
			}
			if result.RequeueAfter != time.Minute {
				t.Fatalf("RequeueAfter = %v, want 1m", result.RequeueAfter)
			}
			stored := getIndexSchedule(t, s, "default", tt.name)
			if stored.Status.Phase != tt.wantPhase || stored.Status.NextScheduleTime != "" {
				t.Fatalf("status = %+v, want phase %q and no next tick", stored.Status, tt.wantPhase)
			}
			if !tt.suspend {
				return
			}
			resourceVersion := stored.ObjectMeta.ResourceVersion
			if _, err := r.Reconcile(context.Background(), Request{Namespace: "default", Name: tt.name}); err != nil {
				t.Fatalf("repeat suspended Reconcile() error = %v", err)
			}
			stored = getIndexSchedule(t, s, "default", tt.name)
			if stored.ObjectMeta.ResourceVersion != resourceVersion {
				t.Fatalf("resourceVersion = %d after unchanged suspended reconcile, want %d", stored.ObjectMeta.ResourceVersion, resourceVersion)
			}
		})
	}
}

func TestKnowledgeBaseIndexScheduleReconciler_NotDueMarksReady(t *testing.T) {
	s := newMemStore()
	now := time.Date(2026, time.January, 2, 12, 0, 30, 0, time.UTC)
	storeIndexSchedule(t, s, makeIndexSchedule("default", "refresh", "docs", "* * * * *", now))
	r := NewKnowledgeBaseIndexScheduleReconciler(s, zaptest.NewLogger(t))
	r.now = func() time.Time { return now }

	result, err := r.Reconcile(context.Background(), Request{Namespace: "default", Name: "refresh"})
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if result.RequeueAfter != 30*time.Second {
		t.Fatalf("RequeueAfter = %v, want 30s", result.RequeueAfter)
	}
	stored := getIndexSchedule(t, s, "default", "refresh")
	if stored.Status.Phase != v1alpha1.PhaseReady || stored.Status.NextScheduleTime != now.Add(30*time.Second).Format(time.RFC3339) {
		t.Fatalf("status = %+v", stored.Status)
	}
	if len(listIndexRuns(t, s, "default")) != 0 {
		t.Fatal("not-due schedule created a run")
	}
}

func TestKnowledgeBaseIndexScheduleReconciler_AtEveryDescriptorCreatesChildRun(t *testing.T) {
	s := newMemStore()
	now := time.Date(2026, time.January, 2, 12, 16, 0, 0, time.UTC)
	sched := makeIndexSchedule("default", "refresh", "docs", "@every 15m", now.Add(-16*time.Minute))
	storeIndexSchedule(t, s, sched)
	r := NewKnowledgeBaseIndexScheduleReconciler(s, zaptest.NewLogger(t))
	r.now = func() time.Time { return now }

	if _, err := r.Reconcile(context.Background(), Request{Namespace: "default", Name: "refresh"}); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	runs := listIndexRuns(t, s, "default")
	if len(runs) != 1 || runs[0].Spec.KnowledgeBaseName != "docs" {
		t.Fatalf("@every schedule runs = %#v, want one docs IndexRun", runs)
	}
	stored := getIndexSchedule(t, s, "default", "refresh")
	if stored.Status.NextScheduleTime != now.Add(15*time.Minute).Format(time.RFC3339) {
		t.Fatalf("@every next schedule time = %q, want %q", stored.Status.NextScheduleTime, now.Add(15*time.Minute).Format(time.RFC3339))
	}
}

func TestKnowledgeBaseIndexScheduleReconciler_DueTickCreatesTemplatedChildOnce(t *testing.T) {
	s := newMemStore()
	now := time.Date(2026, time.January, 2, 12, 2, 30, 0, time.UTC)
	ttl := int32(45)
	sched := makeIndexSchedule("default", "refresh", "docs", "* * * * *", now.Add(-time.Hour))
	sched.Status.LastScheduleTime = now.Add(-90 * time.Second).Format(time.RFC3339)
	sched.Spec.RunTemplate.KnowledgeBaseName = "ignored-template-kb"
	sched.Spec.RunTemplate.TTLSecondsAfterFinished = &ttl
	storeIndexSchedule(t, s, sched)
	r := NewKnowledgeBaseIndexScheduleReconciler(s, zaptest.NewLogger(t))
	r.now = func() time.Time { return now }

	if _, err := r.Reconcile(context.Background(), Request{Namespace: "default", Name: "refresh"}); err != nil {
		t.Fatalf("first Reconcile() error = %v", err)
	}
	runs := listIndexRuns(t, s, "default")
	if len(runs) != 1 {
		t.Fatalf("run count = %d, want 1", len(runs))
	}
	run := runs[0]
	wantName := childRunName("refresh", now.Add(-30*time.Second))
	if run.ObjectMeta.Name != wantName || run.Spec.KnowledgeBaseName != "docs" || run.Spec.TTLSecondsAfterFinished == nil || *run.Spec.TTLSecondsAfterFinished != ttl {
		t.Fatalf("created run = %+v", run)
	}
	if !scheduleOwnsRun(&sched, &run) {
		t.Fatalf("run owner references = %+v", run.ObjectMeta.OwnerReferences)
	}

	stored := getIndexSchedule(t, s, "default", "refresh")
	stored.Status.LastScheduleTime = now.Add(-90 * time.Second).Format(time.RFC3339)
	updateIndexSchedule(t, s, stored)
	if _, err := r.Reconcile(context.Background(), Request{Namespace: "default", Name: "refresh"}); err != nil {
		t.Fatalf("duplicate Reconcile() error = %v", err)
	}
	if got := len(listIndexRuns(t, s, "default")); got != 1 {
		t.Fatalf("run count after duplicate tick = %d, want 1", got)
	}
}

func TestKnowledgeBaseIndexScheduleReconciler_DueTickConflictPreservesChildAndConcurrentState(t *testing.T) {
	baseStore := newMemStore()
	s := &concurrentIndexScheduleStatusStore{memStore: baseStore}
	now := time.Date(2026, time.January, 2, 12, 2, 30, 0, time.UTC)
	sched := makeIndexSchedule("default", "refresh-conflict", "docs", "* * * * *", now.Add(-time.Hour))
	sched.Status.LastScheduleTime = now.Add(-90 * time.Second).Format(time.RFC3339)
	storeIndexSchedule(t, baseStore, sched)
	r := NewKnowledgeBaseIndexScheduleReconciler(s, zaptest.NewLogger(t))
	r.now = func() time.Time { return now }

	result, err := r.Reconcile(context.Background(), Request{Namespace: "default", Name: sched.ObjectMeta.Name})
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if result.RequeueAfter != 30*time.Second {
		t.Fatalf("RequeueAfter = %v, want 30s", result.RequeueAfter)
	}
	if !s.conflicted {
		t.Fatal("expected Ready status conflict fixture to execute")
	}
	if s.runCreates != 1 {
		t.Fatalf("IndexRun create calls = %d, want exactly 1", s.runCreates)
	}
	if runs := listIndexRuns(t, baseStore, "default"); len(runs) != 1 {
		t.Fatalf("IndexRuns = %d, want 1", len(runs))
	}
	stored := getIndexSchedule(t, baseStore, "default", sched.ObjectMeta.Name)
	if stored.Spec.Suspend == nil || !*stored.Spec.Suspend {
		t.Fatalf("concurrent Suspend = %v, want true", stored.Spec.Suspend)
	}
	if stored.Status.Phase != v1alpha1.PhaseReady || stored.Status.LastScheduleTime != now.Format(time.RFC3339) {
		t.Fatalf("status after conflict = %+v", stored.Status)
	}
	if condition := findCondition(stored.Status.Conditions, "Audited"); condition == nil || condition.Reason != "ConcurrentAudit" {
		t.Fatalf("concurrent Audited condition was lost: %#v", condition)
	}
}

func TestKnowledgeBaseIndexScheduleReconciler_DueTickConflictRetriesAreBounded(t *testing.T) {
	baseStore := newMemStore()
	s := &persistentIndexScheduleStatusConflictStore{memStore: baseStore}
	now := time.Date(2026, time.January, 2, 12, 2, 30, 0, time.UTC)
	sched := makeIndexSchedule("default", "refresh-persistent-conflict", "docs", "* * * * *", now.Add(-time.Hour))
	sched.Status.LastScheduleTime = now.Add(-90 * time.Second).Format(time.RFC3339)
	storeIndexSchedule(t, baseStore, sched)
	r := NewKnowledgeBaseIndexScheduleReconciler(s, zaptest.NewLogger(t))
	r.now = func() time.Time { return now }

	_, err := r.Reconcile(context.Background(), Request{Namespace: "default", Name: sched.ObjectMeta.Name})
	if !errors.Is(err, store.ErrConflict) {
		t.Fatalf("Reconcile() error = %v, want ErrConflict", err)
	}
	if s.updates != 5 {
		t.Fatalf("IndexSchedule update attempts = %d, want 5", s.updates)
	}
	if s.runCreates != 1 {
		t.Fatalf("IndexRun create calls = %d, want exactly 1", s.runCreates)
	}
	if runs := listIndexRuns(t, baseStore, "default"); len(runs) != 1 {
		t.Fatalf("IndexRuns = %d, want 1", len(runs))
	}
}

func TestKnowledgeBaseIndexScheduleReconciler_ConcurrencyPolicies(t *testing.T) {
	tests := []struct {
		name            string
		policy          string
		wantOldPresent  bool
		wantNewPresent  bool
		wantRunCount    int
		wantActiveCount int
	}{
		{name: "default forbid", wantOldPresent: true, wantRunCount: 1, wantActiveCount: 1},
		{name: "forbid", policy: v1alpha1.ConcurrencyPolicyForbid, wantOldPresent: true, wantRunCount: 1, wantActiveCount: 1},
		{name: "allow", policy: v1alpha1.ConcurrencyPolicyAllow, wantOldPresent: true, wantNewPresent: true, wantRunCount: 2, wantActiveCount: 2},
		{name: "replace", policy: v1alpha1.ConcurrencyPolicyReplace, wantNewPresent: true, wantRunCount: 1, wantActiveCount: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newMemStore()
			now := time.Date(2026, time.January, 2, 12, 2, 30, 0, time.UTC)
			sched := makeIndexSchedule("default", "refresh", "docs", "* * * * *", now.Add(-time.Hour))
			sched.Spec.ConcurrencyPolicy = tt.policy
			sched.Status.LastScheduleTime = now.Add(-90 * time.Second).Format(time.RFC3339)
			storeIndexSchedule(t, s, sched)
			storeScheduleRun(t, s, sched, "old", v1alpha1.PhaseRunning, now.Add(-time.Minute), "")
			r := NewKnowledgeBaseIndexScheduleReconciler(s, zaptest.NewLogger(t))
			r.now = func() time.Time { return now }

			if _, err := r.Reconcile(context.Background(), Request{Namespace: "default", Name: "refresh"}); err != nil {
				t.Fatalf("Reconcile() error = %v", err)
			}
			runs := listIndexRuns(t, s, "default")
			if len(runs) != tt.wantRunCount {
				t.Fatalf("run count = %d, want %d", len(runs), tt.wantRunCount)
			}
			oldPresent := false
			newPresent := false
			wantNewName := childRunName("refresh", now.Add(-30*time.Second))
			for _, run := range runs {
				oldPresent = oldPresent || run.ObjectMeta.Name == "old"
				newPresent = newPresent || run.ObjectMeta.Name == wantNewName
			}
			if oldPresent != tt.wantOldPresent {
				t.Fatalf("old run present = %v, want %v", oldPresent, tt.wantOldPresent)
			}
			if newPresent != tt.wantNewPresent {
				t.Fatalf("new run %q present = %v, want %v", wantNewName, newPresent, tt.wantNewPresent)
			}
			if active := len(getIndexSchedule(t, s, "default", "refresh").Status.Active); active != tt.wantActiveCount {
				t.Fatalf("active count = %d, want %d", active, tt.wantActiveCount)
			}
		})
	}
}

func TestKnowledgeBaseIndexScheduleReconciler_ReplaceResumesAfterPartialCancellation(t *testing.T) {
	baseStore := newMemStore()
	now := time.Date(2026, time.January, 2, 12, 2, 30, 0, time.UTC)
	sched := makeIndexSchedule("default", "refresh", "docs", "* * * * *", now.Add(-time.Hour))
	sched.Spec.ConcurrencyPolicy = v1alpha1.ConcurrencyPolicyReplace
	sched.Status.LastScheduleTime = now.Add(-90 * time.Second).Format(time.RFC3339)
	storeIndexSchedule(t, baseStore, sched)
	storeScheduleRun(t, baseStore, sched, "old-first", v1alpha1.PhaseRunning, now.Add(-2*time.Minute), "")
	storeScheduleRun(t, baseStore, sched, "old-second", v1alpha1.PhaseRunning, now.Add(-time.Minute), "")
	s := &indexScheduleDeleteFaultStore{memStore: baseStore, failName: "old-second"}
	r := NewKnowledgeBaseIndexScheduleReconciler(s, zaptest.NewLogger(t))
	r.now = func() time.Time { return now }

	_, err := r.Reconcile(context.Background(), Request{Namespace: "default", Name: "refresh"})
	if err == nil || !s.failed {
		t.Fatalf("first Reconcile() error = %v, want injected cancellation failure", err)
	}
	runs := listIndexRuns(t, baseStore, "default")
	if len(runs) != 1 || runs[0].ObjectMeta.Name != "old-second" {
		t.Fatalf("runs after partial cancellation = %+v, want only old-second", runNames(runs))
	}
	wantNewName := childRunName("refresh", now.Add(-30*time.Second))
	if hasRunNamed(runs, wantNewName) {
		t.Fatalf("replacement %q was created before cancellation completed", wantNewName)
	}
	if got := getIndexSchedule(t, baseStore, "default", "refresh").Status.LastScheduleTime; got != sched.Status.LastScheduleTime {
		t.Fatalf("LastScheduleTime after failed cancellation = %q, want unchanged %q", got, sched.Status.LastScheduleTime)
	}

	if _, err := r.Reconcile(context.Background(), Request{Namespace: "default", Name: "refresh"}); err != nil {
		t.Fatalf("retry Reconcile() error = %v", err)
	}
	runs = listIndexRuns(t, baseStore, "default")
	if len(runs) != 1 || runs[0].ObjectMeta.Name != wantNewName {
		t.Fatalf("runs after retry = %+v, want only replacement %q", runNames(runs), wantNewName)
	}
	stored := getIndexSchedule(t, baseStore, "default", "refresh")
	if stored.Status.LastScheduleTime != now.Format(time.RFC3339) {
		t.Fatalf("LastScheduleTime after retry = %q, want %q", stored.Status.LastScheduleTime, now.Format(time.RFC3339))
	}
	if len(stored.Status.Active) != 1 || stored.Status.Active[0].Name != wantNewName {
		t.Fatalf("active after retry = %+v, want replacement %q", stored.Status.Active, wantNewName)
	}
}

func TestKnowledgeBaseIndexScheduleReconciler_RecreatedScheduleIgnoresOldChildren(t *testing.T) {
	s := newMemStore()
	now := time.Date(2026, time.January, 2, 12, 2, 30, 0, time.UTC)
	oldSchedule := makeIndexSchedule("default", "refresh", "docs", "* * * * *", now.Add(-time.Hour))
	oldSchedule.ObjectMeta.UID = "old-uid"
	storeScheduleRun(t, s, oldSchedule, "orphan", v1alpha1.PhaseRunning, now.Add(-time.Minute), "")
	newSchedule := oldSchedule
	newSchedule.ObjectMeta.UID = "new-uid"
	newSchedule.Status.LastScheduleTime = now.Add(-90 * time.Second).Format(time.RFC3339)
	storeIndexSchedule(t, s, newSchedule)
	r := NewKnowledgeBaseIndexScheduleReconciler(s, zaptest.NewLogger(t))
	r.now = func() time.Time { return now }

	if _, err := r.Reconcile(context.Background(), Request{Namespace: "default", Name: "refresh"}); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	stored := getIndexSchedule(t, s, "default", "refresh")
	if len(stored.Status.Active) != 1 || stored.Status.Active[0].Name == "orphan" {
		t.Fatalf("active = %+v, want only new schedule child", stored.Status.Active)
	}
	if len(listIndexRuns(t, s, "default")) != 2 {
		t.Fatal("old child should remain but not be adopted")
	}
}

func TestKnowledgeBaseIndexScheduleReconciler_RecreatedScheduleRetriesOldChildNameCollision(t *testing.T) {
	s := newMemStore()
	now := time.Date(2026, time.January, 2, 12, 2, 30, 0, time.UTC)
	tick := now.Add(-30 * time.Second)
	oldSchedule := makeIndexSchedule("default", "refresh", "docs", "* * * * *", now.Add(-time.Hour))
	oldSchedule.ObjectMeta.UID = "old-uid"
	storeScheduleRun(t, s, oldSchedule, childRunName(oldSchedule.ObjectMeta.Name, tick), v1alpha1.PhaseRunning, tick, "")

	newSchedule := oldSchedule
	newSchedule.ObjectMeta.UID = "new-uid"
	newSchedule.Status.LastScheduleTime = now.Add(-90 * time.Second).Format(time.RFC3339)
	storeIndexSchedule(t, s, newSchedule)
	r := NewKnowledgeBaseIndexScheduleReconciler(s, zaptest.NewLogger(t))
	r.now = func() time.Time { return now }

	_, err := r.Reconcile(context.Background(), Request{Namespace: "default", Name: "refresh"})
	if !errors.Is(err, store.ErrAlreadyExists) {
		t.Fatalf("Reconcile() error = %v, want ErrAlreadyExists", err)
	}
	stored := getIndexSchedule(t, s, "default", "refresh")
	if stored.Status.LastScheduleTime != newSchedule.Status.LastScheduleTime {
		t.Fatalf("LastScheduleTime = %q, want unchanged %q", stored.Status.LastScheduleTime, newSchedule.Status.LastScheduleTime)
	}
	if len(stored.Status.Active) != 0 {
		t.Fatalf("active = %+v, want no old-UID children adopted", stored.Status.Active)
	}
	if err := s.Delete(context.Background(), knowledgeBaseIndexRunKind, "default", childRunName(oldSchedule.ObjectMeta.Name, tick)); err != nil {
		t.Fatalf("delete old child: %v", err)
	}
	if _, err := r.Reconcile(context.Background(), Request{Namespace: "default", Name: "refresh"}); err != nil {
		t.Fatalf("recovery Reconcile() error = %v", err)
	}
	runs := listIndexRuns(t, s, "default")
	if len(runs) != 1 || !scheduleOwnsRun(&newSchedule, &runs[0]) {
		t.Fatalf("runs after recovery = %+v, want one child owned by new UID", runs)
	}
	if got := getIndexSchedule(t, s, "default", "refresh").Status.LastScheduleTime; got != now.Format(time.RFC3339) {
		t.Fatalf("LastScheduleTime after recovery = %q, want %q", got, now.Format(time.RFC3339))
	}
}

func TestKnowledgeBaseIndexScheduleReconciler_PrunesOldHistory(t *testing.T) {
	s := newMemStore()
	now := time.Date(2026, time.January, 2, 12, 0, 30, 0, time.UTC)
	sched := makeIndexSchedule("default", "refresh", "docs", "* * * * *", now)
	sched.Status.LastScheduleTime = now.Format(time.RFC3339)
	sched.Spec.SuccessfulRunsHistoryLimit = int32Ptr(1)
	sched.Spec.FailedRunsHistoryLimit = int32Ptr(1)
	storeIndexSchedule(t, s, sched)
	storeScheduleRun(t, s, sched, "success-old", v1alpha1.PhaseSucceeded, now.Add(-4*time.Minute), now.Add(-3*time.Minute).Format(time.RFC3339))
	storeScheduleRun(t, s, sched, "success-new", v1alpha1.PhaseSucceeded, now.Add(-2*time.Minute), now.Add(-time.Minute).Format(time.RFC3339))
	storeScheduleRun(t, s, sched, "failed-old", v1alpha1.PhaseFailed, now.Add(-4*time.Minute), "")
	storeScheduleRun(t, s, sched, "failed-new", v1alpha1.PhaseFailed, now.Add(-2*time.Minute), "")
	r := NewKnowledgeBaseIndexScheduleReconciler(s, zaptest.NewLogger(t))
	r.now = func() time.Time { return now }

	if _, err := r.Reconcile(context.Background(), Request{Namespace: "default", Name: "refresh"}); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	runs := listIndexRuns(t, s, "default")
	if len(runs) != 2 || hasRunNamed(runs, "success-old") || hasRunNamed(runs, "failed-old") {
		t.Fatalf("remaining runs = %+v, want newest success and failure", runNames(runs))
	}
	if got := getIndexSchedule(t, s, "default", "refresh").Status.LastSuccessfulTime; got != now.Add(-time.Minute).Format(time.RFC3339) {
		t.Fatalf("LastSuccessfulTime = %q", got)
	}
}

func TestKnowledgeBaseIndexScheduleReconciler_HistoryPruningResumesAfterDeleteFailure(t *testing.T) {
	baseStore := newMemStore()
	now := time.Date(2026, time.January, 2, 12, 0, 30, 0, time.UTC)
	sched := makeIndexSchedule("default", "refresh", "docs", "* * * * *", now)
	sched.Status.LastScheduleTime = now.Format(time.RFC3339)
	sched.Spec.SuccessfulRunsHistoryLimit = int32Ptr(1)
	storeIndexSchedule(t, baseStore, sched)
	storeScheduleRun(t, baseStore, sched, "success-old", v1alpha1.PhaseSucceeded, now.Add(-2*time.Minute), now.Add(-90*time.Second).Format(time.RFC3339))
	storeScheduleRun(t, baseStore, sched, "success-new", v1alpha1.PhaseSucceeded, now.Add(-time.Minute), now.Add(-30*time.Second).Format(time.RFC3339))
	s := &indexScheduleDeleteFaultStore{memStore: baseStore, failName: "success-old"}
	r := NewKnowledgeBaseIndexScheduleReconciler(s, zaptest.NewLogger(t))
	r.now = func() time.Time { return now }

	if _, err := r.Reconcile(context.Background(), Request{Namespace: "default", Name: "refresh"}); err != nil {
		t.Fatalf("first Reconcile() error = %v", err)
	}
	runs := listIndexRuns(t, baseStore, "default")
	if !s.failed || len(runs) != 2 {
		t.Fatalf("runs after failed prune = %+v, want both retained after injected failure", runNames(runs))
	}
	wantLastSuccess := now.Add(-30 * time.Second).Format(time.RFC3339)
	if got := getIndexSchedule(t, baseStore, "default", "refresh").Status.LastSuccessfulTime; got != wantLastSuccess {
		t.Fatalf("LastSuccessfulTime after failed prune = %q, want %q", got, wantLastSuccess)
	}

	if _, err := r.Reconcile(context.Background(), Request{Namespace: "default", Name: "refresh"}); err != nil {
		t.Fatalf("retry Reconcile() error = %v", err)
	}
	runs = listIndexRuns(t, baseStore, "default")
	if len(runs) != 1 || runs[0].ObjectMeta.Name != "success-new" {
		t.Fatalf("runs after prune retry = %+v, want only success-new", runNames(runs))
	}
}

func makeIndexSchedule(namespace, name, knowledgeBaseName, schedule string, createdAt time.Time) v1alpha1.KnowledgeBaseIndexSchedule {
	return v1alpha1.KnowledgeBaseIndexSchedule{
		TypeMeta: v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: knowledgeBaseIndexScheduleKind},
		ObjectMeta: v1alpha1.ObjectMeta{
			Namespace:         namespace,
			Name:              name,
			UID:               name + "-uid",
			CreationTimestamp: createdAt,
		},
		Spec: v1alpha1.KnowledgeBaseIndexScheduleSpec{
			KnowledgeBaseName: knowledgeBaseName,
			Schedule:          schedule,
		},
	}
}

func storeIndexSchedule(t *testing.T, s *memStore, sched v1alpha1.KnowledgeBaseIndexSchedule) {
	t.Helper()
	raw, err := json.Marshal(sched)
	if err != nil {
		t.Fatalf("marshal IndexSchedule: %v", err)
	}
	if err := s.Create(context.Background(), knowledgeBaseIndexScheduleKind, sched.ObjectMeta.Namespace, sched.ObjectMeta.Name, raw); err != nil {
		t.Fatalf("store IndexSchedule: %v", err)
	}
}

func getIndexSchedule(t *testing.T, s *memStore, namespace, name string) v1alpha1.KnowledgeBaseIndexSchedule {
	t.Helper()
	raw, err := s.Get(context.Background(), knowledgeBaseIndexScheduleKind, namespace, name)
	if err != nil {
		t.Fatalf("get IndexSchedule: %v", err)
	}
	var sched v1alpha1.KnowledgeBaseIndexSchedule
	if err := json.Unmarshal(raw, &sched); err != nil {
		t.Fatalf("unmarshal IndexSchedule: %v", err)
	}
	return sched
}

func updateIndexSchedule(t *testing.T, s *memStore, sched v1alpha1.KnowledgeBaseIndexSchedule) {
	t.Helper()
	raw, err := json.Marshal(sched)
	if err != nil {
		t.Fatalf("marshal IndexSchedule: %v", err)
	}
	if err := s.Update(context.Background(), knowledgeBaseIndexScheduleKind, sched.ObjectMeta.Namespace, sched.ObjectMeta.Name, raw, sched.ObjectMeta.ResourceVersion); err != nil {
		t.Fatalf("update IndexSchedule: %v", err)
	}
}

func listIndexRuns(t *testing.T, s *memStore, namespace string) []v1alpha1.KnowledgeBaseIndexRun {
	t.Helper()
	items, err := s.List(context.Background(), knowledgeBaseIndexRunKind, store.ListOptions{Namespace: namespace})
	if err != nil {
		t.Fatalf("list IndexRuns: %v", err)
	}
	runs := make([]v1alpha1.KnowledgeBaseIndexRun, 0, len(items))
	for _, raw := range items {
		var run v1alpha1.KnowledgeBaseIndexRun
		if err := json.Unmarshal(raw, &run); err != nil {
			t.Fatalf("unmarshal IndexRun: %v", err)
		}
		runs = append(runs, run)
	}
	return runs
}

func storeScheduleRun(t *testing.T, s *memStore, sched v1alpha1.KnowledgeBaseIndexSchedule, name string, phase v1alpha1.Phase, createdAt time.Time, completedAt string) {
	t.Helper()
	run := makeIndexRun(sched.ObjectMeta.Namespace, name, sched.Spec.KnowledgeBaseName)
	run.ObjectMeta.CreationTimestamp = createdAt
	run.ObjectMeta.Labels = map[string]string{scheduleRunOwnerLabel: sched.ObjectMeta.Name}
	run.ObjectMeta.OwnerReferences = []v1alpha1.OwnerReference{{
		APIVersion: v1alpha1.APIVersion,
		Kind:       knowledgeBaseIndexScheduleKind,
		Name:       sched.ObjectMeta.Name,
		UID:        sched.ObjectMeta.UID,
	}}
	run.Status.Phase = phase
	run.Status.CompletedAt = completedAt
	storeIndexRun(t, s, run)
}

func hasRunNamed(runs []v1alpha1.KnowledgeBaseIndexRun, name string) bool {
	for _, run := range runs {
		if run.ObjectMeta.Name == name {
			return true
		}
	}
	return false
}

func runNames(runs []v1alpha1.KnowledgeBaseIndexRun) []string {
	names := make([]string, 0, len(runs))
	for _, run := range runs {
		names = append(names, run.ObjectMeta.Name)
	}
	return names
}

func boolPtr(value bool) *bool    { return &value }
func int32Ptr(value int32) *int32 { return &value }
