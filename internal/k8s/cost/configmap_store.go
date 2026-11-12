// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package cost persists normalized CostRecords through immutable Kubernetes ConfigMaps.
package cost

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlconfig "sigs.k8s.io/controller-runtime/pkg/client/config"

	k8s "github.com/kognition-ai/kognition/api/k8s/v1alpha1"
	"github.com/kognition-ai/kognition/internal/observability"
)

const (
	configMapPrefix    = "kognition-cost-"
	receiptPrefix      = "kognition-usage-receipt-"
	recordsDataKey     = "records.json"
	fingerprintDataKey = "fingerprint"
	managedLabel       = "kognition.dev/cost-record"
	usageEventLabel    = "kognition.dev/usage-event"
	receiptLabel       = "kognition.dev/usage-receipt"
	workspaceHashLabel = "kognition.dev/cost-workspace"
	queryPageSize      = 100
	maxQueryPages      = 10
	maxRetentionPages  = 10
	maxPayloadBytes    = 900 * 1024
	defaultQueryLimit  = 100
	maxQueryLimit      = 1000
	usageLockDuration  = 15 * time.Second
)

// ConfigMapStore stores immutable CostRecord batches in a control namespace.
type ConfigMapStore struct {
	client    client.Client
	namespace string
}

type usageLockContextKey struct{}

type usageLockIdentity struct {
	key    client.ObjectKey
	holder string
}

var _ observability.CostStore = (*ConfigMapStore)(nil)
var _ observability.UsageEventStore = (*ConfigMapStore)(nil)

// NewConfigMapStore creates a ConfigMap-backed CostStore.
func NewConfigMapStore(kubernetesClient client.Client, namespace string) (*ConfigMapStore, error) {
	if kubernetesClient == nil {
		return nil, fmt.Errorf("Kubernetes cost store requires a client")
	}
	if namespace == "" {
		return nil, fmt.Errorf("Kubernetes cost store requires a namespace")
	}
	return &ConfigMapStore{client: kubernetesClient, namespace: namespace}, nil
}

// NewInClusterConfigMapStore creates a CostStore with pod service-account credentials.
func NewInClusterConfigMapStore(namespace string) (*ConfigMapStore, error) {
	config, err := ctrlconfig.GetConfig()
	if err != nil {
		return nil, fmt.Errorf("load in-cluster Kubernetes config: %w", err)
	}
	scheme, err := newConfigMapStoreScheme()
	if err != nil {
		return nil, err
	}
	kubernetesClient, err := client.New(config, client.Options{Scheme: scheme})
	if err != nil {
		return nil, fmt.Errorf("create Kubernetes cost client: %w", err)
	}
	return NewConfigMapStore(kubernetesClient, namespace)
}

func newConfigMapStoreScheme() (*runtime.Scheme, error) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		return nil, fmt.Errorf("register core Kubernetes cost scheme: %w", err)
	}
	if err := coordinationv1.AddToScheme(scheme); err != nil {
		return nil, fmt.Errorf("register Kubernetes coordination cost scheme: %w", err)
	}
	if err := k8s.AddToScheme(scheme); err != nil {
		return nil, fmt.Errorf("register Kognition cost scheme: %w", err)
	}
	return scheme, nil
}

// ApplyUsageEvent durably creates one immutable model usage event and applies
// it incrementally. Exact retries rebuild projections to repair interrupted
// writes from immutable receipt-bound events.
func (s *ConfigMapStore) ApplyUsageEvent(ctx context.Context, event observability.CostRecord) (bool, error) {
	if event.OperationType != observability.CostOpModelInference {
		return false, fmt.Errorf("usage event requires model inference operation type")
	}
	if err := validateRecord(&event); err != nil {
		return false, err
	}
	if event.ID == "" {
		return false, fmt.Errorf("usage event requires an idempotency ID")
	}
	lockCtx, release, err := s.acquireUsageLock(ctx, event.Workspace)
	if err != nil {
		return false, err
	}
	defer release()
	ctx = lockCtx
	payload, err := json.Marshal([]observability.CostRecord{event})
	if err != nil {
		return false, fmt.Errorf("marshal Kubernetes usage event: %w", err)
	}
	fingerprint := fmt.Sprintf("%x", sha256.Sum256(payload))
	receipt := usageEventReceiptConfigMap(s.namespace, event, fingerprint)
	var storedReceipt corev1.ConfigMap
	if err := s.client.Get(ctx, client.ObjectKeyFromObject(receipt), &storedReceipt); err == nil {
		if storedReceipt.Data[fingerprintDataKey] != fingerprint {
			return false, fmt.Errorf("Kubernetes usage event ID %q was reused with different content", event.ID)
		}
		if err := s.rebuildQuotaProjections(ctx, event.Workspace); err != nil {
			return false, err
		}
		return false, nil
	} else if !apierrors.IsNotFound(err) {
		return false, fmt.Errorf("load Kubernetes usage event receipt: %w", err)
	}
	configMap := usageEventConfigMap(s.namespace, event, payload)
	if err := s.client.Create(ctx, configMap); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return false, fmt.Errorf("persist Kubernetes usage event: %w", err)
		}
		var existing corev1.ConfigMap
		if err := s.client.Get(ctx, client.ObjectKeyFromObject(configMap), &existing); err != nil {
			return false, fmt.Errorf("load duplicate Kubernetes usage event: %w", err)
		}
		if !bytes.Equal(existing.BinaryData[recordsDataKey], payload) {
			return false, fmt.Errorf("Kubernetes usage event ID %q was reused with different content", event.ID)
		}
	}
	applied := true
	if err := s.client.Create(ctx, receipt); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return false, fmt.Errorf("persist Kubernetes usage event receipt: %w", err)
		}
		applied = false
		if err := s.client.Get(ctx, client.ObjectKeyFromObject(receipt), &storedReceipt); err != nil {
			return false, fmt.Errorf("load concurrent Kubernetes usage event receipt: %w", err)
		}
		if storedReceipt.Data[fingerprintDataKey] != fingerprint {
			return false, fmt.Errorf("Kubernetes usage event ID %q was reused with different content", event.ID)
		}
	}
	if applied {
		if err := s.applyEventToQuotaProjections(ctx, event); err != nil {
			return false, err
		}
	} else if err := s.rebuildQuotaProjections(ctx, event.Workspace); err != nil {
		return false, err
	}
	return applied, nil
}

