// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package runtimeapproval

import (
	"context"
	"errors"
	"testing"
)

const validConfiguredBindingKeys = `[{"version":1,"epoch":1,"algorithm":"HMAC-SHA-256","domain":"kognition.dev/runtime-approval/action/v1","status":"Active","material":"MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY="}]`

func TestBindingKeyStoreFromJSON(t *testing.T) {
	keys, err := BindingKeyStoreFromJSON(validConfiguredBindingKeys)
	if err != nil {
		t.Fatalf("BindingKeyStoreFromJSON: %v", err)
	}
	active, err := keys.Active(context.Background())
	if err != nil {
		t.Fatalf("Active: %v", err)
	}
	if active.Version != 1 || active.Epoch != 1 || len(active.Material) != 32 {
		t.Fatalf("active key = %#v", active)
	}
}

func TestBindingKeyStoreFromJSONRejectsInvalidConfiguration(t *testing.T) {
	tests := map[string]string{
		"missing":          "",
		"unknown field":    `[{"version":1,"epoch":1,"algorithm":"HMAC-SHA-256","domain":"kognition.dev/runtime-approval/action/v1","status":"Active","material":"MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY=","extra":true}]`,
		"multiple values":  validConfiguredBindingKeys + ` []`,
		"invalid material": `[{"version":1,"epoch":1,"algorithm":"HMAC-SHA-256","domain":"kognition.dev/runtime-approval/action/v1","status":"Active","material":"%%%"}]`,
	}
	for name, encoded := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := BindingKeyStoreFromJSON(encoded); err == nil || errors.Is(err, context.Canceled) {
				t.Fatalf("BindingKeyStoreFromJSON(%q) error = %v", encoded, err)
			}
		})
	}
}
