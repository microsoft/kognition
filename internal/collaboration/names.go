// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package collaboration

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
)

// Names and keys of the Kubernetes objects that record one collaboration attempt.
// Executors write them and read-only tooling derives them, so both share this one
// definition.
const (
	// LedgerStateKey is the ledger ConfigMap data key that holds the encoded ledger.
	LedgerStateKey = "state"
	// LabelComponent and ComponentValue label every ledger object.
	LabelComponent = "app.kubernetes.io/component"
	ComponentValue = "workflow-collaboration"
	// LabelMember labels a member AgentJob with the step-local member alias.
	LabelMember = "kognition.dev/collaboration-member"
)

// LedgerName returns the ledger ConfigMap name of one step attempt.
func LedgerName(namespace, runUID, runName, step string, attempt int32) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%s\x00%s\x00%s\x00%d", namespace, runUID, runName, step, attempt)))
	return "wfc-" + hex.EncodeToString(sum[:12])
}

// JobName returns the deterministic AgentJob name of one dispatch.
func JobName(namespace, runUID, step string, attempt int32, dispatchID string) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%s\x00%s\x00%d\x00%s", namespace, runUID, step, attempt, dispatchID)))
	return "wfcj-" + hex.EncodeToString(sum[:12])
}

// LabelHash returns the label-safe digest of a run UID or step name.
func LabelHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:8])
}