// QueryUsageEvents returns every retained receipt-bound immutable model usage
// event after validating the stored event and receipt pair.
func (s *ConfigMapStore) QueryUsageEvents(ctx context.Context) ([]observability.CostRecord, error) {
	var records []observability.CostRecord
	continueToken := ""
	seenTokens := make(map[string]struct{})
	for {
		var configMaps corev1.ConfigMapList
		options := []client.ListOption{
			client.InNamespace(s.namespace),
			client.MatchingLabels{managedLabel: "true", usageEventLabel: "true"},
			client.Limit(queryPageSize),
		}
		if continueToken != "" {
			options = append(options, client.Continue(continueToken))
		}
		if err := s.client.List(ctx, &configMaps, options...); err != nil {
			return nil, fmt.Errorf("list retained Kubernetes usage events: %w", err)
		}
		for index := range configMaps.Items {
			batch, err := decodeRecords(&configMaps.Items[index])
			if err != nil {
				return nil, err
			}
			if err := s.validateUsageEventReceipt(ctx, &configMaps.Items[index], batch); err != nil {
				return nil, err
			}
			for recordIndex := range batch {
				if err := validateRecord(&batch[recordIndex]); err != nil {
					return nil, fmt.Errorf("validate retained Kubernetes usage event %s/%s: %w", configMaps.Items[index].Namespace, configMaps.Items[index].Name, err)
				}
				if batch[recordIndex].OperationType != observability.CostOpModelInference {
					return nil, fmt.Errorf("retained Kubernetes usage event %s/%s has operation type %q", configMaps.Items[index].Namespace, configMaps.Items[index].Name, batch[recordIndex].OperationType)
				}
			}
			records = append(records, batch...)
		}
		if configMaps.Continue == "" {
			return records, nil
		}
		if _, seen := seenTokens[configMaps.Continue]; seen {
			return nil, fmt.Errorf("Kubernetes usage event query received repeated continuation token")
		}
		seenTokens[configMaps.Continue] = struct{}{}
		continueToken = configMaps.Continue
	}
}

// RecordCost persists one CostRecord. A trace/span pair is idempotent.
func (s *ConfigMapStore) RecordCost(ctx context.Context, record observability.CostRecord) error {
	if err := validateRecord(&record); err != nil {
		return err
	}
	if record.OperationType == observability.CostOpModelInference && record.ID != "" {
		return fmt.Errorf("model usage events with an ID require ApplyUsageEvent")
	}
	payload, err := json.Marshal([]observability.CostRecord{record})
	if err != nil {
		return fmt.Errorf("marshal Kubernetes cost record: %w", err)
	}
	name := configMapPrefix + recordName(record)
	configMap := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: s.namespace,
			Name:      name,
			Labels: map[string]string{
				managedLabel:       "true",
				workspaceHashLabel: workspaceHash(record.Workspace),
			},
		},
		BinaryData: map[string][]byte{recordsDataKey: payload},
		Immutable:  boolPtr(true),
	}
	if err := s.client.Create(ctx, configMap); err != nil {
		if !apierrors.IsAlreadyExists(err) || record.TraceID == "" || record.SpanID == "" {
			return fmt.Errorf("persist Kubernetes cost record: %w", err)
		}
		var existing corev1.ConfigMap
		if getErr := s.client.Get(ctx, client.ObjectKeyFromObject(configMap), &existing); getErr != nil {
			return fmt.Errorf("load duplicate Kubernetes cost record: %w", getErr)
		}
		records, decodeErr := decodeRecords(&existing)
		if decodeErr != nil || len(records) != 1 || records[0].TraceID != record.TraceID || records[0].SpanID != record.SpanID {
			return fmt.Errorf("Kubernetes cost record name collision for trace/span %q/%q", record.TraceID, record.SpanID)
		}
	}
	return nil
}

