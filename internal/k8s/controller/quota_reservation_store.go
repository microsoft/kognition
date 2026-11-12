// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	k8s "github.com/kognition-ai/kognition/api/k8s/v1alpha1"
	"github.com/kognition-ai/kognition/internal/store"
)

const (
	tokenLedgerConfigMapPrefix     = "kognition-token-reservations-"
	tokenLedgerDataKey             = "ledger.json"
	tokenLedgerMaxRetries          = 8
	tokenLedgerNamespaceAnnotation = "kognition.dev/quota-namespace"
	tokenLedgerManagedLabel        = "kognition.dev/token-ledger"
	tokenLedgerManagedLabelValue   = "true"
)

type tokenLedger struct {
	Reservations       map[string]tokenLedgerReservation             `json:"reservations"`
	Quotas             map[string]tokenLedgerQuota                   `json:"quotas"`
	CanaryDispositions map[string]store.CanaryReservationDisposition `json:"canaryDispositions,omitempty"`
}

type tokenLedgerReservation struct {
	Canary         bool              `json:"canary,omitempty"`
	ReservedInput  int64             `json:"reservedInput"`
	ReservedOutput int64             `json:"reservedOutput"`
	CreatedAt      time.Time         `json:"createdAt"`
	QuotaPeriods   map[string]string `json:"quotaPeriods"`
}

type tokenLedgerQuota struct {
	Period       string `json:"period"`
	InputTokens  int64  `json:"inputTokens"`
	OutputTokens int64  `json:"outputTokens"`
	TotalTokens  int64  `json:"totalTokens"`
}

func (s *StoreAdapter) CreateTokenReservation(ctx context.Context, reservation store.TokenReservation) (bool, error) {
	return s.createTokenReservation(ctx, reservation, false)
}

func (s *StoreAdapter) CreateCanaryReservation(ctx context.Context, reservation store.TokenReservation) (bool, error) {
	return s.createTokenReservation(ctx, reservation, true)
}

func (s *StoreAdapter) createTokenReservation(ctx context.Context, reservation store.TokenReservation, canary bool) (bool, error) {
	if reservation.ID == "" || reservation.Namespace == "" {
		return false, fmt.Errorf("token reservation ID and namespace are required")
	}
	if reservation.ReservedInput < 0 || reservation.ReservedOutput < 0 {
		return false, fmt.Errorf("token reservation ceilings must be non-negative")
	}

	created := false
	_, err := s.mutateTokenLedger(ctx, reservation.Namespace, func(ledger *tokenLedger, quotas []k8s.CognitiveQuota) (bool, error) {
		if _, terminal := ledger.CanaryDispositions[reservation.ID]; terminal {
			return false, store.ErrConflict
		}
		if existing, ok := ledger.Reservations[reservation.ID]; ok {
			created = false
			if existing.Canary != canary || existing.ReservedInput != reservation.ReservedInput || existing.ReservedOutput != reservation.ReservedOutput {
				return false, store.ErrConflict
			}
			return false, nil
		}

		entry := tokenLedgerReservation{
			Canary:         canary,
			ReservedInput:  reservation.ReservedInput,
			ReservedOutput: reservation.ReservedOutput,
			CreatedAt:      time.Now().UTC(),
			QuotaPeriods:   make(map[string]string),
		}
		if _, err := moveReservationToCurrentQuotaPeriods(ledger, &entry, quotas); err != nil {
			return false, err
		}
		ledger.Reservations[reservation.ID] = entry
		created = true
		return true, nil
	})
	if err != nil {
		return false, err
	}
	return created, nil
}

