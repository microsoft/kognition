// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package apiserver

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/kognition-ai/kognition/internal/store"
)

func TestWriteRuntimeApprovalStoreErrorMapsSelfApprovalToForbidden(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want int
	}{
		{name: "self approval", err: fmt.Errorf("decide: %w", store.ErrSelfApproval), want: http.StatusForbidden},
		{name: "conflict", err: store.ErrConflict, want: http.StatusConflict},
		{name: "not found", err: store.ErrNotFound, want: http.StatusNotFound},
		{name: "other", err: errors.New("boom"), want: http.StatusInternalServerError},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			writeRuntimeApprovalStoreError(recorder, test.err, "decision failed")
			if recorder.Code != test.want {
				t.Fatalf("status = %d, want %d", recorder.Code, test.want)
			}
		})
	}
}
