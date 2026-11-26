// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package callhealth shares passive model call health between Kubernetes
// processes through one small ConfigMap per reporting process.
package callhealth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kognition-ai/kognition/internal/store"
)

const (
	// NamePrefix starts the name of every health ConfigMap. The full name is
	// NamePrefix + component + "-" + a hash of the reporter, so admission policy
	// can bind each writer identity to its own component's objects.
	NamePrefix = "kognition-mch-"
	// HealthLabel marks health ConfigMaps. Its value is the reporting component.
	HealthLabel = "kognition.dev/model-call-health"
	// DataKey holds the JSON payload.
	DataKey = "health.json"

	// maxPayloadBytes bounds what a reader will parse. Validated publications are
	// far smaller; the limit protects against objects written by anything else.
	maxPayloadBytes = 900 * 1024
	listPageSize    = 100
	maxListPages    = 20
	updateAttempts  = 4

	// listCacheTTL bounds how often reconciles and the watcher read the API server.
	listCacheTTL = 5 * time.Second
	// retention is how long a reporter's ConfigMap may go without an update
	// before the garbage collector removes it.
	retention = time.Hour
	// garbageCollectionInterval spaces collections of abandoned ConfigMaps.
	garbageCollectionInterval = 5 * time.Minute
)

var (
	componentPattern    = regexp.MustCompile(`^[a-z]+(-[a-z]+)*$`)
	reporterHashPattern = regexp.MustCompile(`^[a-f0-9]{32}$`)
)

// payload is the content of one reporter's ConfigMap.
type payload struct {
	Reporter  string    `json:"reporter"`
	UpdatedAt time.Time `json:"updatedAt"`
	Records   []record  `json:"records"`
}

type record struct {
	Target               string     `json:"target"`
	Calls                int32      `json:"calls"`
	Faults               int32      `json:"faults"`
	ConsecutiveFaults    int32      `json:"consecutiveFaults,omitempty"`
	ConsecutiveSuccesses int32      `json:"consecutiveSuccesses,omitempty"`
	StreakStartedAt      *time.Time `json:"streakStartedAt,omitempty"`
	LastFaultReason      string     `json:"lastFaultReason,omitempty"`
	LastFaultAt          *time.Time `json:"lastFaultAt,omitempty"`
	LastSuccessAt        *time.Time `json:"lastSuccessAt,omitempty"`
}

// ConfigMapStore implements store.ModelCallHealthStore with ConfigMaps in one
// namespace. Each reporter owns exactly one ConfigMap, so replicas never contend,
// and readers merge all of them. Writers must use a client that is allowed to
// create and update ConfigMaps there, and readers one that can list them.
type ConfigMapStore struct {
	client    client.Client
	namespace string
	component string
	logger    *zap.Logger
	now       func() time.Time

	collectGarbage     bool
	collectionInterval time.Duration

	mu       sync.Mutex
	cached   []payload
	cachedAt time.Time
}

var _ store.ModelCallHealthStore = (*ConfigMapStore)(nil)

// Option configures a ConfigMapStore.
type Option func(*ConfigMapStore)

// WithGarbageCollection enables periodic cleanup when the store is registered
// with a manager. Only enable it for an identity that may delete ConfigMaps.
func WithGarbageCollection() Option {
	return func(s *ConfigMapStore) { s.collectGarbage = true }
}

// WithLogger reports best-effort failures, such as garbage collection.
func WithLogger(logger *zap.Logger) Option {
	return func(s *ConfigMapStore) { s.logger = logger }
}

// NewConfigMapStore creates a store whose ConfigMaps live in namespace and are
// named for component, which identifies the writing workload (for example
// "controller" or "model-gateway").
func NewConfigMapStore(kubernetesClient client.Client, namespace, component string, options ...Option) (*ConfigMapStore, error) {
	if kubernetesClient == nil {
		return nil, errors.New("Kubernetes model call health store requires a client")
	}
	if namespace == "" {
		return nil, errors.New("Kubernetes model call health store requires a namespace")
	}
	if len(component) > 32 || !componentPattern.MatchString(component) {
		return nil, fmt.Errorf("Kubernetes model call health store requires a lowercase component name, got %q", component)
	}
	s := &ConfigMapStore{client: kubernetesClient, namespace: namespace, component: component, logger: zap.NewNop(), now: time.Now, collectionInterval: garbageCollectionInterval}
	for _, option := range options {
		option(s)
	}
	return s, nil
}

func (s *ConfigMapStore) objectName(reporter string) string {
	return objectName(s.component, reporter)
}

func objectName(component, reporter string) string {
	sum := sha256.Sum256([]byte(reporter))
	return NamePrefix + component + "-" + hex.EncodeToString(sum[:16])
}

