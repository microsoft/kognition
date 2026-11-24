// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package modelhealth derives model health from the outcomes of real model calls
// ("passive" health). Processes that call models record outcomes in a Tracker and
// publish summaries through a Reporter; controllers merge the summaries from all
// reporters and evaluate them against a fixed policy.
package modelhealth

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"

	"github.com/kognition-ai/kognition/internal/cpi"
)

// TargetKey identifies the destination of model calls: the provider, the
// endpoint and the provider model ID. Calls and Models that share them share
// health. The key is a hash, so published records never contain the endpoint,
// which can carry credentials in its URL.
//
// The provider is canonicalized (for example "azure-openai" and "openai" both map
// to "openai-compatible"), and surrounding whitespace and trailing slashes are
// ignored in the endpoint. An empty endpoint means the driver default.
func TargetKey(provider, endpoint, modelID string) string {
	canonical := cpi.CanonicalProvisioner(strings.TrimSpace(provider))
	normalizedEndpoint := strings.TrimRight(strings.TrimSpace(endpoint), "/")
	sum := sha256.Sum256([]byte(canonical + "\x00" + normalizedEndpoint + "\x00" + strings.TrimSpace(modelID)))
	return hex.EncodeToString(sum[:])
}
