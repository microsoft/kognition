// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"regexp"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"go.uber.org/zap/zaptest"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/cpi"
)

// conditionReasonSchema is the pattern the CRDs enforce on condition reasons.
var conditionReasonSchema = regexp.MustCompile(`^[A-Za-z]([A-Za-z0-9_,:]*[A-Za-z0-9_])?$`)

func TestSanitizeConditionMessage(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "empty", in: "", want: ""},
		{name: "plain text unchanged", in: "HTTP 503 from Anthropic", want: "HTTP 503 from Anthropic"},
		{name: "unicode text unchanged", in: "Quota excedida: créditos esgotados", want: "Quota excedida: créditos esgotados"},
		{name: "newlines tabs and CR become one space", in: "line one\r\nline two\n\n\tline three", want: "line one line two line three"},
		{name: "leading and trailing whitespace trimmed", in: "  \n hello \t ", want: "hello"},
		{name: "only whitespace", in: " \n\t ", want: ""},
		{name: "NUL and C0 controls", in: "a\x00b\x07c\x1bd", want: "a b c d"},
		{name: "ANSI escape is neutralized", in: "\x1b[31mred\x1b[0m", want: "[31mred [0m"},
		{name: "C1 control and NEL", in: "a\u0085b\u009bc", want: "a b c"},
		{name: "unicode line and paragraph separators", in: "a\u2028b\u2029c", want: "a b c"},
		{name: "bidi override and zero width dropped", in: "ok\u202Eevil\u200B\u2066x", want: "okevilx"},
		{name: "invalid utf8 replaced", in: "bad\xff\xfebytes", want: "bad\uFFFDbytes"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := sanitizeConditionMessage(test.in); got != test.want {
				t.Errorf("sanitizeConditionMessage(%q) = %q, want %q", test.in, got, test.want)
			}
		})
	}
}

func TestSanitizeConditionMessageBoundsLengthOnRuneBoundary(t *testing.T) {
	got := sanitizeConditionMessage(strings.Repeat("é", 5*maxConditionMessageRunes))
	if !utf8.ValidString(got) || !strings.HasSuffix(got, "...") {
		t.Fatalf("truncated message invalid or missing marker: valid=%t suffix=%q", utf8.ValidString(got), got[len(got)-5:])
	}
	if utf8.RuneCountInString(got) != maxConditionMessageRunes {
		t.Errorf("rune count = %d, want exactly the %d-rune cap including the marker", utf8.RuneCountInString(got), maxConditionMessageRunes)
	}
	if want := strings.Repeat("é", maxConditionMessageRunes-len("...")) + "..."; got != want {
		t.Errorf("truncated message was not cut on the budget before the marker")
	}

	oneOver := strings.Repeat("a", maxConditionMessageRunes+1)
	if got := sanitizeConditionMessage(oneOver); utf8.RuneCountInString(got) != maxConditionMessageRunes || !strings.HasSuffix(got, "...") {
		t.Errorf("one rune over the limit: rune count = %d, want %d with marker", utf8.RuneCountInString(got), maxConditionMessageRunes)
	}

	exact := strings.Repeat("a", maxConditionMessageRunes)
	if got := sanitizeConditionMessage(exact); got != exact {
		t.Errorf("message of exactly the limit was altered (len %d)", len(got))
	}
	if got := sanitizeConditionMessage(exact + "   \n"); got != exact {
		t.Errorf("trailing whitespace after a limit-length message should not trigger truncation (len %d)", len(got))
	}

	spaced := strings.Repeat("a", maxConditionMessageRunes-len("...")-1) + " " + strings.Repeat("b", 10)
	want := strings.Repeat("a", maxConditionMessageRunes-len("...")-1) + "..."
	if got := sanitizeConditionMessage(spaced); got != want {
		t.Errorf("space left at the cut should be dropped before the marker, got tail %q", got[len(got)-8:])
	}
}

func TestSanitizeConditionReason(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "empty stays empty", in: "", want: ""},
		{name: "valid unchanged", in: "AuthenticationFailed", want: "AuthenticationFailed"},
		{name: "allowed punctuation kept inside", in: "Probe_Failed:429,retry", want: "Probe_Failed:429,retry"},
		{name: "spaces and symbols dropped", in: "Not Ready!", want: "NotReady"},
		{name: "leading digits and underscore trimmed", in: "42_Rate", want: "Rate"},
		{name: "trailing colon and comma trimmed", in: "Failed:,", want: "Failed"},
		{name: "newline injection removed", in: "Healthy\nReady=True", want: "HealthyReadyTrue"},
		{name: "non ascii letters dropped", in: "Falhou\u00e3o", want: "Falhouo"},
		{name: "nothing usable falls back", in: "123 !!!", want: "ProbeFailed"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := sanitizeConditionReason(test.in, "ProbeFailed")
			if got != test.want {
				t.Errorf("sanitizeConditionReason(%q) = %q, want %q", test.in, got, test.want)
			}
			if got != "" && !conditionReasonSchema.MatchString(got) {
				t.Errorf("result %q does not satisfy the CRD reason pattern", got)
			}
		})
	}

	long := sanitizeConditionReason(strings.Repeat("A", 5*maxConditionReasonRunes), "ProbeFailed")
	if len(long) != maxConditionReasonRunes || !conditionReasonSchema.MatchString(long) {
		t.Errorf("long reason length = %d, want %d and schema-valid", len(long), maxConditionReasonRunes)
	}
	capped := sanitizeConditionReason(strings.Repeat("A", maxConditionReasonRunes-1)+":,", "ProbeFailed")
	if !conditionReasonSchema.MatchString(capped) {
		t.Errorf("reason capped on a separator %q does not satisfy the CRD reason pattern", capped)
	}
}

