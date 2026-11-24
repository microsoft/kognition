// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package collaboration

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	api "github.com/kognition-ai/kognition/api/v1alpha1"
)

// ErrStateTooLarge reports that an event cannot be recorded within the ledger
// bound while the activation is no longer running; the caller must park the
// activation as failed outside the ledger.
var ErrStateTooLarge = errors.New("collaboration: ledger exceeds its size bound")

// ReasonNoBranchSucceeded fails a Parallel step with no successful branch.
const ReasonNoBranchSucceeded = "NoBranchSucceeded"

// ReportStatus is the member-reported disposition of one turn.
type ReportStatus string

const (
	ReportCompleted ReportStatus = "Completed"
	ReportFailed    ReportStatus = "Failed"
	ReportCancelled ReportStatus = "Cancelled"
)

// Event is an observed fact fed to Apply. The trusted execution layer
// derives every field from authenticated transport state, never from member
// content.
type Event interface{ isEvent() }

// TurnReport is the settled outcome of one member turn. Actor and Epoch come
// from the authenticated responding principal and the dispatched epoch.
type TurnReport struct {
	TaskID      string
	Turn        int32
	Actor       string
	Epoch       uint64
	Status      ReportStatus
	Operations  []Operation
	Failure     string
	TokensUsed  int64
	TokensKnown bool
}

// CommitDispatch records that the dispatch won the final admission fence, so
// contact may occur. A refused dispatch cannot be committed.
type CommitDispatch struct{ ID string }

// AckDispatch records acknowledged contact. For a Cancel dispatch it means the
// remote task is confirmed canceled. It also resolves an Uncertain dispatch.
type AckDispatch struct{ ID string }

// UncertainDispatch records contact whose effect is unknown. It is never retried.
type UncertainDispatch struct{ ID, Reason string }

// RefuseDispatch records that no contact occurred (denied before commit).
type RefuseDispatch struct{ ID, Reason string }

// TransferAck is the authenticated passive acknowledgement of a handoff.
type TransferAck struct {
	TransferID string
	Actor      string
	Accepted   bool
}

// TaskAbandoned closes an Uncertain task that reconciliation proved or the
// executor decided can no longer settle. It is the only way out of Uncertain
// without a report.
type TaskAbandoned struct{ TaskID, Reason string }

// Stop requests termination. Phase must be Failed or Cancelled.
type Stop struct {
	Phase  Phase
	Reason string
}

func (TurnReport) isEvent()        {}
func (CommitDispatch) isEvent()    {}
func (AckDispatch) isEvent()       {}
func (UncertainDispatch) isEvent() {}
func (RefuseDispatch) isEvent()    {}
func (TransferAck) isEvent()       {}
func (TaskAbandoned) isEvent()     {}
func (Stop) isEvent()              {}

// Engine applies events to a ledger deterministically. It holds no state of its
// own, so a fresh Engine over the same Definition continues any ledger.
type Engine struct{ def Definition }

// New returns an Engine for a definition built by NewDefinition.
func New(def Definition) (*Engine, error) {
	if def.memberSet == nil || len(def.Members) < 2 {
		return nil, definitionErr("definition was not built by NewDefinition")
	}
	return &Engine{def: def}, nil
}

// Start opens a ledger for the activation input.
func (e *Engine) Start(input string) (State, error) {
	if strings.TrimSpace(input) == "" {
		return State{}, fmt.Errorf("%w: input is required", ErrInvalidEvent)
	}
	if err := contentErr("input", input); err != nil {
		return State{}, fmt.Errorf("%w: %v", ErrInvalidEvent, err)
	}
	st := State{
		Version:    StateVersion,
		Strategy:   e.def.Strategy,
		Phase:      PhaseRunning,
		Input:      Content{Text: input, Origins: []string{ExecutorOrigin}},
		Epoch:      1,
		Tasks:      []Task{},
		Dispatches: []Dispatch{},
	}
	root := func(member string) {
		e.newTask(&st, taskSpec{Recipient: member, Required: true, Final: true, Goal: st.Input})
	}
	switch e.def.Strategy {
	case api.CollaborationStrategySupervisor:
		root(e.def.Coordinator)
	case api.CollaborationStrategyPeer:
		root(e.def.Finalizer)
	case api.CollaborationStrategyHandoff:
		st.Owner = e.def.Entry
		root(e.def.Entry)
	case api.CollaborationStrategyParallel:
		for _, b := range e.def.Branches {
			e.newTask(&st, taskSpec{Recipient: b.Member, Required: !b.Optional, Goal: st.Input})
		}
	}
	e.progress(&st)
	return e.bound(State{}, st)
}

