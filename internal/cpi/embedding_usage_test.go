// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cpi_test

import (
	"testing"

	"github.com/kognition-ai/kognition/internal/cpi"
)

func TestEmbeddingUsageRequiresExplicitConsistentProviderCounts(t *testing.T) {
	value := func(number int) *int { return &number }
	for _, test := range []struct {
		name     string
		prompt   *int
		total    *int
		reported bool
	}{
		{name: "absent"},
		{name: "missing prompt", total: value(3)},
		{name: "missing total", prompt: value(3)},
		{name: "negative", prompt: value(-1), total: value(-1)},
		{name: "inconsistent", prompt: value(2), total: value(3)},
		{name: "explicit zero", prompt: value(0), total: value(0), reported: true},
		{name: "reported", prompt: value(3), total: value(3), reported: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			usage := cpi.NormalizeEmbeddingUsage(test.prompt, test.total)
			if usage.Reported != test.reported || (test.reported && (usage.PromptTokens != *test.prompt || usage.TotalTokens != *test.total || usage.CompletionTokens != 0)) {
				t.Fatalf("usage=%+v", usage)
			}
		})
	}
}
