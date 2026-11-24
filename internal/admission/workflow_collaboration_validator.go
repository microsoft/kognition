// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package admission

import (
	"fmt"
	"regexp"
	"time"

	"github.com/kognition-ai/kognition/api/v1alpha1"
)

// collaborationAliasPattern matches the member alias DNS label used by the CRD.
var collaborationAliasPattern = regexp.MustCompile(`^[a-z]([-a-z0-9]{0,30}[a-z0-9])?$`)

type collaborationMemberKey struct {
	namespace string
	name      string
}

// validateCollaborationStep validates a collaboration step. Structural rules
// always run so that an invalid step is reported precisely; a structurally valid
// step is still rejected while collaboration is not enabled.
func (v *WorkflowValidator) validateCollaborationStep(i int, step v1alpha1.WorkflowStep, workflowNamespace string) []string {
	path := fmt.Sprintf("spec.steps[%d(%s)]", i, step.Name)
	var errs []string

	if !v.collaborationEnabled {
		errs = append(errs, fmt.Sprintf("%s.type %q is not enabled: collaboration execution has not been qualified", path, v1alpha1.WorkflowStepTypeCollaboration))
	}
	if step.Collaboration == nil {
		return append(errs, fmt.Sprintf("%s.collaboration is required when type=%q", path, v1alpha1.WorkflowStepTypeCollaboration))
	}
	if step.AgentRef.Name != "" {
		errs = append(errs, fmt.Sprintf("%s.agentRef must be empty when type=%q", path, v1alpha1.WorkflowStepTypeCollaboration))
	}
	if step.ToolRef.Name != "" {
		errs = append(errs, fmt.Sprintf("%s.toolRef must be empty when type=%q", path, v1alpha1.WorkflowStepTypeCollaboration))
	}
	if step.Retry != nil {
		errs = append(errs, fmt.Sprintf("%s.retry is not supported for collaboration steps", path))
	}
	errs = append(errs, validateCollaborationTimeout(path, step.Timeout)...)

	c := step.Collaboration
	cpath := path + ".collaboration"

	switch c.Strategy {
	case v1alpha1.CollaborationStrategySupervisor,
		v1alpha1.CollaborationStrategyParallel,
		v1alpha1.CollaborationStrategyHandoff,
		v1alpha1.CollaborationStrategyPeer:
	default:
		errs = append(errs, fmt.Sprintf("%s.strategy %q is invalid (must be %q, %q, %q or %q)", cpath, c.Strategy,
			v1alpha1.CollaborationStrategySupervisor, v1alpha1.CollaborationStrategyParallel,
			v1alpha1.CollaborationStrategyHandoff, v1alpha1.CollaborationStrategyPeer))
	}
	if c.Protocol != v1alpha1.CollaborationProtocolA2A {
		errs = append(errs, fmt.Sprintf("%s.protocol %q is invalid (must be %q)", cpath, c.Protocol, v1alpha1.CollaborationProtocolA2A))
	}

	aliases, memberErrs := v.validateCollaborationMembers(cpath, c.Members, workflowNamespace)
	errs = append(errs, memberErrs...)

	outgoing, interactionErrs := validateCollaborationInteractions(cpath, c, aliases)
	errs = append(errs, interactionErrs...)

	errs = append(errs, validateCollaborationLimits(cpath, c.Limits)...)
	errs = append(errs, validateCollaborationStrategyBlocks(cpath, c, aliases, outgoing)...)
	return errs
}

func validateCollaborationTimeout(path, timeout string) []string {
	if timeout == "" {
		return []string{fmt.Sprintf("%s.timeout is required for collaboration steps", path)}
	}
	d, err := time.ParseDuration(timeout)
	if err != nil {
		// The generic step validation reports the malformed duration.
		return nil
	}
	if d <= 0 {
		return []string{fmt.Sprintf("%s.timeout %q must be greater than zero", path, timeout)}
	}
	if max := time.Duration(v1alpha1.CollaborationMaxStepTimeoutHours) * time.Hour; d > max {
		return []string{fmt.Sprintf("%s.timeout %q must not exceed %s", path, timeout, max)}
	}
	return nil
}

