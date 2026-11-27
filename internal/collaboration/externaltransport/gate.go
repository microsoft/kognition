// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package externaltransport implements the collaboration driver's Transport for
// members that are ExternalAgents. An external agent is a trust boundary: every
// contact is re-authorized against the live ExternalAgent, the operator egress
// allow-list, the resolved addresses and a live ExternalAgentGrant, and is
// audited before it is made. The bearer credential is resolved only when the
// transport is about to use it and is handed to the remote client alone.
package externaltransport

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"time"

	api "github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/externalagent"
	"github.com/kognition-ai/kognition/internal/store"
)

// Kinds read from the Store.
const (
	kindExternalAgent      = "ExternalAgent"
	kindExternalAgentGrant = "ExternalAgentGrant"
	kindWorkflow           = "Workflow"
	kindWorkflowRun        = "WorkflowRun"
)

// Trust decision outcomes and reason codes used by denials. They are members of
// the Trust Decision v1 schema enumerations.
const (
	outcomeDenied  = "denied"
	outcomeRevoked = "revoked"
	reasonPolicy   = "policy_denied"
	reasonNoGrant  = "grant_missing"
	reasonRevoked  = "grant_revoked"
)

// Denial is a policy decision that forbids contact. It is not an infrastructure
// failure: the dispatch is refused rather than left undecided.
type Denial struct {
	Outcome string
	Code    string
	Reason  string
}

// Error implements error. The text is safe to record.
func (d *Denial) Error() string { return d.Reason }

func deny(code, format string, args ...any) *Denial {
	return &Denial{Outcome: outcomeDenied, Code: code, Reason: fmt.Sprintf(format, args...)}
}

// Target is an ExternalAgent that passed every contact check.
type Target struct {
	Agent  api.ExternalAgent
	Host   string
	Port   int
	Addrs  []netip.Addr
	Limits api.EffectiveExternalAgentLimits
	// Grant is the name of the ExternalAgentGrant that permitted the contact.
	// It is empty when the check was made without a grant requirement.
	Grant string
}

// AuthorizeOptions tunes one authorization.
type AuthorizeOptions struct {
	// SkipGrant omits the ExternalAgentGrant check. It is used only to cancel a
	// task that was already authorized, so that a revoked grant cannot stop the
	// platform from asking the remote agent to stop.
	SkipGrant bool
	// Revalidate marks a re-check of a dispatch that Admit already accepted, so
	// a grant that vanished is reported as revoked rather than missing.
	Revalidate bool
}

// Gate decides whether the platform may contact an ExternalAgent on behalf of one
// Workflow activation. It holds no state: every call reads the live resources.
type Gate struct {
	Store store.Store
	// Allow is the operator egress allow-list. A nil or empty list permits nothing.
	Allow    *externalagent.AllowList
	Resolver externalagent.Resolver
	Now      func() time.Time

	Namespace string
	// OwnerKind is Workflow or WorkflowRun and RunName names that object.
	OwnerKind string
	RunName   string
}