// Apply records one event and returns the next ledger. An invalid event returns
// the original ledger unchanged with an error.
func (e *Engine) Apply(st State, ev Event) (State, error) {
	if st.Version != StateVersion || st.Strategy != e.def.Strategy {
		return st, fmt.Errorf("%w: ledger does not match the definition", ErrInvalidEvent)
	}
	if st.Phase.Terminal() {
		return st, ErrTerminal
	}
	next := st.clone()
	var err error
	switch v := ev.(type) {
	case TurnReport:
		err = e.applyReport(&next, v)
	case CommitDispatch:
		err = e.applyDispatch(&next, v.ID, DispatchCommitted, "")
	case AckDispatch:
		err = e.applyDispatch(&next, v.ID, DispatchSent, "")
	case UncertainDispatch:
		err = e.applyDispatch(&next, v.ID, DispatchUncertain, v.Reason)
	case RefuseDispatch:
		err = e.applyDispatch(&next, v.ID, DispatchRefused, v.Reason)
	case TransferAck:
		err = e.applyAck(&next, v)
	case TaskAbandoned:
		err = e.applyAbandon(&next, v)
	case Stop:
		switch v.Phase {
		case PhaseFailed, PhaseCancelled:
			e.beginStop(&next, v.Phase, truncate(v.Reason), nil)
		default:
			err = fmt.Errorf("%w: stop phase", ErrInvalidEvent)
		}
	default:
		err = fmt.Errorf("%w: unknown event", ErrInvalidEvent)
	}
	if err != nil {
		return st, err
	}
	e.progress(&next)
	return e.bound(st, next)
}

// Recover resolves dispatches that were committed but never acknowledged. After
// a crash the contact outcome is unknown, so they become Uncertain and are
// never resent.
func (e *Engine) Recover(st State) (State, error) {
	if st.Phase.Terminal() {
		return st, nil
	}
	next := st.clone()
	for i := range next.Dispatches {
		if next.Dispatches[i].Status == DispatchCommitted {
			if err := e.applyDispatch(&next, next.Dispatches[i].ID, DispatchUncertain, "RecoveredAfterCommit"); err != nil {
				return st, err
			}
		}
	}
	e.progress(&next)
	return e.bound(st, next)
}

func (e *Engine) bound(prev, next State) (State, error) {
	raw, err := json.Marshal(next)
	if err != nil {
		return prev, err
	}
	if len(raw) <= MaxStateBytes || next.Phase.Terminal() {
		return next, nil
	}
	if prev.Version == 0 || prev.Phase != PhaseRunning {
		return prev, ErrStateTooLarge
	}
	fb := prev.clone()
	e.beginStop(&fb, PhaseFailed, ReasonStateTooLarge, nil)
	e.drain(&fb)
	return fb, nil
}

type taskSpec struct {
	Requester, Recipient string
	ParentID             string
	Depth                int32
	Required, Final      bool
	Goal                 Content
	ClientOp             string
	Inputs               []string
}

// newTask allocates a task. Callers must re-fetch task pointers afterwards.
func (e *Engine) newTask(st *State, s taskSpec) string {
	st.NextTask++
	id := fmt.Sprintf("t%d", st.NextTask)
	st.Tasks = append(st.Tasks, Task{
		ID: id, ParentID: s.ParentID, Requester: s.Requester, Recipient: s.Recipient,
		Depth: s.Depth, Required: s.Required, Final: s.Final, Epoch: st.Epoch,
		State: TaskPending, Goal: s.Goal, ClientOp: s.ClientOp, Inputs: s.Inputs,
		Lineage: unionOrigins(s.Goal.Origins),
	})
	return id
}

func dispatchID(taskID string, turn int32) string { return fmt.Sprintf("%s.%d", taskID, turn) }

func truncate(s string) string {
	if len(s) > 256 {
		return s[:256]
	}
	return s
}

// progress runs the deterministic settle loop after every event.
func (e *Engine) progress(st *State) {
	if st.Phase == PhaseRunning {
		e.joinParallel(st)
	}
	if st.Phase == PhaseRunning {
		e.schedule(st)
	}
	if st.Phase == PhaseRunning {
		e.quiescence(st)
	}
	if st.Phase == PhaseStopping {
		e.drain(st)
	}
}

func (e *Engine) active(st *State) int32 {
	var n int32
	for i := range st.Tasks {
		if s := st.Tasks[i].State; s == TaskRunning || s == TaskUncertain {
			n++
		}
	}
	return n
}

