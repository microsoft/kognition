// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package workflows_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/netip"
	"testing"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/admission"
	"github.com/kognition-ai/kognition/internal/externalagent"
)

type publicResolver struct{}

func (publicResolver) LookupNetIP(context.Context, string, string) ([]netip.Addr, error) {
	return []netip.Addr{netip.MustParseAddr("93.184.216.34")}, nil
}

func admitSample(t *testing.T, validator admission.ValidatingHook, kind, name, namespace string, raw []byte) *admission.Response {
	t.Helper()
	return validator.Validate(context.Background(), &admission.Request{
		Operation: admission.OperationCreate, Kind: kind, Namespace: namespace, Name: name, Object: raw,
	})
}

func strictSample(t *testing.T, raw []byte, into any) {
	t.Helper()
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(into); err != nil {
		t.Fatalf("sample does not match its schema: %v", err)
	}
}

func TestExternalAgentSampleIsAdmittedOnlyWhenEnabled(t *testing.T) {
	objects := loadManifestObjects(t, "external-agent.yaml")

	workflow := requireManifestObject(t, objects, "Workflow/wfdemo-team-external")
	raw, err := json.Marshal(workflow.Object)
	if err != nil {
		t.Fatal(err)
	}
	var strictWorkflow v1alpha1.Workflow
	strictSample(t, raw, &strictWorkflow)
	enabled := admission.NewWorkflowValidator(admission.WithCollaborationEnabled(true), admission.WithExternalAgentsEnabled(true))
	if response := admitSample(t, enabled, "Workflow", workflow.GetName(), workflow.GetNamespace(), raw); !response.Allowed {
		t.Fatalf("workflow rejected with external agents enabled: %s", response.Reason)
	}
	collaborationOnly := admission.NewWorkflowValidator(admission.WithCollaborationEnabled(true))
	if response := admitSample(t, collaborationOnly, "Workflow", workflow.GetName(), workflow.GetNamespace(), raw); response.Allowed {
		t.Fatal("workflow must be rejected when external agents are not enabled")
	}

	agent := requireManifestObject(t, objects, "ExternalAgent/partner-compliance")
	raw, err = json.Marshal(agent.Object)
	if err != nil {
		t.Fatal(err)
	}
	var strictAgent v1alpha1.ExternalAgent
	strictSample(t, raw, &strictAgent)
	allowed, err := externalagent.ParseAllowList(strictAgent.Spec.Endpoint)
	if err != nil {
		t.Fatal(err)
	}
	permitted := admission.NewExternalAgentValidator(admission.WithExternalAgentResolver(publicResolver{}), admission.WithExternalAgentEgress(allowed))
	if response := admitSample(t, permitted, "ExternalAgent", agent.GetName(), agent.GetNamespace(), raw); !response.Allowed {
		t.Fatalf("external agent rejected when its endpoint is allow-listed: %s", response.Reason)
	}
	empty, err := externalagent.ParseAllowList("")
	if err != nil {
		t.Fatal(err)
	}
	denied := admission.NewExternalAgentValidator(admission.WithExternalAgentResolver(publicResolver{}), admission.WithExternalAgentEgress(empty))
	if response := admitSample(t, denied, "ExternalAgent", agent.GetName(), agent.GetNamespace(), raw); response.Allowed {
		t.Fatal("external agent must be rejected when the allow-list is empty")
	}

	grant := requireManifestObject(t, objects, "ExternalAgentGrant/wfdemo-team-external-partner-compliance")
	raw, err = json.Marshal(grant.Object)
	if err != nil {
		t.Fatal(err)
	}
	var strictGrant v1alpha1.ExternalAgentGrant
	strictSample(t, raw, &strictGrant)
	if response := admitSample(t, admission.NewExternalAgentGrantValidator(), "ExternalAgentGrant", grant.GetName(), grant.GetNamespace(), raw); !response.Allowed {
		t.Fatalf("grant rejected: %s", response.Reason)
	}
	if strictGrant.Spec.WorkflowRef.Name != workflow.GetName() || strictGrant.Spec.ExternalAgentRef.Name != agent.GetName() {
		t.Fatalf("grant does not bind %s to %s: %+v", workflow.GetName(), agent.GetName(), strictGrant.Spec)
	}
}
