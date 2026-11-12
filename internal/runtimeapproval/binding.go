// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package runtimeapproval implements backend-neutral RuntimeApproval binding
// and effective autonomy policy resolution.
package runtimeapproval

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base32"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	api "github.com/kognition-ai/kognition/api/v1alpha1"
)

const (
	BindingFormatV1    = "kognition-runtime-action-v1"
	BindingAlgorithmV1 = "HMAC-SHA-256"
	BindingDomainV1    = "kognition.dev/runtime-approval/action/v1"
	maxBindingKeys     = 16
	maxApprovalTTL     = 24 * time.Hour
)

var ErrBindingKeyUnavailable = errors.New("runtime binding key unavailable")

// ValidDecisionActor enforces the shared persistence bound for canonical actors.
func ValidDecisionActor(issuer, subject string) bool {
	return issuer != "" && subject != "" && len(issuer) <= 512 && len(subject) <= 512
}

// NormalizeLegacyStoredApproval fills fields introduced after the original
// RuntimeApproval persistence schema. It is only for already stored objects.
func NormalizeLegacyStoredApproval(approval *api.RuntimeApproval) {
	if approval.Spec.Binding.Epoch == 0 {
		approval.Spec.Binding.Epoch = 1
	}
	if approval.Spec.InitialDecision == "" {
		approval.Spec.InitialDecision = api.RuntimeApprovalDecisionPending
		if approval.Status.DecisionState == api.RuntimeApprovalDecisionNotRequired {
			approval.Spec.InitialDecision = api.RuntimeApprovalDecisionNotRequired
		}
	}
}

// BindingKeyStatus is closed to Active and VerifyOnly in T-0214.
type BindingKeyStatus string

const (
	BindingKeyActive     BindingKeyStatus = "Active"
	BindingKeyVerifyOnly BindingKeyStatus = "VerifyOnly"
)

// BindingKey is key metadata and secret material loaded from a replica-shared
// source. Material must never be copied into a RuntimeApproval.
type BindingKey struct {
	Version   int64
	Epoch     int64
	Algorithm string
	Domain    string
	Status    BindingKeyStatus
	Material  []byte
}

// BindingKeyStore selects the sole active write key while retaining old keys
// for verification. Rotation/persistence adapters serialize writes separately.
type BindingKeyStore interface {
	Active(context.Context) (BindingKey, error)
	ByVersion(context.Context, int64) (BindingKey, error)
	All(context.Context) ([]BindingKey, error)
}

// StaticBindingKeyStore is a concurrency-safe key-version abstraction for
// configuration-backed deployments and unit tests.
type StaticBindingKeyStore struct {
	mu     sync.RWMutex
	keys   map[int64]BindingKey
	active int64
}

func NewStaticBindingKeyStore(keys []BindingKey) (*StaticBindingKeyStore, error) {
	return NewStaticBindingKeyStoreAtEpoch(1, keys)
}

func NewStaticBindingKeyStoreAtEpoch(epoch int64, keys []BindingKey) (*StaticBindingKeyStore, error) {
	if epoch <= 0 {
		return nil, fmt.Errorf("runtime binding key epoch must be positive")
	}
	if len(keys) == 0 || len(keys) > maxBindingKeys {
		return nil, fmt.Errorf("runtime binding key store requires 1..%d versions", maxBindingKeys)
	}
	result := &StaticBindingKeyStore{keys: make(map[int64]BindingKey, len(keys))}
	for _, key := range keys {
		if key.Version <= 0 || len(key.Material) < 32 || key.Algorithm != BindingAlgorithmV1 || key.Domain != BindingDomainV1 {
			return nil, fmt.Errorf("invalid runtime binding key version %d", key.Version)
		}
		if key.Status != BindingKeyActive && key.Status != BindingKeyVerifyOnly {
			return nil, fmt.Errorf("invalid runtime binding key status %q", key.Status)
		}
		if _, exists := result.keys[key.Version]; exists {
			return nil, fmt.Errorf("duplicate runtime binding key version %d", key.Version)
		}
		copyKey := key
		if copyKey.Epoch == 0 {
			copyKey.Epoch = epoch
		}
		if copyKey.Epoch <= 0 || copyKey.Epoch > epoch {
			return nil, fmt.Errorf("invalid runtime binding key epoch %d for version %d", copyKey.Epoch, key.Version)
		}
		copyKey.Material = append([]byte(nil), key.Material...)
		result.keys[key.Version] = copyKey
		if key.Status == BindingKeyActive {
			if result.active != 0 {
				return nil, fmt.Errorf("multiple active runtime binding keys")
			}
			result.active = key.Version
		}
	}
	if result.active == 0 {
		return nil, fmt.Errorf("runtime binding key store has no active version")
	}
	if result.keys[result.active].Epoch != epoch {
		return nil, fmt.Errorf("active runtime binding key must belong to epoch %d", epoch)
	}
	return result, nil
}

