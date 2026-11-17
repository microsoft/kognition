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
	ID             string              `json:"id"`
	BindingDigest  string              `json:"bindingDigest"`
	Actor          string              `json:"actor"`
	Plane          string              `json:"plane"`
	Epoch          int64               `json:"epoch"`
	State          string              `json:"state"`
	Claim          WorkflowEffectClaim `json:"claim,omitempty"`
	Disposition    string              `json:"disposition,omitempty"`
	EvidenceDigest string              `json:"evidenceDigest,omitempty"`
	ProofDigest    string              `json:"proofDigest,omitempty"`
}

type WorkflowActivation struct {
	Spec     WorkflowActivationSpec    `json:"spec"`
	Version  int64                     `json:"version"`
	Epoch    int64                     `json:"epoch"`
	Mode     string                    `json:"mode"`
	StopID   string                    `json:"stopID,omitempty"`
	StopMode string                    `json:"stopMode,omitempty"`
	Effects  map[string]WorkflowEffect `json:"effects"`
}

type WorkflowActivationMutation struct {
	ExpectedVersion int64
	Kind            string
	Effect          WorkflowEffect
	Claim           WorkflowEffectClaim
	StopID          string
	StopMode        string
	Disposition     string
	EvidenceDigest  string
	ProofDigest     string
}

type WorkflowActivationResult struct {
	Activation      WorkflowActivation
	Changed         bool
	DispatchGranted bool
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

	WorkflowGateRegister = "Register"
	WorkflowGateCommit   = "CommitDispatch"
	WorkflowGateStop     = "Stop"
	WorkflowGateSettle   = "Settle"
	WorkflowGateResolve  = "Resolve"
	WorkflowGateDrain    = "Drain"

	WorkflowGateMaxEffects = 128
	WorkflowGateMaxBytes   = 256 << 10
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
	spec.ExpiresAt = spec.ExpiresAt.UTC()
	activation := WorkflowActivation{Spec: spec, Version: 1, Epoch: 1, Mode: WorkflowGateActive, Effects: map[string]WorkflowEffect{}}
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
	next.Effects = make(map[string]WorkflowEffect, len(current.Effects))
	for effectID, effect := range current.Effects {
		next.Effects[effectID] = effect
	}
	granted := false
	switch mutation.Kind {
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
		if !workflowCanDispatch(current, effect.Epoch, now) || len(next.Effects) >= current.Spec.MaxEffects {
			return WorkflowActivationResult{}, ErrConflict
		}
		effect.State = WorkflowEffectRegistered
		next.Effects[effect.ID] = effect
		if !workflowActivationHasCapacity(next) {
			return WorkflowActivationResult{}, ErrConflict
		}
	case WorkflowGateCommit:
		effect, found := next.Effects[mutation.Effect.ID]
		if !found || effect.BindingDigest != mutation.Effect.BindingDigest || !validWorkflowClaim(mutation.Claim) {
			return WorkflowActivationResult{}, ErrConflict
		}
		if effect.State == WorkflowEffectCommitted || effect.State == WorkflowEffectSettled || effect.State == WorkflowEffectUncertain {
			if effect.Claim != mutation.Claim {
				return WorkflowActivationResult{}, ErrConflict
			}
			break
		}
		if effect.State != WorkflowEffectRegistered || !workflowCanDispatch(current, effect.Epoch, now) {
			return WorkflowActivationResult{}, ErrConflict
		}
		effect.State, effect.Claim = WorkflowEffectCommitted, mutation.Claim
		next.Effects[effect.ID] = effect
		granted = true
	case WorkflowGateStop:
		if !boundedWorkflowValue(mutation.StopID, 128) ||
			(mutation.StopMode != WorkflowGateStopping && mutation.StopMode != WorkflowGateQuiescing) {
			return WorkflowActivationResult{}, ErrConflict
		}
		if current.StopID != "" {
			if current.StopID != mutation.StopID || current.StopMode != mutation.StopMode {
				return WorkflowActivationResult{}, ErrConflict
			}
			break
		}
		if current.Mode != WorkflowGateActive && current.Mode != WorkflowGateUncertain {
			return WorkflowActivationResult{}, ErrConflict
		}
		if current.Epoch == math.MaxInt64 {
			return WorkflowActivationResult{}, ErrConflict
		}
		next.Mode, next.StopID, next.StopMode = mutation.StopMode, mutation.StopID, mutation.StopMode
		next.Epoch++
	case WorkflowGateSettle, WorkflowGateResolve:
		effect, found := next.Effects[mutation.Effect.ID]
		if !found || effect.BindingDigest != mutation.Effect.BindingDigest || !workflowDigest(mutation.EvidenceDigest) {
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
		for _, effect := range next.Effects {
			if effect.State != WorkflowEffectSettled && effect.State != WorkflowEffectAborted {
				return WorkflowActivationResult{}, ErrConflict
			}
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
	if !workflowActivationFits(next) {
		return WorkflowActivationResult{}, ErrConflict
	}
	return WorkflowActivationResult{Activation: next, Changed: changed, DispatchGranted: granted}, nil
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
	for effectID, effect := range activation.Effects {
		registration := WorkflowEffect{ID: effect.ID, BindingDigest: effect.BindingDigest, Actor: effect.Actor, Plane: effect.Plane, Epoch: effect.Epoch}
		if effectID != effect.ID || effect.Epoch > activation.Epoch || !validWorkflowEffectRegistration(registration, activation) {
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

func workflowCanDispatch(activation WorkflowActivation, epoch int64, now time.Time) bool {
	return activation.Mode == WorkflowGateActive && activation.Epoch == epoch && now.Before(activation.Spec.ExpiresAt)
}

func validWorkflowEffectRegistration(effect WorkflowEffect, activation WorkflowActivation) bool {
	return boundedWorkflowValue(effect.ID, 128) && workflowDigest(effect.BindingDigest) &&
		slices.Contains(activation.Spec.Actors, effect.Actor) && effect.Epoch > 0 &&
		slices.Contains([]string{"model", "tool", "memory", "knowledge", "communication"}, effect.Plane) &&
		effect.State == "" && effect.Claim == (WorkflowEffectClaim{}) && effect.Disposition == "" && effect.EvidenceDigest == "" && effect.ProofDigest == ""
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
	reserved.Effects = make(map[string]WorkflowEffect, len(activation.Effects))
	for effectID, effect := range activation.Effects {
		effect.State, effect.Disposition = WorkflowEffectCommitted, "NotDispatched"
		effect.EvidenceDigest, effect.ProofDigest = strings.Repeat("a", 64), strings.Repeat("a", 64)
		effect.Claim = WorkflowEffectClaim{
			Kind: strings.Repeat("<", 64), Namespace: strings.Repeat("<", 63), UID: strings.Repeat("<", 128),
			Claimant: strings.Repeat("<", 128), ProcessNonce: strings.Repeat("<", 128), FencingToken: math.MaxInt64,
		}
		reserved.Effects[effectID] = effect
	}
	return workflowActivationFits(reserved)
}
