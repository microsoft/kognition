// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	k8s "github.com/kognition-ai/kognition/api/k8s/v1alpha1"
	api "github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/convert"
	"github.com/kognition-ai/kognition/internal/runtimeapproval"
	"github.com/kognition-ai/kognition/internal/runtimeapproval/conformancetest"
	"github.com/kognition-ai/kognition/internal/store"
)

type runtimeApprovalSuccessfulErrorClient struct {
	client.Client
	returnError bool
	failBefore  bool
}

type runtimeApprovalSuccessfulErrorStatusWriter struct {
	client.SubResourceWriter
	parent *runtimeApprovalSuccessfulErrorClient
}

type runtimeApprovalInitialGetMissClient struct {
	client.Client
	miss bool
}

type runtimeApprovalInitialLeaseGetMissClient struct {
	client.Client
	miss bool
}

type runtimeApprovalSuccessfulCreateErrorClient struct {
	client.Client
	targetName   string
	targetObject client.Object
	once         sync.Once
}

type runtimeApprovalTicketOrderClient struct {
	client.Client
	testing *testing.T
}

type runtimeApprovalTransientWorkspaceLeaseClient struct {
	client.Client
	contentions int
}

type runtimeApprovalTransientBindingLeaseClient struct {
	client.Client
	contentions int
}

func (c *runtimeApprovalTransientBindingLeaseClient) Get(ctx context.Context, key client.ObjectKey, object client.Object, options ...client.GetOption) error {
	if err := c.Client.Get(ctx, key, object, options...); err != nil {
		return err
	}
	if lease, ok := object.(*coordinationv1.Lease); ok && key.Name == runtimeApprovalBindingLeaseName && c.contentions > 0 {
		c.contentions--
		holder := "other-gateway-replica"
		duration := int32(30)
		now := metav1.NewMicroTime(time.Now().UTC())
		lease.Spec.HolderIdentity = &holder
		lease.Spec.LeaseDurationSeconds = &duration
		lease.Spec.RenewTime = &now
	}
	return nil
}

type runtimeApprovalStaleWorkspaceLeaseClient struct {
	client.Client
}

func (c *runtimeApprovalStaleWorkspaceLeaseClient) Get(ctx context.Context, key client.ObjectKey, object client.Object, options ...client.GetOption) error {
	if lease, ok := object.(*coordinationv1.Lease); ok && key.Name == runtimeApprovalWorkspaceLeaseName {
		holder := "stale-cache-holder"
		duration := int32(30)
		now := metav1.NewMicroTime(time.Now().UTC())
		*lease = coordinationv1.Lease{
			ObjectMeta: metav1.ObjectMeta{Namespace: key.Namespace, Name: key.Name},
			Spec: coordinationv1.LeaseSpec{
				HolderIdentity:       &holder,
				LeaseDurationSeconds: &duration,
				RenewTime:            &now,
			},
		}
		return nil
	}
	return c.Client.Get(ctx, key, object, options...)
}

func (c *runtimeApprovalTransientWorkspaceLeaseClient) Get(ctx context.Context, key client.ObjectKey, object client.Object, options ...client.GetOption) error {
	if lease, ok := object.(*coordinationv1.Lease); ok && key.Name == runtimeApprovalWorkspaceLeaseName && c.contentions > 0 {
		c.contentions--
		holder := "retention-reconciler"
		duration := int32(30)
		now := metav1.NewMicroTime(time.Now().UTC())
		*lease = coordinationv1.Lease{
			ObjectMeta: metav1.ObjectMeta{Namespace: key.Namespace, Name: key.Name},
			Spec: coordinationv1.LeaseSpec{
				HolderIdentity:       &holder,
				LeaseDurationSeconds: &duration,
				RenewTime:            &now,
			},
		}
		return nil
	}
	return c.Client.Get(ctx, key, object, options...)
}

func (c *runtimeApprovalTicketOrderClient) Create(ctx context.Context, object client.Object, options ...client.CreateOption) error {
	if approval, ok := object.(*k8s.RuntimeApproval); ok {
		ticket := &k8s.RuntimeApprovalQuotaTicket{}
		if err := c.Client.Get(ctx, client.ObjectKeyFromObject(approval), ticket); err != nil {
			c.testing.Fatalf("quota ticket did not exist before RuntimeApproval create: %v", err)
		}
		if ticket.Spec.Workspace != approval.Spec.Workspace || ticket.Spec.InvocationID != approval.Spec.InvocationID {
			c.testing.Fatalf("quota ticket logical key = %s/%s, want %s/%s", ticket.Spec.Workspace, ticket.Spec.InvocationID, approval.Spec.Workspace, approval.Spec.InvocationID)
		}
		lease := &coordinationv1.Lease{}
		leaseKey := client.ObjectKey{Namespace: approval.Spec.Workspace, Name: runtimeApprovalWorkspaceLeaseName}
		if err := c.Client.Get(ctx, leaseKey, lease); err != nil {
			c.testing.Fatalf("workspace Lease did not exist during RuntimeApproval create: %v", err)
		}
		if !bindingLeaseHeld(lease, time.Now().UTC()) {
			c.testing.Fatal("workspace Lease was not held during RuntimeApproval create")
		}
	}
	return c.Client.Create(ctx, object, options...)
}

func (c *runtimeApprovalSuccessfulCreateErrorClient) Create(ctx context.Context, object client.Object, options ...client.CreateOption) error {
	if err := c.Client.Create(ctx, object, options...); err != nil {
		return err
	}
	returnError := false
	if object.GetName() == c.targetName && reflect.TypeOf(object) == reflect.TypeOf(c.targetObject) {
		c.once.Do(func() { returnError = true })
	}
	if returnError {
		return errors.New("transport result lost after create")
	}
	return nil
}

type blockingBindingLeaseUpdateClient struct {
	client.Client
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (c *blockingBindingLeaseUpdateClient) Update(ctx context.Context, object client.Object, options ...client.UpdateOption) error {
	if lease, ok := object.(*coordinationv1.Lease); ok && lease.Name == runtimeApprovalBindingLeaseName {
		blocked := false
		c.once.Do(func() {
			blocked = true
			close(c.started)
		})
		if blocked {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-c.release:
			}
		}
	}
	return c.Client.Update(ctx, object, options...)
}

func (c *runtimeApprovalInitialGetMissClient) Get(ctx context.Context, key client.ObjectKey, object client.Object, options ...client.GetOption) error {
	if _, ok := object.(*k8s.RuntimeApproval); ok && c.miss {
		c.miss = false
		return apierrors.NewNotFound(schema.GroupResource{Group: k8s.GroupVersion.Group, Resource: "runtimeapprovals"}, key.Name)
	}
	return c.Client.Get(ctx, key, object, options...)
}

func (c *runtimeApprovalInitialLeaseGetMissClient) Get(ctx context.Context, key client.ObjectKey, object client.Object, options ...client.GetOption) error {
	if _, ok := object.(*coordinationv1.Lease); ok && c.miss && key.Name == runtimeApprovalBindingLeaseName {
		c.miss = false
		return apierrors.NewNotFound(schema.GroupResource{Group: coordinationv1.GroupName, Resource: "leases"}, key.Name)
	}
	return c.Client.Get(ctx, key, object, options...)
}

func (c *runtimeApprovalSuccessfulErrorClient) Status() client.SubResourceWriter {
	return &runtimeApprovalSuccessfulErrorStatusWriter{SubResourceWriter: c.Client.Status(), parent: c}
}

func (w *runtimeApprovalSuccessfulErrorStatusWriter) Update(ctx context.Context, object client.Object, options ...client.SubResourceUpdateOption) error {
	if w.parent.failBefore {
		w.parent.failBefore = false
		return errors.New("transport failed before status update")
	}
	if err := w.SubResourceWriter.Update(ctx, object, options...); err != nil {
		return err
	}
	if w.parent.returnError {
		w.parent.returnError = false
		return errors.New("transport result lost after status update")
	}
	return nil
}

