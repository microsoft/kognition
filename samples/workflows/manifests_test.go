// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package workflows_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/yaml"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/admission"
)

func TestHTTPToolWorkflowDefinesRequiredRBAC(t *testing.T) {
	objects := loadManifestObjects(t, "tool-steps-http.yaml")

	role := requireManifestObject(t, objects, "AgentRole/wfdemo-tool-http-invoker")
	rules, found, err := unstructured.NestedSlice(role.Object, "rules")
	if err != nil || !found || len(rules) != 1 {
		t.Fatalf("AgentRole rules = %#v, found = %v, err = %v", rules, found, err)
	}
	rule, ok := rules[0].(map[string]interface{})
	if !ok || !nestedStringsContain(rule, "Tool", "resources") || !nestedStringsContain(rule, "invoke", "verbs") {
		t.Fatalf("AgentRole rule does not grant Tool invoke: %#v", rules[0])
	}

	binding := requireManifestObject(t, objects, "AgentRoleBinding/wfdemo-tool-http-invoker")
	roleName, found, err := unstructured.NestedString(binding.Object, "spec", "roleRef", "name")
	if err != nil || !found || roleName != "wfdemo-tool-http-invoker" {
		t.Fatalf("AgentRoleBinding roleRef.name = %q, found = %v, err = %v", roleName, found, err)
	}
	subjects, found, err := unstructured.NestedSlice(binding.Object, "spec", "subjects")
	if err != nil || !found || !hasSubject(subjects, "Agent", "wfdemo-llm") {
		t.Fatalf("AgentRoleBinding subjects = %#v, found = %v, err = %v", subjects, found, err)
	}
}

func TestSharedMemoryWorkflowAllowsItsAgentContinuation(t *testing.T) {
	objects := loadManifestObjects(t, "shared-memory.yaml")

	policy := requireManifestObject(t, objects, "AgentCommunicationPolicy/wfdemo-shared-memory-communication")
	defaultDeny, found, err := unstructured.NestedBool(policy.Object, "spec", "defaultDeny")
	if err != nil || !found || !defaultDeny {
		t.Fatalf("AgentCommunicationPolicy spec.defaultDeny = %t, found = %v, err = %v", defaultDeny, found, err)
	}
	allowFrom, found, err := unstructured.NestedSlice(policy.Object, "spec", "allowFrom")
	if err != nil || !found || !hasLocalObjectReference(allowFrom, "wfdemo-llm") {
		t.Fatalf("AgentCommunicationPolicy spec.allowFrom = %#v, found = %v, err = %v", allowFrom, found, err)
	}
	allowTo, found, err := unstructured.NestedSlice(policy.Object, "spec", "allowTo")
	if err != nil || !found || !hasLocalObjectReference(allowTo, "wfdemo-llm") {
		t.Fatalf("AgentCommunicationPolicy spec.allowTo = %#v, found = %v, err = %v", allowTo, found, err)
	}
}

func TestSolutionSamplesAllowOnlyTheirWorkflowEdges(t *testing.T) {
	objects := loadManifestObjects(t, "../solution/manifests.yaml")

	edges := []struct {
		policy string
		from   string
		to     string
	}{
		{"write-blog-post-planner-to-researcher", "write-blog-post-agent", "write-blog-post-researcher"},
		{"write-blog-post-researcher-to-writer", "write-blog-post-researcher", "write-blog-post-writer"},
		{"write-blog-post-writer-to-publisher", "write-blog-post-writer", "write-blog-post-publisher"},
		{"analyze-dataset-planner-to-researcher", "analyze-dataset-agent", "analyze-dataset-researcher"},
		{"analyze-dataset-researcher-to-writer", "analyze-dataset-researcher", "analyze-dataset-writer"},
		{"analyze-dataset-writer-to-publisher", "analyze-dataset-writer", "analyze-dataset-publisher"},
	}

	for _, edge := range edges {
		t.Run(edge.policy, func(t *testing.T) {
			policy := requireManifestObject(t, objects, "AgentCommunicationPolicy/"+edge.policy)
			defaultDeny, found, err := unstructured.NestedBool(policy.Object, "spec", "defaultDeny")
			if err != nil || !found || !defaultDeny {
				t.Fatalf("spec.defaultDeny = %t, found = %v, err = %v", defaultDeny, found, err)
			}
			requireExactLocalObjectReference(t, policy, "allowFrom", edge.from)
			requireExactLocalObjectReference(t, policy, "allowTo", edge.to)
		})
	}
}

