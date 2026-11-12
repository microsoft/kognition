// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"bytes"
	"reflect"
	"strings"
	"testing"
)

func TestHelmEnvironmentCannotOverrideTarget(test *testing.T) {
	actual := helmEnvironment([]string{"PATH=tools", "HELM_KUBEAPISERVER=https://wrong", "helm_kubecontext=wrong", "HELM_KUBEINSECURE_SKIP_TLS_VERIFY=true", "HELM_KUBETOKEN=wrong", "HELM_DRIVER=sql", "KOGNITION_INSTALL_PLAN=wrong", "KOGNITION_INSTALL_PLAN_SHA256=wrong"}, "captured", "digest")
	expected := []string{"PATH=tools", "HELM_DRIVER=secret", "KOGNITION_INSTALL_PLAN=captured", "KOGNITION_INSTALL_PLAN_SHA256=digest"}
	if !reflect.DeepEqual(actual, expected) {
		test.Fatalf("unsafe Helm environment: %v", actual)
	}
}

func TestInvalidInvocationHasNoEffects(test *testing.T) {
	for _, arguments := range [][]string{
		nil, {"activate"}, {"bootstrap"}, {"bootstrap", "--plan", "absent"},
		{"recover", "--plan", "absent", "--writers-frozen"},
		{"recover", "--plan", "absent", "--writers-frozen", "--requests-settled"},
		{"recover", "--plan", "absent", "--writers-frozen", "--requests-settled", "--executors-stopped"},
		{"bootstrap", "--plan", "absent", "--writers-frozen", "--claim-uid", "claim"},
		{"bootstrap", "--plan", "absent", "--writers-frozen", "--requests-settled"},
		{"bootstrap", "--plan", "absent", "--writers-frozen", "--atomic"},
		{"bootstrap", "--plan", "absent", "--writers-frozen", "--reuse-values"},
		{"bootstrap", "--plan", "absent", "--writers-frozen", "extra"},
	} {
		var stdout, stderr bytes.Buffer
		if code := run(arguments, bytes.NewReader(nil), &stdout, &stderr); code != 2 || stdout.Len() != 0 {
			test.Fatalf("%v: exit=%d stdout=%s stderr=%s", arguments, code, stdout.String(), stderr.String())
		}
	}
}

func TestPostRendererRejectsMissingTrustedPlan(test *testing.T) {
	test.Setenv("KOGNITION_INSTALL_PLAN", "")
	var output bytes.Buffer
	if err := postRender(bytes.NewBufferString("unreviewed"), &output); err == nil || output.Len() != 0 {
		test.Fatal("post renderer accepted content without reviewed plan")
	}
}

func TestHelmSettlementFlags(test *testing.T) {
	for _, scenario := range []struct {
		name  string
		mode  string
		flags []string
		want  int
	}{
		{"missing-digest", "recover", []string{"--helm-record-uid", "record"}, 2},
		{"missing-uid", "recover", []string{"--helm-record-sha256", strings.Repeat("a", 64)}, 2},
		{"short-digest", "recover", []string{"--helm-record-uid", "record", "--helm-record-sha256", "aa"}, 2},
		{"invalid-hex", "recover", []string{"--helm-record-uid", "record", "--helm-record-sha256", strings.Repeat("z", 64)}, 2},
		{"uppercase", "recover", []string{"--helm-record-uid", "record", "--helm-record-sha256", strings.Repeat("A", 64)}, 2},
		{"bootstrap", "bootstrap", []string{"--helm-record-uid", "record", "--helm-record-sha256", strings.Repeat("a", 64)}, 2},
		{"valid-recovery", "recover", []string{"--helm-record-uid", "record", "--helm-record-sha256", strings.Repeat("a", 64)}, 1},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			arguments := []string{scenario.mode, "--plan", "absent", "--writers-frozen"}
			if scenario.mode == "recover" {
				arguments = append(arguments, "--executors-stopped", "--requests-settled", "--claim-uid", "claim")
			}
			arguments = append(arguments, scenario.flags...)
			var stdout, stderr bytes.Buffer
			if code := run(arguments, bytes.NewReader(nil), &stdout, &stderr); code != scenario.want || stdout.Len() != 0 {
				test.Fatalf("exit=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
			}
		})
	}
}
