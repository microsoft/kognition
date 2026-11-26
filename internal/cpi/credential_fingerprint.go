// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cpi

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
)

// credentialTypeKey is the entry of a credential's data that names its type. It
// is a label, not secret material, and callers disagree on whether it stays in
// Data, so it never takes part in the fingerprint.
const credentialTypeKey = "type"

// CredentialFingerprint returns an opaque identifier of the secret material in
// creds, or the empty string when there is none. Calls that present the same
// secret share it, however the caller assembled the Credentials: the Type field,
// the "type" entry and empty values are ignored.
//
// It is a one-way hash meant to tell accounts apart, never to authenticate. Do
// not log it or export it to metrics; combine it with the call's destination
// before publishing it, as the model health keys do.
func CredentialFingerprint(creds *Credentials) string {
	if creds == nil {
		return ""
	}
	keys := make([]string, 0, len(creds.Data))
	for key, value := range creds.Data {
		if key != credentialTypeKey && value != "" {
			keys = append(keys, key)
		}
	}
	if len(keys) == 0 {
		return ""
	}
	sort.Strings(keys)
	digest := sha256.New()
	_, _ = digest.Write([]byte("kognition-credential-fingerprint-v1\x00"))
	for _, key := range keys {
		_, _ = digest.Write([]byte(key + "\x00" + creds.Data[key] + "\x00"))
	}
	return hex.EncodeToString(digest.Sum(nil))
}
