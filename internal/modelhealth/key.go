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

// CredentialKey identifies the evidence about one credential at a destination.
// Rejected credentials and exhausted quota or billing belong to the account that
// made the call, not to the destination, so they are tracked under this key and
// never under the destination's TargetKey. credential is the call's
// cpi.CredentialFingerprint, empty for calls that present none.
//
// The key hashes the destination together with the credential, so a published
// record reveals neither, and it differs for every destination even when one
// credential is used at several.
func CredentialKey(target, credential string) string {
	sum := sha256.Sum256([]byte("credential\x00" + target + "\x00" + credential))
	return hex.EncodeToString(sum[:])
}

// credentialScoped reports whether a failure reason is about the credential that
// made the call rather than the destination it called.
func credentialScoped(reason string) bool {
	switch cpi.NormalizeReason(reason) {
	case cpi.ReasonAuthenticationFailed, cpi.ReasonQuotaOrBilling:
		return true
	default:
		return false
	}
}

// Targets are the keys of the evidence that applies to one Model.
type Targets struct {
	// Destination is the TargetKey of the provider, endpoint and model ID.
	Destination string
	// Credential is the CredentialKey of the credential the Model's calls
	// present. It is empty when that credential could not be determined, in which
	// case credential failures are not attributed to the Model at all, rather than
	// to whichever account happened to fail.
	Credential string
}

// TargetsFor returns the Targets of a Model. credential is the Model's
// cpi.CredentialFingerprint and known reports whether it could be determined.
func TargetsFor(provider, endpoint, modelID, credential string, known bool) Targets {
	targets := Targets{Destination: TargetKey(provider, endpoint, modelID)}
	if known {
		targets.Credential = CredentialKey(targets.Destination, credential)
	}
	return targets
}

// Contains reports whether target is one of the keys.
func (t Targets) Contains(target string) bool {
	return target == t.Destination || (t.Credential != "" && target == t.Credential)
}