// RecordCostBatch persists a same-workspace batch atomically in one immutable ConfigMap.
func (s *ConfigMapStore) RecordCostBatch(ctx context.Context, records []observability.CostRecord) error {
	if len(records) == 0 {
		return nil
	}
	stored := append([]observability.CostRecord(nil), records...)
	for index := range stored {
		if err := validateRecord(&stored[index]); err != nil {
			return fmt.Errorf("cost record %d: %w", index, err)
		}
		if stored[index].Workspace != stored[0].Workspace {
			return fmt.Errorf("Kubernetes cost batch entries must share workspace %q", stored[0].Workspace)
		}
		if stored[index].OperationType == observability.CostOpModelInference && stored[index].ID != "" {
			return fmt.Errorf("cost record %d: model usage events with an ID require ApplyUsageEvent", index)
		}
	}
	payload, err := json.Marshal(stored)
	if err != nil {
		return fmt.Errorf("marshal Kubernetes cost batch: %w", err)
	}
	if len(payload) > maxPayloadBytes {
		return fmt.Errorf("Kubernetes cost batch is %d bytes; maximum is %d bytes", len(payload), maxPayloadBytes)
	}
	configMap := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: s.namespace,
			Name:      configMapPrefix + uuid.NewString(),
			Labels: map[string]string{
				managedLabel:       "true",
				workspaceHashLabel: workspaceHash(stored[0].Workspace),
			},
		},
		BinaryData: map[string][]byte{recordsDataKey: payload},
		Immutable:  boolPtr(true),
	}
	if err := s.client.Create(ctx, configMap); err != nil {
		return fmt.Errorf("persist Kubernetes cost batch: %w", err)
	}
	return nil
}

// QueryCosts aggregates workspace-scoped records with bounded ConfigMap pagination.
func (s *ConfigMapStore) QueryCosts(ctx context.Context, query observability.CostQuery) (*observability.CostQueryResult, error) {
	if query.Workspace == "" {
		return nil, fmt.Errorf("Kubernetes cost query requires workspace")
	}
	groups, err := normalizeGroups(query.GroupBy)
	if err != nil {
		return nil, err
	}
	if query.Limit < 0 || query.Offset < 0 {
		return nil, fmt.Errorf("cost query limit and offset must be non-negative")
	}
	aggregates := make(map[string]*observability.CostAggregation)
	continueToken := ""
	for page := 0; page < maxQueryPages; page++ {
		var configMaps corev1.ConfigMapList
		options := []client.ListOption{client.InNamespace(s.namespace), client.MatchingLabels{managedLabel: "true", workspaceHashLabel: workspaceHash(query.Workspace)}, client.Limit(queryPageSize)}
		if continueToken != "" {
			options = append(options, client.Continue(continueToken))
		}
		if err := s.client.List(ctx, &configMaps, options...); err != nil {
			return nil, fmt.Errorf("list Kubernetes cost records: %w", err)
		}
		for index := range configMaps.Items {
			records, err := decodeRecords(&configMaps.Items[index])
			if err != nil {
				return nil, err
			}
			for recordIndex := range records {
				record := records[recordIndex]
				if !matchesQuery(record, query) {
					continue
				}
				key, aggregation := aggregateFor(record, groups)
				existing, exists := aggregates[key]
				if !exists {
					aggregates[key] = &aggregation
					continue
				}
				existing.TotalCostUnits += aggregation.TotalCostUnits
				existing.TotalPromptTokens += aggregation.TotalPromptTokens
				existing.TotalCompletionTokens += aggregation.TotalCompletionTokens
				existing.TotalOperations += aggregation.TotalOperations
				if aggregation.PeriodStart.Before(existing.PeriodStart) {
					existing.PeriodStart = aggregation.PeriodStart
				}
				if aggregation.PeriodEnd.After(existing.PeriodEnd) {
					existing.PeriodEnd = aggregation.PeriodEnd
				}
			}
		}
		if configMaps.Continue == "" {
			break
		}
		if page == maxQueryPages-1 {
			return nil, fmt.Errorf("Kubernetes cost query exceeds %d ConfigMap pages; narrow the filter", maxQueryPages)
		}
		continueToken = configMaps.Continue
	}
	items := make([]observability.CostAggregation, 0, len(aggregates))
	for _, aggregation := range aggregates {
		items = append(items, *aggregation)
	}
	sort.Slice(items, func(left, right int) bool {
		return aggregateKey(items[left], groups) < aggregateKey(items[right], groups)
	})
	result := &observability.CostQueryResult{TotalCount: int64(len(items))}
	limit := query.Limit
	if limit == 0 {
		limit = defaultQueryLimit
	}
	if limit > maxQueryLimit {
		limit = maxQueryLimit
	}
	if query.Offset >= len(items) {
		result.Aggregations = []observability.CostAggregation{}
		return result, nil
	}
	end := query.Offset + limit
	if end > len(items) {
		end = len(items)
	}
	result.Aggregations = items[query.Offset:end]
	return result, nil
}

// GetTotalCost returns the total normalized cost for one workspace and time range.
func (s *ConfigMapStore) GetTotalCost(ctx context.Context, workspace string, start, end time.Time) (observability.CostUnit, error) {
	result, err := s.QueryCosts(ctx, observability.CostQuery{Workspace: workspace, Start: start, End: end, GroupBy: []observability.CostGroupBy{observability.GroupByWorkspace}, Limit: 1})
	if err != nil || len(result.Aggregations) == 0 {
		if err != nil {
			return 0, err
		}
		return 0, nil
	}
	return result.Aggregations[0].TotalCostUnits, nil
}

