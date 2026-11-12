// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package audit persists CognitiveAuditLog entries through immutable Kubernetes ConfigMaps.
package audit

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlconfig "sigs.k8s.io/controller-runtime/pkg/client/config"

	"github.com/kognition-ai/kognition/internal/observability"
)

const (
	configMapPrefix    = "kognition-audit-"
	entriesDataKey     = "entries.json"
	managedLabel       = "kognition.dev/cognitive-audit"
	workspaceHashLabel = "kognition.dev/audit-workspace"
	agentLabelPrefix   = "kognition.dev/audit-agent-"
	indexMarkerPrefix  = "kognition-audit-index-"
	createAttempts     = 3
	queryPageSize      = 100
	maxQueryPages      = 10
	maxRetentionPages  = 10
	maxPayloadBytes    = 900 * 1024
)

var ErrQueryPageLimit = errors.New("Kubernetes audit query exceeds ConfigMap page limit")

// ConfigMapStore writes immutable audit batches to the supplied namespace.
// A ConfigMap create is atomic, so every AppendBatch is all-or-nothing.
type ConfigMapStore struct {
	client    client.Client
	namespace string
}

var _ observability.AuditStore = (*ConfigMapStore)(nil)
var _ observability.AuditApplyStore = (*ConfigMapStore)(nil)

// NewConfigMapStore constructs an audit store backed by namespace-local ConfigMaps.
func NewConfigMapStore(client client.Client, namespace string) (*ConfigMapStore, error) {
	if client == nil {
		return nil, fmt.Errorf("Kubernetes audit store requires a client")
	}
	if namespace == "" {
		return nil, fmt.Errorf("Kubernetes audit store requires a namespace")
	}
	return &ConfigMapStore{client: client, namespace: namespace}, nil
}

// NewInClusterConfigMapStore constructs a store using the Pod service-account credentials.
func NewInClusterConfigMapStore(namespace string) (*ConfigMapStore, error) {
	config, err := ctrlconfig.GetConfig()
	if err != nil {
		return nil, fmt.Errorf("load in-cluster Kubernetes config: %w", err)
	}
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		return nil, fmt.Errorf("register core Kubernetes scheme: %w", err)
	}
	kubernetesClient, err := client.New(config, client.Options{Scheme: scheme})
	if err != nil {
		return nil, fmt.Errorf("create Kubernetes audit client: %w", err)
	}
	return NewConfigMapStore(kubernetesClient, namespace)
}

// Append writes one immutable audit entry.
func (s *ConfigMapStore) Append(ctx context.Context, entry observability.AuditEntry) error {
	_, err := s.ApplyAuditEntry(ctx, entry)
	return err
}

// AppendBatch atomically persists the supplied entries as one immutable ConfigMap.
func (s *ConfigMapStore) AppendBatch(ctx context.Context, entries []observability.AuditEntry) error {
	_, err := s.ApplyAuditBatch(ctx, entries)
	return err
}

// ApplyAuditEntry writes one immutable entry and reports whether it was new.
func (s *ConfigMapStore) ApplyAuditEntry(ctx context.Context, entry observability.AuditEntry) (bool, error) {
	applied, err := s.ApplyAuditBatch(ctx, []observability.AuditEntry{entry})
	if err != nil || len(applied) == 0 {
		return false, err
	}
	return applied[0], nil
}

