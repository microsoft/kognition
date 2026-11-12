// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package installation

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestCanonicalManifest(test *testing.T) {
	first, err := CanonicalManifest([]byte("apiVersion: v1\nkind: ConfigMap\nmetadata: {name: first}\n---\napiVersion: v1\nkind: ConfigMap\nmetadata: {name: second}\n"))
	if err != nil {
		test.Fatal(err)
	}
	second, err := CanonicalManifest([]byte("{\"metadata\":{\"name\":\"second\"},\"kind\":\"ConfigMap\",\"apiVersion\":\"v1\"}\n---\n{\"metadata\":{\"name\":\"first\"},\"kind\":\"ConfigMap\",\"apiVersion\":\"v1\"}"))
	if err != nil || !bytes.Equal(first, second) {
		test.Fatalf("semantic parity failed: %v", err)
	}
	for _, content := range []string{
		"apiVersion: v1\nkind: ConfigMap\nmetadata: {name: first}\n---\napiVersion: v1\nkind: ConfigMap\nmetadata: {name: first}",
		"apiVersion: v1\nkind: ConfigMap\nmetadata: {generateName: first}",
		"apiVersion: v1\nkind: ConfigMap\nmetadata: {name: first, annotations: {helm.sh/hook: pre-upgrade}}",
		"apiVersion: v1\nkind: ConfigMap\nmetadata: {name: first, name: second}",
		`{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"first","name":"second"}}`,
		"apiVersion: apps/v1\nkind: Deployment\nmetadata: {name: first}\n---\napiVersion: apps/v1beta1\nkind: Deployment\nmetadata: {name: first}",
	} {
		if _, err := CanonicalManifest([]byte(content)); err == nil {
			test.Fatal("unsafe render accepted")
		}
	}
}

func TestObjectDigestTracksOwnershipAndSpec(test *testing.T) {
	object := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]any{"name": "test", "uid": "original"}, "data": map[string]any{"value": "original"}}}
	original, err := ObjectDigest(object)
	if err != nil {
		test.Fatal(err)
	}
	object.SetResourceVersion("2")
	object.Object["status"] = map[string]any{"ready": true}
	stable, err := ObjectDigest(object)
	if err != nil || original != stable {
		test.Fatal("volatile fields changed digest")
	}
	object.SetUID("replacement")
	changed, err := ObjectDigest(object)
	if err != nil || original == changed {
		test.Fatal("replacement did not change digest")
	}
	object.SetUID("original")
	object.Object["data"] = map[string]any{"value": "changed"}
	changed, err = ObjectDigest(object)
	if err != nil || original == changed {
		test.Fatal("spec change did not change digest")
	}
}

func TestReadPlanRejectsAmbiguousOrIncompleteReview(test *testing.T) {
	artifact := Artifact{Path: "reviewed.tgz", SHA256: strings.Repeat("a", 64)}
	plan := Plan{Version: 1, Kubeconfig: "target.yaml", Endpoint: "https://127.0.0.1:12345", ClusterUID: "cluster", NamespaceUID: "control", AuthorityNamespace: "authority", AuthorityNamespaceUID: "authority-uid", Release: "kognition", Chart: artifact, Values: artifact, Manifest: artifact, Controllers: []Controller{}, Grants: []ObjectRef{}, LegacyPolicies: []ObjectRef{}, WorkloadNamespaceUIDs: map[string]string{"tenant": "tenant-uid"}}
	valid, err := json.Marshal(plan)
	if err != nil {
		test.Fatal(err)
	}
	for _, scenario := range []struct {
		name      string
		content   string
		wantError bool
	}{
		{"valid", string(valid), false},
		{"duplicate", strings.Replace(string(valid), `"version":1`, `"version":1,"version":1`, 1), true},
		{"omitted-revision", strings.Replace(string(valid), `"releaseRevision":0,`, "", 1), true},
		{"null-inventory", strings.Replace(string(valid), `"grants":[]`, `"grants":null`, 1), true},
		{"plaintext", strings.Replace(string(valid), "https://", "http://", 1), true},
		{"upgrade-without-history", strings.Replace(string(valid), `"releaseRevision":0`, `"releaseRevision":1`, 1), true},
		{"case-mismatch", strings.Replace(string(valid), `"version":1`, `"Version":1`, 1), true},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			path := filepath.Join(test.TempDir(), "plan.json")
			if err := os.WriteFile(path, []byte(scenario.content), 0600); err != nil {
				test.Fatal(err)
			}
			parsed, err := ReadPlan(path)
			if (err != nil) != scenario.wantError {
				test.Fatalf("plan result %v", err)
			}
			if err == nil && !filepath.IsAbs(parsed.Chart.Path) {
				test.Fatal("relative artifact path unresolved")
			}
			if !scenario.wantError {
				if err := os.WriteFile(path, []byte("changed after read"), 0600); err != nil {
					test.Fatal(err)
				}
				captured, err := ParsePlan([]byte(scenario.content), path)
				if err != nil || captured.Release != plan.Release || captured.Chart.Path != parsed.Chart.Path {
					test.Fatalf("captured plan was reread: %v", err)
				}
			}
		})
	}
}