func isHealthConfigMap(configMap *corev1.ConfigMap) bool {
	component := configMap.Labels[HealthLabel]
	prefix := NamePrefix + component + "-"
	_, hasHealth := configMap.Data[DataKey]
	return hasHealth && len(component) <= 32 && componentPattern.MatchString(component) &&
		strings.HasPrefix(configMap.Name, prefix) && reporterHashPattern.MatchString(strings.TrimPrefix(configMap.Name, prefix)) &&
		len(configMap.Labels) == 1 && len(configMap.Data) == 1 && len(configMap.BinaryData) == 0 &&
		len(configMap.OwnerReferences) == 0 && len(configMap.Finalizers) == 0
}

func (s *ConfigMapStore) ReplaceModelCallHealth(ctx context.Context, reporter string, records []store.ModelCallHealth) error {
	if err := store.ValidateModelCallHealth(reporter, records); err != nil {
		return err
	}
	content := payload{Reporter: reporter, UpdatedAt: s.now().UTC(), Records: make([]record, 0, len(records))}
	for _, entry := range records {
		content.Records = append(content.Records, encodeRecord(entry))
	}
	// The payload holds only strings, numbers and times, so encoding cannot fail.
	encoded, _ := json.Marshal(content)

	key := client.ObjectKey{Namespace: s.namespace, Name: s.objectName(reporter)}
	for attempt := 0; attempt < updateAttempts; attempt++ {
		var existing corev1.ConfigMap
		err := s.client.Get(ctx, key, &existing)
		if apierrors.IsNotFound(err) {
			created := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{Namespace: key.Namespace, Name: key.Name, Labels: map[string]string{HealthLabel: s.component}},
				Data:       map[string]string{DataKey: string(encoded)},
			}
			switch err := s.client.Create(ctx, created); {
			case err == nil:
				s.invalidateCache()
				return nil
			case apierrors.IsAlreadyExists(err):
				continue
			default:
				return fmt.Errorf("create model call health: %w", err)
			}
		}
		if err != nil {
			return fmt.Errorf("load model call health: %w", err)
		}
		if existing.Labels[HealthLabel] != s.component || !isHealthConfigMap(&existing) {
			return fmt.Errorf("ConfigMap %s/%s exists but is not model call health owned by %q", key.Namespace, key.Name, s.component)
		}
		existing.Data = map[string]string{DataKey: string(encoded)}
		switch err := s.client.Update(ctx, &existing); {
		case err == nil:
			s.invalidateCache()
			return nil
		case apierrors.IsConflict(err):
			continue
		default:
			return fmt.Errorf("update model call health: %w", err)
		}
	}
	return fmt.Errorf("model call health ConfigMap %s/%s kept changing during %d attempts", key.Namespace, key.Name, updateAttempts)
}

func (s *ConfigMapStore) ListModelCallHealth(ctx context.Context, maxAge time.Duration) ([]store.ModelCallHealth, error) {
	payloads, err := s.payloads(ctx)
	if err != nil {
		return nil, err
	}
	cutoff := s.now().Add(-maxAge)
	var records []store.ModelCallHealth
	for _, published := range payloads {
		if published.UpdatedAt.Before(cutoff) {
			continue
		}
		for _, entry := range published.Records {
			records = append(records, decodeRecord(entry))
		}
	}
	return records, nil
}

// payloads returns every reporter's payload, reading the API server at most once
// per listCacheTTL.
func (s *ConfigMapStore) payloads(ctx context.Context) ([]payload, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cached != nil && s.now().Sub(s.cachedAt) < listCacheTTL {
		return s.cached, nil
	}
	configMaps, err := s.listConfigMaps(ctx)
	if err != nil {
		return nil, err
	}
	payloads := make([]payload, 0, len(configMaps))
	for index := range configMaps {
		if published, ok := parse(&configMaps[index]); ok {
			payloads = append(payloads, published)
		}
	}
	s.cached, s.cachedAt = payloads, s.now()
	return payloads, nil
}

func (s *ConfigMapStore) listConfigMaps(ctx context.Context) ([]corev1.ConfigMap, error) {
	var items []corev1.ConfigMap
	continueToken := ""
	for page := 0; page < maxListPages; page++ {
		configMaps, err := s.listConfigMapPage(ctx, continueToken)
		if err != nil {
			return nil, err
		}
		items = append(items, configMaps.Items...)
		if continueToken = configMaps.Continue; continueToken == "" {
			return items, nil
		}
	}
	return nil, fmt.Errorf("model call health spans more than %d pages", maxListPages)
}

func (s *ConfigMapStore) listConfigMapPage(ctx context.Context, continueToken string) (corev1.ConfigMapList, error) {
	var configMaps corev1.ConfigMapList
	options := []client.ListOption{client.InNamespace(s.namespace), client.HasLabels{HealthLabel}, client.Limit(listPageSize)}
	if continueToken != "" {
		options = append(options, client.Continue(continueToken))
	}
	if err := s.client.List(ctx, &configMaps, options...); err != nil {
		return configMaps, fmt.Errorf("list model call health: %w", err)
	}
	valid := configMaps.Items[:0]
	for index := range configMaps.Items {
		if isHealthConfigMap(&configMaps.Items[index]) {
			valid = append(valid, configMaps.Items[index])
		}
	}
	configMaps.Items = valid
	return configMaps, nil
}