func (s *StoreAdapter) SettleTokenReservation(ctx context.Context, reservationID string, actualInput, actualOutput int64) (bool, error) {
	if reservationID == "" {
		return false, fmt.Errorf("token reservation ID is required")
	}
	if actualInput < 0 || actualOutput < 0 {
		return false, fmt.Errorf("actual token usage must be non-negative")
	}
	actualTotal, err := addK8sTokenUsage(actualInput, actualOutput)
	if err != nil {
		return false, err
	}

	namespace, err := s.findTokenReservationNamespace(ctx, reservationID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return false, nil
		}
		return false, err
	}
	found := false
	_, err = s.mutateTokenLedger(ctx, namespace, func(ledger *tokenLedger, quotas []k8s.CognitiveQuota) (bool, error) {
		reservation, ok := ledger.Reservations[reservationID]
		if !ok {
			found = false
			return false, nil
		}
		if reservation.Canary {
			return false, store.ErrConflict
		}
		found = true
		if err := settleK8sTokenReservation(ledger, quotas, reservation, actualInput, actualOutput, actualTotal); err != nil {
			return false, err
		}
		delete(ledger.Reservations, reservationID)
		return true, nil
	})
	return found, err
}

func settleK8sTokenReservation(ledger *tokenLedger, quotas []k8s.CognitiveQuota, reservation tokenLedgerReservation, actualInput, actualOutput, actualTotal int64) error {
	reservedTotal, err := addK8sTokenUsage(reservation.ReservedInput, reservation.ReservedOutput)
	if err != nil {
		return err
	}
	for index := range quotas {
		quota := &quotas[index]
		usage := ledger.Quotas[quota.Name]
		inputDelta, outputDelta, totalDelta := actualInput, actualOutput, actualTotal
		if reservation.QuotaPeriods[quota.Name] == usage.Period {
			inputDelta -= reservation.ReservedInput
			outputDelta -= reservation.ReservedOutput
			totalDelta -= reservedTotal
		}
		if err := adjustK8sLedgerQuota(&usage, inputDelta, outputDelta, totalDelta); err != nil {
			return err
		}
		ledger.Quotas[quota.Name] = usage
	}
	return nil
}

func (s *StoreAdapter) SettleCanaryReservation(ctx context.Context, reservationID string, actualInput, actualOutput int64) (store.CanaryReservationDisposition, error) {
	return s.finalizeCanaryReservation(ctx, reservationID, store.CanaryReservationSettled, actualInput, actualOutput)
}

func (s *StoreAdapter) ReleaseCanaryReservation(ctx context.Context, reservationID string) (store.CanaryReservationDisposition, error) {
	return s.finalizeCanaryReservation(ctx, reservationID, store.CanaryReservationReleased, 0, 0)
}

func (s *StoreAdapter) finalizeCanaryReservation(ctx context.Context, reservationID, state string, actualInput, actualOutput int64) (store.CanaryReservationDisposition, error) {
	want := store.CanaryReservationDisposition{ReservationID: reservationID, State: state, InputTokens: actualInput, OutputTokens: actualOutput}
	if reservationID == "" {
		return store.CanaryReservationDisposition{}, errors.New("canary reservation ID is required")
	}
	actualTotal, err := addK8sTokenUsage(actualInput, actualOutput)
	if err != nil {
		return store.CanaryReservationDisposition{}, err
	}
	namespace, err := s.findTokenReservationNamespace(ctx, reservationID)
	if err != nil {
		return store.CanaryReservationDisposition{}, err
	}
	_, err = s.mutateTokenLedger(ctx, namespace, func(ledger *tokenLedger, quotas []k8s.CognitiveQuota) (bool, error) {
		if existing, ok := ledger.CanaryDispositions[reservationID]; ok {
			if existing != want {
				return false, store.ErrConflict
			}
			return false, nil
		}
		reservation, ok := ledger.Reservations[reservationID]
		if !ok {
			return false, store.ErrNotFound
		}
		if !reservation.Canary {
			return false, store.ErrConflict
		}
		if err := settleK8sTokenReservation(ledger, quotas, reservation, actualInput, actualOutput, actualTotal); err != nil {
			return false, err
		}
		ledger.CanaryDispositions[reservationID] = want
		delete(ledger.Reservations, reservationID)
		return true, nil
	})
	if err != nil {
		return store.CanaryReservationDisposition{}, err
	}
	return want, nil
}

