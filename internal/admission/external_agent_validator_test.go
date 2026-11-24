// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package admission

import (
	"context"
	"encoding/json"
	"net/netip"
	"strings"
	"testing"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/externalagent"
)

type fakeExternalResolver map[string][]netip.Addr

func (f fakeExternalResolver) LookupNetIP(_ context.Context, _, host string) ([]netip.Addr, error) {
	addrs, ok := f[host]
	if !ok {
		return nil, &fakeLookupError{host}
	}
	return addrs, nil
}

type fakeLookupError struct{ host string }

func (e *fakeLookupError) Error() string { return "no such host " + e.host }

const externalAgentTestDigest = "sha256:0000000000000000000000000000000000000000000000000000000000000000"

func externalAgentObject(t *testing.T, mutate func(*v1alpha1.ExternalAgent)) json.RawMessage {
	t.Helper()
	agent := v1alpha1.ExternalAgent{
		TypeMeta:   v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "ExternalAgent"},
		ObjectMeta: v1alpha1.ObjectMeta{Name: "partner", Namespace: "team-a"},
		Spec: v1alpha1.ExternalAgentSpec{
			Protocol:      v1alpha1.ExternalAgentProtocolA2A,
			Endpoint:      "agents.example.com:443",
			Card:          v1alpha1.ExternalAgentCard{Digest: externalAgentTestDigest},
			CredentialRef: v1alpha1.LocalObjectReference{Name: "partner-token"},
		},
	}
	if mutate != nil {
		mutate(&agent)
	}
	data, err := json.Marshal(agent)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func publicResolver() fakeExternalResolver {
	return fakeExternalResolver{"agents.example.com": {netip.MustParseAddr("93.184.216.34")}}
}

// TC-0345: a card digest that is not a sha256 hex string is rejected.
func TestExternalAgentValidatorRejectsMalformedSpec(t *testing.T) {
	validator := NewExternalAgentValidator(WithExternalAgentResolver(publicResolver()))
	cases := map[string]func(*v1alpha1.ExternalAgent){
		"digest without prefix": func(a *v1alpha1.ExternalAgent) { a.Spec.Card.Digest = strings.Repeat("a", 64) },
		"upper case digest":     func(a *v1alpha1.ExternalAgent) { a.Spec.Card.Digest = "sha256:" + strings.Repeat("A", 64) },
		"missing credential":    func(a *v1alpha1.ExternalAgent) { a.Spec.CredentialRef.Name = "" },
		"unsupported protocol":  func(a *v1alpha1.ExternalAgent) { a.Spec.Protocol = "MCP" },
		"scheme in endpoint":    func(a *v1alpha1.ExternalAgent) { a.Spec.Endpoint = "https://agents.example.com:443" },
		"ip literal":            func(a *v1alpha1.ExternalAgent) { a.Spec.Endpoint = "10.0.0.5:443" },
		"limit over ceiling": func(a *v1alpha1.ExternalAgent) {
			a.Spec.Limits = &v1alpha1.ExternalAgentLimits{MaxTokensPerTask: v1alpha1.ExternalAgentMaxTokensPerTaskCeiling + 1}
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			response := validator.Validate(context.Background(), &Request{Operation: OperationCreate, Kind: "ExternalAgent", Namespace: "team-a", Object: externalAgentObject(t, mutate)})
			if response.Allowed {
				t.Fatalf("expected rejection")
			}
		})
	}
	if response := validator.Validate(context.Background(), &Request{Operation: OperationCreate, Kind: "ExternalAgent", Namespace: "team-a", Object: externalAgentObject(t, nil)}); !response.Allowed {
		t.Fatalf("valid ExternalAgent rejected: %s", response.Reason)
	}
}

// TC-0346: an endpoint that resolves to any forbidden address is rejected, even when only one answer is bad;
// an endpoint that does not resolve yet is admitted and reported by status.
func TestExternalAgentValidatorChecksResolvedAddresses(t *testing.T) {
	resolver := fakeExternalResolver{
		"agents.example.com": {netip.MustParseAddr("93.184.216.34")},
		"metadata.example":   {netip.MustParseAddr("169.254.169.254")},
		"mixed.example.com":  {netip.MustParseAddr("93.184.216.34"), netip.MustParseAddr("10.1.2.3")},
		"mapped.example.com": {netip.MustParseAddr("::ffff:127.0.0.1")},
	}
	validator := NewExternalAgentValidator(WithExternalAgentResolver(resolver))
	for _, endpoint := range []string{"metadata.example:443", "mixed.example.com:443", "mapped.example.com:443"} {
		endpoint := endpoint
		response := validator.Validate(context.Background(), &Request{Operation: OperationCreate, Kind: "ExternalAgent", Namespace: "team-a",
			Object: externalAgentObject(t, func(a *v1alpha1.ExternalAgent) { a.Spec.Endpoint = endpoint })})
		if response.Allowed || !strings.Contains(response.Reason, "forbidden address") {
			t.Fatalf("%s: expected forbidden-address rejection, got allowed=%v reason=%q", endpoint, response.Allowed, response.Reason)
		}
	}
	response := validator.Validate(context.Background(), &Request{Operation: OperationCreate, Kind: "ExternalAgent", Namespace: "team-a",
		Object: externalAgentObject(t, func(a *v1alpha1.ExternalAgent) { a.Spec.Endpoint = "not-yet.example.com:443" })})
	if !response.Allowed {
		t.Fatalf("unresolvable endpoint must be admitted and reported by status: %s", response.Reason)
	}
}

