// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"fmt"
	"strings"

	"github.com/kognition-ai/kognition/internal/admission"
	"github.com/kognition-ai/kognition/internal/collaboration/externaltransport"
	intctrl "github.com/kognition-ai/kognition/internal/controller"
	"github.com/kognition-ai/kognition/internal/externalagent"
)

const (
	// externalAgentsEnabledEnvironmentVariable enables the ExternalAgent trust boundary. It is disabled unless "true".
	externalAgentsEnabledEnvironmentVariable = "KOGNITION_EXTERNAL_AGENTS_ENABLED"

	// externalAgentAllowListEnvironmentVariable holds the operator egress allow-list: one host:port or
	// *.suffix:port entry per line or comma. An empty value permits nothing.
	externalAgentAllowListEnvironmentVariable = "KOGNITION_EXTERNAL_AGENT_ALLOWLIST"
)

// externalAgentsEnabled reports whether external agents may be used by workflows.
func externalAgentsEnabled(lookup func(string) string) (bool, error) {
	switch strings.TrimSpace(lookup(externalAgentsEnabledEnvironmentVariable)) {
	case "", "false":
		return false, nil
	case "true":
		return true, nil
	}
	return false, fmt.Errorf("%s must be true or false", externalAgentsEnabledEnvironmentVariable)
}

// externalAgentAllowList parses the operator allow-list. A malformed entry fails startup rather than silently
// widening or narrowing egress.
func externalAgentAllowList(lookup func(string) string) (*externalagent.AllowList, error) {
	list, err := externalagent.ParseAllowList(lookup(externalAgentAllowListEnvironmentVariable))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", externalAgentAllowListEnvironmentVariable, err)
	}
	return list, nil
}

// externalAgentReconcilerOptions returns the status reconciler options. A configuration error yields no options, so
// the reconciler reports FeatureDisabled; startup validates the same environment and fails on the error.
func externalAgentReconcilerOptions(lookup func(string) string) []intctrl.ExternalAgentReconcilerOption {
	enabled, err := externalAgentsEnabled(lookup)
	if err != nil || !enabled {
		return nil
	}
	allowList, err := externalAgentAllowList(lookup)
	if err != nil {
		return nil
	}
	return []intctrl.ExternalAgentReconcilerOption{
		intctrl.WithExternalAgentEgressPolicy(allowList),
		intctrl.WithExternalAgentCardObserver(externaltransport.CardObserver{Resolver: externalagent.DefaultResolver()}),
	}
}

// enableExternalAgentAdmission makes the ExternalAgent validator enforce the operator egress allow-list. It
// reports whether a validator was found.
func enableExternalAgentAdmission(hooks []admission.ValidatingHook, lookup func(string) string) (bool, error) {
	allowList, err := externalAgentAllowList(lookup)
	if err != nil {
		return false, err
	}
	found := false
	for i, hook := range hooks {
		if _, ok := hook.(*admission.ExternalAgentValidator); ok {
			hooks[i] = admission.NewExternalAgentValidator(admission.WithExternalAgentEgress(allowList))
			found = true
		}
	}
	return found, nil
}
