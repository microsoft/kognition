// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	k8s "github.com/kognition-ai/kognition/api/k8s/v1alpha1"
	api "github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/auth"
	"github.com/kognition-ai/kognition/internal/convert"
	"github.com/kognition-ai/kognition/internal/runtimeapproval"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kognition-ai/kognition/internal/store"
	"github.com/kognition-ai/kognition/internal/store/workflowactivationtest"
)

func TestKubernetesWorkflowActivationStoreContract(test *testing.T) {
	workflowactivationtest.Run(test, func(test *testing.T) (store.WorkflowActivationStore, store.WorkflowActivationStore) {
		shared := newFakeClient(test)
		clock := func() (time.Time, error) { return time.Now().UTC(), nil }
		first, err := NewWorkflowActivationGateStore(shared, shared, "kognition-control", clock)
		if err != nil {
			test.Fatal(err)
		}
		second, err := NewWorkflowActivationGateStore(shared, shared, "kognition-control", clock)
		if err != nil {
			test.Fatal(err)
		}
		return first, second
	})
}

func TestKubernetesWorkflowExecutorClockFailsClosed(t *testing.T) {
	adapter := NewStoreAdapter(newFakeClient(t))
	ctx := context.Background()
	if _, err := adapter.WorkflowExecutorClock(ctx); err == nil {
		t.Fatal("unconfigured executor clock accepted")
	}
	cause := errors.New("clock unhealthy")
	if err := adapter.ConfigureRuntimeApprovalClock(defaultRuntimeApprovalClockSkew, func() error { return cause }); err != nil {
		t.Fatal(err)
	}
	if _, err := adapter.WorkflowExecutorClock(ctx); !errors.Is(err, cause) {
		t.Fatalf("executor clock ignored health failure: %v", err)
	}
	if err := adapter.ConfigureRuntimeApprovalClock(defaultRuntimeApprovalClockSkew, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	adapter.now = func() time.Time { return now }
	if actual, err := adapter.WorkflowExecutorClock(ctx); err != nil || !actual.Equal(now) {
		t.Fatalf("healthy clock rejected: %v %v", actual, err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := adapter.WorkflowExecutorClock(canceled); !errors.Is(err, context.Canceled) {
		t.Fatalf("clock ignored cancellation: %v", err)
	}
	adapter.now = func() time.Time { return time.Time{} }
	if _, err := adapter.WorkflowExecutorClock(ctx); err == nil {
		t.Fatal("zero executor clock accepted")
	}
}

func TestKubernetesWorkflowActivationStoreRequiresTrustedClock(test *testing.T) {
	shared := newFakeClient(test)
	if _, err := NewWorkflowActivationGateStore(shared, shared, "control", nil); !errors.Is(err, store.ErrConflict) {
		test.Fatal("missing clock boundary accepted")
	}
	cause := errors.New("clock unhealthy")
	storage, err := NewWorkflowActivationGateStore(shared, shared, "control", func() (time.Time, error) { return time.Time{}, cause })
	if err != nil {
		test.Fatal(err)
	}
	if _, err := storage.CreateWorkflowActivation(context.Background(), store.WorkflowActivationSpec{}); !errors.Is(err, cause) {
		test.Fatal("clock health failure was ignored")
	}
}

type workflowExecutorUIDTestClient struct{ client.Client }

type workflowExecutorMetadataTestReader struct {
	client.Reader
	mutate func(*corev1.ConfigMap)
}

func (reader workflowExecutorMetadataTestReader) Get(ctx context.Context, key client.ObjectKey, object client.Object, options ...client.GetOption) error {
	if err := reader.Reader.Get(ctx, key, object, options...); err != nil {
		return err
	}
	if record, ok := object.(*corev1.ConfigMap); ok {
		reader.mutate(record)
	}
	return nil
}

func (writer workflowExecutorUIDTestClient) Create(ctx context.Context, object client.Object, options ...client.CreateOption) error {
	object.SetUID(types.UID(uuid.NewString()))
	return writer.Client.Create(ctx, object, options...)
}

type workflowSubmissionEvidenceTestAuthorizer struct {
	acceptance runtimeapproval.WorkflowRunAcceptance
	err        error
	calls      int
	failOnCall int
}

func (authorizer *workflowSubmissionEvidenceTestAuthorizer) AuthorizeAcceptance(context.Context, []byte) (runtimeapproval.WorkflowRunAcceptance, error) {
	authorizer.calls++
	if authorizer.calls == authorizer.failOnCall {
		return runtimeapproval.WorkflowRunAcceptance{}, errors.New("final Workflow/use revoked")
	}
	return authorizer.acceptance, authorizer.err
}

func TestKubernetesProtectedExecutorRecords(t *testing.T) {
	ctx := context.Background()
	shared := newFakeClient(t)
	writer := workflowExecutorUIDTestClient{shared}
	now := time.Now().UTC().Add(-time.Second)
	storage, err := NewWorkflowExecutorRecordStore(writer, shared, "control", func() (time.Time, error) { return now, nil })
	if err != nil {
		t.Fatal(err)
	}
	reference := func(kind, name, uid string) api.RuntimeApprovalReference {
		return api.RuntimeApprovalReference{Kind: kind, Scope: "Namespaced", Namespace: "team-a", Name: name, UID: uid, ResourceVersion: 1}
	}
	authority := runtimeapproval.WorkflowExecutorAuthority{PolicyVersion: runtimeapproval.WorkflowExecutorPolicyVersion, Reference: reference("WorkflowExecutionBinding", "approved", "caller-uid"),
		Workflow: reference("Workflow", "investigate", "workflow-uid"), WorkflowRevisionDigest: strings.Repeat("a", 64),
		Executor:       api.ControlPlaneRoleSubject{Method: "tokenreview", Issuer: "kubernetes.io/tokenreview", Subject: "executor"},
		ApprovedBy:     api.ControlPlaneRoleSubject{Method: "tokenreview", Issuer: "kubernetes.io/tokenreview", Subject: "operator"},
		ControllerUIDs: []string{"controller-uid"}, Enabled: true, NotBefore: now.Add(-time.Minute), ExpiresAt: now.Add(time.Hour),
		Limits: runtimeapproval.WorkflowExecutorLimits{MaxEffects: 8, MaxPayloadBytes: 32768, MaxDurationSeconds: 3600, MaxMembers: 8, MaxTurns: 100, MaxMessages: 500, MaxDelegations: 100, MaxHandoffDepth: 5, MaxParallelTasks: 8, ContentRetentionSeconds: 604800, ReplayRetentionSeconds: 7776000},
		Targets: []runtimeapproval.WorkflowExecutorTarget{{Agent: reference("Agent", "analyst", "agent-uid"),
			Service: reference("AgentService", "analyst", "service-uid"), Operation: runtimeapproval.WorkflowExecutorInitiate, InitialDecision: api.RuntimeApprovalDecisionPending}}}
	workflow := &k8s.Workflow{ObjectMeta: metav1.ObjectMeta{Name: authority.Workflow.Name, Namespace: authority.Workflow.Namespace}}
	if err := writer.Create(ctx, workflow); err != nil {
		t.Fatal(err)
	}
	authority.Workflow.UID = string(workflow.UID)
	workflowJSON, err := json.Marshal(convert.WorkflowFromK8s(workflow))
	if err != nil {
		t.Fatal(err)
	}
	authority.WorkflowRevisionDigest, err = runtimeapproval.WorkflowExecutorSpecDigest(workflowJSON)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"uid", "version", "digest"} {
		forged := authority
		switch field {
		case "uid":
			forged.Workflow.UID = "invented"
		case "version":
			forged.Workflow.ResourceVersion++
		case "digest":
			forged.WorkflowRevisionDigest = strings.Repeat("d", 64)
		}
		if _, err := storage.CreateWorkflowExecutorAuthority(ctx, forged); !errors.Is(err, store.ErrConflict) {
			t.Fatalf("binding with invented Workflow %s accepted: %v", field, err)
		}
	}
	created, err := storage.CreateWorkflowExecutorAuthority(ctx, authority)
	if err != nil {
		t.Fatal(err)
	}
	if created.Reference.UID == "caller-uid" || created.Reference.ResourceVersion <= 0 {
		t.Fatal("binding did not use persisted native identity")
	}
	retained, err := storage.GetWorkflowExecutorAuthority(ctx, created.Reference)
	if err != nil || !reflect.DeepEqual(created, retained) {
		t.Fatalf("native binding round trip: %+v %v", retained, err)
	}
	if _, err := storage.CreateWorkflowExecutorAuthority(ctx, authority); !errors.Is(err, store.ErrAlreadyExists) {
		t.Fatalf("binding duplicate: %v", err)
	}
	run := &k8s.WorkflowRun{ObjectMeta: metav1.ObjectMeta{Name: "protected-run", Namespace: "team-a", Annotations: map[string]string{runtimeapproval.WorkflowRunAcceptanceRequiredAnnotation: "true"}}}
	if err := json.Unmarshal([]byte(`{"spec":{"workflowRef":{"name":"investigate"}}}`), run); err != nil {
		t.Fatal(err)
	}
	if err := writer.Create(ctx, run); err != nil {
		t.Fatal(err)
	}
	runReference := reference("WorkflowRun", run.Name, string(run.UID))
	acceptance := runtimeapproval.WorkflowRunAcceptance{Workspace: run.Namespace, RunUID: string(run.UID), Run: runReference,
		Workflow: created.Workflow, WorkflowRevisionDigest: created.WorkflowRevisionDigest, ExecutionBinding: created.Reference,
		Executor: created.Executor, Initiator: api.ControlPlaneRoleSubject{Method: "oidc", Issuer: "https://issuer.example", Subject: "submitter"},
		BindingDigest: strings.Repeat("b", 64), RequestDigest: strings.Repeat("c", 64), AcceptedAt: now, ExpiresAt: now.Add(30 * time.Minute)}
	runJSON, err := json.Marshal(convert.WorkflowRunFromK8s(run))
	if err != nil {
		t.Fatal(err)
	}
	acceptance.RequestDigest, err = runtimeapproval.WorkflowExecutorSpecDigest(runJSON)
	if err != nil {
		t.Fatal(err)
	}
	acceptance.BindingDigest, err = runtimeapproval.WorkflowRunExecutionScopeDigest(acceptance, created)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := storage.FinalizeWorkflowRunSubmission(context.Background(), run.Namespace, run.Name); !errors.Is(err, runtimeapproval.ErrWorkflowDispatchUnsupported) {
		t.Fatalf("unconfigured native finalization accepted: %v", err)
	}
	evidence := &workflowSubmissionEvidenceTestAuthorizer{acceptance: acceptance}
	finalizerPrincipal, finalizerUID := created.Executor, "controller-uid"
	if err := storage.ConfigureWorkflowRunSubmission(evidence, func(context.Context) (api.ControlPlaneRoleSubject, string, error) {
		return finalizerPrincipal, finalizerUID, nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, scenario := range []string{"unverified controller", "wrong executor", "revoked use", "changed request", "final use revoked"} {
		evidence.calls = 0
		switch scenario {
		case "unverified controller":
			finalizerUID = "unapproved-controller"
		case "wrong executor":
			finalizerPrincipal.Subject = "another-executor"
		case "revoked use":
			evidence.err = errors.New("submitter use revoked")
		case "changed request":
			evidence.acceptance.RequestDigest = strings.Repeat("d", 64)
		case "final use revoked":
			evidence.failOnCall = 2
		}
		if _, err := storage.FinalizeWorkflowRunSubmission(context.Background(), run.Namespace, run.Name); err == nil {
			t.Fatalf("invalid native finalization allowed: %s", scenario)
		}
		if _, err := storage.GetWorkflowRunAcceptance(context.Background(), run.Namespace, string(run.UID)); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("invalid finalization persisted acceptance: %s %v", scenario, err)
		}
		finalizerPrincipal, finalizerUID, evidence.err, evidence.acceptance = created.Executor, "controller-uid", nil, acceptance
		evidence.failOnCall = 0
	}
	evidence.calls = 0
	reconciler := NewWorkflowRunSubmissionReconciler(shared, storage)
	request := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: run.Namespace, Name: run.Name}}
	if result, err := reconciler.Reconcile(context.Background(), request); err != nil || result.Requeue || result.RequeueAfter != 0 { //nolint:staticcheck // Verify controller-runtime 1.x immediate requeue compatibility.
		t.Fatalf("protected submission reconciliation failed: %+v %v", result, err)
	}
	if actual, err := storage.GetWorkflowRunAcceptance(context.Background(), run.Namespace, string(run.UID)); err != nil || !reflect.DeepEqual(actual, acceptance) || evidence.calls != 2 {
		t.Fatalf("native finalization failed without manufactured submitter context: %+v %v calls=%d", actual, err, evidence.calls)
	}
	evidence.err = errors.New("submitter use revoked after acceptance")
	if result, err := reconciler.Reconcile(context.Background(), request); err != nil || result.Requeue || result.RequeueAfter != 0 || evidence.calls != 2 { //nolint:staticcheck // Verify controller-runtime 1.x immediate requeue compatibility.
		t.Fatalf("retained acceptance reused submitter authority: %+v %v calls=%d", result, err, evidence.calls)
	}
	unchanged := &k8s.WorkflowRun{}
	if err := shared.Get(context.Background(), request.NamespacedName, unchanged); err != nil ||
		!reflect.DeepEqual(unchanged.ObjectMeta, run.ObjectMeta) || !reflect.DeepEqual(unchanged.Spec, run.Spec) || !reflect.DeepEqual(unchanged.Status, run.Status) {
		t.Fatalf("acceptance-only reconciliation changed public execution state: %+v %v", unchanged, err)
	}
	jobs := &k8s.AgentJobList{}
	if err := shared.List(context.Background(), jobs, client.InNamespace(run.Namespace)); err != nil || len(jobs.Items) != 0 {
		t.Fatalf("acceptance-only reconciliation dispatched legacy jobs: %+v %v", jobs.Items, err)
	}
	evidence.err = nil
	ctx = auth.WithIdentity(ctx, &auth.Identity{Method: acceptance.Initiator.Method, Issuer: acceptance.Initiator.Issuer, Subject: acceptance.Initiator.Subject})
	forgedDigest := acceptance
	forgedDigest.RequestDigest = strings.Repeat("d", 64)
	if _, err := storage.CreateWorkflowRunAcceptance(ctx, forgedDigest); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("forged digest accepted: %v", err)
	}
	if _, err := storage.CreateWorkflowRunAcceptance(ctx, acceptance); err != nil {
		t.Fatal(err)
	}
	if actual, err := storage.CreateWorkflowRunAcceptance(ctx, acceptance); err != nil || !reflect.DeepEqual(actual, acceptance) {
		t.Fatalf("acceptance replay: %+v %v", actual, err)
	}
	for name, mutate := range map[string]func(*corev1.ConfigMap){
		"wrong container namespace":   func(record *corev1.ConfigMap) { record.Namespace = "other" },
		"wrong container name":        func(record *corev1.ConfigMap) { record.Name = "other" },
		"missing persisted UID":       func(record *corev1.ConfigMap) { record.UID = "" },
		"missing persisted version":   func(record *corev1.ConfigMap) { record.ResourceVersion = "" },
		"malformed persisted version": func(record *corev1.ConfigMap) { record.ResourceVersion = "invalid" },
		"extra payload":               func(record *corev1.ConfigMap) { record.Data["other.json"] = "{}" },
		"binary payload":              func(record *corev1.ConfigMap) { record.BinaryData = map[string][]byte{"other": {1}} },
		"wrong immutability": func(record *corev1.ConfigMap) {
			immutable := record.Immutable == nil || !*record.Immutable
			record.Immutable = &immutable
		},
	} {
		t.Run(name, func(t *testing.T) {
			malformed, err := NewWorkflowExecutorRecordStore(writer, workflowExecutorMetadataTestReader{Reader: shared, mutate: mutate}, "control", func() (time.Time, error) { return now, nil })
			if err != nil {
				t.Fatal(err)
			}
			if _, err := malformed.GetWorkflowExecutorAuthority(ctx, created.Reference); !errors.Is(err, store.ErrConflict) {
				t.Fatalf("malformed binding metadata accepted: %v", err)
			}
			if _, err := malformed.GetWorkflowRunAcceptance(ctx, acceptance.Workspace, acceptance.RunUID); !errors.Is(err, store.ErrConflict) {
				t.Fatalf("malformed acceptance metadata accepted: %v", err)
			}
		})
	}
	forged := acceptance
	forged.Initiator.Subject = "forger"
	if _, err := storage.CreateWorkflowRunAcceptance(ctx, forged); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("acceptance substitution: %v", err)
	}
	revoked := created
	revoked.Enabled = false
	updated, err := storage.UpdateWorkflowExecutorAuthority(ctx, revoked)
	if err != nil || updated.Enabled || updated.Reference.ResourceVersion <= created.Reference.ResourceVersion {
		t.Fatalf("revocation: %+v %v", updated, err)
	}
	if _, err := storage.GetWorkflowExecutorAuthority(ctx, created.Reference); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("old binding remained live: %v", err)
	}
	if _, err := storage.UpdateWorkflowExecutorAuthority(ctx, revoked); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("stale update: %v", err)
	}
}

