// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package collaboration

import (
	"fmt"
	"regexp"

	api "github.com/kognition-ai/kognition/api/v1alpha1"
)

var aliasPattern = regexp.MustCompile(`^[a-z]([a-z0-9-]{0,30}[a-z0-9])?$`)

// Branch is one Parallel branch.
type Branch struct {
	Member   string
	Optional bool
}

// Definition is the validated, immutable strategy configuration. The engine
// revalidates it independently of admission so that a stored definition cannot
// widen membership, interactions or limits.
type Definition struct {
	Strategy    api.CollaborationStrategy
	Members     []string
	Limits      api.CollaborationLimits
	Coordinator string
	Branches    []Branch
	Synthesizer string
	Entry       string
	MaxHandoffs int32
	Finalizer   string

	memberSet    map[string]struct{}
	interactions map[[2]string]struct{}
}

// NewDefinition validates a collaboration step and builds its Definition.
func NewDefinition(c *api.WorkflowCollaboration) (Definition, error) {
	if c == nil {
		return Definition{}, definitionErr("collaboration is required")
	}
	var errs []string
	d := Definition{
		Strategy:     c.Strategy,
		Limits:       c.Limits,
		memberSet:    map[string]struct{}{},
		interactions: map[[2]string]struct{}{},
	}
	if c.Protocol != api.CollaborationProtocolA2A {
		errs = append(errs, fmt.Sprintf("protocol %q is unsupported", c.Protocol))
	}
	if n := len(c.Members); n < 2 || n > api.CollaborationMaxMembers {
		errs = append(errs, fmt.Sprintf("members must contain 2 to %d entries", api.CollaborationMaxMembers))
	}
	for _, m := range c.Members {
		if problem := api.CollaborationTextProblem(m.Description, api.CollaborationMaxMemberDescriptionChars, false); problem != "" {
			errs = append(errs, fmt.Sprintf("member %q description %s", m.Name, problem))
		}
		if !aliasPattern.MatchString(m.Name) {
			errs = append(errs, fmt.Sprintf("member alias %q is invalid", m.Name))
			continue
		}
		if _, dup := d.memberSet[m.Name]; dup {
			errs = append(errs, fmt.Sprintf("member alias %q is duplicated", m.Name))
			continue
		}
		d.memberSet[m.Name] = struct{}{}
		d.Members = append(d.Members, m.Name)
	}
	if problem := api.CollaborationTextProblem(c.Guidance, api.CollaborationMaxGuidanceChars, true); problem != "" {
		errs = append(errs, "guidance "+problem)
	}
	if len(c.AllowedInteractions) > api.CollaborationMaxInteractions {
		errs = append(errs, "allowedInteractions exceeds the reviewed ceiling")
	}
	for _, in := range c.AllowedInteractions {
		if !d.isMember(in.From) || !d.isMember(in.To) {
			errs = append(errs, fmt.Sprintf("interaction %q -> %q references an unknown member", in.From, in.To))
			continue
		}
		if in.From == in.To {
			errs = append(errs, fmt.Sprintf("interaction %q is a self-interaction", in.From))
			continue
		}
		d.interactions[[2]string{in.From, in.To}] = struct{}{}
	}
	check := func(name string, v, max int64) {
		if v < 1 || v > max {
			errs = append(errs, fmt.Sprintf("limits.%s must be between 1 and %d", name, max))
		}
	}
	l := c.Limits
	check("maxTurns", int64(l.MaxTurns), api.CollaborationMaxTurns)
	check("maxMessages", int64(l.MaxMessages), api.CollaborationMaxMessages)
	check("maxDelegations", int64(l.MaxDelegations), api.CollaborationMaxDelegations)
	check("maxDelegationDepth", int64(l.MaxDelegationDepth), api.CollaborationMaxDelegationDepth)
	check("maxParallelTasks", int64(l.MaxParallelTasks), api.CollaborationMaxParallelTasks)
	if l.MaxTokens < 1 {
		errs = append(errs, "limits.maxTokens must be positive")
	}
	if l.MaxContentBytes < 1 {
		errs = append(errs, "limits.maxContentBytes must be positive")
	}

	blocks := 0
	for _, present := range []bool{c.Supervisor != nil, c.Parallel != nil, c.Handoff != nil, c.Peer != nil} {
		if present {
			blocks++
		}
	}
	if blocks != 1 {
		errs = append(errs, "exactly the selected strategy block must be present")
	}
	member := func(field, alias string) string {
		if !d.isMember(alias) {
			errs = append(errs, fmt.Sprintf("%s references unknown member %q", field, alias))
		}
		return alias
	}
	switch c.Strategy {
	case api.CollaborationStrategySupervisor:
		if c.Supervisor == nil {
			errs = append(errs, "supervisor block is required")
			break
		}
		d.Coordinator = member("supervisor.coordinator", c.Supervisor.Coordinator)
	case api.CollaborationStrategyParallel:
		if c.Parallel == nil {
			errs = append(errs, "parallel block is required")
			break
		}
		if len(c.AllowedInteractions) > 0 {
			errs = append(errs, "parallel does not allow member interactions")
		}
		if n := len(c.Parallel.Branches); n < 2 || n > api.CollaborationMaxParallelTasks {
			errs = append(errs, fmt.Sprintf("parallel.branches must contain 2 to %d entries", api.CollaborationMaxParallelTasks))
		}
		seen := map[string]struct{}{}
		for _, b := range c.Parallel.Branches {
			member("parallel.branches.member", b.Member)
			if _, dup := seen[b.Member]; dup {
				errs = append(errs, fmt.Sprintf("branch member %q is duplicated", b.Member))
			}
			seen[b.Member] = struct{}{}
			d.Branches = append(d.Branches, Branch{Member: b.Member, Optional: b.Optional})
		}
		if s := c.Parallel.Synthesizer; s != "" {
			d.Synthesizer = member("parallel.synthesizer", s)
			if _, isBranch := seen[s]; isBranch {
				errs = append(errs, "synthesizer must not also be a branch")
			}
		}
	case api.CollaborationStrategyHandoff:
		if c.Handoff == nil {
			errs = append(errs, "handoff block is required")
			break
		}
		d.Entry = member("handoff.entryMember", c.Handoff.EntryMember)
		d.MaxHandoffs = c.Handoff.MaxHandoffs
		if d.MaxHandoffs < 1 || d.MaxHandoffs > l.MaxDelegations {
			errs = append(errs, "handoff.maxHandoffs must be between 1 and limits.maxDelegations")
		}
	case api.CollaborationStrategyPeer:
		if c.Peer == nil {
			errs = append(errs, "peer block is required")
			break
		}
		d.Finalizer = member("peer.finalizer", c.Peer.Finalizer)
	default:
		errs = append(errs, fmt.Sprintf("strategy %q is unsupported", c.Strategy))
	}
	if err := joinErrs(errs); err != nil {
		return Definition{}, err
	}
	return d, nil
}

func (d *Definition) isMember(alias string) bool {
	_, ok := d.memberSet[alias]
	return ok
}

func (d *Definition) allows(from, to string) bool {
	_, ok := d.interactions[[2]string{from, to}]
	return ok
}
