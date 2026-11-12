// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package observability

import (
	"strings"
	"testing"
)

func TestNormalizeTrustDecisionRunID(t *testing.T) {
	accepted := strings.Repeat("é", MaxTrustDecisionRunIDLength)
	for name, input := range map[string]string{
		"empty":   "",
		"trimmed": "  demo-run  ",
		"maximum": accepted,
	} {
		t.Run(name, func(t *testing.T) {
			got, err := NormalizeTrustDecisionRunID(input)
			if err != nil {
				t.Fatal(err)
			}
			if got != strings.TrimSpace(input) {
				t.Fatalf("NormalizeTrustDecisionRunID(%q) = %q", input, got)
			}
		})
	}

	if _, err := NormalizeTrustDecisionRunID(strings.Repeat("a", MaxTrustDecisionRunIDLength+1)); err == nil {
		t.Fatal("oversized run ID was accepted")
	}
}