func (s *StoreAdapter) ReapExpiredTokenReservations(ctx context.Context, expiredBefore time.Time, limit int) (int, error) {
	if limit <= 0 {
		return 0, nil
	}
	var ledgers corev1.ConfigMapList
	if err := s.client.List(ctx, &ledgers, s.tokenLedgerListOptions()...); err != nil {
		return 0, fmt.Errorf("list token reservation ledgers: %w", err)
	}
	sort.Slice(ledgers.Items, func(i, j int) bool { return ledgers.Items[i].Namespace < ledgers.Items[j].Namespace })
	reaped := 0
	for index := range ledgers.Items {
		if reaped >= limit {
			break
		}
		remaining := limit - reaped
		workloadNamespace, err := s.tokenLedgerWorkloadNamespace(&ledgers.Items[index])
		if err != nil {
			return reaped, err
		}
		count, err := s.reapNamespaceTokenReservations(ctx, workloadNamespace, expiredBefore, remaining)
		if err != nil {
			return reaped, err
		}
		reaped += count
	}
	return reaped, nil
}

func (s *StoreAdapter) reapNamespaceTokenReservations(ctx context.Context, namespace string, expiredBefore time.Time, limit int) (int, error) {
	reaped := 0
	_, err := s.mutateTokenLedger(ctx, namespace, func(ledger *tokenLedger, quotas []k8s.CognitiveQuota) (bool, error) {
		reaped = 0
		ids := make([]string, 0, len(ledger.Reservations))
		for id, reservation := range ledger.Reservations {
			if !reservation.Canary && reservation.CreatedAt.Before(expiredBefore) {
				ids = append(ids, id)
			}
		}
		sort.Slice(ids, func(i, j int) bool {
			left, right := ledger.Reservations[ids[i]], ledger.Reservations[ids[j]]
			if left.CreatedAt.Equal(right.CreatedAt) {
				return ids[i] < ids[j]
			}
			return left.CreatedAt.Before(right.CreatedAt)
		})
		if len(ids) > limit {
			ids = ids[:limit]
		}
		for _, id := range ids {
			reservation := ledger.Reservations[id]
			reservedTotal, err := addK8sTokenUsage(reservation.ReservedInput, reservation.ReservedOutput)
			if err != nil {
				return false, err
			}
			for index := range quotas {
				quota := &quotas[index]
				usage := ledger.Quotas[quota.Name]
				if reservation.QuotaPeriods[quota.Name] != usage.Period {
					continue
				}
				if err := adjustK8sLedgerQuota(&usage, -reservation.ReservedInput, -reservation.ReservedOutput, -reservedTotal); err != nil {
					return false, err
				}
				ledger.Quotas[quota.Name] = usage
			}
			delete(ledger.Reservations, id)
			reaped++
		}
		return len(ids) > 0, nil
	})
	return reaped, err
}

func (s *StoreAdapter) findTokenReservationNamespace(ctx context.Context, reservationID string) (string, error) {
	var ledgers corev1.ConfigMapList
	if err := s.client.List(ctx, &ledgers, s.tokenLedgerListOptions()...); err != nil {
		return "", fmt.Errorf("list token reservation ledgers: %w", err)
	}
	for index := range ledgers.Items {
		workloadNamespace, err := s.tokenLedgerWorkloadNamespace(&ledgers.Items[index])
		if err != nil {
			return "", err
		}
		ledger, err := decodeTokenLedger(&ledgers.Items[index])
		if err != nil {
			return "", err
		}
		if _, ok := ledger.Reservations[reservationID]; ok {
			return workloadNamespace, nil
		}
		if _, ok := ledger.CanaryDispositions[reservationID]; ok {
			return workloadNamespace, nil
		}
		if err := s.syncTokenLedgerStatus(ctx, workloadNamespace); err != nil {
			return "", err
		}
	}
	return "", store.ErrNotFound
}

