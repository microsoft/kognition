// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package foundry

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"

	"github.com/kognition-ai/kognition/internal/cpi"
)

// fakeCredential implements azcore.TokenCredential for unit tests.
type fakeCredential struct {
	token string
	err   error
}

func (f *fakeCredential) GetToken(_ context.Context, _ policy.TokenRequestOptions) (azcore.AccessToken, error) {
	if f.err != nil {
		return azcore.AccessToken{}, f.err
	}
	return azcore.AccessToken{Token: f.token, ExpiresOn: time.Now().Add(time.Hour)}, nil
}

func TestNewCredential_ClientSecret_Valid(t *testing.T) {
	data := map[string]string{
		"tenantId":     "tenant-1",
		"clientId":     "client-1",
		"clientSecret": "secret-1",
	}
	cred, err := newCredential("client-secret", data)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if cred == nil {
		t.Fatal("expected non-nil credential")
	}
}

func TestNewCredential_ClientSecret_MissingFields(t *testing.T) {
	tests := []struct {
		name string
		data map[string]string
	}{
		{"missing tenantId", map[string]string{"clientId": "c", "clientSecret": "s"}},
		{"missing clientId", map[string]string{"tenantId": "t", "clientSecret": "s"}},
		{"missing clientSecret", map[string]string{"tenantId": "t", "clientId": "c"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := newCredential("client-secret", tt.data)
			if err == nil {
				t.Fatal("expected error for missing fields")
			}
		})
	}
}

func TestNewCredential_WorkloadIdentity_MissingFields(t *testing.T) {
	tests := []struct {
		name string
		data map[string]string
	}{
		{"missing tenantId", map[string]string{"clientId": "c", "tokenFile": "/tmp/token"}},
		{"missing clientId", map[string]string{"tenantId": "t", "tokenFile": "/tmp/token"}},
		{"missing tokenFile", map[string]string{"tenantId": "t", "clientId": "c"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := newCredential("workload-identity", tt.data)
			if err == nil {
				t.Fatal("expected error for missing fields")
			}
		})
	}
}

func TestNewCredential_WorkloadIdentity_Valid(t *testing.T) {
	tokenFile, err := os.CreateTemp("", "foundry-federated-token-")
	if err != nil {
		t.Fatalf("create token file: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(tokenFile.Name()) })
	if err := tokenFile.Close(); err != nil {
		t.Fatalf("close token file: %v", err)
	}

	credential, err := newCredential("workload-identity", map[string]string{
		"tenantId":  "tenant-1",
		"clientId":  "client-1",
		"tokenFile": tokenFile.Name(),
	})
	if err != nil {
		t.Fatalf("newCredential() error: %v", err)
	}
	if credential == nil {
		t.Fatal("newCredential() returned nil credential")
	}
}

func TestNewCredential_ManagedIdentity(t *testing.T) {
	// With clientId (user-assigned)
	cred, err := newCredential("managed-identity", map[string]string{"clientId": "my-id"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cred == nil {
		t.Fatal("expected non-nil credential")
	}

	// Without clientId (system-assigned)
	cred, err = newCredential("managed-identity", map[string]string{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cred == nil {
		t.Fatal("expected non-nil credential")
	}
}

func TestNewCredential_Default(t *testing.T) {
	for _, credType := range []string{"default", ""} {
		cred, err := newCredential(credType, map[string]string{})
		if err != nil {
			t.Fatalf("credType=%q: unexpected error: %v", credType, err)
		}
		if cred == nil {
			t.Fatalf("credType=%q: expected non-nil credential", credType)
		}
	}
}

func TestNewCredential_Unsupported(t *testing.T) {
	_, err := newCredential("foobar", map[string]string{})
	if err == nil {
		t.Fatal("expected error for unsupported credentialType")
	}
}

func TestAzureTokenProvider_CacheKey(t *testing.T) {
	provider := newAzureTokenProvider()

	// Pre-seed the cache with a fake credential to avoid real Azure calls.
	provider.creds["client-secret/client-1"] = &fakeCredential{token: "token-a"}
	provider.creds["managed-identity/client-2"] = &fakeCredential{token: "token-b"}
	provider.creds["_default"] = &fakeCredential{token: "token-c"}

	tests := []struct {
		name     string
		data     map[string]string
		expected string
	}{
		{
			name:     "client-secret with clientId",
			data:     map[string]string{"credentialType": "client-secret", "clientId": "client-1"},
			expected: "token-a",
		},
		{
			name:     "managed-identity with clientId",
			data:     map[string]string{"credentialType": "managed-identity", "clientId": "client-2"},
			expected: "token-b",
		},
		{
			name:     "no credentialType or clientId",
			data:     map[string]string{},
			expected: "token-c",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			token, err := provider.GetToken(context.Background(), &cpi.Credentials{
				Type: "azure-identity",
				Data: tt.data,
			})
			if err != nil {
				t.Fatalf("GetToken() error: %v", err)
			}
			if token != tt.expected {
				t.Errorf("token = %q, want %q", token, tt.expected)
			}
		})
	}
}

func TestAzureTokenProvider_PropagatesCredentialFailures(t *testing.T) {
	tests := []struct {
		name     string
		provider *azureTokenProvider
		data     map[string]string
		want     string
	}{
		{
			name:     "credential construction",
			provider: newAzureTokenProvider(),
			data:     map[string]string{"credentialType": "unsupported"},
			want:     "create credential: unsupported credentialType",
		},
		{
			name: "token acquisition",
			provider: &azureTokenProvider{creds: map[string]azcore.TokenCredential{
				"managed-identity/client-1": &fakeCredential{err: errors.New("identity unavailable")},
			}},
			data: map[string]string{
				"credentialType": "managed-identity",
				"clientId":       "client-1",
			},
			want: "acquire token: identity unavailable",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			token, err := test.provider.GetToken(context.Background(), &cpi.Credentials{
				Type: "azure-identity",
				Data: test.data,
			})
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("GetToken() error = %v, want %q", err, test.want)
			}
			if token != "" {
				t.Fatalf("GetToken() token = %q, want empty token on failure", token)
			}
		})
	}
}
