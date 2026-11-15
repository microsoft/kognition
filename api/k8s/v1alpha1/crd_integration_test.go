// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build integration

package v1alpha1_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"testing"

	apiextensions "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	structuralschema "k8s.io/apiextensions-apiserver/pkg/apiserver/schema"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	apimachineryruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/apimachinery/pkg/util/validation/field"

	v1alpha1 "github.com/kognition-ai/kognition/api/k8s/v1alpha1"
)

// crdDir returns the path to the CRD manifests directory.
func crdDir() string {
	_, thisFile, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(thisFile), "..", "..", "..", "deploy", "helm", "kognition", "crds")
}

func TestCRDJSONPayloadContracts(t *testing.T) {
	scheme := apimachineryruntime.NewScheme()
	if err := apiextensionsv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		resource string
		path     []string
		wantType string
	}{
		{"toolinvocationruns", []string{"spec", "input"}, ""},
		{"toolinvocationruns", []string{"status", "output"}, ""},
		{"tools", []string{"status", "compiledMCP", "inputSchema"}, "object"},
		{"agentservices", []string{"status"}, "object"},
		{"intentgateways", []string{"status"}, "object"},
		{"prompts", []string{"status"}, "object"},
		{"solutions", []string{"status"}, "object"},
	} {
		t.Run(test.resource+"/"+test.path[len(test.path)-1], func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join(crdDir(), "kognition.dev_"+test.resource+".yaml"))
			if err != nil {
				t.Fatal(err)
			}
			object, _, err := serializer.NewCodecFactory(scheme).UniversalDeserializer().Decode(data, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			property := *object.(*apiextensionsv1.CustomResourceDefinition).Spec.Versions[0].Schema.OpenAPIV3Schema
			for _, name := range test.path {
				var found bool
				property, found = property.Properties[name]
				if !found {
					t.Fatalf("missing property %s", name)
				}
			}
			if property.Type != test.wantType || property.XPreserveUnknownFields == nil || !*property.XPreserveUnknownFields {
				t.Fatalf("JSON contract: type=%q, want %q, preserveUnknown=%v", property.Type, test.wantType, property.XPreserveUnknownFields)
			}
			if len(test.path) == 1 && len(property.Properties) != 0 {
				t.Fatal("opaque status must not constrain or prune previously preserved fields")
			}
		})
	}
}