func (e *Engine) ready(st *State, t *Task) bool {
	if t.State != TaskWaiting || t.CancelRequest {
		return false
	}
	for _, id := range t.Children {
		if c, ok := st.Task(id); !ok || !c.State.Terminal() {
			return false
		}
	}
	if t.TransferID != "" {
		return st.Transfer != nil && st.Transfer.ID == t.TransferID && st.Transfer.Status == TransferRejected
	}
	return true
}

func (e *Engine) schedule(st *State) {
	for i := 0; i < len(st.Tasks) && st.Phase == PhaseRunning; i++ {
		t := &st.Tasks[i]
		var kind DispatchKind
		switch {
		case t.State == TaskPending:
			kind = DispatchStart
		case e.ready(st, t):
			kind = DispatchContinue
		default:
			continue
		}
		if e.active(st) >= e.def.Limits.MaxParallelTasks {
			return
		}
		e.dispatchTurn(st, i, kind)
	}
}

func (e *Engine) allowance(st *State, t *Task) int64 {
	avail := e.def.Limits.MaxTokens - st.Counters.TokensUsed - st.Counters.TokensReserved
	if t.Final {
		// A final task that can still fan out leaves half for its work, so an unknown-usage turn cannot starve it.
		if len(e.allowedOperations(st, t)) > 1 {
			return avail - avail/2
		}
		return avail
	}
	avail -= e.def.Limits.MaxTokens / FinalizationReserveDivisor
	if avail <= 0 {
		return 0
	}
	slots := int64(e.def.Limits.MaxParallelTasks - e.active(st))
	if slots < 1 {
		slots = 1
	}
	return avail / slots
}

func (e *Engine) dispatchTurn(st *State, ti int, kind DispatchKind) {
	c := &st.Counters
	lim := e.def.Limits
	switch {
	case c.Turns >= lim.MaxTurns:
		e.beginStop(st, PhaseFailed, ReasonTurnLimit, nil)
		return
	case c.Messages >= lim.MaxMessages:
		e.beginStop(st, PhaseFailed, ReasonMessageLimit, nil)
		return
	}
	t := &st.Tasks[ti]
	allow := e.allowance(st, t)
	if allow <= 0 {
		e.beginStop(st, PhaseFailed, ReasonTokenBudget, nil)
		return
	}
	d := Dispatch{Kind: kind, TaskID: t.ID, Recipient: t.Recipient, Epoch: t.Epoch, Allowance: allow, Status: DispatchIntended}
	d.Allowed = e.allowedOperations(st, t) // frozen so the dispatch content never changes after intent
	switch kind {
	case DispatchStart:
		d.Inputs = append([]string(nil), t.Inputs...)
		for _, id := range t.Inputs {
			if in, ok := st.Task(id); ok && in.Result != nil {
				t.Lineage = unionOrigins(t.Lineage, in.Result.Origins)
			}
		}
	case DispatchContinue:
		for _, id := range t.Children {
			if !contains(t.Delivered, id) {
				d.Inputs = append(d.Inputs, id)
				t.Delivered = append(t.Delivered, id)
				if ch, ok := st.Task(id); ok && ch.Result != nil {
					t.Lineage = unionOrigins(t.Lineage, ch.Result.Origins)
				}
			}
		}
		if t.TransferID != "" {
			d.Note = "TransferRejected"
			t.TransferID = ""
		}
	}
	t.Turns++
	d.Turn = t.Turns
	d.ID = dispatchID(t.ID, t.Turns)
	t.State = TaskRunning
	c.Turns++
	c.Messages++
	c.TokensReserved += allow
	st.Dispatches = append(st.Dispatches, d)
}

func contains(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}

func (e *Engine) quiescence(st *State) {
	if st.Transfer != nil && (st.Transfer.Status == TransferProposed || st.Transfer.Status == TransferUncertain) {
		return
	}
	for i := range st.Tasks {
		switch st.Tasks[i].State {
		case TaskPending, TaskRunning, TaskUncertain:
			return
		}
		if e.ready(st, &st.Tasks[i]) {
			return
		}
	}
	reason := ReasonNoProgress
	if e.def.Strategy == api.CollaborationStrategyPeer {
		reason = ReasonPeerQuiescence
	}
	e.beginStop(st, PhaseFailed, reason, nil)
}

// beginStop records the first stop decision; later stops never override it.
func (e *Engine) beginStop(st *State, phase Phase, reason string, result *Content) {
	if st.Phase != PhaseRunning {
		return
	}
	st.Phase = PhaseStopping
	st.Outcome = &Outcome{Phase: phase, Reason: reason, Result: result}
}

