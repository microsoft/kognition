// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package azuresearch

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/runtime"
)

const searchAPIVersion = "2026-04-01"
const searchTokenScope = "https://search.azure.com/.default"
const maxSearchResponseBytes = 4 << 20
const maxIndexResponseBytes = 1 << 20

type SearchRequest struct {
	Search        string        `json:"search,omitempty"`
	Filter        string        `json:"filter,omitempty"`
	Select        string        `json:"select"`
	Top           int32         `json:"top"`
	Count         bool          `json:"count,omitempty"`
	VectorQueries []VectorQuery `json:"vectorQueries,omitempty"`
}

type VectorQuery struct {
	Kind   string    `json:"kind"`
	Vector []float64 `json:"vector"`
	Fields string    `json:"fields"`
	K      int32     `json:"k"`
}

type SearchDocument struct {
	Score      float64 `json:"@search.score"`
	Key        string  `json:"id"`
	DocumentID string  `json:"documentId"`
	ChunkID    string  `json:"chunkId"`
	Content    string  `json:"content"`
	SourceURI  string  `json:"sourceUri"`
	Location   string  `json:"location"`
}

type searchResponse struct {
	Count *int64            `json:"@odata.count,omitempty"`
	Value *[]SearchDocument `json:"value"`
}

type IndexDocument struct {
	Action              string    `json:"@search.action"`
	Key                 string    `json:"id"`
	Namespace           string    `json:"namespace,omitempty"`
	KnowledgeBaseUID    string    `json:"knowledgeBaseUid,omitempty"`
	Generation          int64     `json:"generation,omitempty"`
	OperationID         string    `json:"operationId,omitempty"`
	ConfigurationUID    string    `json:"configurationUid,omitempty"`
	ConfigurationDigest string    `json:"configurationDigest,omitempty"`
	DocumentID          string    `json:"documentId,omitempty"`
	ChunkID             string    `json:"chunkId,omitempty"`
	Content             string    `json:"content,omitempty"`
	SourceURI           string    `json:"sourceUri,omitempty"`
	Location            string    `json:"location,omitempty"`
	Embedding           []float64 `json:"embedding,omitempty"`
}

type indexRequest struct {
	Value []IndexDocument `json:"value"`
}

type indexResult struct {
	Key        string `json:"key"`
	Status     bool   `json:"status"`
	StatusCode int    `json:"statusCode"`
}

type indexResponse struct {
	Value []indexResult `json:"value"`
}

type IndexDocumentResult struct {
	Key        string
	Succeeded  bool
	StatusCode int
}

type IndexBatchResult struct {
	RequestID string
	Documents []IndexDocumentResult
}

type ServiceError struct {
	StatusCode int
}

func (err *ServiceError) Error() string {
	return fmt.Sprintf("azure search request failed with status %d", err.StatusCode)
}

type ResponseError struct{}

func (*ResponseError) Error() string {
	return "invalid azure search response"
}

type Client struct {
	endpoint  string
	indexName string
	client    *azcore.Client
}

func NewClient(credential azcore.TokenCredential, spec BackendSpec, options *azcore.ClientOptions) (*Client, error) {
	if credential == nil || spec.Validate() != nil {
		return nil, fmt.Errorf("invalid azure search client configuration")
	}
	client, err := azcore.NewClient("github.com/kognition-ai/kognition/providers/azuresearch", "v0.0.0", runtime.PipelineOptions{
		PerRetry: []policy.Policy{runtime.NewBearerTokenPolicy(credential, []string{searchTokenScope}, nil)},
	}, options)
	if err != nil {
		return nil, fmt.Errorf("create azure search client: %w", err)
	}
	return &Client{endpoint: "https://" + spec.ServiceName + ".search.windows.net", indexName: spec.IndexName, client: client}, nil
}

func (client *Client) Search(ctx context.Context, input SearchRequest) ([]SearchDocument, error) {
	output, err := client.search(ctx, input)
	if err != nil {
		return nil, err
	}
	return *output.Value, nil
}

func (client *Client) Count(ctx context.Context, filter string) (int64, error) {
	output, err := client.search(ctx, SearchRequest{Search: "*", Filter: filter, Select: "id", Top: 1, Count: true})
	if err != nil {
		return 0, err
	}
	if output.Count == nil || *output.Count < 0 {
		return 0, &ResponseError{}
	}
	return *output.Count, nil
}

