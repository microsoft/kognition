// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package recovery

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func testCipher(test *testing.T, current string, keys map[string]string) *Cipher {
	test.Helper()
	configuration, _ := json.Marshal(map[string]WorkspaceKeys{"tenant": {Current: current, Keys: keys}})
	crypto, err := NewCipher(configuration)
	if err != nil {
		test.Fatal(err)
	}
	return crypto
}

func TestCheckpointEncryptionBindingRotationAndExpiry(test *testing.T) {
	oldKey := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{1}, 32))
	newKey := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{2}, 32))
	crypto := testCipher(test, "old", map[string]string{"old": oldKey})
	now := time.Now().UTC()
	binding := Binding{Workspace: "tenant", AgentUID: "agent-uid", InstanceUID: "instance-uid", SessionID: "session", Runtime: "autogen/0.7.5", ConfigurationDigest: strings.Repeat("a", 64)}
	sealed, err := crypto.Seal(binding, []byte("sensitive history"), now.Add(7*24*time.Hour), now)
	if err != nil || bytes.Contains(sealed, []byte("sensitive history")) || bytes.Contains(sealed, []byte(oldKey)) {
		test.Fatal("checkpoint was not encrypted")
	}
	second, err := crypto.Seal(binding, []byte("sensitive history"), now.Add(7*24*time.Hour), now)
	if err != nil || bytes.Equal(second, sealed) {
		test.Fatal("checkpoint nonce was reused")
	}
	rotated := testCipher(test, "new", map[string]string{"old": oldKey, "new": newKey})
	if plaintext, err := rotated.Open(binding, sealed, now); err != nil || string(plaintext) != "sensitive history" {
		test.Fatal("retained read key did not restore")
	}
	for _, mutate := range []func(*Binding){func(value *Binding) { value.Workspace = "other" }, func(value *Binding) { value.AgentUID = "other" }, func(value *Binding) { value.InstanceUID = "other" }, func(value *Binding) { value.SessionID = "other" }, func(value *Binding) { value.Generation++ }, func(value *Binding) { value.ConfigurationDigest = strings.Repeat("b", 64) }, func(value *Binding) { value.Runtime = "other" }} {
		wrong := binding
		mutate(&wrong)
		if _, err := rotated.Open(wrong, sealed, now); err == nil {
			test.Fatal("checkpoint binding substitution accepted")
		}
	}
	if _, err := rotated.Open(binding, sealed, now.Add(8*24*time.Hour)); err == nil {
		test.Fatal("expired checkpoint restored")
	}
	removed := testCipher(test, "new", map[string]string{"new": newKey})
	if _, err := removed.Open(binding, sealed, now); err == nil {
		test.Fatal("removed key silently replaced")
	}
	var tampered map[string]interface{}
	_ = json.Unmarshal(sealed, &tampered)
	tampered["expiresAt"] = now.Add(30 * 24 * time.Hour).Format(time.RFC3339Nano)
	modified, _ := json.Marshal(tampered)
	if _, err := rotated.Open(binding, modified, now); err == nil {
		test.Fatal("expiry AAD tampering accepted")
	}
}

func TestCheckpointCipherRejectsAmbiguousKeysAndOversizedState(test *testing.T) {
	if _, err := NewCipher([]byte(`{"tenant":{"current":"first","current":"second","keys":{}}}`)); err == nil {
		test.Fatal("duplicate encryption key selector accepted")
	}
	key := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{3}, 32))
	crypto := testCipher(test, "key", map[string]string{"key": key})
	now := time.Now().UTC()
	binding := Binding{Workspace: "tenant", AgentUID: "agent", InstanceUID: "instance", SessionID: "session", Runtime: "autogen/0.7.5", ConfigurationDigest: strings.Repeat("a", 64)}
	if _, err := crypto.Seal(binding, make([]byte, MaxPayloadBytes+1), now.Add(time.Hour), now); err == nil {
		test.Fatal("unbounded checkpoint accepted")
	}
	if _, err := crypto.Seal(binding, []byte("state"), now.Add(31*24*time.Hour), now); err == nil {
		test.Fatal("retention contract exceeded")
	}
}