func (s *StoreAdapter) mutateTokenLedger(
	ctx context.Context,
	namespace string,
	mutate func(*tokenLedger, []k8s.CognitiveQuota) (bool, error),
) (bool, error) {
	if namespace == "" {
		return false, fmt.Errorf("token ledger namespace is required")
	}
	for attempt := 0; attempt < tokenLedgerMaxRetries; attempt++ {
		quotas, err := s.listTokenQuotas(ctx, namespace)
		if err != nil {
			return false, err
		}
		configMap := &corev1.ConfigMap{}
		key := s.tokenLedgerObjectKey(namespace)
		err = s.client.Get(ctx, key, configMap)
		create := apierrors.IsNotFound(err)
		if err != nil && !create {
			return false, fmt.Errorf("get token reservation ledger: %w", err)
		}
		ledger := newTokenLedger()
		if !create {
			ledger, err = decodeTokenLedger(configMap)
			if err != nil {
				return false, err
			}
		}
		normalized := normalizeTokenLedger(ledger, quotas)
		changed, err := mutate(ledger, quotas)
		if err != nil {
			return false, err
		}
		if !normalized && !changed && !create {
			if err := s.syncTokenLedgerStatus(ctx, namespace); err != nil {
				return false, err
			}
			return false, nil
		}
		data, err := json.Marshal(ledger)
		if err != nil {
			return false, fmt.Errorf("marshal token reservation ledger: %w", err)
		}
		if create {
			configMap = &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: key.Namespace,
					Name:      key.Name,
					Labels:    map[string]string{tokenLedgerManagedLabel: tokenLedgerManagedLabelValue},
					Annotations: map[string]string{
						tokenLedgerNamespaceAnnotation: namespace,
					},
				},
				Data: map[string]string{tokenLedgerDataKey: string(data)},
			}
			err = s.client.Create(ctx, configMap)
		} else {
			configMap.Data[tokenLedgerDataKey] = string(data)
			err = s.client.Update(ctx, configMap)
		}
		if apierrors.IsAlreadyExists(err) || apierrors.IsConflict(err) {
			continue
		}
		if err != nil {
			return false, fmt.Errorf("persist token reservation ledger: %w", err)
		}
		if err := s.syncTokenLedgerStatus(ctx, namespace); err != nil {
			return false, err
		}
		return changed, nil
	}
	return false, store.ErrConflict
}

func (s *StoreAdapter) listTokenQuotas(ctx context.Context, namespace string) ([]k8s.CognitiveQuota, error) {
	var list k8s.CognitiveQuotaList
	if err := s.client.List(ctx, &list, client.InNamespace(namespace)); err != nil {
		return nil, fmt.Errorf("list CognitiveQuotas for token ledger: %w", err)
	}
	quotas := make([]k8s.CognitiveQuota, 0, len(list.Items))
	for index := range list.Items {
		if list.Items[index].Spec.TokenBudget != nil {
			quotas = append(quotas, list.Items[index])
		}
	}
	sort.Slice(quotas, func(i, j int) bool { return quotas[i].Name < quotas[j].Name })
	return quotas, nil
}

func normalizeTokenLedger(ledger *tokenLedger, quotas []k8s.CognitiveQuota) bool {
	changed := false
	current := make(map[string]struct{}, len(quotas))
	for index := range quotas {
		quota := &quotas[index]
		current[quota.Name] = struct{}{}
		period := k8sQuotaPeriod(quota)
		usage, ok := ledger.Quotas[quota.Name]
		if ok && usage.Period == period {
			continue
		}
		usage = tokenLedgerQuota{Period: period}
		if quota.Status.TokenUsage != nil {
			usage.InputTokens = quota.Status.TokenUsage.InputTokens
			usage.OutputTokens = quota.Status.TokenUsage.OutputTokens
			usage.TotalTokens = quota.Status.TokenUsage.TotalTokens
		}
		ledger.Quotas[quota.Name] = usage
		changed = true
	}
	for name := range ledger.Quotas {
		if _, ok := current[name]; !ok {
			delete(ledger.Quotas, name)
			changed = true
		}
	}
	return changed
}

func moveReservationToCurrentQuotaPeriods(ledger *tokenLedger, reservation *tokenLedgerReservation, quotas []k8s.CognitiveQuota) (bool, error) {
	changed := false
	for index := range quotas {
		quota := &quotas[index]
		usage := ledger.Quotas[quota.Name]
		if reservation.QuotaPeriods[quota.Name] == usage.Period {
			continue
		}
		if err := reserveK8sLedgerQuota(&usage, quota.Spec.TokenBudget, reservation.ReservedInput, reservation.ReservedOutput); err != nil {
			return false, err
		}
		ledger.Quotas[quota.Name] = usage
		reservation.QuotaPeriods[quota.Name] = usage.Period
		changed = true
	}
	return changed, nil
}

