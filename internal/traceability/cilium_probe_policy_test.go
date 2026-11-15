// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package traceability

import (
	"context"
	"os/exec"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kognition-ai/kognition/internal/k8s/installation"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestRuntimeProbeAllowancePreservesExistingIngressPolicy(t *testing.T) {
	helm, err := exec.LookPath("helm")
	if err != nil {
		t.Skip("helm is required for chart rendering")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, helm, "template", "probe-test", "../../deploy/helm/kognition",
		"--namespace", "kognition-system", "--kube-version", "1.35.9",
		"--set", "mode=kubernetes", "--set", "admissionGuard.stdioIsolationProfile=cilium",
		"--set", "networkPolicy.enabled=true",
		"--set", "networkPolicy.ciliumApiServerEgress=true", "--set", "networkPolicy.ciliumHostProbeIngress=true",
		"--set", "k8sController.managedNamespaces[0]=kognition-work").CombinedOutput()
	if err != nil {
		t.Fatalf("core probe render failed: %v\n%s", err, output)
	}
	objects, err := installation.Documents(output)
	if err != nil {
		t.Fatal(err)
	}
	probes := 0
	for _, object := range objects {
		if (object.GetKind() != "CiliumNetworkPolicy" && object.GetKind() != "CiliumClusterwideNetworkPolicy") ||
			!strings.HasSuffix(object.GetName(), "-probes") {
			continue
		}
		probes++
		mode, found, _ := unstructured.NestedMap(object.Object, "spec", "enableDefaultDeny")
		if !found || !reflect.DeepEqual(mode, map[string]interface{}{"ingress": false, "egress": false}) {
			t.Fatalf("health allowance %s must preserve existing ingress/egress isolation, got %v", object.GetName(), mode)
		}
	}
	if probes == 0 {
		t.Fatal("no probe policies rendered")
	}
}
