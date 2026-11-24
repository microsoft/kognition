// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package v1alpha1

import (
	"fmt"
	"net"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// ExternalAgentProtocol identifies the protocol used to reach an external agent.
type ExternalAgentProtocol string

const (
	// ExternalAgentProtocolA2A is the Agent2Agent protocol. It is the only
	// protocol supported in v1.
	ExternalAgentProtocolA2A ExternalAgentProtocol = "A2A"
)

// ExternalAgent limit defaults and ceilings. A value above its ceiling is
// rejected at admission; an unset value takes the default.
const (
	ExternalAgentDefaultMaxTokensPerTask   int64 = 8192
	ExternalAgentMaxTokensPerTaskCeiling   int64 = 131072
	ExternalAgentDefaultTaskTimeout              = 5 * time.Minute
	ExternalAgentTaskTimeoutCeiling              = 30 * time.Minute
	ExternalAgentTaskTimeoutFloor                = time.Second
	ExternalAgentDefaultMaxRequestBytes    int64 = 64 * 1024
	ExternalAgentMaxRequestBytesCeiling    int64 = 1024 * 1024
	ExternalAgentDefaultMaxResponseBytes   int64 = 256 * 1024
	ExternalAgentMaxResponseBytesCeiling   int64 = 4 * 1024 * 1024
	ExternalAgentDefaultMaxConcurrentTasks int32 = 2
	ExternalAgentMaxConcurrentTasksCeiling int32 = 16
	externalAgentMaxSkills                       = 32
	externalAgentMaxSkillLength                  = 128
)

// ExternalAgent condition types.
const (
	ExternalAgentConditionReady      = "Ready"
	ExternalAgentConditionCardPinned = "CardPinned"
	ExternalAgentConditionReachable  = "Reachable"
	ExternalAgentConditionEgress     = "EgressAllowed"
)

var (
	externalAgentCardDigestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	externalAgentHostLabelPattern  = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)
	externalAgentNamePattern       = regexp.MustCompile(`^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$`)
)

// ExternalAgent describes a remote agent that a workflow can use as a
// collaboration member or step target. Kognition does not run, scale, or
// meter an external agent; it is a trust boundary and is governed as one.
type ExternalAgent struct {
	TypeMeta   `json:",inline" yaml:",inline"`
	ObjectMeta `json:"metadata" yaml:"metadata"`
	Spec       ExternalAgentSpec   `json:"spec" yaml:"spec"`
	Status     ExternalAgentStatus `json:"status,omitempty" yaml:"status,omitempty"`
}

func (e *ExternalAgent) GetTypeMeta() TypeMeta      { return e.TypeMeta }
func (e *ExternalAgent) GetObjectMeta() *ObjectMeta { return &e.ObjectMeta }

// ExternalAgentSpec defines the remote endpoint, its pinned card and limits.
type ExternalAgentSpec struct {
	// Protocol is required. Only A2A is supported.
	Protocol ExternalAgentProtocol `json:"protocol" yaml:"protocol"`

	// Endpoint is a static DNS host:port. TLS is always used.
	Endpoint string `json:"endpoint" yaml:"endpoint"`

	// Card pins the Agent Card the platform will accept.
	Card ExternalAgentCard `json:"card" yaml:"card"`

	// CredentialRef names a SecretRef in the same namespace that holds a bearer token.
	CredentialRef LocalObjectReference `json:"credentialRef" yaml:"credentialRef"`

	// Limits bound every task sent to the agent.
	Limits *ExternalAgentLimits `json:"limits,omitempty" yaml:"limits,omitempty"`
}

// ExternalAgentCard pins the canonical Agent Card.
type ExternalAgentCard struct {
	// Digest is "sha256:" plus the lowercase hex SHA-256 of the RFC 8785
	// canonical form of the Agent Card with its signatures removed.
	Digest string `json:"digest" yaml:"digest"`

	// Skills optionally restricts the card skills the platform will use.
	Skills []string `json:"skills,omitempty" yaml:"skills,omitempty"`
}

// ExternalAgentLimits bounds one task. Zero values take the platform defaults.
type ExternalAgentLimits struct {
	MaxTokensPerTask   int64    `json:"maxTokensPerTask,omitempty" yaml:"maxTokensPerTask,omitempty"`
	TaskTimeout        Duration `json:"taskTimeout,omitempty" yaml:"taskTimeout,omitempty"`
	MaxRequestBytes    int64    `json:"maxRequestBytes,omitempty" yaml:"maxRequestBytes,omitempty"`
	MaxResponseBytes   int64    `json:"maxResponseBytes,omitempty" yaml:"maxResponseBytes,omitempty"`
	MaxConcurrentTasks int32    `json:"maxConcurrentTasks,omitempty" yaml:"maxConcurrentTasks,omitempty"`
}

