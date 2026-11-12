// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package azuresearch_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/kognition-ai/kognition/providers/azuresearch"
)

type tokenCredentialFunc func(context.Context, policy.TokenRequestOptions) (azcore.AccessToken, error)

func (fn tokenCredentialFunc) GetToken(ctx context.Context, options policy.TokenRequestOptions) (azcore.AccessToken, error) {
	return fn(ctx, options)
}

type transportFunc func(*http.Request) (*http.Response, error)

func (fn transportFunc) Do(request *http.Request) (*http.Response, error) {
	return fn(request)
}

func TestClientSendsAuthenticatedSearchRequest(t *testing.T) {
	credential := tokenCredentialFunc(func(_ context.Context, options policy.TokenRequestOptions) (azcore.AccessToken, error) {
		if len(options.Scopes) != 1 || options.Scopes[0] != "https://search.azure.com/.default" {
			t.Fatalf("unexpected token scopes: %v", options.Scopes)
		}
		return azcore.AccessToken{Token: "test-token", ExpiresOn: time.Now().Add(time.Hour)}, nil
	})
	transport := transportFunc(func(request *http.Request) (*http.Response, error) {
		if request.Method != http.MethodPost || request.URL.String() != "https://kognition-search.search.windows.net/indexes/knowledge_chunks/docs/search?api-version=2026-04-01" {
			t.Fatalf("unexpected request: %s %s", request.Method, request.URL)
		}
		if request.Header.Get("Authorization") != "Bearer test-token" || request.Header.Get("Content-Type") != "application/json" || request.Header.Get("Accept") != "application/json" {
			t.Fatalf("unexpected headers: %v", request.Header)
		}
		var body azuresearch.SearchRequest
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body.Search != "query" || body.Filter != "namespace eq 'tenant-a'" || body.Select != "documentId,chunkId,content,sourceUri,location" || body.Top != 3 || len(body.VectorQueries) != 1 {
			t.Fatalf("unexpected request body: %+v", body)
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"value":[{"@search.score":0.75,"documentId":"doc","chunkId":"chunk","content":"body","sourceUri":"https://example.test/doc","location":"p1"}]}`))}, nil
	})
	client, err := azuresearch.NewClient(credential, azuresearch.BackendSpec{ServiceName: "kognition-search", IndexName: "knowledge_chunks", EmbeddingDimensions: 3, Authentication: azuresearch.AuthenticationConfig{Mode: "workload-identity"}}, &azcore.ClientOptions{Transport: transport})
	if err != nil {
		t.Fatal(err)
	}
	documents, err := client.Search(context.Background(), azuresearch.SearchRequest{Search: "query", Filter: "namespace eq 'tenant-a'", Select: "documentId,chunkId,content,sourceUri,location", Top: 3, VectorQueries: []azuresearch.VectorQuery{{Kind: "vector", Vector: []float64{0.1, 0.2, 0.3}, Fields: "embedding", K: 3}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(documents) != 1 || documents[0].DocumentID != "doc" || documents[0].Score != 0.75 {
		t.Fatalf("unexpected documents: %+v", documents)
	}
}

func TestClientCountsFilteredDocuments(t *testing.T) {
	credential := tokenCredentialFunc(func(context.Context, policy.TokenRequestOptions) (azcore.AccessToken, error) {
		return azcore.AccessToken{Token: "test-token", ExpiresOn: time.Now().Add(time.Hour)}, nil
	})
	transport := transportFunc(func(request *http.Request) (*http.Response, error) {
		var body azuresearch.SearchRequest
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body.Search != "*" || body.Filter != "operationId eq 'operation-1'" || body.Select != "id" || body.Top != 1 || !body.Count {
			t.Fatalf("unexpected count request body: %+v", body)
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"@odata.count":2,"value":[]}`))}, nil
	})
	client, err := azuresearch.NewClient(credential, azuresearch.BackendSpec{ServiceName: "kognition-search", IndexName: "knowledge_chunks", EmbeddingDimensions: 3, Authentication: azuresearch.AuthenticationConfig{Mode: "workload-identity"}}, &azcore.ClientOptions{Transport: transport})
	if err != nil {
		t.Fatal(err)
	}
	count, err := client.Count(context.Background(), "operationId eq 'operation-1'")
	if err != nil || count != 2 {
		t.Fatalf("count=%d err=%v", count, err)
	}
}

