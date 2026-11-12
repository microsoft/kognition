// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package traceability

import (
	"context"
	"encoding/json"
	"os/exec"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestGitOpsIntentGatewayUsesReviewedImageDigest(test *testing.T) {
	digest := "sha256:" + strings.Repeat("b", 64)
	for _, scenario := range []struct {
		name, registry, repository, digest, tag, expected string
	}{
		{"digest", "", "kognition-intent-gateway", digest, "ignored", "kognition-intent-gateway@" + digest},
		{"registry", "registry.example.com/release", "kognition-intent-gateway", digest, "ignored", "registry.example.com/release/kognition-intent-gateway@" + digest},
		{"qualified-repository", "registry.example.com/release", "mirror.example.com/intent", digest, "ignored", "mirror.example.com/intent@" + digest},
		{"tag-compatibility", "", "kognition-intent-gateway", "", "reviewed", "kognition-intent-gateway:reviewed"},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			found := false
			for _, document := range renderControllerChart(test,
				"--set", "global.imageRegistry="+scenario.registry,
				"--set", "intentGateway.image.repository="+scenario.repository,
				"--set", "intentGateway.image.digest="+scenario.digest,
				"--set", "intentGateway.image.tag="+scenario.tag,
			) {
				var deployment appsv1.Deployment
				if err := json.Unmarshal(document.Raw, &deployment); err != nil {
					test.Fatal(err)
				}
				if deployment.Kind != "Deployment" || deployment.Name != "security-test-kognition-intent-gateway" {
					continue
				}
				found = true
				if len(deployment.Spec.Template.Spec.Containers) != 1 {
					test.Fatal("Intent Gateway requires exactly one container")
				}
				if actual := deployment.Spec.Template.Spec.Containers[0].Image; actual != scenario.expected {
					test.Fatalf("Intent Gateway image = %q, want %q", actual, scenario.expected)
				}
			}
			if !found {
				test.Fatal("enabled Intent Gateway Deployment is missing")
			}
		})
	}
}

func TestGitOpsIntentGatewayRejectsInvalidImageDigest(test *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, "helm", "template", "security-test", "../../deploy/helm/kognition",
		"--namespace", "control", "--kube-version", "1.31.1", "--set", "mode=kubernetes",
		"--set", "intentGateway.image.digest=sha256:invalid").CombinedOutput()
	if err == nil {
		test.Fatal("invalid Intent Gateway image digest was not rejected")
	}

	if !strings.Contains(string(output), "first-party image digest must be a lowercase sha256 digest") {
		test.Fatalf("invalid Intent Gateway image digest was not rejected: %v %s", err, output)
	}
}

func TestGitOpsIsolationBaselinesRetainApplicationTrackingIdentity(test *testing.T) {
	for _, profile := range []string{"network-policy", "cilium"} {
		test.Run(profile, func(test *testing.T) {
			found := 0
			for _, document := range renderControllerChart(test,
				"--set", "k8sController.managedNamespaces[0]=tenant",
				"--set", "admissionGuard.stdioIsolationProfile="+profile,
			) {
				var object unstructured.Unstructured
				if err := json.Unmarshal(document.Raw, &object); err != nil {
					test.Fatal(err)
				}
				if object.GetNamespace() != "tenant" || (object.GetKind() != "NetworkPolicy" && object.GetKind() != "CiliumNetworkPolicy") {
					continue
				}
				found++
				if object.GetLabels()["app.kubernetes.io/instance"] != "security-test" {
					test.Fatalf("%s %s lacks the matching Argo Application and Helm release tracking identity", object.GetKind(), object.GetName())
				}
			}
			expected := 1
			if profile == "cilium" {
				expected++
			}
			if found != expected {
				test.Fatalf("isolation baseline count = %d, want %d", found, expected)
			}
		})
	}
}
