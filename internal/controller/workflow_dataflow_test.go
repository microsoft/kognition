// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/kognition-ai/kognition/api/v1alpha1"
)

func TestBuildTemplateContext_PopulatesStepsInputParams(t *testing.T) {
	statuses := []v1alpha1.WorkflowStepStatus{
		{Name: "fetch", Phase: v1alpha1.PhaseReady, Output: "data from fetch"},
		{Name: "parse", Phase: v1alpha1.PhaseRunning, Output: "partial"},
	}
	ctx := buildTemplateContext(statuses, "initial input", map[string]string{"env": "prod"})

	if ctx.Input != "initial input" {
		t.Fatalf("Input = %q, want %q", ctx.Input, "initial input")
	}
	if ctx.Params["env"] != "prod" {
		t.Fatalf("Params[env] = %q, want %q", ctx.Params["env"], "prod")
	}
	if ctx.Steps["fetch"].Output != "data from fetch" {
		t.Fatalf("Steps[fetch].Output = %q, want %q", ctx.Steps["fetch"].Output, "data from fetch")
	}
	if ctx.Steps["fetch"].Phase != string(v1alpha1.PhaseReady) {
		t.Fatalf("Steps[fetch].Phase = %q, want %q", ctx.Steps["fetch"].Phase, v1alpha1.PhaseReady)
	}
	if ctx.Steps["parse"].Output != "partial" {
		t.Fatalf("Steps[parse].Output = %q, want %q", ctx.Steps["parse"].Output, "partial")
	}
}

func TestBuildTemplateContext_NilParamsBecomesEmptyMap(t *testing.T) {
	ctx := buildTemplateContext(nil, "", nil)
	if ctx.Params == nil {
		t.Fatal("expected non-nil Params map")
	}
	if len(ctx.Steps) != 0 {
		t.Fatalf("expected empty Steps, got %d", len(ctx.Steps))
	}
}

