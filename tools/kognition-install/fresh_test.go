// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/kognition-ai/kognition/internal/k8s/installation"
)

func TestFreshOptionsRejectAmbiguousAndExistingInstallInputs(t *testing.T) {
	base := freshOptions{kubeconfig: "target", chart: "chart.tgz", values: "values.yaml",
		release: "demo", namespace: "control", authority: "authority", workloads: "team-a", state: "state", replicas: 1, exclusive: true}
	if err := base.validate(); err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*freshOptions){
		func(value *freshOptions) { value.kubeconfig = "" },
		func(value *freshOptions) { value.chart = "chart-directory" },
		func(value *freshOptions) { value.exclusive = false },
		func(value *freshOptions) { value.namespace = "default" },
		func(value *freshOptions) { value.authority = value.namespace },
		func(value *freshOptions) { value.workloads = "team-a,team-a" },
		func(value *freshOptions) { value.workloads = "kube-system" },
		func(value *freshOptions) { value.workloads = "" },
		func(value *freshOptions) { value.replicas = 0 },
	} {
		value := base
		change(&value)
		if err := value.validate(); err == nil {
			t.Fatalf("invalid options accepted: %+v", value)
		}
	}
	var stdout, stderr bytes.Buffer
	if code := runFresh([]string{"--help"}, &stdout, &stderr); code != 0 || !strings.Contains(stderr.String(), "workload-namespaces") {
		t.Fatalf("fresh help failed: %d %s", code, stderr.String())
	}
	if code := runFresh(nil, &stdout, &stderr); code != 2 {
		t.Fatal("missing explicit target accepted")
	}
	if code := runFreshRecovery([]string{"--help"}, &stdout, &stderr); code != 0 {
		t.Fatal("fresh recovery help failed")
	}
	if code := runFreshRecovery([]string{"--plan", "retained.json", "--record", "authorization"}, &stdout, &stderr); code != 2 {
		t.Fatal("fresh recovery accepted missing writer/executor confirmation")
	}
}

func TestFreshValuesStartStoppedAndRetainIndependentEnforcement(t *testing.T) {
	options := freshOptions{workloads: "team-a", replicas: 2}
	base := "k8sController:\n  image:\n    digest: sha256:" + strings.Repeat("a", 64) + "\n"
	content, err := freshValues([]byte(base+"networkPolicy:\n  kubeletProbeIngressCIDRs: [10.0.0.0/24]\n"), options)
	if err != nil {
		t.Fatal(err)
	}
	var values map[string]interface{}
	if err := json.Unmarshal(content, &values); err != nil {
		t.Fatal(err)
	}
	controller := values["k8sController"].(map[string]interface{})
	if controller["replicas"] != float64(0) || controller["replicaCount"] != float64(2) ||
		!reflect.DeepEqual(controller["managedNamespaces"], []interface{}{"team-a"}) {
		t.Fatalf("unexpected startup state: %+v", controller)
	}
	if values["admissionGuard"].(map[string]interface{})["stdioIsolationProfile"] != "cilium" ||
		values["networkPolicy"].(map[string]interface{})["enabled"] != true ||
		values["migrations"].(map[string]interface{})["retainCompletedJobs"] != true {
		t.Fatal("fresh values lost security or migration prerequisites")
	}
	password := values["postgres"].(map[string]interface{})["auth"].(map[string]interface{})["password"].(string)
	if len(password) < 40 || strings.Contains(string(content), "SG-07") {
		t.Fatal("fresh values use a weak default password or synthetic acceptance report")
	}
	for _, invalid := range []string{
		"null", "",
		"mode: standalone", "networkPolicy:\n  enabled: false",
		"admissionGuard:\n  stdioIsolationProfile: network-policy", "k8sController: invalid",
	} {
		input := invalid
		if invalid != "" && invalid != "null" && !strings.HasPrefix(invalid, "k8sController:") {
			input = base + invalid
		}
		if _, err := freshValues([]byte(input), options); err == nil {
			t.Fatalf("invalid fresh profile accepted: %s", invalid)
		}
	}
	artifact := installation.Artifact{Path: filepath.Join(t.TempDir(), "values.json"), SHA256: fmt.Sprintf("%x", sha256.Sum256(content))}
	if err := os.WriteFile(artifact.Path, content, 0600); err != nil {
		t.Fatal(err)
	}
	if replicas, err := freshReplicaCount(artifact); err != nil || replicas != 2 {
		t.Fatalf("configured replica count lost: %d %v", replicas, err)
	}
	if err := os.WriteFile(artifact.Path, []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := freshReplicaCount(artifact); err == nil {
		t.Fatal("changed private values accepted")
	}
}
