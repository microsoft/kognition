// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package studiotutorial

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestProgressMergesAndFencesReset(t *testing.T) {
	// TC-0342: concurrent completion cannot cross a reset generation.
	store := NewProgressStore(filepath.Join(t.TempDir(), "progress.json"))
	first, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	second := emptyProgress()
	first.Completed = []string{"practice"}
	first, err = store.Save(first, false)
	if err != nil {
		t.Fatal(err)
	}
	second.Completed = []string{"create"}
	second, err = store.Save(second, false)
	if err != nil || len(second.Completed) != 2 {
		t.Fatalf("concurrent completion lost: %#v %v", second, err)
	}
	second.Completed = []string{}
	reset, err := store.Save(second, true)
	if err != nil || len(reset.Completed) != 0 || reset.Generation <= first.Generation {
		t.Fatalf("reset failed: %#v %v", reset, err)
	}
	if _, err := store.Save(first, false); !errors.Is(err, ErrProgressConflict) {
		t.Fatalf("stale completion crossed reset: %v", err)
	}
}

func TestProgressRejectsContentAndAllowsExplicitRepair(t *testing.T) {
	path := filepath.Join(t.TempDir(), "progress.json")
	if err := os.WriteFile(path, []byte(`{"version":1,"generation":1,"draft":"secret"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	store := NewProgressStore(path)
	if _, err := store.Load(); err == nil {
		t.Fatal("unknown learner-content field accepted")
	}
	if _, err := store.Save(emptyProgress(), false); err == nil {
		t.Fatal("invalid progress silently overwritten")
	}
	if _, err := store.Save(emptyProgress(), true); err != nil {
		t.Fatal(err)
	}
	p := emptyProgress()
	p.Completed = []string{"arbitrary-user-text"}
	if _, err := store.Save(p, false); err == nil {
		t.Fatal("unknown step identifier accepted")
	}
	if _, err := NewProgressStore("").Save(emptyProgress(), false); err == nil {
		t.Fatal("unavailable storage silently succeeded")
	}
}
