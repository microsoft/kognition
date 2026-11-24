package store

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"math"
	"reflect"
	"slices"
	"strings"
	"time"
	"unicode/utf8"
)

type WorkflowActivationKey struct {
	Workspace    string `json:"workspace"`
	RunUID       string `json:"runUID"`
	Step         string `json:"step"`
	ActivationID string `json:"activationID"`
	Attempt      int64  `json:"attempt"`
}

type WorkflowActivationSpec struct {
	Key           WorkflowActivationKey `json:"key"`
	BindingDigest string                `json:"bindingDigest"`
	Actors        []string              `json:"actors"`
	MaxEffects    int                   `json:"maxEffects"`
	MaxBytes      int                   `json:"maxBytes"`
	ExpiresAt     time.Time             `json:"expiresAt"`
	InitialOwner  string                `json:"initialOwner,omitempty"`
	MaxHandoffs   int                   `json:"maxHandoffs,omitempty"`
}

type WorkflowEffectClaim struct {
	Kind         string `json:"kind"`
	Namespace    string `json:"namespace"`
	UID          string `json:"uid"`
	Claimant     string `json:"claimant"`
	ProcessNonce string `json:"processNonce"`
	FencingToken int64  `json:"fencingToken"`
}

type WorkflowEffect struct {
	ID                      string                   `json:"id"`
	BindingDigest           string                   `json:"bindingDigest"`
	Actor                   string                   `json:"actor"`
	Plane                   string                   `json:"plane"`
	Epoch                   int64                    `json:"epoch"`
	State                   string                   `json:"state"`
	Claim                   WorkflowEffectClaim      `json:"claim,omitempty"`
	Disposition             string                   `json:"disposition,omitempty"`
	EvidenceDigest          string                   `json:"evidenceDigest,omitempty"`
	ProofDigest             string                   `json:"proofDigest,omitempty"`
	TaskObservation         *WorkflowTaskObservation `json:"taskObservation,omitempty"`
	TaskCreateReceiptDigest string                   `json:"taskCreateReceiptDigest,omitempty"`
}

type WorkflowTaskObservation struct {
	Protocol     string `json:"protocol"`
	ActionDigest string `json:"actionDigest"`
	TaskID       string `json:"taskID"`
	ContextID    string `json:"contextID"`
}

type WorkflowHandoff struct {
	ID               string `json:"id"`
	SourceOwner      string `json:"sourceOwner"`
	SourceEpoch      int64  `json:"sourceEpoch"`
	Recipient        string `json:"recipient"`
	ContextDigest    string `json:"contextDigest"`
	AcceptanceDigest string `json:"acceptanceDigest,omitempty"`
	SuccessorEpoch   int64  `json:"successorEpoch,omitempty"`
	State            string `json:"state"`
}

type WorkflowActivation struct {
	Spec      WorkflowActivationSpec    `json:"spec"`
	Version   int64                     `json:"version"`
	Epoch     int64                     `json:"epoch"`
	Mode      string                    `json:"mode"`
	StopID    string                    `json:"stopID,omitempty"`
	StopMode  string                    `json:"stopMode,omitempty"`
	QuiesceID string                    `json:"quiesceID,omitempty"`
	Owner     string                    `json:"owner,omitempty"`
	Handoffs  []WorkflowHandoff         `json:"handoffs,omitempty"`
	Effects   map[string]WorkflowEffect `json:"effects"`
}

type WorkflowActivationMutation struct {
	ExpectedVersion         int64
	Kind                    string
	Effect                  WorkflowEffect
	Claim                   WorkflowEffectClaim
	StopID                  string
	StopMode                string
	Disposition             string
	EvidenceDigest          string
	ProofDigest             string
	Actor                   string
	Epoch                   int64
	HandoffID               string
	Recipient               string
	ContextDigest           string
	AcceptanceDigest        string
	TaskObservation         WorkflowTaskObservation
	TaskCreateReceiptDigest string
}

type WorkflowActivationResult struct {
	Activation      WorkflowActivation
	Changed         bool
	DispatchGranted bool
	OwnerActivated  bool
}

type WorkflowActivationStore interface {
	CreateWorkflowActivation(context.Context, WorkflowActivationSpec) (WorkflowActivation, error)
	GetWorkflowActivation(context.Context, WorkflowActivationKey) (WorkflowActivation, error)
	MutateWorkflowActivation(context.Context, WorkflowActivationKey, WorkflowActivationMutation) (WorkflowActivationResult, error)
}

