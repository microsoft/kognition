// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cmd

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
)

func TestRunApprove_SubmitsDecisionAndReason(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if r.URL.Path != "/apis/kognition.dev/v1alpha1/approvals/approval-7/decision" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("content type = %q", got)
		}
		var payload map[string]string
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		if payload["decision"] != "approve" || payload["reason"] != "reviewed" {
			t.Errorf("payload = %#v", payload)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	restore := configureGovernanceTest(server)
	defer restore()
	approvalDecision = "approve"
	approvalReason = "reviewed"

	output := captureStdout(t, func() error { return runApprove(nil, []string{"approval-7"}) })
	if !strings.Contains(output, `approval approval-7: decision "approve" submitted`) {
		t.Fatalf("output = %q", output)
	}
}

func TestRunApprove_RejectsInvalidDecisionWithoutRequest(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("invalid decision reached API server")
	}))
	defer server.Close()

	restore := configureGovernanceTest(server)
	defer restore()
	approvalDecision = "later"

	err := runApprove(nil, []string{"approval-7"})
	if err == nil || !strings.Contains(err.Error(), "decision must be") {
		t.Fatalf("error = %v", err)
	}
}

func TestRunApprove_PropagatesServerError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "approval expired", http.StatusConflict)
	}))
	defer server.Close()

	restore := configureGovernanceTest(server)
	defer restore()
	approvalDecision = "deny"

	err := runApprove(nil, []string{"approval-7"})
	if err == nil || !strings.Contains(err.Error(), "409") || !strings.Contains(err.Error(), "approval expired") {
		t.Fatalf("error = %v, want HTTP 409 response", err)
	}
}

func TestRunRevisions_RendersRevisionTable(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/apis/kognition.dev/v1alpha1/namespaces/team-a/agents/planner/revisions" {
			t.Errorf("path = %q", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"items":[{"metadata":{"generation":3,"resourceVersion":"27","created_at":"2026-08-02T10:00:00Z"}}]}`))
	}))
	defer server.Close()

	restore := configureGovernanceTest(server)
	defer restore()

	output := captureStdout(t, func() error { return runRevisions(nil, []string{"agent", "planner"}) })
	for _, want := range []string{"REVISION", "RESOURCE VERSION", "TIMESTAMP", "3", "27", "2026-08-02T10:00:00Z"} {
		if !strings.Contains(output, want) {
			t.Errorf("output missing %q:\n%s", want, output)
		}
	}
}

func TestRunGetRevision_UsesNamedRevisionPath(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/apis/kognition.dev/v1alpha1/namespaces/team-a/agents/planner/revisions/3" {
			t.Errorf("path = %q", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"metadata":{"generation":3}}`))
	}))
	defer server.Close()

	restore := configureGovernanceTest(server)
	defer restore()
	outputFormat = "json"

	output := captureStdout(t, func() error {
		return runGetRevision(nil, []string{"agent", "planner", "3"})
	})
	if !strings.Contains(output, `"generation": 3`) {
		t.Fatalf("output = %s", output)
	}
}

func TestRunDiffRevisions_UsesDiffPathAndQuery(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/apis/kognition.dev/v1alpha1/namespaces/team-a/agents/planner/revisions/diff" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if r.URL.Query().Get("from") != "2" || r.URL.Query().Get("to") != "3" {
			t.Errorf("query = %q", r.URL.RawQuery)
		}
		_, _ = w.Write([]byte(`{"kind":"RevisionDiff","fromRevision":2,"toRevision":3,"patch":[{"op":"replace","path":"/model","value":"claude-4"}]}`))
	}))
	defer server.Close()

	restore := configureGovernanceTest(server)
	defer restore()
	outputFormat = "json"

	output := captureStdout(t, func() error {
		return runDiffRevisions(nil, []string{"agent", "planner", "2", "3"})
	})
	if !strings.Contains(output, `"fromRevision": 2`) || !strings.Contains(output, `"path": "/model"`) {
		t.Fatalf("output = %s", output)
	}
}