func (s *ConfigMapStore) invalidateCache() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cached = nil
}

// Start collects abandoned reporters independently of this process's traffic.
func (s *ConfigMapStore) Start(ctx context.Context) error {
	if !s.collectGarbage {
		return nil
	}
	s.sweep(ctx)
	ticker := time.NewTicker(s.collectionInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			s.sweep(ctx)
		}
	}
}

// NeedLeaderElection keeps cleanup on the elected controller replica.
func (*ConfigMapStore) NeedLeaderElection() bool { return true }

func (s *ConfigMapStore) sweep(ctx context.Context) {
	sweepCtx, cancel := context.WithTimeout(ctx, garbageCollectionInterval)
	defer cancel()
	if err := s.collect(sweepCtx); err != nil && ctx.Err() == nil {
		s.logger.Warn("collect abandoned model call health", zap.Error(err))
	}
}

func (s *ConfigMapStore) collect(ctx context.Context) error {
	cutoff := s.now().Add(-retention)
	var failures []error
	continueToken := ""
	for {
		if err := ctx.Err(); err != nil {
			return errors.Join(append(failures, err)...)
		}
		configMaps, err := s.listConfigMapPage(ctx, continueToken)
		if err != nil {
			return errors.Join(append(failures, err)...)
		}
		for index := range configMaps.Items {
			configMap := &configMaps.Items[index]
			lastUpdate := configMap.CreationTimestamp.Time
			if published, ok := parse(configMap); ok {
				lastUpdate = published.UpdatedAt
			}
			if !lastUpdate.Before(cutoff) {
				continue
			}
			preconditions := client.Preconditions{UID: &configMap.UID, ResourceVersion: &configMap.ResourceVersion}
			if err := s.client.Delete(ctx, configMap, preconditions); err != nil && !apierrors.IsNotFound(err) && !apierrors.IsConflict(err) {
				failures = append(failures, err)
			}
		}
		if continueToken = configMaps.Continue; continueToken == "" {
			return errors.Join(failures...)
		}
	}
}

// parse decodes a health ConfigMap. Anything malformed or out of bounds is
// skipped rather than failing the read, since a single bad object must not hide
// every other reporter.
func parse(configMap *corev1.ConfigMap) (payload, bool) {
	if !isHealthConfigMap(configMap) {
		return payload{}, false
	}
	raw, ok := configMap.Data[DataKey]
	if !ok || len(raw) > maxPayloadBytes {
		return payload{}, false
	}
	var published payload
	if err := json.Unmarshal([]byte(raw), &published); err != nil || published.Reporter == "" || published.UpdatedAt.IsZero() ||
		len(published.Records) > store.MaxModelCallHealthTargets || configMap.Name != objectName(configMap.Labels[HealthLabel], published.Reporter) {
		return payload{}, false
	}
	valid := published.Records[:0:0]
	seen := make(map[string]struct{}, len(published.Records))
	for _, entry := range published.Records {
		if _, duplicate := seen[entry.Target]; duplicate {
			return payload{}, false
		}
		seen[entry.Target] = struct{}{}
		if store.ValidateModelCallHealthRecord(decodeRecord(entry)) == nil {
			valid = append(valid, entry)
		}
	}
	published.Records = valid
	return published, true
}

func encodeRecord(entry store.ModelCallHealth) record {
	encoded := record{
		Target: entry.Target, Calls: entry.Calls, Faults: entry.Faults,
		ConsecutiveFaults: entry.ConsecutiveFaults, ConsecutiveSuccesses: entry.ConsecutiveSuccesses,
		LastFaultReason: entry.LastFaultReason,
	}
	if !entry.LastFaultAt.IsZero() {
		at := entry.LastFaultAt.UTC()
		encoded.LastFaultAt = &at
	}
	if !entry.LastSuccessAt.IsZero() {
		at := entry.LastSuccessAt.UTC()
		encoded.LastSuccessAt = &at
	}
	if !entry.StreakStartedAt.IsZero() {
		at := entry.StreakStartedAt.UTC()
		encoded.StreakStartedAt = &at
	}
	return encoded
}

func decodeRecord(entry record) store.ModelCallHealth {
	decoded := store.ModelCallHealth{
		Target: entry.Target, Calls: entry.Calls, Faults: entry.Faults,
		ConsecutiveFaults: entry.ConsecutiveFaults, ConsecutiveSuccesses: entry.ConsecutiveSuccesses,
		LastFaultReason: entry.LastFaultReason,
	}
	if entry.LastFaultAt != nil {
		decoded.LastFaultAt = *entry.LastFaultAt
	}
	if entry.LastSuccessAt != nil {
		decoded.LastSuccessAt = *entry.LastSuccessAt
	}
	if entry.StreakStartedAt != nil {
		decoded.StreakStartedAt = *entry.StreakStartedAt
	}
	return decoded
}
