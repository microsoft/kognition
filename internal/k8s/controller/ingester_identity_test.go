// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"strings"
	"testing"

	"go.uber.org/zap"
)

func TestConfiguredIngesterRejectsElevatedIdentityBeforeManagerAccess(t *testing.T) {
	_, err := newConfiguredKBIngesterLauncher(nil, nil, zap.NewNop(), KBIngesterOptions{Image: "ingester:test", ServiceAccount: "privileged"})
	if err == nil || !strings.Contains(err.Error(), "ServiceAccount must be empty or default") {
		t.Fatalf("got %v, want unsupported identity error", err)
	}
}