// drain cancels unfinished work and moves to the outcome phase only after every
// task is terminal and no transfer is uncertain.
func (e *Engine) drain(st *State) {
	if st.Phase != PhaseStopping {
		return
	}
	if st.Transfer != nil && (st.Transfer.Status == TransferProposed || st.Transfer.Status == TransferUncertain) {
		if st.Transfer.Status == TransferProposed {
			st.Transfer.Status = TransferRejected
			if p, ok := st.dispatch(st.Transfer.ID + ".p"); ok && p.Status == DispatchIntended {
				p.Status, p.Reason = DispatchRefused, "StoppedBeforeContact"
			}
		}
	}
	for i := range st.Tasks {
		t := &st.Tasks[i]
		if t.State.Terminal() || t.CancelRequest {
			continue
		}
		if t.State == TaskPending {
			t.State, t.Reason = TaskCancelled, "StoppedBeforeDispatch"
			continue
		}
		if cur, ok := st.dispatch(dispatchID(t.ID, t.Turns)); ok && t.State == TaskRunning && cur.Status == DispatchIntended {
			cur.Status, cur.Reason = DispatchRefused, "StoppedBeforeContact"
			e.refund(st, cur)
			t.State, t.Reason = TaskCancelled, "StoppedBeforeContact"
			continue
		}
		t.CancelRequest = true
		st.Counters.Messages++
		st.Dispatches = append(st.Dispatches, Dispatch{
			ID: t.ID + ".c", Kind: DispatchCancel, TaskID: t.ID, Recipient: t.Recipient,
			Turn: t.Turns, Epoch: t.Epoch, Status: DispatchIntended,
		})
	}
	for i := range st.Tasks {
		if !st.Tasks[i].State.Terminal() {
			return
		}
	}
	if st.Transfer != nil && st.Transfer.Status == TransferUncertain {
		return
	}
	st.Phase = st.Outcome.Phase
}

func (e *Engine) refund(st *State, d *Dispatch) {
	st.Counters.TokensReserved -= d.Allowance
	if st.Counters.TokensReserved < 0 {
		st.Counters.TokensReserved = 0
	}
}

// settleDispatch closes a dispatch's token accounting exactly once.
func (e *Engine) settleDispatch(st *State, d *Dispatch, used int64) {
	if d.Status == DispatchReported || d.Status == DispatchRefused {
		return
	}
	e.refund(st, d)
	st.Counters.TokensUsed += used
	d.Status = DispatchReported
}

func (e *Engine) settleTask(st *State, ti int, state TaskState, reason string, result *Content) {
	t := &st.Tasks[ti]
	t.State, t.Reason, t.Result = state, truncate(reason), result
	if state != TaskSucceeded && t.Required {
		e.beginStop(st, PhaseFailed, ReasonRequiredTask+": "+t.ID+" "+t.Reason, nil)
	}
}

func (e *Engine) failStep(st *State, ti int, reason string) {
	e.beginStop(st, PhaseFailed, reason, nil)
	e.settleTask(st, ti, TaskFailed, reason, nil)
}

func (e *Engine) taskIndex(st *State, id string) int {
	for i := range st.Tasks {
		if st.Tasks[i].ID == id {
			return i
		}
	}
	return -1
}

func (e *Engine) applyReport(st *State, r TurnReport) error {
	ti := e.taskIndex(st, r.TaskID)
	if ti < 0 {
		return ErrUnknownTask
	}
	t := &st.Tasks[ti]
	if r.Actor != t.Recipient {
		return ErrActorMismatch
	}
	if t.Epoch < st.Epoch || r.Epoch != t.Epoch {
		return ErrStaleEpoch
	}
	if t.State.Terminal() {
		return ErrTaskSettled
	}
	if r.Turn != t.Turns {
		return ErrStaleTurn
	}
	switch r.Status {
	case ReportCompleted, ReportFailed, ReportCancelled:
	default:
		return fmt.Errorf("%w: report status", ErrInvalidEvent)
	}
	live := t.State == TaskRunning || t.State == TaskUncertain
	if !live && !(t.CancelRequest && r.Status != ReportCompleted) {
		return fmt.Errorf("%w: task is not awaiting a report", ErrStaleTurn)
	}
	if r.TokensUsed < 0 {
		return fmt.Errorf("%w: negative token usage", ErrInvalidEvent)
	}
	if live {
		d, ok := st.dispatch(dispatchID(t.ID, t.Turns))
		if !ok {
			return ErrUnknownTask
		}
		if d.Status == DispatchIntended {
			return fmt.Errorf("%w: report for an uncontacted dispatch", ErrInvalidEvent)
		}
		used := r.TokensUsed
		if !r.TokensKnown {
			used = d.Allowance
		}
		e.settleDispatch(st, d, used)
		st.Counters.Messages++
	}
	switch r.Status {
	case ReportFailed:
		e.settleTask(st, ti, TaskFailed, "MemberFailed: "+r.Failure, nil)
		return nil
	case ReportCancelled:
		e.settleTask(st, ti, TaskCancelled, "RemoteCancelled", nil)
		return nil
	}
	if st.Counters.TokensUsed > e.def.Limits.MaxTokens {
		e.failStep(st, ti, ReasonTokenBudget)
		return nil
	}
	if st.Phase != PhaseRunning {
		e.settleDuringStop(st, ti, r.Operations)
		return nil
	}
	e.applyOperations(st, ti, r.Operations)
	return nil
}

