// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package router

import (
	"context"
	"errors"
	"strings"
	"testing"

	api "github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/collaboration"
	"github.com/kognition-ai/kognition/internal/collaboration/a2atransport"
	"github.com/kognition-ai/kognition/internal/collaboration/driver"
	"github.com/kognition-ai/kognition/internal/controller"
)

type recorder struct {
	name  string
	calls []string
	err   error
}

func (r *recorder) note(op string, msg collaboration.Message) {
	r.calls = append(r.calls, op+":"+msg.Recipient)
}

func (r *recorder) Admit(_ context.Context, msg collaboration.Message) error {
	r.note("admit", msg)
	return r.err
}

func (r *recorder) Send(_ context.Context, msg collaboration.Message) error {
	r.note("send", msg)
	return r.err
}

func (r *recorder) Observe(_ context.Context, msg collaboration.Message) (driver.Observation, error) {
	r.note("observe", msg)
	return driver.Observation{Settled: true, Actor: r.name}, r.err
}

type factoryFunc func() driver.Transport

func (f factoryFunc) Transport(context.Context, controller.WorkflowCollaborationRun, a2atransport.Handles) (driver.Transport, error) {
	return f(), nil
}

func run(members ...api.CollaborationMember) controller.WorkflowCollaborationRun {
	return controller.WorkflowCollaborationRun{Spec: &api.WorkflowCollaboration{Members: members}}
}

func agent(alias string) api.CollaborationMember {
	return api.CollaborationMember{Name: alias, AgentRef: api.AgentReference{Name: alias + "-agent"}}
}

func remote(alias string) api.CollaborationMember {
	return api.CollaborationMember{Name: alias, ExternalAgentRef: &api.LocalObjectReference{Name: alias + "-ext"}}
}

func TestRoutesByRecipientKind(t *testing.T) {
	internal, external := &recorder{name: "internal"}, &recorder{name: "external"}
	f := Factory{Internal: factoryFunc(func() driver.Transport { return internal }), External: factoryFunc(func() driver.Transport { return external })}
	tr, err := f.Transport(context.Background(), run(agent("lead"), remote("partner")), nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, recipient := range []string{"lead", "partner"} {
		msg := collaboration.Message{Recipient: recipient}
		if err := tr.Admit(ctx, msg); err != nil {
			t.Fatal(err)
		}
		if err := tr.Send(ctx, msg); err != nil {
			t.Fatal(err)
		}
		obs, err := tr.Observe(ctx, msg)
		if err != nil {
			t.Fatal(err)
		}
		want := "internal"
		if recipient == "partner" {
			want = "external"
		}
		if obs.Actor != want {
			t.Fatalf("%s observed by %s, want %s", recipient, obs.Actor, want)
		}
	}
	if got := strings.Join(internal.calls, ","); got != "admit:lead,send:lead,observe:lead" {
		t.Fatalf("internal calls: %s", got)
	}
	if got := strings.Join(external.calls, ","); got != "admit:partner,send:partner,observe:partner" {
		t.Fatalf("external calls: %s", got)
	}
}

func TestStepWithoutExternalMembersUsesTheInternalTransportUnchanged(t *testing.T) {
	internal := &recorder{}
	external := factoryFunc(func() driver.Transport {
		t.Fatal("external transport built for a step with no external member")
		return nil
	})
	f := Factory{Internal: factoryFunc(func() driver.Transport { return internal }), External: external}
	tr, err := f.Transport(context.Background(), run(agent("a"), agent("b")), nil)
	if err != nil || tr != driver.Transport(internal) {
		t.Fatalf("got %v, %v", tr, err)
	}
}

func TestExternalMemberCannotDelegateToAnAgent(t *testing.T) {
	internal, external := &recorder{}, &recorder{}
	f := Factory{Internal: factoryFunc(func() driver.Transport { return internal }), External: factoryFunc(func() driver.Transport { return external })}
	tr, err := f.Transport(context.Background(), run(agent("lead"), remote("partner"), remote("other")), nil)
	if err != nil {
		t.Fatal(err)
	}
	err = tr.Admit(context.Background(), collaboration.Message{Requester: "partner", Recipient: "lead"})
	if !errors.Is(err, driver.ErrRefused) {
		t.Fatalf("external requester was not refused: %v", err)
	}
	if len(internal.calls)+len(external.calls) != 0 {
		t.Fatalf("a refused dispatch reached a transport: %v %v", internal.calls, external.calls)
	}
	if err := tr.Admit(context.Background(), collaboration.Message{Requester: "partner", Recipient: "other"}); err != nil {
		t.Fatalf("external to external delegation must reach the grant check: %v", err)
	}
	if err := tr.Admit(context.Background(), collaboration.Message{Requester: "lead", Recipient: "partner"}); err != nil {
		t.Fatalf("an Agent may delegate to an external member: %v", err)
	}
}

func TestExternalMembersAreRefusedWhenTheFeatureIsOff(t *testing.T) {
	internal := &recorder{}
	tr, err := Factory{Internal: factoryFunc(func() driver.Transport { return internal })}.Transport(context.Background(), run(agent("lead"), remote("partner")), nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := tr.Admit(ctx, collaboration.Message{Recipient: "partner"}); !errors.Is(err, driver.ErrRefused) {
		t.Fatalf("external recipient was not refused: %v", err)
	}
	if err := tr.Admit(ctx, collaboration.Message{Requester: "partner", Recipient: "lead"}); !errors.Is(err, driver.ErrRefused) {
		t.Fatalf("external requester was not refused: %v", err)
	}
	if err := tr.Send(ctx, collaboration.Message{Recipient: "partner"}); err == nil {
		t.Fatal("send to an external member succeeded with the feature off")
	}
	if _, err := tr.Observe(ctx, collaboration.Message{Recipient: "partner"}); err == nil {
		t.Fatal("observe of an external member succeeded with the feature off")
	}
	if err := tr.Admit(ctx, collaboration.Message{Recipient: "lead"}); err != nil {
		t.Fatal(err)
	}
	if len(internal.calls) != 1 {
		t.Fatalf("internal calls: %v", internal.calls)
	}
}

func TestFactoryRequiresAnInternalFactory(t *testing.T) {
	if _, err := (Factory{}).Transport(context.Background(), run(agent("a")), nil); err == nil {
		t.Fatal("missing internal factory accepted")
	}
}
