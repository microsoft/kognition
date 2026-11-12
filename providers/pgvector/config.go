// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package pgvector

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation"
)

const APIGroup = "pgvector.knowledge.kognition.dev"
const APIVersion = "v1alpha1"
const Kind = "PgvectorBackend"

type SecretKeyReference struct {
	Name string `json:"name"`
	Key  string `json:"key"`
}

type CredentialsReference struct {
	Name        string `json:"name"`
	UsernameKey string `json:"usernameKey"`
	PasswordKey string `json:"passwordKey"`
}

type TLSConfig struct {
	Mode        string              `json:"mode"`
	CASecretRef *SecretKeyReference `json:"caSecretRef,omitempty"`
}

type BackendSpec struct {
	Host                 string               `json:"host"`
	Port                 int32                `json:"port"`
	Database             string               `json:"database"`
	Table                string               `json:"table"`
	EmbeddingDimensions  int32                `json:"embeddingDimensions"`
	CredentialsSecretRef CredentialsReference `json:"credentialsSecretRef"`
	TLS                  TLSConfig            `json:"tls"`
}

var identifierPattern = regexp.MustCompile(`^[a-z_][a-z0-9_]{0,62}$`)

func (spec BackendSpec) Validate() error {
	if len(validation.IsDNS1123Subdomain(spec.Host)) != 0 {
		return fmt.Errorf("pgvector host must be a DNS subdomain")
	}
	if spec.Port < 1 || spec.Port > 65535 {
		return fmt.Errorf("pgvector port must be between 1 and 65535")
	}
	if !identifierPattern.MatchString(spec.Database) || !identifierPattern.MatchString(spec.Table) {
		return fmt.Errorf("pgvector database and table must be lowercase SQL identifiers of at most 63 characters")
	}
	if spec.EmbeddingDimensions < 1 || spec.EmbeddingDimensions > 2000 {
		return fmt.Errorf("pgvector embeddingDimensions must be between 1 and 2000")
	}
	credentials := spec.CredentialsSecretRef
	if len(validation.IsDNS1123Subdomain(credentials.Name)) != 0 || !validSecretKey(credentials.UsernameKey) ||
		!validSecretKey(credentials.PasswordKey) || credentials.UsernameKey == credentials.PasswordKey {
		return fmt.Errorf("pgvector credentials require a local Secret name and distinct username/password keys")
	}
	if spec.TLS.Mode != "verify-full" {
		return fmt.Errorf("pgvector TLS mode must be verify-full")
	}
	if reference := spec.TLS.CASecretRef; reference != nil {
		if len(validation.IsDNS1123Subdomain(reference.Name)) != 0 || !validSecretKey(reference.Key) {
			return fmt.Errorf("pgvector TLS CA requires a local Secret name and key")
		}
	}
	return nil
}

func DecodeBackend(configuration *unstructured.Unstructured) (BackendSpec, error) {
	var result BackendSpec
	if configuration == nil || configuration.GroupVersionKind() != (schema.GroupVersionKind{Group: APIGroup, Version: APIVersion, Kind: Kind}) ||
		len(validation.IsDNS1123Label(configuration.GetNamespace())) != 0 || len(validation.IsDNS1123Subdomain(configuration.GetName())) != 0 ||
		!configuration.GetDeletionTimestamp().IsZero() {
		return result, fmt.Errorf("pgvector configuration must be a live namespaced PgvectorBackend v1alpha1")
	}
	spec, found, err := unstructured.NestedMap(configuration.Object, "spec")
	if err != nil || !found {
		return result, fmt.Errorf("pgvector configuration spec is required")
	}
	encoded, err := json.Marshal(spec)
	if err != nil || len(encoded) > 1<<20 {
		return result, fmt.Errorf("invalid pgvector configuration spec")
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&result); err != nil {
		return BackendSpec{}, fmt.Errorf("pgvector configuration contains unknown fields or invalid field types")
	}
	if err := result.Validate(); err != nil {
		return BackendSpec{}, err
	}
	return result, nil
}

func validSecretKey(key string) bool {
	return key != "" && len(validation.IsConfigMapKey(key)) == 0
}
