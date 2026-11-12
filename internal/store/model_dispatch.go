// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package store

import (
	"context"
	"encoding/json"
)

type ModelDispatchRecord struct {
	SessionID string
	Response  json.RawMessage
}

type ModelDispatchStore interface {
	ClaimModelDispatch(ctx context.Context, namespace, operation, sessionID string) (bool, error)
	CompleteModelDispatch(ctx context.Context, namespace, operation, sessionID string, response json.RawMessage) error
	GetModelDispatch(ctx context.Context, namespace, operation string) (ModelDispatchRecord, error)
}
