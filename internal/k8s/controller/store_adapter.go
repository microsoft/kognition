// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package controller provides controller-runtime reconciler adapters that
// bridge the Kubernetes API to the internal Kognition reconciliation framework.
package controller

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	k8s "github.com/kognition-ai/kognition/api/k8s/v1alpha1"
	api "github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/convert"
	"github.com/kognition-ai/kognition/internal/runtimeapproval"
	"github.com/kognition-ai/kognition/internal/store"
)

const (
	instanceEventDetailsAnnotation = "kognition.dev/event-details"
	instanceEventComponent         = "kognition-store"
	revisionParentLabel            = "kognition.dev/revision-parent"
	revisionKindLabel              = "kognition.dev/revision-kind"
)

// StoreAdapter implements store.Store using a controller-runtime client.Client.
// It marshals/unmarshals K8s resources to []byte using the internal types via
// the convert layer, allowing existing reconcilers to operate unchanged.
type StoreAdapter struct {
	client                        client.Client
	runtimeApprovalReader         client.Reader
	workflowExecutorReader        client.Reader
	workflowExecutorAuthorization *WorkflowExecutorKubernetesAuthorization
	workflowExecutorRecords       *WorkflowExecutorRecordStore
	controlNamespace              string
	toolInvocationReceiptWriter   string
	now                           func() time.Time
	maximumClockSkew              time.Duration
	runtimeApprovalQuota          RuntimeApprovalQuotaLimits
	runtimeApprovalClockHealth    func() error
	runtimeApprovalBindingMu      sync.RWMutex
	runtimeApprovalActivationMu   sync.Mutex
	runtimeApprovalBindingKeys    runtimeapproval.BindingKeyStore
}

var _ runtimeapproval.TrustProfileIndexValidator = (*StoreAdapter)(nil)

// ConfigureRuntimeApprovalBindingKeys configures the trusted key boundary used
// by dedicated RuntimeApproval operations. Calls fail closed until configured.
func (s *StoreAdapter) ConfigureRuntimeApprovalBindingKeys(keys runtimeapproval.BindingKeyStore) {
	s.runtimeApprovalBindingMu.Lock()
	defer s.runtimeApprovalBindingMu.Unlock()
	s.runtimeApprovalBindingKeys = keys
}

func (s *StoreAdapter) runtimeApprovalKeys() runtimeapproval.BindingKeyStore {
	s.runtimeApprovalBindingMu.RLock()
	defer s.runtimeApprovalBindingMu.RUnlock()
	return s.runtimeApprovalBindingKeys
}

// ConfigureRuntimeApprovalReader configures the uncached reader used by
// RuntimeApproval Lease coordination, which requires read-after-write consistency.
func (s *StoreAdapter) ConfigureRuntimeApprovalReader(reader client.Reader) {
	if reader != nil {
		s.runtimeApprovalReader = reader
	}
}

// ConfigureRuntimeApprovalClock configures the trusted host-clock boundary.
func (s *StoreAdapter) ConfigureRuntimeApprovalClock(maximumClockSkew time.Duration, health func() error) error {
	if maximumClockSkew < defaultRuntimeApprovalClockSkew {
		return fmt.Errorf("RuntimeApproval maximum clock skew must be at least %s", defaultRuntimeApprovalClockSkew)
	}
	if health == nil {
		return fmt.Errorf("RuntimeApproval clock health check is required")
	}
	s.maximumClockSkew = maximumClockSkew
	s.runtimeApprovalClockHealth = health
	return nil
}

// RuntimeApprovalClockReady verifies that the configured host clock is trusted.
func (s *StoreAdapter) RuntimeApprovalClockReady() error {
	if s.runtimeApprovalClockHealth == nil {
		return fmt.Errorf("RuntimeApproval host clock health is not configured")
	}
	if err := s.runtimeApprovalClockHealth(); err != nil {
		return fmt.Errorf("RuntimeApproval host clock is unhealthy: %w", err)
	}
	return nil
}

// NewStoreAdapter creates a new StoreAdapter wrapping the given client.
func NewStoreAdapter(c client.Client) *StoreAdapter {
	return &StoreAdapter{client: c, runtimeApprovalReader: c, now: time.Now, maximumClockSkew: defaultRuntimeApprovalClockSkew, runtimeApprovalQuota: DefaultRuntimeApprovalQuotaLimits()}
}

// NewStoreAdapterWithControlNamespace stores internal coordination objects in
// the controller namespace rather than tenant application namespaces.
func NewStoreAdapterWithControlNamespace(c client.Client, controlNamespace string) *StoreAdapter {
	return &StoreAdapter{client: c, runtimeApprovalReader: c, controlNamespace: controlNamespace, now: time.Now, maximumClockSkew: defaultRuntimeApprovalClockSkew, runtimeApprovalQuota: DefaultRuntimeApprovalQuotaLimits()}
}

// Ping verifies connectivity to the API server.
func (s *StoreAdapter) Ping(ctx context.Context) error {
	list := &k8s.AgentList{}
	return s.client.List(ctx, list, client.InNamespace("kube-system"), client.Limit(1))
}

func (s *StoreAdapter) ValidateTrustProfileIndex(ctx context.Context, namespace string, agent api.Agent, profile *api.TrustProfile) error {
	index := &k8s.TrustProfileIndex{}
	key := client.ObjectKey{Namespace: namespace, Name: TrustProfileIndexName(types.UID(agent.UID))}
	if err := s.client.Get(ctx, key, index); err != nil {
		if apierrors.IsNotFound(err) {
			if profile == nil {
				return nil
			}
			return fmt.Errorf("TrustProfileIndex is missing for Agent %s", agent.Name)
		}
		return fmt.Errorf("get TrustProfileIndex: %w", err)
	}
	if profile == nil {
		return fmt.Errorf("TrustProfileIndex exists without a live TrustProfile")
	}
	if index.Spec.AgentUID != agent.UID {
		return fmt.Errorf("TrustProfileIndex Agent UID %q does not match %q", index.Spec.AgentUID, agent.UID)
	}
	if index.Spec.SelectedProfileName != profile.Name || index.Spec.SelectedProfileUID != profile.UID {
		return fmt.Errorf("TrustProfileIndex selected TrustProfile does not match %s@%s", profile.Name, profile.UID)
	}
	owner := metav1.GetControllerOf(index)
	if owner == nil || owner.APIVersion != k8s.GroupVersion.String() || owner.Kind != "Agent" || owner.Name != agent.Name || string(owner.UID) != agent.UID {
		return fmt.Errorf("TrustProfileIndex controller owner does not match Agent %s@%s", agent.Name, agent.UID)
	}
	return nil
}

// Get retrieves a resource by kind/namespace/name and returns its JSON representation.
func (s *StoreAdapter) Get(ctx context.Context, kind, namespace, name string) ([]byte, error) {
	obj, err := s.newObject(kind)
	if err != nil {
		return nil, err
	}
	if isClusterScopedKind(kind) {
		namespace = ""
	}
	key := client.ObjectKey{Namespace: namespace, Name: name}
	if err := s.client.Get(ctx, key, obj); err != nil {
		if client.IgnoreNotFound(err) == nil {
			return nil, store.ErrNotFound
		}
		return nil, fmt.Errorf("k8s get %s %s/%s: %w", kind, namespace, name, err)
	}
	return s.marshalToInternal(kind, obj)
}

// List returns all resources of a given kind matching the options.
// Pagination is handled via offset-based slicing on the full result set.
func (s *StoreAdapter) List(ctx context.Context, kind string, opts store.ListOptions) ([][]byte, error) {
	listObj, err := s.newListObject(kind)
	if err != nil {
		return nil, err
	}

	listOpts := []client.ListOption{}
	if opts.Namespace != "" {
		listOpts = append(listOpts, client.InNamespace(opts.Namespace))
	}
	if len(opts.LabelSelector) > 0 {
		listOpts = append(listOpts, client.MatchingLabels(opts.LabelSelector))
	}

	if err := s.client.List(ctx, listObj, listOpts...); err != nil {
		return nil, fmt.Errorf("k8s list %s: %w", kind, err)
	}

	results, err := s.marshalListToInternal(kind, listObj)
	if err != nil {
		return nil, err
	}

	// Apply offset-based pagination if requested.
	if opts.Limit > 0 {
		offset := 0
		if opts.Continue != "" {
			offset, err = decodeContinueOffset(opts.Continue)
			if err != nil {
				return nil, fmt.Errorf("invalid continue token: %w", err)
			}
			if offset < 0 {
				return nil, fmt.Errorf("invalid continue token: offset must be non-negative")
			}
		}
		if offset >= len(results) {
			return nil, nil
		}
		count := len(results) - offset
		if opts.Limit < count {
			count = opts.Limit + 1 // fetch one extra for has-more detection
		}
		results = results[offset : offset+count]
	}

	return results, nil
}

// Create creates a new resource.
func (s *StoreAdapter) Create(ctx context.Context, kind, namespace, name string, data []byte) error {
	obj, err := s.unmarshalFromInternal(kind, data)
	if err != nil {
		return err
	}
	if isClusterScopedKind(kind) {
		namespace = ""
	}
	obj.SetNamespace(namespace)
	obj.SetName(name)
	// Kubernetes assigns resourceVersion during creation; internal resources
	// may carry an in-memory version for standalone stores.
	obj.SetResourceVersion("")
	if err := s.client.Create(ctx, obj); err != nil {
		if isAlreadyExists(err) {
			return store.ErrAlreadyExists
		}
		return fmt.Errorf("k8s create %s %s/%s: %w", kind, namespace, name, err)
	}
	return nil
}