func TestKubernetesRuntimeApprovalCreateReplayDecisionAndConservativeExpiry(t *testing.T) {
	ctx := context.Background()
	adapter := newRuntimeApprovalTestStore(t, newFakeClient(t))
	adapter.ConfigureRuntimeApprovalBindingKeys(kubernetesRuntimeApprovalTestKeys(t))
	now := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	adapter.now = func() time.Time { return now }

	proposed := kubernetesRuntimeApprovalFixture(t, "invocation-a", api.RuntimeApprovalDecisionPending)
	created, first, err := adapter.CreateOrGetRuntimeApproval(ctx, proposed, kubernetesRuntimeApprovalPayload, 15*time.Minute)
	if err != nil || !created || first.ResourceVersion <= 0 || first.Name != runtimeapproval.CanonicalName("team-a", "invocation-a") {
		t.Fatalf("create = created %v, approval %#v, err %v", created, first, err)
	}
	created, replay, err := adapter.CreateOrGetRuntimeApproval(ctx, proposed, kubernetesRuntimeApprovalPayload, time.Minute)
	if err != nil || created || replay.UID != first.UID || !replay.Spec.ExpiresAt.Equal(first.Spec.ExpiresAt) {
		t.Fatalf("replay = created %v, approval %#v, err %v", created, replay, err)
	}
	loosened := proposed
	loosened.Status = runtimeapproval.InitialStatus(api.RuntimeApprovalDecisionNotRequired)
	if _, _, err := adapter.CreateOrGetRuntimeApproval(ctx, loosened, kubernetesRuntimeApprovalPayload, time.Minute); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("policy-loosening replay error = %v, want conflict", err)
	}
	conflict := proposed
	conflict.Spec.Binding.Tag = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	if _, _, err := adapter.CreateOrGetRuntimeApproval(ctx, conflict, kubernetesRuntimeApprovalPayload, time.Minute); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("changed binding error = %v", err)
	}

	decided, err := adapter.DecideRuntimeApproval(ctx, "team-a", "invocation-a", store.RuntimeApprovalDecision{
		Decision: api.RuntimeApprovalDecisionApproved, ExpectedResourceVersion: first.ResourceVersion,
		Issuer: "https://issuer.example", Subject: "operator-1",
	})
	if err != nil || decided.Status.DecisionState != api.RuntimeApprovalDecisionApproved {
		t.Fatalf("decision = %#v, err %v", decided.Status, err)
	}
	if decided.Status.DecisionAudit == nil || decided.Status.DecisionAudit.Payload.Decision != api.RuntimeApprovalDecisionApproved {
		t.Fatalf("decision audit outbox = %#v", decided.Status.DecisionAudit)
	}
	if _, err := adapter.AcknowledgeRuntimeApprovalDecisionAudit(ctx, "team-a", "invocation-a", decided.ResourceVersion, "wrong-audit-id"); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("wrong decision audit ID error = %v, want conflict", err)
	}
	acknowledged, err := adapter.AcknowledgeRuntimeApprovalDecisionAudit(ctx, "team-a", "invocation-a", decided.ResourceVersion, decided.Status.DecisionAudit.ID)
	if err != nil || !acknowledged.Status.DecisionEvidenceDelivered {
		t.Fatalf("acknowledge = %#v, %v", acknowledged.Status, err)
	}
	retried, err := adapter.AcknowledgeRuntimeApprovalDecisionAudit(ctx, "team-a", "invocation-a", decided.ResourceVersion, decided.Status.DecisionAudit.ID)
	if err != nil || retried.ResourceVersion != acknowledged.ResourceVersion {
		t.Fatalf("idempotent acknowledgement retry = rv %d, err %v", retried.ResourceVersion, err)
	}
	retry, err := adapter.DecideRuntimeApproval(ctx, "team-a", "invocation-a", store.RuntimeApprovalDecision{
		Decision: api.RuntimeApprovalDecisionRejected, ExpectedResourceVersion: first.ResourceVersion,
		Issuer: "https://issuer.example", Subject: "operator-2",
	})
	if err != nil || retry.Status.DecisionState != api.RuntimeApprovalDecisionApproved {
		t.Fatalf("decision retry changed winner: %#v, err %v", retry.Status, err)
	}

	expiring := kubernetesRuntimeApprovalFixture(t, "invocation-expiring", api.RuntimeApprovalDecisionPending)
	_, expiring, err = adapter.CreateOrGetRuntimeApproval(ctx, expiring, kubernetesRuntimeApprovalPayload, 29*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	expired, err := adapter.DecideRuntimeApproval(ctx, "team-a", "invocation-expiring", store.RuntimeApprovalDecision{
		Decision: api.RuntimeApprovalDecisionApproved, ExpectedResourceVersion: expiring.ResourceVersion,
		Issuer: "https://issuer.example", Subject: "operator-1",
	})
	if err != nil || expired.Status.DecisionState != api.RuntimeApprovalDecisionExpired {
		t.Fatalf("conservative boundary = %#v, err %v", expired.Status, err)
	}

	for _, test := range []struct {
		name string
		ttl  time.Duration
		want string
	}{
		{name: "at-skew-boundary", ttl: 30 * time.Second, want: api.RuntimeApprovalDecisionExpired},
		{name: "beyond-skew-boundary", ttl: 31 * time.Second, want: api.RuntimeApprovalDecisionApproved},
	} {
		t.Run(test.name, func(t *testing.T) {
			proposal := kubernetesRuntimeApprovalFixture(t, test.name, api.RuntimeApprovalDecisionPending)
			_, proposal, err = adapter.CreateOrGetRuntimeApproval(ctx, proposal, kubernetesRuntimeApprovalPayload, test.ttl)
			if err != nil {
				t.Fatal(err)
			}
			result, err := adapter.DecideRuntimeApproval(ctx, "team-a", test.name, store.RuntimeApprovalDecision{
				Decision: api.RuntimeApprovalDecisionApproved, ExpectedResourceVersion: proposal.ResourceVersion,
				Issuer: "https://issuer.example", Subject: "operator-1",
			})
			if err != nil || result.Status.DecisionState != test.want {
				t.Fatalf("decision at %s = %q, err %v; want %q", test.ttl, result.Status.DecisionState, err, test.want)
			}
		})
	}
}

func TestKubernetesRuntimeApprovalCreateRetriesTransientWorkspaceLeaseContention(t *testing.T) {
	ctx := context.Background()
	contendedClient := &runtimeApprovalTransientWorkspaceLeaseClient{
		Client:      newFakeClient(t),
		contentions: 1,
	}
	adapter := newRuntimeApprovalTestStore(t, contendedClient)
	adapter.ConfigureRuntimeApprovalBindingKeys(kubernetesRuntimeApprovalTestKeys(t))

	proposed := kubernetesRuntimeApprovalFixture(t, "transient-workspace-contention", api.RuntimeApprovalDecisionNotRequired)
	created, approval, err := adapter.CreateOrGetRuntimeApproval(ctx, proposed, kubernetesRuntimeApprovalPayload, 15*time.Minute)
	if err != nil {
		t.Fatalf("create after transient workspace Lease contention: %v", err)
	}
	if !created || approval.Name != runtimeapproval.CanonicalName("team-a", "transient-workspace-contention") {
		t.Fatalf("create = created %v, approval %#v", created, approval)
	}
}