// DeleteCostRecordsBefore deletes immutable batches whose every record is older
// than the cutoff and belongs to the requested workspace.
func (s *ConfigMapStore) DeleteCostRecordsBefore(ctx context.Context, workspace string, before time.Time) (int64, error) {
	if workspace == "" || before.IsZero() {
		return 0, fmt.Errorf("Kubernetes cost retention requires workspace and cutoff")
	}
	periodStart, err := s.activeCostPeriodStart(ctx, workspace)
	if err != nil {
		return 0, err
	}
	if !periodStart.IsZero() && periodStart.Before(before) {
		before = periodStart
	}
	var deleted int64
	continueToken := ""
	for page := 0; page < maxRetentionPages; page++ {
		var configMaps corev1.ConfigMapList
		options := []client.ListOption{client.InNamespace(s.namespace), client.MatchingLabels{managedLabel: "true", workspaceHashLabel: workspaceHash(workspace)}, client.Limit(queryPageSize)}
		if continueToken != "" {
			options = append(options, client.Continue(continueToken))
		}
		if err := s.client.List(ctx, &configMaps, options...); err != nil {
			return deleted, fmt.Errorf("list Kubernetes cost records for retention: %w", err)
		}
		for index := range configMaps.Items {
			records, err := decodeRecords(&configMaps.Items[index])
			if err != nil {
				return deleted, err
			}
			if !recordsBelongToWorkspace(records, workspace) || !recordsBefore(records, before) {
				continue
			}
			if err := s.client.Delete(ctx, &configMaps.Items[index]); err != nil && !apierrors.IsNotFound(err) {
				return deleted, fmt.Errorf("delete Kubernetes cost records %s/%s: %w", configMaps.Items[index].Namespace, configMaps.Items[index].Name, err)
			}
			deleted += int64(len(records))
		}
		if configMaps.Continue == "" {
			return deleted, nil
		}
		if page == maxRetentionPages-1 {
			return deleted, fmt.Errorf("Kubernetes cost retention exceeds %d ConfigMap pages for workspace %q", maxRetentionPages, workspace)
		}
		continueToken = configMaps.Continue
	}
	return deleted, nil
}

func (s *ConfigMapStore) activeCostPeriodStart(ctx context.Context, workspace string) (time.Time, error) {
	var quotas k8s.CognitiveQuotaList
	if err := s.client.List(ctx, &quotas, client.InNamespace(workspace)); err != nil {
		return time.Time{}, fmt.Errorf("list CognitiveQuotas for cost retention: %w", err)
	}
	var earliest time.Time
	for index := range quotas.Items {
		quota := &quotas.Items[index]
		if quota.Spec.CostBudget == nil {
			continue
		}
		start := quota.CreationTimestamp.Time
		if quota.Status.LastResetTime != "" {
			lastReset, err := time.Parse(time.RFC3339Nano, quota.Status.LastResetTime)
			if err != nil {
				return time.Time{}, fmt.Errorf("invalid last reset time %q", quota.Status.LastResetTime)
			}
			if lastReset.After(start) {
				start = lastReset
			}
		}
		if earliest.IsZero() || start.Before(earliest) {
			earliest = start
		}
	}
	return earliest, nil
}

func validateRecord(record *observability.CostRecord) error {
	if record.Workspace == "" || record.OperationType == "" {
		return fmt.Errorf("cost record requires workspace and operation type")
	}
	if record.PromptTokens < 0 || record.CompletionTokens < 0 || record.TotalTokens < 0 || record.CostUnits < 0 || record.InputCostMicrousdPerMillionTokens < 0 || record.OutputCostMicrousdPerMillionTokens < 0 || record.ProviderDurationMillis < 0 {
		return fmt.Errorf("cost record values must be non-negative")
	}
	if record.OperationType == observability.CostOpModelInference && record.TotalTokens != record.PromptTokens+record.CompletionTokens {
		return fmt.Errorf("model cost record total tokens must equal prompt plus completion tokens")
	}
	if !record.Purpose.Valid() {
		return fmt.Errorf("unsupported usage purpose %q", record.Purpose)
	}
	if record.Timestamp.IsZero() {
		record.Timestamp = time.Now().UTC()
	}
	return nil
}

func decodeRecords(configMap *corev1.ConfigMap) ([]observability.CostRecord, error) {
	payload := configMap.BinaryData[recordsDataKey]
	if len(payload) == 0 {
		return nil, fmt.Errorf("decode Kubernetes cost records %s/%s: missing %s", configMap.Namespace, configMap.Name, recordsDataKey)
	}
	var records []observability.CostRecord
	if err := json.Unmarshal(payload, &records); err != nil {
		return nil, fmt.Errorf("decode Kubernetes cost records %s/%s: %w", configMap.Namespace, configMap.Name, err)
	}
	return records, nil
}

func matchesQuery(record observability.CostRecord, query observability.CostQuery) bool {
	if record.Workspace != query.Workspace || (query.AgentIdentity != "" && record.AgentIdentity != query.AgentIdentity) || (query.Solution != "" && record.Solution != query.Solution) || (query.SolutionRev != nil && record.SolutionRev != *query.SolutionRev) {
		return false
	}
	if !query.Start.IsZero() && record.Timestamp.Before(query.Start) {
		return false
	}
	return query.End.IsZero() || !record.Timestamp.After(query.End)
}

func normalizeGroups(groups []observability.CostGroupBy) ([]observability.CostGroupBy, error) {
	if len(groups) == 0 {
		return []observability.CostGroupBy{observability.GroupByWorkspace}, nil
	}
	seen := make(map[observability.CostGroupBy]struct{}, len(groups))
	for _, group := range groups {
		switch group {
		case observability.GroupByWorkspace, observability.GroupByAgent, observability.GroupBySolution, observability.GroupByModel, observability.GroupByOperationType:
		default:
			return nil, fmt.Errorf("unsupported cost group %q", group)
		}
		if _, exists := seen[group]; exists {
			return nil, fmt.Errorf("duplicate cost group %q", group)
		}
		seen[group] = struct{}{}
	}
	return groups, nil
}

