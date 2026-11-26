// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cpi

import (
	"context"
	"strings"
	"testing"
)

func TestCredentialFingerprintIsEmptyWithoutSecretMaterial(t *testing.T) {
	for name, creds := range map[string]*Credentials{
		"nil":                 nil,
		"no data":             {Type: "api-key"},
		"empty data":          {Type: "api-key", Data: map[string]string{}},
		"only the type entry": {Data: map[string]string{"type": "api-key"}},
		"empty values":        {Data: map[string]string{"apiKey": "", "region": ""}},
	} {
		if got := CredentialFingerprint(creds); got != "" {
			t.Errorf("%s: CredentialFingerprint() = %q, want none", name, got)
		}
	}
}

func TestCredentialFingerprintIdentifiesTheSecret(t *testing.T) {
	base := CredentialFingerprint(&Credentials{Type: "api-key", Data: map[string]string{"apiKey": "sk-secret-value"}})
	if len(base) != 64 || strings.Trim(base, "0123456789abcdef") != "" {
		t.Fatalf("CredentialFingerprint() = %q, want 64 lowercase hex characters", base)
	}
	if strings.Contains(base, "secret") {
		t.Fatal("the fingerprint must not contain the secret")
	}

	// Callers assemble the same secret differently: the reconciler and the model
	// gateway keep the "type" entry in Data, the chat handler and dispatcher strip
	// it, and some leave Type empty.
	same := map[string]*Credentials{
		"type stripped from the data": {Type: "api-key", Data: map[string]string{"apiKey": "sk-secret-value"}},
		"type kept in the data":       {Type: "api-key", Data: map[string]string{"apiKey": "sk-secret-value", "type": "api-key"}},
		"no Type field":               {Data: map[string]string{"apiKey": "sk-secret-value"}},
		"another Type label":          {Type: "bearer", Data: map[string]string{"apiKey": "sk-secret-value", "type": "bearer"}},
		"an empty extra value":        {Data: map[string]string{"apiKey": "sk-secret-value", "region": ""}},
	}
	for name, creds := range same {
		if got := CredentialFingerprint(creds); got != base {
			t.Errorf("%s: CredentialFingerprint() = %q, want the same fingerprint as the plain credential", name, got)
		}
	}

	different := map[string]*Credentials{
		"another secret":      {Data: map[string]string{"apiKey": "sk-other-value"}},
		"another key name":    {Data: map[string]string{"token": "sk-secret-value"}},
		"an additional field": {Data: map[string]string{"apiKey": "sk-secret-value", "region": "eu"}},
		"a swapped pair":      {Data: map[string]string{"sk-secret-value": "apiKey"}},
	}
	for name, creds := range different {
		if got := CredentialFingerprint(creds); got == base || got == "" {
			t.Errorf("%s: CredentialFingerprint() = %q, want a fingerprint different from the plain credential", name, got)
		}
	}
}

func TestCredentialFingerprintIsIndependentOfMapOrderAndFieldBoundaries(t *testing.T) {
	data := map[string]string{"a": "1", "b": "2", "c": "3", "d": "4", "e": "5"}
	first := CredentialFingerprint(&Credentials{Data: data})
	for i := 0; i < 20; i++ {
		if got := CredentialFingerprint(&Credentials{Data: data}); got != first {
			t.Fatalf("CredentialFingerprint() = %q, want %q regardless of map iteration order", got, first)
		}
	}
	if CredentialFingerprint(&Credentials{Data: map[string]string{"ab": "c"}}) == CredentialFingerprint(&Credentials{Data: map[string]string{"a": "bc"}}) {
		t.Error("the boundary between a key and its value must be part of the fingerprint")
	}
	if CredentialFingerprint(&Credentials{Data: map[string]string{"a": "b", "c": "d"}}) == CredentialFingerprint(&Credentials{Data: map[string]string{"a": "bcd"}}) {
		t.Error("the boundaries between entries must be part of the fingerprint")
	}
}

func TestObserveRecordsTheCredentialOfEveryCall(t *testing.T) {
	recorder := &recordingRecorder{}
	observed := Observe(newFakeDriver("fake"), recorder)
	creds := &Credentials{Type: "api-key", Data: map[string]string{"apiKey": "sk-secret-value"}}

	if _, err := observed.Complete(context.Background(), &CompletionRequest{Model: "m", Creds: creds}); err != nil {
		t.Fatal(err)
	}
	if _, err := observed.Embed(context.Background(), &EmbedRequest{Model: "m", Creds: creds}); err != nil {
		t.Fatal(err)
	}
	if _, err := observed.Complete(context.Background(), &CompletionRequest{Model: "m"}); err != nil {
		t.Fatal(err)
	}
	if _, err := observed.Complete(context.Background(), nil); err != nil {
		t.Fatal(err)
	}

	want := CredentialFingerprint(creds)
	got := recorder.all()
	if len(got) != 4 {
		t.Fatalf("recorded %d outcomes, want 4", len(got))
	}
	if got[0].Credential != want || got[1].Credential != want || want == "" {
		t.Errorf("credentials = %q, %q, want the fingerprint %q for both operations", got[0].Credential, got[1].Credential, want)
	}
	if got[2].Credential != "" || got[3].Credential != "" {
		t.Errorf("credentials = %q, %q, want none for calls that present no credential", got[2].Credential, got[3].Credential)
	}
}
