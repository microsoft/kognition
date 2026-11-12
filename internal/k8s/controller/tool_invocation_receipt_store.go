// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kognition-ai/kognition/internal/store"
)

const (
	toolInvocationReceiptConfigMapPrefix = "kognition-tool-invocations-"
	toolInvocationReceiptDataKey         = "receipts.json"
	toolInvocationReceiptMaxRetries      = 8
	toolInvocationReceiptNamespaceKey    = "kognition.dev/workload-namespace"
	toolInvocationReceiptManagedLabel    = "kognition.dev/tool-invocation-ledger"
)

// ConfigureToolInvocationReceiptWriter isolates gateway ledgers at startup.
// The unscoped legacy ledger remains read-only for these gateway clients.
func (s *StoreAdapter) ConfigureToolInvocationReceiptWriter(component string) error {
	if s.controlNamespace == "" || (component != "tool-gateway" && component != "agent-communication-gateway") {
		return fmt.Errorf("gateway receipts require a control namespace and an explicit Tool or Communication writer")
	}
	if s.toolInvocationReceiptWriter != "" && s.toolInvocationReceiptWriter != component {
		return fmt.Errorf("gateway receipt writer cannot change after configuration")
	}
	s.toolInvocationReceiptWriter = component
	return nil
}

type toolInvocationReceiptLedger struct {
	Receipts map[string]store.ToolInvocationReceipt `json:"receipts"`
}

// ClaimToolInvocation records a pending invocation before authorization or
// execution. The ConfigMap update is optimistic-concurrency-safe across Tool
// Gateway replicas.
func (s *StoreAdapter) ClaimToolInvocation(ctx context.Context, receipt store.ToolInvocationReceipt) (bool, store.ToolInvocationReceipt, error) {
	if receipt.Namespace == "" || receipt.InvocationID == "" || receipt.Binding == "" {
		return false, store.ToolInvocationReceipt{}, fmt.Errorf("tool invocation receipt requires namespace, invocation ID, and binding")
	}
	if legacy, found, err := s.legacyGatewayReceipt(ctx, receipt.Namespace, receipt.InvocationID, receipt.Binding); err != nil || found {
		return false, legacy, err
	}
	var existing store.ToolInvocationReceipt
	claimed, err := s.mutateToolInvocationReceipts(ctx, receipt.Namespace, func(ledger *toolInvocationReceiptLedger) (bool, error) {
		if current, found := ledger.Receipts[receipt.InvocationID]; found {
			if current.Binding != receipt.Binding {
				return false, store.ErrConflict
			}
			if current.Status == store.ToolInvocationAwaitingApproval {
				current.Status = store.ToolInvocationPending
				current.CompletedAt = nil
				ledger.Receipts[receipt.InvocationID] = current
				existing = current
				return true, nil
			}
			existing = current
			return false, nil
		}
		ledger.Receipts[receipt.InvocationID] = receipt
		existing = receipt
		return true, nil
	})
	return claimed, existing, err
}

// PrepareToolInvocation persists the shared preparation token before the
// RuntimeApproval execution claim. An identical preparation is resumable.
func (s *StoreAdapter) PrepareToolInvocation(ctx context.Context, receipt store.ToolInvocationReceipt) (bool, store.ToolInvocationReceipt, error) {
	if receipt.Namespace == "" || receipt.InvocationID == "" || receipt.Binding == "" || receipt.Status != store.ToolInvocationPrepared || receipt.PreparationToken == "" || receipt.PreparationDeadline == nil {
		return false, store.ToolInvocationReceipt{}, fmt.Errorf("prepared tool invocation receipt requires namespace, invocation ID, binding, token, and deadline")
	}
	if legacy, found, err := s.legacyGatewayReceipt(ctx, receipt.Namespace, receipt.InvocationID, receipt.Binding); err != nil || found {
		return false, legacy, err
	}
	var existing store.ToolInvocationReceipt
	prepared, err := s.mutateToolInvocationReceipts(ctx, receipt.Namespace, func(ledger *toolInvocationReceiptLedger) (bool, error) {
		current, found := ledger.Receipts[receipt.InvocationID]
		if found {
			if current.Binding != receipt.Binding || (current.Status != store.ToolInvocationPending && current.Status != store.ToolInvocationPrepared) {
				return false, store.ErrConflict
			}
			if current.Status == store.ToolInvocationPending {
				ledger.Receipts[receipt.InvocationID] = receipt
				existing = receipt
				return true, nil
			}
			if current.PreparationToken != receipt.PreparationToken || current.PreparationDeadline == nil || !current.PreparationDeadline.Equal(*receipt.PreparationDeadline) {
				return false, store.ErrConflict
			}
			existing = current
			return true, nil
		}
		ledger.Receipts[receipt.InvocationID] = receipt
		existing = receipt
		return true, nil
	})
	return prepared, existing, err
}