func (s *StaticBindingKeyStore) Active(_ context.Context) (BindingKey, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return cloneBindingKey(s.keys[s.active]), nil
}

func (s *StaticBindingKeyStore) ByVersion(_ context.Context, version int64) (BindingKey, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	key, ok := s.keys[version]
	if !ok {
		return BindingKey{}, ErrBindingKeyUnavailable
	}
	return cloneBindingKey(key), nil
}

func (s *StaticBindingKeyStore) All(_ context.Context) ([]BindingKey, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	keys := make([]BindingKey, 0, len(s.keys))
	for _, key := range s.keys {
		keys = append(keys, cloneBindingKey(key))
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i].Version < keys[j].Version })
	return keys, nil
}

func cloneBindingKey(key BindingKey) BindingKey {
	key.Material = append([]byte(nil), key.Material...)
	return key
}

// ValidatedBindingKeys snapshots and validates any BindingKeyStore
// implementation before its metadata crosses a persistence boundary.
func ValidatedBindingKeys(ctx context.Context, keys BindingKeyStore) (BindingKey, []BindingKey, error) {
	if keys == nil {
		return BindingKey{}, nil, ErrBindingKeyUnavailable
	}
	active, err := keys.Active(ctx)
	if err != nil {
		return BindingKey{}, nil, ErrBindingKeyUnavailable
	}
	all, err := keys.All(ctx)
	if err != nil || len(all) == 0 || len(all) > maxBindingKeys {
		return BindingKey{}, nil, ErrBindingKeyUnavailable
	}
	seen := make(map[int64]struct{}, len(all))
	activeCount := 0
	activeMatched := false
	for index := range all {
		key := all[index]
		if key.Version <= 0 || key.Epoch <= 0 || key.Epoch > active.Epoch || len(key.Material) < 32 || key.Algorithm != BindingAlgorithmV1 || key.Domain != BindingDomainV1 {
			return BindingKey{}, nil, ErrBindingKeyUnavailable
		}
		if key.Status != BindingKeyActive && key.Status != BindingKeyVerifyOnly {
			return BindingKey{}, nil, ErrBindingKeyUnavailable
		}
		if _, exists := seen[key.Version]; exists {
			return BindingKey{}, nil, ErrBindingKeyUnavailable
		}
		seen[key.Version] = struct{}{}
		if key.Status == BindingKeyActive {
			activeCount++
			activeMatched = key.Version == active.Version && key.Epoch == active.Epoch && key.Algorithm == active.Algorithm && key.Domain == active.Domain && key.Status == active.Status && bytes.Equal(key.Material, active.Material)
		}
		all[index] = cloneBindingKey(key)
	}
	if activeCount != 1 || !activeMatched || active.Epoch <= 0 || active.Status != BindingKeyActive {
		return BindingKey{}, nil, ErrBindingKeyUnavailable
	}
	sort.Slice(all, func(i, j int) bool { return all[i].Version < all[j].Version })
	return cloneBindingKey(active), all, nil
}

// ActionEnvelope is the complete transient action intent. Payload is
// canonicalized before HMAC computation and is never returned to persistence.
type ActionEnvelope struct {
	Plane             string
	Operation         string
	Workspace         string
	InvocationID      string
	AgentRef          api.RuntimeApprovalReference
	AgentIdentityRef  api.RuntimeApprovalReference
	AgentInstanceRef  api.RuntimeApprovalReference
	SessionRef        string
	TargetRef         api.RuntimeApprovalReference
	TrustProfileRef   *api.RuntimeApprovalReference
	AutonomyClassRef  api.RuntimeApprovalReference
	AuthorizationRefs []api.RuntimeApprovalReference
	Payload           json.RawMessage
}

