// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package traceability

import (
	"encoding/json"
	"testing"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	appsv1 "k8s.io/api/apps/v1"
)

func TestSolutionTargetAdmissionChart(test *testing.T) {
	validated := func(extra ...string) bool {
		for _, document := range renderControllerChart(test, extra...) {
			var configuration admissionregistrationv1.ValidatingWebhookConfiguration
			if err := json.Unmarshal(document.Raw, &configuration); err != nil {
				test.Fatal(err)
			}
			if configuration.Kind != "ValidatingWebhookConfiguration" {
				continue
			}
			for _, hook := range configuration.Webhooks {
				if hook.Name != "validate.kognition.dev" {
					continue
				}
				for _, rule := range hook.Rules {
					hasCreate, hasUpdate := false, false
					for _, operation := range rule.Operations {
						hasCreate = hasCreate || operation == admissionregistrationv1.Create
						hasUpdate = hasUpdate || operation == admissionregistrationv1.Update
					}
					for _, resource := range rule.Resources {
						if resource == "solutions" && hasCreate && hasUpdate {
							return true
						}
					}
				}
			}
		}
		return false
	}
	exempt := func(extra ...string) (string, bool) {
		for _, document := range renderControllerChart(test, extra...) {
			var deployment appsv1.Deployment
			if err := json.Unmarshal(document.Raw, &deployment); err != nil {
				test.Fatal(err)
			}
			if deployment.Kind != "Deployment" || deployment.Labels["app.kubernetes.io/component"] != "resource-webhooks" {
				continue
			}
			for _, variable := range deployment.Spec.Template.Spec.Containers[0].Env {
				if variable.Name == "KOGNITION_SOLUTION_TARGET_EXEMPT_SERVICE_ACCOUNTS" {
					return variable.Value, true
				}
			}
		}
		return "", false
	}

	if !validated() {
		test.Fatal("Solution create and update never reach the validating webhook, so a Solution target is not authorized")
	}
	if value, found := exempt(); !found || value != "security-test-kognition-intent-gateway" {
		test.Fatalf("intent gateway exemption = %q (present %v), want the chart's intent gateway service account", value, found)
	}
	if value, found := exempt("--set", "intentGateway.serviceAccount.name=custom-gateway"); !found || value != "custom-gateway" {
		test.Fatalf("custom intent gateway exemption = %q (present %v)", value, found)
	}
	if value, found := exempt("--set", "intentGateway.enabled=false"); !found || value != "" {
		test.Fatalf("disabled intent gateway still exempted %q (present %v)", value, found)
	}
}
