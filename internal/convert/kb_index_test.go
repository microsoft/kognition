// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package convert_test

import (
	"reflect"
	"testing"
	"time"

	api "github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/convert"
)

func TestKnowledgeBaseIndexRunRoundTrip(t *testing.T) {
	ttlSeconds := int32(3600)
	transitionTime := time.Date(2026, time.August, 3, 14, 15, 0, 0, time.UTC)
	in := &api.KnowledgeBaseIndexRun{
		TypeMeta: api.TypeMeta{Kind: "KnowledgeBaseIndexRun", APIVersion: api.APIVersion},
		ObjectMeta: api.ObjectMeta{
			Name:        "support-corpus-20260803",
			Namespace:   "production",
			Labels:      map[string]string{"schedule": "support-corpus-hourly"},
			Annotations: map[string]string{"trigger": "scheduled"},
			Finalizers:  []string{"kognition.dev/knowledgebaseindexrun-cleanup"},
		},
		Spec: api.KnowledgeBaseIndexRunSpec{
			KnowledgeBaseName:       "support-corpus",
			TTLSecondsAfterFinished: &ttlSeconds,
		},
		Status: api.KnowledgeBaseIndexRunStatus{
			Phase:         api.PhaseSucceeded,
			JobName:       "support-corpus-index-7f8d9",
			StartedAt:     "2026-08-03T14:00:00Z",
			CompletedAt:   "2026-08-03T14:15:00Z",
			DocumentCount: 245,
			IndexedChunks: 1832,
			SourceDigest:  "sha256:9df7a1",
			BackendBinding: &api.KnowledgeBackendBinding{
				Generation: 4, ProviderName: "azuresearch", ClassUID: "class-uid", ClassGeneration: 2,
				ReclaimPolicy: api.ReclaimArchive, ConfigurationUID: "config-uid", ConfigurationDigest: "configuration-digest", OperationID: "team-a/index-run-4",
			},
			Message: "indexed 245 documents",
			Conditions: []api.Condition{{
				Type:               "Ready",
				Status:             "True",
				LastTransitionTime: transitionTime,
				Reason:             "IndexRunComplete",
				Message:            "index is current",
			}},
		},
	}

	out := convert.KnowledgeBaseIndexRunFromK8s(convert.KnowledgeBaseIndexRunToK8s(in))

	if out.TypeMeta != in.TypeMeta || out.ObjectMeta.Name != in.ObjectMeta.Name || out.ObjectMeta.Namespace != in.ObjectMeta.Namespace {
		t.Fatalf("index run identity changed: got %#v, want %#v", out, in)
	}
	if !reflect.DeepEqual(out.ObjectMeta.Labels, in.ObjectMeta.Labels) || !reflect.DeepEqual(out.ObjectMeta.Annotations, in.ObjectMeta.Annotations) || !reflect.DeepEqual(out.ObjectMeta.Finalizers, in.ObjectMeta.Finalizers) {
		t.Errorf("index run metadata changed: got %#v, want %#v", out.ObjectMeta, in.ObjectMeta)
	}
	if !reflect.DeepEqual(out.Spec, in.Spec) {
		t.Errorf("index run spec changed:\n got: %#v\nwant: %#v", out.Spec, in.Spec)
	}
	if !reflect.DeepEqual(out.Status, in.Status) {
		t.Errorf("index run status changed:\n got: %#v\nwant: %#v", out.Status, in.Status)
	}
}