// Authorize returns the Target for name, or a *Denial when policy forbids
// contact. Any other error is an infrastructure failure and decides nothing.
func (g Gate) Authorize(ctx context.Context, name string, opts AuthorizeOptions) (Target, error) {
	raw, err := g.Store.Get(ctx, kindExternalAgent, g.Namespace, name)
	if errors.Is(err, store.ErrNotFound) {
		return Target{}, deny(reasonPolicy, "ExternalAgent %q not found", name)
	}
	if err != nil {
		return Target{}, fmt.Errorf("read ExternalAgent %q: %w", name, err)
	}
	var agent api.ExternalAgent
	if err := json.Unmarshal(raw, &agent); err != nil {
		return Target{}, fmt.Errorf("decode ExternalAgent %q: %w", name, err)
	}
	if agent.ObjectMeta.DeletionTimestamp != nil {
		return Target{}, deny(reasonPolicy, "ExternalAgent %q is being deleted", name)
	}
	if err := agent.Spec.Validate(); err != nil {
		return Target{}, deny(reasonPolicy, "ExternalAgent %q is invalid", name)
	}
	host, port, err := api.ParseExternalAgentEndpoint(agent.Spec.Endpoint)
	if err != nil {
		return Target{}, deny(reasonPolicy, "ExternalAgent %q endpoint is invalid", name)
	}
	if g.Allow == nil || g.Allow.Empty() || !g.Allow.Permits(host, port) {
		return Target{}, deny(reasonPolicy, "ExternalAgent %q endpoint is not in the operator egress allow-list", name)
	}
	addrs, err := externalagent.ResolvePermitted(ctx, g.Resolver, host)
	switch {
	case errors.Is(err, externalagent.ErrForbiddenAddress):
		return Target{}, deny(reasonPolicy, "ExternalAgent %q endpoint resolves to a forbidden address", name)
	case err != nil:
		return Target{}, fmt.Errorf("resolve ExternalAgent %q endpoint: %w", name, err)
	}
	target := Target{Agent: agent, Host: host, Port: port, Addrs: addrs, Limits: agent.Spec.Limits.Effective()}
	if opts.SkipGrant {
		return target, nil
	}
	grant, err := g.grantFor(ctx, name)
	if err != nil {
		var denial *Denial
		if errors.As(err, &denial) && opts.Revalidate {
			return Target{}, &Denial{Outcome: outcomeRevoked, Code: reasonRevoked, Reason: denial.Reason}
		}
		return Target{}, err
	}
	target.Grant = grant
	return target, nil
}

// grantFor returns the name of a live ExternalAgentGrant that lets this Workflow
// invoke the ExternalAgent. There is no implicit access.
func (g Gate) grantFor(ctx context.Context, agent string) (string, error) {
	workflow, labels, err := g.subject(ctx)
	if err != nil {
		return "", err
	}
	items, err := g.Store.List(ctx, kindExternalAgentGrant, store.ListOptions{Namespace: g.Namespace})
	if err != nil {
		return "", fmt.Errorf("list ExternalAgentGrants: %w", err)
	}
	now := g.Now()
	for _, item := range items {
		var grant api.ExternalAgentGrant
		if json.Unmarshal(item, &grant) != nil || grant.ObjectMeta.DeletionTimestamp != nil {
			continue
		}
		if grant.Spec.Permits(agent, workflow, labels, now) {
			return grant.ObjectMeta.Name, nil
		}
	}
	return "", deny(reasonNoGrant, "no ExternalAgentGrant lets Workflow %q invoke ExternalAgent %q", workflow, agent)
}

// subject resolves the Workflow a grant applies to and its live labels. A
// WorkflowRun is governed by the Workflow it was created from.
func (g Gate) subject(ctx context.Context) (string, map[string]string, error) {
	workflow := g.RunName
	if g.OwnerKind == kindWorkflowRun {
		raw, err := g.Store.Get(ctx, kindWorkflowRun, g.Namespace, g.RunName)
		if errors.Is(err, store.ErrNotFound) {
			return "", nil, deny(reasonNoGrant, "WorkflowRun %q not found", g.RunName)
		}
		if err != nil {
			return "", nil, fmt.Errorf("read WorkflowRun %q: %w", g.RunName, err)
		}
		var run api.WorkflowRun
		if err := json.Unmarshal(raw, &run); err != nil {
			return "", nil, fmt.Errorf("decode WorkflowRun %q: %w", g.RunName, err)
		}
		workflow = run.Spec.WorkflowRef.Name
	}
	raw, err := g.Store.Get(ctx, kindWorkflow, g.Namespace, workflow)
	if errors.Is(err, store.ErrNotFound) {
		return "", nil, deny(reasonNoGrant, "Workflow %q not found", workflow)
	}
	if err != nil {
		return "", nil, fmt.Errorf("read Workflow %q: %w", workflow, err)
	}
	var wf api.Workflow
	if err := json.Unmarshal(raw, &wf); err != nil {
		return "", nil, fmt.Errorf("decode Workflow %q: %w", workflow, err)
	}
	return workflow, wf.ObjectMeta.Labels, nil
}
