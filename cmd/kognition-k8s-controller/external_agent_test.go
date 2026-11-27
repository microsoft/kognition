// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"strings"
	"testing"

	"github.com/kognition-ai/kognition/internal/admission"
	"github.com/kognition-ai/kognition/internal/collaboration/externaltransport"
	"github.com/kognition-ai/kognition/internal/observability"
	"github.com/kognition-ai/kognition/internal/store"
)

type externalAgentTestStore struct{ store.Store }

func externalAgentEnv(enabled, allowList string) func(string) string {
	return func(key string) string {
		switch key {
		case externalAgentsEnabledEnvironmentVariable:
			return enabled
		case externalAgentAllowListEnvironmentVariable:
			return allowList
		}
		return ""
	}
}

func TestExternalAgentTransportFactory(t *testing.T) {
	st := &externalAgentTestStore{}
	audit := observability.NewCognitiveAuditLog(nil)

	for _, enabled := range []string{"", "false"} {
		factory, err := externalAgentTransportFactory(externalAgentEnv(enabled, "agents.example.com:443"), nil, nil)
		if err != nil || factory != nil {
			t.Fatalf("flag %q: factory=%v err=%v, want none and no error even without a store", enabled, factory, err)
		}
	}
	if _, err := externalAgentTransportFactory(externalAgentEnv("yes", ""), st, audit); err == nil {
		t.Fatal("an invalid flag value must fail startup")
	}
	for name, tc := range map[string]struct {
		st    store.Store
		audit *observability.CognitiveAuditLog
	}{
		"no store":     {nil, audit},
		"no audit log": {st, nil},
	} {
		factory, err := externalAgentTransportFactory(externalAgentEnv("true", "agents.example.com:443"), tc.st, tc.audit)
		if err == nil || factory != nil || !strings.Contains(err.Error(), "resource store and the audit log") {
			t.Fatalf("%s: factory=%v err=%v", name, factory, err)
		}
	}
	if factory, err := externalAgentTransportFactory(externalAgentEnv("true", "https://agents.example.com"), st, audit); err == nil || factory != nil {
		t.Fatalf("a malformed allow-list must fail startup: factory=%v err=%v", factory, err)
	}

	factory, err := externalAgentTransportFactory(externalAgentEnv("true", "agents.example.com:443"), st, audit)
	if err != nil || factory == nil {
		t.Fatalf("factory=%v err=%v", factory, err)
	}
	built, ok := factory.(externaltransport.Factory)
	if !ok || built.Limiter == nil || built.Audit == nil || built.Audit.Log != audit || !built.Allow.Permits("agents.example.com", 443) || built.Allow.Permits("other.example.com", 443) {
		t.Fatalf("unexpected factory: %+v", factory)
	}
}

func TestWorkflowCollaborationRunnerFromEnvChecksTheStoreOnlyWhenEnabled(t *testing.T) {
	t.Setenv(workflowCollaborationEnabledEnvironmentVariable, "false")
	if instance, err := workflowCollaborationRunnerFromEnv(nil, nil, nil, nil, nil); err != nil || instance != nil {
		t.Fatalf("disabled: instance=%v err=%v", instance, err)
	}
	t.Setenv(workflowCollaborationEnabledEnvironmentVariable, "true")
	_, err := workflowCollaborationRunnerFromEnv(nil, nil, nil, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "requires the resource store") {
		t.Fatalf("enabled without a store: err=%v", err)
	}
}

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
	if got := externalAgentReconcilerOptions(env("true")); len(got) != 2 {
		t.Fatalf("enabled feature must configure the egress policy and the card observer, got %d options", len(got))
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