// settleDuringStop keeps a usable terminal result and discards coordination.
func (e *Engine) settleDuringStop(st *State, ti int, ops []Operation) {
	t := &st.Tasks[ti]
	if ValidateOperations(ops) == nil && len(ops) == 1 {
		op := ops[0]
		if op.Kind == OpComplete && !t.Final || op.Kind == OpFinalize && t.Final {
			e.settleTask(st, ti, TaskSucceeded, "", &Content{Text: op.Result, Origins: unionOrigins(t.Lineage, []string{t.Recipient})})
			return
		}
	}
	e.settleTask(st, ti, TaskCancelled, "StoppedDuringTurn", nil)
}

func (e *Engine) invalid(st *State, ti int, detail string) {
	e.settleTask(st, ti, TaskFailed, ReasonInvalidProposal+": "+detail, nil)
}

func (e *Engine) mayDelegate(t *Task) bool {
	switch e.def.Strategy {
	case api.CollaborationStrategySupervisor:
		return t.Recipient == e.def.Coordinator
	case api.CollaborationStrategyPeer:
		return true
	}
	return false
}

func (e *Engine) applyOperations(st *State, ti int, ops []Operation) {
	t := &st.Tasks[ti]
	if err := ValidateOperations(ops); err != nil {
		e.invalid(st, ti, err.Error())
		return
	}
	var complete, finalize, delegate, handoff int
	for _, op := range ops {
		switch op.Kind {
		case OpComplete:
			complete++
		case OpFinalize:
			finalize++
		case OpDelegate:
			delegate++
		case OpHandoff:
			handoff++
		}
	}
	switch {
	case complete == 1 && len(ops) == 1:
		if t.Final {
			e.invalid(st, ti, "a final task must finalize")
			return
		}
		e.settleTask(st, ti, TaskSucceeded, "", &Content{Text: ops[0].Result, Origins: unionOrigins(t.Lineage, []string{t.Recipient})})
	case finalize == 1 && len(ops) == 1:
		e.finalize(st, ti, ops[0])
	case delegate == len(ops):
		e.delegate(st, ti, ops)
	case handoff == 1 && len(ops) == 1:
		e.handoff(st, ti, ops[0])
	default:
		e.invalid(st, ti, "operations cannot be combined")
	}
}

func (e *Engine) finalize(st *State, ti int, op Operation) {
	t := &st.Tasks[ti]
	if !t.Final {
		e.invalid(st, ti, "only the final task may finalize")
		return
	}
	for i := range st.Tasks {
		if o := &st.Tasks[i]; o.ID != t.ID && o.Required && o.State != TaskSucceeded {
			e.invalid(st, ti, "required task "+o.ID+" is unsettled")
			return
		}
	}
	if st.Transfer != nil && (st.Transfer.Status == TransferProposed || st.Transfer.Status == TransferUncertain) {
		e.invalid(st, ti, "a transfer is pending")
		return
	}
	res := Content{Text: op.Result, Origins: unionOrigins(t.Lineage, []string{t.Recipient})}
	e.settleTask(st, ti, TaskSucceeded, "", &res)
	e.beginStop(st, PhaseSucceeded, "", &res)
}

func (e *Engine) delegate(st *State, ti int, ops []Operation) {
	t := &st.Tasks[ti]
	if !e.mayDelegate(t) {
		e.invalid(st, ti, "member may not delegate")
		return
	}
	for _, op := range ops {
		switch {
		case op.To == t.Recipient || !e.def.isMember(op.To) || !e.def.allows(t.Recipient, op.To):
			e.invalid(st, ti, "interaction "+t.Recipient+" -> "+op.To+" is not allowed")
			return
		case e.childOp(st, t.ID, op.OperationID):
			e.invalid(st, ti, "duplicate operationId "+op.OperationID)
			return
		}
	}
	if t.Depth+1 > e.def.Limits.MaxDelegationDepth {
		e.failStep(st, ti, ReasonDepthLimit)
		return
	}
	if st.Counters.Delegations+int32(len(ops)) > e.def.Limits.MaxDelegations {
		e.failStep(st, ti, ReasonDelegationLimit)
		return
	}
	parent, recipient, depth, lineage := t.ID, t.Recipient, t.Depth+1, t.Lineage
	var children []string
	for _, op := range ops {
		id := e.newTask(st, taskSpec{
			Requester: recipient, Recipient: op.To, ParentID: parent, Depth: depth,
			Required: op.Required == nil || *op.Required, ClientOp: op.OperationID,
			Goal: Content{Text: op.Goal, Origins: unionOrigins(lineage, []string{recipient})},
		})
		children = append(children, id)
	}
	st.Counters.Delegations += int32(len(ops))
	t = &st.Tasks[ti]
	t.Children = append(t.Children, children...)
	t.State = TaskWaiting
}