func TestCRDStructuralSchemas(t *testing.T) {
	paths, err := filepath.Glob(filepath.Join(crdDir(), "*.yaml"))
	if err != nil || len(paths) == 0 {
		t.Fatalf("find CRDs: paths=%v, error=%v", paths, err)
	}
	scheme := apimachineryruntime.NewScheme()
	if err := apiextensionsv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	decoder := serializer.NewCodecFactory(scheme).UniversalDeserializer()
	for _, path := range paths {
		t.Run(filepath.Base(path), func(t *testing.T) {
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			object, _, err := decoder.Decode(data, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			crd := object.(*apiextensionsv1.CustomResourceDefinition)
			for _, version := range crd.Spec.Versions {
				if version.Schema == nil || version.Schema.OpenAPIV3Schema == nil {
					t.Fatalf("%s has no schema", version.Name)
				}
				var internalSchema apiextensions.JSONSchemaProps
				if err := apiextensionsv1.Convert_v1_JSONSchemaProps_To_apiextensions_JSONSchemaProps(version.Schema.OpenAPIV3Schema, &internalSchema, nil); err != nil {
					t.Fatal(err)
				}
				structural, err := structuralschema.NewStructural(&internalSchema)
				if err != nil {
					t.Fatal(err)
				}
				for _, problem := range structuralschema.ValidateStructural(field.NewPath(version.Name), structural) {
					t.Error(problem)
				}
			}
		})
	}
}

// TestCRDFilesExist verifies all expected CRD YAML files are present.
func TestCRDFilesExist(t *testing.T) {
	expectedFiles := []string{
		"kognition.dev_agents.yaml",
		"kognition.dev_agentjobs.yaml",
		"kognition.dev_agentinstances.yaml",
		"kognition.dev_modelautoscalers.yaml",
		"kognition.dev_tools.yaml",
		"kognition.dev_toolgrants.yaml",
		"kognition.dev_toolclasses.yaml",
		"kognition.dev_toolinvocationruns.yaml",
		"kognition.dev_modelclasses.yaml",
		"kognition.dev_memoryclasses.yaml",
		"kognition.dev_memories.yaml",
		"kognition.dev_memoryrecords.yaml",
		"kognition.dev_memorygrants.yaml",
		"kognition.dev_workflows.yaml",
		"kognition.dev_workflowruns.yaml",
		"kognition.dev_agentpolicies.yaml",
		"kognition.dev_cognitiveadmissionpolicies.yaml",
		"kognition.dev_cognitivequotas.yaml",
		"kognition.dev_cognitivelimitranges.yaml",
		"kognition.dev_workspaces.yaml",
		"kognition.dev_agentidentities.yaml",
		"kognition.dev_agentroles.yaml",
		"kognition.dev_agentrolebindings.yaml",
		"kognition.dev_agentcommunicationpolicies.yaml",
		"kognition.dev_models.yaml",
		"kognition.dev_modelgrants.yaml",
		"kognition.dev_secretrefs.yaml",
		"kognition.dev_evaluations.yaml",
		"kognition.dev_evaluationruns.yaml",
		"kognition.dev_knowledgebases.yaml",
		"kognition.dev_knowledgebaseclasses.yaml",
		"kognition.dev_knowledgebindingrecords.yaml",
		"kognition.dev_runtimeapprovals.yaml",
		"kognition.dev_runtimeapprovaltombstones.yaml",
		"kognition.dev_runtimeapprovalquotatickets.yaml",
		"kognition.dev_trustprofileindices.yaml",
	}
	for _, f := range expectedFiles {
		path := filepath.Join(crdDir(), f)
		if _, err := os.Stat(path); os.IsNotExist(err) {
			t.Errorf("CRD file missing: %s", f)
		}
	}
}

func TestKnowledgeBackendReferenceCRDContract(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(crdDir(), "kognition.dev_knowledgebaseclasses.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	scheme := apimachineryruntime.NewScheme()
	if err := apiextensionsv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	object, _, err := serializer.NewCodecFactory(scheme).UniversalDeserializer().Decode(data, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	crd := object.(*apiextensionsv1.CustomResourceDefinition)
	spec := crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"]
	reference := spec.Properties["backendRef"]
	assertRequiredFields(t, reference.Required, "apiGroup", "kind", "name")
	if reference.Type != "object" || len(reference.Properties) != 3 {
		t.Fatalf("backend reference must expose only group, kind and name: %#v", reference)
	}
	if len(spec.XValidations) != 1 || spec.XValidations[0].Rule != "!(has(self.backendRef) && has(self.vectorStore))" {
		t.Fatalf("missing legacy/reference exclusion: %#v", spec.XValidations)
	}
	for _, test := range []struct {
		field   string
		valid   string
		invalid string
	}{
		{field: "apiGroup", valid: "pgvector.knowledge.kognition.dev", invalid: "https://provider"},
		{field: "kind", valid: "PgvectorBackend", invalid: "pgvector-backend"},
		{field: "name", valid: "shared", invalid: "tenant/shared"},
	} {
		property := reference.Properties[test.field]
		pattern, err := regexp.Compile(property.Pattern)
		if err != nil {
			t.Fatal(err)
		}
		if !pattern.MatchString(test.valid) || pattern.MatchString(test.invalid) || pattern.MatchString("") {
			t.Errorf("%s pattern does not enforce the reference contract", test.field)
		}
		if test.field != "kind" && (property.MinLength == nil || *property.MinLength != 1 || property.MaxLength == nil || *property.MaxLength != 253) {
			t.Errorf("%s must have DNS subdomain length bounds", test.field)
		}
	}
}

func TestRuntimeApprovalCRDFoundationParity(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(crdDir(), "kognition.dev_runtimeapprovals.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	scheme := apimachineryruntime.NewScheme()
	if err := apiextensionsv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	object, _, err := serializer.NewCodecFactory(scheme).UniversalDeserializer().Decode(data, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	crd := object.(*apiextensionsv1.CustomResourceDefinition)
	version := crd.Spec.Versions[0]
	if version.Subresources == nil || version.Subresources.Status == nil {
		t.Fatal("RuntimeApproval CRD does not declare the status subresource")
	}
	root := version.Schema.OpenAPIV3Schema
	assertRequiredFields(t, root.Required, "spec")
	for _, field := range root.Required {
		if field == "status" {
			t.Fatal("RuntimeApproval status must not be required on CREATE")
		}
	}
	spec := root.Properties["spec"]
	assertRequiredFields(t, spec.Required, "workspace", "invocationID", "initialDecision", "agentRef", "agentIdentityRef", "agentInstanceRef", "targetRef", "autonomyClassRef", "binding")
	assertStringEnum(t, spec.Properties["initialDecision"], "Pending", "NotRequired")
	for _, name := range []string{"agentRef", "agentIdentityRef", "agentInstanceRef", "targetRef", "trustProfileRef", "autonomyClassRef"} {
		reference := spec.Properties[name]
		assertRequiredFields(t, reference.Required, "kind", "scope", "name", "uid", "resourceVersion")
		assertStringEnum(t, reference.Properties["scope"], "Namespaced", "Cluster")
	}
	authorizationReference := spec.Properties["authorizationRefs"].Items.Schema
	digest := spec.Properties["agentInstanceRef"].Properties["configurationDigest"]
	if digest.Type != "string" || digest.Pattern != "^[a-f0-9]{64}$" ||
		digest.MinLength == nil || *digest.MinLength != 64 || digest.MaxLength == nil || *digest.MaxLength != 64 {
		t.Fatal("AgentInstance configuration digest must retain the bounded SHA-256 wire shape")
	}
	for _, name := range []string{"agentRef", "agentIdentityRef", "targetRef", "trustProfileRef", "autonomyClassRef"} {
		if _, exists := spec.Properties[name].Properties["configurationDigest"]; exists {
			t.Fatalf("configuration digest must not weaken %s version binding", name)
		}
	}
	assertRequiredFields(t, authorizationReference.Required, "kind", "scope", "name", "uid", "resourceVersion")
	assertStringEnum(t, authorizationReference.Properties["scope"], "Namespaced", "Cluster")
	binding := spec.Properties["binding"]
	assertRequiredFields(t, binding.Required, "format", "algorithm", "domain", "epoch", "keyVersion", "tag")
	assertStringEnum(t, binding.Properties["format"], "kognition-runtime-action-v1")
	assertStringEnum(t, binding.Properties["algorithm"], "HMAC-SHA-256")
	assertStringEnum(t, binding.Properties["domain"], "kognition.dev/runtime-approval/action/v1")
	if binding.Properties["epoch"].Minimum == nil || *binding.Properties["epoch"].Minimum != 1 {
		t.Errorf("RuntimeApproval binding epoch minimum = %v, want 1", binding.Properties["epoch"].Minimum)
	}
	if binding.Properties["keyVersion"].Maximum != nil {
		t.Errorf("RuntimeApproval binding keyVersion has identifier magnitude maximum %v", *binding.Properties["keyVersion"].Maximum)
	}
	status := root.Properties["status"]
	assertRequiredFields(t, status.Required, "decisionState", "executionState")
	assertRequiredFields(t, status.Properties["decisionAudit"].Required, "id", "payload")
	for _, name := range []string{"decisionState", "decisionEvidenceDelivered", "decisionAudit", "decidedByIssuer", "decidedBySubject", "decidedAt", "executionState", "executionDisposition", "preparationToken", "preparationDeadline", "claimant", "fencingToken", "terminationProofRef", "reconciliationEvidenceRef", "reconciledByIssuer", "reconciledBySubject"} {
		if _, exists := status.Properties[name]; !exists {
			t.Errorf("RuntimeApproval CRD status is missing %q", name)
		}
	}
}

func TestKnowledgeBackendBindingCRDContract(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(crdDir(), "kognition.dev_knowledgebases.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	scheme := apimachineryruntime.NewScheme()
	if err := apiextensionsv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	object, _, err := serializer.NewCodecFactory(scheme).UniversalDeserializer().Decode(data, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	crd := object.(*apiextensionsv1.CustomResourceDefinition)
	version := crd.Spec.Versions[0]
	if version.Subresources == nil || version.Subresources.Status == nil {
		t.Fatal("binding must live in the status subresource")
	}
	binding := version.Schema.OpenAPIV3Schema.Properties["status"].Properties["backendBinding"]
	assertRequiredFields(t, binding.Required, "generation", "providerName", "classUID", "classGeneration", "configurationAPIGroup", "configurationKind", "configurationName", "configurationUID", "configurationDigest", "operationID", "backendOperationID")
	if binding.Type != "object" || len(binding.Properties) != 13 {
		t.Fatalf("unexpected backend binding schema: %#v", binding)
	}
	embedding := binding.Properties["embeddingModelBinding"]
	assertRequiredFields(t, embedding.Required, "namespace", "modelName", "modelUID", "modelResourceVersion", "specDigest")
	if embedding.Type != "object" || embedding.Properties["specDigest"].Pattern != "^sha256:[a-f0-9]{64}$" {
		t.Fatalf("embedding model binding must pin a prefixed SHA-256 spec digest: %#v", embedding)
	}
	if version := embedding.Properties["modelResourceVersion"]; version.Type != "integer" || version.Format != "int64" || version.Minimum == nil || *version.Minimum != 1 {
		t.Fatal("embedding model binding must pin a positive int64 model resource version")
	}
	for _, field := range []string{"generation", "classGeneration"} {
		property := binding.Properties[field]
		if property.Type != "integer" || property.Format != "int64" || property.Minimum == nil || *property.Minimum != 1 {
			t.Errorf("%s must be a positive int64", field)
		}
	}
	for field, maximum := range map[string]int64{"providerName": 253, "classUID": 128, "configurationAPIGroup": 253, "configurationKind": 63, "configurationName": 253, "configurationUID": 128, "backendOperationID": 1024} {
		property := binding.Properties[field]
		if property.Type != "string" || property.MinLength == nil || *property.MinLength != 1 || property.MaxLength == nil || *property.MaxLength != maximum {
			t.Errorf("%s must be a bounded nonempty string", field)
		}
	}
	assertEnumValues(t, binding.Properties["reclaimPolicy"].Enum, "Delete", "Retain", "Archive")
	if binding.Properties["configurationDigest"].Pattern != "^[a-f0-9]{64}$" {
		t.Fatal("configuration digest must be a lowercase SHA-256 value")
	}
	operationID := binding.Properties["operationID"]
	if operationID.Type != "string" || operationID.MinLength == nil || *operationID.MinLength != 1 || operationID.MaxLength == nil || *operationID.MaxLength != 128 {
		t.Fatal("operation ID must be a bounded nonempty string")
	}
}

func TestKnowledgeBindingRecordCRDContract(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(crdDir(), "kognition.dev_knowledgebindingrecords.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	scheme := apimachineryruntime.NewScheme()
	if err := apiextensionsv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	object, _, err := serializer.NewCodecFactory(scheme).UniversalDeserializer().Decode(data, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	crd := object.(*apiextensionsv1.CustomResourceDefinition)
	if crd.Spec.Scope != apiextensionsv1.ClusterScoped || crd.Spec.Names.Plural != "knowledgebindingrecords" {
		t.Fatalf("KnowledgeBindingRecord identity = scope %q plural %q", crd.Spec.Scope, crd.Spec.Names.Plural)
	}
	root := crd.Spec.Versions[0].Schema.OpenAPIV3Schema
	if len(root.XValidations) != 2 {
		t.Fatalf("KnowledgeBindingRecord validations = %#v, want immutability and Retain policy", root.XValidations)
	}
	assertValidationRule(t, root.XValidations, "self.spec == oldSelf.spec")
	assertValidationRule(t, root.XValidations, "self.spec.reclaimPolicy == 'Retain' && has(self.spec.binding.reclaimPolicy) && self.spec.binding.reclaimPolicy == 'Retain'")
	spec := root.Properties["spec"]
	assertRequiredFields(t, spec.Required, "knowledgeBaseNamespace", "knowledgeBaseName", "knowledgeBaseUID", "reclaimPolicy", "binding")
	binding := spec.Properties["binding"]
	assertRequiredFields(t, binding.Required, "generation", "providerName", "classUID", "classGeneration", "configurationAPIGroup", "configurationKind", "configurationName", "configurationUID", "configurationDigest", "operationID", "backendOperationID")
	if _, hasStatus := root.Properties["status"]; hasStatus {
		t.Fatal("KnowledgeBindingRecord must not expose mutable status")
	}
	for _, forbidden := range []string{"expiresAt", "retentionDeadline"} {
		if _, exists := spec.Properties[forbidden]; exists {
			t.Fatalf("KnowledgeBindingRecord must not expire via %q", forbidden)
		}
	}
}

func TestRuntimeApprovalTombstoneCRDParity(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(crdDir(), "kognition.dev_runtimeapprovaltombstones.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	scheme := apimachineryruntime.NewScheme()
	if err := apiextensionsv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	object, _, err := serializer.NewCodecFactory(scheme).UniversalDeserializer().Decode(data, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	crd := object.(*apiextensionsv1.CustomResourceDefinition)
	if crd.Spec.Scope != apiextensionsv1.NamespaceScoped || crd.Spec.Names.Plural != "runtimeapprovaltombstones" {
		t.Fatalf("RuntimeApprovalTombstone identity = scope %q plural %q", crd.Spec.Scope, crd.Spec.Names.Plural)
	}
	root := crd.Spec.Versions[0].Schema.OpenAPIV3Schema
	if len(root.XValidations) != 1 || root.XValidations[0].Rule != "self.spec == oldSelf.spec" {
		t.Fatalf("RuntimeApprovalTombstone immutability validations = %#v", root.XValidations)
	}
	spec := root.Properties["spec"]
	assertRequiredFields(t, spec.Required, "workspace", "invocationID", "plane", "operation", "agentRef", "runtimeApprovalUID", "initialDecision", "binding", "decisionState", "executionState", "createdAt", "expiresAt", "compactedAt", "retentionDeadline")
	assertStringEnum(t, spec.Properties["decisionState"], "Approved", "Rejected", "Expired", "NotRequired")
	assertStringEnum(t, spec.Properties["executionState"], "NotStarted", "Completed", "Failed")
	assertRequiredFields(t, spec.Properties["binding"].Required, "format", "algorithm", "domain", "epoch", "keyVersion", "tag")
	if _, hasStatus := root.Properties["status"]; hasStatus {
		t.Fatal("RuntimeApprovalTombstone must not expose mutable status")
	}
}

func TestRuntimeApprovalQuotaTicketCRDParity(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(crdDir(), "kognition.dev_runtimeapprovalquotatickets.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	scheme := apimachineryruntime.NewScheme()
	if err := apiextensionsv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	object, _, err := serializer.NewCodecFactory(scheme).UniversalDeserializer().Decode(data, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	crd := object.(*apiextensionsv1.CustomResourceDefinition)
	if crd.Spec.Scope != apiextensionsv1.NamespaceScoped || crd.Spec.Names.Plural != "runtimeapprovalquotatickets" {
		t.Fatalf("RuntimeApprovalQuotaTicket identity = scope %q plural %q", crd.Spec.Scope, crd.Spec.Names.Plural)
	}
	if crd.Spec.Names.Kind != "RuntimeApprovalQuotaTicket" || crd.Spec.Names.ListKind != "RuntimeApprovalQuotaTicketList" {
		t.Fatalf("RuntimeApprovalQuotaTicket kind identity = %q / %q", crd.Spec.Names.Kind, crd.Spec.Names.ListKind)
	}
	root := crd.Spec.Versions[0].Schema.OpenAPIV3Schema
	spec := root.Properties["spec"]
	assertRequiredFields(t, spec.Required, "workspace", "invocationID", "agentRef", "binding", "phase", "chargedBytes", "createdAt")
	assertStringEnum(t, spec.Properties["phase"], "Live", "Compacted")
	if bytes := spec.Properties["chargedBytes"]; bytes.Minimum == nil || *bytes.Minimum != 1 || bytes.Maximum != nil {
		t.Fatalf("RuntimeApprovalQuotaTicket chargedBytes bounds = [%v, %v], want [1, unbounded]", bytes.Minimum, bytes.Maximum)
	}
	assertRequiredFields(t, spec.Properties["binding"].Required, "format", "algorithm", "domain", "epoch", "keyVersion", "tag")
	assertStringEnum(t, spec.Properties["binding"].Properties["format"], "kognition-runtime-action-v1")
	assertStringEnum(t, spec.Properties["binding"].Properties["algorithm"], "HMAC-SHA-256")
	assertStringEnum(t, spec.Properties["binding"].Properties["domain"], "kognition.dev/runtime-approval/action/v1")
	agentRef := spec.Properties["agentRef"]
	assertRequiredFields(t, agentRef.Required, "kind", "scope", "name", "uid", "resourceVersion")
	assertStringEnum(t, agentRef.Properties["kind"], "Agent")
	assertStringEnum(t, agentRef.Properties["scope"], "Namespaced")
	rules := map[string]bool{}
	for _, validation := range spec.XValidations {
		rules[validation.Rule] = true
	}
	if !rules["self.agentRef.kind == 'Agent' && self.agentRef.scope == 'Namespaced' && self.agentRef.__namespace__ == self.workspace"] {
		t.Fatal("RuntimeApprovalQuotaTicket must bind agentRef.namespace to the workspace")
	}
	if !rules["self.phase == 'Compacted' ? (has(self.compactedAt) && has(self.retentionDeadline)) : true"] {
		t.Fatal("RuntimeApprovalQuotaTicket must require compactedAt/retentionDeadline in Compacted phase")
	}
	if !rules["self.phase == 'Live' ? !has(self.compactedAt) : true"] {
		t.Fatal("RuntimeApprovalQuotaTicket must forbid compactedAt in Live phase")
	}
	if _, hasStatus := root.Properties["status"]; hasStatus {
		t.Fatal("RuntimeApprovalQuotaTicket must not expose mutable status")
	}
}

func TestTrustProfileIndexCRDParity(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(crdDir(), "kognition.dev_trustprofileindices.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	scheme := apimachineryruntime.NewScheme()
	if err := apiextensionsv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	object, _, err := serializer.NewCodecFactory(scheme).UniversalDeserializer().Decode(data, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	crd := object.(*apiextensionsv1.CustomResourceDefinition)
	if crd.Spec.Scope != apiextensionsv1.NamespaceScoped || crd.Spec.Names.Plural != "trustprofileindices" {
		t.Fatalf("TrustProfileIndex identity = scope %q plural %q", crd.Spec.Scope, crd.Spec.Names.Plural)
	}
	root := crd.Spec.Versions[0].Schema.OpenAPIV3Schema
	if len(root.XValidations) != 1 || root.XValidations[0].Rule != "self.spec.agentUID == oldSelf.spec.agentUID" {
		t.Fatalf("TrustProfileIndex immutability validations = %#v", root.XValidations)
	}
	spec := root.Properties["spec"]
	assertRequiredFields(t, spec.Required, "agentUID", "selectedProfileName", "selectedProfileUID")
	for _, field := range []string{"agentUID", "selectedProfileUID"} {
		property := spec.Properties[field]
		if property.MinLength == nil || *property.MinLength != 1 || property.MaxLength == nil || *property.MaxLength != 128 {
			t.Errorf("TrustProfileIndex %s bounds = [%v, %v], want [1, 128]", field, property.MinLength, property.MaxLength)
		}
	}
	if _, hasStatus := root.Properties["status"]; hasStatus {
		t.Fatal("TrustProfileIndex must not expose mutable status")
	}
}

func assertStringEnum(t *testing.T, schema apiextensionsv1.JSONSchemaProps, expected ...string) {
	t.Helper()
	actual := make(map[string]bool, len(schema.Enum))
	for _, value := range schema.Enum {
		var decoded string
		if err := json.Unmarshal(value.Raw, &decoded); err != nil {
			t.Fatalf("decode enum value: %v", err)
		}
		actual[decoded] = true
	}
	for _, value := range expected {
		if !actual[value] {
			t.Errorf("enum is missing %q: %#v", value, actual)
		}
	}
	if len(actual) != len(expected) {
		t.Errorf("enum has unexpected values: %#v", actual)
	}
}

func TestAgentJobCRDStatusTelemetryFields(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(crdDir(), "kognition.dev_agentjobs.yaml"))
	if err != nil {
		t.Fatalf("read AgentJob CRD: %v", err)
	}

	scheme := apimachineryruntime.NewScheme()
	if err := apiextensionsv1.AddToScheme(scheme); err != nil {
		t.Fatalf("register CRD scheme: %v", err)
	}
	obj, _, err := serializer.NewCodecFactory(scheme).UniversalDeserializer().Decode(data, nil, nil)
	if err != nil {
		t.Fatalf("decode AgentJob CRD: %v", err)
	}
	crd, ok := obj.(*apiextensionsv1.CustomResourceDefinition)
	if !ok {
		t.Fatalf("decoded object is not a CRD: %T", obj)
	}

	statusProps := crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["status"].Properties
	for _, field := range []string{"startTime", "completionTime", "tokenUsage", "toolCalls", "output"} {
		if _, exists := statusProps[field]; !exists {
			t.Errorf("expected AgentJob status field %q", field)
		}
	}
	tokenUsage := statusProps["tokenUsage"].Properties
	for _, field := range []string{"promptTokens", "completionTokens", "totalTokens"} {
		if _, exists := tokenUsage[field]; !exists {
			t.Errorf("expected AgentJob status.tokenUsage field %q", field)
		}
	}
	jobSpec := crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"].Properties
	if _, exists := jobSpec["agentRevision"]; !exists {
		t.Error("expected AgentJob spec.agentRevision")
	}
}

func TestAgentServiceExecutionCRDFields(t *testing.T) {
	scheme := apimachineryruntime.NewScheme()
	if err := apiextensionsv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	decode := func(file string) *apiextensionsv1.CustomResourceDefinition {
		data, err := os.ReadFile(filepath.Join(crdDir(), file))
		if err != nil {
			t.Fatal(err)
		}
		object, _, err := serializer.NewCodecFactory(scheme).UniversalDeserializer().Decode(data, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		return object.(*apiextensionsv1.CustomResourceDefinition)
	}
	solutionTarget := decode("kognition.dev_solutions.yaml").Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"].Properties["target"]
	foundAgentService := false
	for _, targetKind := range solutionTarget.Properties["kind"].Enum {
		if string(targetKind.Raw) == `"AgentService"` {
			foundAgentService = true
			break
		}
	}
	if !foundAgentService {
		t.Fatal("Solution target kind enum does not include AgentService")
	}
	instanceSpec := decode("kognition.dev_agentinstances.yaml").Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"].Properties
	if _, exists := instanceSpec["agentRevision"]; !exists {
		t.Error("expected AgentInstance spec.agentRevision")
	}
}

func TestReasoningProbeCRDParity(t *testing.T) {
	scheme := apimachineryruntime.NewScheme()
	if err := apiextensionsv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	decode := func(file string) *apiextensionsv1.CustomResourceDefinition {
		t.Helper()
		data, err := os.ReadFile(filepath.Join(crdDir(), file))
		if err != nil {
			t.Fatal(err)
		}
		object, _, err := serializer.NewCodecFactory(scheme).UniversalDeserializer().Decode(data, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		return object.(*apiextensionsv1.CustomResourceDefinition)
	}

	agentSchema := decode("kognition.dev_agents.yaml").Spec.Versions[0].Schema.OpenAPIV3Schema
	reasoningProbe := agentSchema.Properties["spec"].Properties["probes"].Properties["reasoningProbe"]
	maxInputTokens := reasoningProbe.Properties["maxInputTokens"]
	if maxInputTokens.Default == nil || string(maxInputTokens.Default.Raw) != "4096" {
		t.Fatalf("spec.probes.reasoningProbe.maxInputTokens default = %v, want 4096", maxInputTokens.Default)
	}
	assertRequiredFields(t, reasoningProbe.Required, "canaryTask", "expectedOutcome")

	instanceSchema := decode("kognition.dev_agentinstances.yaml").Spec.Versions[0].Schema.OpenAPIV3Schema
	probeStatus := instanceSchema.Properties["status"].Properties["probeStatus"]
	for _, field := range []string{"lastReasoningScheduleSlot", "reasoningOperationID", "reasoningRequestDigest", "reasoningBindings", "lastReasoningResult"} {
		if _, exists := probeStatus.Properties[field]; !exists {
			t.Errorf("status.probeStatus is missing %q", field)
		}
	}
	binding := probeStatus.Properties["reasoningBindings"].Items.Schema
	assertRequiredFields(t, binding.Required, "kind", "namespace", "name", "uid")
	result := probeStatus.Properties["lastReasoningResult"]
	assertRequiredFields(t, result.Required, "operationID", "outcome")
	assertEnumValues(t, result.Properties["outcome"].Enum, "Passed", "OutcomeMismatch", "ExecutionFailed")
	assertEnumValues(t, result.Properties["closedReason"].Enum,
		"InvalidConfiguration", "BindingChanged", "AuthorizationDenied", "BudgetDenied",
		"TimedOut", "ProviderFailed", "ToolCallRequested", "InvalidEncoding",
		"OutputLimitExceeded", "AccountingUncertain", "PersistenceFailed")
	if len(result.XValidations) != 1 || result.XValidations[0].Rule != "(self.outcome == 'ExecutionFailed') == has(self.closedReason)" {
		t.Errorf("lastReasoningResult validations = %#v, want exact outcome/reason rule", result.XValidations)
	}
}

func assertRequiredFields(t *testing.T, got []string, want ...string) {
	t.Helper()
	fields := make(map[string]bool, len(got))
	for _, field := range got {
		fields[field] = true
	}
	for _, field := range want {
		if !fields[field] {
			t.Errorf("required fields %v are missing %q", got, field)
		}
	}
}

func assertValidationRule(t *testing.T, validations apiextensionsv1.ValidationRules, want string) {
	t.Helper()
	for _, validation := range validations {
		if validation.Rule == want {
			return
		}
	}
	t.Errorf("validation rules %#v are missing %q", validations, want)
}

func assertEnumValues(t *testing.T, got []apiextensionsv1.JSON, want ...string) {
	t.Helper()
	values := make(map[string]bool, len(got))
	for _, value := range got {
		values[string(value.Raw)] = true
	}
	if len(got) != len(want) {
		t.Errorf("enum has %d values, want exactly %d: %v", len(got), len(want), values)
	}
	for _, value := range want {
		if !values[`"`+value+`"`] {
			t.Errorf("enum %v is missing %q", values, value)
		}
	}
}

func TestToolCRDExecProbeValidations(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(crdDir(), "kognition.dev_tools.yaml"))
	if err != nil {
		t.Fatalf("read Tool CRD: %v", err)
	}

	scheme := apimachineryruntime.NewScheme()
	if err := apiextensionsv1.AddToScheme(scheme); err != nil {
		t.Fatalf("register CRD scheme: %v", err)
	}
	obj, _, err := serializer.NewCodecFactory(scheme).UniversalDeserializer().Decode(data, nil, nil)
	if err != nil {
		t.Fatalf("decode Tool CRD: %v", err)
	}
	crd, ok := obj.(*apiextensionsv1.CustomResourceDefinition)
	if !ok {
		t.Fatalf("decoded object is not a CRD: %T", obj)
	}
	healthProbe := crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"].Properties["healthProbe"]
	got := make(map[string]bool, len(healthProbe.XValidations))
	for _, validation := range healthProbe.XValidations {
		got[validation.Rule] = true
	}
	want := []string{
		"self.type != 'exec' || (has(self.command) && size(self.command) > 0 && self.command.all(arg, size(arg) > 0))",
		"self.type == 'exec' || !has(self.command) || size(self.command) == 0",
	}
	for _, rule := range want {
		if !got[rule] {
			t.Errorf("Tool CRD is missing exec probe validation rule %q", rule)
		}
	}
}

func TestToolClassCRDSandboxConfiguration(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(crdDir(), "kognition.dev_toolclasses.yaml"))
	if err != nil {
		t.Fatalf("read ToolClass CRD: %v", err)
	}

	scheme := apimachineryruntime.NewScheme()
	if err := apiextensionsv1.AddToScheme(scheme); err != nil {
		t.Fatalf("register CRD scheme: %v", err)
	}
	obj, _, err := serializer.NewCodecFactory(scheme).UniversalDeserializer().Decode(data, nil, nil)
	if err != nil {
		t.Fatalf("decode ToolClass CRD: %v", err)
	}
	crd, ok := obj.(*apiextensionsv1.CustomResourceDefinition)
	if !ok {
		t.Fatalf("decoded object is not a CRD: %T", obj)
	}

	spec := crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"].Properties
	sandboxImage, exists := spec["sandboxImage"]
	if !exists {
		t.Fatal("ToolClass CRD is missing spec.sandboxImage")
	}
	if sandboxImage.Type != "string" {
		t.Errorf("ToolClass CRD spec.sandboxImage type = %q, want string", sandboxImage.Type)
	}
	stdioCommand, exists := spec["stdioCommand"]
	if !exists {
		t.Fatal("ToolClass CRD is missing spec.stdioCommand")
	}
	if stdioCommand.Type != "array" || stdioCommand.Items == nil || stdioCommand.Items.Schema == nil || stdioCommand.Items.Schema.Type != "string" {
		t.Errorf("ToolClass CRD spec.stdioCommand schema = %#v, want array of strings", stdioCommand)
	}
}

func TestToolGrantCRDContract(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(crdDir(), "kognition.dev_toolgrants.yaml"))
	if err != nil {
		t.Fatalf("read ToolGrant CRD: %v", err)
	}

	scheme := apimachineryruntime.NewScheme()
	if err := apiextensionsv1.AddToScheme(scheme); err != nil {
		t.Fatalf("register CRD scheme: %v", err)
	}
	obj, _, err := serializer.NewCodecFactory(scheme).UniversalDeserializer().Decode(data, nil, nil)
	if err != nil {
		t.Fatalf("decode ToolGrant CRD: %v", err)
	}
	crd := obj.(*apiextensionsv1.CustomResourceDefinition)
	version := crd.Spec.Versions[0]
	spec := version.Schema.OpenAPIV3Schema.Properties["spec"]

	for _, field := range []string{"agentSelector", "agentRef", "toolRef", "verbs", "argumentConstraints", "rateLimits", "conditions"} {
		if _, exists := spec.Properties[field]; !exists {
			t.Errorf("ToolGrant CRD is missing spec.%s", field)
		}
	}
	verbs := spec.Properties["verbs"]
	if verbs.MinItems == nil || *verbs.MinItems != 1 || verbs.XListType == nil || *verbs.XListType != "set" {
		t.Errorf("ToolGrant spec.verbs constraints = minItems %v, listType %v", verbs.MinItems, verbs.XListType)
	}
	if len(spec.XValidations) != 1 || spec.XValidations[0].Rule != "!(has(self.agentRef) && has(self.agentSelector))" {
		t.Errorf("ToolGrant selector/reference validations = %#v", spec.XValidations)
	}
	if version.Subresources == nil || version.Subresources.Status == nil {
		t.Error("ToolGrant status subresource is not enabled")
	}
	status := version.Schema.OpenAPIV3Schema.Properties["status"]
	for _, field := range []string{"active", "lastEvaluatedTime", "grantedAgents"} {
		if _, exists := status.Properties[field]; !exists {
			t.Errorf("ToolGrant CRD is missing status.%s", field)
		}
	}
	columns := make(map[string]bool, len(version.AdditionalPrinterColumns))
	for _, column := range version.AdditionalPrinterColumns {
		columns[column.Name] = true
	}
	for _, name := range []string{"Agent", "Tool", "Active", "Age"} {
		if !columns[name] {
			t.Errorf("ToolGrant CRD is missing printer column %q", name)
		}
	}
}

// TestCRDStructuralValidity parses each CRD YAML and verifies basic structural properties.
func TestCRDStructuralValidity(t *testing.T) {
	tests := []struct {
		file     string
		kind     string
		listKind string
		plural   string
		scope    apiextensionsv1.ResourceScope
	}{
		{"kognition.dev_agents.yaml", "Agent", "AgentList", "agents", apiextensionsv1.NamespaceScoped},
		{"kognition.dev_agentinstances.yaml", "AgentInstance", "AgentInstanceList", "agentinstances", apiextensionsv1.NamespaceScoped},
		{"kognition.dev_modelautoscalers.yaml", "ModelAutoscaler", "ModelAutoscalerList", "modelautoscalers", apiextensionsv1.NamespaceScoped},
		{"kognition.dev_tools.yaml", "Tool", "ToolList", "tools", apiextensionsv1.NamespaceScoped},
		{"kognition.dev_toolgrants.yaml", "ToolGrant", "ToolGrantList", "toolgrants", apiextensionsv1.NamespaceScoped},
		{"kognition.dev_toolclasses.yaml", "ToolClass", "ToolClassList", "toolclasses", apiextensionsv1.ClusterScoped},
		{"kognition.dev_toolinvocationruns.yaml", "ToolInvocationRun", "ToolInvocationRunList", "toolinvocationruns", apiextensionsv1.NamespaceScoped},
		{"kognition.dev_modelclasses.yaml", "ModelClass", "ModelClassList", "modelclasses", apiextensionsv1.ClusterScoped},
		{"kognition.dev_memoryclasses.yaml", "MemoryClass", "MemoryClassList", "memoryclasses", apiextensionsv1.ClusterScoped},
		{"kognition.dev_memories.yaml", "Memory", "MemoryList", "memories", apiextensionsv1.NamespaceScoped},
		{"kognition.dev_memoryrecords.yaml", "MemoryRecord", "MemoryRecordList", "memoryrecords", apiextensionsv1.NamespaceScoped},
		{"kognition.dev_memorygrants.yaml", "MemoryGrant", "MemoryGrantList", "memorygrants", apiextensionsv1.NamespaceScoped},
		{"kognition.dev_workflows.yaml", "Workflow", "WorkflowList", "workflows", apiextensionsv1.NamespaceScoped},
		{"kognition.dev_workflowruns.yaml", "WorkflowRun", "WorkflowRunList", "workflowruns", apiextensionsv1.NamespaceScoped},
		{"kognition.dev_agentpolicies.yaml", "AgentPolicy", "AgentPolicyList", "agentpolicies", apiextensionsv1.NamespaceScoped},
		{"kognition.dev_cognitiveadmissionpolicies.yaml", "CognitiveAdmissionPolicy", "CognitiveAdmissionPolicyList", "cognitiveadmissionpolicies", apiextensionsv1.NamespaceScoped},
		{"kognition.dev_cognitivequotas.yaml", "CognitiveQuota", "CognitiveQuotaList", "cognitivequotas", apiextensionsv1.NamespaceScoped},
		{"kognition.dev_cognitivelimitranges.yaml", "CognitiveLimitRange", "CognitiveLimitRangeList", "cognitivelimitranges", apiextensionsv1.NamespaceScoped},
		{"kognition.dev_workspaces.yaml", "Workspace", "WorkspaceList", "workspaces", apiextensionsv1.NamespaceScoped},
		{"kognition.dev_agentidentities.yaml", "AgentIdentity", "AgentIdentityList", "agentidentities", apiextensionsv1.NamespaceScoped},
		{"kognition.dev_agentroles.yaml", "AgentRole", "AgentRoleList", "agentroles", apiextensionsv1.NamespaceScoped},
		{"kognition.dev_agentrolebindings.yaml", "AgentRoleBinding", "AgentRoleBindingList", "agentrolebindings", apiextensionsv1.NamespaceScoped},
		{"kognition.dev_agentcommunicationpolicies.yaml", "AgentCommunicationPolicy", "AgentCommunicationPolicyList", "agentcommunicationpolicies", apiextensionsv1.NamespaceScoped},
		{"kognition.dev_models.yaml", "Model", "ModelList", "models", apiextensionsv1.NamespaceScoped},
		{"kognition.dev_modelgrants.yaml", "ModelGrant", "ModelGrantList", "modelgrants", apiextensionsv1.NamespaceScoped},
		{"kognition.dev_secretrefs.yaml", "SecretRef", "SecretRefList", "secretrefs", apiextensionsv1.NamespaceScoped},
		{"kognition.dev_evaluations.yaml", "Evaluation", "EvaluationList", "evaluations", apiextensionsv1.NamespaceScoped},
		{"kognition.dev_evaluationruns.yaml", "EvaluationRun", "EvaluationRunList", "evaluationruns", apiextensionsv1.NamespaceScoped},
		{"kognition.dev_knowledgebases.yaml", "KnowledgeBase", "KnowledgeBaseList", "knowledgebases", apiextensionsv1.NamespaceScoped},
		{"kognition.dev_knowledgebaseclasses.yaml", "KnowledgeBaseClass", "KnowledgeBaseClassList", "knowledgebaseclasses", apiextensionsv1.ClusterScoped},
		{"kognition.dev_knowledgebindingrecords.yaml", "KnowledgeBindingRecord", "KnowledgeBindingRecordList", "knowledgebindingrecords", apiextensionsv1.ClusterScoped},
		{"kognition.dev_runtimeapprovaltombstones.yaml", "RuntimeApprovalTombstone", "RuntimeApprovalTombstoneList", "runtimeapprovaltombstones", apiextensionsv1.NamespaceScoped},
		{"kognition.dev_runtimeapprovalquotatickets.yaml", "RuntimeApprovalQuotaTicket", "RuntimeApprovalQuotaTicketList", "runtimeapprovalquotatickets", apiextensionsv1.NamespaceScoped},
	}

	for _, tt := range tests {
		t.Run(tt.kind, func(t *testing.T) {
			path := filepath.Join(crdDir(), tt.file)
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("Failed to read CRD file: %v", err)
			}

			scheme := apimachineryruntime.NewScheme()
			_ = apiextensionsv1.AddToScheme(scheme)
			codecFactory := serializer.NewCodecFactory(scheme)
			obj, _, err := codecFactory.UniversalDeserializer().Decode(data, nil, nil)
			if err != nil {
				t.Fatalf("Failed to decode CRD YAML: %v", err)
			}
			crd, ok := obj.(*apiextensionsv1.CustomResourceDefinition)
			if !ok {
				t.Fatalf("Decoded object is not a CRD: %T", obj)
			}

			// Verify group
			if crd.Spec.Group != "kognition.dev" {
				t.Errorf("Group = %q, want kognition.dev", crd.Spec.Group)
			}
			// Verify names
			if crd.Spec.Names.Kind != tt.kind {
				t.Errorf("Kind = %q, want %q", crd.Spec.Names.Kind, tt.kind)
			}
			if crd.Spec.Names.ListKind != tt.listKind {
				t.Errorf("ListKind = %q, want %q", crd.Spec.Names.ListKind, tt.listKind)
			}
			if crd.Spec.Names.Plural != tt.plural {
				t.Errorf("Plural = %q, want %q", crd.Spec.Names.Plural, tt.plural)
			}
			// Verify scope
			if crd.Spec.Scope != tt.scope {
				t.Errorf("Scope = %q, want %q", crd.Spec.Scope, tt.scope)
			}
			// Verify at least one version
			if len(crd.Spec.Versions) == 0 {
				t.Fatal("No versions defined")
			}
			// Verify version name
			if crd.Spec.Versions[0].Name != "v1alpha1" {
				t.Errorf("Version = %q, want v1alpha1", crd.Spec.Versions[0].Name)
			}
			// Verify schema is present
			if crd.Spec.Versions[0].Schema == nil || crd.Spec.Versions[0].Schema.OpenAPIV3Schema == nil {
				t.Fatal("OpenAPIV3Schema is nil")
			}
		})
	}
}

// TestCRDSchemaHasRequiredFields verifies that required fields are marked in the CRD schema.
func TestCRDSchemaHasRequiredFields(t *testing.T) {
	tests := []struct {
		file         string
		kind         string
		specRequired []string // fields required at spec level
		nestedChecks map[string][]string
	}{
		{
			file:         "kognition.dev_agents.yaml",
			kind:         "Agent",
			specRequired: []string{"modelRef", "instructions"},
			nestedChecks: map[string][]string{
				"modelRef": {"name"},
			},
		},
		{
			file:         "kognition.dev_agentinstances.yaml",
			kind:         "AgentInstance",
			specRequired: []string{"agentRef", "revision"},
		},
		{
			file:         "kognition.dev_modelautoscalers.yaml",
			kind:         "ModelAutoscaler",
			specRequired: []string{"agentRef", "preferredModelRef"},
			nestedChecks: map[string][]string{
				"agentRef":          {"name"},
				"preferredModelRef": {"name"},
			},
		},
		{
			file:         "kognition.dev_tools.yaml",
			kind:         "Tool",
			specRequired: []string{"className"},
		},
		{
			file:         "kognition.dev_toolclasses.yaml",
			kind:         "ToolClass",
			specRequired: []string{"provisioner"},
		},
		{
			file:         "kognition.dev_workflows.yaml",
			kind:         "Workflow",
			specRequired: []string{"steps"},
		},
		{
			file:         "kognition.dev_workflowruns.yaml",
			kind:         "WorkflowRun",
			specRequired: []string{"workflowRef"},
		},
		{
			file:         "kognition.dev_modelclasses.yaml",
			kind:         "ModelClass",
			specRequired: []string{"provisioner"},
		},
		{
			file:         "kognition.dev_memoryclasses.yaml",
			kind:         "MemoryClass",
			specRequired: []string{"backendType"},
		},
		{
			file:         "kognition.dev_memories.yaml",
			kind:         "Memory",
			specRequired: []string{"claimRef", "type"},
		},
		{
			file:         "kognition.dev_memoryrecords.yaml",
			kind:         "MemoryRecord",
			specRequired: []string{"memoryRef", "content"},
		},
		{
			file:         "kognition.dev_memorygrants.yaml",
			kind:         "MemoryGrant",
			specRequired: []string{"agentRef", "memoryClaimRef", "accessMode"},
		},
		{
			file:         "kognition.dev_agentpolicies.yaml",
			kind:         "AgentPolicy",
			specRequired: []string{"enforce"},
		},
		{
			file:         "kognition.dev_cognitiveadmissionpolicies.yaml",
			kind:         "CognitiveAdmissionPolicy",
			specRequired: []string{"rego"},
		},
		{
			file:         "kognition.dev_cognitivequotas.yaml",
			kind:         "CognitiveQuota",
			specRequired: []string{"hard"},
		},
		{
			file:         "kognition.dev_cognitivelimitranges.yaml",
			kind:         "CognitiveLimitRange",
			specRequired: []string{"limits"},
		},
		{
			file:         "kognition.dev_workspaces.yaml",
			kind:         "Workspace",
			specRequired: []string{},
		},
		{
			file:         "kognition.dev_agentidentities.yaml",
			kind:         "AgentIdentity",
			specRequired: []string{"agentRef"},
		},
		{
			file:         "kognition.dev_agentrolebindings.yaml",
			kind:         "AgentRoleBinding",
			specRequired: []string{"roleRef", "subjects"},
		},
		{
			file:         "kognition.dev_agentcommunicationpolicies.yaml",
			kind:         "AgentCommunicationPolicy",
			specRequired: []string{"defaultDeny"},
		},
		{
			file:         "kognition.dev_models.yaml",
			kind:         "Model",
			specRequired: []string{"className", "modelID"},
		},
		{
			file:         "kognition.dev_modelgrants.yaml",
			kind:         "ModelGrant",
			specRequired: []string{"agentRef", "modelRef"},
		},
		{
			file:         "kognition.dev_autonomyclasses.yaml",
			kind:         "AutonomyClass",
			specRequired: []string{"level", "requiresApproval"},
		},
		{
			file:         "kognition.dev_trustprofiles.yaml",
			kind:         "TrustProfile",
			specRequired: []string{"agentRef", "initialScore"},
		},
		{
			file:         "kognition.dev_solutions.yaml",
			kind:         "Solution",
			specRequired: []string{"goal"},
		},
		{
			file:         "kognition.dev_evaluations.yaml",
			kind:         "Evaluation",
			specRequired: []string{"targetRef", "datasetRef"},
			nestedChecks: map[string][]string{
				"targetRef":  {"kind", "name"},
				"datasetRef": {"name"},
			},
		},
		{
			file:         "kognition.dev_evaluationruns.yaml",
			kind:         "EvaluationRun",
			specRequired: []string{"evaluationRef", "targetRef", "datasetRef"},
			nestedChecks: map[string][]string{
				"evaluationRef": {"name"},
				"targetRef":     {"kind", "name"},
				"datasetRef":    {"name"},
			},
		},
		{
			file:         "kognition.dev_agentservices.yaml",
			kind:         "AgentService",
			specRequired: []string{"agentRef"},
		},
		{
			file:         "kognition.dev_intentgateways.yaml",
			kind:         "IntentGateway",
			specRequired: []string{"routes"},
		},
		{
			file:         "kognition.dev_crossnamespacegrants.yaml",
			kind:         "CrossNamespaceGrant",
			specRequired: []string{"from", "to", "resources"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.kind, func(t *testing.T) {
			path := filepath.Join(crdDir(), tt.file)
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("Failed to read CRD file: %v", err)
			}

			scheme := apimachineryruntime.NewScheme()
			_ = apiextensionsv1.AddToScheme(scheme)
			codecFactory := serializer.NewCodecFactory(scheme)
			obj, _, err := codecFactory.UniversalDeserializer().Decode(data, nil, nil)
			if err != nil {
				t.Fatalf("Failed to decode CRD: %v", err)
			}
			crd := obj.(*apiextensionsv1.CustomResourceDefinition)

			schema := crd.Spec.Versions[0].Schema.OpenAPIV3Schema
			specProps, ok := schema.Properties["spec"]
			if !ok {
				t.Fatal("spec not found in schema properties")
			}

			for _, reqField := range tt.specRequired {
				found := false
				for _, r := range specProps.Required {
					if r == reqField {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("Field %q not marked as required in spec", reqField)
				}
			}

			// Check nested required fields
			for parent, nestedRequired := range tt.nestedChecks {
				parentProp, ok := specProps.Properties[parent]
				if !ok {
					t.Errorf("Nested field %q not found in spec properties", parent)
					continue
				}
				for _, nr := range nestedRequired {
					found := false
					for _, r := range parentProp.Required {
						if r == nr {
							found = true
							break
						}
					}
					if !found {
						t.Errorf("Field %q not marked as required in spec.%s", nr, parent)
					}
				}
			}
		})
	}
}

// TestCRDSchemaSpecFields verifies key fields exist in the OpenAPI schema.
func TestCRDSchemaSpecFields(t *testing.T) {
	tests := []struct {
		file           string
		kind           string
		expectedFields []string
	}{
		{
			file: "kognition.dev_agents.yaml",
			kind: "Agent",
			expectedFields: []string{
				"modelRef", "instructions", "tools", "memory", "runtime",
				"companions", "knowledgeBaseClaims", "autonomyClassRef", "replicas",
				"probes", "restartLadder",
			},
		},
		{
			file: "kognition.dev_agentinstances.yaml",
			kind: "AgentInstance",
			expectedFields: []string{
				"agentRef", "revision", "sessionContext",
			},
		},
		{
			file: "kognition.dev_tools.yaml",
			kind: "Tool",
			expectedFields: []string{
				"className", "protocol", "description", "endpoint",
				"schema", "sandbox", "secretRefs",
			},
		},
		{
			file: "kognition.dev_toolclasses.yaml",
			kind: "ToolClass",
			expectedFields: []string{
				"provisioner", "parameters", "executionTier", "transport", "sandboxDefaults",
			},
		},
		{
			file: "kognition.dev_modelclasses.yaml",
			kind: "ModelClass",
			expectedFields: []string{
				"provisioner", "parameters", "latencyClass", "costTier",
				"contextWindow", "modalities", "healthProbe", "versionPin",
			},
		},
		{
			file: "kognition.dev_memoryclasses.yaml",
			kind: "MemoryClass",
			expectedFields: []string{
				"backendType", "parameters", "reclaimPolicy",
				"summarizationConfig", "consistencyMode",
			},
		},
		{
			file:           "kognition.dev_memories.yaml",
			kind:           "Memory",
			expectedFields: []string{"claimRef", "type", "ttl", "retentionPolicy"},
		},
		{
			file:           "kognition.dev_memoryrecords.yaml",
			kind:           "MemoryRecord",
			expectedFields: []string{"memoryRef", "content", "metadata", "provenance"},
		},
		{
			file:           "kognition.dev_memorygrants.yaml",
			kind:           "MemoryGrant",
			expectedFields: []string{"agentRef", "memoryClaimRef", "accessMode"},
		},
		{
			file: "kognition.dev_workflows.yaml",
			kind: "Workflow",
			expectedFields: []string{
				"steps", "entryPoint", "edges", "approvalGates", "budgets", "emergent",
			},
		},
		{
			file: "kognition.dev_workflowruns.yaml",
			kind: "WorkflowRun",
			expectedFields: []string{
				"workflowRef", "input", "parameters",
			},
		},
		{
			file: "kognition.dev_agentpolicies.yaml",
			kind: "AgentPolicy",
			expectedFields: []string{
				"rules", "enforce", "scope", "targetAgents", "toolGrants", "safetyRules", "complianceRules",
			},
		},
		{
			file: "kognition.dev_cognitiveadmissionpolicies.yaml",
			kind: "CognitiveAdmissionPolicy",
			expectedFields: []string{
				"rego", "matchKinds", "matchOperations", "mutationRules", "failurePolicy",
			},
		},
		{
			file: "kognition.dev_cognitivequotas.yaml",
			kind: "CognitiveQuota",
			expectedFields: []string{
				"hard", "tokenBudget", "costBudget", "resetInterval",
			},
		},
		{
			file: "kognition.dev_cognitivelimitranges.yaml",
			kind: "CognitiveLimitRange",
			expectedFields: []string{
				"limits",
			},
		},
		{
			file: "kognition.dev_workspaces.yaml",
			kind: "Workspace",
			expectedFields: []string{
				"displayName", "requireSignedArtifacts", "retention",
			},
		},
		{
			file: "kognition.dev_agentidentities.yaml",
			kind: "AgentIdentity",
			expectedFields: []string{
				"spiffeID", "agentRef",
			},
		},
		{
			file: "kognition.dev_agentrolebindings.yaml",
			kind: "AgentRoleBinding",
			expectedFields: []string{
				"roleRef", "subjects",
			},
		},
		{
			file:           "kognition.dev_agentcommunicationpolicies.yaml",
			kind:           "AgentCommunicationPolicy",
			expectedFields: []string{"allowFrom", "allowTo", "defaultDeny"},
		},
		{
			file: "kognition.dev_models.yaml",
			kind: "Model",
			expectedFields: []string{
				"provider", "className", "modelID", "endpoint",
				"capabilities", "credentialRef", "rateLimit", "parameters", "signature",
			},
		},
		{
			file:           "kognition.dev_modelgrants.yaml",
			kind:           "ModelGrant",
			expectedFields: []string{"agentRef", "modelRef"},
		},
		{
			file: "kognition.dev_secretrefs.yaml",
			kind: "SecretRef",
			expectedFields: []string{
				"backendType", "backendRef", "key",
				"provider", "path", "secretKey",
			},
		},
		{
			file: "kognition.dev_knowledgebaseclasses.yaml",
			kind: "KnowledgeBaseClass",
			expectedFields: []string{
				"provisioner", "indexMode", "reclaimPolicy", "parameters",
			},
		},
		{
			file: "kognition.dev_knowledgebases.yaml",
			kind: "KnowledgeBase",
			expectedFields: []string{
				"knowledgeBaseClassName", "sources", "indexConfig", "refreshPolicy",
			},
		},
		{
			file: "kognition.dev_autonomyclasses.yaml",
			kind: "AutonomyClass",
			expectedFields: []string{
				"level", "requiresApproval", "maxCostPerAction", "allowedTools",
				"approvalPolicy", "constraints",
			},
		},
		{
			file: "kognition.dev_trustprofiles.yaml",
			kind: "TrustProfile",
			expectedFields: []string{
				"agentRef", "initialScore", "scoringWeights", "promotionPolicy", "demotionPolicy",
			},
		},
		{
			file: "kognition.dev_prompts.yaml",
			kind: "Prompt",
			expectedFields: []string{
				"content", "parameters", "metadata", "instructions",
			},
		},
		{
			file: "kognition.dev_solutions.yaml",
			kind: "Solution",
			expectedFields: []string{
				"goal", "constraints", "preferences", "modelRef", "overrides",
			},
		},
		{
			file: "kognition.dev_evaluations.yaml",
			kind: "Evaluation",
			expectedFields: []string{
				"targetRef", "datasetRef", "graders", "metricThresholds", "schedule",
			},
		},
		{
			file: "kognition.dev_evaluationruns.yaml",
			kind: "EvaluationRun",
			expectedFields: []string{
				"evaluationRef", "targetRef", "datasetRef", "graders", "metricThresholds",
			},
		},
		{
			file: "kognition.dev_agentservices.yaml",
			kind: "AgentService",
			expectedFields: []string{
				"agentRef", "loadBalancingStrategy", "port", "selector",
			},
		},
		{
			file: "kognition.dev_intentgateways.yaml",
			kind: "IntentGateway",
			expectedFields: []string{
				"routes",
			},
		},
		{
			file: "kognition.dev_crossnamespacegrants.yaml",
			kind: "CrossNamespaceGrant",
			expectedFields: []string{
				"from", "to", "resources",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.kind, func(t *testing.T) {
			path := filepath.Join(crdDir(), tt.file)
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("Failed to read CRD file: %v", err)
			}

			scheme := apimachineryruntime.NewScheme()
			_ = apiextensionsv1.AddToScheme(scheme)
			codecFactory := serializer.NewCodecFactory(scheme)
			obj, _, err := codecFactory.UniversalDeserializer().Decode(data, nil, nil)
			if err != nil {
				t.Fatalf("Failed to decode CRD: %v", err)
			}
			crd := obj.(*apiextensionsv1.CustomResourceDefinition)

			schema := crd.Spec.Versions[0].Schema.OpenAPIV3Schema
			specProps, ok := schema.Properties["spec"]
			if !ok {
				t.Fatal("spec not found in schema properties")
			}

			for _, field := range tt.expectedFields {
				if _, exists := specProps.Properties[field]; !exists {
					t.Errorf("Expected field %q not found in spec schema", field)
				}
			}
		})
	}
}

// TestCRDStatusSubresource verifies that types with status have the subresource enabled.
func TestCRDStatusSubresource(t *testing.T) {
	typesWithStatus := []struct {
		file string
		kind string
	}{
		{"kognition.dev_agents.yaml", "Agent"},
		{"kognition.dev_agentinstances.yaml", "AgentInstance"},
		{"kognition.dev_tools.yaml", "Tool"},
		{"kognition.dev_toolgrants.yaml", "ToolGrant"},
		{"kognition.dev_toolclasses.yaml", "ToolClass"},
		{"kognition.dev_modelclasses.yaml", "ModelClass"},
		{"kognition.dev_memoryclasses.yaml", "MemoryClass"},
		{"kognition.dev_memories.yaml", "Memory"},
		{"kognition.dev_memoryrecords.yaml", "MemoryRecord"},
		{"kognition.dev_workflows.yaml", "Workflow"},
		{"kognition.dev_workflowruns.yaml", "WorkflowRun"},
		{"kognition.dev_evaluations.yaml", "Evaluation"},
		{"kognition.dev_cognitivequotas.yaml", "CognitiveQuota"},
		{"kognition.dev_workspaces.yaml", "Workspace"},
		{"kognition.dev_agentidentities.yaml", "AgentIdentity"},
		{"kognition.dev_models.yaml", "Model"},
		{"kognition.dev_secretrefs.yaml", "SecretRef"},
		{"kognition.dev_knowledgebases.yaml", "KnowledgeBase"},
	}

	for _, tt := range typesWithStatus {
		t.Run(tt.kind, func(t *testing.T) {
			path := filepath.Join(crdDir(), tt.file)
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("Failed to read CRD file: %v", err)
			}

			scheme := apimachineryruntime.NewScheme()
			_ = apiextensionsv1.AddToScheme(scheme)
			codecFactory := serializer.NewCodecFactory(scheme)
			obj, _, err := codecFactory.UniversalDeserializer().Decode(data, nil, nil)
			if err != nil {
				t.Fatalf("Failed to decode CRD: %v", err)
			}
			crd := obj.(*apiextensionsv1.CustomResourceDefinition)

			ver := crd.Spec.Versions[0]
			if ver.Subresources == nil || ver.Subresources.Status == nil {
				t.Error("Status subresource not enabled")
			}
		})
	}
}

// TestAgentRoleCRDTopLevelFields verifies AgentRole has rules at top level (no spec wrapper).
func TestAgentRoleCRDTopLevelFields(t *testing.T) {
	path := filepath.Join(crdDir(), "kognition.dev_agentroles.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("Failed to read CRD file: %v", err)
	}

	scheme := apimachineryruntime.NewScheme()
	_ = apiextensionsv1.AddToScheme(scheme)
	codecFactory := serializer.NewCodecFactory(scheme)
	obj, _, err := codecFactory.UniversalDeserializer().Decode(data, nil, nil)
	if err != nil {
		t.Fatalf("Failed to decode CRD: %v", err)
	}
	crd := obj.(*apiextensionsv1.CustomResourceDefinition)

	schema := crd.Spec.Versions[0].Schema.OpenAPIV3Schema
	if _, ok := schema.Properties["rules"]; !ok {
		t.Error("Expected top-level 'rules' field not found in AgentRole schema")
	}
	found := false
	for _, r := range schema.Required {
		if r == "rules" {
			found = true
			break
		}
	}
	if !found {
		t.Error("'rules' not marked as required at top level")
	}
}

// TestGoTypesToUnstructuredRoundtrip verifies Go types can be converted to/from unstructured.
func TestGoTypesToUnstructuredRoundtrip(t *testing.T) {
	s := apimachineryruntime.NewScheme()
	_ = v1alpha1.AddToScheme(s)

	replicas := int32(2)
	agent := &v1alpha1.Agent{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "kognition.dev/v1alpha1",
			Kind:       "Agent",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-agent",
			Namespace: "default",
		},
		Spec: v1alpha1.AgentSpec{
			ModelRef:     v1alpha1.LocalObjectReference{Name: "gpt4"},
			Instructions: v1alpha1.AgentInstructions{Inline: "You are an assistant"},
			Tools: []v1alpha1.AgentToolRef{
				{Ref: v1alpha1.LocalObjectReference{Name: "search"}, Required: true},
			},
			Replicas: &replicas,
		},
	}

	// Convert to unstructured
	data, err := json.Marshal(agent)
	if err != nil {
		t.Fatalf("Failed to marshal agent: %v", err)
	}

	var u unstructured.Unstructured
	if err := json.Unmarshal(data, &u.Object); err != nil {
		t.Fatalf("Failed to unmarshal to unstructured: %v", err)
	}

	// Verify unstructured has expected fields
	spec, ok := u.Object["spec"].(map[string]interface{})
	if !ok {
		t.Fatal("spec not found in unstructured")
	}
	modelRef, ok := spec["modelRef"].(map[string]interface{})
	if !ok {
		t.Fatal("modelRef not found in spec")
	}
	if modelRef["name"] != "gpt4" {
		t.Errorf("modelRef.name = %v, want gpt4", modelRef["name"])
	}

	// Convert back from unstructured
	backData, err := json.Marshal(u.Object)
	if err != nil {
		t.Fatalf("Failed to marshal unstructured: %v", err)
	}
	var restored v1alpha1.Agent
	if err := json.Unmarshal(backData, &restored); err != nil {
		t.Fatalf("Failed to unmarshal back to Agent: %v", err)
	}

	if restored.Spec.ModelRef.Name != "gpt4" {
		t.Errorf("Restored ModelRef.Name = %q, want gpt4", restored.Spec.ModelRef.Name)
	}
	if restored.Spec.Instructions.Inline != "You are an assistant" {
		t.Errorf("Restored Instructions = %q", restored.Spec.Instructions.Inline)
	}
	if len(restored.Spec.Tools) != 1 || restored.Spec.Tools[0].Ref.Name != "search" {
		t.Error("Restored Tools mismatch")
	}
	if restored.Spec.Replicas == nil || *restored.Spec.Replicas != 2 {
		t.Error("Restored Replicas mismatch")
	}
}

// TestWorkflowUnstructuredRoundtrip verifies Workflow Go types survive unstructured conversion.
func TestWorkflowUnstructuredRoundtrip(t *testing.T) {
	maxTokens := int64(100000)
	wf := &v1alpha1.Workflow{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "kognition.dev/v1alpha1",
			Kind:       "Workflow",
		},
		ObjectMeta: metav1.ObjectMeta{Name: "pipeline", Namespace: "default"},
		Spec: v1alpha1.WorkflowSpec{
			EntryPoint: "step1",
			Steps: []v1alpha1.WorkflowStep{
				{
					Name:     "step1",
					AgentRef: v1alpha1.LocalObjectReference{Name: "researcher"},
					Input:    "research topic",
				},
				{
					Name:     "step2",
					AgentRef: v1alpha1.LocalObjectReference{Name: "writer"},
				},
			},
			Edges: []v1alpha1.WorkflowEdge{
				{From: "step1", To: "step2", Condition: "steps.step1.output != ''"},
			},
			Budgets:  &v1alpha1.WorkflowBudget{MaxTokens: maxTokens, MaxDuration: "2h"},
			Emergent: &v1alpha1.WorkflowEmergentSpec{Enabled: true},
		},
	}

	data, err := json.Marshal(wf)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var u unstructured.Unstructured
	if err := json.Unmarshal(data, &u.Object); err != nil {
		t.Fatalf("Unmarshal to unstructured: %v", err)
	}

	// Verify structural traversal
	spec := u.Object["spec"].(map[string]interface{})
	steps := spec["steps"].([]interface{})
	if len(steps) != 2 {
		t.Fatalf("Expected 2 steps, got %d", len(steps))
	}
	step1 := steps[0].(map[string]interface{})
	if step1["name"] != "step1" {
		t.Errorf("step1 name = %v", step1["name"])
	}

	// Roundtrip back
	backData, err := json.Marshal(u.Object)
	if err != nil {
		t.Fatalf("Marshal back: %v", err)
	}
	var restored v1alpha1.Workflow
	if err := json.Unmarshal(backData, &restored); err != nil {
		t.Fatalf("Unmarshal back: %v", err)
	}
	if restored.Spec.EntryPoint != "step1" {
		t.Errorf("EntryPoint = %q", restored.Spec.EntryPoint)
	}
	if restored.Spec.Emergent == nil || !restored.Spec.Emergent.Enabled {
		t.Error("Emergent should be true")
	}
	if restored.Spec.Budgets == nil || restored.Spec.Budgets.MaxTokens != 100000 {
		t.Error("Budgets.MaxTokens mismatch")
	}
}

// TestToolUnstructuredRoundtrip verifies Tool Go types survive unstructured conversion.
func TestToolUnstructuredRoundtrip(t *testing.T) {
	tool := &v1alpha1.Tool{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "kognition.dev/v1alpha1",
			Kind:       "Tool",
		},
		ObjectMeta: metav1.ObjectMeta{Name: "web-search", Namespace: "default"},
		Spec: v1alpha1.ToolSpec{
			ClassName:   "mcp-browser",
			Protocol:    v1alpha1.ToolProtocolMCP,
			Description: "Web search tool",
			Endpoint:    "http://mcp:8080",
			Metadata:    map[string]string{"maxResults": "10"},
			Sandbox: &v1alpha1.ToolSandboxSpec{
				CPUMilli:      500,
				MemoryMB:      256,
				Timeout:       "30s",
				NetworkPolicy: "restricted",
			},
			SecretRefs: []v1alpha1.ToolSecretRef{{Name: "api-key", Key: "token"}},
		},
	}

	data, err := json.Marshal(tool)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var u unstructured.Unstructured
	if err := json.Unmarshal(data, &u.Object); err != nil {
		t.Fatalf("Unmarshal to unstructured: %v", err)
	}

	backData, err := json.Marshal(u.Object)
	if err != nil {
		t.Fatalf("Marshal back: %v", err)
	}
	var restored v1alpha1.Tool
	if err := json.Unmarshal(backData, &restored); err != nil {
		t.Fatalf("Unmarshal back: %v", err)
	}

	if restored.Spec.Protocol != v1alpha1.ToolProtocolMCP {
		t.Errorf("Protocol = %q, want %q", restored.Spec.Protocol, v1alpha1.ToolProtocolMCP)
	}
	if restored.Spec.Sandbox == nil || restored.Spec.Sandbox.CPUMilli != 500 {
		t.Error("Sandbox.CPUMilli mismatch")
	}
	if restored.Spec.Metadata["maxResults"] != "10" {
		t.Error("Metadata mismatch")
	}
	if len(restored.Spec.SecretRefs) != 1 || restored.Spec.SecretRefs[0].Name != "api-key" || restored.Spec.SecretRefs[0].Key != "token" {
		t.Error("SecretRefs mismatch")
	}
}

// TestModelUnstructuredRoundtrip verifies Model Go types survive unstructured conversion.
func TestModelUnstructuredRoundtrip(t *testing.T) {
	rateLimit := int64(120)
	model := &v1alpha1.Model{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "kognition.dev/v1alpha1",
			Kind:       "Model",
		},
		ObjectMeta: metav1.ObjectMeta{Name: "gpt-4o", Namespace: "default"},
		Spec: v1alpha1.ModelSpec{
			Provider:      "openai-compatible",
			ClassName:     "fast",
			ModelID:       "gpt-4o",
			Endpoint:      "https://api.example.com/v1",
			Capabilities:  []string{"chat", "tool-calling"},
			CredentialRef: &v1alpha1.LocalObjectReference{Name: "openai-creds"},
			RateLimit:     &rateLimit,
			Parameters:    map[string]string{"temperature": "0.7"},
			Signature:     "cosign-sig",
		},
	}

	data, err := json.Marshal(model)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var u unstructured.Unstructured
	if err := json.Unmarshal(data, &u.Object); err != nil {
		t.Fatalf("Unmarshal to unstructured: %v", err)
	}

	spec, ok := u.Object["spec"].(map[string]interface{})
	if !ok {
		t.Fatal("spec not found in unstructured")
	}
	if spec["className"] != "fast" {
		t.Errorf("spec.className = %v", spec["className"])
	}
	if spec["modelID"] != "gpt-4o" {
		t.Errorf("spec.modelID = %v", spec["modelID"])
	}
	credRef, ok := spec["credentialRef"].(map[string]interface{})
	if !ok {
		t.Fatal("credentialRef not found in spec")
	}
	if credRef["name"] != "openai-creds" {
		t.Errorf("credentialRef.name = %v", credRef["name"])
	}

	backData, err := json.Marshal(u.Object)
	if err != nil {
		t.Fatalf("Marshal back: %v", err)
	}
	var restored v1alpha1.Model
	if err := json.Unmarshal(backData, &restored); err != nil {
		t.Fatalf("Unmarshal back: %v", err)
	}

	if restored.Spec.ClassName != "fast" || restored.Spec.ModelID != "gpt-4o" {
		t.Errorf("Restored ClassName/ModelID mismatch: %+v", restored.Spec)
	}
	if restored.Spec.CredentialRef == nil || restored.Spec.CredentialRef.Name != "openai-creds" {
		t.Errorf("Restored CredentialRef mismatch: %+v", restored.Spec.CredentialRef)
	}
	if restored.Spec.RateLimit == nil || *restored.Spec.RateLimit != 120 {
		t.Errorf("Restored RateLimit mismatch: %+v", restored.Spec.RateLimit)
	}
	if len(restored.Spec.Capabilities) != 2 || restored.Spec.Capabilities[0] != "chat" {
		t.Errorf("Restored Capabilities mismatch: %+v", restored.Spec.Capabilities)
	}
	if restored.Spec.Parameters["temperature"] != "0.7" {
		t.Errorf("Restored Parameters mismatch: %+v", restored.Spec.Parameters)
	}
	if restored.Spec.Signature != "cosign-sig" {
		t.Errorf("Restored Signature = %q", restored.Spec.Signature)
	}
}

// TestSecretRefUnstructuredRoundtrip verifies SecretRef Go types survive unstructured conversion.
func TestSecretRefUnstructuredRoundtrip(t *testing.T) {
	sr := &v1alpha1.SecretRef{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "kognition.dev/v1alpha1",
			Kind:       "SecretRef",
		},
		ObjectMeta: metav1.ObjectMeta{Name: "openai-creds", Namespace: "default"},
		Spec: v1alpha1.SecretRefSpec{
			BackendType: "vault",
			BackendRef:  "secret/data/openai",
			Key:         "apiKey",
			Provider:    "vault-legacy",
			Path:        "secret/openai",
			SecretKey:   "legacy-key",
		},
		Status: v1alpha1.SecretRefStatus{
			Synced:       true,
			LastSyncTime: "2024-01-01T00:00:00Z",
		},
	}

	data, err := json.Marshal(sr)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var u unstructured.Unstructured
	if err := json.Unmarshal(data, &u.Object); err != nil {
		t.Fatalf("Unmarshal to unstructured: %v", err)
	}

	spec, ok := u.Object["spec"].(map[string]interface{})
	if !ok {
		t.Fatal("spec not found in unstructured")
	}
	if spec["backendType"] != "vault" {
		t.Errorf("spec.backendType = %v", spec["backendType"])
	}
	if spec["secretKey"] != "legacy-key" {
		t.Errorf("spec.secretKey = %v", spec["secretKey"])
	}

	backData, err := json.Marshal(u.Object)
	if err != nil {
		t.Fatalf("Marshal back: %v", err)
	}
	var restored v1alpha1.SecretRef
	if err := json.Unmarshal(backData, &restored); err != nil {
		t.Fatalf("Unmarshal back: %v", err)
	}

	if restored.Spec.BackendType != "vault" || restored.Spec.BackendRef != "secret/data/openai" || restored.Spec.Key != "apiKey" {
		t.Errorf("Restored canonical spec mismatch: %+v", restored.Spec)
	}
	if restored.Spec.Provider != "vault-legacy" || restored.Spec.Path != "secret/openai" || restored.Spec.SecretKey != "legacy-key" {
		t.Errorf("Restored legacy spec mismatch: %+v", restored.Spec)
	}
	if !restored.Status.Synced || restored.Status.LastSyncTime != "2024-01-01T00:00:00Z" {
		t.Errorf("Restored Status mismatch: %+v", restored.Status)
	}
}

// TestCRDPrinterColumns verifies printer columns are defined correctly.
func TestCRDPrinterColumns(t *testing.T) {
	tests := []struct {
		file    string
		kind    string
		columns []string // expected column names
	}{
		{"kognition.dev_agents.yaml", "Agent", []string{"Ready", "Up-to-date", "Available", "Age", "Phase"}},
		{"kognition.dev_agentinstances.yaml", "AgentInstance", []string{"Agent", "Phase", "Age"}},
		{"kognition.dev_toolgrants.yaml", "ToolGrant", []string{"Agent", "Tool", "Active", "Age"}},
		{"kognition.dev_workflows.yaml", "Workflow", []string{"Entry", "Age"}},
		{"kognition.dev_workflowruns.yaml", "WorkflowRun", []string{"Workflow", "Phase", "Active", "Age"}},
	}

	for _, tt := range tests {
		t.Run(tt.kind, func(t *testing.T) {
			path := filepath.Join(crdDir(), tt.file)
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("Failed to read CRD file: %v", err)
			}

			scheme := apimachineryruntime.NewScheme()
			_ = apiextensionsv1.AddToScheme(scheme)
			codecFactory := serializer.NewCodecFactory(scheme)
			obj, _, err := codecFactory.UniversalDeserializer().Decode(data, nil, nil)
			if err != nil {
				t.Fatalf("Failed to decode CRD: %v", err)
			}
			crd := obj.(*apiextensionsv1.CustomResourceDefinition)

			ver := crd.Spec.Versions[0]
			colNames := make(map[string]bool)
			for _, col := range ver.AdditionalPrinterColumns {
				colNames[col.Name] = true
			}

			for _, expected := range tt.columns {
				if !colNames[expected] {
					t.Errorf("Expected printer column %q not found", expected)
				}
			}
		})
	}
}