const (
	WorkflowGateActive    = "Active"
	WorkflowGateQuiescing = "Quiescing"
	WorkflowGateStopping  = "Stopping"
	WorkflowGateStopped   = "Stopped"
	WorkflowGateUncertain = "OutcomeUncertain"

	WorkflowEffectRegistered = "Registered"
	WorkflowEffectCommitted  = "DispatchCommitted"
	WorkflowEffectSettled    = "Settled"
	WorkflowEffectAborted    = "Aborted"
	WorkflowEffectUncertain  = "Uncertain"

	WorkflowGateRegister        = "Register"
	WorkflowGateCommit          = "CommitDispatch"
	WorkflowGateStop            = "Stop"
	WorkflowGateSettle          = "Settle"
	WorkflowGateResolve         = "Resolve"
	WorkflowGateDrain           = "Drain"
	WorkflowGateProposeHandoff  = "ProposeHandoff"
	WorkflowGateAcceptHandoff   = "AcceptHandoff"
	WorkflowGateActivateHandoff = "ActivateHandoff"
	WorkflowGateObserveTask     = "ObserveTask"
	WorkflowGateAdmitTaskCreate = "AdmitTaskCreate"
	WorkflowTaskA2AGRPC         = "a2a-grpc-1.0"

	WorkflowHandoffProposed  = "Proposed"
	WorkflowHandoffAccepted  = "Accepted"
	WorkflowHandoffCommitted = "Committed"

	WorkflowGateMaxEffects  = 128
	WorkflowGateMaxBytes    = 256 << 10
	WorkflowGateMaxHandoffs = 100
)

func ValidWorkflowActivationKey(key WorkflowActivationKey) bool {
	return boundedWorkflowValue(key.Workspace, 63) && boundedWorkflowValue(key.RunUID, 128) &&
		boundedWorkflowValue(key.Step, 128) && boundedWorkflowValue(key.ActivationID, 128) && key.Attempt > 0
}

func NewWorkflowActivation(spec WorkflowActivationSpec, now time.Time) (WorkflowActivation, error) {
	if !ValidWorkflowActivationKey(spec.Key) || !workflowDigest(spec.BindingDigest) ||
		len(spec.Actors) == 0 || len(spec.Actors) > WorkflowGateMaxEffects ||
		spec.MaxEffects < 1 || spec.MaxEffects > WorkflowGateMaxEffects ||
		spec.MaxBytes < 1 || spec.MaxBytes > WorkflowGateMaxBytes || now.IsZero() || spec.ExpiresAt.IsZero() || !spec.ExpiresAt.After(now) {
		return WorkflowActivation{}, ErrConflict
	}
	spec.Actors = slices.Clone(spec.Actors)
	slices.Sort(spec.Actors)
	for index, actor := range spec.Actors {
		if !workflowDigest(actor) || (index > 0 && actor == spec.Actors[index-1]) {
			return WorkflowActivation{}, ErrConflict
		}
	}
	if spec.InitialOwner == "" {
		if spec.MaxHandoffs != 0 {
			return WorkflowActivation{}, ErrConflict
		}
	} else if !workflowDigest(spec.InitialOwner) || !slices.Contains(spec.Actors, spec.InitialOwner) ||
		len(spec.Actors) < 2 || spec.MaxHandoffs < 1 || spec.MaxHandoffs > WorkflowGateMaxHandoffs {
		return WorkflowActivation{}, ErrConflict
	}
	spec.ExpiresAt = spec.ExpiresAt.UTC()
	activation := WorkflowActivation{Spec: spec, Version: 1, Epoch: 1, Mode: WorkflowGateActive, Owner: spec.InitialOwner, Effects: map[string]WorkflowEffect{}}
	if !workflowActivationHasCapacity(activation) {
		return WorkflowActivation{}, ErrConflict
	}
	return activation, nil
}

