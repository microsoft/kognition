// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cmd

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	api "github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/modelinvocation"
)

func TestInvocationProfileDigestCommandIsOfflineAndCanonical(t *testing.T) {
	maximum := int64(128)
	spec := api.ModelInvocationProfileSpec{Operation: "chat", Defaults: api.InvocationDefaults{MaxCompletionTokens: &maximum}}
	expected, err := modelinvocation.Digest(spec)
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "profile.yaml")
	if err := os.WriteFile(file, []byte("apiVersion: kognition.dev/v1alpha1\nkind: ModelInvocationProfile\nmetadata:\n  name: portable\nspec:\n  defaults:\n    maxCompletionTokens: 128\n  operation: chat\n"), 0600); err != nil {
		t.Fatal(err)
	}
	command := newInvocationProfileDigestCommand()
	var output bytes.Buffer
	command.SetOut(&output)
	command.SetArgs([]string{"-f", file})
	if err := command.Execute(); err != nil || strings.TrimSpace(output.String()) != expected {
		t.Fatalf("digest=%q, error=%v", output.String(), err)
	}
	encoded, err := json.Marshal(api.ModelInvocationProfile{TypeMeta: api.TypeMeta{APIVersion: api.APIVersion, Kind: "ModelInvocationProfile"}, Spec: spec})
	if err != nil {
		t.Fatal(err)
	}
	if actual, err := readInvocationProfileDigest(bytes.NewReader(encoded), ""); err != nil || actual != expected {
		t.Fatalf("JSON/YAML digest differs: %s, %v", actual, err)
	}
	if _, err := readInvocationProfileDigest(strings.NewReader(string(encoded)+"\n---\n"+string(encoded)), ""); err == nil {
		t.Fatal("ambiguous profile selection accepted")
	}
}
