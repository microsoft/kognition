// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package postgres

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap/zaptest"

	"github.com/kognition-ai/kognition/internal/store"
)

func newModelCallHealthStore(t *testing.T) (*Store, string) {
	t.Helper()
	prefix := "health-" + uuid.NewString()
	t.Cleanup(func() {
		if _, err := testPool.Exec(context.Background(), "DELETE FROM model_call_health WHERE reporter LIKE $1", prefix+"%"); err != nil {
			t.Error(err)
		}
	})
	return New(testPool, zaptest.NewLogger(t)), prefix
}

func listForReporters(t *testing.T, backend *Store, prefix string) map[string]store.ModelCallHealth {
	t.Helper()
	records, err := backend.ListModelCallHealth(context.Background(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	byTarget := map[string]store.ModelCallHealth{}
	for _, record := range records {
		if strings.HasPrefix(record.Target, prefix) {
			byTarget[record.Target] = record
		}
	}
	return byTarget
}

func TestModelCallHealthRoundTripsRecords(t *testing.T) {
	backend, prefix := newModelCallHealthStore(t)
	ctx := context.Background()
	faultAt := time.Now().UTC().Truncate(time.Microsecond)
	successAt := faultAt.Add(-time.Second)

	records := []store.ModelCallHealth{
		{Target: prefix + "-faulty", Calls: 10, Faults: 6, ConsecutiveFaults: 4, LastFaultReason: "RateLimited", LastFaultAt: faultAt, LastSuccessAt: successAt},
		{Target: prefix + "-clean", Calls: 3, ConsecutiveSuccesses: 3, LastSuccessAt: successAt},
	}
	if err := backend.ReplaceModelCallHealth(ctx, prefix+"-a", records); err != nil {
		t.Fatalf("ReplaceModelCallHealth() error: %v", err)
	}

	got := listForReporters(t, backend, prefix)
	if len(got) != 2 {
		t.Fatalf("listed %d targets, want 2: %+v", len(got), got)
	}
	faulty := got[prefix+"-faulty"]
	if faulty.Calls != 10 || faulty.Faults != 6 || faulty.ConsecutiveFaults != 4 || faulty.LastFaultReason != "RateLimited" ||
		!faulty.LastFaultAt.Equal(faultAt) || !faulty.LastSuccessAt.Equal(successAt) {
		t.Errorf("faulty record = %+v, want the published values", faulty)
	}
	clean := got[prefix+"-clean"]
	if clean.ConsecutiveSuccesses != 3 || !clean.LastFaultAt.IsZero() || clean.LastFaultReason != "" {
		t.Errorf("clean record = %+v, want no fault and a zero fault time", clean)
	}
}

func TestModelCallHealthReplaceRemovesRetiredTargetsAndIsolatesReporters(t *testing.T) {
	backend, prefix := newModelCallHealthStore(t)
	ctx := context.Background()

	if err := backend.ReplaceModelCallHealth(ctx, prefix+"-a", []store.ModelCallHealth{
		{Target: prefix + "-one", Calls: 2, Faults: 2, ConsecutiveFaults: 2, LastFaultReason: "Timeout"},
		{Target: prefix + "-two", Calls: 1},
	}); err != nil {
		t.Fatal(err)
	}
	if err := backend.ReplaceModelCallHealth(ctx, prefix+"-b", []store.ModelCallHealth{{Target: prefix + "-one", Calls: 5, Faults: 1}}); err != nil {
		t.Fatal(err)
	}

	// Replacing reporter a keeps only its listed target and updates it in place.
	if err := backend.ReplaceModelCallHealth(ctx, prefix+"-a", []store.ModelCallHealth{{Target: prefix + "-one", Calls: 9, Faults: 0}}); err != nil {
		t.Fatal(err)
	}
	records, err := backend.ListModelCallHealth(ctx, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	var calls []int32
	for _, record := range records {
		if strings.HasPrefix(record.Target, prefix) {
			if record.Target == prefix+"-two" {
				t.Errorf("retired target %q is still listed", record.Target)
			}
			calls = append(calls, record.Calls)
		}
	}
	if len(calls) != 2 || !(calls[0] == 9 && calls[1] == 5 || calls[0] == 5 && calls[1] == 9) {
		t.Fatalf("calls for the shared target = %v, want one record per reporter (9 and 5)", calls)
	}

	// Publishing nothing clears the reporter without touching the other one.
	if err := backend.ReplaceModelCallHealth(ctx, prefix+"-a", nil); err != nil {
		t.Fatal(err)
	}
	got := listForReporters(t, backend, prefix)
	if len(got) != 1 || got[prefix+"-one"].Calls != 5 {
		t.Fatalf("after clearing reporter a: %+v, want only reporter b's record", got)
	}
}

func TestModelCallHealthListIgnoresStaleRecordsAndExpiresAbandonedReporters(t *testing.T) {
	backend, prefix := newModelCallHealthStore(t)
	ctx := context.Background()
	if err := backend.ReplaceModelCallHealth(ctx, prefix+"-live", []store.ModelCallHealth{{Target: prefix + "-live", Calls: 1}}); err != nil {
		t.Fatal(err)
	}
	if err := backend.ReplaceModelCallHealth(ctx, prefix+"-stale", []store.ModelCallHealth{{Target: prefix + "-stale", Calls: 1}}); err != nil {
		t.Fatal(err)
	}
	if _, err := testPool.Exec(ctx, "UPDATE model_call_health SET updated_at = now() - interval '10 minutes' WHERE reporter = $1", prefix+"-stale"); err != nil {
		t.Fatal(err)
	}

	got := listForReporters(t, backend, prefix)
	if _, ok := got[prefix+"-stale"]; ok || len(got) != 1 {
		t.Fatalf("listed %+v, want the stale record filtered by maxAge", got)
	}

	// A record older than the retention is deleted by the next publish from anyone.
	if _, err := testPool.Exec(ctx, "UPDATE model_call_health SET updated_at = now() - interval '3 hours' WHERE reporter = $1", prefix+"-stale"); err != nil {
		t.Fatal(err)
	}
	if err := backend.ReplaceModelCallHealth(ctx, prefix+"-live", []store.ModelCallHealth{{Target: prefix + "-live", Calls: 2}}); err != nil {
		t.Fatal(err)
	}
	var remaining int
	if err := testPool.QueryRow(ctx, "SELECT count(*) FROM model_call_health WHERE reporter = $1", prefix+"-stale").Scan(&remaining); err != nil || remaining != 0 {
		t.Fatalf("abandoned reporter rows = %d (err %v), want them expired", remaining, err)
	}
}

func TestModelCallHealthRejectsInvalidRecords(t *testing.T) {
	backend, prefix := newModelCallHealthStore(t)
	ctx := context.Background()
	valid := store.ModelCallHealth{Target: prefix + "-valid", Calls: 1}

	tooMany := make([]store.ModelCallHealth, store.MaxModelCallHealthTargets+1)
	for i := range tooMany {
		tooMany[i] = store.ModelCallHealth{Target: prefix + uuid.NewString()}
	}
	tests := []struct {
		name     string
		reporter string
		records  []store.ModelCallHealth
	}{
		{"missing reporter", "", []store.ModelCallHealth{valid}},
		{"empty target", prefix, []store.ModelCallHealth{{Calls: 1}}},
		{"oversized target", prefix, []store.ModelCallHealth{{Target: strings.Repeat("t", store.MaxModelCallHealthTargetLength+1)}}},
		{"oversized reason", prefix, []store.ModelCallHealth{{Target: prefix + "-r", Calls: 1, Faults: 1, LastFaultReason: strings.Repeat("r", store.MaxModelCallHealthReasonLength+1)}}},
		{"faults exceed calls", prefix, []store.ModelCallHealth{{Target: prefix + "-f", Calls: 1, Faults: 2}}},
		{"negative calls", prefix, []store.ModelCallHealth{{Target: prefix + "-n", Calls: -1}}},
		{"negative streak", prefix, []store.ModelCallHealth{{Target: prefix + "-s", Calls: 1, ConsecutiveFaults: -1}}},
		{"duplicate target", prefix, []store.ModelCallHealth{valid, valid}},
		{"too many targets", prefix, tooMany},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := backend.ReplaceModelCallHealth(ctx, test.reporter, test.records); err == nil {
				t.Fatal("ReplaceModelCallHealth() accepted invalid input")
			}
		})
	}
	if got := listForReporters(t, backend, prefix); len(got) != 0 {
		t.Fatalf("rejected input left records behind: %+v", got)
	}
}

func TestModelCallHealthFailsClosedWhenTheDatabaseIsUnavailable(t *testing.T) {
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, testPool.Config().ConnString())
	if err != nil {
		t.Fatal(err)
	}
	pool.Close()
	backend := New(pool, zaptest.NewLogger(t))

	if err := backend.ReplaceModelCallHealth(ctx, "reporter", []store.ModelCallHealth{{Target: "target", Calls: 1}}); err == nil {
		t.Error("ReplaceModelCallHealth() succeeded against a closed pool")
	}
	if _, err := backend.ListModelCallHealth(ctx, time.Minute); err == nil {
		t.Error("ListModelCallHealth() succeeded against a closed pool")
	}
}

func TestModelCallHealthReplaceIsAtomic(t *testing.T) {
	backend, prefix := newModelCallHealthStore(t)
	ctx := context.Background()
	if err := backend.ReplaceModelCallHealth(ctx, prefix+"-a", []store.ModelCallHealth{{Target: prefix + "-old", Calls: 4}}); err != nil {
		t.Fatal(err)
	}

	// PostgreSQL rejects a NUL byte in text during the insert, after the delete of
	// the reporter's retired records has already run inside the transaction.
	err := backend.ReplaceModelCallHealth(ctx, prefix+"-a", []store.ModelCallHealth{
		{Target: prefix + "-new", Calls: 1, Faults: 1, LastFaultReason: "bad\x00reason"},
	})
	if err == nil {
		t.Fatal("ReplaceModelCallHealth() accepted a record the database rejects")
	}
	got := listForReporters(t, backend, prefix)
	if len(got) != 1 || got[prefix+"-old"].Calls != 4 {
		t.Fatalf("after a failed replace: %+v, want the previous record restored by the rollback", got)
	}
}