func ApplyWorkflowActivation(current WorkflowActivation, mutation WorkflowActivationMutation, now time.Time) (WorkflowActivationResult, error) {
	if ValidateWorkflowActivation(current) != nil || mutation.ExpectedVersion != current.Version || now.IsZero() {
		return WorkflowActivationResult{}, ErrConflict
	}
	next := current
	next.Spec.Actors = slices.Clone(current.Spec.Actors)
	next.Handoffs = slices.Clone(current.Handoffs)
	next.Effects = make(map[string]WorkflowEffect, len(current.Effects))
	for effectID, effect := range current.Effects {
		if effect.TaskObservation != nil {
			observation := *effect.TaskObservation
			effect.TaskObservation = &observation
		}
		next.Effects[effectID] = effect
	}
	granted := false
	activated := false
	switch mutation.Kind {
	case WorkflowGateProposeHandoff, WorkflowGateAcceptHandoff, WorkflowGateActivateHandoff:
		var err error
		next, activated, err = applyWorkflowHandoff(next, mutation, now)
		if err != nil {
			return WorkflowActivationResult{}, err
		}
	case WorkflowGateRegister:
		effect := mutation.Effect
		if !validWorkflowEffectRegistration(effect, current) {
			return WorkflowActivationResult{}, ErrConflict
		}
		if existing, found := next.Effects[effect.ID]; found {
			if existing.BindingDigest != effect.BindingDigest || existing.Actor != effect.Actor ||
				existing.Plane != effect.Plane || existing.Epoch != effect.Epoch {
				return WorkflowActivationResult{}, ErrConflict
			}
			break
		}
		if !workflowCanDispatch(current, effect.Epoch, effect.Actor, now) || len(next.Effects) >= current.Spec.MaxEffects {
			return WorkflowActivationResult{}, ErrConflict
		}
		effect.State = WorkflowEffectRegistered
		next.Effects[effect.ID] = effect
		if !workflowActivationHasCapacity(next) {
			return WorkflowActivationResult{}, ErrConflict
		}
	case WorkflowGateCommit:
		effect, found := next.Effects[mutation.Effect.ID]
		if !found || !sameWorkflowEffectIdentity(effect, mutation.Effect) || !validWorkflowClaim(mutation.Claim) {
			return WorkflowActivationResult{}, ErrConflict
		}
		if effect.State == WorkflowEffectCommitted || effect.State == WorkflowEffectSettled || effect.State == WorkflowEffectUncertain {
			if effect.Claim != mutation.Claim {
				return WorkflowActivationResult{}, ErrConflict
			}
			break
		}
		if effect.State != WorkflowEffectRegistered || !workflowCanDispatch(current, effect.Epoch, effect.Actor, now) {
			return WorkflowActivationResult{}, ErrConflict
		}
		effect.State, effect.Claim = WorkflowEffectCommitted, mutation.Claim
		next.Effects[effect.ID] = effect
		granted = true
	case WorkflowGateAdmitTaskCreate:
		effect, found := next.Effects[mutation.Effect.ID]
		if !found || !sameWorkflowEffectIdentity(effect, mutation.Effect) || !validWorkflowClaim(mutation.Claim) ||
			effect.Claim != mutation.Claim || effect.Plane != "communication" || effect.State != WorkflowEffectCommitted ||
			!workflowCanDispatch(current, effect.Epoch, effect.Actor, now) || !workflowDigest(mutation.TaskCreateReceiptDigest) ||
			!validWorkflowTaskObservation(mutation.TaskObservation) || effect.TaskCreateReceiptDigest != "" ||
			effect.TaskObservation != nil && *effect.TaskObservation != mutation.TaskObservation {
			return WorkflowActivationResult{}, ErrConflict
		}
		observation := mutation.TaskObservation
		effect.TaskObservation = &observation
		effect.TaskCreateReceiptDigest = mutation.TaskCreateReceiptDigest
		next.Effects[effect.ID] = effect
	case WorkflowGateObserveTask:
		effect, found := next.Effects[mutation.Effect.ID]
		if !found || !sameWorkflowEffectIdentity(effect, mutation.Effect) || !validWorkflowClaim(mutation.Claim) ||
			effect.Claim != mutation.Claim || effect.Plane != "communication" || !validWorkflowTaskObservation(mutation.TaskObservation) ||
			(effect.State != WorkflowEffectCommitted && effect.State != WorkflowEffectUncertain && effect.State != WorkflowEffectSettled) {
			return WorkflowActivationResult{}, ErrConflict
		}
		if effect.TaskObservation != nil {
			if *effect.TaskObservation != mutation.TaskObservation {
				return WorkflowActivationResult{}, ErrConflict
			}
			break
		}
		observation := mutation.TaskObservation
		effect.TaskObservation = &observation
		next.Effects[effect.ID] = effect
	case WorkflowGateStop:
		if !boundedWorkflowValue(mutation.StopID, 128) ||
			(mutation.StopMode != WorkflowGateStopping && mutation.StopMode != WorkflowGateQuiescing) {
			return WorkflowActivationResult{}, ErrConflict
		}
		if current.Owner != "" {
			index := workflowHandoffIndex(current.Handoffs, mutation.StopID)
			if mutation.StopMode == WorkflowGateQuiescing {
				if index < 0 || current.Handoffs[index].State != WorkflowHandoffAccepted ||
					mutation.Actor != current.Owner || mutation.Epoch != current.Handoffs[index].SourceEpoch {
					return WorkflowActivationResult{}, ErrConflict
				}
			} else if index >= 0 {
				return WorkflowActivationResult{}, ErrConflict
			}
		}
		if current.StopID != "" {
			if current.StopID == mutation.StopID && current.StopMode == mutation.StopMode {
				break
			}
			if current.StopMode != WorkflowGateQuiescing || mutation.StopMode != WorkflowGateStopping || current.StopID == mutation.StopID || current.Epoch == math.MaxInt64 {
				return WorkflowActivationResult{}, ErrConflict
			}
			next.QuiesceID = current.StopID
			next.StopID, next.StopMode = mutation.StopID, WorkflowGateStopping
			if current.Mode != WorkflowGateUncertain {
				next.Mode = WorkflowGateStopping
			}
			next.Epoch++
			break
		}
		if current.Mode != WorkflowGateActive && current.Mode != WorkflowGateUncertain {
			return WorkflowActivationResult{}, ErrConflict
		}
		if current.Epoch == math.MaxInt64 {
			return WorkflowActivationResult{}, ErrConflict
		}
		next.Mode, next.StopID, next.StopMode = mutation.StopMode, mutation.StopID, mutation.StopMode
		if current.Mode == WorkflowGateUncertain {
			next.Mode = WorkflowGateUncertain
		}
		if mutation.StopMode == WorkflowGateQuiescing {
			next.QuiesceID = mutation.StopID
		}
		next.Epoch++
	case WorkflowGateSettle, WorkflowGateResolve:
		effect, found := next.Effects[mutation.Effect.ID]
		if !found || !sameWorkflowEffectIdentity(effect, mutation.Effect) || !workflowDigest(mutation.EvidenceDigest) {
			return WorkflowActivationResult{}, ErrConflict
		}
		if effect.State == WorkflowEffectSettled || effect.State == WorkflowEffectAborted {
			if effect.Disposition != mutation.Disposition || effect.EvidenceDigest != mutation.EvidenceDigest || effect.Claim != mutation.Claim {
				return WorkflowActivationResult{}, ErrConflict
			}
			if mutation.Kind == WorkflowGateResolve && effect.ProofDigest != mutation.ProofDigest {
				return WorkflowActivationResult{}, ErrConflict
			}
			break
		}
		if mutation.Disposition == "NotDispatched" {
			if effect.State != WorkflowEffectRegistered || (mutation.Claim != (WorkflowEffectClaim{}) && !validWorkflowClaim(mutation.Claim)) || current.Mode == WorkflowGateActive {
				return WorkflowActivationResult{}, ErrConflict
			}
			effect.State, effect.Claim = WorkflowEffectAborted, mutation.Claim
		} else {
			if effect.State != WorkflowEffectCommitted && effect.State != WorkflowEffectUncertain {
				return WorkflowActivationResult{}, ErrConflict
			}
			if effect.Claim != mutation.Claim {
				return WorkflowActivationResult{}, ErrConflict
			}
			switch mutation.Disposition {
			case "Succeeded", "Failed":
				if effect.State == WorkflowEffectUncertain && (mutation.Kind != WorkflowGateResolve || !workflowDigest(mutation.ProofDigest)) {
					return WorkflowActivationResult{}, ErrConflict
				}
				if effect.State == WorkflowEffectUncertain {
					effect.ProofDigest = mutation.ProofDigest
				}
				effect.State = WorkflowEffectSettled
			case "Uncertain":
				if effect.State == WorkflowEffectUncertain {
					if effect.EvidenceDigest != mutation.EvidenceDigest {
						return WorkflowActivationResult{}, ErrConflict
					}
					break
				}
				if current.Epoch == math.MaxInt64 {
					return WorkflowActivationResult{}, ErrConflict
				}
				effect.State, next.Mode = WorkflowEffectUncertain, WorkflowGateUncertain
				next.Epoch++
			default:
				return WorkflowActivationResult{}, ErrConflict
			}
		}
		effect.Disposition, effect.EvidenceDigest = mutation.Disposition, mutation.EvidenceDigest
		next.Effects[effect.ID] = effect
	case WorkflowGateDrain:
		if current.StopID == "" || mutation.StopID != current.StopID {
			return WorkflowActivationResult{}, ErrConflict
		}
		if !workflowEffectsSettled(next) {
			return WorkflowActivationResult{}, ErrConflict
		}
		next.Mode = WorkflowGateStopped
	default:
		return WorkflowActivationResult{}, ErrConflict
	}
	changed := !reflect.DeepEqual(current, next)
	if changed {
		if current.Version == math.MaxInt64 {
			return WorkflowActivationResult{}, ErrConflict
		}
		next.Version++
	}
	if ValidateWorkflowActivation(next) != nil {
		return WorkflowActivationResult{}, ErrConflict
	}
	return WorkflowActivationResult{Activation: next, Changed: changed, DispatchGranted: granted, OwnerActivated: activated}, nil
}