type canonicalActionEnvelope struct {
	Format            string                         `json:"format"`
	Plane             string                         `json:"plane"`
	Operation         string                         `json:"operation"`
	Workspace         string                         `json:"workspace"`
	InvocationID      string                         `json:"invocationID"`
	AgentRef          api.RuntimeApprovalReference   `json:"agentRef"`
	AgentIdentityRef  api.RuntimeApprovalReference   `json:"agentIdentityRef"`
	AgentInstanceRef  api.RuntimeApprovalReference   `json:"agentInstanceRef"`
	SessionRef        string                         `json:"sessionRef"`
	TargetRef         api.RuntimeApprovalReference   `json:"targetRef"`
	TrustProfileRef   *api.RuntimeApprovalReference  `json:"trustProfileRef,omitempty"`
	AutonomyClassRef  api.RuntimeApprovalReference   `json:"autonomyClassRef"`
	AuthorizationRefs []api.RuntimeApprovalReference `json:"authorizationRefs,omitempty"`
	Payload           json.RawMessage                `json:"payload"`
}

// Bind canonicalizes the complete action and signs it with the active key.
func Bind(ctx context.Context, keys BindingKeyStore, action ActionEnvelope) (api.RuntimeActionBinding, error) {
	key, err := keys.Active(ctx)
	if err != nil {
		return api.RuntimeActionBinding{}, fmt.Errorf("load active runtime binding key: %w", err)
	}
	if key.Status != BindingKeyActive {
		return api.RuntimeActionBinding{}, fmt.Errorf("active runtime binding key version %d is verify-only", key.Version)
	}
	encoded, err := canonicalAction(action)
	if err != nil {
		return api.RuntimeActionBinding{}, err
	}
	mac := hmac.New(sha256.New, key.Material)
	_, _ = mac.Write([]byte(key.Domain))
	_, _ = mac.Write([]byte{0})
	_, _ = mac.Write(encoded)
	return api.RuntimeActionBinding{
		Format: BindingFormatV1, Algorithm: key.Algorithm, Domain: key.Domain,
		Epoch: key.Epoch, KeyVersion: key.Version, Tag: base64.RawURLEncoding.EncodeToString(mac.Sum(nil)),
	}, nil
}

// Verify recomputes a persisted binding using its retained key version.
func Verify(ctx context.Context, keys BindingKeyStore, action ActionEnvelope, binding api.RuntimeActionBinding) error {
	if binding.Format != BindingFormatV1 || binding.Algorithm != BindingAlgorithmV1 || binding.Domain != BindingDomainV1 || binding.KeyVersion <= 0 {
		return fmt.Errorf("unsupported runtime action binding")
	}
	key, err := keys.ByVersion(ctx, binding.KeyVersion)
	if err != nil {
		return fmt.Errorf("load runtime binding key version %d: %w", binding.KeyVersion, err)
	}
	encoded, err := canonicalAction(action)
	if err != nil {
		return err
	}
	mac := hmac.New(sha256.New, key.Material)
	_, _ = mac.Write([]byte(key.Domain))
	_, _ = mac.Write([]byte{0})
	_, _ = mac.Write(encoded)
	actual, err := base64.RawURLEncoding.DecodeString(binding.Tag)
	if err != nil || !hmac.Equal(actual, mac.Sum(nil)) {
		return fmt.Errorf("runtime action binding conflict")
	}
	return nil
}

// ValidateNewBindingKey fails closed unless the binding names the sole current
// Active key and all persisted key metadata matches that trusted key.
func ValidateNewBindingKey(ctx context.Context, keys BindingKeyStore, binding api.RuntimeActionBinding) error {
	if keys == nil {
		return ErrBindingKeyUnavailable
	}
	key, err := keys.Active(ctx)
	if err != nil {
		return fmt.Errorf("load active runtime binding key: %w", err)
	}
	if key.Status != BindingKeyActive || !bindingMatchesKey(binding, key) {
		return fmt.Errorf("runtime action binding does not use the current active key")
	}
	return validateBindingTag(binding.Tag)
}

// ValidateRetainedBindingKey verifies that replay can still be authenticated
// by an Active or VerifyOnly retained key.
func ValidateRetainedBindingKey(ctx context.Context, keys BindingKeyStore, binding api.RuntimeActionBinding) error {
	if keys == nil {
		return ErrBindingKeyUnavailable
	}
	key, err := keys.ByVersion(ctx, binding.KeyVersion)
	if err != nil {
		return fmt.Errorf("load runtime binding key version %d: %w", binding.KeyVersion, err)
	}
	if (key.Status != BindingKeyActive && key.Status != BindingKeyVerifyOnly) || !bindingMatchesKey(binding, key) {
		return fmt.Errorf("runtime action binding key version %d is not valid for verification", binding.KeyVersion)
	}
	return validateBindingTag(binding.Tag)
}