// ReleasePreparedToolInvocation removes only the exact preparation owned by
// the caller. A claimed or terminal receipt is never eligible for cleanup.
func (s *StoreAdapter) ReleasePreparedToolInvocation(ctx context.Context, namespace, invocationID, preparationToken string) (bool, error) {
	if namespace == "" || invocationID == "" || preparationToken == "" {
		return false, fmt.Errorf("prepared tool invocation release requires namespace, invocation ID, and preparation token")
	}
	if _, found, err := s.legacyGatewayReceipt(ctx, namespace, invocationID, ""); err != nil || found {
		return false, err
	}
	released, err := s.mutateToolInvocationReceipts(ctx, namespace, func(ledger *toolInvocationReceiptLedger) (bool, error) {
		existing, found := ledger.Receipts[invocationID]
		if !found {
			return false, nil
		}
		if existing.Status != store.ToolInvocationPrepared || existing.PreparationToken != preparationToken {
			return false, store.ErrConflict
		}
		delete(ledger.Receipts, invocationID)
		return true, nil
	})
	return released, err
}

// CompleteToolInvocation parks an invocation awaiting approval or stores a
// terminal result. Repeating it is idempotent; changed binding is rejected.
func (s *StoreAdapter) CompleteToolInvocation(ctx context.Context, receipt store.ToolInvocationReceipt) error {
	if receipt.Namespace == "" || receipt.InvocationID == "" || receipt.Binding == "" || receipt.Status == store.ToolInvocationPending {
		return fmt.Errorf("tool invocation completion requires a terminal receipt")
	}
	if _, found, err := s.legacyGatewayReceipt(ctx, receipt.Namespace, receipt.InvocationID, receipt.Binding); err != nil || found {
		return err
	}
	_, err := s.mutateToolInvocationReceipts(ctx, receipt.Namespace, func(ledger *toolInvocationReceiptLedger) (bool, error) {
		current, found := ledger.Receipts[receipt.InvocationID]
		if !found {
			return false, store.ErrNotFound
		}
		if current.Binding != receipt.Binding {
			return false, store.ErrConflict
		}
		if current.Status != store.ToolInvocationPending && current.Status != store.ToolInvocationPrepared {
			return false, nil
		}
		receipt.PreparationToken = ""
		receipt.PreparationDeadline = nil
		ledger.Receipts[receipt.InvocationID] = receipt
		return true, nil
	})
	return err
}

func (s *StoreAdapter) mutateToolInvocationReceipts(ctx context.Context, workloadNamespace string, mutate func(*toolInvocationReceiptLedger) (bool, error)) (bool, error) {
	for attempt := 0; attempt < toolInvocationReceiptMaxRetries; attempt++ {
		configMap := &corev1.ConfigMap{}
		key := s.toolInvocationReceiptObjectKey(workloadNamespace)
		err := s.client.Get(ctx, key, configMap)
		create := apierrors.IsNotFound(err)
		if err != nil && !create {
			return false, fmt.Errorf("get tool invocation receipt ledger: %w", err)
		}
		ledger := newToolInvocationReceiptLedger()
		if !create {
			ledger, err = decodeToolInvocationReceiptLedger(configMap)
			if err != nil {
				return false, err
			}
		}
		changed, err := mutate(ledger)
		if err != nil || !changed {
			return changed, err
		}
		data, err := json.Marshal(ledger)
		if err != nil {
			return false, fmt.Errorf("marshal tool invocation receipt ledger: %w", err)
		}
		if create {
			configMap = &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
				Namespace: key.Namespace, Name: key.Name,
				Labels:      map[string]string{toolInvocationReceiptManagedLabel: "true"},
				Annotations: map[string]string{toolInvocationReceiptNamespaceKey: workloadNamespace},
			}, Data: map[string]string{toolInvocationReceiptDataKey: string(data)}}
			err = s.client.Create(ctx, configMap)
		} else {
			configMap.Data[toolInvocationReceiptDataKey] = string(data)
			err = s.client.Update(ctx, configMap)
		}
		if apierrors.IsAlreadyExists(err) || apierrors.IsConflict(err) {
			continue
		}
		if err != nil {
			return false, fmt.Errorf("persist tool invocation receipt ledger: %w", err)
		}
		return changed, nil
	}
	return false, store.ErrConflict
}