func sameWorkflowEffectIdentity(first, second WorkflowEffect) bool {
	return first.ID == second.ID && first.BindingDigest == second.BindingDigest && first.Actor == second.Actor &&
		first.Plane == second.Plane && first.Epoch == second.Epoch
}

func ValidateWorkflowActivation(activation WorkflowActivation) error {
	if activation.Version < 1 || activation.Epoch < 1 || activation.Spec.ExpiresAt.IsZero() || activation.Effects == nil || !workflowActivationFits(activation) {
		return ErrConflict
	}
	initial, err := NewWorkflowActivation(activation.Spec, activation.Spec.ExpiresAt.Add(-time.Nanosecond))
	if err != nil || !reflect.DeepEqual(initial.Spec, activation.Spec) {
		return ErrConflict
	}
	if activation.StopID == "" {
		if activation.StopMode != "" || (activation.Mode != WorkflowGateActive && activation.Mode != WorkflowGateUncertain) {
			return ErrConflict
		}
	} else if !boundedWorkflowValue(activation.StopID, 128) ||
		(activation.StopMode != WorkflowGateStopping && activation.StopMode != WorkflowGateQuiescing) ||
		(activation.Mode != activation.StopMode && activation.Mode != WorkflowGateStopped && activation.Mode != WorkflowGateUncertain) {
		return ErrConflict
	}
	if activation.QuiesceID != "" && (!boundedWorkflowValue(activation.QuiesceID, 128) || activation.StopID == "" ||
		(activation.StopMode == WorkflowGateQuiescing && activation.QuiesceID != activation.StopID) ||
		(activation.StopMode == WorkflowGateStopping && activation.QuiesceID == activation.StopID)) {
		return ErrConflict
	}
	if !validWorkflowHandoffs(activation) {
		return ErrConflict
	}
	for effectID, effect := range activation.Effects {
		registration := WorkflowEffect{ID: effect.ID, BindingDigest: effect.BindingDigest, Actor: effect.Actor, Plane: effect.Plane, Epoch: effect.Epoch}
		if effectID != effect.ID || effect.Epoch > activation.Epoch || !validWorkflowEffectRegistration(registration, activation) {
			return ErrConflict
		}
		if effect.TaskObservation != nil && (effect.Plane != "communication" || !validWorkflowTaskObservation(*effect.TaskObservation) ||
			(effect.State != WorkflowEffectCommitted && effect.State != WorkflowEffectUncertain && effect.State != WorkflowEffectSettled)) {
			return ErrConflict
		}
		if effect.TaskCreateReceiptDigest != "" && (!workflowDigest(effect.TaskCreateReceiptDigest) || effect.TaskObservation == nil) {
			return ErrConflict
		}
		switch effect.State {
		case WorkflowEffectRegistered:
			if effect.Claim != (WorkflowEffectClaim{}) || effect.Disposition != "" || effect.EvidenceDigest != "" || effect.ProofDigest != "" || activation.Mode == WorkflowGateStopped {
				return ErrConflict
			}
		case WorkflowEffectCommitted:
			if !validWorkflowClaim(effect.Claim) || effect.Disposition != "" || effect.EvidenceDigest != "" || effect.ProofDigest != "" || activation.Mode == WorkflowGateStopped {
				return ErrConflict
			}
		case WorkflowEffectSettled:
			if !validWorkflowClaim(effect.Claim) || (effect.Disposition != "Succeeded" && effect.Disposition != "Failed") || !workflowDigest(effect.EvidenceDigest) || (effect.ProofDigest != "" && !workflowDigest(effect.ProofDigest)) {
				return ErrConflict
			}
		case WorkflowEffectAborted:
			if (effect.Claim != (WorkflowEffectClaim{}) && !validWorkflowClaim(effect.Claim)) || effect.Disposition != "NotDispatched" || !workflowDigest(effect.EvidenceDigest) || effect.ProofDigest != "" {
				return ErrConflict
			}
		case WorkflowEffectUncertain:
			if !validWorkflowClaim(effect.Claim) || effect.Disposition != "Uncertain" || !workflowDigest(effect.EvidenceDigest) || effect.ProofDigest != "" || activation.Mode == WorkflowGateActive || activation.Mode == WorkflowGateStopped {
				return ErrConflict
			}
		default:
			return ErrConflict
		}
	}
	return nil
}