func aggregateFor(record observability.CostRecord, groups []observability.CostGroupBy) (string, observability.CostAggregation) {
	aggregation := observability.CostAggregation{TotalCostUnits: record.CostUnits, TotalPromptTokens: record.PromptTokens, TotalCompletionTokens: record.CompletionTokens, TotalOperations: 1, PeriodStart: record.Timestamp, PeriodEnd: record.Timestamp}
	for _, group := range groups {
		switch group {
		case observability.GroupByWorkspace:
			aggregation.Workspace = record.Workspace
		case observability.GroupByAgent:
			aggregation.AgentIdentity = record.AgentIdentity
		case observability.GroupBySolution:
			aggregation.Solution, aggregation.SolutionRev = record.Solution, record.SolutionRev
		case observability.GroupByModel:
			aggregation.ModelName = record.ModelName
		case observability.GroupByOperationType:
			aggregation.OperationType = string(record.OperationType)
		}
	}
	return aggregateKey(aggregation, groups), aggregation
}

func aggregateKey(aggregation observability.CostAggregation, groups []observability.CostGroupBy) string {
	parts := make([]string, 0, len(groups))
	for _, group := range groups {
		switch group {
		case observability.GroupByWorkspace:
			parts = append(parts, aggregation.Workspace)
		case observability.GroupByAgent:
			parts = append(parts, aggregation.AgentIdentity)
		case observability.GroupBySolution:
			parts = append(parts, aggregation.Solution, fmt.Sprintf("%d", aggregation.SolutionRev))
		case observability.GroupByModel:
			parts = append(parts, aggregation.ModelName)
		case observability.GroupByOperationType:
			parts = append(parts, aggregation.OperationType)
		}
	}
	return strings.Join(parts, "\x00")
}

func recordsBelongToWorkspace(records []observability.CostRecord, workspace string) bool {
	if len(records) == 0 {
		return false
	}
	for index := range records {
		if records[index].Workspace != workspace {
			return false
		}
	}
	return true
}

func recordsBefore(records []observability.CostRecord, before time.Time) bool {
	for index := range records {
		if !records[index].Timestamp.Before(before) {
			return false
		}
	}
	return true
}

func recordName(record observability.CostRecord) string {
	if record.TraceID != "" && record.SpanID != "" {
		digest := sha256.Sum256([]byte(record.TraceID + "\x00" + record.SpanID))
		return fmt.Sprintf("%x", digest[:12])
	}
	return uuid.NewString()
}

func usageEventRecordName(eventID string) string {
	digest := sha256.Sum256([]byte(eventID))
	return fmt.Sprintf("%x", digest[:12])
}

func usageEventConfigMap(namespace string, event observability.CostRecord, payload []byte) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: namespace,
			Name:      configMapPrefix + usageEventRecordName(event.ID),
			Labels: map[string]string{
				managedLabel:       "true",
				usageEventLabel:    "true",
				workspaceHashLabel: workspaceHash(event.Workspace),
			},
		},
		BinaryData: map[string][]byte{recordsDataKey: payload},
		Immutable:  boolPtr(true),
	}
}

func usageEventReceiptConfigMap(namespace string, event observability.CostRecord, fingerprint string) *corev1.ConfigMap {
	digest := sha256.Sum256([]byte(event.ID))
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: namespace,
			Name:      receiptPrefix + fmt.Sprintf("%x", digest[:12]),
			Labels:    map[string]string{receiptLabel: "true", workspaceHashLabel: workspaceHash(event.Workspace)},
			Annotations: map[string]string{
				"kognition.dev/workspace": event.Workspace,
			},
		},
		Data:      map[string]string{fingerprintDataKey: fingerprint},
		Immutable: boolPtr(true),
	}
}

func (s *ConfigMapStore) acquireUsageLock(ctx context.Context, workspace string) (context.Context, func(), error) {
	digest := sha256.Sum256([]byte(workspace))
	key := client.ObjectKey{Namespace: s.namespace, Name: "kognition-usage-lock-" + fmt.Sprintf("%x", digest[:12])}
	holder := uuid.NewString()
	deadline := time.Now().Add(usageLockDuration)
	for {
		now := metav1.NewMicroTime(time.Now().UTC())
		durationSeconds := int32(usageLockDuration / time.Second)
		lease := &coordinationv1.Lease{}
		err := s.client.Get(ctx, key, lease)
		if apierrors.IsNotFound(err) {
			lease = &coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{Namespace: key.Namespace, Name: key.Name, Labels: map[string]string{"app.kubernetes.io/managed-by": "kognition"}}, Spec: coordinationv1.LeaseSpec{HolderIdentity: &holder, LeaseDurationSeconds: &durationSeconds, AcquireTime: &now, RenewTime: &now}}
			if err := s.client.Create(ctx, lease); err == nil {
				lockCtx, release := s.maintainUsageLock(ctx, key, holder)
				return lockCtx, release, nil
			} else if !apierrors.IsAlreadyExists(err) {
				return nil, nil, fmt.Errorf("create Kubernetes usage lock: %w", err)
			}
		} else if err != nil {
			return nil, nil, fmt.Errorf("load Kubernetes usage lock: %w", err)
		} else if leaseExpired(lease, now.Time) || lease.Spec.HolderIdentity == nil || *lease.Spec.HolderIdentity == "" {
			lease.Spec.HolderIdentity = &holder
			lease.Spec.LeaseDurationSeconds = &durationSeconds
			lease.Spec.AcquireTime = &now
			lease.Spec.RenewTime = &now
			if err := s.client.Update(ctx, lease); err == nil {
				lockCtx, release := s.maintainUsageLock(ctx, key, holder)
				return lockCtx, release, nil
			} else if !apierrors.IsConflict(err) {
				return nil, nil, fmt.Errorf("acquire Kubernetes usage lock: %w", err)
			}
		}
		if time.Now().After(deadline) {
			return nil, nil, fmt.Errorf("acquire Kubernetes usage lock for workspace %q: timed out", workspace)
		}
		select {
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		case <-time.After(25 * time.Millisecond):
		}
	}
}