func (s *StoreAdapter) toolInvocationReceiptObjectKey(workloadNamespace string) client.ObjectKey {
	return s.toolInvocationReceiptKey(workloadNamespace, s.toolInvocationReceiptWriter)
}

func (s *StoreAdapter) toolInvocationReceiptKey(workloadNamespace, writer string) client.ObjectKey {
	storageNamespace := workloadNamespace
	if s.controlNamespace != "" {
		storageNamespace = s.controlNamespace
	}
	digest := sha256.Sum256([]byte(workloadNamespace))
	prefix := toolInvocationReceiptConfigMapPrefix
	if writer != "" {
		prefix += writer + "-"
	}
	return client.ObjectKey{Namespace: storageNamespace, Name: fmt.Sprintf("%s%x", prefix, digest[:])}
}

func (s *StoreAdapter) legacyGatewayReceipt(ctx context.Context, namespace, invocationID, binding string) (store.ToolInvocationReceipt, bool, error) {
	if s.toolInvocationReceiptWriter == "" {
		return store.ToolInvocationReceipt{}, false, nil
	}
	var object corev1.ConfigMap
	if err := s.runtimeApprovalReader.Get(ctx, s.toolInvocationReceiptKey(namespace, ""), &object); err != nil {
		if apierrors.IsNotFound(err) {
			return store.ToolInvocationReceipt{}, false, nil
		}
		return store.ToolInvocationReceipt{}, false, fmt.Errorf("read retained legacy gateway receipts: %w", err)
	}
	if object.Labels[toolInvocationReceiptManagedLabel] != "true" || object.Annotations[toolInvocationReceiptNamespaceKey] != namespace || object.Data[toolInvocationReceiptDataKey] == "" {
		return store.ToolInvocationReceipt{}, false, fmt.Errorf("legacy gateway receipt ledger has unverified ownership or payload")
	}
	ledger, err := decodeToolInvocationReceiptLedger(&object)
	if err != nil {
		return store.ToolInvocationReceipt{}, false, fmt.Errorf("decode retained legacy gateway receipts: %w", err)
	}
	receipt, found := ledger.Receipts[invocationID]
	if !found {
		return store.ToolInvocationReceipt{}, false, nil
	}
	if receipt.Namespace != namespace || receipt.InvocationID != invocationID || receipt.CompletedAt == nil || (binding != "" && receipt.Binding != binding) ||
		(receipt.Status != store.ToolInvocationSucceeded && receipt.Status != store.ToolInvocationFailed) {
		return store.ToolInvocationReceipt{}, true, fmt.Errorf("%w: legacy gateway invocation requires reviewed recovery; its receipt cannot be rewritten or replayed", store.ErrConflict)
	}
	return receipt, true, nil
}

func newToolInvocationReceiptLedger() *toolInvocationReceiptLedger {
	return &toolInvocationReceiptLedger{Receipts: make(map[string]store.ToolInvocationReceipt)}
}

func decodeToolInvocationReceiptLedger(configMap *corev1.ConfigMap) (*toolInvocationReceiptLedger, error) {
	ledger := newToolInvocationReceiptLedger()
	data := configMap.Data[toolInvocationReceiptDataKey]
	if data == "" {
		return ledger, nil
	}
	if err := json.Unmarshal([]byte(data), ledger); err != nil {
		return nil, fmt.Errorf("decode tool invocation receipt ledger %s/%s: %w", configMap.Namespace, configMap.Name, err)
	}
	if ledger.Receipts == nil {
		ledger.Receipts = make(map[string]store.ToolInvocationReceipt)
	}
	return ledger, nil
}