// Update updates an existing resource. resourceVersion must match.
// Because K8s CRDs with a status subresource require separate calls to
// update spec/metadata vs status, this method performs both operations.
// To avoid spurious ResourceVersion bumps that trigger re-reconciliation,
// the spec/metadata update is skipped when only status has changed.
func (s *StoreAdapter) Update(ctx context.Context, kind, namespace, name string, data []byte, resourceVersion int64) error {
	_, err := s.UpdateAndGet(ctx, kind, namespace, name, data, resourceVersion)
	return err
}

// UpdateAndGet updates an existing resource and returns the exact object
// produced by the Kubernetes API writes.
func (s *StoreAdapter) UpdateAndGet(ctx context.Context, kind, namespace, name string, data []byte, resourceVersion int64) ([]byte, error) {
	obj, err := s.unmarshalFromInternal(kind, data)
	if err != nil {
		return nil, err
	}
	if isClusterScopedKind(kind) {
		namespace = ""
	}
	obj.SetNamespace(namespace)
	obj.SetName(name)
	obj.SetResourceVersion(strconv.FormatInt(resourceVersion, 10))

	// Read current object to determine if spec/metadata actually changed.
	current, err := s.newObject(kind)
	if err != nil {
		return nil, err
	}
	if err := s.client.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, current); err != nil {
		if client.IgnoreNotFound(err) == nil {
			return nil, store.ErrNotFound
		}
		return nil, fmt.Errorf("k8s get for update %s %s/%s: %w", kind, namespace, name, err)
	}

	rv := current.GetResourceVersion()

	// Verify the caller's resource version matches current (optimistic concurrency).
	if strconv.FormatInt(resourceVersion, 10) != rv {
		return nil, store.ErrConflict
	}

	statusNeedsUpdate := kindHasStatusSubresource(kind) && statusChanged(current, obj)
	// Compare against the live object in the internal representation, so a
	// value the API server stores in another spelling (a duration "5m" that
	// round-trips as "5m0s") does not turn a status write into a spec write.
	nonStatusChanged := metadataChanged(s.normalizedThroughInternal(kind, current), obj)

	// Only perform the spec/metadata update if something changed.
	if nonStatusChanged {
		obj.SetResourceVersion(rv)
		if err := s.client.Update(ctx, obj); err != nil {
			if isConflict(err) {
				return nil, store.ErrConflict
			}
			return nil, fmt.Errorf("k8s update %s %s/%s: %w", kind, namespace, name, err)
		}
		rv = obj.GetResourceVersion()
		if current.GetDeletionTimestamp() != nil && len(obj.GetFinalizers()) == 0 {
			return s.marshalToInternal(kind, obj)
		}
	}

	if !kindHasStatusSubresource(kind) {
		if nonStatusChanged {
			return s.marshalToInternal(kind, obj)
		}
		return s.marshalToInternal(kind, current)
	}
	if !statusNeedsUpdate {
		if nonStatusChanged {
			return s.marshalToInternal(kind, obj)
		}
		return s.marshalToInternal(kind, current)
	}

	// Status-bearing CRDs require a separate status update.
	statusObj, err := s.unmarshalFromInternal(kind, data)
	if err != nil {
		return nil, err
	}
	statusObj.SetNamespace(namespace)
	statusObj.SetName(name)
	statusObj.SetResourceVersion(rv)

	if err := s.client.Status().Update(ctx, statusObj); err != nil {
		if isConflict(err) {
			return nil, store.ErrConflict
		}
		return nil, fmt.Errorf("k8s status update %s %s/%s: %w", kind, namespace, name, err)
	}
	return s.marshalToInternal(kind, statusObj)
}

// Delete removes a resource.
func (s *StoreAdapter) Delete(ctx context.Context, kind, namespace, name string) error {
	obj, err := s.newObject(kind)
	if err != nil {
		return err
	}
	if isClusterScopedKind(kind) {
		namespace = ""
	}
	obj.SetNamespace(namespace)
	obj.SetName(name)
	if err := s.client.Delete(ctx, obj); err != nil {
		if client.IgnoreNotFound(err) == nil {
			return store.ErrNotFound
		}
		return fmt.Errorf("k8s delete %s %s/%s: %w", kind, namespace, name, err)
	}
	return nil
}

// Watch is not implemented for the K8s adapter; controller-runtime watches are
// set up via the controller builder instead.
func (s *StoreAdapter) Watch(_ context.Context, _ string, _ store.ListOptions) (<-chan store.WatchEvent, error) {
	return nil, fmt.Errorf("watch not supported on K8s store adapter (use controller-runtime watches)")
}

// CreateRevision stores an immutable snapshot in a namespaced ConfigMap owned
// by the parent Kognition resource.
func (s *StoreAdapter) CreateRevision(ctx context.Context, kind, namespace, name string, revision int64, spec, data []byte) error {
	if namespace == "" {
		return fmt.Errorf("create revision for %s %q: namespace is required", kind, name)
	}
	obj, err := s.newObject(kind)
	if err != nil {
		return err
	}
	key := client.ObjectKey{Namespace: namespace, Name: name}
	if err := s.client.Get(ctx, key, obj); err != nil {
		return fmt.Errorf("get %s %s/%s for revision: %w", kind, namespace, name, err)
	}
	createdAt := time.Now().UTC()
	revisionObject := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      revisionConfigMapName(kind, name, revision),
			Namespace: namespace,
			Labels: map[string]string{
				revisionParentLabel: revisionParentKey(kind, name),
				revisionKindLabel:   strings.ToLower(kind),
			},
			Annotations: map[string]string{
				"kognition.dev/revision":   strconv.FormatInt(revision, 10),
				"kognition.dev/created-at": createdAt.Format(time.RFC3339),
			},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: api.APIVersion,
				Kind:       kind,
				Name:       name,
				UID:        obj.GetUID(),
			}},
		},
		BinaryData: map[string][]byte{"spec": spec, "data": data},
	}
	immutable := true
	revisionObject.Immutable = &immutable
	if err := s.client.Create(ctx, revisionObject); err != nil {
		if isAlreadyExists(err) {
			return store.ErrAlreadyExists
		}
		return fmt.Errorf("create %s %s/%s revision %d: %w", kind, namespace, name, revision, err)
	}
	return nil
}

// ListRevisions reads immutable ConfigMap snapshots, with latest-annotation
// fallback for resources created before revision ConfigMaps were introduced.
func (s *StoreAdapter) ListRevisions(ctx context.Context, kind, namespace, name string) ([]store.Revision, error) {
	obj, err := s.newObject(kind)
	if err != nil {
		return nil, err
	}
	key := client.ObjectKey{Namespace: namespace, Name: name}
	if err := s.client.Get(ctx, key, obj); err != nil {
		if client.IgnoreNotFound(err) == nil {
			return nil, nil
		}
		return nil, fmt.Errorf("get %s %s/%s for revisions: %w", kind, namespace, name, err)
	}
	var revisionObjects corev1.ConfigMapList
	if err := s.client.List(ctx, &revisionObjects,
		client.InNamespace(namespace),
		client.MatchingLabels{
			revisionParentLabel: revisionParentKey(kind, name),
			revisionKindLabel:   strings.ToLower(kind),
		},
	); err != nil {
		return nil, fmt.Errorf("list %s %s/%s revisions: %w", kind, namespace, name, err)
	}
	revisions := make([]store.Revision, 0, len(revisionObjects.Items))
	for _, item := range revisionObjects.Items {
		revision, ok := revisionFromConfigMap(&item, kind, name, obj.GetUID())
		if ok {
			revisions = append(revisions, revision)
		}
	}
	annotations := obj.GetAnnotations()
	if legacy, ok := legacyRevisionFromAnnotations(annotations); ok && !containsRevision(revisions, legacy.Revision) {
		revisions = append(revisions, legacy)
	}
	if len(revisions) == 0 {
		return nil, nil
	}
	sort.Slice(revisions, func(left, right int) bool { return revisions[left].Revision < revisions[right].Revision })
	return revisions, nil
}

// GetRevision returns an exact immutable revision snapshot.
func (s *StoreAdapter) GetRevision(ctx context.Context, kind, namespace, name string, revision int64) (*store.Revision, error) {
	obj, err := s.newObject(kind)
	if err != nil {
		return nil, err
	}
	if err := s.client.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, obj); err != nil {
		if client.IgnoreNotFound(err) == nil {
			return nil, store.ErrNotFound
		}
		return nil, fmt.Errorf("get %s %s/%s for revision: %w", kind, namespace, name, err)
	}
	var revisionObject corev1.ConfigMap
	key := client.ObjectKey{Namespace: namespace, Name: revisionConfigMapName(kind, name, revision)}
	if err := s.client.Get(ctx, key, &revisionObject); err == nil {
		parsed, ok := revisionFromConfigMap(&revisionObject, kind, name, obj.GetUID())
		if !ok || parsed.Revision != revision {
			return nil, store.ErrNotFound
		}
		return &parsed, nil
	} else if !apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("get %s %s/%s revision %d: %w", kind, namespace, name, revision, err)
	}
	revisions, err := s.ListRevisions(ctx, kind, namespace, name)
	if err != nil {
		return nil, err
	}
	for index := range revisions {
		if revisions[index].Revision == revision {
			return &revisions[index], nil
		}
	}
	return nil, store.ErrNotFound
}

