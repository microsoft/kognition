// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package traceability

import (
	"encoding/json"
	"strings"
	"testing"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
)

// failurePolicies returns the failurePolicy of every kognition validating and
// mutating webhook in a render, keyed by configuration kind and webhook name.
func failurePolicies(test *testing.T, extra ...string) map[string]string {
	test.Helper()
	policies := map[string]string{}
	for _, document := range renderControllerChart(test, extra...) {
		var meta struct {
			Kind string `json:"kind"`
		}
		if err := json.Unmarshal(document.Raw, &meta); err != nil {
			test.Fatal(err)
		}
		switch meta.Kind {
		case "ValidatingWebhookConfiguration":
			var configuration admissionregistrationv1.ValidatingWebhookConfiguration
			if err := json.Unmarshal(document.Raw, &configuration); err != nil {
				test.Fatal(err)
			}
			for _, hook := range configuration.Webhooks {
				if hook.FailurePolicy != nil {
					policies[meta.Kind+"/"+hook.Name] = string(*hook.FailurePolicy)
				}
			}
		case "MutatingWebhookConfiguration":
			var configuration admissionregistrationv1.MutatingWebhookConfiguration
			if err := json.Unmarshal(document.Raw, &configuration); err != nil {
				test.Fatal(err)
			}
			for _, hook := range configuration.Webhooks {
				if hook.FailurePolicy != nil {
					policies[meta.Kind+"/"+hook.Name] = string(*hook.FailurePolicy)
				}
			}
		}
	}
	return policies
}

// Authorization decisions made in admission (Solution targets, AgentJobs, approval
// decisions) fail open when the webhook is unavailable and failurePolicy is Ignore.
func TestAdmissionWebhooksFailClosedByDefault(test *testing.T) {
	policies := failurePolicies(test)
	if policies["ValidatingWebhookConfiguration/validate.kognition.dev"] != "Fail" {
		test.Fatalf("the validating webhook is not fail-closed by default: %v", policies)
	}
	for name, policy := range policies {
		if policy != "Fail" {
			test.Fatalf("%s is %s by default, want Fail", name, policy)
		}
	}
}

func TestIgnoreFailurePolicyIsRefusedUnlessExplicitlyAllowed(test *testing.T) {
	output, err := runHelmTemplate(test, "--set", "mode=kubernetes", "--set", "webhook.enabled=true", "--set", "webhook.failurePolicy=Ignore")
	if err == nil {
		test.Fatal("failurePolicy=Ignore rendered although admission authorization would fail open")
	}
	if !strings.Contains(string(output), "failurePolicy") {
		test.Fatalf("the refusal does not explain itself:\n%s", output)
	}

	policies := failurePolicies(test, "--set", "webhook.failurePolicy=Ignore", "--set", "webhook.allowFailOpen=true")
	if policies["ValidatingWebhookConfiguration/validate.kognition.dev"] != "Ignore" {
		test.Fatalf("the explicit opt-out was not honored: %v", policies)
	}
}

func TestUnknownFailurePolicyIsRefused(test *testing.T) {
	if _, err := runHelmTemplate(test, "--set", "webhook.failurePolicy=Bogus", "--set", "webhook.allowFailOpen=true"); err == nil {
		test.Fatal("an invalid failurePolicy rendered")
	}
}