func (v *WorkflowValidator) validateCollaborationMembers(cpath string, members []v1alpha1.CollaborationMember, workflowNamespace string) (map[string]struct{}, []string) {
	var errs []string
	aliases := make(map[string]struct{}, len(members))

	if len(members) < 2 || len(members) > v1alpha1.CollaborationMaxMembers {
		errs = append(errs, fmt.Sprintf("%s.members must contain between 2 and %d members, got %d", cpath, v1alpha1.CollaborationMaxMembers, len(members)))
	}

	agents := make(map[collaborationMemberKey]string, len(members))
	externals := make(map[string]string, len(members))
	for j, m := range members {
		mpath := fmt.Sprintf("%s.members[%d]", cpath, j)
		if !collaborationAliasPattern.MatchString(m.Name) {
			errs = append(errs, fmt.Sprintf("%s.name %q must be a lowercase DNS label of at most 32 characters starting with a letter", mpath, m.Name))
		} else if _, dup := aliases[m.Name]; dup {
			errs = append(errs, fmt.Sprintf("%s.name %q is duplicated", mpath, m.Name))
		}
		aliases[m.Name] = struct{}{}

		switch {
		case m.ExternalAgentRef != nil && m.AgentRef.Name != "":
			errs = append(errs, fmt.Sprintf("%s must set exactly one of agentRef and externalAgentRef", mpath))
		case m.ExternalAgentRef != nil:
			if !v.externalAgentsEnabled {
				errs = append(errs, fmt.Sprintf("%s.externalAgentRef is not enabled: external agents have not been enabled by the operator", mpath))
			}
			if m.AgentRef.Namespace != "" {
				errs = append(errs, fmt.Sprintf("%s.agentRef must be empty when externalAgentRef is set", mpath))
			}
			if m.ExternalAgentRef.Name == "" {
				errs = append(errs, fmt.Sprintf("%s.externalAgentRef.name is required", mpath))
			} else if prev, dup := externals[m.ExternalAgentRef.Name]; dup {
				errs = append(errs, fmt.Sprintf("%s.externalAgentRef references ExternalAgent %q already used by member %q", mpath, m.ExternalAgentRef.Name, prev))
			} else {
				externals[m.ExternalAgentRef.Name] = m.Name
			}
			if m.ServiceRef != nil {
				errs = append(errs, fmt.Sprintf("%s.serviceRef is not valid with externalAgentRef", mpath))
			}
			continue
		case m.AgentRef.Name == "":
			errs = append(errs, fmt.Sprintf("%s.agentRef.name is required", mpath))
		default:
			ns := m.AgentRef.Namespace
			if ns == "" {
				ns = workflowNamespace
			}
			key := collaborationMemberKey{namespace: ns, name: m.AgentRef.Name}
			if prev, dup := agents[key]; dup {
				errs = append(errs, fmt.Sprintf("%s.agentRef references agent %q already used by member %q", mpath, m.AgentRef.Name, prev))
			} else {
				agents[key] = m.Name
			}
		}
		if m.ServiceRef != nil && m.ServiceRef.Name == "" {
			errs = append(errs, fmt.Sprintf("%s.serviceRef.name is required when serviceRef is set", mpath))
		}
	}
	return aliases, errs
}

func validateCollaborationInteractions(cpath string, c *v1alpha1.WorkflowCollaboration, aliases map[string]struct{}) (map[string]int, []string) {
	var errs []string
	outgoing := make(map[string]int, len(c.AllowedInteractions))

	if len(c.AllowedInteractions) > v1alpha1.CollaborationMaxInteractions {
		errs = append(errs, fmt.Sprintf("%s.allowedInteractions must not contain more than %d entries", cpath, v1alpha1.CollaborationMaxInteractions))
	}
	if c.Strategy == v1alpha1.CollaborationStrategyParallel {
		if len(c.AllowedInteractions) > 0 {
			errs = append(errs, fmt.Sprintf("%s.allowedInteractions must be empty for strategy %q", cpath, c.Strategy))
		}
		return outgoing, errs
	}
	if len(c.AllowedInteractions) == 0 {
		errs = append(errs, fmt.Sprintf("%s.allowedInteractions must contain at least one interaction for strategy %q", cpath, c.Strategy))
	}

	seen := make(map[v1alpha1.CollaborationInteraction]struct{}, len(c.AllowedInteractions))
	for j, in := range c.AllowedInteractions {
		ipath := fmt.Sprintf("%s.allowedInteractions[%d]", cpath, j)
		_, fromOK := aliases[in.From]
		_, toOK := aliases[in.To]
		if !fromOK {
			errs = append(errs, fmt.Sprintf("%s.from references unknown member %q", ipath, in.From))
		}
		if !toOK {
			errs = append(errs, fmt.Sprintf("%s.to references unknown member %q", ipath, in.To))
		}
		if in.From != "" && in.From == in.To {
			errs = append(errs, fmt.Sprintf("%s is a self-interaction (%q)", ipath, in.From))
		}
		if _, dup := seen[in]; dup {
			errs = append(errs, fmt.Sprintf("%s duplicates an earlier interaction (%q -> %q)", ipath, in.From, in.To))
		}
		seen[in] = struct{}{}
		if fromOK && toOK && in.From != in.To {
			outgoing[in.From]++
		}
	}
	return outgoing, errs
}