func workflowCanDispatch(activation WorkflowActivation, epoch int64, actor string, now time.Time) bool {
	return activation.Mode == WorkflowGateActive && activation.Epoch == epoch && now.Before(activation.Spec.ExpiresAt) &&
		(activation.Owner == "" || activation.Owner == actor)
}

func validWorkflowEffectRegistration(effect WorkflowEffect, activation WorkflowActivation) bool {
	return boundedWorkflowValue(effect.ID, 128) && workflowDigest(effect.BindingDigest) &&
		slices.Contains(activation.Spec.Actors, effect.Actor) && effect.Epoch > 0 &&
		slices.Contains([]string{"model", "tool", "memory", "knowledge", "communication"}, effect.Plane) &&
		effect.State == "" && effect.Claim == (WorkflowEffectClaim{}) && effect.Disposition == "" && effect.EvidenceDigest == "" && effect.ProofDigest == "" &&
		effect.TaskObservation == nil && effect.TaskCreateReceiptDigest == ""
}

func validWorkflowTaskObservation(observation WorkflowTaskObservation) bool {
	return observation.Protocol == WorkflowTaskA2AGRPC && workflowDigest(observation.ActionDigest) &&
		boundedWorkflowValue(observation.TaskID, 128) && boundedWorkflowValue(observation.ContextID, 128) &&
		utf8.ValidString(observation.TaskID) && utf8.ValidString(observation.ContextID) && !strings.ContainsAny(observation.TaskID+observation.ContextID, "\t")
}

