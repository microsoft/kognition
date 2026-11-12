// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"crypto/sha256"
	"encoding/json"
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
	toolCallLedgerConfigMapPrefix     = "kognition-tool-call-reservations-"
	toolCallLedgerDataKey             = "ledger.json"
	toolCallLedgerManagedLabel        = "kognition.dev/tool-call-ledger"
	toolCallLedgerManagedLabelValue   = "true"
	toolCallLedgerNamespaceAnnotation = "kognition.dev/quota-namespace"
)

type toolCallLedger struct {
	Reservations map[string]toolCallLedgerReservation `json:"reservations"`
	Quotas       map[string]toolCallLedgerQuota       `json:"quotas"`
}

type toolCallLedgerReservation struct {
	Binding             string            `json:"binding"`
	PreparationToken    string            `json:"preparationToken"`
	PreparationDeadline time.Time         `json:"preparationDeadline"`
	QuotaPeriods        map[string]string `json:"quotaPeriods"`
}

type toolCallLedgerQuota struct {
	Period    string `json:"period"`
	ToolCalls int64  `json:"toolCalls"`
}

func (s *StoreAdapter) PrepareToolCallReservation(ctx context.Context, reservation store.ToolCallReservation) (bool, error) {
	if reservation.Namespace == "" || reservation.InvocationID == "" || reservation.Binding == "" || reservation.PreparationToken == "" {
		return false, fmt.Errorf("Tool-call reservation namespace, invocation ID, binding, and preparation token are required")
	}
	if reservation.PreparationDeadline.IsZero() {
		return false, fmt.Errorf("Tool-call preparation deadline is required")
	}
	if !reservation.PreparationDeadline.After(s.now()) {
		return false, fmt.Errorf("Tool-call preparation deadline must be in the future")
	}

	created := false
	_, err := s.mutateToolCallLedger(ctx, reservation.Namespace, func(ledger *toolCallLedger, quotas []k8s.CognitiveQuota) (bool, error) {
		if existing, ok := ledger.Reservations[reservation.InvocationID]; ok {
			if existing.Binding != reservation.Binding ||
				existing.PreparationToken != reservation.PreparationToken ||
				!existing.PreparationDeadline.Equal(reservation.PreparationDeadline) {
				return false, store.ErrConflict
			}
			return false, nil
		}

		for index := range quotas {
			quota := &quotas[index]
			usage := ledger.Quotas[quota.Name]
			if usage.ToolCalls == int64(^uint64(0)>>1) {
				return false, fmt.Errorf("Tool-call usage overflow")
			}
			requested := usage.ToolCalls + 1
			if limit := quota.Spec.CostBudget.MaxToolCallsPerPeriod; limit > 0 && requested > limit {
				return false, fmt.Errorf("%w: Tool call limit exceeded", store.ErrQuotaExceeded)
			}
		}
		for index := range quotas {
			quota := &quotas[index]
			usage := ledger.Quotas[quota.Name]
			usage.ToolCalls++
			ledger.Quotas[quota.Name] = usage
		}
		ledger.Reservations[reservation.InvocationID] = toolCallLedgerReservation{
			Binding:             reservation.Binding,
			PreparationToken:    reservation.PreparationToken,
			PreparationDeadline: reservation.PreparationDeadline,
			QuotaPeriods:        make(map[string]string, len(quotas)),
		}
		entry := ledger.Reservations[reservation.InvocationID]
		for index := range quotas {
			entry.QuotaPeriods[quotas[index].Name] = ledger.Quotas[quotas[index].Name].Period
		}
		ledger.Reservations[reservation.InvocationID] = entry
		created = true
		return true, nil
	})
	if err != nil {
		return false, err
	}
	return created, nil
}

func (s *StoreAdapter) SettleToolCallReservation(ctx context.Context, namespace, invocationID, preparationToken string) (bool, error) {
	return s.finalizeToolCallReservation(ctx, namespace, invocationID, preparationToken, false)
}

func (s *StoreAdapter) ReleaseToolCallReservation(ctx context.Context, namespace, invocationID, preparationToken string) (bool, error) {
	return s.finalizeToolCallReservation(ctx, namespace, invocationID, preparationToken, true)
}

