// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"gopkg.in/yaml.v3"
)

// knownResources maps singular to plural forms for all Kognition resource types
// that have installed CRDs. Aspirational kinds without CRDs are intentionally
// omitted so the dashboard catalog and CLI commands do not surface unusable kinds.
var knownResources = map[string]string{
	"agent":                      "agents",
	"agentinstance":              "agentinstances",
	"agentjob":                   "agentjobs",
	"agentautoscaler":            "agentautoscalers",
	"agentcommunicationpolicy":   "agentcommunicationpolicies",
	"agentidentity":              "agentidentities",
	"agentpolicy":                "agentpolicies",
	"agentrole":                  "agentroles",
	"agentrolebinding":           "agentrolebindings",
	"agentservice":               "agentservices",
	"approvaldecision":           "approvaldecisions",
	"autonomyclass":              "autonomyclasses",
	"autonomyreview":             "autonomyreviews",
	"cognitiveadmissionpolicy":   "cognitiveadmissionpolicies",
	"cognitivelimitrange":        "cognitivelimitranges",
	"cognitivequota":             "cognitivequotas",
	"crossnamespacegrant":        "crossnamespacegrants",
	"evaluation":                 "evaluations",
	"evaluationdataset":          "evaluationdatasets",
	"evaluationrun":              "evaluationruns",
	"intentgateway":              "intentgateways",
	"knowledgebase":              "knowledgebases",
	"knowledgebaseclass":         "knowledgebaseclasses",
	"knowledgebaseclaim":         "knowledgebaseclaims",
	"knowledgebaseindexrun":      "knowledgebaseindexruns",
	"knowledgebaseindexschedule": "knowledgebaseindexschedules",
	"memory":                     "memories",
	"memoryclaim":                "memoryclaims",
	"memoryclass":                "memoryclasses",
	"memorygrant":                "memorygrants",
	"memoryrecord":               "memoryrecords",
	"memoryvolume":               "memoryvolumes",
	"model":                      "models",
	"modelautoscaler":            "modelautoscalers",
	"modelclass":                 "modelclasses",
	"modelgrant":                 "modelgrants",
	"modelinvocationprofile":     "modelinvocationprofiles",
	"prompt":                     "prompts",
	"secretref":                  "secretrefs",
	"solution":                   "solutions",
	"tool":                       "tools",
	"toolclass":                  "toolclasses",
	"toolgrant":                  "toolgrants",
	"toolinvocationrun":          "toolinvocationruns",
	"trustprofile":               "trustprofiles",
	"workflow":                   "workflows",
	"workflowrun":                "workflowruns",
	"workspace":                  "workspaces",
}

// pluralToSingular is the reverse mapping.
var pluralToSingular map[string]string

// resourceKind maps singular resource names to their Kubernetes Kind
// (CamelCase) as registered on the CRDs. Used when constructing field
// selectors against involvedObject.kind for core/v1 Events.
var resourceKind = map[string]string{
	"agent":                      "Agent",
	"agentinstance":              "AgentInstance",
	"agentjob":                   "AgentJob",
	"agentautoscaler":            "AgentAutoscaler",
	"agentcommunicationpolicy":   "AgentCommunicationPolicy",
	"agentidentity":              "AgentIdentity",
	"agentpolicy":                "AgentPolicy",
	"agentrole":                  "AgentRole",
	"agentrolebinding":           "AgentRoleBinding",
	"agentservice":               "AgentService",
	"approvaldecision":           "ApprovalDecision",
	"autonomyclass":              "AutonomyClass",
	"autonomyreview":             "AutonomyReview",
	"cognitiveadmissionpolicy":   "CognitiveAdmissionPolicy",
	"cognitivelimitrange":        "CognitiveLimitRange",
	"cognitivequota":             "CognitiveQuota",
	"crossnamespacegrant":        "CrossNamespaceGrant",
	"evaluation":                 "Evaluation",
	"evaluationdataset":          "EvaluationDataset",
	"evaluationrun":              "EvaluationRun",
	"intentgateway":              "IntentGateway",
	"knowledgebase":              "KnowledgeBase",
	"knowledgebaseclass":         "KnowledgeBaseClass",
	"knowledgebaseclaim":         "KnowledgeBaseClaim",
	"knowledgebaseindexrun":      "KnowledgeBaseIndexRun",
	"knowledgebaseindexschedule": "KnowledgeBaseIndexSchedule",
	"memory":                     "Memory",
	"memoryclaim":                "MemoryClaim",
	"memoryclass":                "MemoryClass",
	"memorygrant":                "MemoryGrant",
	"memoryrecord":               "MemoryRecord",
	"memoryvolume":               "MemoryVolume",
	"model":                      "Model",
	"modelautoscaler":            "ModelAutoscaler",
	"modelclass":                 "ModelClass",
	"modelgrant":                 "ModelGrant",
	"modelinvocationprofile":     "ModelInvocationProfile",
	"prompt":                     "Prompt",
	"secretref":                  "SecretRef",
	"solution":                   "Solution",
	"tool":                       "Tool",
	"toolclass":                  "ToolClass",
	"toolgrant":                  "ToolGrant",
	"toolinvocationrun":          "ToolInvocationRun",
	"trustprofile":               "TrustProfile",
	"workflow":                   "Workflow",
	"workflowrun":                "WorkflowRun",
	"workspace":                  "Workspace",
}

// kindFor returns the Kubernetes Kind for a resource name in any form
// (singular, plural, or already the Kind). Returns "" if unknown.
func kindFor(resource string) string {
	if k, ok := resourceKind[singularize(resource)]; ok {
		return k
	}
	return ""
}

func init() {
	pluralToSingular = make(map[string]string, len(knownResources))
	for s, p := range knownResources {
		pluralToSingular[p] = s
	}
}

// pluralize converts a resource type argument to its plural API path form.
func pluralize(resource string) string {
	r := strings.ToLower(resource)
	if p, ok := knownResources[r]; ok {
		return p
	}
	// Maybe already plural
	if _, ok := pluralToSingular[r]; ok {
		return r
	}
	// Fallback: naive pluralization
	return r + "s"
}

// singularize converts a plural resource type to singular.
func singularize(resource string) string {
	r := strings.ToLower(resource)
	if _, ok := knownResources[r]; ok {
		return r
	}
	if s, ok := pluralToSingular[r]; ok {
		return s
	}
	return r
}

// apiURL builds the base URL for a namespaced resource collection.
func apiURL(ns, resource string) string {
	return fmt.Sprintf("%s/apis/kognition.dev/v1alpha1/namespaces/%s/%s",
		serverURL, ns, pluralize(resource))
}

// apiURLName builds the URL for a specific named resource.
func apiURLName(ns, resource, name string) string {
	return fmt.Sprintf("%s/%s", apiURL(ns, resource), name)
}

// doGet performs a GET request and returns the response body bytes.
func doGet(url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("server error (%d): %s", resp.StatusCode, string(body))
	}
	return body, nil
}

// printOutput formats and prints the given JSON data according to the output format flag.
func printOutput(data []byte) error {
	switch outputFormat {
	case "json":
		var pretty map[string]any
		if err := json.Unmarshal(data, &pretty); err != nil {
			fmt.Println(string(data))
			return nil
		}
		out, _ := json.MarshalIndent(pretty, "", "  ")
		fmt.Println(string(out))
	default:
		// Default to YAML
		var obj any
		if err := json.Unmarshal(data, &obj); err != nil {
			fmt.Println(string(data))
			return nil
		}
		out, _ := yaml.Marshal(obj)
		fmt.Print(string(out))
	}
	return nil
}