func TestKubernetesRuntimeApprovalCreateRetriesTransientBindingLeaseContention(t *testing.T) {
	ctx := context.Background()
	base := newFakeClient(t)
	keys := kubernetesRuntimeApprovalTestKeys(t)
	initial := newRuntimeApprovalTestStore(t, base)
	if err := initial.ActivateRuntimeApprovalBindingKeys(ctx, keys); err != nil {
		t.Fatal(err)
	}
	contended := &runtimeApprovalTransientBindingLeaseClient{Client: base, contentions: 1}
	adapter := newRuntimeApprovalTestStore(t, contended)
	adapter.ConfigureRuntimeApprovalBindingKeys(keys)
	proposed := kubernetesRuntimeApprovalFixture(t, "transient-binding-contention", api.RuntimeApprovalDecisionNotRequired)
	created, approval, err := adapter.CreateOrGetRuntimeApproval(ctx, proposed, kubernetesRuntimeApprovalPayload, 15*time.Minute)
	if err != nil {
		t.Fatalf("create after transient binding Lease contention: %v", err)
	}
	if !created || approval.Name != runtimeapproval.CanonicalName("team-a", "transient-binding-contention") {
		t.Fatalf("create=%t approval=%+v", created, approval)
	}
}

func TestRuntimeApprovalWorkspaceLeaseUsesConfiguredAuthoritativeReader(t *testing.T) {
	ctx := context.Background()
	authoritativeClient := newFakeClient(t)
	adapter := NewStoreAdapter(&runtimeApprovalStaleWorkspaceLeaseClient{Client: authoritativeClient})
	adapter.ConfigureRuntimeApprovalReader(authoritativeClient)

	lease, err := adapter.acquireRuntimeApprovalWorkspaceLease(ctx, "team-a")
	if err != nil {
		t.Fatalf("acquire workspace Lease through authoritative reader: %v", err)
	}
	if err := adapter.releaseRuntimeApprovalWorkspaceLease(ctx, lease); err != nil {
		t.Fatalf("release workspace Lease through authoritative reader: %v", err)
	}
	current := &coordinationv1.Lease{}
	if err := authoritativeClient.Get(ctx, client.ObjectKey{Namespace: "team-a", Name: runtimeApprovalWorkspaceLeaseName}, current); err != nil {
		t.Fatal(err)
	}
	if current.Spec.HolderIdentity == nil || *current.Spec.HolderIdentity != "" {
		t.Fatalf("released workspace Lease holder = %v, want empty", current.Spec.HolderIdentity)
	}
}

func TestKubernetesRuntimeApprovalCreatesQuotaTicketBeforeLiveRecord(t *testing.T) {
	ctx := context.Background()
	base := newFakeClient(t)
	adapter := newRuntimeApprovalTestStore(t, &runtimeApprovalTicketOrderClient{Client: base, testing: t})
	adapter.ConfigureRuntimeApprovalBindingKeys(kubernetesRuntimeApprovalTestKeys(t))

	created, approval, err := adapter.CreateOrGetRuntimeApproval(ctx, kubernetesRuntimeApprovalFixture(t, "ticket-order", api.RuntimeApprovalDecisionPending), kubernetesRuntimeApprovalPayload, time.Minute)
	if err != nil || !created {
		t.Fatalf("CreateOrGetRuntimeApproval() = created %v, err %v", created, err)
	}
	ticket := &k8s.RuntimeApprovalQuotaTicket{}
	key := client.ObjectKey{Namespace: "team-a", Name: runtimeapproval.CanonicalName("team-a", "ticket-order")}
	if err := base.Get(ctx, key, ticket); err != nil {
		t.Fatal(err)
	}
	if ticket.Spec.RuntimeApprovalUID != approval.UID || ticket.Spec.Phase != k8s.RuntimeApprovalQuotaTicketPhaseLive || ticket.Spec.ChargedBytes <= 0 {
		t.Fatalf("quota ticket = %#v, approval UID = %q", ticket.Spec, approval.UID)
	}
	if !runtimeApprovalHasFinalizer(ticket, k8s.RuntimeApprovalQuotaTicketRetirementFinalizer) {
		t.Fatal("quota ticket lacks retirement finalizer")
	}
}

func TestKubernetesRuntimeApprovalQuotaRejectsOverLimitCreation(t *testing.T) {
	ctx := context.Background()
	adapter := newRuntimeApprovalTestStore(t, newFakeClient(t))
	adapter.ConfigureRuntimeApprovalBindingKeys(kubernetesRuntimeApprovalTestKeys(t))
	if err := adapter.ConfigureRuntimeApprovalQuota(RuntimeApprovalQuotaLimits{
		WorkspaceRecords: 1, WorkspaceBytes: 64 << 20,
		AgentRecords: 1, AgentBytes: 8 << 20,
	}); err != nil {
		t.Fatal(err)
	}
	if created, _, err := adapter.CreateOrGetRuntimeApproval(ctx, kubernetesRuntimeApprovalFixture(t, "quota-first", api.RuntimeApprovalDecisionPending), kubernetesRuntimeApprovalPayload, time.Minute); err != nil || !created {
		t.Fatalf("first create = created %v, err %v", created, err)
	}
	if _, _, err := adapter.CreateOrGetRuntimeApproval(ctx, kubernetesRuntimeApprovalFixture(t, "quota-second", api.RuntimeApprovalDecisionPending), kubernetesRuntimeApprovalPayload, time.Minute); !errors.Is(err, store.ErrQuotaExceeded) {
		t.Fatalf("second create error = %v, want %v", err, store.ErrQuotaExceeded)
	}
	second := &k8s.RuntimeApproval{}
	key := client.ObjectKey{Namespace: "team-a", Name: runtimeapproval.CanonicalName("team-a", "quota-second")}
	if err := adapter.client.Get(ctx, key, second); !apierrors.IsNotFound(err) {
		t.Fatalf("over-limit RuntimeApproval lookup error = %v, want not found", err)
	}
}

func TestKubernetesRuntimeApprovalClockHealthFailsClosed(t *testing.T) {
	adapter := NewStoreAdapter(newFakeClient(t))
	adapter.ConfigureRuntimeApprovalBindingKeys(kubernetesRuntimeApprovalTestKeys(t))
	if err := adapter.RuntimeApprovalClockReady(); err == nil {
		t.Fatal("omitted RuntimeApproval clock health passed readiness")
	}
	clockErr := errors.New("clock synchronization lost")
	if err := adapter.ConfigureRuntimeApprovalClock(defaultRuntimeApprovalClockSkew, func() error { return clockErr }); err != nil {
		t.Fatalf("ConfigureRuntimeApprovalClock() error = %v", err)
	}
	if err := adapter.RuntimeApprovalClockReady(); !errors.Is(err, clockErr) {
		t.Fatalf("RuntimeApprovalClockReady() error = %v, want %v", err, clockErr)
	}
	_, _, err := adapter.CreateOrGetRuntimeApproval(context.Background(), kubernetesRuntimeApprovalFixture(t, "unhealthy-clock", api.RuntimeApprovalDecisionPending), kubernetesRuntimeApprovalPayload, time.Minute)
	if !errors.Is(err, clockErr) {
		t.Fatalf("CreateOrGetRuntimeApproval() error = %v, want %v", err, clockErr)
	}
	if err := adapter.ConfigureRuntimeApprovalClock(defaultRuntimeApprovalClockSkew-time.Second, func() error { return nil }); err == nil {
		t.Fatal("ConfigureRuntimeApprovalClock() accepted a skew below the ADR minimum")
	}
}

