// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package conformancetest

import (
	"context"
	"errors"
	"testing"

	api "github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/runtimeapproval"
	"github.com/kognition-ai/kognition/internal/store"
)

type ExecutionDriver struct {
	Store    store.RuntimeApprovalExecutionStore
	NewReady func(*testing.T, string, string) api.RuntimeApproval
}

func RunPreDispatchDispositionContract(t *testing.T, driver ExecutionDriver) {
	t.Helper()
	ctx := context.Background()
	accepted := []string{
		runtimeapproval.ExecutionDispositionQuotaDenied,
		runtimeapproval.ExecutionDispositionReceiptConflict,
		runtimeapproval.ExecutionDispositionPolicyChanged,
		runtimeapproval.ExecutionDispositionTargetChanged,
		runtimeapproval.ExecutionDispositionSessionChanged,
		runtimeapproval.ExecutionDispositionPreparationRejected,
	}
	for _, disposition := range accepted {
		t.Run(disposition, func(t *testing.T) {
			approval := driver.NewReady(t, "conformance-abort-"+disposition, api.RuntimeApprovalDecisionNotRequired)
			aborted, err := driver.Store.AbortRuntimeApprovalBeforeDispatch(
				ctx, approval.Spec.Workspace, approval.Spec.InvocationID, approval.ResourceVersion, "", disposition,
			)
			if err != nil {
				t.Fatalf("abort with %s: %v", disposition, err)
			}
			if aborted.Status.ExecutionState != api.RuntimeApprovalExecutionFailed || aborted.Status.ExecutionDisposition != disposition {
				t.Fatalf("abort status = %q/%q, want %q/%q", aborted.Status.ExecutionState, aborted.Status.ExecutionDisposition, api.RuntimeApprovalExecutionFailed, disposition)
			}
		})
	}

	for _, disposition := range []string{
		runtimeapproval.ExecutionDispositionApprovalExpiredBeforeClaim,
		runtimeapproval.ExecutionDispositionAbandonedPreparation,
	} {
		t.Run("Reserved/"+disposition, func(t *testing.T) {
			approval := driver.NewReady(t, "conformance-reserved-"+disposition, api.RuntimeApprovalDecisionNotRequired)
			_, err := driver.Store.AbortRuntimeApprovalBeforeDispatch(
				ctx, approval.Spec.Workspace, approval.Spec.InvocationID, approval.ResourceVersion, "", disposition,
			)
			if !errors.Is(err, runtimeapproval.ErrPreDispatchDispositionInvalid) {
				t.Fatalf("abort with reserved disposition %s error = %v", disposition, err)
			}
		})
	}
}
