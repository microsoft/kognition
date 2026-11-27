// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package externaltransport

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"

	api "github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/collaboration"
	"github.com/kognition-ai/kognition/internal/collaboration/a2atransport"
	"github.com/kognition-ai/kognition/internal/collaboration/driver"
	"github.com/kognition-ai/kognition/internal/controller"
	"github.com/kognition-ai/kognition/internal/externalagent"
	"github.com/kognition-ai/kognition/internal/store"
)

// Failure reasons recorded when an external member's task does not yield a result.
const (
	// FailureNotSent reports that no contact was made, so nothing ran remotely.
	FailureNotSent = "NotSent"
	// FailureSendUnknown reports that a send may have reached the agent but no
	// task could be observed. The dispatch is never resent.
	FailureSendUnknown = "SendOutcomeUnknown"
	// FailureTimedOut reports that the task outlived the ExternalAgent taskTimeout.
	FailureTimedOut = "MemberTimedOut"
	// FailureAccessRevoked reports that contact stopped being permitted mid-run.
	FailureAccessRevoked = "AccessRevoked"
)

const (
	reasonAuthorized = "authorized"
	reasonCompleted  = "target_completed"
	reasonFailed     = "target_failed"
	// observeTimeout bounds one poll of the remote agent.
	observeTimeout = 30 * time.Second
	// slotGrace extends a limiter reservation past the task timeout.
	slotGrace = time.Minute
)

// Secret is a credential. It never prints, marshals or logs its value; only
// Reveal returns it, and only the remote client calls Reveal.
type Secret string

// String implements fmt.Stringer.
func (Secret) String() string { return "[redacted]" }

// GoString implements fmt.GoStringer.
func (Secret) GoString() string { return "externaltransport.Secret([redacted])" }

// MarshalText implements encoding.TextMarshaler, so JSON and YAML never see the value.
func (Secret) MarshalText() ([]byte, error) { return []byte("[redacted]"), nil }

// Reveal returns the credential for the one connection it is meant for.
func (s Secret) Reveal() string { return string(s) }

// CredentialResolver resolves the bearer token held by a SecretRef. It is the
// same resolver the gateways use, so no new Secret access is introduced.
type CredentialResolver interface {
	ResolveAPIKey(ctx context.Context, namespace, name string) (string, error)
}

// Call identifies one contact with a pinned endpoint.
type Call struct {
	Namespace string
	Agent     string
	Host      string
	Port      int
	// Addrs are the addresses Host resolved to when the contact was authorized.
	Addrs []netip.Addr
	// CardDigest is the pinned Agent Card digest the client must verify.
	CardDigest string
	// Token must be attached to the connection to Host:Port and nowhere else.
	Token  Secret
	Limits api.EffectiveExternalAgentLimits
}

// Client performs the remote A2A calls. Implementations enforce the pinned card,
// the response size limit and the connection rules; the transport owns policy.
type Client interface {
	// Send creates a task from text and returns its identifier. messageID is
	// stable per dispatch so the remote agent can deduplicate.
	Send(ctx context.Context, call Call, messageID, text string) (taskID string, err error)
	GetTask(ctx context.Context, call Call, taskID string) (*a2a.Task, error)
	Cancel(ctx context.Context, call Call, taskID string) error
}

// Config configures a Transport for one collaboration activation.
type Config struct {
	Store store.Store
	// Run identifies the activation. Run.OwnerKind and Run.RunName name the
	// Workflow or WorkflowRun whose grants govern the contact.
	Run controller.WorkflowCollaborationRun
	// Members maps a member alias to the ExternalAgent it stands for.
	Members     map[string]string
	Allow       *externalagent.AllowList
	Resolver    externalagent.Resolver
	Credentials CredentialResolver
	Client      Client
	Handles     a2atransport.HandleReplacer
	Audit       *Auditor
	// Limiter is shared by every activation of the process. A private one is used when nil.
	Limiter *Limiter
	Now     func() time.Time
}

// Transport implements driver.Transport for ExternalAgent members.
type Transport struct {
	cfg  Config
	gate Gate
}

var _ driver.Transport = (*Transport)(nil)