func TestResolveStepInput_NoTemplate(t *testing.T) {
	got, err := resolveStepInput("plain text", templateContext{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "plain text" {
		t.Fatalf("got %q, want %q", got, "plain text")
	}
}

func TestResolveStepInput_ReferencesStepOutput(t *testing.T) {
	ctx := templateContext{
		Steps: map[string]stepData{
			"fetch": {Output: "hello world", Phase: "Ready"},
		},
		Input:  "workflow input",
		Params: map[string]string{},
	}
	got, err := resolveStepInput("Summarize: {{ .Steps.fetch.Output }}", ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "Summarize: hello world" {
		t.Fatalf("got %q, want %q", got, "Summarize: hello world")
	}
}

func TestResolveStepInput_ReferencesWorkflowInput(t *testing.T) {
	ctx := templateContext{
		Steps:  map[string]stepData{},
		Input:  "my workflow input",
		Params: map[string]string{},
	}
	got, err := resolveStepInput("Process: {{ .Input }}", ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "Process: my workflow input" {
		t.Fatalf("got %q, want %q", got, "Process: my workflow input")
	}
}

func TestResolveStepInput_ReferencesParams(t *testing.T) {
	ctx := templateContext{
		Steps:  map[string]stepData{},
		Input:  "",
		Params: map[string]string{"env": "staging", "region": "us-east"},
	}
	got, err := resolveStepInput("Deploy to {{ .Params.env }} in {{ .Params.region }}", ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "Deploy to staging in us-east" {
		t.Fatalf("got %q, want %q", got, "Deploy to staging in us-east")
	}
}

func TestResolveStepInput_MissingKeyReturnsError(t *testing.T) {
	ctx := templateContext{
		Steps:  map[string]stepData{},
		Input:  "",
		Params: map[string]string{},
	}
	_, err := resolveStepInput("{{ .Steps.nonexistent.Output }}", ctx)
	if err == nil {
		t.Fatal("expected error for missing key")
	}
}

func TestResolveStepInput_InvalidTemplateSyntax(t *testing.T) {
	_, err := resolveStepInput("{{ .Steps.fetch.Output", templateContext{})
	if err == nil {
		t.Fatal("expected error for invalid template syntax")
	}
}

func TestResolveStepInput_ChainsMultipleStepOutputs(t *testing.T) {
	ctx := templateContext{
		Steps: map[string]stepData{
			"A": {Output: "alpha", Phase: "Ready"},
			"B": {Output: "beta", Phase: "Ready"},
		},
		Input:  "",
		Params: map[string]string{},
	}
	got, err := resolveStepInput("A={{ .Steps.A.Output }} B={{ .Steps.B.Output }}", ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "A=alpha B=beta" {
		t.Fatalf("got %q, want %q", got, "A=alpha B=beta")
	}
}

func TestResolveStepInput_ReferencesStepPhase(t *testing.T) {
	ctx := templateContext{
		Steps: map[string]stepData{
			"check": {Output: "ok", Phase: "Ready"},
		},
		Input:  "",
		Params: map[string]string{},
	}
	got, err := resolveStepInput("Phase: {{ .Steps.check.Phase }}", ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "Phase: Ready" {
		t.Fatalf("got %q, want %q", got, "Phase: Ready")
	}
}

func TestTruncateOutput_WithinLimit(t *testing.T) {
	output := strings.Repeat("a", maxOutputBytes)
	got, truncated := truncateOutput(output)
	if truncated {
		t.Fatal("expected no truncation")
	}
	if got != output {
		t.Fatal("output should be unchanged")
	}
}

func TestTruncateOutput_ExceedsLimit(t *testing.T) {
	output := strings.Repeat("a", maxOutputBytes+100)
	got, truncated := truncateOutput(output)
	if !truncated {
		t.Fatal("expected truncation")
	}
	if len(got) != maxOutputBytes+len(truncationMarker) {
		t.Fatalf("truncated length = %d, want %d", len(got), maxOutputBytes+len(truncationMarker))
	}
	if !strings.HasSuffix(got, truncationMarker) {
		t.Fatal("expected truncation marker suffix")
	}
}

func TestTruncateOutput_DoesNotSplitUTF8Rune(t *testing.T) {
	// Build a string of (maxOutputBytes-1) ASCII bytes followed by a 3-byte
	// UTF-8 rune ('€' = 0xE2 0x82 0xAC). A naive byte-slice at maxOutputBytes
	// would cut inside the '€', producing invalid UTF-8.
	prefix := strings.Repeat("a", maxOutputBytes-1)
	input := prefix + "€" + "tail" // total = maxOutputBytes-1 + 3 + 4 = maxOutputBytes+6
	got, truncated := truncateOutput(input)
	if !truncated {
		t.Fatal("expected truncation")
	}
	if !utf8.ValidString(got) {
		t.Fatal("truncated output is not valid UTF-8")
	}
	// The '€' rune should be excluded because including it partially
	// would produce invalid UTF-8.
	body := got[:len(got)-len(truncationMarker)]
	if strings.Contains(body, "€") {
		t.Fatal("expected the split rune to be excluded")
	}
	if !strings.HasSuffix(got, truncationMarker) {
		t.Fatal("expected truncation marker suffix")
	}
}

func TestTruncateOutput_EmptyString(t *testing.T) {
	got, truncated := truncateOutput("")
	if truncated {
		t.Fatal("expected no truncation for empty string")
	}
	if got != "" {
		t.Fatalf("got %q, want empty", got)
	}
}

func TestContainsTemplateDirective(t *testing.T) {
	tests := []struct {
		input string
		want  bool
	}{
		{"plain text", false},
		{"{{ .Steps.A.Output }}", true},
		{"prefix {{ .Input }} suffix", true},
		{"{not a template}", false},
		{"", false},
	}
	for _, tt := range tests {
		if got := containsTemplateDirective(tt.input); got != tt.want {
			t.Errorf("containsTemplateDirective(%q) = %v, want %v", tt.input, got, tt.want)
		}
	}
}