func revisionFromConfigMap(configMap *corev1.ConfigMap, kind, name string, ownerUID types.UID) (store.Revision, bool) {
	if configMap.Immutable == nil || !*configMap.Immutable ||
		configMap.Labels[revisionParentLabel] != revisionParentKey(kind, name) ||
		configMap.Labels[revisionKindLabel] != strings.ToLower(kind) ||
		!revisionConfigMapOwnedBy(configMap, kind, name, ownerUID) {
		return store.Revision{}, false
	}
	revision, err := strconv.ParseInt(configMap.Annotations["kognition.dev/revision"], 10, 64)
	if err != nil || revision < 1 {
		return store.Revision{}, false
	}
	createdAt := configMap.Annotations["kognition.dev/created-at"]
	if createdAt == "" && !configMap.CreationTimestamp.IsZero() {
		createdAt = configMap.CreationTimestamp.UTC().Format(time.RFC3339)
	}
	return store.Revision{
		Revision:  revision,
		Spec:      append([]byte(nil), configMap.BinaryData["spec"]...),
		Data:      append([]byte(nil), configMap.BinaryData["data"]...),
		CreatedAt: createdAt,
	}, true
}

func revisionConfigMapOwnedBy(configMap *corev1.ConfigMap, kind, name string, ownerUID types.UID) bool {
	for _, owner := range configMap.OwnerReferences {
		if owner.APIVersion == api.APIVersion && owner.Kind == kind && owner.Name == name && owner.UID == ownerUID {
			return true
		}
	}
	return false
}

func legacyRevisionFromAnnotations(annotations map[string]string) (store.Revision, bool) {
	revision, err := strconv.ParseInt(annotations["kognition.dev/revision-count"], 10, 64)
	if err != nil || revision < 1 {
		return store.Revision{}, false
	}
	specEncoded, ok := annotations["kognition.dev/revision-spec"]
	if !ok {
		return store.Revision{}, false
	}
	spec, err := base64.StdEncoding.DecodeString(specEncoded)
	if err != nil {
		return store.Revision{}, false
	}
	var data []byte
	if dataEncoded, ok := annotations["kognition.dev/revision-data"]; ok {
		data, err = base64.StdEncoding.DecodeString(dataEncoded)
		if err != nil {
			return store.Revision{}, false
		}
	}
	return store.Revision{Revision: revision, Spec: spec, Data: data}, true
}

func containsRevision(revisions []store.Revision, revision int64) bool {
	for _, candidate := range revisions {
		if candidate.Revision == revision {
			return true
		}
	}
	return false
}

func revisionParentKey(kind, name string) string {
	digest := sha256.Sum256([]byte(kind + "\x00" + name))
	return fmt.Sprintf("%x", digest[:12])
}

func revisionConfigMapName(kind, name string, revision int64) string {
	prefix := strings.ToLower(kind + "-" + name)
	prefix = strings.Trim(strings.ReplaceAll(prefix, "_", "-"), "-.")
	if len(prefix) > 40 {
		prefix = prefix[:40]
	}
	return fmt.Sprintf("kognition-revision-%s-%s-%d", prefix, revisionParentKey(kind, name)[:12], revision)
}

// AppendEvent persists an execution event through the standard Kubernetes Events API.
func (s *StoreAdapter) AppendEvent(ctx context.Context, kind, namespace, name string, event store.InstanceEvent) error {
	if namespace == "" {
		return fmt.Errorf("append event for %s %q: namespace is required", kind, name)
	}
	obj, err := s.newObject(kind)
	if err != nil {
		return err
	}
	if err := s.client.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, obj); err != nil {
		if client.IgnoreNotFound(err) == nil {
			return store.ErrNotFound
		}
		return fmt.Errorf("get %s %s/%s for event: %w", kind, namespace, name, err)
	}
	timestamp := time.Now().UTC()
	if parsed, err := time.Parse(time.RFC3339, event.Timestamp); err == nil {
		timestamp = parsed
	}
	annotations := map[string]string{}
	if len(event.Details) > 0 {
		annotations[instanceEventDetailsAnnotation] = string(event.Details)
	}
	k8sEvent := &corev1.Event{
		ObjectMeta: metav1.ObjectMeta{
			Name:        instanceEventName(kind, name),
			Namespace:   namespace,
			Annotations: annotations,
		},
		InvolvedObject: corev1.ObjectReference{
			APIVersion: api.APIVersion,
			Kind:       kind,
			Namespace:  namespace,
			Name:       name,
			UID:        obj.GetUID(),
		},
		Reason:         event.Reason,
		Message:        event.Message,
		Type:           event.Type,
		FirstTimestamp: metav1.NewTime(timestamp),
		LastTimestamp:  metav1.NewTime(timestamp),
		Count:          1,
		Source:         corev1.EventSource{Component: instanceEventComponent},
	}
	if err := s.client.Create(ctx, k8sEvent); err != nil {
		return fmt.Errorf("create Kubernetes event for %s %s/%s: %w", kind, namespace, name, err)
	}
	return nil
}

// ListEvents returns Kubernetes Events involving the requested Kognition resource.
func (s *StoreAdapter) ListEvents(ctx context.Context, kind, namespace, name string) ([]store.InstanceEvent, error) {
	var list corev1.EventList
	if err := s.client.List(ctx, &list, client.InNamespace(namespace)); err != nil {
		return nil, fmt.Errorf("list Kubernetes events for %s %s/%s: %w", kind, namespace, name, err)
	}
	events := make([]store.InstanceEvent, 0)
	for _, item := range list.Items {
		if item.InvolvedObject.Kind != kind || item.InvolvedObject.Namespace != namespace || item.InvolvedObject.Name != name ||
			!strings.HasPrefix(item.InvolvedObject.APIVersion, "kognition.dev/") {
			continue
		}
		timestamp := item.LastTimestamp.Time
		if timestamp.IsZero() {
			timestamp = item.FirstTimestamp.Time
		}
		if timestamp.IsZero() && !item.EventTime.IsZero() {
			timestamp = item.EventTime.Time
		}
		if timestamp.IsZero() {
			timestamp = item.CreationTimestamp.Time
		}
		var details json.RawMessage
		if raw := item.Annotations[instanceEventDetailsAnnotation]; json.Valid([]byte(raw)) {
			details = json.RawMessage(raw)
		}
		events = append(events, store.InstanceEvent{
			Timestamp: timestamp.UTC().Format(time.RFC3339),
			Type:      item.Type,
			Reason:    item.Reason,
			Message:   item.Message,
			Details:   details,
		})
	}
	sort.SliceStable(events, func(left, right int) bool { return events[left].Timestamp < events[right].Timestamp })
	if len(events) == 0 {
		return nil, nil
	}
	return events, nil
}

func instanceEventName(kind, name string) string {
	prefix := strings.ToLower(kind + "-" + name)
	prefix = strings.Trim(strings.ReplaceAll(prefix, "_", "-"), "-.")
	if len(prefix) > 210 {
		prefix = prefix[:210]
	}
	return prefix + "-" + uuid.NewString()
}

// normalizedThroughInternal returns obj as it reads back from the internal
// representation. It returns obj unchanged when the conversion is unavailable.
func (s *StoreAdapter) normalizedThroughInternal(kind string, obj client.Object) client.Object {
	data, err := s.marshalToInternal(kind, obj)
	if err != nil {
		return obj
	}
	normalized, err := s.unmarshalFromInternal(kind, data)
	if err != nil {
		return obj
	}
	return normalized
}

