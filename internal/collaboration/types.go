// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package collaboration implements the deterministic strategy engine for
// governed multi-agent collaboration steps. It owns strategy semantics, the
// logical task ledger, limits and accounting. It performs no I/O: trusted
// execution code persists each returned State, performs the dispatch intents it
// contains through the governed A2A adapter, and feeds the observed outcomes
// back as events.
package collaboration

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	api "github.com/kognition-ai/kognition/api/v1alpha1"
)

const (
	// StateVersion is the persisted ledger schema version.
	StateVersion = 1

	// ExecutorOrigin labels content that the trusted executor produced.
	ExecutorOrigin = "executor"

	// MaxStateBytes bounds the encoded ledger. It stays below the 1 MiB
	// Kubernetes object limit; the engine fails closed when it is exceeded.
	MaxStateBytes = 900 << 10

	// FinalizationReserveDivisor reserves MaxTokens/divisor for final tasks so
	// that intermediate work cannot starve the result-producing turn.
	FinalizationReserveDivisor = 5
)

// Failure and stop reasons recorded in the ledger.
const (
	ReasonTurnLimit        = "TurnLimitExceeded"
	ReasonMessageLimit     = "MessageLimitExceeded"
	ReasonDelegationLimit  = "DelegationLimitExceeded"
	ReasonDepthLimit       = "DelegationDepthExceeded"
	ReasonTokenBudget      = "TokenBudgetExhausted"
	ReasonRequiredTask     = "RequiredTaskFailed"
	ReasonInvalidProposal  = "InvalidProposal"
	ReasonDispatchRefused  = "DispatchRefused"
	ReasonPeerQuiescence   = "PeerQuiescence"
	ReasonNoProgress       = "NoProgress"
	ReasonStateTooLarge    = "StateTooLarge"
	ReasonCancelled        = "Cancelled"
	ReasonDeadlineExceeded = "DeadlineExceeded"
	ReasonHandoffLimit     = "HandoffLimitExceeded"
)

var (
	// ErrTerminal rejects an event for a collaboration that already finished.
	ErrTerminal = errors.New("collaboration: activation is terminal")
	// ErrUnknownTask rejects a report or dispatch outcome with no ledger entry.
	ErrUnknownTask = errors.New("collaboration: unknown task or operation")
	// ErrActorMismatch rejects a report from a principal that is not the recipient.
	ErrActorMismatch = errors.New("collaboration: actor is not the task recipient")
	// ErrStaleTurn rejects a report for a turn that is not the task's current turn.
	ErrStaleTurn = errors.New("collaboration: stale turn")
	// ErrStaleEpoch rejects work from a previous owner epoch.
	ErrStaleEpoch = errors.New("collaboration: stale owner epoch")
	// ErrTaskSettled rejects a report for a task that already has a disposition.
	ErrTaskSettled = errors.New("collaboration: task already settled")
	// ErrInvalidEvent rejects a malformed event.
	ErrInvalidEvent = errors.New("collaboration: invalid event")
	// ErrInvalidDefinition rejects a collaboration definition.
	ErrInvalidDefinition = errors.New("collaboration: invalid definition")
)

// Phase is the activation phase.
type Phase string

const (
	PhaseRunning   Phase = "Running"
	PhaseStopping  Phase = "Stopping"
	PhaseSucceeded Phase = "Succeeded"
	PhaseFailed    Phase = "Failed"
	PhaseCancelled Phase = "Cancelled"
)

// Terminal reports whether no further execution can occur.
func (p Phase) Terminal() bool {
	return p == PhaseSucceeded || p == PhaseFailed || p == PhaseCancelled
}

// TaskState is the disposition of one logical task.
type TaskState string

const (
	// TaskPending is allocated and awaiting capacity; no dispatch intent exists.
	TaskPending TaskState = "Pending"
	// TaskRunning has a dispatch intent and awaits the member's turn report.
	TaskRunning TaskState = "Running"
	// TaskWaiting returned child delegations or a transfer and awaits them.
	TaskWaiting TaskState = "Waiting"
	// TaskUncertain may have reached the member; it is never resubmitted.
	TaskUncertain TaskState = "Uncertain"
	// TaskSucceeded, TaskFailed and TaskCancelled are terminal.
	TaskSucceeded TaskState = "Succeeded"
	TaskFailed    TaskState = "Failed"
	TaskCancelled TaskState = "Cancelled"
)

// Terminal reports whether the task has a final disposition.
func (s TaskState) Terminal() bool {
	return s == TaskSucceeded || s == TaskFailed || s == TaskCancelled
}

// Content is a bounded inline payload with trusted lineage. Origins are assigned
// by the engine from trusted execution state and never from member text.
type Content struct {
	Text    string   `json:"text,omitempty"`
	Origins []string `json:"origins,omitempty"`
}

