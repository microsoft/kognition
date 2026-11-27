// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package externaltransport

import (
	"context"
	"errors"
	"time"

	"github.com/kognition-ai/kognition/internal/collaboration/a2atransport"
	"github.com/kognition-ai/kognition/internal/collaboration/driver"
	"github.com/kognition-ai/kognition/internal/controller"
	"github.com/kognition-ai/kognition/internal/externalagent"
	"github.com/kognition-ai/kognition/internal/store"
)

// Factory builds one Transport per activation. It is created once per process,
// so its Limiter is shared by every activation and bounds concurrent tasks per
// ExternalAgent across them. It implements runner.TransportFactory.
type Factory struct {
	Store       store.Store
	Allow       *externalagent.AllowList
	Resolver    externalagent.Resolver
	Credentials CredentialResolver
	Client      Client
	Audit       *Auditor
	Limiter     *Limiter
	Now         func() time.Time
}

// Transport builds the transport of the ExternalAgent members of run.
func (f Factory) Transport(_ context.Context, run controller.WorkflowCollaborationRun, handles a2atransport.Handles) (driver.Transport, error) {
	if run.Spec == nil {
		return nil, errors.New("externaltransport: collaboration spec is required")
	}
	replacer, ok := handles.(a2atransport.HandleReplacer)
	if !ok {
		return nil, errors.New("externaltransport: the handle store cannot record send state changes")
	}
	return New(Config{
		Store: f.Store, Run: run, Members: Members(run), Allow: f.Allow, Resolver: f.Resolver,
		Credentials: f.Credentials, Client: f.Client, Handles: replacer, Audit: f.Audit,
		Limiter: f.Limiter, Now: f.Now,
	})
}

// Members maps the alias of each ExternalAgent member of run to its ExternalAgent.
// The reference is name-only, so the ExternalAgent is always in the run namespace.
func Members(run controller.WorkflowCollaborationRun) map[string]string {
	members := map[string]string{}
	if run.Spec == nil {
		return members
	}
	for _, m := range run.Spec.Members {
		if m.ExternalAgentRef != nil && m.ExternalAgentRef.Name != "" {
			members[m.Name] = m.ExternalAgentRef.Name
		}
	}
	return members
}