// marshalToInternal converts a K8s runtime object to internal JSON via the convert layer.
func (s *StoreAdapter) marshalToInternal(kind string, obj client.Object) ([]byte, error) {
	switch kind {
	case "Agent":
		resource, ok := obj.(*k8s.Agent)
		if !ok || resource == nil {
			return nil, fmt.Errorf("expected *k8s.Agent, got %T", obj)
		}
		return json.Marshal(convert.AgentFromK8s(resource))
	case "AgentInstance":
		resource, ok := obj.(*k8s.AgentInstance)
		if !ok || resource == nil {
			return nil, fmt.Errorf("expected *k8s.AgentInstance, got %T", obj)
		}
		return json.Marshal(convert.AgentInstanceFromK8s(resource))
	case "AgentJob":
		resource, ok := obj.(*k8s.AgentJob)
		if !ok || resource == nil {
			return nil, fmt.Errorf("expected *k8s.AgentJob, got %T", obj)
		}
		return json.Marshal(convert.AgentJobFromK8s(resource))
	case "Workflow":
		resource, ok := obj.(*k8s.Workflow)
		if !ok || resource == nil {
			return nil, fmt.Errorf("expected *k8s.Workflow, got %T", obj)
		}
		return json.Marshal(convert.WorkflowFromK8s(resource))
	case "Tool":
		resource, ok := obj.(*k8s.Tool)
		if !ok || resource == nil {
			return nil, fmt.Errorf("expected *k8s.Tool, got %T", obj)
		}
		return json.Marshal(convert.ToolFromK8s(resource))
	case "ToolGrant":
		return marshalConverted(obj, "*k8s.ToolGrant", convert.ToolGrantFromK8s)
	case "ToolInvocationRun":
		resource, ok := obj.(*k8s.ToolInvocationRun)
		if !ok || resource == nil {
			return nil, fmt.Errorf("expected *k8s.ToolInvocationRun, got %T", obj)
		}
		return json.Marshal(convert.ToolInvocationRunFromK8s(resource))
	case "ToolClass":
		resource, ok := obj.(*k8s.ToolClass)
		if !ok || resource == nil {
			return nil, fmt.Errorf("expected *k8s.ToolClass, got %T", obj)
		}
		return json.Marshal(convert.ToolClassFromK8s(resource))
	case "Model":
		resource, ok := obj.(*k8s.Model)
		if !ok || resource == nil {
			return nil, fmt.Errorf("expected *k8s.Model, got %T", obj)
		}
		return json.Marshal(convert.ModelFromK8s(resource))
	case "ModelClass":
		resource, ok := obj.(*k8s.ModelClass)
		if !ok || resource == nil {
			return nil, fmt.Errorf("expected *k8s.ModelClass, got %T", obj)
		}
		return json.Marshal(convert.ModelClassFromK8s(resource))
	case "ModelInvocationProfile":
		return marshalConverted(obj, "*k8s.ModelInvocationProfile", convert.ModelInvocationProfileFromK8s)
	case "ExternalAgent":
		return marshalConverted(obj, "*k8s.ExternalAgent", convert.ExternalAgentFromK8s)
	case "ExternalAgentGrant":
		return marshalConverted(obj, "*k8s.ExternalAgentGrant", convert.ExternalAgentGrantFromK8s)
	case "ModelGrant":
		resource, ok := obj.(*k8s.ModelGrant)
		if !ok || resource == nil {
			return nil, fmt.Errorf("expected *k8s.ModelGrant, got %T", obj)
		}
		return json.Marshal(convert.ModelGrantFromK8s(resource))
	case "AutonomyClass":
		resource, ok := obj.(*k8s.AutonomyClass)
		if !ok || resource == nil {
			return nil, fmt.Errorf("expected *k8s.AutonomyClass, got %T", obj)
		}
		return json.Marshal(convert.AutonomyClassFromK8s(resource))
	case "AutonomyReview":
		resource, ok := obj.(*k8s.AutonomyReview)
		if !ok || resource == nil {
			return nil, fmt.Errorf("expected *k8s.AutonomyReview, got %T", obj)
		}
		return json.Marshal(convert.AutonomyReviewFromK8s(resource))
	case "TrustProfile":
		resource, ok := obj.(*k8s.TrustProfile)
		if !ok || resource == nil {
			return nil, fmt.Errorf("expected *k8s.TrustProfile, got %T", obj)
		}
		return json.Marshal(convert.TrustProfileFromK8s(resource))
	case "RuntimeApproval":
		resource, ok := obj.(*k8s.RuntimeApproval)
		if !ok || resource == nil {
			return nil, fmt.Errorf("expected *k8s.RuntimeApproval, got %T", obj)
		}
		return json.Marshal(convert.RuntimeApprovalFromK8s(resource))
	case "AgentService":
		resource, ok := obj.(*k8s.AgentService)
		if !ok || resource == nil {
			return nil, fmt.Errorf("expected *k8s.AgentService, got %T", obj)
		}
		return json.Marshal(convert.AgentServiceFromK8s(resource))
	case "IntentGateway":
		resource, ok := obj.(*k8s.IntentGateway)
		if !ok || resource == nil {
			return nil, fmt.Errorf("expected *k8s.IntentGateway, got %T", obj)
		}
		return json.Marshal(convert.IntentGatewayFromK8s(resource))
	case "Prompt":
		resource, ok := obj.(*k8s.Prompt)
		if !ok || resource == nil {
			return nil, fmt.Errorf("expected *k8s.Prompt, got %T", obj)
		}
		return json.Marshal(convert.PromptFromK8s(resource))
	case "Solution":
		resource, ok := obj.(*k8s.Solution)
		if !ok || resource == nil {
			return nil, fmt.Errorf("expected *k8s.Solution, got %T", obj)
		}
		return json.Marshal(convert.SolutionFromK8s(resource))
	case "CrossNamespaceGrant":
		resource, ok := obj.(*k8s.CrossNamespaceGrant)
		if !ok || resource == nil {
			return nil, fmt.Errorf("expected *k8s.CrossNamespaceGrant, got %T", obj)
		}
		return json.Marshal(convert.CrossNamespaceGrantFromK8s(resource))
	case "ModelAutoscaler":
		resource, ok := obj.(*k8s.ModelAutoscaler)
		if !ok || resource == nil {
			return nil, fmt.Errorf("expected *k8s.ModelAutoscaler, got %T", obj)
		}
		return json.Marshal(convert.ModelAutoscalerFromK8s(resource))
	case "AgentAutoscaler":
		resource, ok := obj.(*k8s.AgentAutoscaler)
		if !ok || resource == nil {
			return nil, fmt.Errorf("expected *k8s.AgentAutoscaler, got %T", obj)
		}
		return json.Marshal(convert.AgentAutoscalerFromK8s(resource))
	case "SecretRef":
		resource, ok := obj.(*k8s.SecretRef)
		if !ok || resource == nil {
			return nil, fmt.Errorf("expected *k8s.SecretRef, got %T", obj)
		}
		return json.Marshal(convert.SecretRefFromK8s(resource))
	case "MemoryClass":
		resource, ok := obj.(*k8s.MemoryClass)
		if !ok || resource == nil {
			return nil, fmt.Errorf("expected *k8s.MemoryClass, got %T", obj)
		}
		return json.Marshal(convert.MemoryClassFromK8s(resource))
	case "MemoryClaim":
		resource, ok := obj.(*k8s.MemoryClaim)
		if !ok || resource == nil {
			return nil, fmt.Errorf("expected *k8s.MemoryClaim, got %T", obj)
		}
		return json.Marshal(convert.MemoryClaimFromK8s(resource))
	case "MemoryVolume":
		return marshalConverted(obj, "*k8s.MemoryVolume", convert.MemoryVolumeFromK8s)
	case "AgentPolicy":
		return marshalConverted(obj, "*k8s.AgentPolicy", convert.AgentPolicyFromK8s)
	case "CognitiveAdmissionPolicy":
		return marshalConverted(obj, "*k8s.CognitiveAdmissionPolicy", convert.CognitiveAdmissionPolicyFromK8s)
	case "CognitiveQuota":
		return marshalConverted(obj, "*k8s.CognitiveQuota", convert.CognitiveQuotaFromK8s)
	case "CognitiveLimitRange":
		return marshalConverted(obj, "*k8s.CognitiveLimitRange", convert.CognitiveLimitRangeFromK8s)
	case "Workspace":
		return marshalConverted(obj, "*k8s.Workspace", convert.WorkspaceFromK8s)
	case "AgentIdentity":
		return marshalConverted(obj, "*k8s.AgentIdentity", convert.AgentIdentityFromK8s)
	case "AgentRole":
		return marshalConverted(obj, "*k8s.AgentRole", convert.AgentRoleFromK8s)
	case "AgentRoleBinding":
		return marshalConverted(obj, "*k8s.AgentRoleBinding", convert.AgentRoleBindingFromK8s)
	case "AgentCommunicationPolicy":
		return marshalConverted(obj, "*k8s.AgentCommunicationPolicy", convert.AgentCommunicationPolicyFromK8s)
	case "MemoryGrant":
		return marshalConverted(obj, "*k8s.MemoryGrant", convert.MemoryGrantFromK8s)
	case "Memory":
		return marshalConverted(obj, "*k8s.Memory", convert.MemoryFromK8s)
	case "MemoryRecord":
		return marshalConverted(obj, "*k8s.MemoryRecord", convert.MemoryRecordFromK8s)
	case "WorkflowRun":
		return marshalConverted(obj, "*k8s.WorkflowRun", convert.WorkflowRunFromK8s)
	case "Evaluation":
		return marshalConverted(obj, "*k8s.Evaluation", convert.EvaluationFromK8s)
	case "EvaluationDataset":
		return marshalConverted(obj, "*k8s.EvaluationDataset", convert.EvaluationDatasetFromK8s)
	case "EvaluationRun":
		return marshalConverted(obj, "*k8s.EvaluationRun", convert.EvaluationRunFromK8s)
	case "KnowledgeBaseClass":
		return marshalConverted(obj, "*k8s.KnowledgeBaseClass", convert.KnowledgeBaseClassFromK8s)
	case "KnowledgeBase":
		return marshalConverted(obj, "*k8s.KnowledgeBase", convert.KnowledgeBaseFromK8s)
	case "KnowledgeBaseClaim":
		return marshalConverted(obj, "*k8s.KnowledgeBaseClaim", convert.KnowledgeBaseClaimFromK8s)
	case "KnowledgeBaseIndexRun":
		return marshalConverted(obj, "*k8s.KnowledgeBaseIndexRun", convert.KnowledgeBaseIndexRunFromK8s)
	case "KnowledgeBaseIndexSchedule":
		return marshalConverted(obj, "*k8s.KnowledgeBaseIndexSchedule", convert.KnowledgeBaseIndexScheduleFromK8s)
	case "ApprovalDecision":
		return marshalConverted(obj, "*k8s.ApprovalDecision", convert.ApprovalDecisionFromK8s)
	default:
		return nil, fmt.Errorf("unsupported kind for marshal: %s", kind)
	}
}

func marshalConverted[T client.Object, U any](obj client.Object, expected string, convertFn func(T) U) ([]byte, error) {
	resource, ok := obj.(T)
	if !ok || reflect.ValueOf(resource).IsNil() {
		return nil, fmt.Errorf("expected %s, got %T", expected, obj)
	}
	return json.Marshal(convertFn(resource))
}