// EffectiveExternalAgentLimits is ExternalAgentLimits with every default applied.
type EffectiveExternalAgentLimits struct {
	MaxTokensPerTask   int64
	TaskTimeout        time.Duration
	MaxRequestBytes    int64
	MaxResponseBytes   int64
	MaxConcurrentTasks int32
}

// ExternalAgentStatus is the observed state of an ExternalAgent.
type ExternalAgentStatus struct {
	// ObservedCardDigest is the digest of the card last fetched from the endpoint.
	ObservedCardDigest string `json:"observedCardDigest,omitempty" yaml:"observedCardDigest,omitempty"`

	// ObservedGeneration is the spec generation the status reflects.
	ObservedGeneration int64 `json:"observedGeneration,omitempty" yaml:"observedGeneration,omitempty"`

	// Conditions are Ready, CardPinned, Reachable and EgressAllowed.
	Conditions []Condition `json:"conditions,omitempty" yaml:"conditions,omitempty"`
}

// Effective returns the limits with defaults applied.
func (l *ExternalAgentLimits) Effective() EffectiveExternalAgentLimits {
	out := EffectiveExternalAgentLimits{
		MaxTokensPerTask:   ExternalAgentDefaultMaxTokensPerTask,
		TaskTimeout:        ExternalAgentDefaultTaskTimeout,
		MaxRequestBytes:    ExternalAgentDefaultMaxRequestBytes,
		MaxResponseBytes:   ExternalAgentDefaultMaxResponseBytes,
		MaxConcurrentTasks: ExternalAgentDefaultMaxConcurrentTasks,
	}
	if l == nil {
		return out
	}
	if l.MaxTokensPerTask > 0 {
		out.MaxTokensPerTask = l.MaxTokensPerTask
	}
	if l.TaskTimeout.Duration > 0 {
		out.TaskTimeout = l.TaskTimeout.Duration
	}
	if l.MaxRequestBytes > 0 {
		out.MaxRequestBytes = l.MaxRequestBytes
	}
	if l.MaxResponseBytes > 0 {
		out.MaxResponseBytes = l.MaxResponseBytes
	}
	if l.MaxConcurrentTasks > 0 {
		out.MaxConcurrentTasks = l.MaxConcurrentTasks
	}
	return out
}

// Validate validates the spec. It is purely syntactic; resolved-address and
// allow-list checks need the network and the operator configuration and are
// performed by the admission validator and the transport.
func (s ExternalAgentSpec) Validate() error {
	if s.Protocol == "" {
		return fmt.Errorf("externalAgent.spec.protocol is required")
	}
	if s.Protocol != ExternalAgentProtocolA2A {
		return fmt.Errorf("externalAgent.spec.protocol must be A2A")
	}
	if _, _, err := ParseExternalAgentEndpoint(s.Endpoint); err != nil {
		return fmt.Errorf("externalAgent.spec.endpoint %w", err)
	}
	if !externalAgentCardDigestPattern.MatchString(s.Card.Digest) {
		return fmt.Errorf("externalAgent.spec.card.digest must be sha256:<64 lowercase hex characters>")
	}
	if len(s.Card.Skills) > externalAgentMaxSkills {
		return fmt.Errorf("externalAgent.spec.card.skills allows at most %d entries", externalAgentMaxSkills)
	}
	seen := make(map[string]bool, len(s.Card.Skills))
	for i, skill := range s.Card.Skills {
		if skill == "" || len(skill) > externalAgentMaxSkillLength {
			return fmt.Errorf("externalAgent.spec.card.skills[%d] must be 1-%d characters", i, externalAgentMaxSkillLength)
		}
		if seen[skill] {
			return fmt.Errorf("externalAgent.spec.card.skills[%d] duplicate skill %q", i, skill)
		}
		seen[skill] = true
	}
	if s.CredentialRef.Name == "" {
		return fmt.Errorf("externalAgent.spec.credentialRef.name is required")
	}
	if len(s.CredentialRef.Name) > 253 || !externalAgentNamePattern.MatchString(s.CredentialRef.Name) {
		return fmt.Errorf("externalAgent.spec.credentialRef.name must be a DNS-1123 subdomain")
	}
	return s.Limits.validate()
}