type mockDriverWithProbe struct {
	mockDriver
	result *cpi.HealthResult
}

func (d *mockDriverWithProbe) Probe(_ context.Context, _ *v1alpha1.ModelSpec, _ *cpi.Credentials) (*cpi.HealthResult, error) {
	return d.result, nil
}

func TestModelReconciler_SanitizesProbeTextBeforePersisting(t *testing.T) {
	for _, available := range []bool{false, true} {
		name := "unavailable"
		if available {
			name = "available"
		}
		t.Run(name, func(t *testing.T) {
			s := newMemStore()
			reg := cpi.NewRegistry()
			reg.MustRegister(&mockDriverWithProbe{mockDriver: mockDriver{name: "openai-compatible"}, result: &cpi.HealthResult{
				Available: available,
				Reason:    "Bad Reason\nReady=True",
				Message:   "provider said:\r\n\x1b[31mall good\u202E " + strings.Repeat("x", 5000),
			}})
			r := NewModelReconciler(s, reg, zaptest.NewLogger(t))
			storeModel(t, s, v1alpha1.Model{
				TypeMeta:   v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "Model"},
				ObjectMeta: v1alpha1.ObjectMeta{Name: "m", Namespace: "default"},
				Spec:       v1alpha1.ModelSpec{Provider: "openai-compatible", ModelID: "gpt-4o", Endpoint: "https://model-gateway.internal/v1"},
			})

			reconcileModel(t, r, "default", "m")

			cond := findModelCondition(getModel(t, s, "default", "m"), "Ready")
			if cond == nil {
				t.Fatal("expected a Ready condition")
			}
			if cond.Reason != "BadReasonReadyTrue" || !conditionReasonSchema.MatchString(cond.Reason) {
				t.Errorf("Reason = %q, want a schema-valid sanitized reason", cond.Reason)
			}
			if strings.ContainsAny(cond.Message, "\r\n\x1b\u202E") || strings.Count(cond.Message, "x") > maxConditionMessageRunes ||
				!strings.HasPrefix(cond.Message, "provider said: [31mall good") || !strings.HasSuffix(cond.Message, "...") {
				t.Errorf("Message not sanitized and bounded: %q", cond.Message)
			}
		})
	}
}

func TestModelReconciler_EmptyProbeReasonGetsFallbackBeforePersisting(t *testing.T) {
	tests := []struct {
		name      string
		available bool
		reason    string
		want      string
	}{
		{name: "unavailable empty", available: false, want: "ProbeFailed"},
		{name: "available empty", available: true, want: "ProbeSucceeded"},
		{name: "unavailable unusable", available: false, reason: "!!! ", want: "ProbeFailed"},
		{name: "available unusable", available: true, reason: "!!! ", want: "ProbeSucceeded"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newMemStore()
			reg := cpi.NewRegistry()
			reg.MustRegister(&mockDriverWithProbe{mockDriver: mockDriver{name: "openai-compatible"}, result: &cpi.HealthResult{
				Available: tt.available,
				Reason:    tt.reason,
				Message:   "probe finished",
			}})
			r := NewModelReconciler(s, reg, zaptest.NewLogger(t))
			storeModel(t, s, v1alpha1.Model{
				TypeMeta:   v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "Model"},
				ObjectMeta: v1alpha1.ObjectMeta{Name: "m", Namespace: "default"},
				Spec:       v1alpha1.ModelSpec{Provider: "openai-compatible", ModelID: "gpt-4o", Endpoint: "https://model-gateway.internal/v1"},
			})

			reconcileModel(t, r, "default", "m")

			cond := findModelCondition(getModel(t, s, "default", "m"), "Ready")
			if cond == nil {
				t.Fatal("expected a Ready condition")
			}
			if cond.Reason != tt.want || !conditionReasonSchema.MatchString(cond.Reason) {
				t.Errorf("Reason = %q, want %q", cond.Reason, tt.want)
			}
		})
	}
}