func TestTransitionRejectsUnreviewedHelmDeletion(test *testing.T) {
	previous := []byte("apiVersion: v1\nkind: Secret\nmetadata: {name: unrelated, namespace: control}\n")
	if err := ValidateTransition(previous, nil, Plan{}); err == nil {
		test.Fatal("unreviewed deletion accepted")
	}
	previous = []byte("apiVersion: rbac.authorization.k8s.io/v1\nkind: ClusterRoleBinding\nmetadata: {name: old}\n")
	plan := Plan{Grants: []ObjectRef{{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "ClusterRoleBinding", Name: "old"}}}
	if err := ValidateTransition(previous, nil, plan); err != nil {
		test.Fatal(err)
	}
	if err := ValidateTransition(previous, previous, Plan{}); err != nil {
		test.Fatal(err)
	}
	unrelated := bytes.ReplaceAll(previous, []byte("rbac.authorization.k8s.io"), []byte("unrelated.example"))
	if err := ValidateTransition(previous, unrelated, Plan{}); err == nil {
		test.Fatal("different API group incorrectly retained old resource")
	}
	if err := ValidateTransition(unrelated, nil, plan); err == nil {
		test.Fatal("cleanup inventory authorized another API group")
	}
}

func TestPlanRequiresCompleteCleanupInventory(test *testing.T) {
	for _, scenario := range []string{"valid", "rbac-colon-name", "slash-name", "percent-name", "dot-name", "null-retained", "null-attestations", "missing-uid", "invalid-digest", "invalid-version", "missing-name", "duplicate-object", "duplicate-uid", "unknown-resource", "retained-attestation", "unknown-controller", "duplicate-attestation", "empty-evidence", "missing-invocation"} {
		test.Run(scenario, func(test *testing.T) {
			artifact := Artifact{Path: "reviewed.tgz", SHA256: strings.Repeat("a", 64)}
			controller := ObjectRef{APIVersion: "apps/v1", Kind: "Deployment", Namespace: "control", Name: "controller", UID: "controller", SHA256: strings.Repeat("b", 64)}
			policy := ObjectRef{APIVersion: "networking.k8s.io/v1", Kind: "NetworkPolicy", Namespace: "tenant", Name: "orphan", UID: "policy", SHA256: strings.Repeat("c", 64)}
			retained := ObjectRef{APIVersion: "v1", Kind: "ConfigMap", Namespace: "control", Name: "settings", UID: "settings", SHA256: strings.Repeat("d", 64)}
			plan := Plan{Version: 1, Kubeconfig: "target.yaml", Endpoint: "https://127.0.0.1:12345", ClusterUID: "cluster", NamespaceUID: "control", AuthorityNamespace: "authority", AuthorityNamespaceUID: "authority-uid", Release: "kognition", ReleaseRevision: 1, Chart: artifact, Values: artifact, Manifest: artifact, PreviousManifest: &artifact, Controllers: []Controller{{Deployment: controller, ServiceAccount: "controller"}}, Grants: []ObjectRef{}, LegacyPolicies: []ObjectRef{policy}, RetainedObjects: []ObjectRef{retained}, CleanupAttestations: []CleanupAttestation{{ResourceUID: "policy", ControllerUID: "controller", InvocationID: "invocation", Evidence: "reviewed audit"}}, WorkloadNamespaceUIDs: map[string]string{"tenant": "tenant-uid"}}
			switch scenario {
			case "rbac-colon-name":
				plan.Grants = []ObjectRef{{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "ClusterRoleBinding", Name: "system:controller:example", UID: "grant", SHA256: strings.Repeat("e", 64)}}
				plan.RetainedObjects[0].APIVersion = "rbac.authorization.k8s.io/v1"
				plan.RetainedObjects[0].Kind = "ClusterRole"
				plan.RetainedObjects[0].Namespace = ""
				plan.RetainedObjects[0].Name = "system:controller:example"
			case "slash-name":
				plan.RetainedObjects[0].Name = "settings/other"
			case "percent-name":
				plan.RetainedObjects[0].Name = "settings%2Fother"
			case "dot-name":
				plan.RetainedObjects[0].Name = ".."
			case "missing-uid":
				plan.RetainedObjects[0].UID = ""
			case "invalid-digest":
				plan.RetainedObjects[0].SHA256 = strings.Repeat("z", 64)
			case "invalid-version":
				plan.RetainedObjects[0].APIVersion = ""
			case "missing-name":
				plan.RetainedObjects[0].Name = ""
			case "duplicate-object":
				plan.RetainedObjects = append(plan.RetainedObjects, retained)
			case "duplicate-uid":
				plan.RetainedObjects[0].UID = controller.UID
			case "unknown-resource":
				plan.CleanupAttestations[0].ResourceUID = "other"
			case "retained-attestation":
				plan.CleanupAttestations[0].ResourceUID = retained.UID
			case "unknown-controller":
				plan.CleanupAttestations[0].ControllerUID = "other"
			case "duplicate-attestation":
				plan.CleanupAttestations = append(plan.CleanupAttestations, plan.CleanupAttestations[0])
			case "empty-evidence":
				plan.CleanupAttestations[0].Evidence = " "
			case "missing-invocation":
				plan.CleanupAttestations[0].InvocationID = ""
			}
			content, err := json.Marshal(plan)
			if err != nil {
				test.Fatal(err)
			}
			if strings.HasPrefix(scenario, "null-") {
				var fields map[string]json.RawMessage
				if err := json.Unmarshal(content, &fields); err != nil {
					test.Fatal(err)
				}
				field := "retainedObjects"
				if scenario == "null-attestations" {
					field = "cleanupAttestations"
				}
				fields[field] = json.RawMessage("null")
				content, err = json.Marshal(fields)
				if err != nil {
					test.Fatal(err)
				}
			}
			_, err = ParsePlan(content, filepath.Join(test.TempDir(), "plan.json"))
			if (err == nil) != (scenario == "valid" || scenario == "rbac-colon-name") {
				test.Fatalf("plan validation: %v", err)
			}
		})
	}
}