// TC-0347: the spec is immutable, and a status-only update is not a spec change.
func TestExternalAgentValidatorSpecIsImmutable(t *testing.T) {
	validator := NewExternalAgentValidator(WithExternalAgentResolver(publicResolver()))
	previous := externalAgentObject(t, nil)
	changes := map[string]func(*v1alpha1.ExternalAgent){
		"endpoint":   func(a *v1alpha1.ExternalAgent) { a.Spec.Endpoint = "other.example.com:443" },
		"digest":     func(a *v1alpha1.ExternalAgent) { a.Spec.Card.Digest = "sha256:" + strings.Repeat("1", 64) },
		"credential": func(a *v1alpha1.ExternalAgent) { a.Spec.CredentialRef.Name = "other" },
		"limits": func(a *v1alpha1.ExternalAgent) {
			a.Spec.Limits = &v1alpha1.ExternalAgentLimits{MaxTokensPerTask: 10}
		},
	}
	for name, mutate := range changes {
		response := validator.Validate(context.Background(), &Request{Operation: OperationUpdate, Kind: "ExternalAgent", Namespace: "team-a", OldObject: previous, Object: externalAgentObject(t, mutate)})
		if response.Allowed || !strings.Contains(response.Reason, "immutable") {
			t.Fatalf("%s change: expected immutability rejection, got allowed=%v reason=%q", name, response.Allowed, response.Reason)
		}
	}
	withStatus := externalAgentObject(t, func(a *v1alpha1.ExternalAgent) {
		a.Status.ObservedCardDigest = externalAgentTestDigest
	})
	if response := validator.Validate(context.Background(), &Request{Operation: OperationUpdate, Kind: "ExternalAgent", Namespace: "team-a", OldObject: previous, Object: withStatus}); !response.Allowed {
		t.Fatalf("status-only update rejected: %s", response.Reason)
	}
}

// TC-0346: with the operator egress policy on, only listed endpoints are admitted and an empty list permits none.
func TestExternalAgentValidatorEnforcesAllowList(t *testing.T) {
	allowed, err := externalagent.ParseAllowList("agents.example.com:443")
	if err != nil {
		t.Fatal(err)
	}
	empty, err := externalagent.ParseAllowList("")
	if err != nil {
		t.Fatal(err)
	}
	object := externalAgentObject(t, nil)
	request := &Request{Operation: OperationCreate, Kind: "ExternalAgent", Namespace: "team-a", Object: object}
	if response := NewExternalAgentValidator(WithExternalAgentResolver(publicResolver()), WithExternalAgentEgress(allowed)).Validate(context.Background(), request); !response.Allowed {
		t.Fatalf("listed endpoint rejected: %s", response.Reason)
	}
	for name, list := range map[string]*externalagent.AllowList{"empty": empty, "other host": mustAllowList(t, "other.example.com:443")} {
		response := NewExternalAgentValidator(WithExternalAgentResolver(publicResolver()), WithExternalAgentEgress(list)).Validate(context.Background(), request)
		if response.Allowed || !strings.Contains(response.Reason, "allow-list") {
			t.Fatalf("%s: expected allow-list rejection, got allowed=%v reason=%q", name, response.Allowed, response.Reason)
		}
	}
}

func mustAllowList(t *testing.T, text string) *externalagent.AllowList {
	t.Helper()
	list, err := externalagent.ParseAllowList(text)
	if err != nil {
		t.Fatal(err)
	}
	return list
}

func TestExternalAgentGrantValidator(t *testing.T) {
	validator := NewExternalAgentGrantValidator()
	build := func(mutate func(*v1alpha1.ExternalAgentGrant)) *Request {
		grant := v1alpha1.ExternalAgentGrant{
			TypeMeta:   v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "ExternalAgentGrant"},
			ObjectMeta: v1alpha1.ObjectMeta{Name: "g", Namespace: "team-a"},
			Spec: v1alpha1.ExternalAgentGrantSpec{
				ExternalAgentRef: v1alpha1.LocalObjectReference{Name: "partner"},
				WorkflowRef:      v1alpha1.LocalObjectReference{Name: "wf"},
				Verbs:            []v1alpha1.ExternalAgentGrantVerb{v1alpha1.ExternalAgentGrantVerbInvoke},
			},
		}
		if mutate != nil {
			mutate(&grant)
		}
		data, err := json.Marshal(grant)
		if err != nil {
			t.Fatal(err)
		}
		return &Request{Operation: OperationCreate, Kind: "ExternalAgentGrant", Namespace: "team-a", Object: data}
	}
	if response := validator.Validate(context.Background(), build(nil)); !response.Allowed {
		t.Fatalf("valid grant rejected: %s", response.Reason)
	}
	for name, mutate := range map[string]func(*v1alpha1.ExternalAgentGrant){
		"neither workflowRef nor selector": func(g *v1alpha1.ExternalAgentGrant) { g.Spec.WorkflowRef = v1alpha1.LocalObjectReference{} },
		"both workflowRef and selector":    func(g *v1alpha1.ExternalAgentGrant) { g.Spec.WorkflowSelector = map[string]string{"a": "b"} },
		"no verbs":                         func(g *v1alpha1.ExternalAgentGrant) { g.Spec.Verbs = nil },
		"unknown verb":                     func(g *v1alpha1.ExternalAgentGrant) { g.Spec.Verbs = []v1alpha1.ExternalAgentGrantVerb{"admin"} },
	} {
		if response := validator.Validate(context.Background(), build(mutate)); response.Allowed {
			t.Fatalf("%s: expected rejection", name)
		}
	}
}