func TestRevisionCommandsUseImmutableConfigMapsInKubernetesMode(t *testing.T) {
	const (
		targetNamespace = "team-a"
		name            = "planner"
		kind            = "Agent"
		ownerUID        = "agent-uid"
	)
	parent := revisionParentKey(kind, name)
	immutable := true
	configMaps := []*corev1.ConfigMap{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      revisionConfigMapName(kind, name, 2),
				Namespace: targetNamespace,
				Labels: map[string]string{
					"kognition.dev/revision-parent": parent,
					"kognition.dev/revision-kind":   "agent",
				},
				Annotations: map[string]string{
					"kognition.dev/revision":   "2",
					"kognition.dev/created-at": "2026-08-15T12:00:00Z",
				},
				OwnerReferences: []metav1.OwnerReference{{APIVersion: "kognition.dev/v1alpha1", Kind: kind, Name: name, UID: ownerUID}},
			},
			Immutable:  &immutable,
			BinaryData: map[string][]byte{"spec": []byte(`{"model":"gpt-4"}`), "data": []byte(`{"kind":"Agent"}`)},
		},
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      revisionConfigMapName(kind, name, 3),
				Namespace: targetNamespace,
				Labels: map[string]string{
					"kognition.dev/revision-parent": parent,
					"kognition.dev/revision-kind":   "agent",
				},
				Annotations: map[string]string{
					"kognition.dev/revision":   "3",
					"kognition.dev/created-at": "2026-08-15T12:01:00Z",
				},
				OwnerReferences: []metav1.OwnerReference{{APIVersion: "kognition.dev/v1alpha1", Kind: kind, Name: name, UID: ownerUID}},
			},
			Immutable:  &immutable,
			BinaryData: map[string][]byte{"spec": []byte(`{"model":"claude-4"}`), "data": []byte(`{"kind":"Agent"}`)},
		},
	}
	objects := make([]runtime.Object, len(configMaps))
	for index := range configMaps {
		objects[index] = configMaps[index]
	}
	parentObject := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "kognition.dev/v1alpha1",
		"kind":       kind,
		"metadata":   map[string]any{"name": name, "namespace": targetNamespace, "uid": ownerUID},
	}}
	previousMode, previousNamespace, previousFormat := k8sMode, namespace, outputFormat
	previousCore, previousDynamic := k8sCoreClient, k8sDynClient
	k8sMode, namespace, outputFormat = true, targetNamespace, "json"
	k8sCoreClient = fake.NewSimpleClientset(objects...)
	k8sDynClient = dynamicfake.NewSimpleDynamicClient(runtime.NewScheme(), parentObject)
	t.Cleanup(func() {
		k8sMode, namespace, outputFormat = previousMode, previousNamespace, previousFormat
		k8sCoreClient, k8sDynClient = previousCore, previousDynamic
	})

	listOutput := captureStdout(t, func() error { return runRevisions(nil, []string{"agent", name}) })
	if !strings.Contains(listOutput, `"revision": 2`) || !strings.Contains(listOutput, `"revision": 3`) {
		t.Fatalf("revision list output = %s", listOutput)
	}
	getOutput := captureStdout(t, func() error { return runGetRevision(nil, []string{"agent", name, "2"}) })
	if !strings.Contains(getOutput, `"kind": "Revision"`) || !strings.Contains(getOutput, `"model": "gpt-4"`) {
		t.Fatalf("revision get output = %s", getOutput)
	}
	diffOutput := captureStdout(t, func() error { return runDiffRevisions(nil, []string{"agent", name, "2", "3"}) })
	if !strings.Contains(diffOutput, `"kind": "RevisionDiff"`) || !strings.Contains(diffOutput, `"path": "/model"`) {
		t.Fatalf("revision diff output = %s", diffOutput)
	}
}

func TestK8sRevisionSnapshotRejectsForgedConfigMap(t *testing.T) {
	const (
		namespace = "team-a"
		name      = "planner"
		kind      = "Agent"
	)
	immutable := true
	forged := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      revisionConfigMapName(kind, name, 2),
			Namespace: namespace,
			Labels: map[string]string{
				"kognition.dev/revision-parent": revisionParentKey(kind, name),
				"kognition.dev/revision-kind":   "agent",
			},
			Annotations: map[string]string{"kognition.dev/revision": "2"},
		},
		Immutable:  &immutable,
		BinaryData: map[string][]byte{"spec": []byte(`{"model":"forged"}`)},
	}
	parentObject := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "kognition.dev/v1alpha1",
		"kind":       kind,
		"metadata":   map[string]any{"name": name, "namespace": namespace, "uid": "agent-uid"},
	}}
	previousCore, previousDynamic := k8sCoreClient, k8sDynClient
	k8sCoreClient = fake.NewSimpleClientset(forged)
	k8sDynClient = dynamicfake.NewSimpleDynamicClient(runtime.NewScheme(), parentObject)
	t.Cleanup(func() { k8sCoreClient, k8sDynClient = previousCore, previousDynamic })

	if _, _, err := k8sRevisionSnapshot(namespace, "agent", name, "2"); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("k8sRevisionSnapshot() error = %v, want forged ConfigMap rejection", err)
	}
}

