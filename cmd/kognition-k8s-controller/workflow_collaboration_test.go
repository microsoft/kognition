// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"testing"

	kubefake "k8s.io/client-go/kubernetes/fake"

	"github.com/kognition-ai/kognition/internal/admission"
	intctrl "github.com/kognition-ai/kognition/internal/controller"
)

func TestLedgerOwnersReferenceTheRun(t *testing.T) {
	owners := ledgerOwners(intctrl.WorkflowCollaborationRun{OwnerKind: "WorkflowRun", RunName: "r", RunUID: "u1"})
	if len(owners) != 1 || owners[0].Kind != "WorkflowRun" || owners[0].Name != "r" || owners[0].UID != "u1" || owners[0].APIVersion == "" {
		t.Fatalf("unexpected owners: %+v", owners)
	}
}

func TestWorkflowCollaborationEnabled(t *testing.T) {
	for value, want := range map[string]bool{"": false, "false": false, " true ": true, "true": true} {
		got, err := workflowCollaborationEnabled(func(string) string { return value })
		if err != nil || got != want {
			t.Fatalf("%q: got %t, %v", value, got, err)
		}
	}
	if _, err := workflowCollaborationEnabled(func(string) string { return "yes" }); err == nil {
		t.Fatal("unknown flag accepted")
	}
}

func TestNewWorkflowCollaborationRunnerNeedsNoCertificates(t *testing.T) {
	configMaps := kubefake.NewSimpleClientset().CoreV1().ConfigMaps
	if _, err := newWorkflowCollaborationRunner(nil, configMaps, nil); err != nil {
		t.Fatal(err)
	}
}

func TestEnableCollaborationAdmissionReplacesOnlyTheWorkflowValidator(t *testing.T) {
	other := admission.NewAgentValidator()
	original := admission.NewWorkflowValidator()
	hooks := []admission.ValidatingHook{other, original}
	if !enableCollaborationAdmission(hooks) {
		t.Fatal("workflow validator not found")
	}
	if hooks[0] != admission.ValidatingHook(other) || hooks[1] == admission.ValidatingHook(original) {
		t.Fatal("unexpected hook replacement")
	}
	if enableCollaborationAdmission([]admission.ValidatingHook{other}) {
		t.Fatal("reported a validator that is absent")
	}
}
