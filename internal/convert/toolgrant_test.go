// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package convert_test

import (
	"reflect"
	"testing"

	api "github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/convert"
)

func TestToolGrantRoundTripPreservesSemantics(t *testing.T) {
	in := &api.ToolGrant{
		TypeMeta: api.TypeMeta{Kind: "ToolGrant", APIVersion: api.APIVersion},
		ObjectMeta: api.ObjectMeta{
			Name:        "researcher-search",
			Namespace:   "team-a",
			Labels:      map[string]string{"purpose": "research"},
			Annotations: map[string]string{"kognition.dev/source": "test"},
		},
		Spec: api.ToolGrantSpec{
			AgentSelector: map[string]string{"role": "researcher"},
			ToolRef:       api.LocalObjectReference{Name: "web-search"},
			Verbs:         []api.ToolGrantVerb{api.ToolGrantVerbInvoke, api.ToolGrantVerbInspect},
			ArgumentConstraints: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"query": map[string]any{"type": "string", "maxLength": float64(200)},
				},
			},
			RateLimits: &api.ToolGrantRateLimits{
				CallsPerMinute: 10,
				CallsPerHour:   100,
				CallsPerDay:    500,
			},
			Conditions: &api.ToolGrantConditions{
				ValidFrom:           "2026-01-01T00:00:00Z",
				ValidUntil:          "2026-12-31T23:59:59Z",
				RequiredAgentLabels: map[string]string{"clearance": "standard"},
			},
		},
		Status: api.ToolGrantStatus{
			Active:            true,
			LastEvaluatedTime: "2026-03-19T12:00:00Z",
			GrantedAgents:     3,
		},
	}

	k8sGrant := convert.ToolGrantToK8s(in)
	out := convert.ToolGrantFromK8s(k8sGrant)

	if !reflect.DeepEqual(out, in) {
		t.Fatalf("ToolGrant round trip changed resource:\n got: %#v\nwant: %#v", out, in)
	}

	direct := &api.ToolGrant{
		TypeMeta:   api.TypeMeta{Kind: "ToolGrant", APIVersion: api.APIVersion},
		ObjectMeta: api.ObjectMeta{Name: "direct-search", Namespace: "team-a"},
		Spec: api.ToolGrantSpec{
			AgentRef: api.LocalObjectReference{Name: "researcher-a"},
			ToolRef:  api.LocalObjectReference{Name: "web-search"},
			Verbs:    []api.ToolGrantVerb{api.ToolGrantVerbDiscover},
		},
	}
	directOut := convert.ToolGrantFromK8s(convert.ToolGrantToK8s(direct))
	if !reflect.DeepEqual(directOut, direct) {
		t.Fatalf("direct ToolGrant round trip changed resource:\n got: %#v\nwant: %#v", directOut, direct)
	}
}