func TestModelClassReconciler_SanitizesRetainedConditionOfDegradedClass(t *testing.T) {
	const unsafeMessage = "old\x1b[31mred\u202E text"
	transition := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	tests := []struct {
		name      string
		available bool
		threshold int32
	}{
		{name: "failed probe", available: false, threshold: 3},
		{name: "partial recovery", available: true, threshold: 3},
		{name: "full recovery", available: true, threshold: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			driver := &configurableMockDriver{
				name:        "openai-compatible",
				probeResult: &cpi.HealthResult{Available: tt.available, Reason: "OK", Message: "probe done"},
			}
			s := newMemStore()
			r := NewModelClassReconciler(s, registryWith(driver), zaptest.NewLogger(t))
			storeModelClass(t, s, v1alpha1.ModelClass{
				TypeMeta:   v1alpha1.TypeMeta{Kind: "ModelClass", APIVersion: "v1alpha1"},
				ObjectMeta: v1alpha1.ObjectMeta{Name: "legacy", Namespace: "default", Finalizers: []string{modelClassFinalizer}},
				Spec: v1alpha1.ModelClassSpec{
					Provisioner:  "openai",
					LatencyClass: "low",
					HealthProbe:  &v1alpha1.HealthProbeSpec{Endpoint: "https://api.example.com/health", IntervalSeconds: 10, Type: "inference-ping", SuccessThreshold: tt.threshold, FailureThreshold: 2},
				},
				Status: v1alpha1.ModelClassStatus{
					Phase:       v1alpha1.PhaseDegraded,
					Provisioned: true,
					Health:      "Degraded",
					Conditions: []v1alpha1.Condition{{
						Type: "Ready", Status: "False", Reason: "Down", Message: unsafeMessage, LastTransitionTime: transition,
					}},
				},
			})

			reconcileModelClass(t, r, "default", "legacy")

			got := getModelClass(t, s, "default", "legacy")
			var ready *v1alpha1.Condition
			for i := range got.Status.Conditions {
				if got.Status.Conditions[i].Type == "Ready" {
					ready = &got.Status.Conditions[i]
				}
			}
			if ready == nil {
				t.Fatal("expected a Ready condition")
			}
			if strings.ContainsAny(ready.Message, "\x1b\u202E") {
				t.Errorf("retained Ready message still contains control characters: %q", ready.Message)
			}
			if tt.threshold > 1 && (ready.Status != "False" || !ready.LastTransitionTime.Equal(transition) || ready.Reason != "Down") {
				t.Errorf("retained condition changed beyond text sanitization: %+v", *ready)
			}
		})
	}
}

func TestModelClassReconciler_SanitizesProbeTextBeforePersisting(t *testing.T) {
	driver := &configurableMockDriver{
		name: "openai-compatible",
		probeResult: &cpi.HealthResult{
			Available: false,
			Reason:    "Down",
			Message:   "line1\nline2\x1b[0m\u202E " + strings.Repeat("y", 5000),
		},
	}
	s := newMemStore()
	r := NewModelClassReconciler(s, registryWith(driver), zaptest.NewLogger(t))
	storeModelClass(t, s, v1alpha1.ModelClass{
		TypeMeta:   v1alpha1.TypeMeta{Kind: "ModelClass", APIVersion: "v1alpha1"},
		ObjectMeta: v1alpha1.ObjectMeta{Name: "hostile", Namespace: "default", Finalizers: []string{modelClassFinalizer}},
		Spec: v1alpha1.ModelClassSpec{
			Provisioner:  "openai",
			LatencyClass: "low",
			HealthProbe:  &v1alpha1.HealthProbeSpec{Endpoint: "https://api.example.com/health", IntervalSeconds: 10, Type: "inference-ping", FailureThreshold: 1},
		},
		Status: v1alpha1.ModelClassStatus{Phase: v1alpha1.PhaseReady, Provisioned: true, Health: "Healthy"},
	})

	reconcileModelClass(t, r, "default", "hostile")

	got := getModelClass(t, s, "default", "hostile")
	var ready *v1alpha1.Condition
	for i := range got.Status.Conditions {
		if got.Status.Conditions[i].Type == "Ready" {
			ready = &got.Status.Conditions[i]
		}
	}
	if got.Status.Phase != v1alpha1.PhaseDegraded || ready == nil {
		t.Fatalf("expected a Degraded class with a Ready condition, got phase %q, condition %v", got.Status.Phase, ready)
	}
	if !conditionReasonSchema.MatchString(ready.Reason) {
		t.Errorf("Reason = %q does not satisfy the CRD reason pattern", ready.Reason)
	}
	if strings.ContainsAny(ready.Message, "\r\n\x1b\u202E") || !strings.HasPrefix(ready.Message, "inference-ping: endpoint unavailable: line1 line2 [0m") || !strings.HasSuffix(ready.Message, "...") {
		t.Errorf("Message not sanitized and bounded: %q", ready.Message)
	}
}