func TestKubernetesRuntimeApprovalDecisionActorBounds(t *testing.T) {
	adapter := newRuntimeApprovalTestStore(t, newFakeClient(t))
	adapter.ConfigureRuntimeApprovalBindingKeys(kubernetesRuntimeApprovalTestKeys(t))
	_, approval, err := adapter.CreateOrGetRuntimeApproval(context.Background(), kubernetesRuntimeApprovalFixture(t, "actor-bounds", api.RuntimeApprovalDecisionPending), kubernetesRuntimeApprovalPayload, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	_, err = adapter.DecideRuntimeApproval(context.Background(), "team-a", "actor-bounds", store.RuntimeApprovalDecision{Decision: api.RuntimeApprovalDecisionApproved, ExpectedResourceVersion: approval.ResourceVersion, Issuer: "https://issuer.example", Subject: string(make([]byte, 513))})
	if err == nil {
		t.Fatal("oversized decision actor was accepted")
	}
}

func TestKubernetesRuntimeApprovalNotRequiredInitialState(t *testing.T) {
	adapter := newRuntimeApprovalTestStore(t, newFakeClient(t))
	adapter.ConfigureRuntimeApprovalBindingKeys(kubernetesRuntimeApprovalTestKeys(t))
	created, result, err := adapter.CreateOrGetRuntimeApproval(context.Background(), kubernetesRuntimeApprovalFixture(t, "not-required", api.RuntimeApprovalDecisionNotRequired), kubernetesRuntimeApprovalPayload, time.Minute)
	if err != nil || !created || result.Status.DecisionState != api.RuntimeApprovalDecisionNotRequired || result.Status.ExecutionState != api.RuntimeApprovalExecutionNotStarted {
		t.Fatalf("NotRequired create = created %v, state %#v, err %v", created, result.Status, err)
	}
	if result.Status.DecisionAudit == nil || result.Status.DecidedAt == nil || result.Status.DecisionAudit.Payload.Decision != api.RuntimeApprovalDecisionNotRequired {
		t.Fatalf("NotRequired decision evidence = %#v", result.Status)
	}
}

func TestKubernetesRuntimeApprovalConcurrentAuditAcknowledgementIsIdempotent(t *testing.T) {
	ctx := context.Background()
	adapter := newRuntimeApprovalTestStore(t, newFakeClient(t))
	adapter.ConfigureRuntimeApprovalBindingKeys(kubernetesRuntimeApprovalTestKeys(t))
	_, approval, err := adapter.CreateOrGetRuntimeApproval(ctx, kubernetesRuntimeApprovalFixture(t, "ack-race", api.RuntimeApprovalDecisionPending), kubernetesRuntimeApprovalPayload, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	approval, err = adapter.DecideRuntimeApproval(ctx, "team-a", "ack-race", store.RuntimeApprovalDecision{
		Decision: api.RuntimeApprovalDecisionApproved, ExpectedResourceVersion: approval.ResourceVersion,
		Issuer: "https://issuer.example", Subject: "operator-1",
	})
	if err != nil {
		t.Fatal(err)
	}

	start := make(chan struct{})
	results := make(chan error, 2)
	for range 2 {
		go func() {
			<-start
			_, err := adapter.AcknowledgeRuntimeApprovalDecisionAudit(ctx, "team-a", "ack-race", approval.ResourceVersion, approval.Status.DecisionAudit.ID)
			results <- err
		}()
	}
	close(start)
	for range 2 {
		if err := <-results; err != nil {
			t.Fatalf("concurrent acknowledgement returned %v", err)
		}
	}
}

func TestKubernetesRuntimeApprovalDecideAndExpireRaceConvergesOnExpired(t *testing.T) {
	ctx := context.Background()
	adapter := newRuntimeApprovalTestStore(t, newFakeClient(t))
	adapter.ConfigureRuntimeApprovalBindingKeys(kubernetesRuntimeApprovalTestKeys(t))
	now := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	adapter.now = func() time.Time { return now }
	_, approval, err := adapter.CreateOrGetRuntimeApproval(ctx, kubernetesRuntimeApprovalFixture(t, "decide-expire-race", api.RuntimeApprovalDecisionPending), kubernetesRuntimeApprovalPayload, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	adapter.now = func() time.Time { return now.Add(time.Minute) }
	start := make(chan struct{})
	results := make(chan api.RuntimeApproval, 2)
	errorsSeen := make(chan error, 2)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		result, err := adapter.DecideRuntimeApproval(ctx, "team-a", "decide-expire-race", store.RuntimeApprovalDecision{Decision: api.RuntimeApprovalDecisionApproved, ExpectedResourceVersion: approval.ResourceVersion, Issuer: "https://issuer.example", Subject: "operator-1"})
		results <- result
		errorsSeen <- err
	}()
	go func() {
		defer wg.Done()
		<-start
		result, err := adapter.ExpireRuntimeApprovalDecision(ctx, "team-a", "decide-expire-race", approval.ResourceVersion)
		results <- result
		errorsSeen <- err
	}()
	close(start)
	wg.Wait()
	for range 2 {
		if err := <-errorsSeen; err != nil {
			t.Fatalf("Decide/Expire race error: %v", err)
		}
		if result := <-results; result.Status.DecisionState != api.RuntimeApprovalDecisionExpired {
			t.Fatalf("Decide/Expire race state = %q", result.Status.DecisionState)
		}
	}
}

func TestKubernetesRuntimeApprovalDecisionResolvesSuccessfulAmbiguousWrite(t *testing.T) {
	ctx := context.Background()
	faultClient := &runtimeApprovalSuccessfulErrorClient{Client: newFakeClient(t)}
	adapter := newRuntimeApprovalTestStore(t, faultClient)
	adapter.ConfigureRuntimeApprovalBindingKeys(kubernetesRuntimeApprovalTestKeys(t))
	_, approval, err := adapter.CreateOrGetRuntimeApproval(ctx, kubernetesRuntimeApprovalFixture(t, "ambiguous-decision", api.RuntimeApprovalDecisionPending), kubernetesRuntimeApprovalPayload, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	faultClient.returnError = true
	result, err := adapter.DecideRuntimeApproval(ctx, "team-a", "ambiguous-decision", store.RuntimeApprovalDecision{
		Decision: api.RuntimeApprovalDecisionApproved, ExpectedResourceVersion: approval.ResourceVersion,
		Issuer: "https://issuer.example", Subject: "operator-1",
	})
	if err != nil || result.Status.DecisionState != api.RuntimeApprovalDecisionApproved {
		t.Fatalf("ambiguous successful decision = %#v, err %v", result.Status, err)
	}
}

func TestKubernetesRuntimeApprovalCreateResolvesSuccessfulAmbiguousStatusInitialization(t *testing.T) {
	ctx := context.Background()
	faultClient := &runtimeApprovalSuccessfulErrorClient{Client: newFakeClient(t), returnError: true}
	adapter := newRuntimeApprovalTestStore(t, faultClient)
	adapter.ConfigureRuntimeApprovalBindingKeys(kubernetesRuntimeApprovalTestKeys(t))
	created, result, err := adapter.CreateOrGetRuntimeApproval(ctx, kubernetesRuntimeApprovalFixture(t, "ambiguous-initialization", api.RuntimeApprovalDecisionPending), kubernetesRuntimeApprovalPayload, time.Minute)
	if err != nil || !created || result.Status.DecisionState != api.RuntimeApprovalDecisionPending || result.Status.ExecutionState != api.RuntimeApprovalExecutionNotStarted {
		t.Fatalf("ambiguous successful initialization = created %v, status %#v, err %v", created, result.Status, err)
	}
}

func TestKubernetesRuntimeApprovalLookupRecoversBlankInitialStatus(t *testing.T) {
	ctx := context.Background()
	faultClient := &runtimeApprovalSuccessfulErrorClient{Client: newFakeClient(t), failBefore: true}
	adapter := newRuntimeApprovalTestStore(t, faultClient)
	adapter.ConfigureRuntimeApprovalBindingKeys(kubernetesRuntimeApprovalTestKeys(t))
	if _, _, err := adapter.CreateOrGetRuntimeApproval(ctx, kubernetesRuntimeApprovalFixture(t, "lookup-recovers-initialization", api.RuntimeApprovalDecisionPending), kubernetesRuntimeApprovalPayload, time.Minute); err == nil {
		t.Fatal("create with interrupted status initialization unexpectedly succeeded")
	}
	result, err := adapter.GetRuntimeApprovalByInvocation(ctx, "team-a", "lookup-recovers-initialization")
	if err != nil || result.Status.DecisionState != api.RuntimeApprovalDecisionPending || result.Status.ExecutionState != api.RuntimeApprovalExecutionNotStarted {
		t.Fatalf("lookup recovery status = %#v, err %v", result.Status, err)
	}
}

func TestKubernetesRuntimeApprovalTransitionsRecoverBlankInitialStatus(t *testing.T) {
	ctx := context.Background()
	for _, test := range []struct {
		name            string
		initialDecision string
		transition      func(*StoreAdapter, int64) (api.RuntimeApproval, error)
	}{
		{name: "pending decision", initialDecision: api.RuntimeApprovalDecisionPending, transition: func(adapter *StoreAdapter, resourceVersion int64) (api.RuntimeApproval, error) {
			return adapter.DecideRuntimeApproval(ctx, "team-a", "recover-pending", store.RuntimeApprovalDecision{Decision: api.RuntimeApprovalDecisionApproved, ExpectedResourceVersion: resourceVersion, Issuer: "https://issuer.example", Subject: "operator-1"})
		}},
		{name: "not required expiry", initialDecision: api.RuntimeApprovalDecisionNotRequired, transition: func(adapter *StoreAdapter, resourceVersion int64) (api.RuntimeApproval, error) {
			return adapter.ExpireRuntimeApprovalDecision(ctx, "team-a", "recover-not-required", resourceVersion)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			invocationID := "recover-pending"
			if test.initialDecision == api.RuntimeApprovalDecisionNotRequired {
				invocationID = "recover-not-required"
			}
			sharedClient := newFakeClient(t)
			faultClient := &runtimeApprovalSuccessfulErrorClient{Client: sharedClient, failBefore: true}
			adapter := newRuntimeApprovalTestStore(t, faultClient)
			adapter.ConfigureRuntimeApprovalBindingKeys(kubernetesRuntimeApprovalTestKeys(t))
			if _, _, err := adapter.CreateOrGetRuntimeApproval(ctx, kubernetesRuntimeApprovalFixture(t, invocationID, test.initialDecision), kubernetesRuntimeApprovalPayload, time.Minute); err == nil {
				t.Fatal("create with interrupted status initialization unexpectedly succeeded")
			}
			object := &k8s.RuntimeApproval{}
			key := client.ObjectKey{Namespace: "team-a", Name: runtimeapproval.CanonicalName("team-a", invocationID)}
			if err := sharedClient.Get(ctx, key, object); err != nil {
				t.Fatal(err)
			}
			resourceVersion := convert.RuntimeApprovalFromK8s(object).ResourceVersion
			result, err := test.transition(adapter, resourceVersion)
			if err != nil {
				t.Fatal(err)
			}
			want := api.RuntimeApprovalDecisionApproved
			if test.initialDecision == api.RuntimeApprovalDecisionNotRequired {
				want = api.RuntimeApprovalDecisionNotRequired
			}
			if result.Status.DecisionState != want {
				t.Fatalf("recovered decision = %q, want %q", result.Status.DecisionState, want)
			}
		})
	}
}

func TestKubernetesRuntimeApprovalConcurrentCreateLoserAuthenticatesPayload(t *testing.T) {
	ctx := context.Background()
	sharedClient := newFakeClient(t)
	keys := kubernetesRuntimeApprovalTestKeys(t)
	winner := newRuntimeApprovalTestStore(t, sharedClient)
	winner.ConfigureRuntimeApprovalBindingKeys(keys)
	proposal := kubernetesRuntimeApprovalFixture(t, "concurrent-payload-mismatch", api.RuntimeApprovalDecisionPending)
	if created, _, err := winner.CreateOrGetRuntimeApproval(ctx, proposal, kubernetesRuntimeApprovalPayload, time.Minute); err != nil || !created {
		t.Fatalf("seed winner = created %v, err %v", created, err)
	}

	loser := newRuntimeApprovalTestStore(t, &runtimeApprovalInitialGetMissClient{Client: sharedClient, miss: true})
	loser.ConfigureRuntimeApprovalBindingKeys(keys)
	if _, _, err := loser.CreateOrGetRuntimeApproval(ctx, proposal, json.RawMessage(`{"argument":"different"}`), time.Minute); err == nil {
		t.Fatal("concurrent create loser accepted a payload that did not match the retained binding")
	}
}

func TestKubernetesRuntimeApprovalAmbiguousCreateRereadsPersistedObject(t *testing.T) {
	ctx := context.Background()
	invocationID := "ambiguous-create"
	base := newFakeClient(t)
	fault := &runtimeApprovalSuccessfulCreateErrorClient{
		Client: base, targetName: runtimeapproval.CanonicalName("team-a", invocationID), targetObject: &k8s.RuntimeApproval{},
	}
	adapter := NewStoreAdapter(fault)
	if err := adapter.ConfigureRuntimeApprovalClock(defaultRuntimeApprovalClockSkew, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	adapter.ConfigureRuntimeApprovalBindingKeys(kubernetesRuntimeApprovalTestKeys(t))
	created, approval, err := adapter.CreateOrGetRuntimeApproval(ctx, kubernetesRuntimeApprovalFixture(t, invocationID, api.RuntimeApprovalDecisionPending), kubernetesRuntimeApprovalPayload, time.Minute)
	if err != nil || created || approval.Status.DecisionState != api.RuntimeApprovalDecisionPending {
		t.Fatalf("ambiguous create = created %v, state %q, error %v", created, approval.Status.DecisionState, err)
	}
}

func TestKubernetesRuntimeApprovalFailsClosedWithoutBindingKeys(t *testing.T) {
	adapter := newRuntimeApprovalTestStore(t, newFakeClient(t))
	proposal := kubernetesRuntimeApprovalFixture(t, "missing-keys", api.RuntimeApprovalDecisionPending)
	if _, _, err := adapter.CreateOrGetRuntimeApproval(context.Background(), proposal, kubernetesRuntimeApprovalPayload, time.Minute); !errors.Is(err, runtimeapproval.ErrBindingKeyUnavailable) {
		t.Fatalf("missing key store error = %v", err)
	}
	proposal.Status.PreparationToken = "forged"
	if _, _, err := adapter.CreateOrGetRuntimeApproval(context.Background(), proposal, kubernetesRuntimeApprovalPayload, time.Minute); err == nil {
		t.Fatal("caller-owned preparation state was accepted")
	}
	adapter.ConfigureRuntimeApprovalBindingKeys(kubernetesRuntimeApprovalTestKeys(t))
	forged := kubernetesRuntimeApprovalFixture(t, "forged-binding", api.RuntimeApprovalDecisionPending)
	forged.Spec.Binding.Tag = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	if _, _, err := adapter.CreateOrGetRuntimeApproval(context.Background(), forged, kubernetesRuntimeApprovalPayload, time.Minute); err == nil {
		t.Fatal("forged binding tag was accepted for a new write")
	}
}

func TestKubernetesRuntimeApprovalReplayAuthenticatesRetainedVerifyOnlyKey(t *testing.T) {
	ctx := context.Background()
	adapter := newRuntimeApprovalTestStore(t, newFakeClient(t))
	adapter.ConfigureRuntimeApprovalBindingKeys(kubernetesRuntimeApprovalTestKeys(t))
	proposal := kubernetesRuntimeApprovalFixture(t, "rotated-replay", api.RuntimeApprovalDecisionPending)
	if created, _, err := adapter.CreateOrGetRuntimeApproval(ctx, proposal, kubernetesRuntimeApprovalPayload, time.Minute); err != nil || !created {
		t.Fatalf("create before rotation = %v, %v", created, err)
	}
	if err := adapter.ActivateRuntimeApprovalBindingKeys(ctx, kubernetesRuntimeApprovalRotatedTestKeys(t)); err != nil {
		t.Fatalf("activate rotated keys: %v", err)
	}
	if created, _, err := adapter.CreateOrGetRuntimeApproval(ctx, proposal, kubernetesRuntimeApprovalPayload, time.Minute); err != nil || created {
		t.Fatalf("replay after rotation = %v, %v", created, err)
	}
	fresh := kubernetesRuntimeApprovalFixture(t, "verify-only-new-write", api.RuntimeApprovalDecisionPending)
	if _, _, err := adapter.CreateOrGetRuntimeApproval(ctx, fresh, kubernetesRuntimeApprovalPayload, time.Minute); err == nil {
		t.Fatal("verify-only key accepted for a new write")
	}
	adapter.ConfigureRuntimeApprovalBindingKeys(kubernetesRuntimeApprovalTestKeys(t))
	if _, _, err := adapter.CreateOrGetRuntimeApproval(ctx, proposal, kubernetesRuntimeApprovalPayload, time.Minute); !errors.Is(err, runtimeapproval.ErrBindingKeyUnavailable) {
		t.Fatalf("local key rollback error = %v", err)
	}
}

func TestKubernetesRuntimeApprovalBindingKeyActivationIsMonotonicAndIdempotent(t *testing.T) {
	ctx := context.Background()
	adapter := newRuntimeApprovalTestStore(t, newFakeClient(t))
	adapter.ConfigureRuntimeApprovalBindingKeys(kubernetesRuntimeApprovalTestKeys(t))
	proposal := kubernetesRuntimeApprovalFixture(t, "activation-invariants", api.RuntimeApprovalDecisionPending)
	if created, _, err := adapter.CreateOrGetRuntimeApproval(ctx, proposal, kubernetesRuntimeApprovalPayload, time.Minute); err != nil || !created {
		t.Fatal(err)
	}
	rotated := kubernetesRuntimeApprovalRotatedTestKeys(t)
	if err := adapter.ActivateRuntimeApprovalBindingKeys(ctx, rotated); err != nil {
		t.Fatalf("activate epoch 2: %v", err)
	}
	if err := adapter.ActivateRuntimeApprovalBindingKeys(ctx, rotated); err != nil {
		t.Fatalf("idempotent activation: %v", err)
	}
	if err := adapter.ActivateRuntimeApprovalBindingKeys(ctx, kubernetesRuntimeApprovalTestKeys(t)); err == nil {
		t.Fatal("binding-key epoch rollback was accepted")
	}
	removed, err := runtimeapproval.NewStaticBindingKeyStoreAtEpoch(3, []runtimeapproval.BindingKey{{
		Version: 3, Algorithm: runtimeapproval.BindingAlgorithmV1, Domain: runtimeapproval.BindingDomainV1,
		Status: runtimeapproval.BindingKeyActive, Material: []byte("fedcba9876543210fedcba9876543210"),
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := adapter.ActivateRuntimeApprovalBindingKeys(ctx, removed); err == nil {
		t.Fatal("historical binding-key removal was accepted")
	}
	mutated, err := runtimeapproval.NewStaticBindingKeyStoreAtEpoch(3, []runtimeapproval.BindingKey{
		{Version: 1, Epoch: 1, Algorithm: runtimeapproval.BindingAlgorithmV1, Domain: runtimeapproval.BindingDomainV1, Status: runtimeapproval.BindingKeyVerifyOnly, Material: []byte("mutated-retained-material-00000000")},
		{Version: 2, Epoch: 2, Algorithm: runtimeapproval.BindingAlgorithmV1, Domain: runtimeapproval.BindingDomainV1, Status: runtimeapproval.BindingKeyVerifyOnly, Material: []byte("abcdef0123456789abcdef0123456789")},
		{Version: 3, Algorithm: runtimeapproval.BindingAlgorithmV1, Domain: runtimeapproval.BindingDomainV1, Status: runtimeapproval.BindingKeyActive, Material: []byte("fedcba9876543210fedcba9876543210")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := adapter.ActivateRuntimeApprovalBindingKeys(ctx, mutated); err == nil {
		t.Fatal("historical binding-key material mutation was accepted")
	}
	third, err := runtimeapproval.NewStaticBindingKeyStoreAtEpoch(3, []runtimeapproval.BindingKey{
		{Version: 1, Epoch: 1, Algorithm: runtimeapproval.BindingAlgorithmV1, Domain: runtimeapproval.BindingDomainV1, Status: runtimeapproval.BindingKeyVerifyOnly, Material: []byte("0123456789abcdef0123456789abcdef")},
		{Version: 2, Epoch: 2, Algorithm: runtimeapproval.BindingAlgorithmV1, Domain: runtimeapproval.BindingDomainV1, Status: runtimeapproval.BindingKeyVerifyOnly, Material: []byte("abcdef0123456789abcdef0123456789")},
		{Version: 3, Algorithm: runtimeapproval.BindingAlgorithmV1, Domain: runtimeapproval.BindingDomainV1, Status: runtimeapproval.BindingKeyActive, Material: []byte("fedcba9876543210fedcba9876543210")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := adapter.ActivateRuntimeApprovalBindingKeys(ctx, third); err != nil {
		t.Fatalf("activate third binding-key epoch: %v", err)
	}
	if created, replay, err := adapter.CreateOrGetRuntimeApproval(ctx, proposal, kubernetesRuntimeApprovalPayload, time.Minute); err != nil || created || replay.Spec.Binding.Epoch != 1 {
		t.Fatalf("epoch-one replay after third activation = created %v, binding %#v, err %v", created, replay.Spec.Binding, err)
	}
}

func TestKubernetesRuntimeApprovalBindingKeyReadinessRejectsMixedVersionsAndMaterial(t *testing.T) {
	ctx := context.Background()
	sharedClient := newFakeClient(t)
	active := NewStoreAdapter(sharedClient)
	if err := active.ActivateRuntimeApprovalBindingKeys(ctx, kubernetesRuntimeApprovalTestKeys(t)); err != nil {
		t.Fatal(err)
	}
	if err := active.ActivateRuntimeApprovalBindingKeys(ctx, kubernetesRuntimeApprovalRotatedTestKeys(t)); err != nil {
		t.Fatal(err)
	}

	oldReplica := NewStoreAdapter(sharedClient)
	oldReplica.ConfigureRuntimeApprovalBindingKeys(kubernetesRuntimeApprovalTestKeys(t))
	if err := oldReplica.RuntimeApprovalBindingKeysReady(ctx); !errors.Is(err, runtimeapproval.ErrBindingKeyUnavailable) {
		t.Fatalf("old-version readiness error = %v", err)
	}
	materialMismatch, err := runtimeapproval.NewStaticBindingKeyStoreAtEpoch(2, []runtimeapproval.BindingKey{
		{Version: 1, Epoch: 1, Algorithm: runtimeapproval.BindingAlgorithmV1, Domain: runtimeapproval.BindingDomainV1, Status: runtimeapproval.BindingKeyVerifyOnly, Material: []byte("0123456789abcdef0123456789abcdef")},
		{Version: 2, Algorithm: runtimeapproval.BindingAlgorithmV1, Domain: runtimeapproval.BindingDomainV1, Status: runtimeapproval.BindingKeyActive, Material: []byte("different-material-abcdef0123456789")},
	})
	if err != nil {
		t.Fatal(err)
	}
	mismatchedReplica := NewStoreAdapter(sharedClient)
	mismatchedReplica.ConfigureRuntimeApprovalBindingKeys(materialMismatch)
	if err := mismatchedReplica.RuntimeApprovalBindingKeysReady(ctx); !errors.Is(err, runtimeapproval.ErrBindingKeyUnavailable) {
		t.Fatalf("material-mismatch readiness error = %v", err)
	}
}

func TestKubernetesRuntimeApprovalBindingKeyBootstrapCreateRaceConverges(t *testing.T) {
	ctx := context.Background()
	sharedClient := newFakeClient(t)
	keys := kubernetesRuntimeApprovalTestKeys(t)
	winner := NewStoreAdapter(sharedClient)
	if err := winner.ActivateRuntimeApprovalBindingKeys(ctx, keys); err != nil {
		t.Fatal(err)
	}

	activationLoser := NewStoreAdapter(&runtimeApprovalInitialLeaseGetMissClient{Client: sharedClient, miss: true})
	if err := activationLoser.ActivateRuntimeApprovalBindingKeys(ctx, keys); err != nil {
		t.Fatalf("identical activation after lost bootstrap create race: %v", err)
	}
	if err := activationLoser.RuntimeApprovalBindingKeysReady(ctx); err != nil {
		t.Fatalf("activation loser readiness: %v", err)
	}

	acquisitionLoser := NewStoreAdapter(&runtimeApprovalInitialLeaseGetMissClient{Client: sharedClient, miss: true})
	lease, err := acquisitionLoser.acquireRuntimeApprovalBindingEpoch(ctx, keys)
	if err != nil {
		t.Fatalf("acquire after lost bootstrap create race: %v", err)
	}
	if err := acquisitionLoser.releaseRuntimeApprovalBindingEpoch(ctx, lease); err != nil {
		t.Fatalf("release recovered bootstrap Lease: %v", err)
	}
}

func TestKubernetesRuntimeApprovalBindingKeyAmbiguousBootstrapCreateConverges(t *testing.T) {
	ctx := context.Background()
	keys := kubernetesRuntimeApprovalTestKeys(t)

	activationClient := &runtimeApprovalSuccessfulCreateErrorClient{
		Client: newFakeClient(t), targetName: runtimeApprovalBindingLeaseName, targetObject: &coordinationv1.Lease{},
	}
	activation := NewStoreAdapter(activationClient)
	if err := activation.ActivateRuntimeApprovalBindingKeys(ctx, keys); err != nil {
		t.Fatalf("activation after lost create response: %v", err)
	}
	if err := activation.RuntimeApprovalBindingKeysReady(ctx); err != nil {
		t.Fatalf("activation readiness after lost create response: %v", err)
	}

	acquisitionClient := &runtimeApprovalSuccessfulCreateErrorClient{
		Client: newFakeClient(t), targetName: runtimeApprovalBindingLeaseName, targetObject: &coordinationv1.Lease{},
	}
	acquisition := NewStoreAdapter(acquisitionClient)
	acquisition.ConfigureRuntimeApprovalBindingKeys(keys)
	lease, err := acquisition.acquireRuntimeApprovalBindingEpoch(ctx, keys)
	if err != nil {
		t.Fatalf("acquire after lost create response: %v", err)
	}
	if err := acquisition.releaseRuntimeApprovalBindingEpoch(ctx, lease); err != nil {
		t.Fatalf("release ambiguously created Lease: %v", err)
	}
}

func TestKubernetesRuntimeApprovalBindingKeyExpiredLeaseIsReclaimed(t *testing.T) {
	ctx := context.Background()
	sharedClient := newFakeClient(t)
	keys := kubernetesRuntimeApprovalTestKeys(t)
	started := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
	first := NewStoreAdapter(sharedClient)
	first.now = func() time.Time { return started }
	lease, err := first.acquireRuntimeApprovalBindingEpoch(ctx, keys)
	if err != nil {
		t.Fatal(err)
	}

	second := NewStoreAdapter(sharedClient)
	second.now = func() time.Time { return started.Add(time.Duration(runtimeApprovalBindingLockSeconds+1) * time.Second) }
	reclaimed, err := second.acquireRuntimeApprovalBindingEpoch(ctx, keys)
	if err != nil {
		t.Fatalf("reclaim expired Lease: %v", err)
	}
	if reclaimed.Spec.HolderIdentity == nil || lease.Spec.HolderIdentity == nil || *reclaimed.Spec.HolderIdentity == *lease.Spec.HolderIdentity {
		t.Fatalf("expired Lease holder was not replaced: old %#v, new %#v", lease.Spec.HolderIdentity, reclaimed.Spec.HolderIdentity)
	}
	if err := second.releaseRuntimeApprovalBindingEpoch(ctx, reclaimed); err != nil {
		t.Fatalf("release reclaimed Lease: %v", err)
	}
}

func TestKubernetesRuntimeApprovalBindingKeyActivationFencesDelayedWriter(t *testing.T) {
	ctx := context.Background()
	sharedClient := newFakeClient(t)
	seed := NewStoreAdapter(sharedClient)
	if err := seed.ActivateRuntimeApprovalBindingKeys(ctx, kubernetesRuntimeApprovalTestKeys(t)); err != nil {
		t.Fatal(err)
	}
	blockingClient := &blockingBindingLeaseUpdateClient{Client: sharedClient, started: make(chan struct{}), release: make(chan struct{})}
	delayed := NewStoreAdapter(blockingClient)
	winner := NewStoreAdapter(sharedClient)
	delayedResult := make(chan error, 1)
	go func() {
		delayedResult <- delayed.ActivateRuntimeApprovalBindingKeys(ctx, kubernetesRuntimeApprovalRotatedTestKeys(t))
	}()
	<-blockingClient.started
	if err := winner.ActivateRuntimeApprovalBindingKeys(ctx, kubernetesRuntimeApprovalRotatedTestKeys(t)); err != nil {
		t.Fatalf("publish winning epoch: %v", err)
	}
	close(blockingClient.release)
	if err := <-delayedResult; !errors.Is(err, runtimeapproval.ErrBindingKeyUnavailable) {
		t.Fatalf("delayed activation error = %v", err)
	}
	lease := &coordinationv1.Lease{}
	if err := sharedClient.Get(ctx, seed.runtimeApprovalBindingLeaseKey(), lease); err != nil {
		t.Fatal(err)
	}
	state, err := bindingStateFromLease(lease)
	if err != nil {
		t.Fatal(err)
	}
	want, err := kubernetesBindingState(ctx, kubernetesRuntimeApprovalRotatedTestKeys(t))
	if err != nil {
		t.Fatal(err)
	}
	if !sameKubernetesBindingState(state, want) {
		t.Fatalf("durable binding state = %#v, want %#v", state, want)
	}
}

func TestKubernetesRuntimeApprovalBindingKeyLeaseFailsClosedWhileHeld(t *testing.T) {
	ctx := context.Background()
	sharedClient := newFakeClient(t)
	first := newRuntimeApprovalTestStore(t, sharedClient)
	second := newRuntimeApprovalTestStore(t, sharedClient)
	keys := kubernetesRuntimeApprovalTestKeys(t)
	first.ConfigureRuntimeApprovalBindingKeys(keys)
	second.ConfigureRuntimeApprovalBindingKeys(keys)
	lease, err := first.acquireRuntimeApprovalBindingEpoch(ctx, keys)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := first.releaseRuntimeApprovalBindingEpoch(ctx, lease); err != nil {
			t.Errorf("release binding-key Lease: %v", err)
		}
	}()
	proposal := kubernetesRuntimeApprovalFixture(t, "held-lease", api.RuntimeApprovalDecisionPending)
	if _, _, err := second.CreateOrGetRuntimeApproval(ctx, proposal, kubernetesRuntimeApprovalPayload, time.Minute); !errors.Is(err, runtimeapproval.ErrBindingKeyUnavailable) {
		t.Fatalf("create under held Lease error = %v", err)
	}
	if err := second.ActivateRuntimeApprovalBindingKeys(ctx, kubernetesRuntimeApprovalRotatedTestKeys(t)); !errors.Is(err, runtimeapproval.ErrBindingKeyUnavailable) {
		t.Fatalf("activation under held Lease error = %v", err)
	}
}

var kubernetesRuntimeApprovalPayload = json.RawMessage(`{"argument":"sensitive"}`)

func TestKubernetesRuntimeApprovalExecutorContract(t *testing.T) {
	storage := newRuntimeApprovalTestStore(t, newFakeClient(t))
	keys := kubernetesRuntimeApprovalTestKeys(t)
	storage.ConfigureRuntimeApprovalBindingKeys(keys)
	proposed, payload, terminal := conformancetest.RunExecutorApprovalContract(t, storage, keys)
	object := convert.RuntimeApprovalToK8s(&terminal)
	tombstone, err := storage.CreateOrVerifyRuntimeApprovalTombstone(context.Background(), object, DefaultRuntimeApprovalTombstoneRetention)
	if err != nil || tombstone.Spec.ActorKind != api.RuntimeApprovalActorExecutor || tombstone.Spec.ExecutorAction == nil {
		t.Fatalf("executor replay fence lost its actor: %v", err)
	}
	object.Finalizers = nil
	if err := storage.client.Update(context.Background(), object); err != nil {
		t.Fatal(err)
	}
	if err := storage.client.Delete(context.Background(), object); err != nil {
		t.Fatal(err)
	}
	created, replay, err := storage.CreateOrGetRuntimeApproval(context.Background(), proposed, payload, time.Hour)
	if err != nil || created || replay.UID != terminal.UID || replay.Status.ExecutionState != api.RuntimeApprovalExecutionCompleted {
		t.Fatalf("compacted executor replay reopened: created=%v error=%v", created, err)
	}
	retained, err := storage.GetRuntimeApprovalByInvocation(context.Background(), proposed.Spec.Workspace, proposed.Spec.InvocationID)
	if err != nil || retained.Spec.ActorKind != api.RuntimeApprovalActorExecutor || retained.Spec.ExecutorAction == nil {
		t.Fatalf("executor tombstone read lost actor: %v", err)
	}
}

func TestKubernetesRuntimeApprovalWorkflowContextPersistenceAndReplay(t *testing.T) {
	storage := newRuntimeApprovalTestStore(t, newFakeClient(t))
	keys := kubernetesRuntimeApprovalTestKeys(t)
	storage.ConfigureRuntimeApprovalBindingKeys(keys)
	proposed := kubernetesRuntimeApprovalFixture(t, "workflow-context", api.RuntimeApprovalDecisionPending)
	proposed.Spec.WorkflowContext = &api.WorkflowActionContext{Workspace: "origin", RunUID: "run-uid", Step: "team",
		ActivationID: "visit-1", Attempt: 1, OwnerEpoch: 1, BindingDigest: strings.Repeat("a", 64)}
	binding, err := runtimeapproval.Bind(context.Background(), keys, runtimeapproval.ActionEnvelopeFromApproval(proposed, kubernetesRuntimeApprovalPayload))
	if err != nil {
		t.Fatal(err)
	}
	proposed.Spec.Binding = binding
	created, first, err := storage.CreateOrGetRuntimeApproval(context.Background(), proposed, kubernetesRuntimeApprovalPayload, time.Minute)
	if err != nil || !created || !reflect.DeepEqual(first.Spec.WorkflowContext, proposed.Spec.WorkflowContext) {
		t.Fatalf("Workflow context creation: %v %+v %v", created, first.Spec.WorkflowContext, err)
	}
	retained, err := storage.GetRuntimeApprovalByInvocation(context.Background(), proposed.Spec.Workspace, proposed.Spec.InvocationID)
	if err != nil || !reflect.DeepEqual(retained.Spec.WorkflowContext, proposed.Spec.WorkflowContext) {
		t.Fatalf("Workflow context read: %+v %v", retained.Spec.WorkflowContext, err)
	}
	created, replay, err := storage.CreateOrGetRuntimeApproval(context.Background(), proposed, kubernetesRuntimeApprovalPayload, time.Minute)
	if err != nil || created || replay.UID != first.UID {
		t.Fatalf("exact Workflow replay: %v %+v %v", created, replay, err)
	}
	changed := proposed
	changedContext := *proposed.Spec.WorkflowContext
	changedContext.OwnerEpoch++
	changed.Spec.WorkflowContext = &changedContext
	if _, _, err := storage.CreateOrGetRuntimeApproval(context.Background(), changed, kubernetesRuntimeApprovalPayload, time.Minute); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("changed Workflow epoch accepted: %v", err)
	}
}

func newRuntimeApprovalTestStore(t *testing.T, c client.Client) *StoreAdapter {
	t.Helper()
	adapter := NewStoreAdapter(c)
	if err := adapter.ConfigureRuntimeApprovalClock(defaultRuntimeApprovalClockSkew, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	return adapter
}

func kubernetesRuntimeApprovalFixture(t *testing.T, invocationID, decision string) api.RuntimeApproval {
	t.Helper()
	ref := func(kind, name, uid string) api.RuntimeApprovalReference {
		return api.RuntimeApprovalReference{Kind: kind, Scope: "Namespaced", Namespace: "team-a", Name: name, UID: uid, ResourceVersion: 1}
	}
	approval := api.RuntimeApproval{
		Spec: api.RuntimeApprovalSpec{
			Workspace: "team-a", InvocationID: invocationID, Plane: api.RuntimeApprovalPlaneTool, Operation: "invoke",
			AgentRef: ref("Agent", "agent-a", "agent-uid"), AgentIdentityRef: ref("AgentIdentity", "agent-a", "identity-uid"),
			AgentInstanceRef: ref("AgentInstance", "agent-a-1", "instance-uid"), SessionRef: "session-a",
			TargetRef: ref("Tool", "tool-a", "tool-uid"), AutonomyClassRef: api.RuntimeApprovalReference{Kind: "AutonomyClass", Scope: "Cluster", Name: "supervised", UID: "class-uid", ResourceVersion: 1},
		},
		Status: api.RuntimeApprovalStatus{DecisionState: decision, ExecutionState: api.RuntimeApprovalExecutionNotStarted},
	}
	binding, err := runtimeapproval.Bind(context.Background(), kubernetesRuntimeApprovalTestKeys(t), runtimeapproval.ActionEnvelopeFromApproval(approval, kubernetesRuntimeApprovalPayload))
	if err != nil {
		t.Fatal(err)
	}
	approval.Spec.Binding = binding
	return approval
}

func kubernetesRuntimeApprovalTestKeys(t *testing.T) runtimeapproval.BindingKeyStore {
	t.Helper()
	keys, err := runtimeapproval.NewStaticBindingKeyStore([]runtimeapproval.BindingKey{{
		Version: 1, Algorithm: runtimeapproval.BindingAlgorithmV1, Domain: runtimeapproval.BindingDomainV1,
		Status: runtimeapproval.BindingKeyActive, Material: []byte("0123456789abcdef0123456789abcdef"),
	}})
	if err != nil {
		t.Fatal(err)
	}
	return keys
}

func kubernetesRuntimeApprovalRotatedTestKeys(t *testing.T) runtimeapproval.BindingKeyStore {
	t.Helper()
	keys, err := runtimeapproval.NewStaticBindingKeyStoreAtEpoch(2, []runtimeapproval.BindingKey{
		{Version: 1, Epoch: 1, Algorithm: runtimeapproval.BindingAlgorithmV1, Domain: runtimeapproval.BindingDomainV1, Status: runtimeapproval.BindingKeyVerifyOnly, Material: []byte("0123456789abcdef0123456789abcdef")},
		{Version: 2, Algorithm: runtimeapproval.BindingAlgorithmV1, Domain: runtimeapproval.BindingDomainV1, Status: runtimeapproval.BindingKeyActive, Material: []byte("abcdef0123456789abcdef0123456789")},
	})
	if err != nil {
		t.Fatal(err)
	}
	return keys
}
