// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Command studio-catalog generates Studio's field catalog from the shipped CRDs.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"time"

	api "github.com/kognition-ai/kognition/api/v1alpha1"
	"sigs.k8s.io/yaml"
)

type resource struct {
	Kind             string         `json:"kind"`
	Plural           string         `json:"plural"`
	Namespaced       bool           `json:"namespaced"`
	Group            string         `json:"group"`
	Treatment        string         `json:"treatment"`
	Reason           string         `json:"reason,omitempty"`
	Schema           map[string]any `json:"schema,omitempty"`
	KubernetesSchema map[string]any `json:"kubernetesSchema,omitempty"`
}

func main() {
	if err := generate("."); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func generate(root string) error {
	data, err := catalog(root)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(root, "cmd", "kognitionctl", "cmd", "studio_assets", "resource-catalog.json"), data, 0o644)
}

func catalog(root string) ([]byte, error) {
	files, err := filepath.Glob(filepath.Join(root, "deploy", "helm", "kognition", "crds", "*.yaml"))
	if err != nil {
		return nil, err
	}
	resources := map[string]resource{}
	for _, file := range files {
		raw, err := os.ReadFile(file)
		if err != nil {
			return nil, err
		}
		var crd struct {
			Spec struct {
				Names struct {
					Kind   string `json:"kind"`
					Plural string `json:"plural"`
				} `json:"names"`
				Scope    string `json:"scope"`
				Versions []struct {
					Name   string `json:"name"`
					Schema struct {
						OpenAPI map[string]any `json:"openAPIV3Schema"`
					} `json:"schema"`
				} `json:"versions"`
			} `json:"spec"`
		}
		if err := yaml.Unmarshal(raw, &crd); err != nil {
			return nil, fmt.Errorf("%s: %w", file, err)
		}
		for _, version := range crd.Spec.Versions {
			if version.Name != api.Version {
				continue
			}
			properties, ok := version.Schema.OpenAPI["properties"].(map[string]any)
			if !ok {
				return nil, fmt.Errorf("%s: missing resource properties", file)
			}
			for _, name := range []string{"apiVersion", "kind", "metadata", "status"} {
				delete(properties, name)
			}
			resources[crd.Spec.Names.Kind] = resource{
				Kind: crd.Spec.Names.Kind, Plural: crd.Spec.Names.Plural,
				Namespaced:       crd.Spec.Scope == "Namespaced",
				KubernetesSchema: map[string]any{"type": "object", "properties": properties},
			}
		}
	}
	kinds := api.RegisteredKinds()
	sort.Strings(kinds)
	result := make([]resource, 0, len(kinds))
	for _, kind := range kinds {
		r, found := resources[kind]
		if !found && api.IsKubernetesNativeKind(kind) {
			return nil, fmt.Errorf("registered kind %s has no shipped CRD schema", kind)
		}
		r.Kind = kind
		r.Schema = typeSchema(reflect.TypeOf(api.KindRegistry[kind]()).Elem(), map[reflect.Type]bool{})
		props := r.Schema["properties"].(map[string]any)
		for _, name := range []string{"apiVersion", "kind", "metadata", "status"} {
			delete(props, name)
		}
		delete(r.Schema, "required")
		r.Group, r.Treatment = "Build", "author"
		if r.Plural == "" {
			r.Plural = strings.ToLower(kind) + "s"
		}
		switch kind {
		case "AutonomyClass", "TrustProfile", "Workspace", "AgentPolicy", "CognitiveAdmissionPolicy",
			"CognitiveQuota", "CognitiveLimitRange", "AgentRole", "AgentRoleBinding",
			"AgentCommunicationPolicy", "ModelGrant", "MemoryGrant", "ToolGrant", "CrossNamespaceGrant", "ExternalAgentGrant":
			r.Group = "Govern"
		case "MemoryClass", "MemoryClaim", "KnowledgeBaseClass", "KnowledgeBaseClaim", "SecretRef", "KnowledgeBaseIndexSchedule":
			r.Group = "Operate"
		case "Evaluation", "EvaluationDataset", "EvaluationRun":
			r.Group = "Test"
		}
		switch kind {
		case "AgentJob", "AgentInstance", "WorkflowRun", "KnowledgeBaseIndexRun", "EvaluationRun":
			r.Treatment = "launch"
			r.Reason = "Creating this resource starts execution. Existing runs are read-only."
		case "MemoryRecord", "ToolInvocationRun", "AutonomyReview", "ApprovalDecision":
			r.Treatment = "view"
			r.Reason = "Controller-owned or operational evidence. Use the supported workload, memory or authenticated decision workflow; generated state is read-only."
		case "RuntimeApproval", "ControlPlaneRole", "ControlPlaneRoleBinding":
			r.Treatment = "unavailable"
			r.Reason = "Protected authority resource: excluded from Studio's generic API. Use the authenticated administrative or decision interface."
		case "ModelAutoscaler":
			r.Treatment = "unavailable"
			r.Reason = "Model-tier scaling is deferred; a registered schema does not establish backend support."
		}
		inheritHints(r.Schema, r.KubernetesSchema)
		customize(kind, "", r.Schema)
		customize(kind, "", r.KubernetesSchema)
		result = append(result, r)
	}
	data, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

func customize(kind, path string, schema map[string]any) {
	if schema == nil {
		return
	}
	enums := map[string][]string{
		"SecretRef.backendType":                              {"internal", "env"},
		"SecretRef.provider":                                 {"internal", "env"},
		"MemoryClaim.type":                                   {"episodic", "semantic", "working", "procedural", "shared"},
		"MemoryClaim.accessModes[]":                          {"ReadWrite", "ReadOnly", "Shared"},
		"AutonomyClass.approvalPolicy.toolInvocation":        {"always", "never", "high-risk-only"},
		"AutonomyClass.approvalPolicy.memoryWrite":           {"always", "never", "high-risk-only"},
		"AutonomyClass.approvalPolicy.externalCommunication": {"always", "never", "high-risk-only"},
	}
	localPath := strings.TrimPrefix(path, "spec.")
	if values, ok := enums[kind+"."+localPath]; ok {
		schema["enum"] = values
	}
	if (kind == "AutonomyClass" && localPath == "approvalPolicy.costThreshold") ||
		((kind == "MemoryClass" || kind == "MemoryClaim") && (localPath == "consistencyMode" || localPath == "concurrencyMode")) {
		schema["readOnly"] = true
		schema["description"] = "Stored compatibility field; broader enforcement semantics are not certified. Preserved on import, not offered as an implemented control."
	}
	if kind == "MemoryVolume" && localPath == "claimRef" {
		schema["readOnly"] = true
		schema["description"] = "Claim bindings are assigned by the controller, not by the static-volume author."
	}

	properties, _ := schema["properties"].(map[string]any)
	for name, child := range properties {
		field, ok := child.(map[string]any)
		if !ok {
			continue
		}
		childPath := name
		if path != "" {
			childPath = path + "." + name
		}
		customize(kind, childPath, field)
	}
	if items, ok := schema["items"].(map[string]any); ok {
		customize(kind, path+"[]", items)
	}
	if values, ok := schema["additionalProperties"].(map[string]any); ok {
		customize(kind, path+"{}", values)
	}
}

func inheritHints(target, source map[string]any) {
	if target == nil || source == nil {
		return
	}
	if target["type"] == source["type"] {
		for _, key := range []string{"description", "enum", "minimum", "maximum", "pattern", "minLength", "maxLength"} {
			if value, ok := source[key]; ok {
				target[key] = value
			}
		}
	}
	properties, _ := target["properties"].(map[string]any)
	other, _ := source["properties"].(map[string]any)
	for name, child := range properties {
		a, _ := child.(map[string]any)
		b, _ := other[name].(map[string]any)
		inheritHints(a, b)
	}
	for _, key := range []string{"items", "additionalProperties"} {
		a, _ := target[key].(map[string]any)
		b, _ := source[key].(map[string]any)
		inheritHints(a, b)
	}
}

func typeSchema(t reflect.Type, ancestors map[reflect.Type]bool) map[string]any {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t == reflect.TypeFor[time.Time]() || t == reflect.TypeFor[api.Duration]() {
		return map[string]any{"type": "string"}
	}
	if t == reflect.TypeFor[json.RawMessage]() || t.Kind() == reflect.Interface || ancestors[t] {
		return map[string]any{"x-kubernetes-preserve-unknown-fields": true}
	}
	if t.Kind() == reflect.Slice && t.Elem().Kind() == reflect.Uint8 {
		return map[string]any{"type": "string", "format": "byte"}
	}
	switch t.Kind() {
	case reflect.Bool:
		return map[string]any{"type": "boolean"}
	case reflect.String:
		return map[string]any{"type": "string"}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64, reflect.Uint, reflect.Uint32, reflect.Uint64:
		return map[string]any{"type": "integer"}
	case reflect.Float32, reflect.Float64:
		return map[string]any{"type": "number"}
	case reflect.Slice, reflect.Array:
		return map[string]any{"type": "array", "items": typeSchema(t.Elem(), ancestors)}
	case reflect.Map:
		return map[string]any{"type": "object", "additionalProperties": typeSchema(t.Elem(), ancestors)}
	case reflect.Struct:
		next := make(map[reflect.Type]bool, len(ancestors)+1)
		for key, value := range ancestors {
			next[key] = value
		}

		next[t] = true
		properties := map[string]any{}
		var required []string
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			if !f.IsExported() {
				continue
			}
			tag := strings.Split(f.Tag.Get("json"), ",")
			name := tag[0]
			if name == "-" {
				continue
			}
			child := typeSchema(f.Type, next)
			if f.Anonymous && name == "" {
				for key, value := range child["properties"].(map[string]any) {
					properties[key] = value
				}
				continue
			}
			if name == "" {
				name = f.Name
			}
			properties[name] = child
			if !strings.Contains(f.Tag.Get("json"), "omitempty") {
				required = append(required, name)
			}
		}
		result := map[string]any{"type": "object", "properties": properties}
		if len(required) > 0 {
			result["required"] = required
		}
		return result
	default:
		return map[string]any{"x-kubernetes-preserve-unknown-fields": true}
	}
}