func (l *ExternalAgentLimits) validate() error {
	if l == nil {
		return nil
	}
	if l.MaxTokensPerTask < 0 || l.MaxTokensPerTask > ExternalAgentMaxTokensPerTaskCeiling {
		return fmt.Errorf("externalAgent.spec.limits.maxTokensPerTask must be between 0 and %d", ExternalAgentMaxTokensPerTaskCeiling)
	}
	if l.TaskTimeout.Duration != 0 && (l.TaskTimeout.Duration < ExternalAgentTaskTimeoutFloor || l.TaskTimeout.Duration > ExternalAgentTaskTimeoutCeiling) {
		return fmt.Errorf("externalAgent.spec.limits.taskTimeout must be between %s and %s", ExternalAgentTaskTimeoutFloor, ExternalAgentTaskTimeoutCeiling)
	}
	if l.MaxRequestBytes < 0 || l.MaxRequestBytes > ExternalAgentMaxRequestBytesCeiling {
		return fmt.Errorf("externalAgent.spec.limits.maxRequestBytes must be between 0 and %d", ExternalAgentMaxRequestBytesCeiling)
	}
	if l.MaxResponseBytes < 0 || l.MaxResponseBytes > ExternalAgentMaxResponseBytesCeiling {
		return fmt.Errorf("externalAgent.spec.limits.maxResponseBytes must be between 0 and %d", ExternalAgentMaxResponseBytesCeiling)
	}
	if l.MaxConcurrentTasks < 0 || l.MaxConcurrentTasks > ExternalAgentMaxConcurrentTasksCeiling {
		return fmt.Errorf("externalAgent.spec.limits.maxConcurrentTasks must be between 0 and %d", ExternalAgentMaxConcurrentTasksCeiling)
	}
	return nil
}

// ParseExternalAgentEndpoint splits a static "host:port" endpoint. It rejects
// schemes, userinfo, paths, queries, IP literals (including bracketed IPv6),
// numeric-only hosts, single-label hosts, and ports outside 1-65535. The
// returned host is lower case.
func ParseExternalAgentEndpoint(endpoint string) (host string, port int, err error) {
	if endpoint == "" {
		return "", 0, fmt.Errorf("is required")
	}
	if strings.ContainsAny(endpoint, "/\\?#@ \t\r\n[]") || strings.Contains(endpoint, "://") {
		return "", 0, fmt.Errorf("must be host:port without scheme, userinfo, path, query or brackets")
	}
	h, p, splitErr := net.SplitHostPort(endpoint)
	if splitErr != nil {
		return "", 0, fmt.Errorf("must be host:port: %v", splitErr)
	}
	n, convErr := strconv.Atoi(p)
	if convErr != nil || n < 1 || n > 65535 {
		return "", 0, fmt.Errorf("port must be between 1 and 65535")
	}
	h = strings.ToLower(strings.TrimSuffix(h, "."))
	if h == "" || len(h) > 253 {
		return "", 0, fmt.Errorf("host must be a DNS name of at most 253 characters")
	}
	if net.ParseIP(h) != nil {
		return "", 0, fmt.Errorf("host must be a DNS name, not an IP address")
	}
	labels := strings.Split(h, ".")
	if len(labels) < 2 {
		return "", 0, fmt.Errorf("host must be a fully qualified DNS name")
	}
	allNumeric := true
	for _, label := range labels {
		if len(label) == 0 || len(label) > 63 || !externalAgentHostLabelPattern.MatchString(label) {
			return "", 0, fmt.Errorf("host %q is not a valid DNS name", h)
		}
		if strings.Trim(label, "0123456789") != "" {
			allNumeric = false
		}
	}
	if allNumeric {
		return "", 0, fmt.Errorf("host must be a DNS name, not an IP address")
	}
	return h, n, nil
}

// ExternalAgentGrantVerb identifies an action permitted by an ExternalAgentGrant.
type ExternalAgentGrantVerb string

const (
	// ExternalAgentGrantVerbInvoke permits sending tasks to the external agent.
	ExternalAgentGrantVerbInvoke ExternalAgentGrantVerb = "invoke"
)

// ExternalAgentGrant authorizes a Workflow to use an ExternalAgent. There is
// no implicit access: without a live grant the platform never sends a task.
type ExternalAgentGrant struct {
	TypeMeta   `json:",inline" yaml:",inline"`
	ObjectMeta `json:"metadata" yaml:"metadata"`
	Spec       ExternalAgentGrantSpec `json:"spec" yaml:"spec"`
}

func (g *ExternalAgentGrant) GetTypeMeta() TypeMeta      { return g.TypeMeta }
func (g *ExternalAgentGrant) GetObjectMeta() *ObjectMeta { return &g.ObjectMeta }