func (e *Engine) childOp(st *State, parent, op string) bool {
	for i := range st.Tasks {
		if st.Tasks[i].ParentID == parent && st.Tasks[i].ClientOp == op {
			return true
		}
	}
	return false
}

func (e *Engine) handoff(st *State, ti int, op Operation) {
	t := &st.Tasks[ti]
	if e.def.Strategy != api.CollaborationStrategyHandoff || !t.Final {
		e.invalid(st, ti, "handoff is not available to this task")
		return
	}
	if op.To == t.Recipient || !e.def.isMember(op.To) || !e.def.allows(t.Recipient, op.To) {
		e.invalid(st, ti, "interaction "+t.Recipient+" -> "+op.To+" is not allowed")
		return
	}
	switch {
	case st.Handoffs >= e.def.MaxHandoffs:
		e.failStep(st, ti, ReasonHandoffLimit)
		return
	case st.Counters.Delegations >= e.def.Limits.MaxDelegations:
		e.failStep(st, ti, ReasonDelegationLimit)
		return
	case st.Counters.Messages >= e.def.Limits.MaxMessages:
		e.failStep(st, ti, ReasonMessageLimit)
		return
	}
	st.NextTransfer++
	ctx := Content{Text: op.Context, Origins: unionOrigins(t.Lineage, []string{t.Recipient})}
	sum := sha256.Sum256([]byte(ctx.Text + "\x00" + strings.Join(ctx.Origins, ",")))
	tr := &Transfer{
		ID: fmt.Sprintf("x%d", st.NextTransfer), TaskID: t.ID, From: t.Recipient, To: op.To,
		FromEpoch: st.Epoch, ToEpoch: st.Epoch + 1, Context: ctx,
		ContextDigest: hex.EncodeToString(sum[:]), Status: TransferProposed,
	}
	st.Transfer = tr
	st.Handoffs++
	st.Counters.Delegations++
	st.Counters.Messages++
	st.Dispatches = append(st.Dispatches, Dispatch{
		ID: tr.ID + ".p", Kind: DispatchPrepare, TaskID: t.ID, Recipient: op.To,
		Epoch: st.Epoch, TransferID: tr.ID, Status: DispatchIntended,
	})
	t.TransferID = tr.ID
	t.State = TaskWaiting
}

func (e *Engine) applyAck(st *State, a TransferAck) error {
	tr := st.Transfer
	if tr == nil || tr.ID != a.TransferID {
		return ErrUnknownTask
	}
	if a.Actor != tr.To {
		return ErrActorMismatch
	}
	if st.Epoch != tr.FromEpoch {
		return ErrStaleEpoch
	}
	if tr.Status != TransferProposed && tr.Status != TransferUncertain {
		return ErrTaskSettled
	}
	if d, ok := st.dispatch(tr.ID + ".p"); ok && d.Status != DispatchReported {
		d.Status = DispatchReported
	}
	if !a.Accepted || st.Phase != PhaseRunning {
		tr.Status = TransferRejected
		return nil
	}
	ti := e.taskIndex(st, tr.TaskID)
	if ti < 0 {
		return ErrUnknownTask
	}
	for i := range st.Tasks {
		if o := &st.Tasks[i]; i != ti && !o.State.Terminal() {
			return fmt.Errorf("%w: unexpected active task %s", ErrInvalidEvent, o.ID)
		}
	}
	old := &st.Tasks[ti]
	oldLineage, oldDepth, oldID := old.Lineage, old.Depth, old.ID
	tr.Status = TransferCommitted
	st.Epoch, st.Owner = tr.ToEpoch, tr.To
	old.State, old.Reason, old.TransferID = TaskSucceeded, "HandedOff", ""
	old.Result = &Content{Text: "handoff to " + tr.To, Origins: unionOrigins(oldLineage, []string{tr.From})}
	e.newTask(st, taskSpec{
		Requester: tr.From, Recipient: tr.To, ParentID: oldID, Depth: oldDepth,
		Required: true, Final: true, Goal: tr.Context,
	})
	return nil
}