func (s *StoreAdapter) finalizeToolCallReservation(ctx context.Context, namespace, invocationID, preparationToken string, refund bool) (bool, error) {
	if namespace == "" || invocationID == "" || preparationToken == "" {
		return false, fmt.Errorf("Tool-call reservation namespace, invocation ID, and preparation token are required")
	}
	found := false
	_, err := s.mutateToolCallLedger(ctx, namespace, func(ledger *toolCallLedger, quotas []k8s.CognitiveQuota) (bool, error) {
		reservation, ok := ledger.Reservations[invocationID]
		if !ok {
			return false, nil
		}
		if reservation.PreparationToken != preparationToken {
			return false, store.ErrConflict
		}
		if refund {
			for index := range quotas {
				quota := &quotas[index]
				usage := ledger.Quotas[quota.Name]
				if reservation.QuotaPeriods[quota.Name] != usage.Period {
					continue
				}
				if usage.ToolCalls <= 0 {
					return false, fmt.Errorf("Tool-call usage cannot be negative")
				}
				usage.ToolCalls--
				ledger.Quotas[quota.Name] = usage
			}
		}
		delete(ledger.Reservations, invocationID)
		found = true
		return true, nil
	})
	return found, err
}

func (s *StoreAdapter) mutateToolCallLedger(
	ctx context.Context,
	namespace string,
	mutate func(*toolCallLedger, []k8s.CognitiveQuota) (bool, error),
) (bool, error) {
	for attempt := 0; attempt < tokenLedgerMaxRetries; attempt++ {
		quotas, err := s.listToolCallQuotas(ctx, namespace)
		if err != nil {
			return false, err
		}
		configMap := &corev1.ConfigMap{}
		key := s.toolCallLedgerObjectKey(namespace)
		err = s.client.Get(ctx, key, configMap)
		create := apierrors.IsNotFound(err)
		if err != nil && !create {
			return false, fmt.Errorf("get Tool-call reservation ledger: %w", err)
		}
		ledger := newToolCallLedger()
		if !create {
			ledger, err = decodeToolCallLedger(configMap)
			if err != nil {
				return false, err
			}
		}
		normalized := normalizeToolCallLedger(ledger, quotas)
		changed, err := mutate(ledger, quotas)
		if err != nil {
			return false, err
		}
		if !normalized && !changed && create {
			return false, nil
		}
		if !normalized && !changed {
			if err := s.syncToolCallLedgerStatus(ctx, namespace); err != nil {
				return false, err
			}
			return false, nil
		}
		data, err := json.Marshal(ledger)
		if err != nil {
			return false, fmt.Errorf("marshal Tool-call reservation ledger: %w", err)
		}
		if create {
			configMap = &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: key.Namespace,
					Name:      key.Name,
					Labels:    map[string]string{toolCallLedgerManagedLabel: toolCallLedgerManagedLabelValue},
					Annotations: map[string]string{
						toolCallLedgerNamespaceAnnotation: namespace,
					},
				},
				Data: map[string]string{toolCallLedgerDataKey: string(data)},
			}
			err = s.client.Create(ctx, configMap)
		} else {
			configMap.Data[toolCallLedgerDataKey] = string(data)
			err = s.client.Update(ctx, configMap)
		}
		if apierrors.IsAlreadyExists(err) || apierrors.IsConflict(err) {
			continue
		}
		if err != nil {
			return false, fmt.Errorf("persist Tool-call reservation ledger: %w", err)
		}
		if err := s.syncToolCallLedgerStatus(ctx, namespace); err != nil {
			return false, err
		}
		return changed, nil
	}
	return false, store.ErrConflict
}

func (s *StoreAdapter) listToolCallQuotas(ctx context.Context, namespace string) ([]k8s.CognitiveQuota, error) {
	var list k8s.CognitiveQuotaList
	if err := s.client.List(ctx, &list, client.InNamespace(namespace)); err != nil {
		return nil, fmt.Errorf("list CognitiveQuotas for Tool-call ledger: %w", err)
	}
	quotas := make([]k8s.CognitiveQuota, 0, len(list.Items))
	for index := range list.Items {
		if list.Items[index].Spec.CostBudget != nil {
			quotas = append(quotas, list.Items[index])
		}
	}
	sort.Slice(quotas, func(i, j int) bool { return quotas[i].Name < quotas[j].Name })
	return quotas, nil
}