// marshalListToInternal converts a K8s list object to a slice of internal JSON blobs.
func (s *StoreAdapter) marshalListToInternal(kind string, listObj client.ObjectList) ([][]byte, error) {
	switch kind {
	case "Agent":
		return marshalConvertedList(listObj, "*k8s.AgentList", func(list *k8s.AgentList) []k8s.Agent { return list.Items }, convert.AgentFromK8s)
	case "AgentInstance":
		return marshalConvertedList(listObj, "*k8s.AgentInstanceList", func(list *k8s.AgentInstanceList) []k8s.AgentInstance { return list.Items }, convert.AgentInstanceFromK8s)
	case "AgentJob":
		return marshalConvertedList(listObj, "*k8s.AgentJobList", func(list *k8s.AgentJobList) []k8s.AgentJob { return list.Items }, convert.AgentJobFromK8s)
	case "Workflow":
		return marshalConvertedList(listObj, "*k8s.WorkflowList", func(list *k8s.WorkflowList) []k8s.Workflow { return list.Items }, convert.WorkflowFromK8s)
	case "Tool":
		return marshalConvertedList(listObj, "*k8s.ToolList", func(list *k8s.ToolList) []k8s.Tool { return list.Items }, convert.ToolFromK8s)
	case "ToolGrant":
		return marshalConvertedList(listObj, "*k8s.ToolGrantList", func(list *k8s.ToolGrantList) []k8s.ToolGrant { return list.Items }, convert.ToolGrantFromK8s)
	case "ToolInvocationRun":
		return marshalConvertedList(listObj, "*k8s.ToolInvocationRunList", func(list *k8s.ToolInvocationRunList) []k8s.ToolInvocationRun { return list.Items }, convert.ToolInvocationRunFromK8s)
	case "ToolClass":
		return marshalConvertedList(listObj, "*k8s.ToolClassList", func(list *k8s.ToolClassList) []k8s.ToolClass { return list.Items }, convert.ToolClassFromK8s)
	case "Model":
		return marshalConvertedList(listObj, "*k8s.ModelList", func(list *k8s.ModelList) []k8s.Model { return list.Items }, convert.ModelFromK8s)
	case "ModelClass":
		return marshalConvertedList(listObj, "*k8s.ModelClassList", func(list *k8s.ModelClassList) []k8s.ModelClass { return list.Items }, convert.ModelClassFromK8s)
	case "ModelInvocationProfile":
		return marshalConvertedList(listObj, "*k8s.ModelInvocationProfileList", func(list *k8s.ModelInvocationProfileList) []k8s.ModelInvocationProfile { return list.Items }, convert.ModelInvocationProfileFromK8s)
	case "ExternalAgent":
		return marshalConvertedList(listObj, "*k8s.ExternalAgentList", func(list *k8s.ExternalAgentList) []k8s.ExternalAgent { return list.Items }, convert.ExternalAgentFromK8s)
	case "ExternalAgentGrant":
		return marshalConvertedList(listObj, "*k8s.ExternalAgentGrantList", func(list *k8s.ExternalAgentGrantList) []k8s.ExternalAgentGrant { return list.Items }, convert.ExternalAgentGrantFromK8s)
	case "ModelGrant":
		return marshalConvertedList(listObj, "*k8s.ModelGrantList", func(list *k8s.ModelGrantList) []k8s.ModelGrant { return list.Items }, convert.ModelGrantFromK8s)
	case "AutonomyClass":
		return marshalConvertedList(listObj, "*k8s.AutonomyClassList", func(list *k8s.AutonomyClassList) []k8s.AutonomyClass { return list.Items }, convert.AutonomyClassFromK8s)
	case "AutonomyReview":
		return marshalConvertedList(listObj, "*k8s.AutonomyReviewList", func(list *k8s.AutonomyReviewList) []k8s.AutonomyReview { return list.Items }, convert.AutonomyReviewFromK8s)
	case "TrustProfile":
		return marshalConvertedList(listObj, "*k8s.TrustProfileList", func(list *k8s.TrustProfileList) []k8s.TrustProfile { return list.Items }, convert.TrustProfileFromK8s)
	case "RuntimeApproval":
		return marshalConvertedList(listObj, "*k8s.RuntimeApprovalList", func(list *k8s.RuntimeApprovalList) []k8s.RuntimeApproval { return list.Items }, convert.RuntimeApprovalFromK8s)
	case "AgentService":
		return marshalConvertedList(listObj, "*k8s.AgentServiceList", func(list *k8s.AgentServiceList) []k8s.AgentService { return list.Items }, convert.AgentServiceFromK8s)
	case "IntentGateway":
		return marshalConvertedList(listObj, "*k8s.IntentGatewayList", func(list *k8s.IntentGatewayList) []k8s.IntentGateway { return list.Items }, convert.IntentGatewayFromK8s)
	case "Prompt":
		return marshalConvertedList(listObj, "*k8s.PromptList", func(list *k8s.PromptList) []k8s.Prompt { return list.Items }, convert.PromptFromK8s)
	case "Solution":
		return marshalConvertedList(listObj, "*k8s.SolutionList", func(list *k8s.SolutionList) []k8s.Solution { return list.Items }, convert.SolutionFromK8s)
	case "CrossNamespaceGrant":
		return marshalConvertedList(listObj, "*k8s.CrossNamespaceGrantList", func(list *k8s.CrossNamespaceGrantList) []k8s.CrossNamespaceGrant { return list.Items }, convert.CrossNamespaceGrantFromK8s)
	case "ModelAutoscaler":
		return marshalConvertedList(listObj, "*k8s.ModelAutoscalerList", func(list *k8s.ModelAutoscalerList) []k8s.ModelAutoscaler { return list.Items }, convert.ModelAutoscalerFromK8s)
	case "AgentAutoscaler":
		return marshalConvertedList(listObj, "*k8s.AgentAutoscalerList", func(list *k8s.AgentAutoscalerList) []k8s.AgentAutoscaler { return list.Items }, convert.AgentAutoscalerFromK8s)
	case "SecretRef":
		return marshalConvertedList(listObj, "*k8s.SecretRefList", func(list *k8s.SecretRefList) []k8s.SecretRef { return list.Items }, convert.SecretRefFromK8s)
	case "MemoryClass":
		return marshalConvertedList(listObj, "*k8s.MemoryClassList", func(list *k8s.MemoryClassList) []k8s.MemoryClass { return list.Items }, convert.MemoryClassFromK8s)
	case "MemoryClaim":
		return marshalConvertedList(listObj, "*k8s.MemoryClaimList", func(list *k8s.MemoryClaimList) []k8s.MemoryClaim { return list.Items }, convert.MemoryClaimFromK8s)
	case "MemoryVolume":
		return marshalConvertedList(listObj, "*k8s.MemoryVolumeList", func(list *k8s.MemoryVolumeList) []k8s.MemoryVolume { return list.Items }, convert.MemoryVolumeFromK8s)
	case "AgentPolicy":
		return marshalConvertedList(listObj, "*k8s.AgentPolicyList", func(list *k8s.AgentPolicyList) []k8s.AgentPolicy { return list.Items }, convert.AgentPolicyFromK8s)
	case "CognitiveAdmissionPolicy":
		return marshalConvertedList(listObj, "*k8s.CognitiveAdmissionPolicyList", func(list *k8s.CognitiveAdmissionPolicyList) []k8s.CognitiveAdmissionPolicy { return list.Items }, convert.CognitiveAdmissionPolicyFromK8s)
	case "CognitiveQuota":
		return marshalConvertedList(listObj, "*k8s.CognitiveQuotaList", func(list *k8s.CognitiveQuotaList) []k8s.CognitiveQuota { return list.Items }, convert.CognitiveQuotaFromK8s)
	case "CognitiveLimitRange":
		return marshalConvertedList(listObj, "*k8s.CognitiveLimitRangeList", func(list *k8s.CognitiveLimitRangeList) []k8s.CognitiveLimitRange { return list.Items }, convert.CognitiveLimitRangeFromK8s)
	case "Workspace":
		return marshalConvertedList(listObj, "*k8s.WorkspaceList", func(list *k8s.WorkspaceList) []k8s.Workspace { return list.Items }, convert.WorkspaceFromK8s)
	case "AgentIdentity":
		return marshalConvertedList(listObj, "*k8s.AgentIdentityList", func(list *k8s.AgentIdentityList) []k8s.AgentIdentity { return list.Items }, convert.AgentIdentityFromK8s)
	case "AgentRole":
		return marshalConvertedList(listObj, "*k8s.AgentRoleList", func(list *k8s.AgentRoleList) []k8s.AgentRole { return list.Items }, convert.AgentRoleFromK8s)
	case "AgentRoleBinding":
		return marshalConvertedList(listObj, "*k8s.AgentRoleBindingList", func(list *k8s.AgentRoleBindingList) []k8s.AgentRoleBinding { return list.Items }, convert.AgentRoleBindingFromK8s)
	case "AgentCommunicationPolicy":
		return marshalConvertedList(listObj, "*k8s.AgentCommunicationPolicyList", func(list *k8s.AgentCommunicationPolicyList) []k8s.AgentCommunicationPolicy { return list.Items }, convert.AgentCommunicationPolicyFromK8s)
	case "MemoryGrant":
		return marshalConvertedList(listObj, "*k8s.MemoryGrantList", func(list *k8s.MemoryGrantList) []k8s.MemoryGrant { return list.Items }, convert.MemoryGrantFromK8s)
	case "Memory":
		return marshalConvertedList(listObj, "*k8s.MemoryList", func(list *k8s.MemoryList) []k8s.Memory { return list.Items }, convert.MemoryFromK8s)
	case "MemoryRecord":
		return marshalConvertedList(listObj, "*k8s.MemoryRecordList", func(list *k8s.MemoryRecordList) []k8s.MemoryRecord { return list.Items }, convert.MemoryRecordFromK8s)
	case "WorkflowRun":
		return marshalConvertedList(listObj, "*k8s.WorkflowRunList", func(list *k8s.WorkflowRunList) []k8s.WorkflowRun { return list.Items }, convert.WorkflowRunFromK8s)
	case "Evaluation":
		return marshalConvertedList(listObj, "*k8s.EvaluationList", func(list *k8s.EvaluationList) []k8s.Evaluation { return list.Items }, convert.EvaluationFromK8s)
	case "EvaluationDataset":
		return marshalConvertedList(listObj, "*k8s.EvaluationDatasetList", func(list *k8s.EvaluationDatasetList) []k8s.EvaluationDataset { return list.Items }, convert.EvaluationDatasetFromK8s)
	case "EvaluationRun":
		return marshalConvertedList(listObj, "*k8s.EvaluationRunList", func(list *k8s.EvaluationRunList) []k8s.EvaluationRun { return list.Items }, convert.EvaluationRunFromK8s)
	case "KnowledgeBaseClass":
		return marshalConvertedList(listObj, "*k8s.KnowledgeBaseClassList", func(list *k8s.KnowledgeBaseClassList) []k8s.KnowledgeBaseClass { return list.Items }, convert.KnowledgeBaseClassFromK8s)
	case "KnowledgeBase":
		return marshalConvertedList(listObj, "*k8s.KnowledgeBaseList", func(list *k8s.KnowledgeBaseList) []k8s.KnowledgeBase { return list.Items }, convert.KnowledgeBaseFromK8s)
	case "KnowledgeBaseClaim":
		return marshalConvertedList(listObj, "*k8s.KnowledgeBaseClaimList", func(list *k8s.KnowledgeBaseClaimList) []k8s.KnowledgeBaseClaim { return list.Items }, convert.KnowledgeBaseClaimFromK8s)
	case "KnowledgeBaseIndexRun":
		return marshalConvertedList(listObj, "*k8s.KnowledgeBaseIndexRunList", func(list *k8s.KnowledgeBaseIndexRunList) []k8s.KnowledgeBaseIndexRun { return list.Items }, convert.KnowledgeBaseIndexRunFromK8s)
	case "KnowledgeBaseIndexSchedule":
		return marshalConvertedList(listObj, "*k8s.KnowledgeBaseIndexScheduleList", func(list *k8s.KnowledgeBaseIndexScheduleList) []k8s.KnowledgeBaseIndexSchedule { return list.Items }, convert.KnowledgeBaseIndexScheduleFromK8s)
	case "ApprovalDecision":
		return marshalConvertedList(listObj, "*k8s.ApprovalDecisionList", func(list *k8s.ApprovalDecisionList) []k8s.ApprovalDecision { return list.Items }, convert.ApprovalDecisionFromK8s)
	default:
		return nil, fmt.Errorf("unsupported kind for list: %s", kind)
	}
}