func TestKnowledgeBaseIndexScheduleRoundTrip(t *testing.T) {
	suspend := true
	successfulHistory := int32(5)
	failedHistory := int32(2)
	startingDeadline := int64(180)
	ttlSeconds := int32(7200)
	transitionTime := time.Date(2026, time.August, 3, 15, 0, 0, 0, time.UTC)
	in := &api.KnowledgeBaseIndexSchedule{
		TypeMeta: api.TypeMeta{Kind: "KnowledgeBaseIndexSchedule", APIVersion: api.APIVersion},
		ObjectMeta: api.ObjectMeta{
			Name:        "support-corpus-hourly",
			Namespace:   "production",
			Labels:      map[string]string{"team": "support"},
			Annotations: map[string]string{"timezone": "UTC"},
			Finalizers:  []string{"kognition.dev/knowledgebaseindexschedule-cleanup"},
		},
		Spec: api.KnowledgeBaseIndexScheduleSpec{
			KnowledgeBaseName:          "support-corpus",
			Schedule:                   "0 * * * *",
			Suspend:                    &suspend,
			ConcurrencyPolicy:          api.ConcurrencyPolicyReplace,
			SuccessfulRunsHistoryLimit: &successfulHistory,
			FailedRunsHistoryLimit:     &failedHistory,
			StartingDeadlineSeconds:    &startingDeadline,
			RunTemplate: api.KnowledgeBaseIndexRunSpec{
				KnowledgeBaseName:       "ignored-template-name",
				TTLSecondsAfterFinished: &ttlSeconds,
			},
		},
		Status: api.KnowledgeBaseIndexScheduleStatus{
			Phase:              api.PhaseSuspended,
			LastScheduleTime:   "2026-08-03T14:00:00Z",
			LastSuccessfulTime: "2026-08-03T14:12:00Z",
			NextScheduleTime:   "2026-08-03T15:00:00Z",
			Active: []api.LocalObjectReference{
				{Name: "support-corpus-20260803-1400"},
				{Name: "support-corpus-20260803-1430"},
			},
			Conditions: []api.Condition{{
				Type:               "Ready",
				Status:             "False",
				LastTransitionTime: transitionTime,
				Reason:             "ScheduleSuspended",
				Message:            "new index runs are suspended",
			}},
		},
	}

	out := convert.KnowledgeBaseIndexScheduleFromK8s(convert.KnowledgeBaseIndexScheduleToK8s(in))

	if out.TypeMeta != in.TypeMeta || out.ObjectMeta.Name != in.ObjectMeta.Name || out.ObjectMeta.Namespace != in.ObjectMeta.Namespace {
		t.Fatalf("index schedule identity changed: got %#v, want %#v", out, in)
	}
	if !reflect.DeepEqual(out.ObjectMeta.Labels, in.ObjectMeta.Labels) || !reflect.DeepEqual(out.ObjectMeta.Annotations, in.ObjectMeta.Annotations) || !reflect.DeepEqual(out.ObjectMeta.Finalizers, in.ObjectMeta.Finalizers) {
		t.Errorf("index schedule metadata changed: got %#v, want %#v", out.ObjectMeta, in.ObjectMeta)
	}
	if !reflect.DeepEqual(out.Spec, in.Spec) {
		t.Errorf("index schedule policy changed:\n got: %#v\nwant: %#v", out.Spec, in.Spec)
	}
	if !reflect.DeepEqual(out.Status, in.Status) {
		t.Errorf("index schedule status changed:\n got: %#v\nwant: %#v", out.Status, in.Status)
	}
}

func TestKnowledgeBaseIndexScheduleRoundTrip_NoActiveRuns(t *testing.T) {
	in := &api.KnowledgeBaseIndexSchedule{
		TypeMeta:   api.TypeMeta{Kind: "KnowledgeBaseIndexSchedule", APIVersion: api.APIVersion},
		ObjectMeta: api.ObjectMeta{Name: "support-corpus-hourly", Namespace: "production"},
		Spec: api.KnowledgeBaseIndexScheduleSpec{
			KnowledgeBaseName: "support-corpus",
			Schedule:          "0 * * * *",
		},
	}

	out := convert.KnowledgeBaseIndexScheduleFromK8s(convert.KnowledgeBaseIndexScheduleToK8s(in))

	if out.Status.Active != nil {
		t.Errorf("expected active run references to remain absent, got %#v", out.Status.Active)
	}
}