func normalizeToolCallLedger(ledger *toolCallLedger, quotas []k8s.CognitiveQuota) bool {
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
		usage = toolCallLedgerQuota{Period: period}
		if quota.Status.CostUsage != nil {
			usage.ToolCalls = quota.Status.CostUsage.ToolCalls
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

func newToolCallLedger() *toolCallLedger {
	return &toolCallLedger{
		Reservations: make(map[string]toolCallLedgerReservation),
		Quotas:       make(map[string]toolCallLedgerQuota),
	}
}

func decodeToolCallLedger(configMap *corev1.ConfigMap) (*toolCallLedger, error) {
	ledger := newToolCallLedger()
	data := configMap.Data[toolCallLedgerDataKey]
	if data == "" {
		return ledger, nil
	}
	if err := json.Unmarshal([]byte(data), ledger); err != nil {
		return nil, fmt.Errorf("decode Tool-call reservation ledger %s/%s: %w", configMap.Namespace, configMap.Name, err)
	}
	if ledger.Reservations == nil {
		ledger.Reservations = make(map[string]toolCallLedgerReservation)
	}
	if ledger.Quotas == nil {
		ledger.Quotas = make(map[string]toolCallLedgerQuota)
	}
	return ledger, nil
}

func (s *StoreAdapter) toolCallLedgerObjectKey(workloadNamespace string) client.ObjectKey {
	storageNamespace := workloadNamespace
	if s.controlNamespace != "" {
		storageNamespace = s.controlNamespace
	}
	digest := sha256.Sum256([]byte(workloadNamespace))
	return client.ObjectKey{
		Namespace: storageNamespace,
		Name:      fmt.Sprintf("%s%x", toolCallLedgerConfigMapPrefix, digest[:]),
	}
}

func (s *StoreAdapter) syncToolCallLedgerStatus(ctx context.Context, namespace string) error {
	for attempt := 0; attempt < tokenLedgerMaxRetries; attempt++ {
		configMap := &corev1.ConfigMap{}
		key := s.toolCallLedgerObjectKey(namespace)
		if err := s.client.Get(ctx, key, configMap); err != nil {
			return fmt.Errorf("get Tool-call ledger for status sync: %w", err)
		}
		ledger, err := decodeToolCallLedger(configMap)
		if err != nil {
			return err
		}
		ledgerVersion := configMap.ResourceVersion
		quotas, err := s.listToolCallQuotas(ctx, namespace)
		if err != nil {
			return err
		}
		for index := range quotas {
			usage, ok := ledger.Quotas[quotas[index].Name]
			if !ok || usage.Period != k8sQuotaPeriod(&quotas[index]) {
				continue
			}
			if err := s.syncToolCallQuotaStatus(ctx, &quotas[index], usage); err != nil {
				return err
			}
		}
		latest := &corev1.ConfigMap{}
		if err := s.client.Get(ctx, key, latest); err != nil {
			return fmt.Errorf("verify Tool-call ledger status sync: %w", err)
		}
		if latest.ResourceVersion == ledgerVersion {
			return nil
		}
	}
	return store.ErrConflict
}

func (s *StoreAdapter) syncToolCallQuotaStatus(ctx context.Context, quota *k8s.CognitiveQuota, usage toolCallLedgerQuota) error {
	key := client.ObjectKeyFromObject(quota)
	for attempt := 0; attempt < tokenLedgerMaxRetries; attempt++ {
		latest := &k8s.CognitiveQuota{}
		if err := s.client.Get(ctx, key, latest); err != nil {
			return fmt.Errorf("get CognitiveQuota for Tool-call status sync: %w", err)
		}
		if k8sQuotaPeriod(latest) != usage.Period {
			return nil
		}
		if latest.Status.CostUsage == nil {
			latest.Status.CostUsage = &k8s.QuotaCostUsage{}
		}
		latest.Status.CostUsage.ToolCalls = usage.ToolCalls
		if err := s.client.Status().Update(ctx, latest); err != nil {
			if apierrors.IsConflict(err) {
				continue
			}
			return fmt.Errorf("sync CognitiveQuota Tool-call usage: %w", err)
		}
		return nil
	}
	return store.ErrConflict
}

var _ store.ToolCallReservationStore = (*StoreAdapter)(nil)