func TestRevisionCommandsUseLegacyParentAnnotationsInKubernetesMode(t *testing.T) {
	const (
		targetNamespace = "team-a"
		name            = "legacy-agent"
		kind            = "Agent"
		ownerUID        = "agent-uid"
	)
	immutable := true
	configMap := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      revisionConfigMapName(kind, name, 5),
			Namespace: targetNamespace,
			Labels: map[string]string{
				"kognition.dev/revision-parent": revisionParentKey(kind, name),
				"kognition.dev/revision-kind":   "agent",
			},
			Annotations: map[string]string{"kognition.dev/revision": "5"},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "kognition.dev/v1alpha1", Kind: kind, Name: name, UID: ownerUID,
			}},
		},
		Immutable:  &immutable,
		BinaryData: map[string][]byte{"spec": []byte(`{"model":"claude-4"}`)},
	}
	parentObject := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "kognition.dev/v1alpha1",
		"kind":       kind,
		"metadata": map[string]any{
			"name":      name,
			"namespace": targetNamespace,
			"uid":       ownerUID,
			"annotations": map[string]any{
				"kognition.dev/revision-count": "4",
				"kognition.dev/revision-spec":  "eyJtb2RlbCI6ImdwdC00In0=",
				"kognition.dev/revision-data":  "eyJraW5kIjoiQWdlbnQifQ==",
			},
		},
	}}
	previousMode, previousNamespace, previousFormat := k8sMode, namespace, outputFormat
	previousCore, previousDynamic := k8sCoreClient, k8sDynClient
	k8sMode, namespace, outputFormat = true, targetNamespace, "json"
	k8sCoreClient = fake.NewSimpleClientset(configMap)
	k8sDynClient = dynamicfake.NewSimpleDynamicClient(runtime.NewScheme(), parentObject)
	t.Cleanup(func() {
		k8sMode, namespace, outputFormat = previousMode, previousNamespace, previousFormat
		k8sCoreClient, k8sDynClient = previousCore, previousDynamic
	})

	listOutput := captureStdout(t, func() error { return runRevisions(nil, []string{"agent", name}) })
	if !strings.Contains(listOutput, `"revision": 4`) || !strings.Contains(listOutput, `"revision": 5`) {
		t.Fatalf("legacy revision list output = %s", listOutput)
	}
	getOutput := captureStdout(t, func() error { return runGetRevision(nil, []string{"agent", name, "4"}) })
	if !strings.Contains(getOutput, `"model": "gpt-4"`) {
		t.Fatalf("legacy revision get output = %s", getOutput)
	}
}

func TestSanitizeRevisionSnapshotDataRemovesInternalSecretData(t *testing.T) {
	input := []byte(`{"metadata":{"annotations":{"kognition.dev/internal-secret-data":"secret","public":"value"}}}`)
	sanitized := sanitizeRevisionSnapshotData("SecretRef", input)
	if strings.Contains(string(sanitized), "internal-secret-data") || !strings.Contains(string(sanitized), `"public":"value"`) {
		t.Fatalf("sanitized SecretRef snapshot = %s", sanitized)
	}
	if got := sanitizeRevisionSnapshotData("Agent", input); string(got) != string(input) {
		t.Fatalf("non-SecretRef snapshot = %s, want unchanged", got)
	}
}

func TestRunRTBF_SubmitsRequestAndRendersResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if r.URL.Path != "/apis/kognition.dev/v1alpha1/namespaces/team-a/memoryrecords/memory-9/rtbf" {
			t.Errorf("path = %q", r.URL.Path)
		}
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		if len(payload) != 0 {
			t.Errorf("payload = %#v, want empty object", payload)
		}
		_, _ = w.Write([]byte(`{"status":"accepted"}`))
	}))
	defer server.Close()

	restore := configureGovernanceTest(server)
	defer restore()
	outputFormat = "json"

	output := captureStdout(t, func() error { return runRTBF(nil, []string{"memory-9"}) })
	if !strings.Contains(output, `"status": "accepted"`) {
		t.Fatalf("output = %s", output)
	}
}

func TestRunRTBF_PropagatesServerError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "record locked", http.StatusLocked)
	}))
	defer server.Close()

	restore := configureGovernanceTest(server)
	defer restore()

	err := runRTBF(nil, []string{"memory-9"})
	if err == nil || !strings.Contains(err.Error(), "423") || !strings.Contains(err.Error(), "record locked") {
		t.Fatalf("error = %v, want HTTP 423 response", err)
	}
}

func configureGovernanceTest(server *httptest.Server) func() {
	restore := configureGetTest(server)
	oldDecision, oldReason := approvalDecision, approvalReason
	approvalDecision, approvalReason = "", ""
	return func() {
		approvalDecision, approvalReason = oldDecision, oldReason
		restore()
	}
}
