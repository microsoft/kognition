// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package conformance

import (
	"strings"
	"testing"
	"time"

	"github.com/kognition-ai/kognition/internal/runtime"
)

func TestValidateCheckpoint(t *testing.T) {
	valid := runtime.CheckpointResponse{Token: []byte("checkpoint"), CreatedAt: time.Now()}
	if err := validateCheckpoint(valid); err != nil {
		t.Fatalf("valid checkpoint: %v", err)
	}
	if err := validateCheckpoint(runtime.CheckpointResponse{CreatedAt: time.Now()}); err == nil || !strings.Contains(err.Error(), "token") {
		t.Fatalf("missing token error = %v", err)
	}
	if err := validateCheckpoint(runtime.CheckpointResponse{Token: []byte("checkpoint")}); err == nil || !strings.Contains(err.Error(), "timestamp") {
		t.Fatalf("missing timestamp error = %v", err)
	}
}

func TestValidateResume(t *testing.T) {
	if err := validateResume(runtime.ResumeResponse{SessionID: "session"}); err != nil {
		t.Fatalf("valid resume: %v", err)
	}
	if err := validateResume(runtime.ResumeResponse{}); err == nil || !strings.Contains(err.Error(), "session ID") {
		t.Fatalf("missing session error = %v", err)
	}
}

func TestValidateSessionID(t *testing.T) {
	if err := validateSessionID("session"); err != nil {
		t.Fatalf("valid session ID: %v", err)
	}
	if err := validateSessionID(""); err == nil || !strings.Contains(err.Error(), "session ID") {
		t.Fatalf("missing session error = %v", err)
	}
}

func TestValidateCancellation(t *testing.T) {
	if err := validateCancellation(runtime.CancelResponse{State: runtime.CancelAcknowledged}); err != nil {
		t.Fatalf("acknowledged cancellation: %v", err)
	}
	if err := validateCancellation(runtime.CancelResponse{}); err == nil || !strings.Contains(err.Error(), "unspecified") {
		t.Fatalf("unspecified cancellation error = %v", err)
	}
}

func TestValidateHealth(t *testing.T) {
	if err := validateHealth(runtime.HealthStatus{State: runtime.HealthHealthy}); err != nil {
		t.Fatalf("healthy status: %v", err)
	}
	if err := validateHealth(runtime.HealthStatus{}); err == nil || !strings.Contains(err.Error(), "known health state") {
		t.Fatalf("unknown health error = %v", err)
	}
}

func TestValidateRuntimeVersion(t *testing.T) {
	if err := validateRuntimeVersion(runtime.Capabilities{RuntimeVersion: "1.0.0"}); err != nil {
		t.Fatalf("valid runtime version: %v", err)
	}
	if err := validateRuntimeVersion(runtime.Capabilities{}); err == nil || !strings.Contains(err.Error(), "runtime version") {
		t.Fatalf("missing runtime version error = %v", err)
	}
}

func TestValidateUniqueSessionIDs(t *testing.T) {
	if err := validateUniqueSessionIDs("first", "second"); err != nil {
		t.Fatalf("unique session IDs: %v", err)
	}
	if err := validateUniqueSessionIDs("same", "same"); err == nil || !strings.Contains(err.Error(), "unique") {
		t.Fatalf("duplicate session error = %v", err)
	}
}