// ExternalAgentGrantSpec selects which Workflows may use which ExternalAgent.
type ExternalAgentGrantSpec struct {
	// ExternalAgentRef is the ExternalAgent in the grant's namespace.
	ExternalAgentRef LocalObjectReference `json:"externalAgentRef" yaml:"externalAgentRef"`

	// WorkflowRef directly names a Workflow. Exactly one of WorkflowRef and
	// WorkflowSelector must be set.
	WorkflowRef LocalObjectReference `json:"workflowRef,omitempty" yaml:"workflowRef,omitempty"`

	// WorkflowSelector selects Workflows by label (matchLabels semantics). It
	// must contain at least one label.
	WorkflowSelector map[string]string `json:"workflowSelector,omitempty" yaml:"workflowSelector,omitempty"`

	// Verbs is the non-empty set of granted actions; only invoke exists.
	Verbs []ExternalAgentGrantVerb `json:"verbs" yaml:"verbs"`

	// Conditions optionally time-bounds the grant.
	Conditions *ExternalAgentGrantConditions `json:"conditions,omitempty" yaml:"conditions,omitempty"`
}

// ExternalAgentGrantConditions time-bounds a grant.
type ExternalAgentGrantConditions struct {
	ValidFrom  string `json:"validFrom,omitempty" yaml:"validFrom,omitempty"`
	ValidUntil string `json:"validUntil,omitempty" yaml:"validUntil,omitempty"`
}

// Validate validates the spec.
func (s ExternalAgentGrantSpec) Validate() error {
	if s.ExternalAgentRef.Name == "" {
		return fmt.Errorf("externalAgentGrant.spec.externalAgentRef.name is required")
	}
	hasRef := s.WorkflowRef.Name != ""
	hasSelector := len(s.WorkflowSelector) > 0
	if hasRef == hasSelector {
		return fmt.Errorf("externalAgentGrant.spec requires exactly one of workflowRef and workflowSelector")
	}
	for k, v := range s.WorkflowSelector {
		if k == "" {
			return fmt.Errorf("externalAgentGrant.spec.workflowSelector keys must be non-empty")
		}
		if len(k) > 253 || len(v) > 63 {
			return fmt.Errorf("externalAgentGrant.spec.workflowSelector entry %q is too long", k)
		}
	}
	if len(s.Verbs) == 0 {
		return fmt.Errorf("externalAgentGrant.spec.verbs must be non-empty")
	}
	seen := make(map[ExternalAgentGrantVerb]bool, len(s.Verbs))
	for i, v := range s.Verbs {
		if v != ExternalAgentGrantVerbInvoke {
			return fmt.Errorf("externalAgentGrant.spec.verbs[%d] must be invoke", i)
		}
		if seen[v] {
			return fmt.Errorf("externalAgentGrant.spec.verbs[%d] duplicate verb %q", i, v)
		}
		seen[v] = true
	}
	if _, _, err := s.Conditions.window(); err != nil {
		return err
	}
	return nil
}

func (c *ExternalAgentGrantConditions) window() (from, until time.Time, err error) {
	if c == nil {
		return time.Time{}, time.Time{}, nil
	}
	if c.ValidFrom != "" {
		from, err = time.Parse(time.RFC3339, c.ValidFrom)
		if err != nil {
			return time.Time{}, time.Time{}, fmt.Errorf("externalAgentGrant.spec.conditions.validFrom must be RFC3339: %w", err)
		}
	}
	if c.ValidUntil != "" {
		until, err = time.Parse(time.RFC3339, c.ValidUntil)
		if err != nil {
			return time.Time{}, time.Time{}, fmt.Errorf("externalAgentGrant.spec.conditions.validUntil must be RFC3339: %w", err)
		}
		if !from.IsZero() && !until.After(from) {
			return time.Time{}, time.Time{}, fmt.Errorf("externalAgentGrant.spec.conditions.validUntil must be after validFrom")
		}
	}
	return from, until, nil
}

// Permits reports whether this grant allows the named Workflow (with the given
// labels) to invoke the named ExternalAgent at the given instant. A grant with
// an invalid time window permits nothing.
func (s ExternalAgentGrantSpec) Permits(externalAgent, workflow string, workflowLabels map[string]string, now time.Time) bool {
	if s.ExternalAgentRef.Name != externalAgent || externalAgent == "" {
		return false
	}
	hasVerb := false
	for _, v := range s.Verbs {
		if v == ExternalAgentGrantVerbInvoke {
			hasVerb = true
		}
	}
	if !hasVerb {
		return false
	}
	if s.WorkflowRef.Name != "" {
		if len(s.WorkflowSelector) > 0 || s.WorkflowRef.Name != workflow {
			return false
		}
	} else {
		if len(s.WorkflowSelector) == 0 {
			return false
		}
		for k, v := range s.WorkflowSelector {
			if got, ok := workflowLabels[k]; !ok || got != v {
				return false
			}
		}
	}
	from, until, err := s.Conditions.window()
	if err != nil {
		return false
	}
	if !from.IsZero() && now.Before(from) {
		return false
	}
	if !until.IsZero() && !now.Before(until) {
		return false
	}
	return true
}