func TestClientRejectsInvalidCountResponses(t *testing.T) {
	for _, body := range []string{`{"value":[]}`, `{"@odata.count":-1,"value":[]}`} {
		credential := tokenCredentialFunc(func(context.Context, policy.TokenRequestOptions) (azcore.AccessToken, error) {
			return azcore.AccessToken{Token: "test-token", ExpiresOn: time.Now().Add(time.Hour)}, nil
		})
		transport := transportFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
		})
		client, err := azuresearch.NewClient(credential, azuresearch.BackendSpec{ServiceName: "kognition-search", IndexName: "knowledge_chunks", EmbeddingDimensions: 3, Authentication: azuresearch.AuthenticationConfig{Mode: "workload-identity"}}, &azcore.ClientOptions{Transport: transport})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := client.Count(context.Background(), "operationId eq 'operation-1'"); err == nil {
			t.Fatalf("accepted invalid count response: %s", body)
		}
	}
}

func TestClientRejectsInvalidConfiguration(t *testing.T) {
	validSpec := azuresearch.BackendSpec{ServiceName: "kognition-search", IndexName: "knowledge_chunks", EmbeddingDimensions: 3, Authentication: azuresearch.AuthenticationConfig{Mode: "workload-identity"}}
	credential := tokenCredentialFunc(func(context.Context, policy.TokenRequestOptions) (azcore.AccessToken, error) {
		return azcore.AccessToken{}, nil
	})
	if _, err := azuresearch.NewClient(nil, validSpec, nil); err == nil {
		t.Fatal("expected nil credential to be rejected")
	}
	validSpec.ServiceName = "https://attacker.example"
	if _, err := azuresearch.NewClient(credential, validSpec, nil); err == nil {
		t.Fatal("expected invalid service name to be rejected")
	}
}