func validWorkflowClaim(claim WorkflowEffectClaim) bool {
	return boundedWorkflowValue(claim.Kind, 64) && boundedWorkflowValue(claim.Namespace, 63) &&
		boundedWorkflowValue(claim.UID, 128) && boundedWorkflowValue(claim.Claimant, 128) &&
		boundedWorkflowValue(claim.ProcessNonce, 128) && claim.FencingToken > 0
}

func boundedWorkflowValue(value string, limit int) bool {
	return value != "" && len(value) <= limit && strings.TrimSpace(value) == value && !strings.ContainsAny(value, "\x00\r\n")
}

func workflowDigest(value string) bool {
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == 32 && strings.ToLower(value) == value
}

func workflowActivationFits(activation WorkflowActivation) bool {
	if activation.Spec.MaxEffects < 1 || activation.Spec.MaxEffects > WorkflowGateMaxEffects ||
		len(activation.Effects) > activation.Spec.MaxEffects || activation.Spec.MaxBytes < 1 || activation.Spec.MaxBytes > WorkflowGateMaxBytes {
		return false
	}
	encoded, err := json.Marshal(activation)
	return err == nil && len(encoded) <= activation.Spec.MaxBytes
}

func workflowActivationHasCapacity(activation WorkflowActivation) bool {
	reserved := activation
	reserved.Version, reserved.Epoch = math.MaxInt64, math.MaxInt64
	reserved.Mode, reserved.StopMode = WorkflowGateUncertain, WorkflowGateQuiescing
	reserved.StopID = strings.Repeat("<", 128)
	reserved.QuiesceID = strings.Repeat("<", 128)
	reserved.Handoffs = make([]WorkflowHandoff, activation.Spec.MaxHandoffs)
	for index := range reserved.Handoffs {
		reserved.Handoffs[index] = WorkflowHandoff{
			ID: strings.Repeat("<", 128), SourceOwner: strings.Repeat("a", 64), SourceEpoch: math.MaxInt64,
			Recipient: strings.Repeat("a", 64), ContextDigest: strings.Repeat("a", 64), AcceptanceDigest: strings.Repeat("a", 64),
			SuccessorEpoch: math.MaxInt64, State: WorkflowHandoffCommitted,
		}
	}
	reserved.Effects = make(map[string]WorkflowEffect, len(activation.Effects))
	for effectID, effect := range activation.Effects {
		effect.State, effect.Disposition = WorkflowEffectCommitted, "NotDispatched"
		if effect.Plane == "communication" {
			effect.TaskObservation = &WorkflowTaskObservation{Protocol: WorkflowTaskA2AGRPC, ActionDigest: strings.Repeat("a", 64),
				TaskID: strings.Repeat("<", 128), ContextID: strings.Repeat("<", 128)}
		}
		effect.EvidenceDigest, effect.ProofDigest = strings.Repeat("a", 64), strings.Repeat("a", 64)
		effect.Claim = WorkflowEffectClaim{
			Kind: strings.Repeat("<", 64), Namespace: strings.Repeat("<", 63), UID: strings.Repeat("<", 128),
			Claimant: strings.Repeat("<", 128), ProcessNonce: strings.Repeat("<", 128), FencingToken: math.MaxInt64,
		}
		reserved.Effects[effectID] = effect
	}
	return workflowActivationFits(reserved)
}