// AuthenticateNewBinding verifies that a proposed approval was signed with the
// current active key over the complete transient action payload.
func AuthenticateNewBinding(ctx context.Context, keys BindingKeyStore, approval api.RuntimeApproval, payload json.RawMessage) error {
	if err := ValidateNewBindingKey(ctx, keys, approval.Spec.Binding); err != nil {
		return err
	}
	return Verify(ctx, keys, ActionEnvelopeFromApproval(approval, payload), approval.Spec.Binding)
}

// AuthenticateRetainedBinding verifies an identical replay with the retained
// Active or VerifyOnly key version that originally signed it.
func AuthenticateRetainedBinding(ctx context.Context, keys BindingKeyStore, approval api.RuntimeApproval, payload json.RawMessage) error {
	if err := ValidateRetainedBindingKey(ctx, keys, approval.Spec.Binding); err != nil {
		return err
	}
	return Verify(ctx, keys, ActionEnvelopeFromApproval(approval, payload), approval.Spec.Binding)
}

// ActionEnvelopeFromApproval restores the non-secret portion of the canonical
// action while accepting the sensitive payload only transiently.
func ActionEnvelopeFromApproval(approval api.RuntimeApproval, payload json.RawMessage) ActionEnvelope {
	return ActionEnvelope{
		Plane: approval.Spec.Plane, Operation: approval.Spec.Operation,
		Workspace: approval.Spec.Workspace, InvocationID: approval.Spec.InvocationID,
		AgentRef: approval.Spec.AgentRef, AgentIdentityRef: approval.Spec.AgentIdentityRef,
		AgentInstanceRef: approval.Spec.AgentInstanceRef, SessionRef: approval.Spec.SessionRef,
		TargetRef: approval.Spec.TargetRef, TrustProfileRef: approval.Spec.TrustProfileRef,
		AutonomyClassRef:  approval.Spec.AutonomyClassRef,
		AuthorizationRefs: append([]api.RuntimeApprovalReference(nil), approval.Spec.AuthorizationRefs...),
		Payload:           append(json.RawMessage(nil), payload...),
	}
}

func bindingMatchesKey(binding api.RuntimeActionBinding, key BindingKey) bool {
	return binding.Format == BindingFormatV1 && binding.Algorithm == key.Algorithm && binding.Domain == key.Domain && binding.Epoch == key.Epoch && binding.KeyVersion == key.Version && len(key.Material) >= 32
}

func validateBindingTag(tag string) error {
	decoded, err := base64.RawURLEncoding.DecodeString(tag)
	if err != nil || len(decoded) != sha256.Size {
		return fmt.Errorf("runtime action binding tag must be a base64url SHA-256 value")
	}
	return nil
}

func canonicalAction(action ActionEnvelope) ([]byte, error) {
	if err := validateActionEnvelope(action); err != nil {
		return nil, err
	}
	payload, err := CanonicalizeJSON(action.Payload)
	if err != nil {
		return nil, fmt.Errorf("canonicalize runtime action payload: %w", err)
	}
	references, err := canonicalAuthorizationReferences(action.AuthorizationRefs)
	if err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(canonicalActionEnvelope{
		Format: BindingFormatV1, Plane: action.Plane, Operation: action.Operation,
		Workspace: action.Workspace, InvocationID: action.InvocationID,
		AgentRef: action.AgentRef, AgentIdentityRef: action.AgentIdentityRef,
		AgentInstanceRef: stableInstanceReference(action.AgentInstanceRef), SessionRef: action.SessionRef,
		TargetRef: action.TargetRef, TrustProfileRef: action.TrustProfileRef,
		AutonomyClassRef: action.AutonomyClassRef, AuthorizationRefs: references,
		Payload: payload,
	})
	if err != nil {
		return nil, fmt.Errorf("marshal runtime action envelope: %w", err)
	}
	return CanonicalizeJSON(encoded)
}

