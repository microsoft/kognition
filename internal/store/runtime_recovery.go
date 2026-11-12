// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package store

import (
	"context"
	"encoding/json"
	"time"
)

const (
	RuntimeRecoveryPrepared = "prepared"
	RuntimeRecoveryClaimed  = "claimed"
	RuntimeRecoveryTerminal = "terminal"
	RuntimeRecoveryRetired  = "retired"
)

type RuntimeRecoveryRecord struct {
	Workspace     string          `json:"workspace"`
	SessionID     string          `json:"sessionID"`
	BindingDigest string          `json:"bindingDigest"`
	Phase         string          `json:"phase"`
	Owner         string          `json:"owner"`
	Version       int64           `json:"version"`
	Ciphertext    json.RawMessage `json:"ciphertext,omitempty"`
	ExpiresAt     time.Time       `json:"expiresAt"`
}

type RuntimeRecoveryStore interface {
	PrepareRuntimeRecovery(context.Context, RuntimeRecoveryRecord) (RuntimeRecoveryRecord, error)
	GetRuntimeRecovery(context.Context, string, string) (RuntimeRecoveryRecord, error)
	ClaimRuntimeRecovery(context.Context, string, string, int64, string) (RuntimeRecoveryRecord, error)
	CompleteRuntimeRecovery(context.Context, RuntimeRecoveryRecord) error
	ExpireRuntimeRecovery(context.Context, time.Time, int) (int, error)
}
