// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package router sends each collaboration dispatch to the transport that governs
// its recipient: Agent members run as AgentJobs, ExternalAgent members are
// contacted over A2A under an ExternalAgentGrant. The two trust models never mix.
package router

import (
	"context"
	"errors"
	"fmt"

	"github.com/kognition-ai/kognition/internal/collaboration"
	"github.com/kognition-ai/kognition/internal/collaboration/a2atransport"
	"github.com/kognition-ai/kognition/internal/collaboration/driver"
	"github.com/kognition-ai/kognition/internal/collaboration/externaltransport"
	"github.com/kognition-ai/kognition/internal/controller"
)

// TransportFactory builds a transport for one activation. It is satisfied by
// runner.TransportFactory.
type TransportFactory interface {
	Transport(ctx context.Context, run controller.WorkflowCollaborationRun, handles a2atransport.Handles) (driver.Transport, error)
}

// Factory builds a routing transport. External may be nil, which means external
// agents are not enabled and any ExternalAgent member is refused.
type Factory struct {
	Internal TransportFactory
	External TransportFactory
}

// Transport implements TransportFactory. A step without ExternalAgent members
// gets the internal transport unchanged.
func (f Factory) Transport(ctx context.Context, run controller.WorkflowCollaborationRun, handles a2atransport.Handles) (driver.Transport, error) {
	if f.Internal == nil {
		return nil, errors.New("router: an internal transport factory is required")
	}
	internal, err := f.Internal.Transport(ctx, run, handles)
	if err != nil {
		return nil, err
	}
	external := externaltransport.Members(run)
	if len(external) == 0 {
		return internal, nil
	}
	if f.External == nil {
		return refusing{reason: "ExternalAgent members are not enabled", external: external, internal: internal}, nil
	}
	remote, err := f.External.Transport(ctx, run, handles)
	if err != nil {
		return nil, err
	}
	return &Router{internal: internal, external: remote, members: external}, nil
}

// Router implements driver.Transport.
type Router struct {
	internal driver.Transport
	external driver.Transport
	members  map[string]string
}

var _ driver.Transport = (*Router)(nil)

func (r *Router) route(recipient string) driver.Transport {
	if _, ok := r.members[recipient]; ok {
		return r.external
	}
	return r.internal
}

// Admit implements driver.Transport. An ExternalAgent cannot request work from
// an Agent member: its replies are untrusted, so they must not spend the
// authority an AgentCommunicationPolicy grants an Agent.
func (r *Router) Admit(ctx context.Context, msg collaboration.Message) error {
	if _, requesterExternal := r.members[msg.Requester]; requesterExternal {
		if _, recipientExternal := r.members[msg.Recipient]; !recipientExternal {
			return fmt.Errorf("%w: external member %q may not delegate to Agent member %q", driver.ErrRefused, msg.Requester, msg.Recipient)
		}
	}
	return r.route(msg.Recipient).Admit(ctx, msg)
}

// Send implements driver.Transport.
func (r *Router) Send(ctx context.Context, msg collaboration.Message) error {
	return r.route(msg.Recipient).Send(ctx, msg)
}

// Observe implements driver.Transport.
func (r *Router) Observe(ctx context.Context, msg collaboration.Message) (driver.Observation, error) {
	return r.route(msg.Recipient).Observe(ctx, msg)
}

// refusing serves a step whose ExternalAgent members cannot be contacted. It
// refuses their dispatches before any commit and leaves Agent members alone.
type refusing struct {
	reason   string
	external map[string]string
	internal driver.Transport
}

func (r refusing) Admit(ctx context.Context, msg collaboration.Message) error {
	_, recipient := r.external[msg.Recipient]
	_, requester := r.external[msg.Requester]
	if recipient || requester {
		return fmt.Errorf("%w: %s", driver.ErrRefused, r.reason)
	}
	return r.internal.Admit(ctx, msg)
}

func (r refusing) Send(ctx context.Context, msg collaboration.Message) error {
	if _, ok := r.external[msg.Recipient]; ok {
		return fmt.Errorf("router: %s", r.reason)
	}
	return r.internal.Send(ctx, msg)
}

func (r refusing) Observe(ctx context.Context, msg collaboration.Message) (driver.Observation, error) {
	if _, ok := r.external[msg.Recipient]; ok {
		return driver.Observation{}, fmt.Errorf("router: %s", r.reason)
	}
	return r.internal.Observe(ctx, msg)
}
