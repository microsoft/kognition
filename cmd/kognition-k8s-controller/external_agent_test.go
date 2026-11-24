// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"testing"

	"github.com/kognition-ai/kognition/internal/admission"
)

func TestExternalAgentsEnabled(t *testing.T) {
	for value, want := range map[string]bool{"": false, "false": false, " true ": true, "true": true} {
		got, err := externalAgentsEnabled(func(string) string { return value })
		if err != nil || got != want {
			t.Fatalf("%q: got %t, %v", value, got, err)
		}
	}
	if _, err := externalAgentsEnabled(func(string) string { return "yes" }); err == nil {
		t.Fatal("unknown flag accepted")
	}
}

func TestExternalAgentAllowListRejectsMalformedEntries(t *testing.T) {
	lookup := func(value string) func(string) string {
		return func(key string) string {
			if key == externalAgentAllowListEnvironmentVariable {
				return value
			}
			return ""
		}
	}
	if _, err := externalAgentAllowList(lookup("https://agents.example.com:443")); err == nil {
		t.Fatal("a URL entry must fail startup")
	}
	list, err := externalAgentAllowList(lookup(""))
	if err != nil || !list.Empty() || list.Permits("agents.example.com", 443) {
		t.Fatalf("an empty allow-list must permit nothing: %v", err)
	}
	list, err = externalAgentAllowList(lookup("agents.example.com:443, *.partner.example:8443"))
	if err != nil || !list.Permits("agents.example.com", 443) || !list.Permits("a.partner.example", 8443) || list.Permits("partner.example", 8443) {
		t.Fatalf("unexpected allow-list behaviour: %v", err)
	}
}

func TestExternalAgentReconcilerOptionsFollowTheFlag(t *testing.T) {
	env := func(enabled string) func(string) string {
		return func(key string) string {
			switch key {
			case externalAgentsEnabledEnvironmentVariable:
				return enabled
			case externalAgentAllowListEnvironmentVariable:
				return "agents.example.com:443"
			}
			return ""
		}
	}
	if got := externalAgentReconcilerOptions(env("false")); len(got) != 0 {
		t.Fatalf("disabled feature must configure no egress policy, got %d options", len(got))
	}
	if got := externalAgentReconcilerOptions(env("true")); len(got) != 1 {
		t.Fatalf("enabled feature must configure the egress policy, got %d options", len(got))
	}
}

func TestEnableExternalAgentAdmissionReplacesOnlyTheExternalAgentValidator(t *testing.T) {
	other := admission.NewAgentValidator()
	hooks := []admission.ValidatingHook{other, admission.NewExternalAgentValidator()}
	original := hooks[1]
	found, err := enableExternalAgentAdmission(hooks, func(string) string { return "" })
	if err != nil || !found {
		t.Fatalf("found=%t err=%v", found, err)
	}
	if hooks[0] != admission.ValidatingHook(other) || hooks[1] == original {
		t.Fatal("unexpected hook replacement")
	}
	if found, _ := enableExternalAgentAdmission([]admission.ValidatingHook{other}, func(string) string { return "" }); found {
		t.Fatal("reported a validator that is absent")
	}
	if _, err := enableExternalAgentAdmission(hooks, func(string) string { return "bad entry:" }); err == nil {
		t.Fatal("malformed allow-list accepted")
	}
}