// New validates the configuration. Every dependency that carries a security
// decision is required: without one the transport cannot be made to fail open.
func New(cfg Config) (*Transport, error) {
	run := cfg.Run
	if cfg.Store == nil || run.Spec == nil || run.Namespace == "" || run.RunName == "" || run.OwnerKind == "" {
		return nil, errors.New("externaltransport: store, run identity, owner kind and collaboration spec are required")
	}
	if cfg.Resolver == nil || cfg.Credentials == nil || cfg.Client == nil || cfg.Handles == nil || cfg.Audit == nil || cfg.Audit.Log == nil {
		return nil, errors.New("externaltransport: resolver, credentials, client, handles and audit log are required")
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Limiter == nil {
		cfg.Limiter = NewLimiter()
	}
	gate := Gate{
		Store: cfg.Store, Allow: cfg.Allow, Resolver: cfg.Resolver, Now: cfg.Now,
		Namespace: run.Namespace, OwnerKind: run.OwnerKind, RunName: run.RunName,
	}
	return &Transport{cfg: cfg, gate: gate}, nil
}

func refused(reason string) error { return fmt.Errorf("%w: %s", driver.ErrRefused, reason) }

// Admit implements driver.Transport. It is the dispatch-admission fence: it
// refuses, before any commit, a dispatch that policy forbids.
func (t *Transport) Admit(ctx context.Context, msg collaboration.Message) error {
	agent, ok := t.cfg.Members[msg.Recipient]
	if !ok {
		return refused(fmt.Sprintf("unknown external member %q", msg.Recipient))
	}
	cancel := msg.Kind == collaboration.DispatchCancel
	target, err := t.gate.Authorize(ctx, agent, AuthorizeOptions{SkipGrant: cancel})
	if err != nil {
		return t.refuse(ctx, msg, agent, err)
	}
	if !cancel {
		if int64(len(t.prompt(msg))) > target.Limits.MaxRequestBytes {
			return t.refuse(ctx, msg, agent, deny(reasonPolicy, "request exceeds the ExternalAgent maxRequestBytes limit"))
		}
	}
	if _, err := t.credential(ctx, target); err != nil {
		return t.refuse(ctx, msg, agent, deny(reasonPolicy, "ExternalAgent credential is unavailable"))
	}
	if cancel {
		return nil
	}
	if !t.cfg.Limiter.Reserve(t.slotKey(agent), t.reservationID(msg.DispatchID), target.Limits.MaxConcurrentTasks, t.cfg.Now(), target.Limits.TaskTimeout+slotGrace) {
		return fmt.Errorf("%w: ExternalAgent %q already has %d tasks in flight", driver.ErrDeferred, agent, target.Limits.MaxConcurrentTasks)
	}
	return nil
}

// refuse audits a policy denial and returns it as a refusal. An error that is
// not a denial leaves the dispatch undecided.
func (t *Transport) refuse(ctx context.Context, msg collaboration.Message, agent string, cause error) error {
	var denial *Denial
	if !errors.As(cause, &denial) {
		return cause
	}
	if err := t.record(ctx, msg, agent, Decision{
		Outcome: denial.Outcome, ReasonCode: denial.Code, Reason: denial.Reason, Execution: "not_started",
	}); err != nil {
		return err
	}
	return refused(denial.Reason)
}

// Send implements driver.Transport. Every check is repeated, so a grant revoked
// after Admit stops the contact. An ambiguous send is never repeated: the intent
// is persisted before contact and the task handle after it.
func (t *Transport) Send(ctx context.Context, msg collaboration.Message) error {
	agent, ok := t.cfg.Members[msg.Recipient]
	if !ok {
		return fmt.Errorf("externaltransport: unknown external member %q", msg.Recipient)
	}
	if msg.Kind == collaboration.DispatchCancel {
		return t.sendCancel(ctx, msg, agent)
	}
	target, err := t.gate.Authorize(ctx, agent, AuthorizeOptions{Revalidate: true})
	if err != nil {
		return t.notSent(ctx, msg, agent, err)
	}
	text := t.prompt(msg)
	if int64(len(text)) > target.Limits.MaxRequestBytes {
		return t.notSent(ctx, msg, agent, deny(reasonPolicy, "request exceeds the ExternalAgent maxRequestBytes limit"))
	}
	token, err := t.credential(ctx, target)
	if err != nil {
		return t.notSent(ctx, msg, agent, deny(reasonPolicy, "ExternalAgent credential is unavailable"))
	}
	if err := t.record(ctx, msg, agent, Decision{
		Outcome: "allowed", ReasonCode: reasonAuthorized, Grant: target.Grant, Execution: "not_started", BeforeEffect: true,
	}); err != nil {
		return t.notSent(ctx, msg, agent, err)
	}
	pending := encodeHandle(handlePending, "", t.cfg.Now())
	if err := t.cfg.Handles.Put(ctx, msg.DispatchID, pending); err != nil {
		return fmt.Errorf("externaltransport: persist send intent: %w", err)
	}
	call := callFor(t.gate.Namespace, target, token)
	taskID, err := t.cfg.Client.Send(ctx, call, msg.DispatchID, text)
	if err != nil {
		if errors.Is(err, ErrNotSent) {
			// Nothing reached the agent: settle as not sent instead of waiting.
			return t.notSentAfter(ctx, msg, agent, pending, scrub(err, token))
		}
		return scrub(err, token)
	}
	if err := t.cfg.Handles.Replace(ctx, msg.DispatchID, pending, encodeHandle(handleTask, taskID, t.cfg.Now())); err != nil {
		// The task exists but cannot be tracked: ask the agent to stop it.
		_ = t.cfg.Client.Cancel(ctx, call, taskID)
		return fmt.Errorf("externaltransport: persist task handle: %w", err)
	}
	return nil
}

// notSent records that no contact was made and returns cause. The durable
// marker lets Observe settle the dispatch truthfully instead of waiting.
func (t *Transport) notSent(ctx context.Context, msg collaboration.Message, agent string, cause error) error {
	return t.notSentAfter(ctx, msg, agent, "", cause)
}

// notSentAfter is notSent for a dispatch whose send intent, pending, was already
// recorded; the marker then replaces the intent.
func (t *Transport) notSentAfter(ctx context.Context, msg collaboration.Message, agent, pending string, cause error) error {
	t.cfg.Limiter.Release(t.slotKey(agent), t.reservationID(msg.DispatchID))
	var denial *Denial
	if errors.As(cause, &denial) {
		if err := t.record(ctx, msg, agent, Decision{
			Outcome: denial.Outcome, ReasonCode: denial.Code, Reason: denial.Reason, Execution: "not_started",
		}); err != nil {
			return err
		}
	}
	marker := encodeHandle(handleNotSent, "", t.cfg.Now())
	var err error
	if pending == "" {
		err = t.cfg.Handles.Put(ctx, msg.DispatchID, marker)
	} else {
		err = t.cfg.Handles.Replace(ctx, msg.DispatchID, pending, marker)
	}
	if err != nil {
		return fmt.Errorf("externaltransport: persist not-sent marker: %w", err)
	}
	return cause
}

// sendCancel asks the agent to stop the task of the dispatch being cancelled. It
// does not need a grant: revoking access must not stop the platform from asking
// a remote agent to stop work it was already given.
func (t *Transport) sendCancel(ctx context.Context, msg collaboration.Message, agent string) error {
	if msg.Target == nil {
		return errors.New("externaltransport: cancel has no target dispatch")
	}
	kind, taskID, _, ok, err := t.handle(ctx, msg.Target.DispatchID)
	if err != nil || !ok || kind != handleTask {
		return err // no remote task is known, so there is nothing to cancel
	}
	target, err := t.gate.Authorize(ctx, agent, AuthorizeOptions{SkipGrant: true})
	if err != nil {
		var denial *Denial
		if errors.As(err, &denial) {
			if recErr := t.record(ctx, msg, agent, Decision{Outcome: denial.Outcome, ReasonCode: denial.Code, Reason: denial.Reason, Execution: "not_started"}); recErr != nil {
				return recErr
			}
		}
		return err
	}
	token, err := t.credential(ctx, target)
	if err != nil {
		return err
	}
	if err := t.record(ctx, msg, agent, Decision{
		Outcome: "allowed", ReasonCode: reasonAuthorized, Execution: "not_started", BeforeEffect: true,
	}); err != nil {
		return err
	}
	return scrub(t.cfg.Client.Cancel(ctx, callFor(t.gate.Namespace, target, token), taskID), token)
}

// Observe implements driver.Transport. A transient error leaves the dispatch
// unsettled; the task timeout bounds how long that can last.
func (t *Transport) Observe(ctx context.Context, msg collaboration.Message) (driver.Observation, error) {
	agent, ok := t.cfg.Members[msg.Recipient]
	if !ok {
		return driver.Observation{}, fmt.Errorf("externaltransport: unknown external member %q", msg.Recipient)
	}
	cancel := msg.Kind == collaboration.DispatchCancel
	subject := msg
	if cancel {
		if msg.Target == nil {
			return driver.Observation{}, errors.New("externaltransport: cancel has no target dispatch")
		}
		subject = *msg.Target
	}
	kind, taskID, sentAt, ok, err := t.handle(ctx, subject.DispatchID)
	if err != nil || !ok {
		return driver.Observation{}, err
	}
	if kind == handleNotSent {
		return t.settle(ctx, msg, agent, subject, cancelled(msg, cancel, FailureNotSent), nil)
	}
	target, err := t.gate.Authorize(ctx, agent, AuthorizeOptions{SkipGrant: cancel, Revalidate: true})
	deadline := sentAt.Add(api.ExternalAgentDefaultTaskTimeout)
	if err == nil {
		deadline = sentAt.Add(target.Limits.TaskTimeout)
	}
	expired := !t.cfg.Now().Before(deadline)
	var denial *Denial
	switch {
	case errors.As(err, &denial):
		if cancel && !expired {
			return driver.Observation{}, nil // the remote cancel cannot be confirmed yet
		}
		return t.settle(ctx, msg, agent, subject, cancelled(msg, cancel, FailureAccessRevoked), denial)
	case err != nil:
		return driver.Observation{}, err
	}
	if kind == handlePending {
		if !expired {
			return driver.Observation{}, nil
		}
		return t.settle(ctx, msg, agent, subject, cancelled(msg, cancel, FailureSendUnknown), nil)
	}
	token, err := t.credential(ctx, target)
	if err != nil {
		if expired {
			return t.settle(ctx, msg, agent, subject, cancelled(msg, cancel, FailureTimedOut), nil)
		}
		return driver.Observation{}, err
	}
	call := callFor(t.gate.Namespace, target, token)
	pollCtx, stop := context.WithTimeout(ctx, observeTimeout)
	defer stop()
	task, err := t.cfg.Client.GetTask(pollCtx, call, taskID)
	if err != nil {
		if expired {
			return t.settle(ctx, msg, agent, subject, cancelled(msg, cancel, FailureTimedOut), nil)
		}
		return driver.Observation{}, scrub(err, token)
	}
	if !task.Status.State.Terminal() && expired {
		if !cancel {
			// Best effort: the task is already being abandoned.
			_ = t.cfg.Client.Cancel(pollCtx, call, taskID)
		}
		return t.settle(ctx, msg, agent, subject, cancelled(msg, cancel, FailureTimedOut), nil)
	}
	obs := t.interpret(msg, task, target)
	if !obs.Settled {
		return obs, nil
	}
	return t.settle(ctx, msg, agent, subject, obs, nil)
}

// interpret maps the polled task to an observation. A reply that is not text or
// the collaboration payload, or that exceeds the response limit, is rejected.
func (t *Transport) interpret(msg collaboration.Message, task *a2a.Task, target Target) driver.Observation {
	if task.Status.State == a2a.TaskStateCompleted && msg.Kind != collaboration.DispatchCancel && !validReply(task, target.Limits.MaxResponseBytes) {
		return driver.Observation{Settled: true, Actor: msg.Recipient, Status: collaboration.ReportFailed, Failure: a2atransport.FailureInvalidPayload}
	}
	obs := a2atransport.Interpret(msg, msg.Recipient, task)
	if obs.TokensKnown && obs.TokensUsed > target.Limits.MaxTokensPerTask {
		obs.TokensUsed = target.Limits.MaxTokensPerTask
	}
	return obs
}

// settle releases the dispatch's slot, audits the outcome and returns obs. The
// audit is written first, so a lost write is retried by the next poll.
func (t *Transport) settle(ctx context.Context, msg collaboration.Message, agent string, subject collaboration.Message, obs driver.Observation, denial *Denial) (driver.Observation, error) {
	if msg.Kind != collaboration.DispatchCancel {
		d := Decision{Outcome: "completed", ReasonCode: reasonCompleted, Execution: "completed", TargetReached: true}
		switch {
		case denial != nil:
			d = Decision{Outcome: denial.Outcome, ReasonCode: denial.Code, Reason: denial.Reason, Execution: "failed", TargetReached: true}
		case obs.Status != collaboration.ReportCompleted:
			d = Decision{Outcome: "failed", ReasonCode: reasonFailed, Reason: obs.Failure, Execution: "failed", TargetReached: obs.Failure != FailureNotSent}
		}
		if err := t.record(ctx, subject, agent, d); err != nil {
			return driver.Observation{}, err
		}
	}
	t.cfg.Limiter.Release(t.slotKey(agent), t.reservationID(subject.DispatchID))
	obs.Settled = true
	if obs.Actor == "" {
		obs.Actor = msg.Recipient
	}
	return obs, nil
}

// cancelled builds the settled observation for failure. A cancel is confirmed
// as cancelled: the task is no longer something the platform waits for.
func cancelled(msg collaboration.Message, cancel bool, failure string) driver.Observation {
	if cancel {
		return driver.Observation{Settled: true, Actor: msg.Recipient, Status: collaboration.ReportCancelled}
	}
	return driver.Observation{Settled: true, Actor: msg.Recipient, Status: collaboration.ReportFailed, Failure: failure}
}

func (t *Transport) prompt(msg collaboration.Message) string {
	return a2atransport.Prompt(msg, a2atransport.Reachable(t.cfg.Run.Spec.AllowedInteractions, msg.Recipient), a2atransport.NewTeam(t.cfg.Run.Spec))
}

func (t *Transport) credential(ctx context.Context, target Target) (Secret, error) {
	value, err := t.cfg.Credentials.ResolveAPIKey(ctx, t.gate.Namespace, target.Agent.Spec.CredentialRef.Name)
	if err != nil || value == "" {
		return "", errors.New("externaltransport: ExternalAgent credential is unavailable")
	}
	return Secret(value), nil
}

func (t *Transport) record(ctx context.Context, msg collaboration.Message, agent string, d Decision) error {
	d.Namespace, d.Session, d.Agent, d.Dispatch = t.gate.Namespace, t.gate.RunName, agent, msg.DispatchID
	d.Operation = auditOperationInvoke
	if msg.Kind == collaboration.DispatchCancel {
		d.Operation = auditOperationCancel
	}
	return t.cfg.Audit.Record(ctx, d)
}

func (t *Transport) slotKey(agent string) string { return t.gate.Namespace + "/" + agent }

func (t *Transport) reservationID(dispatchID string) string {
	run := t.cfg.Run
	return collaboration.LedgerName(run.Namespace, run.RunUID, run.RunName, run.Step, run.Attempt) + "/" + dispatchID
}

func callFor(namespace string, target Target, token Secret) Call {
	return Call{
		Namespace: namespace, Agent: target.Agent.ObjectMeta.Name, Host: target.Host, Port: target.Port,
		Addrs: target.Addrs, CardDigest: target.Agent.Spec.Card.Digest, Token: token, Limits: target.Limits,
	}
}

// validReply reports whether every artifact part is text or the collaboration
// payload and the reply fits in max bytes.
func validReply(task *a2a.Task, max int64) bool {
	var total int64
	for _, artifact := range task.Artifacts {
		for _, part := range artifact.Parts {
			switch content := part.Content.(type) {
			case a2a.Text:
				total += int64(len(content))
			case a2a.Raw:
				if part.MediaType != collaboration.MediaType {
					return false
				}
				total += int64(len(content))
			case a2a.Data:
				if part.MediaType != collaboration.MediaType {
					return false
				}
				raw, err := json.Marshal(content.Value)
				if err != nil {
					return false
				}
				total += int64(len(raw))
			default:
				return false
			}
		}
	}
	return total <= max
}

// Handle kinds. A handle records what is durably known about a dispatch's
// contact: not made, started but unconfirmed, or a known remote task.
const (
	handleNotSent = "notsent"
	handlePending = "pending"
	handleTask    = "task"
)

func encodeHandle(kind, taskID string, at time.Time) string {
	return kind + ":" + taskID + "|" + strconv.FormatInt(at.UnixNano(), 10)
}

func (t *Transport) handle(ctx context.Context, dispatch string) (kind, taskID string, at time.Time, ok bool, err error) {
	raw, found, err := t.cfg.Handles.Get(ctx, dispatch)
	if err != nil || !found {
		return "", "", time.Time{}, false, err
	}
	sep := strings.LastIndex(raw, "|")
	colon := strings.Index(raw, ":")
	if sep < 0 || colon < 0 || colon > sep {
		return "", "", time.Time{}, false, fmt.Errorf("externaltransport: malformed handle for dispatch %q", dispatch)
	}
	nanos, perr := strconv.ParseInt(raw[sep+1:], 10, 64)
	if perr != nil {
		return "", "", time.Time{}, false, fmt.Errorf("externaltransport: malformed handle for dispatch %q", dispatch)
	}
	return raw[:colon], raw[colon+1 : sep], time.Unix(0, nanos), true, nil
}

// redactedError carries an error message with a credential removed. It does not
// unwrap, so the original text cannot be recovered from it.
type redactedError struct{ msg string }

func (e redactedError) Error() string { return e.msg }

// scrub removes token from err so a client error can never leak the credential.
func scrub(err error, token Secret) error {
	if err == nil || token == "" {
		return err
	}
	if msg := err.Error(); strings.Contains(msg, token.Reveal()) {
		return redactedError{msg: strings.ReplaceAll(msg, token.Reveal(), "[redacted]")}
	}
	return err
}