func validateActionEnvelope(action ActionEnvelope) error {
	if action.Workspace == "" || action.InvocationID == "" || len(action.InvocationID) > 128 || action.Plane == "" || action.Operation == "" || action.SessionRef == "" {
		return fmt.Errorf("runtime action requires bounded workspace, invocation, plane, operation, and session")
	}
	approval := api.RuntimeApproval{Spec: api.RuntimeApprovalSpec{
		Workspace: action.Workspace, Plane: action.Plane, AgentRef: action.AgentRef, AgentIdentityRef: action.AgentIdentityRef,
		AgentInstanceRef: action.AgentInstanceRef, TargetRef: action.TargetRef,
		TrustProfileRef: action.TrustProfileRef, AutonomyClassRef: action.AutonomyClassRef,
		AuthorizationRefs: append([]api.RuntimeApprovalReference(nil), action.AuthorizationRefs...),
	}}
	if err := NormalizeAndValidateReferences(&approval); err != nil {
		return err
	}
	return nil
}

func referenceKey(ref api.RuntimeApprovalReference) string {
	return strings.Join([]string{ref.Kind, ref.Scope, ref.Namespace, ref.Name, ref.UID, fmt.Sprint(ref.ResourceVersion)}, "\x00")
}

// CanonicalizeJSON rejects duplicate object keys and trailing values, then
// emits deterministic JSON. encoding/json sorts object keys during encoding.
func CanonicalizeJSON(raw []byte) ([]byte, error) {
	if !utf8.Valid(raw) {
		return nil, fmt.Errorf("JSON payload must be valid UTF-8")
	}
	if err := validateJSONSurrogateEscapes(raw); err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	value, err := decodeUniqueJSON(decoder)
	if err != nil {
		return nil, err
	}
	if token, err := decoder.Token(); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("unexpected trailing JSON token %v", token)
		}
		return nil, err
	}
	var output bytes.Buffer
	encoder := json.NewEncoder(&output)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(output.Bytes(), []byte{'\n'}), nil
}

func validateJSONSurrogateEscapes(raw []byte) error {
	for index := 0; index < len(raw); index++ {
		if raw[index] != '"' {
			continue
		}
		for index++; index < len(raw) && raw[index] != '"'; index++ {
			if raw[index] != '\\' {
				continue
			}
			index++
			if index >= len(raw) {
				break
			}
			if raw[index] != 'u' {
				continue
			}
			first, next, err := parseJSONUnicodeEscape(raw, index)
			if err != nil {
				return err
			}
			index = next - 1
			if first >= 0xDC00 && first <= 0xDFFF {
				return fmt.Errorf("JSON payload contains an unpaired low surrogate")
			}
			if first < 0xD800 || first > 0xDBFF {
				continue
			}
			if next+1 >= len(raw) || raw[next] != '\\' || raw[next+1] != 'u' {
				return fmt.Errorf("JSON payload contains an unpaired high surrogate")
			}
			second, afterSecond, err := parseJSONUnicodeEscape(raw, next+1)
			if err != nil || second < 0xDC00 || second > 0xDFFF {
				return fmt.Errorf("JSON payload contains an unpaired high surrogate")
			}
			index = afterSecond - 1
		}
	}
	return nil
}

func parseJSONUnicodeEscape(raw []byte, unicodeIndex int) (uint64, int, error) {
	if unicodeIndex+5 > len(raw) {
		return 0, unicodeIndex, fmt.Errorf("JSON payload contains a truncated Unicode escape")
	}
	value, err := strconv.ParseUint(string(raw[unicodeIndex+1:unicodeIndex+5]), 16, 16)
	if err != nil {
		return 0, unicodeIndex, fmt.Errorf("JSON payload contains an invalid Unicode escape")
	}
	return value, unicodeIndex + 5, nil
}

func decodeUniqueJSON(decoder *json.Decoder) (any, error) {
	token, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	delimiter, isDelimiter := token.(json.Delim)
	if !isDelimiter {
		if number, ok := token.(json.Number); ok {
			canonical, err := canonicalizeJSONNumber(number.String())
			if err != nil {
				return nil, err
			}
			return json.Number(canonical), nil
		}
		return token, nil
	}
	switch delimiter {
	case '{':
		object := map[string]any{}
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return nil, err
			}
			key, ok := keyToken.(string)
			if !ok {
				return nil, fmt.Errorf("object key is not a string")
			}
			if _, duplicate := object[key]; duplicate {
				return nil, fmt.Errorf("duplicate JSON key %q", key)
			}
			value, err := decodeUniqueJSON(decoder)
			if err != nil {
				return nil, err
			}
			object[key] = value
		}
		if end, err := decoder.Token(); err != nil || end != json.Delim('}') {
			return nil, fmt.Errorf("unterminated JSON object")
		}
		return object, nil
	case '[':
		var array []any
		for decoder.More() {
			value, err := decodeUniqueJSON(decoder)
			if err != nil {
				return nil, err
			}
			array = append(array, value)
		}
		if end, err := decoder.Token(); err != nil || end != json.Delim(']') {
			return nil, fmt.Errorf("unterminated JSON array")
		}
		return array, nil
	default:
		return nil, fmt.Errorf("unexpected JSON delimiter %q", delimiter)
	}
}