func TestCollaborationSampleWorkflowsAreAdmitted(t *testing.T) {
	objects := loadManifestObjects(t, "collaboration.yaml")
	validator := admission.NewWorkflowValidator(admission.WithCollaborationEnabled(true))
	disabled := admission.NewWorkflowValidator()

	for _, name := range []string{"wfdemo-team-parallel", "wfdemo-team-supervisor"} {
		t.Run(name, func(t *testing.T) {
			object := requireManifestObject(t, objects, "Workflow/"+name)
			raw, err := json.Marshal(object.Object)
			if err != nil {
				t.Fatal(err)
			}
			var strict v1alpha1.Workflow
			decoder := json.NewDecoder(bytes.NewReader(raw))
			decoder.DisallowUnknownFields()
			if err := decoder.Decode(&strict); err != nil {
				t.Fatalf("sample does not match the Workflow schema: %v", err)
			}
			request := &admission.Request{Operation: admission.OperationCreate, Kind: "Workflow", Namespace: object.GetNamespace(), Name: name, Object: raw}
			if response := validator.Validate(context.Background(), request); !response.Allowed {
				t.Fatalf("sample rejected with collaboration enabled: %s", response.Reason)
			}
			if response := disabled.Validate(context.Background(), request); response.Allowed {
				t.Fatal("sample must be rejected when collaboration is disabled")
			}
		})
	}
}

func TestToolSandboxImplementsMCPHandshake(t *testing.T) {
	server := toolSandboxServer(t)

	for _, required := range []string{
		"if method == 'initialize':",
		"'protocolVersion': '2025-03-26'",
		"if method == 'notifications/initialized':",
	} {
		if !strings.Contains(server, required) {
			t.Errorf("server.py does not contain %q", required)
		}
	}
}

func TestToolSandboxMCPRoundTripWithoutModel(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		python, err = exec.LookPath("python")
	}
	if err != nil {
		t.Skip("Python is required to execute the embedded tool sandbox")
	}
	server, _, found := strings.Cut(toolSandboxServer(t), "print('tool-sandbox listening on :8080', flush=True)")
	if !found {
		t.Fatal("embedded sample server entrypoint not found")
	}
	probe := server + `
import threading, urllib.request

server = HTTPServer(('127.0.0.1', 0), H)
worker = threading.Thread(target=server.serve_forever)
worker.start()
try:
    endpoint = 'http://127.0.0.1:%s/mcp' % server.server_port
    def request(body):
        payload = json.dumps(body).encode()
        call = urllib.request.Request(endpoint, data=payload, headers={'Content-Type': 'application/json'})
        with urllib.request.urlopen(call, timeout=5) as response:
            return response.status, json.load(response)
    _, initialized = request({'jsonrpc':'2.0','id':1,'method':'initialize','params':{}})
    assert initialized['result']['protocolVersion'] == '2025-03-26'
    status, _ = request({'jsonrpc':'2.0','method':'notifications/initialized'})
    assert status == 202
    _, listed = request({'jsonrpc':'2.0','id':2,'method':'tools/list','params':{}})
    assert [tool['name'] for tool in listed['result']['tools']] == ['sandbox-greet']
    _, called = request({'jsonrpc':'2.0','id':3,'method':'tools/call',
                         'params':{'name':'sandbox-greet','arguments':{'who':'sample-user'}}})
    assert called['result']['content'][0]['text'].startswith('Hello, sample-user!')
    print('MCP sample round trip passed')
finally:
    server.shutdown()
    server.server_close()
    worker.join(timeout=5)
`
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, python, "-c", probe).CombinedOutput()
	if err != nil || !strings.Contains(string(output), "MCP sample round trip passed") {
		t.Fatalf("sample MCP round trip failed: %v\n%s", err, output)
	}
}