type workflowActivationLostAcknowledgementClient struct {
	client.Client
	cause error
}

func (writer *workflowActivationLostAcknowledgementClient) Update(ctx context.Context, object client.Object, options ...client.UpdateOption) error {
	if err := writer.Client.Update(ctx, object, options...); err != nil {
		return err
	}
	return writer.cause
}

func TestKubernetesWorkflowActivationLostAcknowledgementCannotReplay(test *testing.T) {
	shared := newFakeClient(test)
	clock := func() (time.Time, error) { return time.Now().UTC(), nil }
	storage, err := NewWorkflowActivationGateStore(shared, shared, "control", clock)
	if err != nil {
		test.Fatal(err)
	}
	gate, err := storage.CreateWorkflowActivation(context.Background(), store.WorkflowActivationSpec{
		Key:           store.WorkflowActivationKey{Workspace: "work", RunUID: "run", Step: "team", ActivationID: "visit", Attempt: 1},
		BindingDigest: strings.Repeat("a", 64), Actors: []string{strings.Repeat("b", 64)}, MaxEffects: 2, MaxBytes: 16384, ExpiresAt: time.Now().Add(time.Hour),
	})
	if err != nil {
		test.Fatal(err)
	}
	effect := store.WorkflowEffect{ID: "invoke", BindingDigest: strings.Repeat("c", 64), Actor: strings.Repeat("b", 64), Plane: "model", Epoch: 1}
	registered, err := storage.MutateWorkflowActivation(context.Background(), gate.Spec.Key, store.WorkflowActivationMutation{ExpectedVersion: gate.Version, Kind: store.WorkflowGateRegister, Effect: effect})
	if err != nil {
		test.Fatal(err)
	}
	cause := errors.New("commit acknowledgement lost")
	writer := &workflowActivationLostAcknowledgementClient{Client: shared, cause: cause}
	ambiguous, err := NewWorkflowActivationGateStore(writer, shared, "control", clock)
	if err != nil {
		test.Fatal(err)
	}
	claim := store.WorkflowEffectClaim{Kind: "ModelDispatch", Namespace: "worker", UID: "dispatch", Claimant: "gateway", ProcessNonce: "nonce", FencingToken: 1}
	mutation := store.WorkflowActivationMutation{ExpectedVersion: registered.Activation.Version, Kind: store.WorkflowGateCommit, Effect: effect, Claim: claim}
	result, err := ambiguous.MutateWorkflowActivation(context.Background(), gate.Spec.Key, mutation)
	if !errors.Is(err, cause) || result.DispatchGranted {
		test.Fatal("ambiguous write exposed dispatch authority")
	}
	retained, err := storage.GetWorkflowActivation(context.Background(), gate.Spec.Key)
	if err != nil || retained.Effects[effect.ID].State != store.WorkflowEffectCommitted {
		test.Fatalf("committed evidence missing: %+v %v", retained, err)
	}
	mutation.ExpectedVersion = retained.Version
	repeated, err := storage.MutateWorkflowActivation(context.Background(), gate.Spec.Key, mutation)
	if err != nil || repeated.DispatchGranted || repeated.Changed {
		test.Fatal("lost acknowledgement retry created new dispatch authority")
	}
	object := &corev1.ConfigMap{}
	if err := shared.Get(context.Background(), client.ObjectKey{Namespace: "control", Name: workflowActivationObjectName(gate.Spec.Key)}, object); err != nil {
		test.Fatal(err)
	}
	retained.Mode = "ForgedMode"
	encoded, err := json.Marshal(retained)
	if err != nil {
		test.Fatal(err)
	}
	object.Data["activation.json"] = string(encoded)
	if err := shared.Update(context.Background(), object); err != nil {
		test.Fatal(err)
	}
	if _, err := storage.GetWorkflowActivation(context.Background(), gate.Spec.Key); !errors.Is(err, store.ErrConflict) {
		test.Fatal("malformed retained object accepted")
	}
}