func (s *ConfigMapStore) maintainUsageLock(parent context.Context, key client.ObjectKey, holder string) (context.Context, func()) {
	ctx, cancel := context.WithCancel(context.WithValue(parent, usageLockContextKey{}, usageLockIdentity{key: key, holder: holder}))
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(usageLockDuration / 3)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-stop:
				return
			case <-ticker.C:
				if err := s.renewUsageLock(ctx, key, holder); err != nil {
					cancel()
					return
				}
			}
		}
	}()
	var once sync.Once
	release := func() {
		once.Do(func() {
			close(stop)
			<-done
			cancel()
			s.releaseUsageLock(key, holder)
		})
	}
	return ctx, release
}

func (s *ConfigMapStore) verifyUsageLock(ctx context.Context) error {
	identity, ok := ctx.Value(usageLockContextKey{}).(usageLockIdentity)
	if !ok {
		return fmt.Errorf("Kubernetes usage lock identity is missing")
	}
	lease := &coordinationv1.Lease{}
	if err := s.client.Get(ctx, identity.key, lease); err != nil {
		return fmt.Errorf("verify Kubernetes usage lock: %w", err)
	}
	if lease.Spec.HolderIdentity == nil || *lease.Spec.HolderIdentity != identity.holder || leaseExpired(lease, time.Now().UTC()) {
		return fmt.Errorf("Kubernetes usage lock ownership was lost")
	}
	return nil
}

func (s *ConfigMapStore) renewUsageLock(ctx context.Context, key client.ObjectKey, holder string) error {
	lease := &coordinationv1.Lease{}
	if err := s.client.Get(ctx, key, lease); err != nil {
		return err
	}
	if lease.Spec.HolderIdentity == nil || *lease.Spec.HolderIdentity != holder {
		return fmt.Errorf("Kubernetes usage lock ownership was lost")
	}
	now := metav1.NewMicroTime(time.Now().UTC())
	lease.Spec.RenewTime = &now
	return s.client.Update(ctx, lease)
}

func (s *ConfigMapStore) releaseUsageLock(key client.ObjectKey, holder string) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	lease := &coordinationv1.Lease{}
	if err := s.client.Get(ctx, key, lease); err != nil || lease.Spec.HolderIdentity == nil || *lease.Spec.HolderIdentity != holder {
		return
	}
	empty := ""
	lease.Spec.HolderIdentity = &empty
	_ = s.client.Update(ctx, lease)
}

func leaseExpired(lease *coordinationv1.Lease, now time.Time) bool {
	if lease.Spec.RenewTime == nil || lease.Spec.LeaseDurationSeconds == nil {
		return true
	}
	return !lease.Spec.RenewTime.Add(time.Duration(*lease.Spec.LeaseDurationSeconds) * time.Second).After(now)
}

func (s *ConfigMapStore) validateUsageEventReceipt(ctx context.Context, configMap *corev1.ConfigMap, records []observability.CostRecord) error {
	if len(records) != 1 || records[0].ID == "" {
		return fmt.Errorf("Kubernetes usage event %s/%s requires exactly one record with an ID", configMap.Namespace, configMap.Name)
	}
	record := records[0]
	if configMap.Name != configMapPrefix+usageEventRecordName(record.ID) {
		return fmt.Errorf("Kubernetes usage event %s/%s has a non-deterministic name", configMap.Namespace, configMap.Name)
	}
	payload := configMap.BinaryData[recordsDataKey]
	fingerprint := fmt.Sprintf("%x", sha256.Sum256(payload))
	receiptKey := client.ObjectKeyFromObject(usageEventReceiptConfigMap(s.namespace, record, fingerprint))
	var receipt corev1.ConfigMap
	if err := s.client.Get(ctx, receiptKey, &receipt); err != nil {
		return fmt.Errorf("load Kubernetes usage receipt for event %q: %w", record.ID, err)
	}
	if receipt.Data[fingerprintDataKey] != fingerprint || receipt.Annotations["kognition.dev/workspace"] != record.Workspace {
		return fmt.Errorf("Kubernetes usage receipt for event %q does not match the event payload", record.ID)
	}
	return nil
}

func (s *ConfigMapStore) applyEventToQuotaProjections(ctx context.Context, event observability.CostRecord) error {
	var quotas k8s.CognitiveQuotaList
	if err := s.client.List(ctx, &quotas, client.InNamespace(event.Workspace)); err != nil {
		return fmt.Errorf("list CognitiveQuotas for incremental usage projection: %w", err)
	}
	for index := range quotas.Items {
		if quotas.Items[index].Spec.CostBudget == nil {
			continue
		}
		if err := s.applyEventToQuotaProjection(ctx, client.ObjectKeyFromObject(&quotas.Items[index]), event); err != nil {
			return err
		}
	}
	return nil
}