func validateCollaborationLimits(cpath string, l v1alpha1.CollaborationLimits) []string {
	var errs []string
	check := func(name string, value int64, max int64) {
		if value < 1 || value > max {
			errs = append(errs, fmt.Sprintf("%s.limits.%s must be between 1 and %d, got %d", cpath, name, max, value))
		}
	}
	check("maxTurns", int64(l.MaxTurns), v1alpha1.CollaborationMaxTurns)
	check("maxMessages", int64(l.MaxMessages), v1alpha1.CollaborationMaxMessages)
	check("maxDelegations", int64(l.MaxDelegations), v1alpha1.CollaborationMaxDelegations)
	check("maxDelegationDepth", int64(l.MaxDelegationDepth), v1alpha1.CollaborationMaxDelegationDepth)
	check("maxParallelTasks", int64(l.MaxParallelTasks), v1alpha1.CollaborationMaxParallelTasks)
	if l.MaxTokens < 1 {
		errs = append(errs, fmt.Sprintf("%s.limits.maxTokens must be greater than zero, got %d", cpath, l.MaxTokens))
	}
	return errs
}

func validateCollaborationStrategyBlocks(cpath string, c *v1alpha1.WorkflowCollaboration, aliases map[string]struct{}, outgoing map[string]int) []string {
	var errs []string

	present := map[v1alpha1.CollaborationStrategy]bool{
		v1alpha1.CollaborationStrategySupervisor: c.Supervisor != nil,
		v1alpha1.CollaborationStrategyParallel:   c.Parallel != nil,
		v1alpha1.CollaborationStrategyHandoff:    c.Handoff != nil,
		v1alpha1.CollaborationStrategyPeer:       c.Peer != nil,
	}
	for _, s := range []v1alpha1.CollaborationStrategy{
		v1alpha1.CollaborationStrategySupervisor,
		v1alpha1.CollaborationStrategyParallel,
		v1alpha1.CollaborationStrategyHandoff,
		v1alpha1.CollaborationStrategyPeer,
	} {
		block := blockName(s)
		switch {
		case s == c.Strategy && !present[s]:
			errs = append(errs, fmt.Sprintf("%s.%s is required when strategy=%q", cpath, block, s))
		case s != c.Strategy && present[s]:
			errs = append(errs, fmt.Sprintf("%s.%s must be empty unless strategy=%q", cpath, block, s))
		}
	}

	member := func(field, alias string) bool {
		if _, ok := aliases[alias]; !ok {
			errs = append(errs, fmt.Sprintf("%s.%s references unknown member %q", cpath, field, alias))
			return false
		}
		return true
	}

	if s := c.Supervisor; s != nil {
		if member("supervisor.coordinator", s.Coordinator) && outgoing[s.Coordinator] == 0 {
			errs = append(errs, fmt.Sprintf("%s.supervisor.coordinator %q must have at least one outgoing allowed interaction", cpath, s.Coordinator))
		}
	}

	if p := c.Parallel; p != nil {
		if len(p.Branches) < 2 {
			errs = append(errs, fmt.Sprintf("%s.parallel.branches must contain at least 2 branches, got %d", cpath, len(p.Branches)))
		}
		if len(p.Branches) > v1alpha1.CollaborationMaxParallelTasks {
			errs = append(errs, fmt.Sprintf("%s.parallel.branches must not contain more than %d branches, got %d", cpath, v1alpha1.CollaborationMaxParallelTasks, len(p.Branches)))
		}
		branches := make(map[string]struct{}, len(p.Branches))
		for j, b := range p.Branches {
			if !member(fmt.Sprintf("parallel.branches[%d].member", j), b.Member) {
				continue
			}
			if _, dup := branches[b.Member]; dup {
				errs = append(errs, fmt.Sprintf("%s.parallel.branches[%d].member %q is duplicated", cpath, j, b.Member))
			}
			branches[b.Member] = struct{}{}
		}
		if p.Synthesizer != "" && member("parallel.synthesizer", p.Synthesizer) {
			if _, isBranch := branches[p.Synthesizer]; isBranch {
				errs = append(errs, fmt.Sprintf("%s.parallel.synthesizer %q must not also be a branch", cpath, p.Synthesizer))
			}
		}
	}

	if h := c.Handoff; h != nil {
		if member("handoff.entryMember", h.EntryMember) && outgoing[h.EntryMember] == 0 {
			errs = append(errs, fmt.Sprintf("%s.handoff.entryMember %q must have at least one outgoing allowed interaction", cpath, h.EntryMember))
		}
		if h.MaxHandoffs < 1 || h.MaxHandoffs > c.Limits.MaxDelegations {
			errs = append(errs, fmt.Sprintf("%s.handoff.maxHandoffs must be between 1 and limits.maxDelegations (%d), got %d", cpath, c.Limits.MaxDelegations, h.MaxHandoffs))
		}
	}

	if p := c.Peer; p != nil {
		member("peer.finalizer", p.Finalizer)
	}
	return errs
}

func blockName(s v1alpha1.CollaborationStrategy) string {
	switch s {
	case v1alpha1.CollaborationStrategySupervisor:
		return "supervisor"
	case v1alpha1.CollaborationStrategyParallel:
		return "parallel"
	case v1alpha1.CollaborationStrategyHandoff:
		return "handoff"
	default:
		return "peer"
	}
}