// canonicalizeJSONNumber emits an exact coefficient/exponent form so the
// result is independent of input spelling and floating-point precision.
func canonicalizeJSONNumber(encoded string) (string, error) {
	negative := strings.HasPrefix(encoded, "-")
	if negative {
		encoded = encoded[1:]
	}
	mantissa, exponentText, hasExponent := strings.Cut(encoded, "e")
	if !hasExponent {
		mantissa, exponentText, hasExponent = strings.Cut(encoded, "E")
	}
	var exponent int64
	var err error
	if hasExponent {
		exponent, err = strconv.ParseInt(exponentText, 10, 32)
		if err != nil {
			return "", fmt.Errorf("JSON number exponent is out of range")
		}
	}
	integer, fraction, hasFraction := strings.Cut(mantissa, ".")
	digits := integer
	if hasFraction {
		digits += fraction
		exponent -= int64(len(fraction))
	}
	digits = strings.TrimLeft(digits, "0")
	if digits == "" {
		return "0", nil
	}
	trimmed := strings.TrimRight(digits, "0")
	exponent += int64(len(digits) - len(trimmed))
	if exponent > int64(^uint32(0)>>1) || exponent < -int64(^uint32(0)>>1)-1 {
		return "", fmt.Errorf("JSON number exponent is out of range")
	}
	if negative {
		trimmed = "-" + trimmed
	}
	if exponent == 0 {
		return trimmed, nil
	}
	return trimmed + "e" + strconv.FormatInt(exponent, 10), nil
}

// CanonicalName returns the ADR-0014 DNS-safe logical name.
func CanonicalName(workspace, invocationID string) string {
	hash := sha256.New()
	writeLengthDelimited(hash, workspace)
	writeLengthDelimited(hash, invocationID)
	encoded := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(hash.Sum(nil))
	return "ra-" + strings.ToLower(encoded[:52])
}

func writeLengthDelimited(writer io.Writer, value string) {
	_, _ = fmt.Fprintf(writer, "%d:", len(value))
	_, _ = io.WriteString(writer, value)
}

// SameImmutableBinding compares every caller-bound field while excluding
// store-assigned timestamps. The keyed tag comparison is constant-time.
func SameImmutableBinding(existing, proposed api.RuntimeApproval) bool {
	if !hmac.Equal([]byte(existing.Spec.Binding.Tag), []byte(proposed.Spec.Binding.Tag)) {
		return false
	}
	existingSpec := existing.Spec
	proposedSpec := proposed.Spec
	existingSpec.AgentInstanceRef = stableInstanceReference(existingSpec.AgentInstanceRef)
	proposedSpec.AgentInstanceRef = stableInstanceReference(proposedSpec.AgentInstanceRef)
	existingSpec.Binding.Tag = ""
	proposedSpec.Binding.Tag = ""
	existingSpec.CreatedAt, existingSpec.ExpiresAt = time.Time{}, time.Time{}
	proposedSpec.CreatedAt, proposedSpec.ExpiresAt = time.Time{}, time.Time{}
	var err error
	existingSpec.AuthorizationRefs, err = canonicalAuthorizationReferences(existingSpec.AuthorizationRefs)
	if err != nil {
		return false
	}
	proposedSpec.AuthorizationRefs, err = canonicalAuthorizationReferences(proposedSpec.AuthorizationRefs)
	if err != nil {
		return false
	}
	return reflect.DeepEqual(existingSpec, proposedSpec)
}

func stableInstanceReference(reference api.RuntimeApprovalReference) api.RuntimeApprovalReference {
	if reference.Kind == "AgentInstance" && validInstanceConfigurationDigest(reference.ConfigurationDigest) {
		reference.ResourceVersion = 0
	}
	return reference
}