func (s *ConfigMapStore) applyEventToQuotaProjection(ctx context.Context, key client.ObjectKey, event observability.CostRecord) error {
	const maxRetries = 5
	for attempt := 0; attempt < maxRetries; attempt++ {
		var quota k8s.CognitiveQuota
		if err := s.client.Get(ctx, key, &quota); err != nil {
			return fmt.Errorf("load CognitiveQuota for incremental usage projection: %w", err)
		}
		if quota.Spec.CostBudget == nil {
			return nil
		}
		periodStart, err := kubernetesQuotaPeriodStart(&quota)
		if err != nil {
			return fmt.Errorf("resolve CognitiveQuota %s/%s period: %w", key.Namespace, key.Name, err)
		}
		if !periodStart.IsZero() && event.Timestamp.Before(periodStart) {
			return nil
		}
		if quota.Status.CostUsage == nil {
			quota.Status.CostUsage = &k8s.QuotaCostUsage{}
		}
		usage := quota.Status.CostUsage
		if event.CostUnits > observability.CostUnit(math.MaxInt64-usage.TotalCost) {
			return fmt.Errorf("increment CognitiveQuota %s/%s cost: overflow", key.Namespace, key.Name)
		}
		usage.TotalCost += int64(event.CostUnits)
		if event.ModelCostTier != "" && event.ProviderDurationMillis > 0 {
			if usage.ModelTierMilliseconds == nil {
				usage.ModelTierMilliseconds = make(map[string]int64)
			}
			current := usage.ModelTierMilliseconds[event.ModelCostTier]
			if event.ProviderDurationMillis > math.MaxInt64-current {
				return fmt.Errorf("increment CognitiveQuota %s/%s model-tier duration: overflow", key.Namespace, key.Name)
			}
			milliseconds := current + event.ProviderDurationMillis
			usage.ModelTierMilliseconds[event.ModelCostTier] = milliseconds
			if usage.ModelTierSeconds == nil {
				usage.ModelTierSeconds = make(map[string]int64)
			}
			usage.ModelTierSeconds[event.ModelCostTier] = (milliseconds + 999) / 1000
		}
		if err := s.verifyUsageLock(ctx); err != nil {
			return err
		}
		if err := s.client.Status().Update(ctx, &quota); err != nil {
			if apierrors.IsConflict(err) {
				continue
			}
			return fmt.Errorf("update CognitiveQuota %s/%s incremental usage: %w", key.Namespace, key.Name, err)
		}
		return nil
	}
	return fmt.Errorf("update CognitiveQuota %s/%s incremental usage: conflict retries exhausted", key.Namespace, key.Name)
}

func kubernetesQuotaPeriodStart(quota *k8s.CognitiveQuota) (time.Time, error) {
	periodStart := quota.CreationTimestamp.Time
	if quota.Status.LastResetTime == "" {
		return periodStart, nil
	}
	lastReset, err := time.Parse(time.RFC3339Nano, quota.Status.LastResetTime)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid last reset time %q", quota.Status.LastResetTime)
	}
	if lastReset.After(periodStart) {
		periodStart = lastReset
	}
	return periodStart, nil
}

func (s *ConfigMapStore) rebuildQuotaProjections(ctx context.Context, workspace string) error {
	var quotas k8s.CognitiveQuotaList
	if err := s.client.List(ctx, &quotas, client.InNamespace(workspace)); err != nil {
		return fmt.Errorf("list CognitiveQuotas for usage projection: %w", err)
	}
	for index := range quotas.Items {
		if quotas.Items[index].Spec.CostBudget == nil {
			continue
		}
		if err := s.rebuildQuotaProjection(ctx, client.ObjectKeyFromObject(&quotas.Items[index])); err != nil {
			return err
		}
	}
	return nil
}

func (s *ConfigMapStore) rebuildQuotaProjection(ctx context.Context, key client.ObjectKey) error {
	const maxRetries = 5
	for attempt := 0; attempt < maxRetries; attempt++ {
		var quota k8s.CognitiveQuota
		if err := s.client.Get(ctx, key, &quota); err != nil {
			return fmt.Errorf("load CognitiveQuota for usage projection: %w", err)
		}
		if quota.Spec.CostBudget == nil {
			return nil
		}
		records, err := s.workspaceRecords(ctx, key.Namespace)
		if err != nil {
			return err
		}
		periodStart, err := kubernetesQuotaPeriodStart(&quota)
		if err != nil {
			return fmt.Errorf("resolve CognitiveQuota %s/%s period: %w", key.Namespace, key.Name, err)
		}
		usage, err := projectedCostUsage(periodStart, records)
		if err != nil {
			return fmt.Errorf("project CognitiveQuota %s/%s usage: %w", key.Namespace, key.Name, err)
		}
		if quota.Status.CostUsage != nil {
			usage.ToolCalls = quota.Status.CostUsage.ToolCalls
		}
		quota.Status.CostUsage = usage
		if err := s.verifyUsageLock(ctx); err != nil {
			return err
		}
		if err := s.client.Status().Update(ctx, &quota); err != nil {
			if apierrors.IsConflict(err) {
				continue
			}
			return fmt.Errorf("update CognitiveQuota %s/%s usage projection: %w", key.Namespace, key.Name, err)
		}
		return nil
	}
	return fmt.Errorf("update CognitiveQuota %s/%s usage projection: conflict retries exhausted", key.Namespace, key.Name)
}