func marshalConvertedList[L client.ObjectList, K any, I any](listObj client.ObjectList, expected string, itemsFn func(L) []K, convertFn func(*K) *I) ([][]byte, error) {
	list, ok := listObj.(L)
	if !ok || reflect.ValueOf(list).IsNil() {
		return nil, fmt.Errorf("expected %s, got %T", expected, listObj)
	}
	return marshalConvertedItems(itemsFn(list), convertFn)
}

func marshalConvertedItems[K any, I any](items []K, convertFn func(*K) *I) ([][]byte, error) {
	result := make([][]byte, 0, len(items))
	for i := range items {
		internal := convertFn(&items[i])
		data, err := json.Marshal(internal)
		if err != nil {
			return nil, err
		}
		result = append(result, data)
	}
	return result, nil
}

// unmarshalFromInternal creates a K8s object from internal JSON via the convert layer.
func (s *StoreAdapter) unmarshalFromInternal(kind string, data []byte) (client.Object, error) {
	switch kind {
	case "Agent":
		var internal api.Agent
		if err := json.Unmarshal(data, &internal); err != nil {
			return nil, fmt.Errorf("unmarshal %s from internal: %w", kind, err)
		}
		return convert.AgentToK8s(&internal), nil
	case "AgentInstance":
		var internal api.AgentInstance
		if err := json.Unmarshal(data, &internal); err != nil {
			return nil, fmt.Errorf("unmarshal %s from internal: %w", kind, err)
		}
		return convert.AgentInstanceToK8s(&internal), nil
	case "AgentJob":
		var internal api.AgentJob
		if err := json.Unmarshal(data, &internal); err != nil {
			return nil, fmt.Errorf("unmarshal %s from internal: %w", kind, err)
		}
		return convert.AgentJobToK8s(&internal), nil
	case "Workflow":
		var internal api.Workflow
		if err := json.Unmarshal(data, &internal); err != nil {
			return nil, fmt.Errorf("unmarshal %s from internal: %w", kind, err)
		}
		return convert.WorkflowToK8s(&internal), nil
	case "Tool":
		var internal api.Tool
		if err := json.Unmarshal(data, &internal); err != nil {
			return nil, fmt.Errorf("unmarshal %s from internal: %w", kind, err)
		}
		return convert.ToolToK8s(&internal), nil
	case "ToolGrant":
		var internal api.ToolGrant
		if err := json.Unmarshal(data, &internal); err != nil {
			return nil, fmt.Errorf("unmarshal %s from internal: %w", kind, err)
		}
		return convert.ToolGrantToK8s(&internal), nil
	case "ToolInvocationRun":
		var internal api.ToolInvocationRun
		if err := json.Unmarshal(data, &internal); err != nil {
			return nil, fmt.Errorf("unmarshal %s from internal: %w", kind, err)
		}
		return convert.ToolInvocationRunToK8s(&internal), nil
	case "ToolClass":
		var internal api.ToolClass
		if err := json.Unmarshal(data, &internal); err != nil {
			return nil, fmt.Errorf("unmarshal %s from internal: %w", kind, err)
		}
		return convert.ToolClassToK8s(&internal), nil
	case "Model":
		var internal api.Model
		if err := json.Unmarshal(data, &internal); err != nil {
			return nil, fmt.Errorf("unmarshal %s from internal: %w", kind, err)
		}
		return convert.ModelToK8s(&internal), nil
	case "ModelClass":
		var internal api.ModelClass
		if err := json.Unmarshal(data, &internal); err != nil {
			return nil, fmt.Errorf("unmarshal %s from internal: %w", kind, err)
		}
		return convert.ModelClassToK8s(&internal), nil
	case "ModelInvocationProfile":
		var internal api.ModelInvocationProfile
		if err := json.Unmarshal(data, &internal); err != nil {
			return nil, fmt.Errorf("unmarshal %s from internal: %w", kind, err)
		}
		return convert.ModelInvocationProfileToK8s(&internal), nil
	case "ExternalAgent":
		var internal api.ExternalAgent
		if err := json.Unmarshal(data, &internal); err != nil {
			return nil, fmt.Errorf("unmarshal %s from internal: %w", kind, err)
		}
		return convert.ExternalAgentToK8s(&internal), nil
	case "ExternalAgentGrant":
		var internal api.ExternalAgentGrant
		if err := json.Unmarshal(data, &internal); err != nil {
			return nil, fmt.Errorf("unmarshal %s from internal: %w", kind, err)
		}
		return convert.ExternalAgentGrantToK8s(&internal), nil
	case "ModelGrant":
		var internal api.ModelGrant
		if err := json.Unmarshal(data, &internal); err != nil {
			return nil, fmt.Errorf("unmarshal %s from internal: %w", kind, err)
		}
		return convert.ModelGrantToK8s(&internal), nil
	case "AutonomyClass":
		var internal api.AutonomyClass
		if err := json.Unmarshal(data, &internal); err != nil {
			return nil, fmt.Errorf("unmarshal %s from internal: %w", kind, err)
		}
		return convert.AutonomyClassToK8s(&internal), nil
	case "AutonomyReview":
		var internal api.AutonomyReview
		if err := json.Unmarshal(data, &internal); err != nil {
			return nil, fmt.Errorf("unmarshal %s from internal: %w", kind, err)
		}
		return convert.AutonomyReviewToK8s(&internal), nil
	case "ModelAutoscaler":
		var internal api.ModelAutoscaler
		if err := json.Unmarshal(data, &internal); err != nil {
			return nil, fmt.Errorf("unmarshal %s from internal: %w", kind, err)
		}
		return convert.ModelAutoscalerToK8s(&internal), nil
	case "AgentAutoscaler":
		var internal api.AgentAutoscaler
		if err := json.Unmarshal(data, &internal); err != nil {
			return nil, fmt.Errorf("unmarshal %s from internal: %w", kind, err)
		}
		return convert.AgentAutoscalerToK8s(&internal), nil
	case "SecretRef":
		var internal api.SecretRef
		if err := json.Unmarshal(data, &internal); err != nil {
			return nil, fmt.Errorf("unmarshal %s from internal: %w", kind, err)
		}
		return convert.SecretRefToK8s(&internal), nil
	case "MemoryClass":
		var internal api.MemoryClass
		if err := json.Unmarshal(data, &internal); err != nil {
			return nil, fmt.Errorf("unmarshal %s from internal: %w", kind, err)
		}
		return convert.MemoryClassToK8s(&internal), nil
	case "MemoryClaim":
		var internal api.MemoryClaim
		if err := json.Unmarshal(data, &internal); err != nil {
			return nil, fmt.Errorf("unmarshal %s from internal: %w", kind, err)
		}
		return convert.MemoryClaimToK8s(&internal), nil
	case "MemoryVolume":
		var internal api.MemoryVolume
		if err := json.Unmarshal(data, &internal); err != nil {
			return nil, fmt.Errorf("unmarshal %s from internal: %w", kind, err)
		}
		return convert.MemoryVolumeToK8s(&internal), nil
	case "AgentPolicy":
		var internal api.AgentPolicy
		if err := json.Unmarshal(data, &internal); err != nil {
			return nil, fmt.Errorf("unmarshal %s from internal: %w", kind, err)
		}
		return convert.AgentPolicyToK8s(&internal), nil
	case "CognitiveAdmissionPolicy":
		var internal api.CognitiveAdmissionPolicy
		if err := json.Unmarshal(data, &internal); err != nil {
			return nil, fmt.Errorf("unmarshal %s from internal: %w", kind, err)
		}
		return convert.CognitiveAdmissionPolicyToK8s(&internal), nil
	case "CognitiveQuota":
		var internal api.CognitiveQuota
		if err := json.Unmarshal(data, &internal); err != nil {
			return nil, fmt.Errorf("unmarshal %s from internal: %w", kind, err)
		}
		return convert.CognitiveQuotaToK8s(&internal), nil
	case "CognitiveLimitRange":
		var internal api.CognitiveLimitRange
		if err := json.Unmarshal(data, &internal); err != nil {
			return nil, fmt.Errorf("unmarshal %s from internal: %w", kind, err)
		}
		return convert.CognitiveLimitRangeToK8s(&internal), nil
	case "Workspace":
		var internal api.Workspace
		if err := json.Unmarshal(data, &internal); err != nil {
			return nil, fmt.Errorf("unmarshal %s from internal: %w", kind, err)
		}
		return convert.WorkspaceToK8s(&internal), nil
	case "AgentIdentity":
		var internal api.AgentIdentity
		if err := json.Unmarshal(data, &internal); err != nil {
			return nil, fmt.Errorf("unmarshal %s from internal: %w", kind, err)
		}
		return convert.AgentIdentityToK8s(&internal), nil
	case "AgentRole":
		var internal api.AgentRole
		if err := json.Unmarshal(data, &internal); err != nil {
			return nil, fmt.Errorf("unmarshal %s from internal: %w", kind, err)
		}
		return convert.AgentRoleToK8s(&internal), nil
	case "AgentRoleBinding":
		var internal api.AgentRoleBinding
		if err := json.Unmarshal(data, &internal); err != nil {
			return nil, fmt.Errorf("unmarshal %s from internal: %w", kind, err)
		}
		return convert.AgentRoleBindingToK8s(&internal), nil
	case "AgentCommunicationPolicy":
		var internal api.AgentCommunicationPolicy
		if err := json.Unmarshal(data, &internal); err != nil {
			return nil, fmt.Errorf("unmarshal %s from internal: %w", kind, err)
		}
		return convert.AgentCommunicationPolicyToK8s(&internal), nil
	case "MemoryGrant":
		var internal api.MemoryGrant
		if err := json.Unmarshal(data, &internal); err != nil {
			return nil, fmt.Errorf("unmarshal %s from internal: %w", kind, err)
		}
		return convert.MemoryGrantToK8s(&internal), nil
	case "Memory":
		var internal api.Memory
		if err := json.Unmarshal(data, &internal); err != nil {
			return nil, fmt.Errorf("unmarshal %s from internal: %w", kind, err)
		}
		return convert.MemoryToK8s(&internal), nil
	case "MemoryRecord":
		var internal api.MemoryRecord
		if err := json.Unmarshal(data, &internal); err != nil {
			return nil, fmt.Errorf("unmarshal %s from internal: %w", kind, err)
		}
		return convert.MemoryRecordToK8s(&internal), nil
	case "WorkflowRun":
		var internal api.WorkflowRun
		if err := json.Unmarshal(data, &internal); err != nil {
			return nil, fmt.Errorf("unmarshal %s from internal: %w", kind, err)
		}
		return convert.WorkflowRunToK8s(&internal), nil
	case "Evaluation":
		var internal api.Evaluation
		if err := json.Unmarshal(data, &internal); err != nil {
			return nil, fmt.Errorf("unmarshal %s from internal: %w", kind, err)
		}
		return convert.EvaluationToK8s(&internal), nil
	case "EvaluationDataset":
		var internal api.EvaluationDataset
		if err := json.Unmarshal(data, &internal); err != nil {
			return nil, fmt.Errorf("unmarshal %s from internal: %w", kind, err)
		}
		return convert.EvaluationDatasetToK8s(&internal), nil
	case "EvaluationRun":
		var internal api.EvaluationRun
		if err := json.Unmarshal(data, &internal); err != nil {
			return nil, fmt.Errorf("unmarshal %s from internal: %w", kind, err)
		}
		return convert.EvaluationRunToK8s(&internal), nil
	case "KnowledgeBaseClass":
		var internal api.KnowledgeBaseClass
		if err := json.Unmarshal(data, &internal); err != nil {
			return nil, fmt.Errorf("unmarshal %s from internal: %w", kind, err)
		}
		return convert.KnowledgeBaseClassToK8s(&internal), nil
	case "KnowledgeBase":
		var internal api.KnowledgeBase
		if err := json.Unmarshal(data, &internal); err != nil {
			return nil, fmt.Errorf("unmarshal %s from internal: %w", kind, err)
		}
		return convert.KnowledgeBaseToK8s(&internal), nil
	case "KnowledgeBaseClaim":
		var internal api.KnowledgeBaseClaim
		if err := json.Unmarshal(data, &internal); err != nil {
			return nil, fmt.Errorf("unmarshal %s from internal: %w", kind, err)
		}
		return convert.KnowledgeBaseClaimToK8s(&internal), nil
	case "KnowledgeBaseIndexRun":
		var internal api.KnowledgeBaseIndexRun
		if err := json.Unmarshal(data, &internal); err != nil {
			return nil, fmt.Errorf("unmarshal %s from internal: %w", kind, err)
		}
		return convert.KnowledgeBaseIndexRunToK8s(&internal), nil
	case "KnowledgeBaseIndexSchedule":
		var internal api.KnowledgeBaseIndexSchedule
		if err := json.Unmarshal(data, &internal); err != nil {
			return nil, fmt.Errorf("unmarshal %s from internal: %w", kind, err)
		}
		return convert.KnowledgeBaseIndexScheduleToK8s(&internal), nil
	case "ApprovalDecision":
		var internal api.ApprovalDecision
		if err := json.Unmarshal(data, &internal); err != nil {
			return nil, fmt.Errorf("unmarshal %s from internal: %w", kind, err)
		}
		return convert.ApprovalDecisionToK8s(&internal), nil
	case "TrustProfile":
		var internal api.TrustProfile
		if err := json.Unmarshal(data, &internal); err != nil {
			return nil, fmt.Errorf("unmarshal %s from internal: %w", kind, err)
		}
		return convert.TrustProfileToK8s(&internal), nil
	case "RuntimeApproval":
		var internal api.RuntimeApproval
		if err := json.Unmarshal(data, &internal); err != nil {
			return nil, fmt.Errorf("unmarshal %s from internal: %w", kind, err)
		}
		return convert.RuntimeApprovalToK8s(&internal), nil
	case "AgentService":
		var internal api.AgentService
		if err := json.Unmarshal(data, &internal); err != nil {
			return nil, fmt.Errorf("unmarshal %s from internal: %w", kind, err)
		}
		return convert.AgentServiceToK8s(&internal), nil
	case "IntentGateway":
		var internal api.IntentGateway
		if err := json.Unmarshal(data, &internal); err != nil {
			return nil, fmt.Errorf("unmarshal %s from internal: %w", kind, err)
		}
		return convert.IntentGatewayToK8s(&internal), nil
	case "Prompt":
		var internal api.Prompt
		if err := json.Unmarshal(data, &internal); err != nil {
			return nil, fmt.Errorf("unmarshal %s from internal: %w", kind, err)
		}
		return convert.PromptToK8s(&internal), nil
	case "Solution":
		var internal api.Solution
		if err := json.Unmarshal(data, &internal); err != nil {
			return nil, fmt.Errorf("unmarshal %s from internal: %w", kind, err)
		}
		return convert.SolutionToK8s(&internal), nil
	case "CrossNamespaceGrant":
		var internal api.CrossNamespaceGrant
		if err := json.Unmarshal(data, &internal); err != nil {
			return nil, fmt.Errorf("unmarshal %s from internal: %w", kind, err)
		}
		return convert.CrossNamespaceGrantToK8s(&internal), nil
	default:
		return nil, fmt.Errorf("unsupported kind: %s", kind)
	}
}