// NormalizeAndValidateReferences validates every exact RuntimeApproval
// reference and canonicalizes authorization reference ordering in place.
func NormalizeAndValidateReferences(approval *api.RuntimeApproval) error {
	if approval == nil || approval.Spec.Workspace == "" {
		return fmt.Errorf("RuntimeApproval workspace is required")
	}
	if len(approval.Spec.AuthorizationRefs) > 64 {
		return fmt.Errorf("RuntimeApproval supports at most 64 authorization references")
	}
	fixed := []struct {
		name string
		kind string
		ref  *api.RuntimeApprovalReference
	}{
		{"agentRef", "Agent", &approval.Spec.AgentRef},
		{"agentIdentityRef", "AgentIdentity", &approval.Spec.AgentIdentityRef},
		{"agentInstanceRef", "AgentInstance", &approval.Spec.AgentInstanceRef},
		{"targetRef", "", &approval.Spec.TargetRef},
		{"autonomyClassRef", "AutonomyClass", &approval.Spec.AutonomyClassRef},
	}
	if approval.Spec.TrustProfileRef != nil {
		fixed = append(fixed, struct {
			name string
			kind string
			ref  *api.RuntimeApprovalReference
		}{"trustProfileRef", "TrustProfile", approval.Spec.TrustProfileRef})
	}
	for _, item := range fixed {
		if item.ref.ConfigurationDigest != "" {
			if item.name != "agentInstanceRef" || !validInstanceConfigurationDigest(item.ref.ConfigurationDigest) {
				return fmt.Errorf("%s configurationDigest must be a lowercase SHA-256 digest on agentInstanceRef", item.name)
			}
		}
		workspace := approval.Spec.Workspace
		if item.name == "targetRef" && approval.Spec.Plane == api.RuntimeApprovalPlaneCommunication && item.ref.Kind == "AgentService" && item.ref.Scope == "Namespaced" && item.ref.Namespace != "" {
			workspace = item.ref.Namespace
		}
		if err := validateReference(item.name, item.kind, workspace, *item.ref); err != nil {
			return err
		}
	}
	for index := range approval.Spec.AuthorizationRefs {
		if approval.Spec.AuthorizationRefs[index].ConfigurationDigest != "" {
			return fmt.Errorf("authorization references cannot carry configurationDigest")
		}
		if err := validateReference(fmt.Sprintf("authorizationRefs[%d]", index), "", approval.Spec.Workspace, approval.Spec.AuthorizationRefs[index]); err != nil {
			return err
		}
	}
	var err error
	approval.Spec.AuthorizationRefs, err = canonicalAuthorizationReferences(approval.Spec.AuthorizationRefs)
	if err != nil {
		return err
	}
	return nil
}

