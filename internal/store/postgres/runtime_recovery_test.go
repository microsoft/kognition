// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kognition-ai/kognition/internal/store"
)

func TestRuntimeRecoveryDurableCASAndExpiry(test *testing.T) {
	storage := newTestStore(test)
	ctx := context.Background()
	input := store.RuntimeRecoveryRecord{Workspace: "tenant", SessionID: "session", BindingDigest: strings.Repeat("a", 64), Ciphertext: json.RawMessage(`{"ciphertext":"encrypted-fixture"}`), ExpiresAt: time.Now().Add(time.Hour)}
	prepared, err := storage.PrepareRuntimeRecovery(ctx, input)
	if err != nil || prepared.Phase != store.RuntimeRecoveryPrepared || prepared.Version != 1 {
		test.Fatalf("prepare=%+v err=%v", prepared, err)
	}
	if repeated, err := storage.PrepareRuntimeRecovery(ctx, input); err != nil || repeated.Version != 1 {
		test.Fatal("prepare retry changed durable ownership")
	}
	wrong := input
	wrong.BindingDigest = strings.Repeat("b", 64)
	if _, err := storage.PrepareRuntimeRecovery(ctx, wrong); !errors.Is(err, store.ErrConflict) {
		test.Fatal("binding overwrite accepted")
	}
	claimed, err := storage.ClaimRuntimeRecovery(ctx, "tenant", "session", prepared.Version, "owner-a")
	if err != nil || claimed.Phase != store.RuntimeRecoveryClaimed {
		test.Fatal(err)
	}
	if _, err := storage.ClaimRuntimeRecovery(ctx, "tenant", "session", prepared.Version, "owner-b"); !errors.Is(err, store.ErrConflict) {
		test.Fatal("second execution owner accepted")
	}
	terminal := claimed
	terminal.Ciphertext = json.RawMessage(`{"ciphertext":"terminal-state"}`)
	wrong = terminal
	wrong.Owner = "owner-b"
	if err := storage.CompleteRuntimeRecovery(ctx, wrong); !errors.Is(err, store.ErrConflict) {
		test.Fatal("foreign owner completed execution")
	}
	if err := storage.CompleteRuntimeRecovery(ctx, terminal); err != nil {
		test.Fatal(err)
	}
	if err := storage.CompleteRuntimeRecovery(ctx, terminal); !errors.Is(err, store.ErrConflict) {
		test.Fatal("stale completion overwrote terminal facts")
	}
	input.SessionID = "uncertain"
	uncertain, err := storage.PrepareRuntimeRecovery(ctx, input)
	if err != nil {
		test.Fatal(err)
	}
	if _, err := storage.ClaimRuntimeRecovery(ctx, "tenant", "uncertain", uncertain.Version, "owner-a"); err != nil {
		test.Fatal(err)
	}
	if expired, err := storage.ExpireRuntimeRecovery(ctx, time.Now().Add(2*time.Hour), 100); err != nil || expired != 1 {
		test.Fatalf("expiry count=%d err=%v", expired, err)
	}
	retired, err := storage.GetRuntimeRecovery(ctx, "tenant", "session")
	if err != nil || retired.Phase != store.RuntimeRecoveryRetired || len(retired.Ciphertext) != 0 || retired.BindingDigest != input.BindingDigest {
		test.Fatal("expiry erased replay fence or retained ciphertext")
	}
	retained, err := storage.GetRuntimeRecovery(ctx, "tenant", "uncertain")
	if err != nil || retained.Phase != store.RuntimeRecoveryClaimed || len(retained.Ciphertext) == 0 {
		test.Fatal("age cleanup erased unresolved claim")
	}
	if _, err := storage.GetRuntimeRecovery(ctx, "other", "session"); !errors.Is(err, store.ErrNotFound) {
		test.Fatal("cross-workspace recovery read succeeded")
	}
}