func reserveK8sLedgerQuota(usage *tokenLedgerQuota, budget *k8s.TokenBudget, input, output int64) error {
	requestedInput, err := addK8sTokenUsage(usage.InputTokens, input)
	if err != nil {
		return err
	}
	requestedOutput, err := addK8sTokenUsage(usage.OutputTokens, output)
	if err != nil {
		return err
	}
	additionalTotal, err := addK8sTokenUsage(input, output)
	if err != nil {
		return err
	}
	requestedTotal, err := addK8sTokenUsage(usage.TotalTokens, additionalTotal)
	if err != nil {
		return err
	}
	if budget.MaxInputTokens > 0 && requestedInput > budget.MaxInputTokens {
		return fmt.Errorf("%w: input token limit exceeded", store.ErrQuotaExceeded)
	}
	if budget.MaxOutputTokens > 0 && requestedOutput > budget.MaxOutputTokens {
		return fmt.Errorf("%w: output token limit exceeded", store.ErrQuotaExceeded)
	}
	if budget.MaxTotalTokens > 0 && requestedTotal > budget.MaxTotalTokens {
		return fmt.Errorf("%w: total token limit exceeded", store.ErrQuotaExceeded)
	}
	usage.InputTokens = requestedInput
	usage.OutputTokens = requestedOutput
	usage.TotalTokens = requestedTotal
	return nil
}

func adjustK8sLedgerQuota(usage *tokenLedgerQuota, inputDelta, outputDelta, totalDelta int64) error {
	var err error
	usage.InputTokens, err = addK8sTokenDelta(usage.InputTokens, inputDelta)
	if err != nil {
		return err
	}
	usage.OutputTokens, err = addK8sTokenDelta(usage.OutputTokens, outputDelta)
	if err != nil {
		return err
	}
	usage.TotalTokens, err = addK8sTokenDelta(usage.TotalTokens, totalDelta)
	return err
}

func addK8sTokenUsage(left, right int64) (int64, error) {
	if left < 0 || right < 0 || left > int64(^uint64(0)>>1)-right {
		return 0, fmt.Errorf("invalid token usage")
	}
	return left + right, nil
}

func addK8sTokenDelta(current, delta int64) (int64, error) {
	if delta > 0 && current > int64(^uint64(0)>>1)-delta {
		return 0, fmt.Errorf("token usage overflow")
	}
	result := current + delta
	if result < 0 {
		return 0, fmt.Errorf("token usage cannot be negative")
	}
	return result, nil
}

func newTokenLedger() *tokenLedger {
	return &tokenLedger{
		Reservations:       make(map[string]tokenLedgerReservation),
		Quotas:             make(map[string]tokenLedgerQuota),
		CanaryDispositions: make(map[string]store.CanaryReservationDisposition),
	}
}

func decodeTokenLedger(configMap *corev1.ConfigMap) (*tokenLedger, error) {
	ledger := newTokenLedger()
	data := configMap.Data[tokenLedgerDataKey]
	if data == "" {
		return ledger, nil
	}
	if err := json.Unmarshal([]byte(data), ledger); err != nil {
		return nil, fmt.Errorf("decode token reservation ledger %s/%s: %w", configMap.Namespace, configMap.Name, err)
	}
	if ledger.Reservations == nil {
		ledger.Reservations = make(map[string]tokenLedgerReservation)
	}
	if ledger.Quotas == nil {
		ledger.Quotas = make(map[string]tokenLedgerQuota)
	}
	if ledger.CanaryDispositions == nil {
		ledger.CanaryDispositions = make(map[string]store.CanaryReservationDisposition)
	}
	return ledger, nil
}

