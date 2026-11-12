// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cmd

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/kognition-ai/kognition/internal/admission"
)

func TestArtifactSigningPayloadMatchesAdmission(test *testing.T) {
	input := `{"kind":"Tool","metadata":{"name":"search","namespace":"tenant","resourceVersion":"7"},"spec":{"endpoint":"https://provider.example.test","signature":"old"},"status":{"phase":"Ready"}}`
	command := newArtifactCommand()
	command.SetArgs([]string{"signing-payload", "-f", "-"})
	command.SetIn(strings.NewReader(input))
	var output bytes.Buffer
	command.SetOut(&output)
	if err := command.Execute(); err != nil {
		test.Fatal(err)
	}
	expected, err := admission.ArtifactSignaturePayload("Tool", "tenant", "search", []byte(input))
	if err != nil {
		test.Fatal(err)
	}
	if !bytes.Equal(output.Bytes(), expected) || bytes.HasSuffix(output.Bytes(), []byte("\n")) {
		test.Fatal("CLI signing bytes differ from admission payload")
	}
	var payload map[string]any
	if err := json.Unmarshal(output.Bytes(), &payload); err != nil {
		test.Fatal(err)
	}
	if strings.Contains(output.String(), "resourceVersion") || strings.Contains(output.String(), "signature") && strings.Contains(output.String(), "\"old\"") || strings.Contains(output.String(), "Ready") {
		test.Fatal("server-owned state or old signature entered signing payload")
	}
}

func TestArtifactSigningPayloadRejectsAmbiguousInputs(test *testing.T) {
	for _, input := range []string{
		`kind: Tool`,
		`{"kind":"Agent","metadata":{"name":"worker","namespace":"tenant"},"spec":{}}`,
		"kind: Tool\nmetadata:\n  name: first\n  name: second\n  namespace: tenant\nspec: {}\n",
		"kind: Tool\nmetadata:\n  name: search\n  namespace: tenant\nspec: {}\n---\nkind: Tool\n",
		strings.Repeat("x", (4<<20)+1),
	} {
		command := newArtifactCommand()
		command.SetArgs([]string{"signing-payload", "-f", "-"})
		command.SetIn(strings.NewReader(input))
		var output bytes.Buffer
		command.SetOut(&output)
		command.SetErr(&bytes.Buffer{})
		command.SilenceUsage = true
		if err := command.Execute(); err == nil || output.Len() != 0 {
			test.Fatal("ambiguous or invalid artifact produced signing bytes")
		}
	}
}