func TestClientReturnsTypedSanitizedResponseErrors(t *testing.T) {
	tests := []struct {
		name       string
		statusCode int
		body       string
		service    bool
	}{
		{name: "service failure", statusCode: http.StatusBadRequest, body: `{"error":{"message":"secret backend detail"}}`, service: true},
		{name: "malformed json", statusCode: http.StatusOK, body: `{"value":`},
		{name: "missing value", statusCode: http.StatusOK, body: `{}`},
		{name: "null value", statusCode: http.StatusOK, body: `{"value":null}`},
		{name: "trailing json", statusCode: http.StatusOK, body: `{"value":[]} {"private":"detail"}`},
		{name: "oversized json", statusCode: http.StatusOK, body: `{"value":[{"content":"` + strings.Repeat("x", (4<<20)+1) + `"}]}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			credential := tokenCredentialFunc(func(context.Context, policy.TokenRequestOptions) (azcore.AccessToken, error) {
				return azcore.AccessToken{Token: "test-token", ExpiresOn: time.Now().Add(time.Hour)}, nil
			})
			transport := transportFunc(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: test.statusCode, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(test.body))}, nil
			})
			client, err := azuresearch.NewClient(credential, azuresearch.BackendSpec{ServiceName: "kognition-search", IndexName: "knowledge_chunks", EmbeddingDimensions: 3, Authentication: azuresearch.AuthenticationConfig{Mode: "workload-identity"}}, &azcore.ClientOptions{Transport: transport})
			if err != nil {
				t.Fatal(err)
			}
			_, err = client.Search(context.Background(), azuresearch.SearchRequest{Select: "documentId", Top: 1})
			if test.service {
				var serviceError *azuresearch.ServiceError
				if !errors.As(err, &serviceError) || serviceError.StatusCode != test.statusCode {
					t.Fatalf("unexpected service error: %v", err)
				}
			} else {
				var responseError *azuresearch.ResponseError
				if !errors.As(err, &responseError) {
					t.Fatalf("unexpected response error: %v", err)
				}
			}
			if strings.Contains(err.Error(), "secret backend detail") || strings.Contains(err.Error(), "private") {
				t.Fatalf("error leaked response body: %v", err)
			}
		})
	}
}

func TestClientIndexChecksEveryDocumentResult(t *testing.T) {
	credential := tokenCredentialFunc(func(context.Context, policy.TokenRequestOptions) (azcore.AccessToken, error) {
		return azcore.AccessToken{Token: "test-token", ExpiresOn: time.Now().Add(time.Hour)}, nil
	})
	documents := []azuresearch.IndexDocument{
		{Action: "mergeOrUpload", Key: "key-1", Namespace: "tenant-a", KnowledgeBaseUID: "kb-uid", Generation: 2, DocumentID: "doc-1", ChunkID: "chunk-1", Content: "one", Embedding: []float64{0.1, 0.2, 0.3}},
		{Action: "mergeOrUpload", Key: "key-2", Namespace: "tenant-a", KnowledgeBaseUID: "kb-uid", Generation: 2, DocumentID: "doc-2", ChunkID: "chunk-2", Content: "two", Embedding: []float64{0.4, 0.5, 0.6}},
	}
	transport := transportFunc(func(request *http.Request) (*http.Response, error) {
		if request.Method != http.MethodPost || request.URL.String() != "https://kognition-search.search.windows.net/indexes/knowledge_chunks/docs/index?api-version=2026-04-01" {
			t.Fatalf("unexpected request: %s %s", request.Method, request.URL)
		}
		body, err := io.ReadAll(request.Body)
		if err != nil || !bytes.Contains(body, []byte(`"@search.action":"mergeOrUpload"`)) || !bytes.Contains(body, []byte(`"knowledgeBaseUid":"kb-uid"`)) {
			t.Fatalf("unexpected body: %s (%v)", body, err)
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"X-Ms-Request-Id": []string{"request-123"}}, Body: io.NopCloser(strings.NewReader(`{"value":[{"key":"key-1","status":true,"statusCode":200},{"key":"key-2","status":true,"statusCode":201}]}`))}, nil
	})
	client, err := azuresearch.NewClient(credential, azuresearch.BackendSpec{ServiceName: "kognition-search", IndexName: "knowledge_chunks", EmbeddingDimensions: 3, Authentication: azuresearch.AuthenticationConfig{Mode: "workload-identity"}}, &azcore.ClientOptions{Transport: transport})
	if err != nil {
		t.Fatal(err)
	}
	result, err := client.Index(context.Background(), documents)
	if err != nil || result.RequestID != "request-123" || len(result.Documents) != 2 || !result.Documents[0].Succeeded || !result.Documents[1].Succeeded {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	documents[1].Key = "missing-result"
	if _, err := client.Index(context.Background(), documents); err == nil {
		t.Fatal("index accepted a response missing the requested document result")
	}
}

func TestClientIndexReturnsPartialDocumentOutcomes(t *testing.T) {
	credential := tokenCredentialFunc(func(context.Context, policy.TokenRequestOptions) (azcore.AccessToken, error) {
		return azcore.AccessToken{Token: "test-token", ExpiresOn: time.Now().Add(time.Hour)}, nil
	})
	transport := transportFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusMultiStatus, Header: http.Header{"X-Ms-Request-Id": []string{"request-207"}}, Body: io.NopCloser(strings.NewReader(`{"value":[{"key":"key-1","status":true,"statusCode":201},{"key":"key-2","status":false,"statusCode":503}]}`))}, nil
	})
	client, err := azuresearch.NewClient(credential, azuresearch.BackendSpec{ServiceName: "kognition-search", IndexName: "knowledge_chunks", EmbeddingDimensions: 3, Authentication: azuresearch.AuthenticationConfig{Mode: "workload-identity"}}, &azcore.ClientOptions{Transport: transport})
	if err != nil {
		t.Fatal(err)
	}
	result, err := client.Index(context.Background(), []azuresearch.IndexDocument{{Action: "mergeOrUpload", Key: "key-1"}, {Action: "mergeOrUpload", Key: "key-2"}})
	if err != nil {
		t.Fatal(err)
	}
	if result.RequestID != "request-207" || len(result.Documents) != 2 || !result.Documents[0].Succeeded || result.Documents[1].Succeeded || result.Documents[1].StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("unexpected partial result: %+v", result)
	}
}

func TestDocumentKeyIsScopedAndDeterministic(t *testing.T) {
	first := azuresearch.DocumentKey("tenant-a", "kb-uid", 2, "operation-1", "doc", "chunk")
	if first == "" || first != azuresearch.DocumentKey("tenant-a", "kb-uid", 2, "operation-1", "doc", "chunk") {
		t.Fatal("document key is empty or unstable")
	}
	if first == azuresearch.DocumentKey("tenant-a", "kb-uid", 3, "operation-1", "doc", "chunk") || first == azuresearch.DocumentKey("tenant-b", "kb-uid", 2, "operation-1", "doc", "chunk") || first == azuresearch.DocumentKey("tenant-a", "kb-uid", 2, "operation-2", "doc", "chunk") {
		t.Fatal("document key does not isolate scope, generation, or operation")
	}
}