// newObject creates a zero-value K8s object for the given kind.
func (s *StoreAdapter) newObject(kind string) (client.Object, error) {
	switch kind {
	case "Agent":
		return &k8s.Agent{}, nil
	case "AgentInstance":
		return &k8s.AgentInstance{}, nil
	case "AgentJob":
		return &k8s.AgentJob{}, nil
	case "Workflow":
		return &k8s.Workflow{}, nil
	case "Tool":
		return &k8s.Tool{}, nil
	case "ToolGrant":
		return &k8s.ToolGrant{}, nil
	case "ToolInvocationRun":
		return &k8s.ToolInvocationRun{}, nil
	case "ToolClass":
		return &k8s.ToolClass{}, nil
	case "Model":
		return &k8s.Model{}, nil
	case "ModelClass":
		return &k8s.ModelClass{}, nil
	case "ModelInvocationProfile":
		return &k8s.ModelInvocationProfile{}, nil
	case "ExternalAgent":
		return &k8s.ExternalAgent{}, nil
	case "ExternalAgentGrant":
		return &k8s.ExternalAgentGrant{}, nil
	case "ModelGrant":
		return &k8s.ModelGrant{}, nil
	case "AutonomyClass":
		return &k8s.AutonomyClass{}, nil
	case "AutonomyReview":
		return &k8s.AutonomyReview{}, nil
	case "TrustProfile":
		return &k8s.TrustProfile{}, nil
	case "RuntimeApproval":
		return &k8s.RuntimeApproval{}, nil
	case "AgentService":
		return &k8s.AgentService{}, nil
	case "IntentGateway":
		return &k8s.IntentGateway{}, nil
	case "Prompt":
		return &k8s.Prompt{}, nil
	case "Solution":
		return &k8s.Solution{}, nil
	case "CrossNamespaceGrant":
		return &k8s.CrossNamespaceGrant{}, nil
	case "ModelAutoscaler":
		return &k8s.ModelAutoscaler{}, nil
	case "AgentAutoscaler":
		return &k8s.AgentAutoscaler{}, nil
	case "SecretRef":
		return &k8s.SecretRef{}, nil
	case "MemoryClass":
		return &k8s.MemoryClass{}, nil
	case "MemoryClaim":
		return &k8s.MemoryClaim{}, nil
	case "MemoryVolume":
		return &k8s.MemoryVolume{}, nil
	case "AgentPolicy":
		return &k8s.AgentPolicy{}, nil
	case "CognitiveAdmissionPolicy":
		return &k8s.CognitiveAdmissionPolicy{}, nil
	case "CognitiveQuota":
		return &k8s.CognitiveQuota{}, nil
	case "CognitiveLimitRange":
		return &k8s.CognitiveLimitRange{}, nil
	case "Workspace":
		return &k8s.Workspace{}, nil
	case "AgentIdentity":
		return &k8s.AgentIdentity{}, nil
	case "AgentRole":
		return &k8s.AgentRole{}, nil
	case "AgentRoleBinding":
		return &k8s.AgentRoleBinding{}, nil
	case "AgentCommunicationPolicy":
		return &k8s.AgentCommunicationPolicy{}, nil
	case "MemoryGrant":
		return &k8s.MemoryGrant{}, nil
	case "Memory":
		return &k8s.Memory{}, nil
	case "MemoryRecord":
		return &k8s.MemoryRecord{}, nil
	case "WorkflowRun":
		return &k8s.WorkflowRun{}, nil
	case "Evaluation":
		return &k8s.Evaluation{}, nil
	case "EvaluationDataset":
		return &k8s.EvaluationDataset{}, nil
	case "EvaluationRun":
		return &k8s.EvaluationRun{}, nil
	case "KnowledgeBaseClass":
		return &k8s.KnowledgeBaseClass{}, nil
	case "KnowledgeBase":
		return &k8s.KnowledgeBase{}, nil
	case "KnowledgeBaseClaim":
		return &k8s.KnowledgeBaseClaim{}, nil
	case "KnowledgeBaseIndexRun":
		return &k8s.KnowledgeBaseIndexRun{}, nil
	case "KnowledgeBaseIndexSchedule":
		return &k8s.KnowledgeBaseIndexSchedule{}, nil
	case "ApprovalDecision":
		return &k8s.ApprovalDecision{}, nil
	default:
		return nil, fmt.Errorf("unsupported kind: %s", kind)
	}
}

