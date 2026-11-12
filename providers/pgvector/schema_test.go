// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package pgvector_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/kognition-ai/kognition/providers/pgvector"
	apiextensions "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	structuralschema "k8s.io/apiextensions-apiserver/pkg/apiserver/schema"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/schema/cel"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/schema/pruning"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"sigs.k8s.io/yaml"
)

func backendSchema(t *testing.T) (*structuralschema.Structural, validation.SchemaValidator) {
	t.Helper()
	filename := "pgvector.knowledge.kognition.dev_pgvectorbackends.yaml"
	if _, err := os.Stat(filepath.Join("../../deploy/helm/kognition/crds", filename)); !os.IsNotExist(err) {
		t.Fatal("provider CRD must not ship in core")
	}
	data, err := os.ReadFile(filepath.Join("../../deploy/helm/kognition-knowledge-pgvector/crds", filename))
	if err != nil {
		t.Fatal(err)
	}
	var crd apiextensionsv1.CustomResourceDefinition
	if err := yaml.UnmarshalStrict(data, &crd); err != nil {
		t.Fatal(err)
	}
	if crd.Spec.Group != pgvector.APIGroup || crd.Spec.Names.Kind != pgvector.Kind || crd.Spec.Scope != apiextensionsv1.NamespaceScoped ||
		len(crd.Spec.Versions) != 1 || crd.Spec.Versions[0].Name != pgvector.APIVersion || !crd.Spec.Versions[0].Storage || !crd.Spec.Versions[0].Served {
		t.Fatal("provider CRD identity or scope differs from decoder")
	}
	var schema apiextensions.JSONSchemaProps
	if err := apiextensionsv1.Convert_v1_JSONSchemaProps_To_apiextensions_JSONSchemaProps(crd.Spec.Versions[0].Schema.OpenAPIV3Schema, &schema, nil); err != nil {
		t.Fatal(err)
	}
	structural, err := structuralschema.NewStructural(&schema)
	if err != nil {
		t.Fatal(err)
	}
	if errors := structuralschema.ValidateStructural(field.NewPath("schema"), structural); len(errors) != 0 {
		t.Fatal(errors)
	}
	validator, _, err := validation.NewSchemaValidator(&schema)
	if err != nil {
		t.Fatal(err)
	}
	return structural, validator
}

func TestBackendSchemaValidation(t *testing.T) {
	structural, validator := backendSchema(t)
	celValidator := cel.NewValidator(structural, true, 1000000)
	validate := func(object map[string]interface{}) field.ErrorList {
		errors := validation.ValidateCustomResource(field.NewPath("backend"), object, validator)
		if len(errors) != 0 {
			return errors
		}
		celErrors, _ := celValidator.Validate(context.Background(), field.NewPath("backend"), structural, object, nil, 10000000)
		return celErrors
	}
	if errors := validate(validBackend().Object); len(errors) != 0 {
		t.Fatalf("valid configuration rejected: %v", errors)
	}
	unknownFields := map[string]bool{"inline credentials": true, "connection string": true, "adapter endpoint": true, "cross namespace": true, "cross namespace CA": true}
	for name, mutate := range invalidSpecs() {
		t.Run(name, func(t *testing.T) {
			configuration := validBackend()
			mutate(configuration.Object["spec"].(map[string]interface{}))
			if unknownFields[name] {
				pruning.Prune(configuration.Object, structural, true)
				if _, err := pgvector.DecodeBackend(configuration); err != nil {
					t.Fatalf("unknown provider fields survived Kubernetes pruning: %v", err)
				}
				return
			}
			if errors := validate(configuration.Object); len(errors) == 0 {
				t.Fatal("schema accepted configuration rejected by provider")
			}
		})
	}
	for _, key := range []string{".", "..", "..private", "../key", "", "normal.key"} {
		t.Run("secret key "+key, func(t *testing.T) {
			configuration := validBackend()
			configuration.Object["spec"].(map[string]interface{})["credentialsSecretRef"].(map[string]interface{})["passwordKey"] = key
			_, err := pgvector.DecodeBackend(configuration)
			if schemaValid := len(validate(configuration.Object)) == 0; schemaValid != (err == nil) {
				t.Fatalf("schema and decoder disagree for secret key: schema valid=%v, decoder=%v", schemaValid, err)
			}
		})
	}
}