func (e *Engine) applyAbandon(st *State, a TaskAbandoned) error {
	ti := e.taskIndex(st, a.TaskID)
	if ti < 0 {
		return ErrUnknownTask
	}
	t := &st.Tasks[ti]
	if t.State != TaskUncertain {
		return fmt.Errorf("%w: task is not uncertain", ErrInvalidEvent)
	}
	if d, ok := st.dispatch(dispatchID(t.ID, t.Turns)); ok {
		e.settleDispatch(st, d, d.Allowance)
	}
	e.settleTask(st, ti, TaskFailed, "UncertainAbandoned: "+a.Reason, nil)
	return nil
}

func (e *Engine) applyDispatch(st *State, id string, to DispatchStatus, reason string) error {
	d, ok := st.dispatch(id)
	if !ok {
		return ErrUnknownTask
	}
	ti := e.taskIndex(st, d.TaskID)
	if ti < 0 {
		return ErrUnknownTask
	}
	bad := func() error {
		return fmt.Errorf("%w: dispatch %s is %s", ErrInvalidEvent, id, d.Status)
	}
	switch to {
	case DispatchCommitted:
		if d.Status != DispatchIntended {
			return bad()
		}
		d.Status = DispatchCommitted
	case DispatchSent:
		if d.Status != DispatchCommitted && d.Status != DispatchUncertain {
			return bad()
		}
		d.Status = DispatchSent
		switch d.Kind {
		case DispatchStart, DispatchContinue:
			if t := &st.Tasks[ti]; t.State == TaskUncertain {
				t.State = TaskRunning
			}
		case DispatchPrepare:
			if st.Transfer != nil && st.Transfer.Status == TransferUncertain {
				st.Transfer.Status = TransferProposed
			}
		case DispatchCancel:
			t := &st.Tasks[ti]
			if !t.State.Terminal() {
				if cur, ok := st.dispatch(dispatchID(t.ID, t.Turns)); ok {
					e.settleDispatch(st, cur, cur.Allowance)
				}
				e.settleTask(st, ti, TaskCancelled, "RemoteCancelled", nil)
			}
		}
	case DispatchUncertain:
		if d.Status != DispatchCommitted {
			return bad()
		}
		d.Status, d.Reason = DispatchUncertain, truncate(reason)
		switch d.Kind {
		case DispatchStart, DispatchContinue:
			if t := &st.Tasks[ti]; t.State == TaskRunning {
				t.State = TaskUncertain
			}
		case DispatchPrepare:
			if st.Transfer != nil && st.Transfer.Status == TransferProposed {
				st.Transfer.Status = TransferUncertain
			}
		}
	case DispatchRefused:
		if d.Status != DispatchIntended {
			return bad()
		}
		d.Status, d.Reason = DispatchRefused, truncate(reason)
		switch d.Kind {
		case DispatchStart, DispatchContinue:
			e.refund(st, d)
			e.settleTask(st, ti, TaskFailed, ReasonDispatchRefused+": "+d.Reason, nil)
		case DispatchPrepare:
			if st.Transfer != nil && st.Transfer.ID == d.TransferID {
				st.Transfer.Status = TransferRejected
			}
		}
	}
	return nil
}

type manifestBranch struct {
	Member   string    `json:"member"`
	TaskID   string    `json:"taskId"`
	Optional bool      `json:"optional,omitempty"`
	State    TaskState `json:"state"`
	Reason   string    `json:"reason,omitempty"`
	Result   string    `json:"result,omitempty"`
}

