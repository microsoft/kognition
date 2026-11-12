// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package openai

import "testing"

func TestBuildEndpoint(t *testing.T) {
	tests := []struct {
		name   string
		base   string
		suffix string
		want   string
	}{
		{"bare host", "https://api.openai.com", "models", "https://api.openai.com/v1/models"},
		{"host with trailing slash", "https://api.openai.com/", "models", "https://api.openai.com/v1/models"},
		{"host already includes v1", "https://api.openai.com/v1", "models", "https://api.openai.com/v1/models"},
		{"host includes v1 and trailing slash", "https://api.openai.com/v1/", "models", "https://api.openai.com/v1/models"},
		{"chat completions suffix", "https://api.openai.com/v1", "chat/completions", "https://api.openai.com/v1/chat/completions"},
		{"embeddings on bare host", "https://api.openai.com", "embeddings", "https://api.openai.com/v1/embeddings"},
		{"local endpoint with v1", "http://localhost:8080/v1", "chat/completions", "http://localhost:8080/v1/chat/completions"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := buildEndpoint(tc.base, tc.suffix); got != tc.want {
				t.Fatalf("buildEndpoint(%q, %q) = %q; want %q", tc.base, tc.suffix, got, tc.want)
			}
		})
	}
}
