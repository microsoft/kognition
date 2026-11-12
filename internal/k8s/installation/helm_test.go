// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package installation

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func encodeHelmTestRecord(test *testing.T, value any) []byte {
	test.Helper()
	content, err := json.Marshal(value)
	if err != nil {
		test.Fatal(err)
	}
	var buffer bytes.Buffer
	writer := gzip.NewWriter(&buffer)
	if _, err := writer.Write(content); err != nil {
		test.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		test.Fatal(err)
	}
	return []byte(base64.StdEncoding.EncodeToString(buffer.Bytes()))
}

func TestHelmSettlementPreservesReviewedRecord(test *testing.T) {
	for _, scenario := range []string{"install", "upgrade", "rollback", "deployed", "failed", "wrong-name", "wrong-version", "wrong-namespace", "wrong-owner", "owner-reference", "finalizer", "payload-status", "duplicate-status", "payload-name", "manifest-drift", "bad-encoding"} {
		test.Run(scenario, func(test *testing.T) {
			manifest := "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: settings, namespace: control}\n"
			path := filepath.Join(test.TempDir(), "manifest.yaml")
			if err := os.WriteFile(path, []byte(manifest), 0600); err != nil {
				test.Fatal(err)
			}
			plan := Plan{Release: "release", Manifest: Artifact{Path: path, SHA256: fmt.Sprintf("%x", sha256.Sum256([]byte(manifest)))}}
			status, revision := "pending-install", 1
			if scenario == "upgrade" {
				plan.ReleaseRevision, status, revision = 1, "pending-upgrade", 2
			}
			switch scenario {
			case "rollback":
				status = "pending-rollback"
			case "deployed", "failed":
				status = scenario
			}
			payload := map[string]any{"name": "release", "namespace": "control", "version": revision, "manifest": manifest, "info": map[string]any{"status": status}, "config": map[string]any{"activated": true}, "chart": map[string]any{"metadata": map[string]any{"name": "fixture", "version": "0.1.0"}}}
			secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("sh.helm.release.v1.release.v%d", revision), Namespace: "control", UID: "record", ResourceVersion: "1", Labels: map[string]string{"owner": "helm", "name": "release", "version": fmt.Sprint(revision), "status": status}}, Type: "helm.sh/release.v1"}
			switch scenario {
			case "wrong-name":
				secret.Name = "other"
			case "wrong-version":
				secret.Labels["version"] = "9"
			case "wrong-namespace":
				secret.Namespace = "other"
			case "wrong-owner":
				secret.Labels["owner"] = "other"
			case "owner-reference":
				secret.OwnerReferences = []metav1.OwnerReference{{Name: "other", UID: "other"}}
			case "finalizer":
				secret.Finalizers = []string{"example.invalid/hold"}
			case "payload-status":
				payload["info"] = map[string]any{"status": "deployed"}
			case "duplicate-status":
				payload["info"] = json.RawMessage(`{"status":"deployed","status":"pending-install"}`)
			case "payload-name":
				payload["name"] = "other"
			case "manifest-drift":
				payload["manifest"] = ""
			}
			secret.Data = map[string][]byte{"release": encodeHelmTestRecord(test, payload)}
			if scenario == "bad-encoding" {
				secret.Data["release"] = []byte("invalid")
			}
			before := secret.DeepCopy()
			next, err := failedHelmRecord(secret, plan, "control")
			if (err == nil) != (scenario == "install" || scenario == "upgrade") {
				test.Fatalf("unexpected settlement result: %v", err)
			}
			if !reflect.DeepEqual(secret, before) {
				test.Fatal("input record mutated")
			}
			if err != nil {
				return
			}
			compressed, err := base64.StdEncoding.DecodeString(string(next.Data["release"]))
			if err != nil {
				test.Fatal(err)
			}
			reader, err := gzip.NewReader(bytes.NewReader(compressed))
			if err != nil {
				test.Fatal(err)
			}
			defer reader.Close()
			content, err := io.ReadAll(reader)
			if err != nil {
				test.Fatal(err)
			}
			var actual map[string]any
			if err := json.Unmarshal(content, &actual); err != nil {
				test.Fatal(err)
			}
			info := actual["info"].(map[string]any)
			if next.Labels["status"] != "failed" || info["status"] != "failed" || info["description"] == nil {
				test.Fatal("failed status or settlement description missing")
			}
			delete(actual, "info")
			delete(payload, "info")
			actualJSON, _ := json.Marshal(actual)
			expectedJSON, _ := json.Marshal(payload)
			if !bytes.Equal(actualJSON, expectedJSON) || next.UID != secret.UID || next.ResourceVersion != secret.ResourceVersion {
				test.Fatal("settlement changed release content or identity")
			}
		})
	}
}
