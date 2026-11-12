// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package foundry

import (
	"context"
	"fmt"
	"sync"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"

	"github.com/kognition-ai/kognition/internal/cpi"
)

const cognitiveServicesScope = "https://cognitiveservices.azure.com/.default"

// TokenProvider abstracts Azure token acquisition for testing.
type TokenProvider interface {
	GetToken(ctx context.Context, creds *cpi.Credentials) (string, error)
}

// NewTokenProvider creates the cached Azure token provider used by Foundry
// callers that need to authenticate an OpenAI-compatible request.
func NewTokenProvider() TokenProvider {
	return newAzureTokenProvider()
}

// azureTokenProvider acquires tokens via azidentity, caching credential objects.
type azureTokenProvider struct {
	mu    sync.Mutex
	creds map[string]azcore.TokenCredential
}

func newAzureTokenProvider() *azureTokenProvider {
	return &azureTokenProvider{creds: make(map[string]azcore.TokenCredential)}
}

func (p *azureTokenProvider) GetToken(ctx context.Context, creds *cpi.Credentials) (string, error) {
	credType := creds.Data["credentialType"]
	clientID := creds.Data["clientId"]

	key := credType + "/" + clientID
	if key == "/" {
		key = "_default"
	}

	p.mu.Lock()
	credential, exists := p.creds[key]
	if !exists {
		var err error
		credential, err = newCredential(credType, creds.Data)
		if err != nil {
			p.mu.Unlock()
			return "", fmt.Errorf("create credential: %w", err)
		}
		p.creds[key] = credential
	}
	p.mu.Unlock()

	token, err := credential.GetToken(ctx, policy.TokenRequestOptions{
		Scopes: []string{cognitiveServicesScope},
	})
	if err != nil {
		return "", fmt.Errorf("acquire token: %w", err)
	}
	return token.Token, nil
}

// newCredential creates an azcore.TokenCredential based on credentialType.
//
// Supported values:
//   - "client-secret": uses ClientSecretCredential (requires tenantId, clientId, clientSecret)
//   - "workload-identity": uses WorkloadIdentityCredential (requires tenantId, clientId, tokenFile)
//   - "managed-identity": uses ManagedIdentityCredential (optional clientId for user-assigned)
//   - "default" or "": uses DefaultAzureCredential (respects environment variables)
func newCredential(credentialType string, data map[string]string) (azcore.TokenCredential, error) {
	switch credentialType {
	case "client-secret":
		tenantID := data["tenantId"]
		clientID := data["clientId"]
		clientSecret := data["clientSecret"]
		if tenantID == "" || clientID == "" || clientSecret == "" {
			return nil, fmt.Errorf("client-secret credential requires tenantId, clientId, and clientSecret")
		}
		return azidentity.NewClientSecretCredential(tenantID, clientID, clientSecret, nil)

	case "workload-identity":
		tenantID := data["tenantId"]
		clientID := data["clientId"]
		tokenFile := data["tokenFile"]
		if tenantID == "" || clientID == "" || tokenFile == "" {
			return nil, fmt.Errorf("workload-identity credential requires tenantId, clientId, and tokenFile")
		}
		return azidentity.NewWorkloadIdentityCredential(&azidentity.WorkloadIdentityCredentialOptions{
			TenantID:      tenantID,
			ClientID:      clientID,
			TokenFilePath: tokenFile,
		})

	case "managed-identity":
		clientID := data["clientId"]
		if clientID != "" {
			return azidentity.NewManagedIdentityCredential(&azidentity.ManagedIdentityCredentialOptions{
				ID: azidentity.ClientID(clientID),
			})
		}
		return azidentity.NewManagedIdentityCredential(nil)

	case "default", "":
		return azidentity.NewDefaultAzureCredential(nil)

	default:
		return nil, fmt.Errorf("unsupported credentialType %q", credentialType)
	}
}