func k8sQuotaPeriod(quota *k8s.CognitiveQuota) string {
	identity := string(quota.UID)
	if identity == "" && !quota.CreationTimestamp.IsZero() {
		identity = quota.CreationTimestamp.UTC().Format(time.RFC3339Nano)
	}
	if identity == "" {
		identity = quota.Name
	}
	return identity + "|" + quota.Status.LastResetTime
}

func (s *StoreAdapter) tokenLedgerObjectKey(workloadNamespace string) client.ObjectKey {
	storageNamespace := workloadNamespace
	if s.controlNamespace != "" {
		storageNamespace = s.controlNamespace
	}
	digest := sha256.Sum256([]byte(workloadNamespace))
	return client.ObjectKey{
		Namespace: storageNamespace,
		Name:      fmt.Sprintf("%s%x", tokenLedgerConfigMapPrefix, digest[:]),
	}
}

func (s *StoreAdapter) tokenLedgerListOptions() []client.ListOption {
	options := []client.ListOption{client.MatchingLabels{tokenLedgerManagedLabel: tokenLedgerManagedLabelValue}}
	if s.controlNamespace != "" {
		options = append(options, client.InNamespace(s.controlNamespace))
	}
	return options
}

func (s *StoreAdapter) tokenLedgerWorkloadNamespace(configMap *corev1.ConfigMap) (string, error) {
	namespace := configMap.Annotations[tokenLedgerNamespaceAnnotation]
	if namespace == "" && s.controlNamespace == "" {
		namespace = configMap.Namespace
	}
	if namespace == "" || s.tokenLedgerObjectKey(namespace) != client.ObjectKeyFromObject(configMap) {
		return "", fmt.Errorf("token ledger %s/%s has invalid workload namespace binding", configMap.Namespace, configMap.Name)
	}
	return namespace, nil
}

func (s *StoreAdapter) syncTokenLedgerStatus(ctx context.Context, namespace string) error {
	for attempt := 0; attempt < tokenLedgerMaxRetries; attempt++ {
		configMap := &corev1.ConfigMap{}
		key := s.tokenLedgerObjectKey(namespace)
		if err := s.client.Get(ctx, key, configMap); err != nil {
			return fmt.Errorf("get token ledger for status sync: %w", err)
		}
		ledger, err := decodeTokenLedger(configMap)
		if err != nil {
			return err
		}
		ledgerVersion := configMap.ResourceVersion
		quotas, err := s.listTokenQuotas(ctx, namespace)
		if err != nil {
			return err
		}
		for index := range quotas {
			usage, ok := ledger.Quotas[quotas[index].Name]
			if !ok || usage.Period != k8sQuotaPeriod(&quotas[index]) {
				continue
			}
			if err := s.syncTokenQuotaStatus(ctx, &quotas[index], usage); err != nil {
				return err
			}
		}
		latest := &corev1.ConfigMap{}
		if err := s.client.Get(ctx, key, latest); err != nil {
			return fmt.Errorf("verify token ledger status sync: %w", err)
		}
		if latest.ResourceVersion == ledgerVersion {
			return nil
		}
	}
	return store.ErrConflict
}

func (s *StoreAdapter) syncTokenQuotaStatus(ctx context.Context, quota *k8s.CognitiveQuota, usage tokenLedgerQuota) error {
	key := client.ObjectKeyFromObject(quota)
	for attempt := 0; attempt < tokenLedgerMaxRetries; attempt++ {
		latest := &k8s.CognitiveQuota{}
		if err := s.client.Get(ctx, key, latest); err != nil {
			return fmt.Errorf("get CognitiveQuota for token status sync: %w", err)
		}
		if k8sQuotaPeriod(latest) != usage.Period {
			return nil
		}
		latest.Status.TokenUsage = &k8s.QuotaTokenUsage{
			InputTokens:  usage.InputTokens,
			OutputTokens: usage.OutputTokens,
			TotalTokens:  usage.TotalTokens,
		}
		if err := s.client.Status().Update(ctx, latest); err != nil {
			if apierrors.IsConflict(err) {
				continue
			}
			return fmt.Errorf("sync CognitiveQuota token usage: %w", err)
		}
		return nil
	}
	return store.ErrConflict
}