// joinParallel is the trusted join: it runs when every branch is terminal.
func (e *Engine) joinParallel(st *State) {
	if e.def.Strategy != api.CollaborationStrategyParallel || st.SynthStarted || len(st.Manifest) > 0 {
		return
	}
	var branches []*Task
	for i := range st.Tasks {
		if t := &st.Tasks[i]; t.Requester == "" && !t.Final {
			if !t.State.Terminal() {
				return
			}
			branches = append(branches, t)
		}
	}
	if len(branches) == 0 {
		return
	}
	var ids []string
	var origins []string
	report := make([]manifestBranch, 0, len(branches))
	for _, t := range branches {
		entry := ManifestEntry{Member: t.Recipient, TaskID: t.ID, Optional: !t.Required, State: t.State, Reason: t.Reason}
		st.Manifest = append(st.Manifest, entry)
		mb := manifestBranch{Member: entry.Member, TaskID: entry.TaskID, Optional: entry.Optional, State: entry.State, Reason: entry.Reason}
		if t.State == TaskSucceeded && t.Result != nil {
			ids = append(ids, t.ID)
			origins = unionOrigins(origins, t.Result.Origins)
			mb.Result = t.Result.Text
		}
		report = append(report, mb)
	}
	if len(ids) == 0 {
		e.beginStop(st, PhaseFailed, ReasonNoBranchSucceeded, nil)
		return
	}
	origins = unionOrigins(origins, []string{ExecutorOrigin})
	if e.def.Synthesizer != "" {
		st.SynthStarted = true
		e.newTask(st, taskSpec{
			Recipient: e.def.Synthesizer, Required: true, Final: true,
			Goal: Content{Origins: origins}, Inputs: ids,
		})
		return
	}
	raw, err := json.Marshal(struct {
		Branches []manifestBranch `json:"branches"`
	}{report})
	if err != nil {
		e.beginStop(st, PhaseFailed, ReasonNoProgress, nil)
		return
	}
	res := Content{Text: string(raw), Origins: origins}
	e.beginStop(st, PhaseSucceeded, "", &res)
}

// ChildResult is one settled task result released to a member.
type ChildResult struct {
	TaskID   string    `json:"taskId"`
	Member   string    `json:"member"`
	State    TaskState `json:"state"`
	Required bool      `json:"required"`
	Reason   string    `json:"reason,omitempty"`
	Result   *Content  `json:"result,omitempty"`
}

// Message is the trusted content a dispatch delivers to its recipient.
type Message struct {
	DispatchID        string
	Kind              DispatchKind
	TaskID            string
	Recipient         string
	Requester         string // empty means the trusted executor
	Turn              int32
	Epoch             uint64
	Goal              Content
	Results           []ChildResult
	Note              string
	Transfer          *Transfer
	Origins           []string
	Allowance         int64
	AllowedOperations []OperationKind
	Target            *Message // the dispatch a Cancel refers to
}

// Message assembles the filtered content for one dispatch.
func (e *Engine) Message(st State, dispatch string) (Message, error) {
	d, ok := st.dispatch(dispatch)
	if !ok {
		return Message{}, ErrUnknownTask
	}
	t, ok := st.Task(d.TaskID)
	if !ok {
		return Message{}, ErrUnknownTask
	}
	m := Message{
		DispatchID: d.ID, Kind: d.Kind, TaskID: d.TaskID, Recipient: d.Recipient, Requester: t.Requester,
		Turn: d.Turn, Epoch: d.Epoch, Note: d.Note, Allowance: d.Allowance,
	}
	switch d.Kind {
	case DispatchStart, DispatchContinue:
		m.Goal = t.Goal
		m.Origins = unionOrigins(t.Goal.Origins)
		for _, id := range d.Inputs {
			in, ok := st.Task(id)
			if !ok {
				return Message{}, ErrUnknownTask
			}
			r := ChildResult{TaskID: in.ID, Member: in.Recipient, State: in.State, Required: in.Required, Reason: in.Reason}
			if in.Result != nil {
				c := *in.Result
				r.Result = &c
				m.Origins = unionOrigins(m.Origins, c.Origins)
			}
			m.Results = append(m.Results, r)
		}
		m.AllowedOperations = append([]OperationKind(nil), d.Allowed...)
		if len(m.AllowedOperations) == 0 {
			m.AllowedOperations = e.allowedOperations(&st, t)
		}
	case DispatchPrepare:
		if st.Transfer == nil || st.Transfer.ID != d.TransferID {
			return Message{}, ErrUnknownTask
		}
		tr := *st.Transfer
		m.Transfer = &tr
		m.Origins = unionOrigins(tr.Context.Origins)
	case DispatchCancel:
		target, err := e.Message(st, dispatchID(d.TaskID, d.Turn))
		if err != nil {
			return Message{}, err
		}
		m.Target = &target
	}
	return m, nil
}

func (e *Engine) allowedOperations(st *State, t *Task) []OperationKind {
	var ops []OperationKind
	if t.Final {
		ops = append(ops, OpFinalize)
	} else {
		ops = append(ops, OpComplete)
	}
	lim := e.def.Limits
	if e.mayDelegate(t) && t.Depth+1 <= lim.MaxDelegationDepth && st.Counters.Delegations < lim.MaxDelegations {
		ops = append(ops, OpDelegate)
	}
	if e.def.Strategy == api.CollaborationStrategyHandoff && t.Final &&
		st.Handoffs < e.def.MaxHandoffs && st.Counters.Delegations < lim.MaxDelegations {
		ops = append(ops, OpHandoff)
	}
	return ops
}