func TestFailedReleasePlanReview(test *testing.T) {
	for _, scenario := range []string{"failed-install", "failed-upgrade", "absent", "without-failed-record", "fresh-with-failed-record", "negative-deployed", "same-revision", "future-revision", "missing-deployed-manifest", "missing-deployed-revision", "duplicate-absence", "live-and-absent", "null-failedRelease", "null-previousDeployedRevision", "null-previousDeployedManifest", "null-absentObjects"} {
		test.Run(scenario, func(test *testing.T) {
			artifact := Artifact{Path: "reviewed.tgz", SHA256: strings.Repeat("a", 64)}
			record := ObjectRef{APIVersion: "v1", Kind: "Secret", Namespace: "control", Name: "sh.helm.release.v1.kognition.v2", UID: "record", SHA256: strings.Repeat("b", 64)}
			absent := ObjectIdentity{APIVersion: "v1", Kind: "ConfigMap", Namespace: "control", Name: "settings"}
			plan := Plan{Version: 1, Kubeconfig: "target.yaml", Endpoint: "https://127.0.0.1:12345", ClusterUID: "cluster", NamespaceUID: "control", AuthorityNamespace: "authority", AuthorityNamespaceUID: "authority-uid", Release: "kognition", ReleaseRevision: 2, Chart: artifact, Values: artifact, Manifest: artifact, PreviousManifest: &artifact, FailedRelease: &record, Controllers: []Controller{}, Grants: []ObjectRef{}, LegacyPolicies: []ObjectRef{}, WorkloadNamespaceUIDs: map[string]string{"tenant": "tenant-uid"}}
			switch scenario {
			case "failed-upgrade":
				plan.PreviousDeployedRevision, plan.PreviousDeployedManifest = 1, &artifact
			case "absent":
				plan.AbsentObjects = []ObjectIdentity{absent}
			case "without-failed-record":
				plan.FailedRelease, plan.AbsentObjects = nil, []ObjectIdentity{absent}
			case "fresh-with-failed-record":
				plan.ReleaseRevision, plan.PreviousManifest = 0, nil
			case "negative-deployed":
				plan.PreviousDeployedRevision = -1
			case "same-revision":
				plan.PreviousDeployedRevision, plan.PreviousDeployedManifest = 2, &artifact
			case "future-revision":
				plan.PreviousDeployedRevision, plan.PreviousDeployedManifest = 3, &artifact
			case "missing-deployed-manifest":
				plan.PreviousDeployedRevision = 1
			case "missing-deployed-revision":
				plan.PreviousDeployedManifest = &artifact
			case "duplicate-absence":
				plan.AbsentObjects = []ObjectIdentity{absent, absent}
			case "live-and-absent":
				plan.AbsentObjects = []ObjectIdentity{{APIVersion: record.APIVersion, Kind: record.Kind, Namespace: record.Namespace, Name: record.Name}}
			}
			content, err := json.Marshal(plan)
			if err != nil {
				test.Fatal(err)
			}
			if field, found := strings.CutPrefix(scenario, "null-"); found {
				var fields map[string]json.RawMessage
				if err := json.Unmarshal(content, &fields); err != nil {
					test.Fatal(err)
				}
				fields[field] = json.RawMessage("null")
				content, err = json.Marshal(fields)
				if err != nil {
					test.Fatal(err)
				}
			}
			parsed, err := ParsePlan(content, filepath.Join(test.TempDir(), "plan.json"))
			wantValid := scenario == "failed-install" || scenario == "failed-upgrade" || scenario == "absent"
			if (err == nil) != wantValid {
				test.Fatalf("retry plan validation: %v", err)
			}
			if err == nil && parsed.PreviousDeployedManifest != nil && !filepath.IsAbs(parsed.PreviousDeployedManifest.Path) {
				test.Fatal("deployed manifest path was not resolved")
			}
		})
	}
}