func (client *Client) search(ctx context.Context, input SearchRequest) (searchResponse, error) {
	endpoint := client.endpoint + "/indexes/" + url.PathEscape(client.indexName) + "/docs/search?api-version=" + searchAPIVersion
	request, err := runtime.NewRequest(ctx, http.MethodPost, endpoint)
	if err != nil {
		return searchResponse{}, fmt.Errorf("create azure search request: %w", err)
	}
	request.Raw().Header.Set("Accept", "application/json")
	if err := runtime.MarshalAsJSON(request, input); err != nil {
		return searchResponse{}, fmt.Errorf("encode azure search request: %w", err)
	}
	response, err := client.client.Pipeline().Do(request)
	if err != nil {
		return searchResponse{}, fmt.Errorf("send azure search request: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return searchResponse{}, &ServiceError{StatusCode: response.StatusCode}
	}
	var output searchResponse
	limited := io.LimitReader(response.Body, maxSearchResponseBytes+1)
	decoder := json.NewDecoder(limited)
	if err := decoder.Decode(&output); err != nil {
		return searchResponse{}, &ResponseError{}
	}
	if _, err := decoder.Token(); err != io.EOF {
		return searchResponse{}, &ResponseError{}
	}
	if output.Value == nil {
		return searchResponse{}, &ResponseError{}
	}
	return output, nil
}

func (client *Client) Index(ctx context.Context, documents []IndexDocument) (IndexBatchResult, error) {
	if len(documents) == 0 || len(documents) > 1000 {
		return IndexBatchResult{}, fmt.Errorf("invalid azure search index batch")
	}
	endpoint := client.endpoint + "/indexes/" + url.PathEscape(client.indexName) + "/docs/index?api-version=" + searchAPIVersion
	request, err := runtime.NewRequest(ctx, http.MethodPost, endpoint)
	if err != nil {
		return IndexBatchResult{}, fmt.Errorf("create azure search index request: %w", err)
	}
	request.Raw().Header.Set("Accept", "application/json")
	if err := runtime.MarshalAsJSON(request, indexRequest{Value: documents}); err != nil {
		return IndexBatchResult{}, fmt.Errorf("encode azure search index request: %w", err)
	}
	response, err := client.client.Pipeline().Do(request)
	if err != nil {
		return IndexBatchResult{}, fmt.Errorf("send azure search index request: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK && response.StatusCode != http.StatusMultiStatus {
		return IndexBatchResult{}, &ServiceError{StatusCode: response.StatusCode}
	}
	var output indexResponse
	limited := io.LimitReader(response.Body, maxIndexResponseBytes+1)
	decoder := json.NewDecoder(limited)
	if err := decoder.Decode(&output); err != nil {
		return IndexBatchResult{}, &ResponseError{}
	}
	if _, err := decoder.Token(); err != io.EOF || len(output.Value) != len(documents) {
		return IndexBatchResult{}, &ResponseError{}
	}
	expected := make(map[string]struct{}, len(documents))
	for _, document := range documents {
		if document.Key == "" {
			return IndexBatchResult{}, fmt.Errorf("azure search document key is required")
		}
		expected[document.Key] = struct{}{}
	}
	if len(expected) != len(documents) {
		return IndexBatchResult{}, fmt.Errorf("duplicate azure search document key")
	}
	batchResult := IndexBatchResult{Documents: make([]IndexDocumentResult, 0, len(output.Value))}
	for _, documentResult := range output.Value {
		if _, found := expected[documentResult.Key]; !found || documentResult.Status != (documentResult.StatusCode >= 200 && documentResult.StatusCode < 300) {
			return IndexBatchResult{}, &ResponseError{}
		}
		delete(expected, documentResult.Key)
		batchResult.Documents = append(batchResult.Documents, IndexDocumentResult{Key: documentResult.Key, Succeeded: documentResult.Status, StatusCode: documentResult.StatusCode})
	}
	if len(expected) != 0 {
		return IndexBatchResult{}, &ResponseError{}
	}
	requestID := response.Header.Get("x-ms-request-id")
	if requestID == "" {
		requestID = response.Header.Get("request-id")
	}
	if requestID == "" || len(requestID) > 1024 {
		return IndexBatchResult{}, &ResponseError{}
	}
	batchResult.RequestID = requestID
	return batchResult, nil
}

func DocumentKey(namespace, knowledgeBaseUID string, generation int64, operationID, documentID, chunkID string) string {
	digest := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%s\x00%d\x00%s\x00%s\x00%s", namespace, knowledgeBaseUID, generation, operationID, documentID, chunkID)))
	return base64.RawURLEncoding.EncodeToString(digest[:])
}