// ApplyAuditBatch atomically persists entries and reports which were new.
func (s *ConfigMapStore) ApplyAuditBatch(ctx context.Context, entries []observability.AuditEntry) ([]bool, error) {
	if len(entries) == 0 {
		return []bool{}, nil
	}
	stored := make([]observability.AuditEntry, len(entries))
	copy(stored, entries)
	for index := range stored {
		if stored[index].Workspace == "" || stored[index].EventType == "" {
			return nil, fmt.Errorf("cognitive audit entry %d requires workspace and event type", index)
		}
		if stored[index].Workspace != stored[0].Workspace {
			return nil, fmt.Errorf("cognitive audit batch entries must share workspace %q", stored[0].Workspace)
		}
		if stored[index].ID == "" {
			stored[index].ID = uuid.NewString()
		}
		if stored[index].Timestamp.IsZero() {
			stored[index].Timestamp = time.Now().UTC()
		}
	}
	payload, err := json.Marshal(stored)
	if err != nil {
		return nil, fmt.Errorf("marshal Kubernetes audit entries: %w", err)
	}
	if len(payload) > maxPayloadBytes {
		return nil, fmt.Errorf("Kubernetes audit batch is %d bytes; maximum is %d bytes", len(payload), maxPayloadBytes)
	}
	configMap := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: s.namespace,
			Name:      auditBatchConfigMapName(stored),
			Labels: map[string]string{
				managedLabel:       "true",
				workspaceHashLabel: workspaceHash(stored[0].Workspace),
			},
		},
		BinaryData: map[string][]byte{entriesDataKey: payload},
		Immutable:  boolPtr(true),
	}
	for _, entry := range stored {
		if entry.AgentIdentity != "" {
			configMap.Labels[agentIdentityLabel(entry.AgentIdentity)] = "true"
		}
	}
	if err := s.client.Create(ctx, configMap); apierrors.IsAlreadyExists(err) {
		existing := &corev1.ConfigMap{}
		if getErr := s.client.Get(ctx, client.ObjectKeyFromObject(configMap), existing); getErr != nil {
			return nil, fmt.Errorf("load duplicate Kubernetes audit batch: %w", getErr)
		}
		existingEntries, decodeErr := decodeEntries(existing)
		if decodeErr != nil {
			return nil, decodeErr
		}
		if len(existingEntries) == len(stored) {
			for index := range stored {
				stored[index].Timestamp = existingEntries[index].Timestamp
			}
			payload, err = json.Marshal(stored)
			if err != nil {
				return nil, fmt.Errorf("marshal duplicate Kubernetes audit entries: %w", err)
			}
		}
		if !bytes.Equal(existing.BinaryData[entriesDataKey], payload) {
			return nil, fmt.Errorf("Kubernetes audit entry ID was reused with different content")
		}
		return make([]bool, len(entries)), nil
	} else if err != nil {
		return nil, fmt.Errorf("persist Kubernetes audit batch: %w", err)
	}
	applied := make([]bool, len(entries))
	for index := range applied {
		applied[index] = true
	}
	return applied, nil
}

func auditBatchConfigMapName(entries []observability.AuditEntry) string {
	hash := sha256.New()
	for _, entry := range entries {
		_, _ = hash.Write([]byte(entry.ID))
		_, _ = hash.Write([]byte{0})
	}
	return fmt.Sprintf("%s%x", configMapPrefix, hash.Sum(nil)[:16])
}

// Query returns matching entries in chronological order.
func (s *ConfigMapStore) Query(ctx context.Context, filter observability.AuditFilter) ([]observability.AuditEntry, error) {
	if filter.Limit <= 0 {
		return nil, fmt.Errorf("Kubernetes audit queries require a positive limit")
	}
	listOptions := []client.ListOption{client.InNamespace(s.namespace), client.MatchingLabels{managedLabel: "true"}, client.Limit(queryPageSize)}
	if filter.Workspace != "" {
		listOptions = append(listOptions, client.MatchingLabels{workspaceHashLabel: workspaceHash(filter.Workspace)})
	}
	pageLimit := maxQueryPages
	if filter.AgentIdentity != "" && filter.Workspace != "" {
		if err := s.ensureIdentityIndex(ctx, filter.Workspace); err != nil {
			return nil, err
		}
		listOptions = append(listOptions, client.MatchingLabels{agentIdentityLabel(filter.AgentIdentity): "true"})
		pageLimit = 1000
	}
	entries := make([]observability.AuditEntry, 0)
	continueToken := ""
	reachedEnd := false
	for page := 0; page < pageLimit; page++ {
		configMaps := corev1.ConfigMapList{}
		options := append([]client.ListOption{}, listOptions...)
		if continueToken != "" {
			options = append(options, client.Continue(continueToken))
		}
		if err := s.client.List(ctx, &configMaps, options...); err != nil {
			return nil, fmt.Errorf("list Kubernetes audit entries: %w", err)
		}
		for index := range configMaps.Items {
			batch, err := decodeEntries(&configMaps.Items[index])
			if err != nil {
				return nil, err
			}
			for entryIndex := range batch {
				matches, err := matchesFilter(batch[entryIndex], filter)
				if err != nil {
					return nil, err
				}
				if matches {
					entries = append(entries, batch[entryIndex])
				}
			}
		}
		continueToken = configMaps.Continue
		if continueToken == "" {
			reachedEnd = true
			break
		}
	}
	if !reachedEnd {
		return nil, fmt.Errorf("%w (%d pages); narrow the filter", ErrQueryPageLimit, pageLimit)
	}
	sort.Slice(entries, func(left, right int) bool {
		if entries[left].Timestamp.Equal(entries[right].Timestamp) {
			if filter.Descending {
				return entries[left].ID > entries[right].ID
			}
			return entries[left].ID < entries[right].ID
		}
		if filter.Descending {
			return entries[left].Timestamp.After(entries[right].Timestamp)
		}
		return entries[left].Timestamp.Before(entries[right].Timestamp)
	})
	if filter.Limit > 0 && len(entries) > filter.Limit {
		entries = entries[:filter.Limit]
	}
	return entries, nil
}