func (s *ConfigMapStore) workspaceRecords(ctx context.Context, workspace string) ([]observability.CostRecord, error) {
	var records []observability.CostRecord
	continueToken := ""
	seenTokens := make(map[string]struct{})
	for {
		var configMaps corev1.ConfigMapList
		options := []client.ListOption{client.InNamespace(s.namespace), client.MatchingLabels{managedLabel: "true", workspaceHashLabel: workspaceHash(workspace)}, client.Limit(queryPageSize)}
		if continueToken != "" {
			options = append(options, client.Continue(continueToken))
		}
		if err := s.client.List(ctx, &configMaps, options...); err != nil {
			return nil, fmt.Errorf("list Kubernetes usage events: %w", err)
		}
		for index := range configMaps.Items {
			batch, err := decodeRecords(&configMaps.Items[index])
			if err != nil {
				return nil, err
			}
			if configMaps.Items[index].Labels[usageEventLabel] != "true" {
				if err := validateLegacyModelRecords(&configMaps.Items[index], batch); err != nil {
					return nil, err
				}
			}
			for recordIndex := range batch {
				if batch[recordIndex].Workspace != workspace {
					return nil, fmt.Errorf("Kubernetes usage event %s/%s belongs to workspace %q, want %q", configMaps.Items[index].Namespace, configMaps.Items[index].Name, batch[recordIndex].Workspace, workspace)
				}
				if err := validateRecord(&batch[recordIndex]); err != nil {
					return nil, fmt.Errorf("validate Kubernetes usage event %s/%s: %w", configMaps.Items[index].Namespace, configMaps.Items[index].Name, err)
				}
			}
			if configMaps.Items[index].Labels[usageEventLabel] == "true" {
				// Pre-T-0111 immutable records have no receipt label. New usage
				// events must be receipt-bound, while legacy reporting records
				// remain eligible for upgrade-time projection repair.
				if err := s.validateUsageEventReceipt(ctx, &configMaps.Items[index], batch); err != nil {
					return nil, err
				}
			}
			records = append(records, batch...)
		}
		if configMaps.Continue == "" {
			return records, nil
		}
		if _, seen := seenTokens[configMaps.Continue]; seen {
			return nil, fmt.Errorf("Kubernetes usage projection received repeated continuation token for workspace %q", workspace)
		}
		seenTokens[configMaps.Continue] = struct{}{}
		continueToken = configMaps.Continue
	}
}

func validateLegacyModelRecords(configMap *corev1.ConfigMap, records []observability.CostRecord) error {
	modelRecords := 0
	for index := range records {
		if records[index].OperationType != observability.CostOpModelInference {
			continue
		}
		modelRecords++
		if records[index].ID != "" {
			return fmt.Errorf("legacy Kubernetes model cost record %s/%s contains a post-T-0111 event ID", configMap.Namespace, configMap.Name)
		}
	}
	if modelRecords == 0 {
		return nil
	}
	if configMap.Immutable == nil || !*configMap.Immutable {
		return fmt.Errorf("legacy Kubernetes model cost record %s/%s is mutable", configMap.Namespace, configMap.Name)
	}
	if len(records) == 1 && records[0].TraceID != "" && records[0].SpanID != "" {
		if configMap.Name != configMapPrefix+recordName(records[0]) {
			return fmt.Errorf("legacy Kubernetes model cost record %s/%s has a non-deterministic name", configMap.Namespace, configMap.Name)
		}
		return nil
	}
	if _, err := uuid.Parse(strings.TrimPrefix(configMap.Name, configMapPrefix)); err != nil {
		return fmt.Errorf("legacy Kubernetes model cost batch %s/%s has an invalid name", configMap.Namespace, configMap.Name)
	}
	return nil
}

func projectedCostUsage(periodStart time.Time, records []observability.CostRecord) (*k8s.QuotaCostUsage, error) {
	usage := &k8s.QuotaCostUsage{ModelTierSeconds: make(map[string]int64)}
	tierMillis := make(map[string]int64)
	for index := range records {
		record := records[index]
		if record.Workspace == "" || (!periodStart.IsZero() && record.Timestamp.Before(periodStart)) {
			continue
		}
		if record.CostUnits > observability.CostUnit(math.MaxInt64-usage.TotalCost) {
			return nil, fmt.Errorf("cost usage overflow")
		}
		usage.TotalCost += int64(record.CostUnits)
		if record.OperationType != observability.CostOpModelInference || record.ModelCostTier == "" || record.ProviderDurationMillis == 0 {
			continue
		}
		current := tierMillis[record.ModelCostTier]
		if record.ProviderDurationMillis > math.MaxInt64-current {
			return nil, fmt.Errorf("model-tier usage overflow")
		}
		tierMillis[record.ModelCostTier] = current + record.ProviderDurationMillis
	}
	for tier, milliseconds := range tierMillis {
		seconds := milliseconds / 1000
		if milliseconds%1000 != 0 {
			seconds++
		}
		usage.ModelTierSeconds[tier] = seconds
	}
	if len(tierMillis) > 0 {
		usage.ModelTierMilliseconds = tierMillis
	}
	if len(usage.ModelTierSeconds) == 0 {
		usage.ModelTierSeconds = nil
	}
	return usage, nil
}

func workspaceHash(workspace string) string {
	digest := sha256.Sum256([]byte(workspace))
	return fmt.Sprintf("%x", digest[:12])
}

func boolPtr(value bool) *bool { return &value }
