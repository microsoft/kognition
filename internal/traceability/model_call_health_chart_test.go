// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package traceability

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	admissionv1 "k8s.io/api/admissionregistration/v1"
	rbacv1 "k8s.io/api/rbac/v1"
)

func TestModelCallHealthChartScopesEnabledGatewayWriters(t *testing.T) {
	const release = "security-test-kognition"
	for _, tc := range []struct {
		name      string
		model     bool
		knowledge bool
	}{
		{"neither", false, false},
		{"model only", true, false},
		{"knowledge only", false, true},
		{"both", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := []string{"--set", fmt.Sprintf("modelGateway.enabled=%t", tc.model), "--set", fmt.Sprintf("knowledgeGateway.enabled=%t", tc.knowledge)}
			if tc.knowledge {
				args = append(args, "--set", "knowledgeGateway.tls.secretName=provider-client-tls", "--set", "knowledgeGateway.runtimeTLS.secretName=knowledge-runtime-tls",
					"--set", "knowledgeGateway.provider.address=provider:9443", "--set", "knowledgeGateway.provider.serverName=provider",
					"--set", "knowledgeGateway.provider.peerURI=spiffe://kognition.dev/knowledge/provider")
			}
			healthFound, costFound := false, false
			for _, document := range renderControllerRBAC(t, args...) {
				var policy admissionv1.ValidatingAdmissionPolicy
				if err := json.Unmarshal(document.Raw, &policy); err != nil {
					t.Fatal(err)
				}
				if policy.Kind == "ValidatingAdmissionPolicy" && strings.HasSuffix(policy.Name, "-model-call-health") {
					healthFound = true
					var expressions string
					for _, validation := range policy.Spec.Validations {
						expressions += validation.Expression + "\n"
					}
					for _, writer := range []struct {
						component string
						enabled   bool
					}{{"model-gateway", tc.model}, {"knowledge-gateway", tc.knowledge}} {
						username := fmt.Sprintf("system:serviceaccount:control:%s-%s", release, writer.component)
						if strings.Contains(expressions, username) != writer.enabled {
							t.Fatalf("writer %s authorization does not match enabled=%t: %s", writer.component, writer.enabled, expressions)
						}
						if writer.enabled && (!strings.Contains(expressions, "request.operation != 'DELETE'") ||
							!strings.Contains(expressions, "object.metadata.labels['kognition.dev/model-call-health'] == \""+writer.component+"\"")) {
							t.Fatalf("writer %s is missing own-component or delete restriction: %s", writer.component, expressions)
						}
					}
				}
				var role rbacv1.Role
				if err := json.Unmarshal(document.Raw, &role); err != nil {
					t.Fatal(err)
				}
				if role.Kind == "Role" && role.Name == release+"-knowledge-gateway-cost" {
					for _, rule := range role.Rules {
						if reflect.DeepEqual(rule.Resources, []string{"configmaps"}) {
							costFound = true
							if !reflect.DeepEqual(rule.Verbs, []string{"get", "list", "create", "update"}) {
								t.Fatalf("Knowledge Gateway ConfigMap verbs widened: %+v", rule)
							}
						}
					}
				}
			}
			if !healthFound || costFound != tc.knowledge {
				t.Fatalf("health policy=%t cost role=%t, want policy and cost role only when knowledge enabled", healthFound, costFound)
			}
		})
	}
}