func agentIdentityLabel(identity string) string {
	return agentLabelPrefix + workspaceHash(identity)
}

func (s *ConfigMapStore) ensureIdentityIndex(ctx context.Context, workspace string) error {
	marker := &corev1.ConfigMap{}
	key := client.ObjectKey{Namespace: s.namespace, Name: indexMarkerPrefix + workspaceHash(workspace)}
	if err := s.client.Get(ctx, key, marker); err == nil {
		return nil
	} else if !apierrors.IsNotFound(err) {
		return fmt.Errorf("load Kubernetes audit identity index marker: %w", err)
	}
	continueToken := ""
	for {
		var configMaps corev1.ConfigMapList
		options := []client.ListOption{client.InNamespace(s.namespace), client.MatchingLabels{managedLabel: "true", workspaceHashLabel: workspaceHash(workspace)}, client.Limit(queryPageSize)}
		if continueToken != "" {
			options = append(options, client.Continue(continueToken))
		}
		if err := s.client.List(ctx, &configMaps, options...); err != nil {
			return fmt.Errorf("list Kubernetes audit entries for identity indexing: %w", err)
		}
		for index := range configMaps.Items {
			item := &configMaps.Items[index]
			batch, err := decodeEntries(item)
			if err != nil {
				return err
			}
			labels := map[string]string{}
			for _, entry := range batch {
				if entry.AgentIdentity != "" {
					labels[agentIdentityLabel(entry.AgentIdentity)] = "true"
				}
			}
			missing := false
			for label := range labels {
				if item.Labels[label] != "true" {
					missing = true
				}
			}
			if !missing {
				continue
			}
			if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
				current := &corev1.ConfigMap{}
				if err := s.client.Get(ctx, client.ObjectKeyFromObject(item), current); err != nil {
					return err
				}
				if current.Labels == nil {
					current.Labels = map[string]string{}
				}
				for label, value := range labels {
					current.Labels[label] = value
				}
				return s.client.Update(ctx, current)
			}); err != nil {
				return fmt.Errorf("index Kubernetes audit batch %s: %w", item.Name, err)
			}
		}
		if configMaps.Continue == "" {
			break
		}
		continueToken = configMaps.Continue
	}
	marker = &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: key.Namespace, Name: key.Name, Labels: map[string]string{managedLabel: "true"}},
		BinaryData: map[string][]byte{entriesDataKey: []byte("[]")},
		Immutable:  boolPtr(true),
	}
	if err := s.client.Create(ctx, marker); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("persist Kubernetes audit identity index marker: %w", err)
	}
	return nil
}