func validInstanceConfigurationDigest(value string) bool {
	if len(value) != 64 || value != strings.ToLower(value) {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size
}

func canonicalAuthorizationReferences(references []api.RuntimeApprovalReference) ([]api.RuntimeApprovalReference, error) {
	canonical := append([]api.RuntimeApprovalReference(nil), references...)
	sort.Slice(canonical, func(i, j int) bool {
		return referenceKey(canonical[i]) < referenceKey(canonical[j])
	})
	for index := 1; index < len(canonical); index++ {
		if referenceKey(canonical[index-1]) == referenceKey(canonical[index]) {
			return nil, fmt.Errorf("duplicate authorization reference")
		}
	}
	return canonical, nil
}

func validateReference(field, expectedKind, workspace string, ref api.RuntimeApprovalReference) error {
	if ref.Kind == "" || ref.Name == "" || ref.UID == "" || ref.ResourceVersion <= 0 {
		return fmt.Errorf("%s requires kind, name, UID, and positive resource version", field)
	}
	if len(ref.Kind) > 64 || len(ref.Namespace) > 253 || len(ref.Name) > 253 || len(ref.UID) > 128 {
		return fmt.Errorf("%s exceeds the RuntimeApproval reference bounds", field)
	}
	if expectedKind != "" && ref.Kind != expectedKind {
		return fmt.Errorf("%s kind must be %s", field, expectedKind)
	}
	switch ref.Scope {
	case "Namespaced":
		if ref.Namespace != workspace {
			return fmt.Errorf("%s namespace must equal RuntimeApproval workspace", field)
		}
	case "Cluster":
		if ref.Namespace != "" {
			return fmt.Errorf("%s cluster-scoped reference must not set namespace", field)
		}
	default:
		return fmt.Errorf("%s scope must be Namespaced or Cluster", field)
	}
	if field == "autonomyClassRef" && ref.Scope != "Cluster" {
		return fmt.Errorf("autonomyClassRef must be cluster-scoped")
	}
	if (field == "agentRef" || field == "agentIdentityRef" || field == "agentInstanceRef" || field == "trustProfileRef") && ref.Scope != "Namespaced" {
		return fmt.Errorf("%s must be namespaced", field)
	}
	return nil
}

// ValidateCreate enforces the backend-neutral bounded persistence contract.
func ValidateCreate(approval api.RuntimeApproval, ttl time.Duration) error {
	if ttl <= 0 || ttl > maxApprovalTTL {
		return fmt.Errorf("RuntimeApproval TTL must be positive and at most 24h")
	}
	spec := approval.Spec
	if spec.Workspace == "" || len(spec.Workspace) > 253 || spec.InvocationID == "" || len(spec.InvocationID) > 128 || spec.Operation == "" || len(spec.Operation) > 64 || spec.SessionRef == "" || len(spec.SessionRef) > 256 {
		return fmt.Errorf("RuntimeApproval requires bounded workspace, invocation ID, operation, and session reference")
	}
	if spec.Plane != api.RuntimeApprovalPlaneTool && spec.Plane != api.RuntimeApprovalPlaneMemory && spec.Plane != api.RuntimeApprovalPlaneCommunication {
		return fmt.Errorf("RuntimeApproval plane must be Tool, Memory, or Communication")
	}
	if err := ValidateInitialStatus(approval.Status); err != nil {
		return err
	}
	binding := spec.Binding
	if binding.Format != BindingFormatV1 || binding.Algorithm != BindingAlgorithmV1 || binding.Domain != BindingDomainV1 || binding.KeyVersion <= 0 || len(binding.Tag) < 43 || len(binding.Tag) > 64 {
		return fmt.Errorf("RuntimeApproval requires a supported bounded action binding")
	}
	return nil
}

// ValidateInitialStatus rejects all caller-owned lifecycle state beyond the
// permitted decision intent and required NotStarted execution state.
func ValidateInitialStatus(status api.RuntimeApprovalStatus) error {
	if status.DecisionState != api.RuntimeApprovalDecisionPending && status.DecisionState != api.RuntimeApprovalDecisionNotRequired {
		return fmt.Errorf("RuntimeApproval must start Pending or NotRequired")
	}
	if status.ExecutionState != api.RuntimeApprovalExecutionNotStarted {
		return fmt.Errorf("RuntimeApproval must start with execution NotStarted")
	}
	clean := api.RuntimeApprovalStatus{DecisionState: status.DecisionState, ExecutionState: api.RuntimeApprovalExecutionNotStarted}
	if !reflect.DeepEqual(status, clean) {
		return fmt.Errorf("RuntimeApproval initial status contains caller-owned lifecycle fields")
	}
	return nil
}

// InitialStatus reconstructs the complete store-owned initial status.
func InitialStatus(decision string) api.RuntimeApprovalStatus {
	return api.RuntimeApprovalStatus{DecisionState: decision, ExecutionState: api.RuntimeApprovalExecutionNotStarted}
}

// FinalizeInitialStatus creates evidence for the terminal policy decision after
// the persistence layer has assigned the approval UID.
func FinalizeInitialStatus(approval *api.RuntimeApproval, decidedAt time.Time) {
	if approval == nil || approval.Status.DecisionState != api.RuntimeApprovalDecisionNotRequired {
		return
	}
	approval.Status.DecidedAt = timePointer(decidedAt.UTC())
	PopulateDecisionAudit(approval)
}

func timePointer(value time.Time) *time.Time { return &value }

// PopulateDecisionAudit creates the deterministic bounded decision outbox.
func PopulateDecisionAudit(approval *api.RuntimeApproval) {
	if approval == nil || approval.Status.DecidedAt == nil {
		return
	}
	hash := sha256.New()
	writeLengthDelimited(hash, approval.UID)
	writeLengthDelimited(hash, approval.Status.DecisionState)
	id := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(hash.Sum(nil))
	approval.Status.DecisionAudit = &api.RuntimeApprovalDecisionAudit{
		ID: "rad-" + strings.ToLower(id),
		Payload: api.RuntimeApprovalDecisionAuditPayload{
			Workspace: approval.Spec.Workspace, InvocationID: approval.Spec.InvocationID,
			ApprovalUID: approval.UID, Decision: approval.Status.DecisionState,
			Issuer: approval.Status.DecidedByIssuer, Subject: approval.Status.DecidedBySubject,
			DecidedAt: approval.Status.DecidedAt.UTC(),
		},
	}
}
