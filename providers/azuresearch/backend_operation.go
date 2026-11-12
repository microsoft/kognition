// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package azuresearch

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	protocol "github.com/kognition-ai/kognition/proto/gen/kognition/v1alpha1"
)

const backendOperationVersion = "v1"

type backendOperation struct {
	ServiceName         string `json:"serviceName"`
	IndexName           string `json:"indexName"`
	OperationID         string `json:"operationId"`
	ConfigurationDigest string `json:"configurationDigest"`
}

func encodeBackendOperation(spec BackendSpec, scope *protocol.KnowledgeScope, operationID string) (string, error) {
	if spec.Validate() != nil || scope == nil || strings.TrimSpace(operationID) == "" || strings.TrimSpace(scope.ConfigurationDigest) == "" {
		return "", fmt.Errorf("invalid azure search backend operation")
	}
	payload, err := json.Marshal(backendOperation{
		ServiceName: spec.ServiceName, IndexName: spec.IndexName,
		OperationID: operationID, ConfigurationDigest: scope.ConfigurationDigest,
	})
	if err != nil {
		return "", fmt.Errorf("encode azure search backend operation: %w", err)
	}
	return backendOperationVersion + "." + base64.RawURLEncoding.EncodeToString(payload), nil
}

func decodeBackendOperation(value string, scope *protocol.KnowledgeScope) (BackendSpec, error) {
	var result BackendSpec
	prefix, encoded, found := strings.Cut(value, ".")
	if !found || prefix != backendOperationVersion || encoded == "" || scope == nil {
		return result, fmt.Errorf("invalid azure search backend operation")
	}
	payload, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil || len(payload) > 512 {
		return result, fmt.Errorf("invalid azure search backend operation")
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	var operation backendOperation
	if err := decoder.Decode(&operation); err != nil || !errors.Is(decoder.Decode(&struct{}{}), io.EOF) ||
		operation.OperationID != scope.OperationId || operation.ConfigurationDigest != scope.ConfigurationDigest {
		return result, fmt.Errorf("invalid azure search backend operation")
	}
	result = BackendSpec{
		ServiceName: operation.ServiceName, IndexName: operation.IndexName, EmbeddingDimensions: 1,
		Authentication: AuthenticationConfig{Mode: "workload-identity"},
	}
	if err := result.Validate(); err != nil {
		return BackendSpec{}, fmt.Errorf("invalid azure search backend operation")
	}
	return result, nil
}

func NewCleanupClient(credential azcore.TokenCredential, backendOperationID string, scope *protocol.KnowledgeScope, options *azcore.ClientOptions) (*Client, error) {
	spec, err := decodeBackendOperation(backendOperationID, scope)
	if err != nil {
		return nil, err
	}
	return NewClient(credential, spec, options)
}