// newListObject creates a zero-value K8s list object for the given kind.
func (s *StoreAdapter) newListObject(kind string) (client.ObjectList, error) {
	switch kind {
	case "Agent":
		return &k8s.AgentList{}, nil
	case "AgentInstance":
		return &k8s.AgentInstanceList{}, nil
	case "AgentJob":
		return &k8s.AgentJobList{}, nil
	case "Workflow":
		return &k8s.WorkflowList{}, nil
	case "Tool":
		return &k8s.ToolList{}, nil
	case "ToolGrant":
		return &k8s.ToolGrantList{}, nil
	case "ToolInvocationRun":
		return &k8s.ToolInvocationRunList{}, nil
	case "ToolClass":
		return &k8s.ToolClassList{}, nil
	case "Model":
		return &k8s.ModelList{}, nil
	case "ModelClass":
		return &k8s.ModelClassList{}, nil
	case "ModelInvocationProfile":
		return &k8s.ModelInvocationProfileList{}, nil
	case "ExternalAgent":
		return &k8s.ExternalAgentList{}, nil
	case "ExternalAgentGrant":
		return &k8s.ExternalAgentGrantList{}, nil
	case "ModelGrant":
		return &k8s.ModelGrantList{}, nil
	case "AutonomyClass":
		return &k8s.AutonomyClassList{}, nil
	case "AutonomyReview":
		return &k8s.AutonomyReviewList{}, nil
	case "TrustProfile":
		return &k8s.TrustProfileList{}, nil
	case "RuntimeApproval":
		return &k8s.RuntimeApprovalList{}, nil
	case "AgentService":
		return &k8s.AgentServiceList{}, nil
	case "IntentGateway":
		return &k8s.IntentGatewayList{}, nil
	case "Prompt":
		return &k8s.PromptList{}, nil
	case "Solution":
		return &k8s.SolutionList{}, nil
	case "CrossNamespaceGrant":
		return &k8s.CrossNamespaceGrantList{}, nil
	case "ModelAutoscaler":
		return &k8s.ModelAutoscalerList{}, nil
	case "AgentAutoscaler":
		return &k8s.AgentAutoscalerList{}, nil
	case "SecretRef":
		return &k8s.SecretRefList{}, nil
	case "MemoryClass":
		return &k8s.MemoryClassList{}, nil
	case "MemoryClaim":
		return &k8s.MemoryClaimList{}, nil
	case "MemoryVolume":
		return &k8s.MemoryVolumeList{}, nil
	case "AgentPolicy":
		return &k8s.AgentPolicyList{}, nil
	case "CognitiveAdmissionPolicy":
		return &k8s.CognitiveAdmissionPolicyList{}, nil
	case "CognitiveQuota":
		return &k8s.CognitiveQuotaList{}, nil
	case "CognitiveLimitRange":
		return &k8s.CognitiveLimitRangeList{}, nil
	case "Workspace":
		return &k8s.WorkspaceList{}, nil
	case "AgentIdentity":
		return &k8s.AgentIdentityList{}, nil
	case "AgentRole":
		return &k8s.AgentRoleList{}, nil
	case "AgentRoleBinding":
		return &k8s.AgentRoleBindingList{}, nil
	case "AgentCommunicationPolicy":
		return &k8s.AgentCommunicationPolicyList{}, nil
	case "MemoryGrant":
		return &k8s.MemoryGrantList{}, nil
	case "Memory":
		return &k8s.MemoryList{}, nil
	case "MemoryRecord":
		return &k8s.MemoryRecordList{}, nil
	case "WorkflowRun":
		return &k8s.WorkflowRunList{}, nil
	case "Evaluation":
		return &k8s.EvaluationList{}, nil
	case "EvaluationDataset":
		return &k8s.EvaluationDatasetList{}, nil
	case "EvaluationRun":
		return &k8s.EvaluationRunList{}, nil
	case "KnowledgeBaseClass":
		return &k8s.KnowledgeBaseClassList{}, nil
	case "KnowledgeBase":
		return &k8s.KnowledgeBaseList{}, nil
	case "KnowledgeBaseClaim":
		return &k8s.KnowledgeBaseClaimList{}, nil
	case "KnowledgeBaseIndexRun":
		return &k8s.KnowledgeBaseIndexRunList{}, nil
	case "KnowledgeBaseIndexSchedule":
		return &k8s.KnowledgeBaseIndexScheduleList{}, nil
	case "ApprovalDecision":
		return &k8s.ApprovalDecisionList{}, nil
	default:
		return nil, fmt.Errorf("unsupported kind for list: %s", kind)
	}
}

// decodeContinueOffset decodes a base64-encoded integer offset from a continue token.
func decodeContinueOffset(token string) (int, error) {
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(string(raw))
}

// metadataChanged returns true if the non-status portion of the object has
// changed (labels, annotations, finalizers, owner references, or spec).
func metadataChanged(current, desired client.Object) bool {
	if !reflect.DeepEqual(current.GetFinalizers(), desired.GetFinalizers()) {
		return true
	}
	if !reflect.DeepEqual(current.GetLabels(), desired.GetLabels()) {
		return true
	}
	if !reflect.DeepEqual(current.GetAnnotations(), desired.GetAnnotations()) {
		return true
	}
	if !reflect.DeepEqual(current.GetOwnerReferences(), desired.GetOwnerReferences()) {
		return true
	}
	return specChanged(current, desired)
}

func statusChanged(current, desired client.Object) bool {
	currentStatus := reflect.ValueOf(current).Elem().FieldByName("Status")
	desiredStatus := reflect.ValueOf(desired).Elem().FieldByName("Status")
	if !currentStatus.IsValid() || !desiredStatus.IsValid() {
		return false
	}
	return !reflect.DeepEqual(currentStatus.Interface(), desiredStatus.Interface())
}

func isClusterScopedKind(kind string) bool {
	switch kind {
	case "AutonomyClass", "KnowledgeBaseClass", "MemoryClass", "ModelClass", "ToolClass":
		return true
	default:
		return false
	}
}

func kindHasStatusSubresource(kind string) bool {
	switch kind {
	case "AgentRole", "AgentRoleBinding", "AgentCommunicationPolicy", "AutonomyClass", "CognitiveAdmissionPolicy", "CognitiveLimitRange", "CrossNamespaceGrant", "MemoryGrant", "ModelGrant", "ModelInvocationProfile", "ExternalAgentGrant":
		return false
	default:
		return true
	}
}

// specChanged compares the .Spec field of two typed objects via reflection.
// Returns false if either object lacks a Spec field (treat as unchanged).
func specChanged(current, desired client.Object) bool {
	currentSpec := reflect.ValueOf(current).Elem().FieldByName("Spec")
	desiredSpec := reflect.ValueOf(desired).Elem().FieldByName("Spec")
	if !currentSpec.IsValid() || !desiredSpec.IsValid() {
		return false
	}
	return !reflect.DeepEqual(currentSpec.Interface(), desiredSpec.Interface())
}
