// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package modelgateway

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	api "github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/cpi"
	"github.com/kognition-ai/kognition/internal/k8s/callhealth"
	"github.com/kognition-ai/kognition/internal/modelhealth"
	"go.uber.org/zap"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestQueryEmbeddingPublishesCredentialScopedPassiveHealth(t *testing.T) {
	for _, tc := range []struct {
		name     string
		status   int
		calls    int
		reason   string
		degraded bool
	}{
		{"rate limited", http.StatusTooManyRequests, 3, cpi.ReasonRateLimited, true},
		{"billing", http.StatusPaymentRequired, 2, cpi.ReasonQuotaOrBilling, true},
		{"authentication", http.StatusUnauthorized, 2, cpi.ReasonAuthenticationFailed, true},
		{"invalid request", http.StatusBadRequest, 3, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			tracker := modelhealth.NewTracker()
			f := newQueryEmbeddingFixture(t, WithCallRecorder(tracker))
			f.providerStatus = tc.status
			scheme := runtime.NewScheme()
			if err := corev1.AddToScheme(scheme); err != nil {
				t.Fatal(err)
			}
			sink, err := callhealth.NewConfigMapStore(fake.NewClientBuilder().WithScheme(scheme).Build(), "control", "knowledge-gateway")
			if err != nil {
				t.Fatal(err)
			}
			reporter := modelhealth.NewReporter(tracker, sink, "query-reporter", zap.NewNop())
			for i := range tc.calls {
				if _, err := f.executor.EmbedQuery(ctx, f.claims, "docs", f.binding, "regions", fmt.Sprintf("query-failure-%d", i)); err == nil {
					t.Fatalf("call %d unexpectedly succeeded", i)
				}
			}
			if f.calls != tc.calls || len(f.audit.entries) != 2*tc.calls {
				t.Fatalf("calls=%d audit=%d, want one accounted attempt and two audit entries per query", f.calls, len(f.audit.entries))
			}
			if err := reporter.Flush(ctx); err != nil {
				t.Fatal(err)
			}
			records, err := sink.ListModelCallHealth(ctx, modelhealth.ReporterStaleAfter)
			if err != nil {
				t.Fatal(err)
			}
			var model api.Model
			if err := json.Unmarshal(f.storage.data["Model/team-a/embedding-model"], &model); err != nil {
				t.Fatal(err)
			}
			targets := modelhealth.TargetsFor(model.Spec.Provider, model.Spec.Endpoint, model.Spec.ModelID, fixtureCredentialFingerprint(), true)
			aggregates := modelhealth.MergeByTarget(records)
			verdict := modelhealth.EvaluateModel(aggregates, targets, modelhealth.Verdict{}, false)
			if verdict.Degraded != tc.degraded || verdict.Reason != tc.reason {
				t.Fatalf("verdict=%+v records=%+v, want degraded=%t reason=%q", verdict, records, tc.degraded, tc.reason)
			}
			if tc.reason == cpi.ReasonQuotaOrBilling || tc.reason == cpi.ReasonAuthenticationFailed {
				other := modelhealth.TargetsFor(model.Spec.Provider, model.Spec.Endpoint, model.Spec.ModelID, "different-account", true)
				if verdict := modelhealth.EvaluateModel(aggregates, other, modelhealth.Verdict{}, false); verdict.Degraded {
					t.Fatalf("another account was degraded: %+v", verdict)
				}
			}
			f.providerStatus = http.StatusOK
			for i := range 3 {
				if vector, err := f.executor.EmbedQuery(ctx, f.claims, "docs", f.binding, "regions", fmt.Sprintf("query-recovery-%d", i)); err != nil || len(vector) != 3 {
					t.Fatalf("recovery vector=%v error=%v", vector, err)
				}
				if i == 1 && verdict.Degraded {
					if err := reporter.Flush(ctx); err != nil {
						t.Fatal(err)
					}
					current, err := sink.ListModelCallHealth(ctx, modelhealth.ReporterStaleAfter)
					if err != nil {
						t.Fatal(err)
					}
					if got := modelhealth.EvaluateModel(modelhealth.MergeByTarget(current), targets, verdict, true); !got.Degraded {
						t.Fatalf("two successful embeddings healed too soon: %+v", got)
					}
				}
			}
			if err := reporter.Flush(ctx); err != nil {
				t.Fatal(err)
			}
			records, err = sink.ListModelCallHealth(ctx, modelhealth.ReporterStaleAfter)
			if err != nil {
				t.Fatal(err)
			}
			if got := modelhealth.EvaluateModel(modelhealth.MergeByTarget(records), targets, verdict, verdict.Degraded); got.Degraded {
				t.Fatalf("successful embeddings did not recover: %+v", got)
			}
			if f.calls != tc.calls+3 || len(f.audit.entries) != 2*(tc.calls+3) {
				t.Fatalf("calls=%d audit=%d, want no automatic retries or missing audit evidence", f.calls, len(f.audit.entries))
			}
		})
	}
}