func workflowHandoffIndex(handoffs []WorkflowHandoff, handoffID string) int {
	for index, handoff := range handoffs {
		if handoff.ID == handoffID {
			return index
		}
	}
	return -1
}

func workflowEffectsSettled(activation WorkflowActivation) bool {
	for _, effect := range activation.Effects {
		if effect.State != WorkflowEffectSettled && effect.State != WorkflowEffectAborted {
			return false
		}
	}
	return true
}

func validWorkflowHandoffs(activation WorkflowActivation) bool {
	if activation.Spec.InitialOwner == "" {
		return activation.Owner == "" && len(activation.Handoffs) == 0
	}
	if len(activation.Handoffs) > activation.Spec.MaxHandoffs {
		return false
	}
	owner := activation.Spec.InitialOwner
	previousEpoch := int64(0)
	seen := make(map[string]bool, len(activation.Handoffs))
	for index, handoff := range activation.Handoffs {
		if !boundedWorkflowValue(handoff.ID, 128) || seen[handoff.ID] || handoff.SourceOwner != owner ||
			handoff.SourceEpoch < 1 || handoff.SourceEpoch < previousEpoch || handoff.SourceEpoch > activation.Epoch ||
			!slices.Contains(activation.Spec.Actors, handoff.Recipient) || handoff.Recipient == owner || !workflowDigest(handoff.ContextDigest) {
			return false
		}
		seen[handoff.ID] = true
		switch handoff.State {
		case WorkflowHandoffProposed:
			if index != len(activation.Handoffs)-1 || handoff.AcceptanceDigest != "" || handoff.SuccessorEpoch != 0 {
				return false
			}
		case WorkflowHandoffAccepted:
			if index != len(activation.Handoffs)-1 || !workflowDigest(handoff.AcceptanceDigest) || handoff.SuccessorEpoch != 0 {
				return false
			}
		case WorkflowHandoffCommitted:
			if !workflowDigest(handoff.AcceptanceDigest) || handoff.SuccessorEpoch <= handoff.SourceEpoch || handoff.SuccessorEpoch > activation.Epoch {
				return false
			}
			owner, previousEpoch = handoff.Recipient, handoff.SuccessorEpoch
		default:
			return false
		}
	}
	return owner == activation.Owner
}

