// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cost

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kognition-ai/kognition/internal/store"
)

func TestRuntimeRecoveryCASAndTerminalExpiry(test *testing.T) {
	storage := newTestStore(test)
	ctx := context.Background()
	input := store.RuntimeRecoveryRecord{Workspace: "tenant", SessionID: "session", BindingDigest: strings.Repeat("a", 64), Ciphertext: json.RawMessage(`{"ciphertext":"encrypted"}`), ExpiresAt: time.Now().Add(time.Hour)}
	prepared, err := storage.PrepareRuntimeRecovery(ctx, input)
	if err != nil {
		test.Fatal(err)
	}
	claimed, err := storage.ClaimRuntimeRecovery(ctx, "tenant", "session", prepared.Version, "owner")
	if err != nil {
		test.Fatal(err)
	}
	if _, err := storage.ClaimRuntimeRecovery(ctx, "tenant", "session", prepared.Version, "other"); !errors.Is(err, store.ErrConflict) {
		test.Fatal("second owner accepted")
	}
	if expired, err := storage.ExpireRuntimeRecovery(ctx, time.Now().Add(2*time.Hour), 100); err != nil || expired != 0 {
		test.Fatal("unresolved claim expired")
	}
	wrong := claimed
	wrong.Owner = "other"
	if err := storage.CompleteRuntimeRecovery(ctx, wrong); !errors.Is(err, store.ErrConflict) {
		test.Fatal("foreign owner completed checkpoint")
	}
	if err := storage.CompleteRuntimeRecovery(ctx, claimed); err != nil {
		test.Fatal(err)
	}
	if expired, err := storage.ExpireRuntimeRecovery(ctx, time.Now().Add(2*time.Hour), 100); err != nil || expired != 1 {
		test.Fatal("terminal checkpoint did not expire")
	}
	retired, err := storage.GetRuntimeRecovery(ctx, "tenant", "session")
	if err != nil || retired.Phase != store.RuntimeRecoveryRetired || len(retired.Ciphertext) != 0 {
		test.Fatal("retirement failed")
	}
	if _, err := storage.GetRuntimeRecovery(ctx, "other", "session"); !errors.Is(err, store.ErrNotFound) {
		test.Fatal("cross-workspace recovery read")
	}
	if _, err := storage.ClaimRuntimeRecovery(ctx, "tenant", "session", retired.Version, "owner"); !errors.Is(err, store.ErrConflict) {
		test.Fatal("retired replay fence permitted execution")
	}
}