// DeleteAuditLogsBefore deletes immutable batches whose newest entry is older
// than the cutoff. AppendBatch enforces one workspace per ConfigMap, so a
// workspace retention policy cannot delete another workspace's records.
func (s *ConfigMapStore) DeleteAuditLogsBefore(ctx context.Context, workspace string, before time.Time) (int64, error) {
	if workspace == "" {
		return 0, fmt.Errorf("Kubernetes audit retention requires a workspace")
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
			return deleted, fmt.Errorf("list Kubernetes audit entries for retention: %w", err)
		}
		for index := range configMaps.Items {
			batch, err := decodeEntries(&configMaps.Items[index])
			if err != nil {
				return deleted, err
			}
			if !batchBelongsToWorkspace(batch, workspace) || !batchBefore(batch, before) {
				continue
			}
			if err := s.client.Delete(ctx, &configMaps.Items[index]); err != nil && !apierrors.IsNotFound(err) {
				return deleted, fmt.Errorf("delete Kubernetes audit entries %s/%s: %w", configMaps.Items[index].Namespace, configMaps.Items[index].Name, err)
			}
			deleted += int64(len(batch))
		}
		if configMaps.Continue == "" {
			return deleted, nil
		}
		continueToken = configMaps.Continue
	}
	return deleted, fmt.Errorf("Kubernetes audit retention exceeds %d ConfigMap pages for workspace %q", maxRetentionPages, workspace)
}

func decodeEntries(configMap *corev1.ConfigMap) ([]observability.AuditEntry, error) {
	payload := configMap.BinaryData[entriesDataKey]
	if len(payload) == 0 {
		return nil, fmt.Errorf("decode Kubernetes audit entries %s/%s: missing %s", configMap.Namespace, configMap.Name, entriesDataKey)
	}
	var batch []observability.AuditEntry
	if err := json.Unmarshal(payload, &batch); err != nil {
		return nil, fmt.Errorf("decode Kubernetes audit entries %s/%s: %w", configMap.Namespace, configMap.Name, err)
	}
	return batch, nil
}

func batchBefore(entries []observability.AuditEntry, before time.Time) bool {
	for index := range entries {
		if !entries[index].Timestamp.Before(before) {
			return false
		}
	}
	return true
}

func batchBelongsToWorkspace(entries []observability.AuditEntry, workspace string) bool {
	if len(entries) == 0 {
		return false
	}
	for index := range entries {
		if entries[index].Workspace != workspace {
			return false
		}
	}
	return true
}

func matchesFilter(entry observability.AuditEntry, filter observability.AuditFilter) (bool, error) {
	if filter.Workspace != "" && entry.Workspace != filter.Workspace {
		return false, nil
	}
	if filter.AgentIdentity != "" && entry.AgentIdentity != filter.AgentIdentity {
		return false, nil
	}
	if filter.SessionID != "" && entry.SessionID != filter.SessionID {
		return false, nil
	}
	if filter.EventType != "" && entry.EventType != filter.EventType {
		return false, nil
	}
	if len(filter.EventTypes) > 0 {
		matched := false
		for _, eventType := range filter.EventTypes {
			if entry.EventType == eventType {
				matched = true
				break
			}
		}
		if !matched {
			return false, nil
		}
	}
	if filter.SchemaVersion > 0 || filter.Plane != "" || filter.Outcome != "" {
		var decision struct {
			SchemaVersion int    `json:"schemaVersion"`
			Plane         string `json:"plane"`
			Outcome       string `json:"outcome"`
		}
		if err := json.Unmarshal(entry.Details, &decision); err != nil {
			return false, fmt.Errorf("decode Kubernetes audit entry %q trust decision details: %w", entry.ID, err)
		}
		if filter.SchemaVersion > 0 && decision.SchemaVersion != filter.SchemaVersion {
			return false, nil
		}
		if filter.Plane != "" && decision.Plane != filter.Plane {
			return false, nil
		}
		if filter.Outcome != "" && decision.Outcome != filter.Outcome {
			return false, nil
		}
	}
	if !filter.Since.IsZero() && entry.Timestamp.Before(filter.Since) {
		return false, nil
	}
	return filter.Until.IsZero() || !entry.Timestamp.After(filter.Until), nil
}

func workspaceHash(workspace string) string {
	digest := sha256.Sum256([]byte(workspace))
	return fmt.Sprintf("%x", digest[:12])
}

func boolPtr(value bool) *bool { return &value }
