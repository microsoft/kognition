// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package azuresearch

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation"
)

const APIGroup = "azuresearch.knowledge.kognition.dev"
const APIVersion = "v1alpha1"
const Kind = "AzureSearchBackend"

type AuthenticationConfig struct {
	Mode string `json:"mode"`
}

type BackendSpec struct {
	ServiceName         string               `json:"serviceName"`
	IndexName           string               `json:"indexName"`
	EmbeddingDimensions int32                `json:"embeddingDimensions"`
	Authentication      AuthenticationConfig `json:"authentication"`
}

var serviceNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9](?:[a-z0-9-]{0,57}[a-z0-9])?$`)
var indexNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{1,127}$`)

func (spec BackendSpec) Validate() error {
	if !serviceNamePattern.MatchString(spec.ServiceName) || strings.Contains(spec.ServiceName, "--") || len(validation.IsDNS1123Label(spec.ServiceName)) != 0 {
		return fmt.Errorf("azure search serviceName must be a lowercase Azure Search service name of at most 60 characters")
	}
	if !indexNamePattern.MatchString(spec.IndexName) || strings.Contains(spec.IndexName, "--") || strings.Contains(spec.IndexName, "__") {
		return fmt.Errorf("azure search indexName must be a lowercase index name of at most 128 characters")
	}
	if spec.EmbeddingDimensions < 1 || spec.EmbeddingDimensions > 4096 {
		return fmt.Errorf("azure search embeddingDimensions must be between 1 and 4096")
	}
	if spec.Authentication.Mode != "workload-identity" {
		return fmt.Errorf("azure search authentication mode must be workload-identity")
	}
	return nil
}

func DecodeBackend(configuration *unstructured.Unstructured) (BackendSpec, error) {
	var result BackendSpec
	if configuration == nil || configuration.GroupVersionKind() != (schema.GroupVersionKind{Group: APIGroup, Version: APIVersion, Kind: Kind}) ||
		len(validation.IsDNS1123Label(configuration.GetNamespace())) != 0 || len(validation.IsDNS1123Subdomain(configuration.GetName())) != 0 ||
		!configuration.GetDeletionTimestamp().IsZero() {
		return result, fmt.Errorf("azure search configuration must be a live namespaced AzureSearchBackend v1alpha1")
	}
	spec, found, err := unstructured.NestedMap(configuration.Object, "spec")
	if err != nil || !found {
		return result, fmt.Errorf("azure search configuration spec is required")
	}
	encoded, err := json.Marshal(spec)
	if err != nil || len(encoded) > 1<<20 {
		return result, fmt.Errorf("invalid azure search configuration spec")
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&result); err != nil {
		return BackendSpec{}, fmt.Errorf("azure search configuration contains unknown fields or invalid field types")
	}
	if err := result.Validate(); err != nil {
		return BackendSpec{}, err
	}
	return result, nil
}