// Task is one logical unit of work addressed to one member.
type Task struct {
	ID        string    `json:"id"`
	ParentID  string    `json:"parentId,omitempty"`
	Requester string    `json:"requester,omitempty"` // empty means the trusted executor
	Recipient string    `json:"recipient"`
	Depth     int32     `json:"depth"`
	Required  bool      `json:"required"`
	Final     bool      `json:"final,omitempty"` // completes the step through finalize
	Epoch     uint64    `json:"epoch"`
	State     TaskState `json:"state"`
	Turns     int32     `json:"turns"`
	Goal      Content   `json:"goal"`
	Result    *Content  `json:"result,omitempty"`
	Reason    string    `json:"reason,omitempty"`
	ClientOp  string    `json:"clientOp,omitempty"`

	// Children are the delegated tasks this task waits for. Delivered lists the
	// children whose results were already released to this task.
	Children  []string `json:"children,omitempty"`
	Delivered []string `json:"delivered,omitempty"`
	// Inputs are settled task results released to this task when it starts.
	Inputs []string `json:"inputs,omitempty"`
	// Lineage is the union of origins released to this task.
	Lineage []string `json:"lineage,omitempty"`

	TransferID    string `json:"transferId,omitempty"`
	CancelRequest bool   `json:"cancelRequested,omitempty"`
}

// DispatchKind classifies a dispatch intent.
type DispatchKind string

const (
	// DispatchStart opens the recipient's task.
	DispatchStart DispatchKind = "Start"
	// DispatchContinue releases child results or a rejection to a waiting task.
	DispatchContinue DispatchKind = "Continue"
	// DispatchPrepare asks a handoff recipient to passively acknowledge a transfer.
	DispatchPrepare DispatchKind = "Prepare"
	// DispatchCancel requests cancellation of a known remote task.
	DispatchCancel DispatchKind = "Cancel"
)

// DispatchStatus is the observed progress of one dispatch intent.
type DispatchStatus string

const (
	// DispatchIntended is persisted and has made no contact. A stop may refuse it.
	DispatchIntended DispatchStatus = "Intended"
	// DispatchCommitted won the final dispatch-admission fence; contact may occur.
	// Recovery treats it as uncertain because the contact outcome is unknown.
	DispatchCommitted DispatchStatus = "Committed"
	DispatchSent      DispatchStatus = "Sent"
	DispatchUncertain DispatchStatus = "Uncertain"
	DispatchRefused   DispatchStatus = "Refused"
	DispatchReported  DispatchStatus = "Reported"
)

// Dispatch is a persisted intent to contact a member. The intent is recorded
// before any contact so that recovery resolves the same operation.
type Dispatch struct {
	ID         string          `json:"id"`
	Kind       DispatchKind    `json:"kind"`
	TaskID     string          `json:"taskId"`
	Recipient  string          `json:"recipient"`
	Turn       int32           `json:"turn,omitempty"`
	Epoch      uint64          `json:"epoch"`
	TransferID string          `json:"transferId,omitempty"`
	Inputs     []string        `json:"inputs,omitempty"` // child task IDs whose results are released
	Note       string          `json:"note,omitempty"`
	Allowance  int64           `json:"allowance,omitempty"`
	Allowed    []OperationKind `json:"allowed,omitempty"` // operations the recipient was offered at intent time
	Status     DispatchStatus  `json:"status"`
	Reason     string          `json:"reason,omitempty"`
}

// TransferStatus is the state of a handoff transfer.
type TransferStatus string

const (
	TransferProposed  TransferStatus = "Proposed"
	TransferUncertain TransferStatus = "Uncertain"
	TransferCommitted TransferStatus = "Committed"
	TransferRejected  TransferStatus = "Rejected"
)

// Transfer is a persisted handoff proposal. It binds the recipient, the filtered
// context digest and both epochs.
type Transfer struct {
	ID            string         `json:"id"`
	TaskID        string         `json:"taskId"`
	From          string         `json:"from"`
	To            string         `json:"to"`
	FromEpoch     uint64         `json:"fromEpoch"`
	ToEpoch       uint64         `json:"toEpoch"`
	Context       Content        `json:"context"`
	ContextDigest string         `json:"contextDigest"`
	Status        TransferStatus `json:"status"`
}

// ManifestEntry records one Parallel branch disposition.
type ManifestEntry struct {
	Member   string    `json:"member"`
	TaskID   string    `json:"taskId"`
	Optional bool      `json:"optional,omitempty"`
	State    TaskState `json:"state"`
	Reason   string    `json:"reason,omitempty"`
}

// Counters are the cumulative step accounting values.
type Counters struct {
	Turns          int32 `json:"turns"`
	Messages       int32 `json:"messages"`
	Delegations    int32 `json:"delegations"`
	TokensUsed     int64 `json:"tokensUsed"`
	TokensReserved int64 `json:"tokensReserved"`
}