func applyWorkflowHandoff(activation WorkflowActivation, mutation WorkflowActivationMutation, now time.Time) (WorkflowActivation, bool, error) {
	if activation.Owner == "" || !boundedWorkflowValue(mutation.HandoffID, 128) {
		return WorkflowActivation{}, false, ErrConflict
	}
	index := workflowHandoffIndex(activation.Handoffs, mutation.HandoffID)
	if mutation.Kind == WorkflowGateProposeHandoff {
		if !workflowDigest(mutation.ContextDigest) || !slices.Contains(activation.Spec.Actors, mutation.Recipient) || mutation.Actor == mutation.Recipient {
			return WorkflowActivation{}, false, ErrConflict
		}
		if index >= 0 {
			handoff := activation.Handoffs[index]
			if handoff.SourceOwner != mutation.Actor || handoff.SourceEpoch != mutation.Epoch || handoff.Recipient != mutation.Recipient || handoff.ContextDigest != mutation.ContextDigest {
				return WorkflowActivation{}, false, ErrConflict
			}
			return activation, false, nil
		}
		if !workflowCanDispatch(activation, mutation.Epoch, mutation.Actor, now) || len(activation.Handoffs) >= activation.Spec.MaxHandoffs ||
			(len(activation.Handoffs) > 0 && activation.Handoffs[len(activation.Handoffs)-1].State != WorkflowHandoffCommitted) {
			return WorkflowActivation{}, false, ErrConflict
		}
		activation.Handoffs = append(activation.Handoffs, WorkflowHandoff{ID: mutation.HandoffID, SourceOwner: mutation.Actor, SourceEpoch: mutation.Epoch,
			Recipient: mutation.Recipient, ContextDigest: mutation.ContextDigest, State: WorkflowHandoffProposed})
		return activation, false, nil
	}
	if index < 0 {
		return WorkflowActivation{}, false, ErrConflict
	}
	handoff := activation.Handoffs[index]
	if mutation.ContextDigest != handoff.ContextDigest || mutation.Epoch != handoff.SourceEpoch || mutation.Recipient != handoff.Recipient {
		return WorkflowActivation{}, false, ErrConflict
	}
	if mutation.Kind == WorkflowGateAcceptHandoff {
		if mutation.Actor != handoff.Recipient || !workflowDigest(mutation.AcceptanceDigest) {
			return WorkflowActivation{}, false, ErrConflict
		}
		if handoff.State != WorkflowHandoffProposed {
			if handoff.AcceptanceDigest != mutation.AcceptanceDigest {
				return WorkflowActivation{}, false, ErrConflict
			}
			return activation, false, nil
		}
		if !workflowCanDispatch(activation, handoff.SourceEpoch, handoff.SourceOwner, now) {
			return WorkflowActivation{}, false, ErrConflict
		}
		handoff.State, handoff.AcceptanceDigest = WorkflowHandoffAccepted, mutation.AcceptanceDigest
		activation.Handoffs[index] = handoff
		return activation, false, nil
	}
	if mutation.Actor != handoff.SourceOwner || mutation.AcceptanceDigest != handoff.AcceptanceDigest || handoff.State == WorkflowHandoffProposed {
		return WorkflowActivation{}, false, ErrConflict
	}
	if handoff.State == WorkflowHandoffCommitted {
		return activation, false, nil
	}
	if (activation.Mode != WorkflowGateQuiescing && activation.Mode != WorkflowGateStopped) ||
		activation.StopMode != WorkflowGateQuiescing || activation.StopID != handoff.ID || activation.QuiesceID != handoff.ID ||
		activation.Owner != handoff.SourceOwner || activation.Epoch <= handoff.SourceEpoch || !now.Before(activation.Spec.ExpiresAt) || !workflowEffectsSettled(activation) {
		return WorkflowActivation{}, false, ErrConflict
	}
	handoff.State, handoff.SuccessorEpoch = WorkflowHandoffCommitted, activation.Epoch
	activation.Handoffs[index] = handoff
	activation.Owner, activation.Mode = handoff.Recipient, WorkflowGateActive
	activation.StopID, activation.StopMode, activation.QuiesceID = "", "", ""
	return activation, true, nil
}