func TestToolSandboxCalculatorPreservesRationalResults(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		python, err = exec.LookPath("python")
	}
	if err != nil {
		t.Skip("Python is required to execute the embedded tool sandbox")
	}

	server := toolSandboxServer(t)
	entrypoint := "print('tool-sandbox listening on :8080', flush=True)"
	server, _, found := strings.Cut(server, entrypoint)
	if !found {
		t.Fatalf("server.py entrypoint %q not found", entrypoint)
	}
	probe := server + "print(json.dumps(_calculator('(2+(3*3))/((2+(4*3)+5)/2)'), separators=(',', ':')))\n"
	output, err := exec.Command(python, "-c", probe).CombinedOutput()
	if err != nil {
		t.Fatalf("execute embedded calculator: %v\n%s", err, output)
	}
	if got, want := strings.TrimSpace(string(output)), `{"expression":"(2+(3*3))/((2+(4*3)+5)/2)","result":"22/19"}`; got != want {
		t.Fatalf("calculator result = %s, want %s", got, want)
	}
}

func toolSandboxServer(t *testing.T) string {
	t.Helper()
	objects := loadManifestObjects(t, "tool-sandbox.yaml")
	configMap := requireManifestObject(t, objects, "ConfigMap/tool-sandbox-src")
	server, found, err := unstructured.NestedString(configMap.Object, "data", "server.py")
	if err != nil || !found {
		t.Fatalf("ConfigMap data.server.py not found: found = %v, err = %v", found, err)
	}
	return server
}

func loadManifestObjects(t *testing.T, path string) map[string]unstructured.Unstructured {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer func() { _ = file.Close() }()

	objects := make(map[string]unstructured.Unstructured)
	decoder := yaml.NewYAMLOrJSONDecoder(file, 4096)
	for {
		var object unstructured.Unstructured
		err := decoder.Decode(&object)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("decode %s: %v", path, err)
		}
		if len(object.Object) == 0 {
			continue
		}
		objects[object.GetKind()+"/"+object.GetName()] = object
	}
	return objects
}

func requireManifestObject(t *testing.T, objects map[string]unstructured.Unstructured, key string) unstructured.Unstructured {
	t.Helper()
	object, found := objects[key]
	if !found {
		t.Fatalf("manifest object %s not found", key)
	}
	return object
}

func nestedStringsContain(object map[string]interface{}, expected string, fields ...string) bool {
	values, found, err := unstructured.NestedStringSlice(object, fields...)
	if err != nil || !found {
		return false
	}
	for _, value := range values {
		if value == expected {
			return true
		}
	}
	return false
}

func hasSubject(subjects []interface{}, kind, name string) bool {
	for _, rawSubject := range subjects {
		subject, ok := rawSubject.(map[string]interface{})
		if !ok {
			continue
		}
		if subject["kind"] == kind && subject["name"] == name {
			return true
		}
	}
	return false
}

func hasLocalObjectReference(references []interface{}, name string) bool {
	for _, rawReference := range references {
		reference, ok := rawReference.(map[string]interface{})
		if ok && reference["name"] == name {
			return true
		}
	}
	return false
}

func requireExactLocalObjectReference(t *testing.T, object unstructured.Unstructured, field, expected string) {
	t.Helper()
	references, found, err := unstructured.NestedSlice(object.Object, "spec", field)
	if err != nil || !found || len(references) != 1 || !hasLocalObjectReference(references, expected) {
		t.Fatalf("spec.%s = %#v, want exactly [{name: %q}], found = %v, err = %v", field, references, expected, found, err)
	}
}