// Outcome is the pending or final disposition of the activation.
type Outcome struct {
	Phase  Phase    `json:"phase"`
	Reason string   `json:"reason,omitempty"`
	Result *Content `json:"result,omitempty"`
}

// State is the complete, serializable collaboration ledger for one activation.
type State struct {
	Version      int                       `json:"version"`
	Strategy     api.CollaborationStrategy `json:"strategy"`
	Phase        Phase                     `json:"phase"`
	Outcome      *Outcome                  `json:"outcome,omitempty"`
	Input        Content                   `json:"input"`
	Epoch        uint64                    `json:"epoch"`
	Owner        string                    `json:"owner,omitempty"`
	Counters     Counters                  `json:"counters"`
	Handoffs     int32                     `json:"handoffs,omitempty"`
	NextTask     int                       `json:"nextTask"`
	NextTransfer int                       `json:"nextTransfer"`
	Tasks        []Task                    `json:"tasks"`
	Dispatches   []Dispatch                `json:"dispatches"`
	Transfer     *Transfer                 `json:"transfer,omitempty"`
	Manifest     []ManifestEntry           `json:"manifest,omitempty"`
	SynthStarted bool                      `json:"synthStarted,omitempty"`
}

// Result returns the published result of a succeeded activation.
func (s *State) Result() *Content {
	if s.Phase == PhaseSucceeded && s.Outcome != nil {
		return s.Outcome.Result
	}
	return nil
}

// Task returns the task with the given ID.
func (s *State) Task(id string) (*Task, bool) {
	for i := range s.Tasks {
		if s.Tasks[i].ID == id {
			return &s.Tasks[i], true
		}
	}
	return nil, false
}

// Pending returns the dispatch intents that still need contact, in ledger order.
// Recovery re-reads these, so an Intended entry is resolved rather than replaced.
func (s *State) Pending() []Dispatch {
	var out []Dispatch
	for _, d := range s.Dispatches {
		if d.Status == DispatchIntended {
			out = append(out, d)
		}
	}
	return out
}

func (s *State) dispatch(id string) (*Dispatch, bool) {
	for i := range s.Dispatches {
		if s.Dispatches[i].ID == id {
			return &s.Dispatches[i], true
		}
	}
	return nil, false
}

func (s State) clone() State {
	out := s
	out.Tasks = make([]Task, len(s.Tasks))
	for i, t := range s.Tasks {
		t.Children = append([]string(nil), t.Children...)
		t.Delivered = append([]string(nil), t.Delivered...)
		t.Inputs = append([]string(nil), t.Inputs...)
		t.Lineage = append([]string(nil), t.Lineage...)
		t.Goal.Origins = append([]string(nil), t.Goal.Origins...)
		if t.Result != nil {
			r := *t.Result
			r.Origins = append([]string(nil), r.Origins...)
			t.Result = &r
		}
		out.Tasks[i] = t
	}
	out.Dispatches = make([]Dispatch, len(s.Dispatches))
	for i, d := range s.Dispatches {
		d.Inputs = append([]string(nil), d.Inputs...)
		out.Dispatches[i] = d
	}
	out.Manifest = append([]ManifestEntry(nil), s.Manifest...)
	out.Input.Origins = append([]string(nil), s.Input.Origins...)
	if s.Transfer != nil {
		t := *s.Transfer
		t.Context.Origins = append([]string(nil), t.Context.Origins...)
		out.Transfer = &t
	}
	if s.Outcome != nil {
		o := *s.Outcome
		if o.Result != nil {
			r := *o.Result
			r.Origins = append([]string(nil), r.Origins...)
			o.Result = &r
		}
		out.Outcome = &o
	}
	return out
}

func unionOrigins(sets ...[]string) []string {
	seen := map[string]struct{}{}
	for _, set := range sets {
		for _, o := range set {
			if o != "" {
				seen[o] = struct{}{}
			}
		}
	}
	if len(seen) == 0 {
		return nil
	}
	out := make([]string, 0, len(seen))
	for o := range seen {
		out = append(out, o)
	}
	sort.Strings(out)
	return out
}

func contentErr(field, text string, maxContentBytes int64) error {
	if maxContentBytes < 1 {
		return errors.New("limits.maxContentBytes must be positive")
	}
	if int64(len(text)) > maxContentBytes {
		return fmt.Errorf("%s exceeds %d bytes", field, maxContentBytes)
	}
	return nil
}

func definitionErr(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidDefinition, fmt.Sprintf(format, args...))
}

func joinErrs(errs []string) error {
	if len(errs) == 0 {
		return nil
	}
	return fmt.Errorf("%w: %s", ErrInvalidDefinition, strings.Join(errs, "; "))
}
