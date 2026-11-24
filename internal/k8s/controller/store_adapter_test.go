// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"
	appsv1 "k8s.io/api/apps/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	k8s "github.com/kognition-ai/kognition/api/k8s/v1alpha1"
	api "github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/controlplaneauth"
	"github.com/kognition-ai/kognition/internal/convert"
	"github.com/kognition-ai/kognition/internal/k8s/dispatcher"
	"github.com/kognition-ai/kognition/internal/runtimeapproval"
	"github.com/kognition-ai/kognition/internal/runtimeapproval/conformancetest"
	"github.com/kognition-ai/kognition/internal/store"
)

type executorTokenReviewer struct {
	response *authenticationv1.TokenReview
	err      error
	spec     authenticationv1.TokenReviewSpec
}

func (reviewer *executorTokenReviewer) Create(_ context.Context, review *authenticationv1.TokenReview, _ metav1.CreateOptions) (*authenticationv1.TokenReview, error) {
	reviewer.spec = review.Spec
	return reviewer.response, reviewer.err
}

type executorAccessReviewer struct {
	response *authorizationv1.SubjectAccessReview
	err      error
	spec     authorizationv1.SubjectAccessReviewSpec
	calls    int
}

func (reviewer *executorAccessReviewer) Create(_ context.Context, review *authorizationv1.SubjectAccessReview, _ metav1.CreateOptions) (*authorizationv1.SubjectAccessReview, error) {
	reviewer.calls++
	reviewer.spec = review.Spec
	return reviewer.response, reviewer.err
}

func TestExecutorKubernetesAuthenticationAndNamedInvocation(t *testing.T) {
	ctx := context.Background()
	keys := kubernetesRuntimeApprovalTestKeys(t)
	proposal, payload := conformancetest.ExecutorApprovalProposal(t, keys, "native-permission")
	action, err := runtimeapproval.WorkflowExecutorActionFromApproval(proposal, payload)
	if err != nil {
		t.Fatal(err)
	}
	action.Executor = api.ControlPlaneRoleSubject{Method: "tokenreview", Issuer: "kubernetes.io/tokenreview", Subject: "system:serviceaccount:control:executor"}
	tokens := &executorTokenReviewer{response: &authenticationv1.TokenReview{Status: authenticationv1.TokenReviewStatus{
		Authenticated: true, Audiences: []string{WorkflowExecutorKubernetesAudience},
		User: authenticationv1.UserInfo{Username: action.Executor.Subject, UID: "executor-uid", Groups: []string{"executors"},
			Extra: map[string]authenticationv1.ExtraValue{"attested": {"value"}, "authentication.kubernetes.io/pod-name": {"executor-pod"}, "authentication.kubernetes.io/pod-uid": {"pod-uid"}}}}}}
	access := &executorAccessReviewer{response: &authorizationv1.SubjectAccessReview{Status: authorizationv1.SubjectAccessReviewStatus{Allowed: true}}}
	adapter := NewStoreAdapterWithControlNamespace(newFakeClient(t), "control")
	if _, err := adapter.AuthenticatedWorkflowExecutor(ctx); err == nil {
		t.Fatal("unconfigured executor authenticated")
	}
	config := WorkflowExecutorKubernetesAuthorization{TokenReviews: tokens, AccessReviews: access,
		Credential: func(context.Context) (string, error) { return "transient-credential", nil }, Subject: action.Executor.Subject, WorkloadUID: "pod-uid",
		ServiceAccountUID: "executor-uid", PodNamespace: "control", PodName: "executor-pod",
		Pods: newFakeClient(t, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "executor-pod", Namespace: "control", UID: "pod-uid"}, Spec: corev1.PodSpec{ServiceAccountName: "executor"},
			Status: corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}}})}
	if err := adapter.ConfigureWorkflowExecutorAuthorization(config); err != nil {
		t.Fatal(err)
	}
	if principal, err := adapter.AuthenticatedWorkflowExecutor(ctx); err != nil || principal != action.Executor {
		t.Fatalf("authenticated executor = %+v %v", principal, err)
	}
	if tokens.spec.Token != "transient-credential" || len(tokens.spec.Audiences) != 1 || tokens.spec.Audiences[0] != WorkflowExecutorKubernetesAudience {
		t.Fatal("executor authentication did not require its dedicated audience")
	}
	if err := adapter.ConfigureWorkflowExecutorReader(newFakeClient(t)); err != nil {
		t.Fatal(err)
	}
	if err := adapter.AuthorizeWorkflowExecutorTarget(ctx, action); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("live target absence = %v", err)
	}
	want := authorizationv1.ResourceAttributes{Namespace: "team-a", Verb: "invoke", Group: "kognition.dev", Resource: "agents", Name: "analyst"}
	if access.spec.User != action.Executor.Subject || access.spec.UID != "executor-uid" || access.spec.ResourceAttributes == nil || *access.spec.ResourceAttributes != want ||
		len(access.spec.Groups) != 1 || access.spec.Groups[0] != "executors" || access.spec.Extra["attested"][0] != "value" {
		t.Fatalf("invocation SAR did not retain the authenticated principal and exact target: %+v", access.spec)
	}
	revision := int64(1)
	agent := &k8s.Agent{ObjectMeta: metav1.ObjectMeta{Name: action.Target.Name, Namespace: action.Target.Namespace, UID: types.UID(action.Target.UID), ResourceVersion: "1"},
		Spec: k8s.AgentSpec{Runtime: &k8s.AgentRuntimeSpec{Class: "native"}}, Status: k8s.AgentStatus{Phase: string(api.PhaseReady), RevisionCount: 1, Runtime: &k8s.AgentRuntimeStatus{Ready: "True", ReadyReplicas: 1, AvailableReplicas: 1}}}
	service := &k8s.AgentService{ObjectMeta: metav1.ObjectMeta{Name: action.Service.Name, Namespace: action.Service.Namespace, UID: types.UID(action.Service.UID), ResourceVersion: "1"},
		Spec:   k8s.AgentServiceSpec{AgentRef: k8s.LocalObjectReference{Name: action.Target.Name}, RevisionPin: &revision},
		Status: k8s.AgentServiceStatus{Phase: string(api.PhaseReady), ActiveRevision: 1, ReadyInstances: 1}}
	workflow := api.Workflow{ObjectMeta: api.ObjectMeta{Name: action.Workflow.Name, Namespace: action.Workflow.Namespace, UID: action.Workflow.UID, ResourceVersion: action.Workflow.ResourceVersion}, Spec: api.WorkflowSpec{Steps: []api.WorkflowStep{{Name: action.Activation.Step, Type: api.WorkflowStepTypeCollaboration, Collaboration: &api.WorkflowCollaboration{Strategy: api.CollaborationStrategySupervisor, Protocol: api.CollaborationProtocolA2A, Members: []api.CollaborationMember{{Name: "analyst", AgentRef: api.AgentReference{Name: action.Target.Name}, ServiceRef: &api.LocalObjectReference{Name: action.Service.Name}}}, Supervisor: &api.CollaborationSupervisor{Coordinator: "analyst"}, Limits: api.CollaborationLimits{MaxTurns: 1, MaxMessages: 1, MaxDelegations: 1, MaxDelegationDepth: 1, MaxParallelTasks: 1, MaxTokens: 64}}}}}}
	live := newFakeClient(t, agent, service, convert.WorkflowToK8s(&workflow))
	if err := adapter.ConfigureWorkflowExecutorReader(live); err != nil {
		t.Fatal(err)
	}
	if err := adapter.AuthorizeWorkflowExecutorTarget(ctx, action); err != nil {
		t.Fatalf("authorized live native target rejected: %v", err)
	}
	for _, reference := range []*api.RuntimeApprovalReference{&action.Target, &action.Service} {
		raw, err := adapter.GetWorkflowExecutorResource(ctx, reference.Kind, reference.Namespace, reference.Name)
		if err != nil {
			t.Fatal(err)
		}
		reference.ConfigurationDigest, err = runtimeapproval.WorkflowExecutorSpecDigest(raw)
		if err != nil {
			t.Fatal(err)
		}
	}
	workflowRaw, err := adapter.GetWorkflowExecutorResource(ctx, "Workflow", action.Workflow.Namespace, action.Workflow.Name)
	if err != nil {
		t.Fatal(err)
	}
	action.WorkflowRevisionDigest, err = runtimeapproval.WorkflowExecutorSpecDigest(workflowRaw)
	if err != nil {
		t.Fatal(err)
	}
	authority := runtimeapproval.WorkflowExecutorAuthority{PolicyVersion: runtimeapproval.WorkflowExecutorPolicyVersion, Reference: action.ExecutionBinding, Workflow: action.Workflow, WorkflowRevisionDigest: action.WorkflowRevisionDigest, Executor: action.Executor, Enabled: true, NotBefore: time.Now().UTC().Add(-time.Minute), ExpiresAt: time.Now().UTC().Add(time.Hour), ControllerUIDs: []string{"pod-uid"}, ApprovedBy: action.Initiator,
		Limits: runtimeapproval.WorkflowExecutorLimits{MaxEffects: 8, MaxPayloadBytes: 32768, MaxDurationSeconds: 3600, MaxMembers: 8, MaxTurns: 100, MaxMessages: 500, MaxDelegations: 100, MaxHandoffDepth: 5, MaxParallelTasks: 8, ContentRetentionSeconds: 604800, ReplayRetentionSeconds: 7776000}, Targets: []runtimeapproval.WorkflowExecutorTarget{{Agent: action.Target, Service: action.Service, Operation: runtimeapproval.WorkflowExecutorInitiate, InitialDecision: api.RuntimeApprovalDecisionPending}}}
	if err := adapter.AuthorizeWorkflowExecutorStep(ctx, authority, action); err != nil {
		t.Fatalf("native pinned-step member preflight failed: %v", err)
	}
	missingStep := action
	missingStep.Activation.Step = "undeclared"
	beforeMissingStep := access.calls
	if err := adapter.AuthorizeWorkflowExecutorStep(ctx, authority, missingStep); err == nil || access.calls != beforeMissingStep {
		t.Fatalf("undeclared step reached member invocation checks: %v", err)
	}
	access.response.Status.Denied = true
	if err := adapter.AuthorizeWorkflowExecutorStep(ctx, authority, action); !errors.Is(err, controlplaneauth.ErrForbidden) {
		t.Fatalf("native member preflight bypassed named invoke denial: %v", err)
	}
	access.response.Status.Denied = false
	foreign := action
	foreign.Target.Namespace, foreign.Service.Namespace = "team-b", "team-b"
	beforeForeign := access.calls
	if err := adapter.AuthorizeWorkflowExecutorTarget(ctx, foreign); !errors.Is(err, controlplaneauth.ErrForbidden) || access.calls != beforeForeign {
		t.Fatalf("foreign target proceeded without a reference grant: %v", err)
	}
	foreignAgent, foreignService := agent.DeepCopy(), service.DeepCopy()
	foreignAgent.Namespace, foreignService.Namespace = "team-b", "team-b"
	agentGrant := &k8s.CrossNamespaceGrant{ObjectMeta: metav1.ObjectMeta{Name: "agents", Namespace: "team-b", UID: "agent-grant", ResourceVersion: "1"}, Spec: k8s.CrossNamespaceGrantSpec{From: "team-b", To: "team-a", Resources: []k8s.GrantResource{{Kind: "Agent", Name: foreign.Target.Name}}}}
	serviceGrant := &k8s.CrossNamespaceGrant{ObjectMeta: metav1.ObjectMeta{Name: "services", Namespace: "team-b", UID: "service-grant", ResourceVersion: "1"}, Spec: k8s.CrossNamespaceGrantSpec{From: "team-b", To: "team-a", Resources: []k8s.GrantResource{{Kind: "AgentService", Name: foreign.Service.Name}}}}
	foreignReader := newFakeClient(t, foreignAgent, foreignService, agentGrant)
	if err := adapter.ConfigureWorkflowExecutorReader(foreignReader); err != nil {
		t.Fatal(err)
	}
	if err := adapter.AuthorizeWorkflowExecutorTarget(ctx, foreign); !errors.Is(err, controlplaneauth.ErrForbidden) || access.calls != beforeForeign {
		t.Fatalf("Agent grant replaced service grant: %v", err)
	}
	serviceGrant.ResourceVersion = ""
	if err := foreignReader.Create(ctx, serviceGrant); err != nil {
		t.Fatal(err)
	}
	if err := adapter.AuthorizeWorkflowExecutorTarget(ctx, foreign); err != nil || access.spec.ResourceAttributes.Namespace != "team-b" {
		t.Fatalf("independently granted foreign target: %v", err)
	}
	access.response.Status.Denied = true
	if err := adapter.AuthorizeWorkflowExecutorTarget(ctx, foreign); !errors.Is(err, controlplaneauth.ErrForbidden) {
		t.Fatalf("reference grants replaced named invoke authority: %v", err)
	}
	access.response.Status.Denied = false
	if err := foreignReader.Delete(ctx, serviceGrant); err != nil {
		t.Fatal(err)
	}
	if err := adapter.AuthorizeWorkflowExecutorTarget(ctx, foreign); !errors.Is(err, controlplaneauth.ErrForbidden) {
		t.Fatalf("service grant revocation ignored: %v", err)
	}
	if err := adapter.ConfigureWorkflowExecutorReader(live); err != nil {
		t.Fatal(err)
	}
	agent.Status.Phase = string(api.PhasePending)
	if err := live.Status().Update(ctx, agent); err != nil {
		t.Fatal(err)
	}
	if err := adapter.AuthorizeWorkflowExecutorTarget(ctx, action); err == nil {
		t.Fatal("live readiness change was ignored")
	}
	agent.Status.Phase = string(api.PhaseReady)
	if err := live.Status().Update(ctx, agent); err != nil {
		t.Fatal(err)
	}
	action.Target.ResourceVersion, err = strconv.ParseInt(agent.ResourceVersion, 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	if err := adapter.AuthorizeWorkflowExecutorTarget(ctx, action); err != nil {
		t.Fatalf("healthy baseline before permission denials: %v", err)
	}
	for name, mutate := range map[string]func(){
		"wrong workload UID": func() { tokens.response.Status.User.UID = "recreated-executor" },
		"missing bound pod":  func() { delete(tokens.response.Status.User.Extra, "authentication.kubernetes.io/pod-uid") },
		"copied service account credential": func() {
			tokens.response.Status.User.Extra["authentication.kubernetes.io/pod-uid"] = authenticationv1.ExtraValue{"other-pod"}
		},
		"ambiguous pod proof": func() {
			tokens.response.Status.User.Extra["authentication.kubernetes.io/pod-name"] = authenticationv1.ExtraValue{"executor-pod", "other-pod"}
		},
		"wrong subject":             func() { tokens.response.Status.User.Username = "submitter" },
		"wrong audience":            func() { tokens.response.Status.Audiences = []string{"general-api"} },
		"unauthenticated":           func() { tokens.response.Status.Authenticated = false },
		"token evaluation error":    func() { tokens.response.Status.Error = "unavailable" },
		"nil token review":          func() { tokens.response = nil },
		"token review unavailable":  func() { tokens.err = errors.New("unavailable") },
		"denied overrides allow":    func() { access.response.Status.Denied = true },
		"not allowed":               func() { access.response.Status.Allowed = false },
		"access evaluation error":   func() { access.response.Status.EvaluationError = "unavailable" },
		"nil access review":         func() { access.response = nil },
		"access review unavailable": func() { access.err = errors.New("unavailable") },
	} {
		t.Run(name, func(t *testing.T) {
			originalToken, originalAccess := tokens.response.DeepCopy(), access.response.DeepCopy()
			defer func() {
				tokens.response, access.response = originalToken, originalAccess
				tokens.err, access.err = nil, nil
			}()
			mutate()
			if err := adapter.AuthorizeWorkflowExecutorTarget(ctx, action); err == nil || errors.Is(err, store.ErrNotFound) {
				t.Fatalf("invalid authentication/permission reached target read: %v", err)
			}
		})
	}
	if controllerUID, err := adapter.AuthenticatedWorkflowControllerUID(ctx); err != nil || controllerUID != "pod-uid" {
		t.Fatalf("controller incarnation: %s %v", controllerUID, err)
	}
	pod := &corev1.Pod{}
	if err := config.Pods.Get(ctx, client.ObjectKey{Namespace: "control", Name: "executor-pod"}, pod); err != nil {
		t.Fatal(err)
	}
	if err := config.Pods.(client.Client).Delete(ctx, pod); err != nil {
		t.Fatal(err)
	}
	pod.UID, pod.ResourceVersion = "recreated-pod", ""
	if err := config.Pods.(client.Client).Create(ctx, pod); err != nil {
		t.Fatal(err)
	}
	if _, err := adapter.AuthenticatedWorkflowControllerUID(ctx); !errors.Is(err, controlplaneauth.ErrForbidden) {
		t.Fatalf("recreated controller inherited authority: %v", err)
	}
	pod.UID = "pod-uid"
	if err := config.Pods.(client.Client).Update(ctx, pod); err != nil {
		t.Fatal(err)
	}
	access.response.Status.Denied = true
	if err := adapter.AuthorizeWorkflowExecutorTarget(ctx, action); !errors.Is(err, controlplaneauth.ErrForbidden) {
		t.Fatalf("revoked SAR = %v", err)
	}
}

func newScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := k8s.AddToScheme(s); err != nil {
		t.Fatalf("add k8s scheme: %v", err)
	}
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatalf("add core scheme: %v", err)
	}
	if err := coordinationv1.AddToScheme(s); err != nil {
		t.Fatalf("add coordination scheme: %v", err)
	}
	if err := appsv1.AddToScheme(s); err != nil {
		t.Fatalf("add apps scheme: %v", err)
	}
	if err := networkingv1.AddToScheme(s); err != nil {
		t.Fatalf("add networking scheme: %v", err)
	}
	return s
}

func newFakeClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	return fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(objs...).WithStatusSubresource(
		&k8s.Agent{},
		&k8s.AgentIdentity{},
		&k8s.AgentInstance{},
		&k8s.ToolGrant{},
		&k8s.Workflow{},
		&k8s.CognitiveQuota{},
		&k8s.EvaluationDataset{},
		&k8s.Memory{},
		&k8s.MemoryRecord{},
		&k8s.TrustProfile{},
		&k8s.RuntimeApproval{},
	).Build()
}

func TestExecutorResourceReaderRequiresLiveConfiguration(t *testing.T) {
	ctx := context.Background()
	cachedAgent := &k8s.Agent{ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "analyst", UID: "cached-agent", ResourceVersion: "1"}}
	adapter := NewStoreAdapterWithControlNamespace(newFakeClient(t, cachedAgent), "kognition-system")
	if _, err := adapter.GetWorkflowExecutorResource(ctx, "Agent", "team-a", "analyst"); err == nil {
		t.Fatal("unconfigured executor reader fell back to the cached client")
	}
	if err := adapter.ConfigureWorkflowExecutorReader(nil); err == nil {
		t.Fatal("nil live reader accepted")
	}
	liveAgent := cachedAgent.DeepCopy()
	liveAgent.UID, liveAgent.ResourceVersion = "live-agent", "2"
	liveService := &k8s.AgentService{ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "analyst", UID: "live-service", ResourceVersion: "3"}}
	if err := adapter.ConfigureWorkflowExecutorReader(newFakeClient(t, liveAgent, liveService)); err != nil {
		t.Fatal(err)
	}
	for kind, expected := range map[string]api.ObjectMeta{
		"Agent":        {Name: "analyst", Namespace: "team-a", UID: "live-agent", ResourceVersion: 2},
		"AgentService": {Name: "analyst", Namespace: "team-a", UID: "live-service", ResourceVersion: 3},
	} {
		data, err := adapter.GetWorkflowExecutorResource(ctx, kind, "team-a", "analyst")
		if err != nil {
			t.Fatal(err)
		}
		var resource struct {
			Metadata api.ObjectMeta `json:"metadata"`
		}
		if err := json.Unmarshal(data, &resource); err != nil {
			t.Fatal(err)
		}
		if resource.Metadata.Name != expected.Name || resource.Metadata.Namespace != expected.Namespace ||
			resource.Metadata.UID != expected.UID || resource.Metadata.ResourceVersion != expected.ResourceVersion {
			t.Fatalf("%s returned cached or changed identity: %+v", kind, resource.Metadata)
		}
	}
	if _, err := adapter.GetWorkflowExecutorResource(ctx, "Agent", "team-b", "analyst"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("foreign namespace read = %v", err)
	}
	if _, err := adapter.GetWorkflowExecutorResource(ctx, "Workflow", "team-a", "analyst"); err == nil {
		t.Fatal("executor reader accepted an unsupported resource kind")
	}
}

func TestToolGrantRateReservationIsSharedAcrossStoreAdapters(t *testing.T) {
	sharedClient := newFakeClient(t)
	first := NewStoreAdapterWithControlNamespace(sharedClient, "kognition-system")
	second := NewStoreAdapterWithControlNamespace(sharedClient, "kognition-system")
	reservation := store.ToolGrantRateReservation{
		Key: "team-a/grant@1/agent/tool", Namespace: "team-a", Now: time.Now().UTC(), CallsPerMinute: 1,
	}
	if err := first.ReserveToolGrantInvocation(context.Background(), reservation); err != nil {
		t.Fatalf("first reservation failed: %v", err)
	}
	if err := second.ReserveToolGrantInvocation(context.Background(), reservation); !errors.Is(err, store.ErrQuotaExceeded) {
		t.Fatalf("second reservation error = %v, want ErrQuotaExceeded", err)
	}
}

func TestConcurrentToolGrantRateReservationsCannotOvercommit(t *testing.T) {
	adapter := NewStoreAdapterWithControlNamespace(newFakeClient(t), "kognition-system")
	reservation := store.ToolGrantRateReservation{
		Key: "team-a/grant@1/agent/tool", Namespace: "team-a", Now: time.Now().UTC(), CallsPerMinute: 5,
	}
	results := make(chan error, 20)
	var waitGroup sync.WaitGroup
	for index := 0; index < 20; index++ {
		waitGroup.Add(1)
		go func() {
			defer waitGroup.Done()
			results <- adapter.ReserveToolGrantInvocation(context.Background(), reservation)
		}()
	}
	waitGroup.Wait()
	close(results)
	succeeded := 0
	denied := 0
	for err := range results {
		switch {
		case err == nil:
			succeeded++
		case errors.Is(err, store.ErrQuotaExceeded):
			denied++
		default:
			t.Fatalf("unexpected reservation error: %v", err)
		}
	}
	if succeeded != 5 || denied != 15 {
		t.Fatalf("reservations = %d succeeded, %d denied; want 5 and 15", succeeded, denied)
	}
}

func TestToolGrantRateReservationChecksAllWindowsAtomically(t *testing.T) {
	sharedClient := newFakeClient(t)
	adapter := NewStoreAdapterWithControlNamespace(sharedClient, "kognition-system")
	now := time.Now().UTC().Truncate(time.Second)
	reservation := store.ToolGrantRateReservation{
		Key: "team-a/grant@1/agent/tool", Namespace: "team-a", Now: now, CallsPerMinute: 1, CallsPerHour: 1,
	}
	if err := adapter.ReserveToolGrantInvocation(context.Background(), reservation); err != nil {
		t.Fatalf("first reservation failed: %v", err)
	}
	reservation.Now = now.Add(2 * time.Minute)
	if err := adapter.ReserveToolGrantInvocation(context.Background(), reservation); !errors.Is(err, store.ErrQuotaExceeded) {
		t.Fatalf("hour-limited reservation error = %v, want ErrQuotaExceeded", err)
	}
	lease := &coordinationv1.Lease{}
	if err := sharedClient.Get(context.Background(), types.NamespacedName{
		Namespace: "kognition-system", Name: toolGrantRateLeaseName(reservation.Key),
	}, lease); err != nil {
		t.Fatalf("get rate Lease: %v", err)
	}
	var ledger toolGrantRateLedger
	if err := json.Unmarshal([]byte(lease.Annotations[toolGrantRateDataAnnotation]), &ledger); err != nil {
		t.Fatalf("decode rate ledger: %v", err)
	}
	if ledger.MinuteCount != 1 || ledger.HourCount != 1 {
		t.Fatalf("rejected reservation changed counters to minute=%d hour=%d", ledger.MinuteCount, ledger.HourCount)
	}
}

func makeK8sAgent(ns, name string) *k8s.Agent {
	return &k8s.Agent{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:       ns,
			Name:            name,
			ResourceVersion: "10",
		},
		Spec: k8s.AgentSpec{
			ModelRef:     k8s.LocalObjectReference{Name: "gpt-4"},
			Instructions: k8s.AgentInstructions{Inline: "You are a helpful assistant"},
		},
	}
}

func TestStoreAdapterValidateTrustProfileIndex(t *testing.T) {
	agent := api.Agent{ObjectMeta: api.ObjectMeta{Namespace: "team-a", Name: "agent-a", UID: "agent-uid", ResourceVersion: 7}}
	profile := api.TrustProfile{ObjectMeta: api.ObjectMeta{Namespace: "team-a", Name: "profile-a", UID: "profile-uid", ResourceVersion: 8}}
	controller := true
	validIndex := &k8s.TrustProfileIndex{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "team-a",
			Name:      TrustProfileIndexName(types.UID(agent.UID)),
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: k8s.GroupVersion.String(), Kind: "Agent", Name: agent.Name, UID: types.UID(agent.UID), Controller: &controller,
			}},
		},
		Spec: k8s.TrustProfileIndexSpec{
			AgentUID: agent.UID, SelectedProfileName: profile.Name, SelectedProfileUID: profile.UID,
		},
	}

	tests := []struct {
		name    string
		index   *k8s.TrustProfileIndex
		profile *api.TrustProfile
		wantErr string
	}{
		{name: "valid selection", index: validIndex.DeepCopy(), profile: &profile},
		{name: "empty cold start"},
		{name: "missing index", profile: &profile, wantErr: "missing"},
		{name: "orphan index during cold start", index: validIndex.DeepCopy(), wantErr: "without a live TrustProfile"},
		{name: "Agent UID mismatch", index: func() *k8s.TrustProfileIndex {
			index := validIndex.DeepCopy()
			index.Spec.AgentUID = "previous-agent-uid"
			return index
		}(), profile: &profile, wantErr: "Agent UID"},
		{name: "selected profile UID mismatch", index: func() *k8s.TrustProfileIndex {
			index := validIndex.DeepCopy()
			index.Spec.SelectedProfileUID = "previous-profile-uid"
			return index
		}(), profile: &profile, wantErr: "selected TrustProfile"},
		{name: "Agent owner mismatch", index: func() *k8s.TrustProfileIndex {
			index := validIndex.DeepCopy()
			index.OwnerReferences[0].UID = "previous-agent-uid"
			return index
		}(), profile: &profile, wantErr: "controller owner"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			objects := []client.Object{}
			if test.index != nil {
				objects = append(objects, test.index)
			}
			adapter := NewStoreAdapter(newFakeClient(t, objects...))
			err := adapter.ValidateTrustProfileIndex(context.Background(), agent.Namespace, agent, test.profile)
			if test.wantErr == "" && err != nil {
				t.Fatalf("ValidateTrustProfileIndex() error = %v", err)
			}
			if test.wantErr != "" && (err == nil || !strings.Contains(err.Error(), test.wantErr)) {
				t.Fatalf("ValidateTrustProfileIndex() error = %v, want mention of %q", err, test.wantErr)
			}
		})
	}
}

type storeAdapterFaultClient struct {
	client.Client
	createErr error
	deleteErr error
	getErr    error
	listErr   error
	updateErr error
	statusErr error
}

type storeAdapterFaultStatusWriter struct {
	client.SubResourceWriter
	err error
}

type retryingStatusClient struct {
	client.Client
	remainingConflicts int
	statusUpdates      int
}

type retryingStatusWriter struct {
	client.SubResourceWriter
	parent *retryingStatusClient
}

type retryingReceiptClient struct {
	client.Client
	remainingCreateConflicts int
	remainingUpdateConflicts int
	createCalls              int
	updateCalls              int
}

type changingTokenLedgerClient struct {
	client.Client
	ledgerKey       client.ObjectKey
	replacementData string
	ledgerGets      int
}

func (c *storeAdapterFaultClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if c.getErr != nil {
		return c.getErr
	}
	return c.Client.Get(ctx, key, obj, opts...)
}

func (c *storeAdapterFaultClient) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	if c.listErr != nil {
		return c.listErr
	}
	return c.Client.List(ctx, list, opts...)
}

func (c *storeAdapterFaultClient) Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
	if c.createErr != nil {
		return c.createErr
	}
	return c.Client.Create(ctx, obj, opts...)
}

func (c *storeAdapterFaultClient) Delete(ctx context.Context, obj client.Object, opts ...client.DeleteOption) error {
	if c.deleteErr != nil {
		return c.deleteErr
	}
	return c.Client.Delete(ctx, obj, opts...)
}

func (c *storeAdapterFaultClient) Update(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error {
	if c.updateErr != nil {
		return c.updateErr
	}
	return c.Client.Update(ctx, obj, opts...)
}

func (c *storeAdapterFaultClient) Status() client.SubResourceWriter {
	return &storeAdapterFaultStatusWriter{SubResourceWriter: c.Client.Status(), err: c.statusErr}
}

func (w *storeAdapterFaultStatusWriter) Update(ctx context.Context, obj client.Object, opts ...client.SubResourceUpdateOption) error {
	if w.err != nil {
		return w.err
	}
	return w.SubResourceWriter.Update(ctx, obj, opts...)
}

func (c *retryingStatusClient) Status() client.SubResourceWriter {
	return &retryingStatusWriter{SubResourceWriter: c.Client.Status(), parent: c}
}

func (w *retryingStatusWriter) Update(ctx context.Context, obj client.Object, opts ...client.SubResourceUpdateOption) error {
	w.parent.statusUpdates++
	if w.parent.remainingConflicts > 0 {
		w.parent.remainingConflicts--
		return apierrors.NewConflict(schema.GroupResource{Group: "kognition.dev", Resource: "cognitivequotas"}, obj.GetName(), errors.New("conflict"))
	}
	return w.SubResourceWriter.Update(ctx, obj, opts...)
}

func (c *retryingReceiptClient) Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
	c.createCalls++
	if c.remainingCreateConflicts > 0 {
		c.remainingCreateConflicts--
		return apierrors.NewAlreadyExists(schema.GroupResource{Resource: "configmaps"}, obj.GetName())
	}
	return c.Client.Create(ctx, obj, opts...)
}

func (c *retryingReceiptClient) Update(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error {
	c.updateCalls++
	if c.remainingUpdateConflicts > 0 {
		c.remainingUpdateConflicts--
		return apierrors.NewConflict(schema.GroupResource{Resource: "configmaps"}, obj.GetName(), errors.New("conflict"))
	}
	return c.Client.Update(ctx, obj, opts...)
}

func (c *changingTokenLedgerClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if key == c.ledgerKey {
		c.ledgerGets++
		if c.ledgerGets == 2 {
			latest := &corev1.ConfigMap{}
			if err := c.Client.Get(ctx, key, latest); err != nil {
				return err
			}
			latest.Data[tokenLedgerDataKey] = c.replacementData
			if err := c.Client.Update(ctx, latest); err != nil {
				return err
			}
		}
	}
	return c.Client.Get(ctx, key, obj, opts...)
}

// --- Get Tests ---

func TestStoreAdapter_Get_Found(t *testing.T) {
	agent := makeK8sAgent("default", "my-agent")
	fc := newFakeClient(t, agent)
	sa := NewStoreAdapter(fc)

	data, err := sa.Get(context.Background(), "Agent", "default", "my-agent")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify the data is valid internal JSON.
	var internal api.Agent
	if err := json.Unmarshal(data, &internal); err != nil {
		t.Fatalf("unmarshal internal: %v", err)
	}
	if internal.ObjectMeta.Name != "my-agent" {
		t.Errorf("name = %q, want %q", internal.ObjectMeta.Name, "my-agent")
	}
	if internal.ObjectMeta.Namespace != "default" {
		t.Errorf("namespace = %q, want %q", internal.ObjectMeta.Namespace, "default")
	}
	if internal.Spec.ModelRef.Name != "gpt-4" {
		t.Errorf("modelRef = %q, want %q", internal.Spec.ModelRef.Name, "gpt-4")
	}
	if internal.Spec.Instructions.Inline != "You are a helpful assistant" {
		t.Errorf("instructions = %q, want %q", internal.Spec.Instructions.Inline, "You are a helpful assistant")
	}
}

func TestStoreAdapter_MarshalRejectsUnexpectedObjectTypes(t *testing.T) {
	sa := NewStoreAdapter(newFakeClient(t))

	for _, tc := range []struct {
		name string
		run  func() error
	}{
		{
			name: "object type mismatch",
			run: func() error {
				_, err := sa.marshalToInternal("Agent", &k8s.Tool{})
				return err
			},
		},
		{
			name: "typed nil object",
			run: func() error {
				var agent *k8s.Agent
				_, err := sa.marshalToInternal("Agent", agent)
				return err
			},
		},
		{
			name: "list type mismatch",
			run: func() error {
				_, err := sa.marshalListToInternal("Agent", &k8s.ToolList{})
				return err
			},
		},
		{
			name: "typed nil list",
			run: func() error {
				var agents *k8s.AgentList
				_, err := sa.marshalListToInternal("Agent", agents)
				return err
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.run(); err == nil || !strings.Contains(err.Error(), "expected *k8s.Agent") {
				t.Fatalf("marshal error = %v, want expected-type error", err)
			}
		})
	}
}

func TestStoreAdapter_Get_NotFound(t *testing.T) {
	fc := newFakeClient(t)
	sa := NewStoreAdapter(fc)

	_, err := sa.Get(context.Background(), "Agent", "default", "missing")
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestStoreAdapter_Get_UnsupportedKind(t *testing.T) {
	fc := newFakeClient(t)
	sa := NewStoreAdapter(fc)

	_, err := sa.Get(context.Background(), "Unknown", "default", "x")
	if err == nil {
		t.Fatal("expected error for unsupported kind")
	}
}

func TestStoreAdapter_Get_ClientFailure(t *testing.T) {
	wantErr := errors.New("read unavailable")
	adapter := NewStoreAdapter(&storeAdapterFaultClient{Client: newFakeClient(t), getErr: wantErr})
	_, err := adapter.Get(context.Background(), "Agent", "default", "agent")
	if !errors.Is(err, wantErr) || !strings.Contains(err.Error(), "k8s get Agent") {
		t.Fatalf("Get() error = %v, want wrapped client failure", err)
	}
}

// --- List Tests ---

func TestStoreAdapter_List_All(t *testing.T) {
	a1 := makeK8sAgent("default", "agent-1")
	a2 := makeK8sAgent("default", "agent-2")
	fc := newFakeClient(t, a1, a2)
	sa := NewStoreAdapter(fc)

	items, err := sa.List(context.Background(), "Agent", store.ListOptions{Namespace: "default"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("len = %d, want 2", len(items))
	}

	// Verify each item is valid internal JSON.
	for i, raw := range items {
		var a api.Agent
		if err := json.Unmarshal(raw, &a); err != nil {
			t.Errorf("item[%d]: unmarshal failed: %v", i, err)
		}
	}
}

func TestStoreAdapter_List_Empty(t *testing.T) {
	fc := newFakeClient(t)
	sa := NewStoreAdapter(fc)

	items, err := sa.List(context.Background(), "Agent", store.ListOptions{Namespace: "default"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(items) != 0 {
		t.Fatalf("len = %d, want 0", len(items))
	}
}

func TestStoreAdapter_List_ByLabel(t *testing.T) {
	a1 := makeK8sAgent("default", "agent-1")
	a1.Labels = map[string]string{"env": "prod"}
	a2 := makeK8sAgent("default", "agent-2")
	a2.Labels = map[string]string{"env": "dev"}
	fc := newFakeClient(t, a1, a2)
	sa := NewStoreAdapter(fc)

	items, err := sa.List(context.Background(), "Agent", store.ListOptions{
		Namespace:     "default",
		LabelSelector: map[string]string{"env": "prod"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("len = %d, want 1", len(items))
	}
}

func TestStoreAdapter_List_UnsupportedKind(t *testing.T) {
	fc := newFakeClient(t)
	sa := NewStoreAdapter(fc)

	_, err := sa.List(context.Background(), "Unknown", store.ListOptions{})
	if err == nil {
		t.Fatal("expected error for unsupported kind")
	}
}

func TestStoreAdapter_ActiveParityKindsSupportCRUDLifecycle(t *testing.T) {
	for _, kind := range []string{
		"ModelGrant",
		"ToolGrant",
		"AgentCommunicationPolicy",
		"EvaluationDataset",
		"MemoryGrant",
		"Memory",
		"MemoryRecord",
	} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			sa := NewStoreAdapter(newFakeClient(t))
			name := strings.ToLower(kind) + "-parity"

			resource, err := api.NewObject(kind)
			if err != nil {
				t.Fatalf("NewObject(%s): %v", kind, err)
			}
			resource.GetObjectMeta().Name = name
			resource.GetObjectMeta().Namespace = "default"
			resource.GetObjectMeta().Annotations = map[string]string{"parity": "created"}
			data, err := json.Marshal(resource)
			if err != nil {
				t.Fatalf("marshal %s fixture: %v", kind, err)
			}

			if err := sa.Create(ctx, kind, "default", name, data); err != nil {
				t.Fatalf("Create(%s) must be supported in Kubernetes mode: %v", kind, err)
			}
			storedData, err := sa.Get(ctx, kind, "default", name)
			if err != nil {
				t.Fatalf("Get(%s) must be supported in Kubernetes mode: %v", kind, err)
			}
			stored, err := api.NewObject(kind)
			if err != nil {
				t.Fatalf("NewObject(%s) for stored data: %v", kind, err)
			}
			if err := json.Unmarshal(storedData, stored); err != nil {
				t.Fatalf("unmarshal stored %s: %v", kind, err)
			}
			if got := stored.GetObjectMeta().Annotations["parity"]; got != "created" {
				t.Fatalf("Get(%s) annotation = %q, want created", kind, got)
			}

			items, err := sa.List(ctx, kind, store.ListOptions{Namespace: "default"})
			if err != nil {
				t.Fatalf("List(%s) must be supported in Kubernetes mode: %v", kind, err)
			}
			if len(items) != 1 {
				t.Fatalf("List(%s) returned %d items, want 1", kind, len(items))
			}

			stored.GetObjectMeta().Annotations["parity"] = "updated"
			updateData, err := json.Marshal(stored)
			if err != nil {
				t.Fatalf("marshal updated %s: %v", kind, err)
			}
			updatedData, err := sa.UpdateAndGet(ctx, kind, "default", name, updateData, stored.GetObjectMeta().ResourceVersion)
			if err != nil {
				t.Fatalf("UpdateAndGet(%s) must be supported in Kubernetes mode: %v", kind, err)
			}
			updated, err := api.NewObject(kind)
			if err != nil {
				t.Fatalf("NewObject(%s) for updated data: %v", kind, err)
			}
			if err := json.Unmarshal(updatedData, updated); err != nil {
				t.Fatalf("unmarshal updated %s: %v", kind, err)
			}
			if got := updated.GetObjectMeta().Annotations["parity"]; got != "updated" {
				t.Fatalf("UpdateAndGet(%s) annotation = %q, want updated", kind, got)
			}

			if err := sa.Delete(ctx, kind, "default", name); err != nil {
				t.Fatalf("Delete(%s) must be supported in Kubernetes mode: %v", kind, err)
			}
			if _, err := sa.Get(ctx, kind, "default", name); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("Get(%s) after delete error = %v, want ErrNotFound", kind, err)
			}
		})
	}
}

func TestStoreAdapter_SupportsEveryCanonicalNativeKind(t *testing.T) {
	sa := NewStoreAdapter(newFakeClient(t))

	for _, kind := range api.RegisteredKinds() {
		if !api.IsKubernetesNativeKind(kind) {
			continue
		}
		t.Run(kind, func(t *testing.T) {
			obj, err := sa.newObject(kind)
			if err != nil {
				t.Fatalf("newObject(%s): %v", kind, err)
			}
			if _, err := sa.marshalToInternal(kind, obj); err != nil {
				t.Fatalf("marshalToInternal(%s): %v", kind, err)
			}
			if _, err := sa.unmarshalFromInternal(kind, []byte(`{}`)); err != nil {
				t.Fatalf("unmarshalFromInternal(%s): %v", kind, err)
			}

			listObj, err := sa.newListObject(kind)
			if err != nil {
				t.Fatalf("newListObject(%s): %v", kind, err)
			}
			if _, err := sa.marshalListToInternal(kind, listObj); err != nil {
				t.Fatalf("marshalListToInternal(%s): %v", kind, err)
			}
		})
	}
}

func TestStoreAdapter_ToolGrantListIsNativeLosslessAndPaginated(t *testing.T) {
	firstGrant := &k8s.ToolGrant{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "agent-a-search",
			Namespace: "team-a",
			Labels:    map[string]string{"tier": "trusted"},
		},
		Spec: k8s.ToolGrantSpec{
			AgentSelector: map[string]string{"role": "researcher"},
			ToolRef:       k8s.LocalObjectReference{Name: "search"},
			Verbs:         []k8s.ToolGrantVerb{k8s.ToolGrantVerbInvoke, k8s.ToolGrantVerbInspect},
			ArgumentConstraints: map[string]any{
				"properties": map[string]any{"query": map[string]any{"type": "string"}},
			},
			RateLimits: &k8s.ToolGrantRateLimits{CallsPerMinute: 10},
			Conditions: &k8s.ToolGrantConditions{
				ValidUntil:          "2026-12-31T23:59:59Z",
				RequiredAgentLabels: map[string]string{"clearance": "standard"},
			},
		},
		Status: k8s.ToolGrantStatus{Active: true, GrantedAgents: 2},
	}
	secondGrant := firstGrant.DeepCopy()
	secondGrant.Name = "agent-b-search"
	secondGrant.Spec.AgentSelector = nil
	secondGrant.Spec.AgentRef = k8s.LocalObjectReference{Name: "agent-b"}
	otherNamespace := firstGrant.DeepCopy()
	otherNamespace.Name = "other-search"
	otherNamespace.Namespace = "team-b"
	sa := NewStoreAdapter(newFakeClient(t, firstGrant, secondGrant, otherNamespace))

	items, err := sa.List(context.Background(), "ToolGrant", store.ListOptions{
		Namespace:     "team-a",
		LabelSelector: map[string]string{"tier": "trusted"},
		Limit:         1,
	})
	if err != nil {
		t.Fatalf("list first ToolGrant page: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("first page length = %d, want limit plus one sentinel", len(items))
	}
	var first api.ToolGrant
	if err := json.Unmarshal(items[0], &first); err != nil {
		t.Fatalf("unmarshal first ToolGrant: %v", err)
	}
	if first.ObjectMeta.Namespace != "team-a" || first.Spec.AgentSelector["role"] != "researcher" || first.Spec.ToolRef.Name != "search" {
		t.Fatalf("first native ToolGrant = %#v", first)
	}
	if !first.Spec.HasVerb(api.ToolGrantVerbInvoke) || !first.Spec.HasVerb(api.ToolGrantVerbInspect) || first.Spec.HasVerb(api.ToolGrantVerbDiscover) {
		t.Fatalf("native ToolGrant verbs = %#v", first.Spec.Verbs)
	}
	if first.Spec.RateLimits == nil || first.Spec.RateLimits.CallsPerMinute != 10 || first.Spec.Conditions == nil || first.Spec.Conditions.RequiredAgentLabels["clearance"] != "standard" {
		t.Fatalf("native ToolGrant restrictions = %#v", first.Spec)
	}
	properties, ok := first.Spec.ArgumentConstraints["properties"].(map[string]any)
	if !ok || properties["query"] == nil {
		t.Fatalf("native ToolGrant argument constraints = %#v", first.Spec.ArgumentConstraints)
	}
	if !first.Status.Active || first.Status.GrantedAgents != 2 {
		t.Fatalf("native ToolGrant status = %#v", first.Status)
	}

	continueToken := base64.RawURLEncoding.EncodeToString([]byte("1"))
	items, err = sa.List(context.Background(), "ToolGrant", store.ListOptions{
		Namespace: "team-a",
		Limit:     1,
		Continue:  continueToken,
	})
	if err != nil {
		t.Fatalf("list second ToolGrant page: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("second page length = %d, want 1", len(items))
	}
	var second api.ToolGrant
	if err := json.Unmarshal(items[0], &second); err != nil {
		t.Fatalf("unmarshal second ToolGrant: %v", err)
	}
	if second.Spec.AgentRef.Name != "agent-b" {
		t.Fatalf("second native ToolGrant agent = %q, want agent-b", second.Spec.AgentRef.Name)
	}
}

func TestStoreAdapter_List_RejectsInvalidContinueToken(t *testing.T) {
	tokens := []string{
		"not-base64!",
		base64.RawURLEncoding.EncodeToString([]byte("-1")),
	}
	for _, token := range tokens {
		sa := NewStoreAdapter(newFakeClient(t, makeK8sAgent("default", "agent")))
		_, err := sa.List(context.Background(), "Agent", store.ListOptions{
			Namespace: "default",
			Limit:     1,
			Continue:  token,
		})
		if err == nil || !strings.Contains(err.Error(), "invalid continue token") {
			t.Fatalf("List() continue token %q error = %v", token, err)
		}
	}
}

func TestStoreAdapter_List_LargeLimitDoesNotOverflow(t *testing.T) {
	sa := NewStoreAdapter(newFakeClient(t, makeK8sAgent("default", "agent")))
	items, err := sa.List(context.Background(), "Agent", store.ListOptions{
		Namespace: "default",
		Limit:     int(^uint(0) >> 1),
	})
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("List() returned %d items, want 1", len(items))
	}
}

// --- Create Tests ---

func TestStoreAdapter_Create_Success(t *testing.T) {
	fc := newFakeClient(t)
	sa := NewStoreAdapter(fc)

	internal := api.Agent{
		TypeMeta:   api.TypeMeta{APIVersion: api.APIVersion, Kind: "Agent"},
		ObjectMeta: api.ObjectMeta{Namespace: "default", Name: "new-agent", ResourceVersion: 99},
		Spec: api.AgentSpec{
			ModelRef:     api.LocalObjectReference{Name: "gpt-4"},
			Instructions: api.AgentInstructions{Inline: "hello"},
		},
	}
	data, _ := json.Marshal(internal)

	err := sa.Create(context.Background(), "Agent", "default", "new-agent", data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify it was created.
	got := &k8s.Agent{}
	if err := fc.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "new-agent"}, got); err != nil {
		t.Fatalf("get after create: %v", err)
	}
	if got.Spec.ModelRef.Name != "gpt-4" {
		t.Errorf("modelRef = %q, want %q", got.Spec.ModelRef.Name, "gpt-4")
	}
	if got.ResourceVersion == "99" {
		t.Fatalf("created resourceVersion = %q, want server-assigned value", got.ResourceVersion)
	}
}

func TestStoreAdapter_AutonomyClassUsesClusterScope(t *testing.T) {
	ctx := context.Background()
	fc := newFakeClient(t)
	sa := NewStoreAdapter(fc)
	autonomyClass := api.AutonomyClass{
		TypeMeta:   api.TypeMeta{APIVersion: api.APIVersion, Kind: "AutonomyClass"},
		ObjectMeta: api.ObjectMeta{Namespace: "system", Name: "supervised"},
		Spec:       api.AutonomyClassSpec{Level: 10, RequiresApproval: true},
	}
	data, _ := json.Marshal(autonomyClass)

	if err := sa.Create(ctx, "AutonomyClass", "system", "supervised", data); err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	stored := &k8s.AutonomyClass{}
	if err := fc.Get(ctx, client.ObjectKey{Name: "supervised"}, stored); err != nil {
		t.Fatalf("cluster-scoped get after create: %v", err)
	}
	if stored.Namespace != "" {
		t.Fatalf("stored namespace = %q, want empty", stored.Namespace)
	}
	if _, err := sa.Get(ctx, "AutonomyClass", "agent-namespace", "supervised"); err != nil {
		t.Fatalf("Get() with caller namespace error = %v", err)
	}
}

func TestStoreAdapter_Create_AlreadyExists(t *testing.T) {
	existing := makeK8sAgent("default", "existing")
	fc := newFakeClient(t, existing)
	sa := NewStoreAdapter(fc)

	internal := api.Agent{
		TypeMeta:   api.TypeMeta{APIVersion: api.APIVersion, Kind: "Agent"},
		ObjectMeta: api.ObjectMeta{Namespace: "default", Name: "existing"},
		Spec: api.AgentSpec{
			ModelRef:     api.LocalObjectReference{Name: "gpt-4"},
			Instructions: api.AgentInstructions{Inline: "hello"},
		},
	}
	data, _ := json.Marshal(internal)

	err := sa.Create(context.Background(), "Agent", "default", "existing", data)
	if !errors.Is(err, store.ErrAlreadyExists) {
		t.Fatalf("err = %v, want ErrAlreadyExists", err)
	}
}

func TestStoreAdapter_Create_ValidationAndClientFailure(t *testing.T) {
	adapter := NewStoreAdapter(newFakeClient(t))
	if err := adapter.Create(context.Background(), "Agent", "default", "agent", []byte("{")); err == nil {
		t.Fatal("Create() error = nil, want malformed JSON error")
	}

	wantErr := errors.New("create unavailable")
	adapter = NewStoreAdapter(&storeAdapterFaultClient{Client: newFakeClient(t), createErr: wantErr})
	data, _ := json.Marshal(api.Agent{})
	err := adapter.Create(context.Background(), "Agent", "default", "agent", data)
	if !errors.Is(err, wantErr) || !strings.Contains(err.Error(), "k8s create Agent") {
		t.Fatalf("Create() error = %v, want wrapped client failure", err)
	}
}

func TestStoreAdapter_Create_RejectsMalformedJSONForEveryKind(t *testing.T) {
	adapter := NewStoreAdapter(newFakeClient(t))
	kinds := []string{
		"Agent", "AgentInstance", "AgentJob", "Workflow", "Tool", "ToolClass", "Model", "ModelClass", "AutonomyClass", "ModelAutoscaler",
		"MemoryClass", "MemoryClaim", "MemoryVolume",
		"AgentPolicy", "CognitiveAdmissionPolicy", "CognitiveQuota", "CognitiveLimitRange",
		"Workspace", "AgentIdentity", "AgentRole", "AgentRoleBinding", "WorkflowRun",
		"SecretRef", "KnowledgeBaseClass", "KnowledgeBase", "KnowledgeBaseClaim",
		"KnowledgeBaseIndexRun", "KnowledgeBaseIndexSchedule", "ApprovalDecision",
	}

	for _, kind := range kinds {
		t.Run(kind, func(t *testing.T) {
			err := adapter.Create(context.Background(), kind, "default", "invalid", []byte("{"))
			if err == nil || !strings.Contains(err.Error(), "unexpected end of JSON input") {
				t.Fatalf("Create(%q) error = %v, want malformed JSON error", kind, err)
			}
		})
	}
}

// --- Update Tests ---

func TestStoreAdapter_Update_Success(t *testing.T) {
	agent := makeK8sAgent("default", "my-agent")
	fc := newFakeClient(t, agent)
	sa := NewStoreAdapter(fc)

	// Read the actual resource version assigned by the fake client.
	got := &k8s.Agent{}
	if err := fc.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "my-agent"}, got); err != nil {
		t.Fatalf("get: %v", err)
	}

	// Get the internal representation so we know the resource version.
	data, err := sa.Get(context.Background(), "Agent", "default", "my-agent")
	if err != nil {
		t.Fatalf("store get: %v", err)
	}
	var current api.Agent
	if err := json.Unmarshal(data, &current); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	internal := api.Agent{
		TypeMeta:   api.TypeMeta{APIVersion: api.APIVersion, Kind: "Agent"},
		ObjectMeta: api.ObjectMeta{Namespace: "default", Name: "my-agent"},
		Spec: api.AgentSpec{
			ModelRef:     api.LocalObjectReference{Name: "gpt-4o"},
			Instructions: api.AgentInstructions{Inline: "updated"},
		},
	}
	updateData, _ := json.Marshal(internal)

	// Use the actual resource version from the store.
	updatedData, err := sa.UpdateAndGet(context.Background(), "Agent", "default", "my-agent", updateData, current.ObjectMeta.ResourceVersion)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var updated api.Agent
	if err := json.Unmarshal(updatedData, &updated); err != nil {
		t.Fatalf("unmarshal update result: %v", err)
	}
	if got := updated.Spec.Instructions.Inline; got != "updated" {
		t.Fatalf("returned instructions: want updated, got %q", got)
	}
	if updated.ObjectMeta.ResourceVersion <= current.ObjectMeta.ResourceVersion {
		t.Fatalf("returned resourceVersion: want greater than %d, got %d", current.ObjectMeta.ResourceVersion, updated.ObjectMeta.ResourceVersion)
	}
}

func TestStoreAdapter_Update_AutonomyClassSkipsStatusSubresource(t *testing.T) {
	ctx := context.Background()
	existing := &k8s.AutonomyClass{
		ObjectMeta: metav1.ObjectMeta{Name: "assisted"},
		Spec:       k8s.AutonomyClassSpec{Level: 50, RequiresApproval: true},
	}
	faults := &storeAdapterFaultClient{
		Client:    newFakeClient(t, existing),
		statusErr: errors.New("status update must not be called"),
	}
	sa := NewStoreAdapter(faults)

	data, err := sa.Get(ctx, "AutonomyClass", "ignored", "assisted")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	var desired api.AutonomyClass
	if err := json.Unmarshal(data, &desired); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	desired.Spec.Level = 60
	updateData, _ := json.Marshal(desired)

	updatedData, err := sa.UpdateAndGet(ctx, "AutonomyClass", "ignored", "assisted", updateData, desired.ResourceVersion)
	if err != nil {
		t.Fatalf("UpdateAndGet() error = %v", err)
	}
	var updated api.AutonomyClass
	if err := json.Unmarshal(updatedData, &updated); err != nil {
		t.Fatalf("unmarshal update result: %v", err)
	}
	if updated.Spec.Level != 60 {
		t.Fatalf("updated level = %d, want 60", updated.Spec.Level)
	}
}

func TestStoreAdapter_LastFinalizerRemovalDoesNotWriteDeletedStatus(t *testing.T) {
	ctx := context.Background()
	deleting := metav1.NewTime(time.Now().UTC())
	identity := &k8s.AgentIdentity{
		ObjectMeta: metav1.ObjectMeta{Name: "agent-identity-worker", Namespace: "tenant", Finalizers: []string{"kognition.dev/identity-cleanup"}, DeletionTimestamp: &deleting},
		Status:     k8s.AgentIdentityStatus{WorkloadIdentity: &k8s.WorkloadIdentityStatus{Ready: true}},
	}
	faults := &storeAdapterFaultClient{Client: newFakeClient(t, identity), statusErr: errors.New("status must not be written after last finalizer removal")}
	adapter := NewStoreAdapter(faults)
	body, err := adapter.Get(ctx, "AgentIdentity", "tenant", identity.Name)
	if err != nil {
		t.Fatal(err)
	}
	var desired api.AgentIdentity
	if err := json.Unmarshal(body, &desired); err != nil {
		t.Fatal(err)
	}
	desired.ObjectMeta.Finalizers = nil
	desired.Status.WorkloadIdentity.Ready = false
	body, err = json.Marshal(desired)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := adapter.UpdateAndGet(ctx, "AgentIdentity", "tenant", identity.Name, body, desired.ObjectMeta.ResourceVersion); err != nil {
		t.Fatal(err)
	}
	if _, err := adapter.Get(ctx, "AgentIdentity", "tenant", identity.Name); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("identity should be normally deleted: %v", err)
	}
}

func TestStoreAdapter_Update_PersistsOwnerReferenceOnlyChange(t *testing.T) {
	ctx := context.Background()
	agent := makeK8sAgent("default", "my-agent")
	fc := newFakeClient(t, agent)
	sa := NewStoreAdapter(fc)

	data, err := sa.Get(ctx, "Agent", "default", "my-agent")
	if err != nil {
		t.Fatalf("store get: %v", err)
	}
	var current api.Agent
	if err := json.Unmarshal(data, &current); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	current.ObjectMeta.OwnerReferences = []api.OwnerReference{{
		APIVersion: api.APIVersion,
		Kind:       "Workspace",
		Name:       "platform",
		UID:        "workspace-uid",
	}}
	updateData, err := json.Marshal(current)
	if err != nil {
		t.Fatalf("marshal update: %v", err)
	}

	if _, err := sa.UpdateAndGet(ctx, "Agent", "default", "my-agent", updateData, current.ObjectMeta.ResourceVersion); err != nil {
		t.Fatalf("update owner reference: %v", err)
	}
	updated := &k8s.Agent{}
	if err := fc.Get(ctx, client.ObjectKey{Namespace: "default", Name: "my-agent"}, updated); err != nil {
		t.Fatalf("get updated Agent: %v", err)
	}
	if len(updated.OwnerReferences) != 1 || updated.OwnerReferences[0].Kind != "Workspace" ||
		updated.OwnerReferences[0].Name != "platform" || string(updated.OwnerReferences[0].UID) != "workspace-uid" {
		t.Fatalf("owner references = %#v, want Workspace platform", updated.OwnerReferences)
	}
}

func TestStoreAdapter_Update_PersistsMetadataOnlyChanges(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*api.Agent)
		check  func(*testing.T, *k8s.Agent)
	}{
		{
			name: "labels",
			mutate: func(agent *api.Agent) {
				agent.ObjectMeta.Labels = map[string]string{"environment": "production"}
			},
			check: func(t *testing.T, agent *k8s.Agent) {
				t.Helper()
				if got := agent.Labels["environment"]; got != "production" {
					t.Fatalf("label environment = %q, want production", got)
				}
			},
		},
		{
			name: "annotations",
			mutate: func(agent *api.Agent) {
				agent.ObjectMeta.Annotations = map[string]string{"kognition.dev/checkpoint": "revision-2"}
			},
			check: func(t *testing.T, agent *k8s.Agent) {
				t.Helper()
				if got := agent.Annotations["kognition.dev/checkpoint"]; got != "revision-2" {
					t.Fatalf("checkpoint annotation = %q, want revision-2", got)
				}
			},
		},
		{
			name: "finalizers",
			mutate: func(agent *api.Agent) {
				agent.ObjectMeta.Finalizers = []string{"kognition.dev/agent-cleanup"}
			},
			check: func(t *testing.T, agent *k8s.Agent) {
				t.Helper()
				if len(agent.Finalizers) != 1 || agent.Finalizers[0] != "kognition.dev/agent-cleanup" {
					t.Fatalf("finalizers = %v, want agent cleanup finalizer", agent.Finalizers)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			fc := newFakeClient(t, makeK8sAgent("default", "my-agent"))
			sa := NewStoreAdapter(fc)

			data, err := sa.Get(ctx, "Agent", "default", "my-agent")
			if err != nil {
				t.Fatalf("store get: %v", err)
			}
			var current api.Agent
			if err := json.Unmarshal(data, &current); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			tt.mutate(&current)
			updateData, err := json.Marshal(current)
			if err != nil {
				t.Fatalf("marshal update: %v", err)
			}

			if _, err := sa.UpdateAndGet(ctx, "Agent", "default", "my-agent", updateData, current.ObjectMeta.ResourceVersion); err != nil {
				t.Fatalf("metadata-only update: %v", err)
			}
			updated := &k8s.Agent{}
			if err := fc.Get(ctx, client.ObjectKey{Namespace: "default", Name: "my-agent"}, updated); err != nil {
				t.Fatalf("get updated Agent: %v", err)
			}
			tt.check(t, updated)
		})
	}
}

func TestStoreAdapter_Update_Conflict(t *testing.T) {
	agent := makeK8sAgent("default", "my-agent")
	fc := newFakeClient(t, agent)
	sa := NewStoreAdapter(fc)

	internal := api.Agent{
		TypeMeta:   api.TypeMeta{APIVersion: api.APIVersion, Kind: "Agent"},
		ObjectMeta: api.ObjectMeta{Namespace: "default", Name: "my-agent"},
		Spec: api.AgentSpec{
			ModelRef:     api.LocalObjectReference{Name: "gpt-4o"},
			Instructions: api.AgentInstructions{Inline: "updated"},
		},
	}
	data, _ := json.Marshal(internal)

	// Use a stale resource version to trigger conflict.
	err := sa.Update(context.Background(), "Agent", "default", "my-agent", data, 99999)
	if !errors.Is(err, store.ErrConflict) {
		t.Fatalf("err = %v, want ErrConflict", err)
	}
}

func TestStoreAdapter_UpdateAndGet_NotFound(t *testing.T) {
	fc := newFakeClient(t)
	sa := NewStoreAdapter(fc)
	internal := api.Agent{
		TypeMeta:   api.TypeMeta{APIVersion: api.APIVersion, Kind: "Agent"},
		ObjectMeta: api.ObjectMeta{Namespace: "default", Name: "missing-agent"},
	}
	data, _ := json.Marshal(internal)

	_, err := sa.UpdateAndGet(context.Background(), "Agent", "default", "missing-agent", data, 1)
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestStoreAdapter_UpdateAndGet_ValidationAndClientFailures(t *testing.T) {
	t.Run("malformed internal object", func(t *testing.T) {
		adapter := NewStoreAdapter(newFakeClient(t))
		if _, err := adapter.UpdateAndGet(context.Background(), "Agent", "default", "agent", []byte("{"), 1); err == nil {
			t.Fatal("UpdateAndGet() error = nil, want malformed JSON error")
		}
	})

	t.Run("unsupported kind", func(t *testing.T) {
		adapter := NewStoreAdapter(newFakeClient(t))
		if _, err := adapter.UpdateAndGet(context.Background(), "Unsupported", "default", "resource", []byte(`{}`), 1); err == nil || !strings.Contains(err.Error(), "unsupported kind") {
			t.Fatalf("UpdateAndGet() error = %v, want unsupported kind", err)
		}
	})

	t.Run("read failure", func(t *testing.T) {
		wantErr := errors.New("read unavailable")
		adapter := NewStoreAdapter(&storeAdapterFaultClient{Client: newFakeClient(t), getErr: wantErr})
		data, _ := json.Marshal(api.Agent{})
		_, err := adapter.UpdateAndGet(context.Background(), "Agent", "default", "agent", data, 1)
		if !errors.Is(err, wantErr) || !strings.Contains(err.Error(), "k8s get for update") {
			t.Fatalf("UpdateAndGet() error = %v, want wrapped read failure", err)
		}
	})

	conflictErr := apierrors.NewConflict(schema.GroupResource{Group: "kognition.dev", Resource: "agents"}, "agent", errors.New("conflict"))
	for _, tc := range []struct {
		name       string
		updateErr  error
		statusErr  error
		wantErr    error
		wantPrefix string
	}{
		{name: "spec conflict", updateErr: conflictErr, wantErr: store.ErrConflict},
		{name: "spec failure", updateErr: errors.New("update unavailable"), wantPrefix: "k8s update Agent"},
		{name: "status conflict", statusErr: conflictErr, wantErr: store.ErrConflict},
		{name: "status failure", statusErr: errors.New("status unavailable"), wantPrefix: "k8s status update Agent"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := newFakeClient(t, makeK8sAgent("default", "agent"))
			faults := &storeAdapterFaultClient{Client: base, updateErr: tc.updateErr, statusErr: tc.statusErr}
			adapter := NewStoreAdapter(faults)
			data, err := adapter.Get(context.Background(), "Agent", "default", "agent")
			if err != nil {
				t.Fatalf("Get() fixture: %v", err)
			}
			var desired api.Agent
			if err := json.Unmarshal(data, &desired); err != nil {
				t.Fatalf("unmarshal fixture: %v", err)
			}
			if tc.updateErr != nil {
				desired.Spec.Instructions.Inline = "changed"
			}
			if tc.statusErr != nil {
				desired.Status.Phase = api.PhaseReady
			}
			updateData, err := json.Marshal(desired)
			if err != nil {
				t.Fatalf("marshal desired Agent: %v", err)
			}

			_, err = adapter.UpdateAndGet(context.Background(), "Agent", "default", "agent", updateData, desired.ObjectMeta.ResourceVersion)
			if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Fatalf("UpdateAndGet() error = %v, want %v", err, tc.wantErr)
			}
			if tc.wantPrefix != "" && (err == nil || !strings.Contains(err.Error(), tc.wantPrefix)) {
				t.Fatalf("UpdateAndGet() error = %v, want message containing %q", err, tc.wantPrefix)
			}
		})
	}
}

// --- Delete Tests ---

func TestStoreAdapter_Delete_Success(t *testing.T) {
	agent := makeK8sAgent("default", "del-agent")
	fc := newFakeClient(t, agent)
	sa := NewStoreAdapter(fc)

	err := sa.Delete(context.Background(), "Agent", "default", "del-agent")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify it's gone.
	_, err = sa.Get(context.Background(), "Agent", "default", "del-agent")
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("after delete: err = %v, want ErrNotFound", err)
	}
}

func TestStoreAdapter_Delete_NotFound(t *testing.T) {
	fc := newFakeClient(t)
	sa := NewStoreAdapter(fc)

	err := sa.Delete(context.Background(), "Agent", "default", "missing")
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestStoreAdapter_Delete_ValidationAndClientFailure(t *testing.T) {
	adapter := NewStoreAdapter(newFakeClient(t))
	if err := adapter.Delete(context.Background(), "Unsupported", "default", "resource"); err == nil || !strings.Contains(err.Error(), "unsupported kind") {
		t.Fatalf("Delete() error = %v, want unsupported kind", err)
	}

	wantErr := errors.New("delete unavailable")
	adapter = NewStoreAdapter(&storeAdapterFaultClient{Client: newFakeClient(t), deleteErr: wantErr})
	err := adapter.Delete(context.Background(), "Agent", "default", "agent")
	if !errors.Is(err, wantErr) || !strings.Contains(err.Error(), "k8s delete Agent") {
		t.Fatalf("Delete() error = %v, want wrapped client failure", err)
	}
}

// --- No-op methods ---

func TestStoreAdapter_Watch_ReturnsError(t *testing.T) {
	fc := newFakeClient(t)
	sa := NewStoreAdapter(fc)

	_, err := sa.Watch(context.Background(), "Agent", store.ListOptions{})
	if err == nil {
		t.Fatal("expected error from Watch")
	}
}

func TestStoreAdapter_ListCrossNamespaceGrants(t *testing.T) {
	grant := &k8s.CrossNamespaceGrant{
		ObjectMeta: metav1.ObjectMeta{Name: "shared-tools", Namespace: "platform"},
		Spec: k8s.CrossNamespaceGrantSpec{
			From: "platform", To: "apps",
			Resources: []k8s.GrantResource{{Kind: "Tool", Name: "search"}},
		},
	}
	sa := NewStoreAdapter(newFakeClient(t, grant))

	items, err := sa.List(context.Background(), "CrossNamespaceGrant", store.ListOptions{Namespace: "platform"})
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("List() returned %d grants, want 1", len(items))
	}
	var got api.CrossNamespaceGrant
	if err := json.Unmarshal(items[0], &got); err != nil {
		t.Fatalf("unmarshal grant: %v", err)
	}
	if got.Spec.From != "platform" || got.Spec.To != "apps" || len(got.Spec.Resources) != 1 || got.Spec.Resources[0].Name != "search" {
		t.Fatalf("grant = %#v", got.Spec)
	}
}

func TestStoreAdapter_CreateRevision_RoundTripsImmutableHistory(t *testing.T) {
	agent := makeK8sAgent("default", "x")
	fc := newFakeClient(t, agent)
	sa := NewStoreAdapter(fc)
	for revision := int64(1); revision <= 3; revision++ {
		spec := []byte(`{"revision":` + strconv.FormatInt(revision, 10) + `}`)
		data := []byte("data-" + strconv.FormatInt(revision, 10))
		if err := sa.CreateRevision(context.Background(), "Agent", "default", "x", revision, spec, data); err != nil {
			t.Fatalf("CreateRevision(%d) error = %v", revision, err)
		}
	}
	revisions, err := sa.ListRevisions(context.Background(), "Agent", "default", "x")
	if err != nil {
		t.Fatalf("ListRevisions() error = %v", err)
	}
	if len(revisions) != 3 || revisions[0].Revision != 1 || revisions[2].Revision != 3 {
		t.Fatalf("revisions = %+v, want ordered history 1..3", revisions)
	}
	var revisionConfig corev1.ConfigMap
	if err := fc.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: revisionConfigMapName("Agent", "x", 2)}, &revisionConfig); err != nil {
		t.Fatalf("get revision ConfigMap: %v", err)
	}
	if revisionConfig.Immutable == nil || !*revisionConfig.Immutable {
		t.Fatal("revision ConfigMap is not Kubernetes-immutable")
	}
	exact, err := sa.GetRevision(context.Background(), "Agent", "default", "x", 2)
	if err != nil || exact.Revision != 2 || string(exact.Spec) != `{"revision":2}` || string(exact.Data) != "data-2" {
		t.Fatalf("GetRevision(2) = %+v, err = %v", exact, err)
	}
	if err := sa.CreateRevision(context.Background(), "Agent", "default", "x", 2, []byte(`{}`), nil); !errors.Is(err, store.ErrAlreadyExists) {
		t.Fatalf("duplicate CreateRevision() error = %v, want ErrAlreadyExists", err)
	}
}

func TestStoreAdapter_RevisionReadsRejectForgedConfigMaps(t *testing.T) {
	agent := makeK8sAgent("default", "x")
	agent.UID = "real-agent-uid"
	immutable := true
	forged := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      revisionConfigMapName("Agent", "x", 2),
			Namespace: "default",
			Labels: map[string]string{
				revisionParentLabel: revisionParentKey("Agent", "x"),
				revisionKindLabel:   "agent",
			},
			Annotations: map[string]string{"kognition.dev/revision": "2"},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: api.APIVersion,
				Kind:       "Agent",
				Name:       "x",
				UID:        "forged-agent-uid",
			}},
		},
		Immutable:  &immutable,
		BinaryData: map[string][]byte{"spec": []byte(`{"model":"forged"}`)},
	}
	sa := NewStoreAdapter(newFakeClient(t, agent, forged))

	revisions, err := sa.ListRevisions(context.Background(), "Agent", "default", "x")
	if err != nil {
		t.Fatalf("ListRevisions() error = %v", err)
	}
	if len(revisions) != 0 {
		t.Fatalf("ListRevisions() = %#v, want forged ConfigMap ignored", revisions)
	}
	if _, err := sa.GetRevision(context.Background(), "Agent", "default", "x", 2); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetRevision() error = %v, want ErrNotFound for forged ConfigMap", err)
	}
}

func TestStoreAdapter_RevisionReadsRejectNonPositiveRevisions(t *testing.T) {
	for _, revision := range []string{"0", "-1"} {
		t.Run(revision, func(t *testing.T) {
			agent := makeK8sAgent("default", "x")
			agent.UID = "agent-uid"
			immutable := true
			configMap := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "invalid-revision-" + strings.ReplaceAll(revision, "-", "negative-"),
					Namespace: "default",
					Labels: map[string]string{
						revisionParentLabel: revisionParentKey("Agent", "x"),
						revisionKindLabel:   "agent",
					},
					Annotations: map[string]string{"kognition.dev/revision": revision},
					OwnerReferences: []metav1.OwnerReference{{
						APIVersion: api.APIVersion, Kind: "Agent", Name: "x", UID: agent.UID,
					}},
				},
				Immutable:  &immutable,
				BinaryData: map[string][]byte{"spec": []byte(`{}`)},
			}
			sa := NewStoreAdapter(newFakeClient(t, agent, configMap))
			revisions, err := sa.ListRevisions(context.Background(), "Agent", "default", "x")
			if err != nil {
				t.Fatalf("ListRevisions() error = %v", err)
			}
			if len(revisions) != 0 {
				t.Fatalf("ListRevisions() = %#v, want non-positive revision ignored", revisions)
			}
		})
	}
}

func TestStoreAdapter_ListRevisionsMergesLegacyAndConfigMapHistory(t *testing.T) {
	agent := makeK8sAgent("default", "x")
	agent.UID = "agent-uid"
	agent.Annotations = map[string]string{
		"kognition.dev/revision-count": "5",
		"kognition.dev/revision-spec":  base64.StdEncoding.EncodeToString([]byte(`{"model":"legacy"}`)),
	}
	fc := newFakeClient(t, agent)
	sa := NewStoreAdapter(fc)
	if err := sa.CreateRevision(context.Background(), "Agent", "default", "x", 6, []byte(`{"model":"current"}`), nil); err != nil {
		t.Fatalf("CreateRevision() error = %v", err)
	}

	revisions, err := sa.ListRevisions(context.Background(), "Agent", "default", "x")
	if err != nil {
		t.Fatalf("ListRevisions() error = %v", err)
	}
	if len(revisions) != 2 || revisions[0].Revision != 5 || revisions[1].Revision != 6 {
		t.Fatalf("ListRevisions() = %#v, want legacy revision 5 and ConfigMap revision 6", revisions)
	}
}

func TestStoreAdapter_RevisionValidationAndClientFailures(t *testing.T) {
	adapter := NewStoreAdapter(newFakeClient(t))
	if err := adapter.CreateRevision(context.Background(), "Unsupported", "default", "resource", 1, []byte(`{}`), nil); err == nil || !strings.Contains(err.Error(), "unsupported kind") {
		t.Fatalf("CreateRevision() error = %v, want unsupported kind", err)
	}
	if _, err := adapter.ListRevisions(context.Background(), "Unsupported", "default", "resource"); err == nil || !strings.Contains(err.Error(), "unsupported kind") {
		t.Fatalf("ListRevisions() error = %v, want unsupported kind", err)
	}

	readErr := errors.New("read unavailable")
	adapter = NewStoreAdapter(&storeAdapterFaultClient{Client: newFakeClient(t), getErr: readErr})
	if err := adapter.CreateRevision(context.Background(), "Agent", "default", "agent", 1, []byte(`{}`), nil); !errors.Is(err, readErr) || !strings.Contains(err.Error(), "for revision") {
		t.Fatalf("CreateRevision() error = %v, want wrapped read failure", err)
	}
	if _, err := adapter.ListRevisions(context.Background(), "Agent", "default", "agent"); !errors.Is(err, readErr) || !strings.Contains(err.Error(), "for revisions") {
		t.Fatalf("ListRevisions() error = %v, want wrapped read failure", err)
	}

	writeErr := errors.New("create unavailable")
	adapter = NewStoreAdapter(&storeAdapterFaultClient{Client: newFakeClient(t, makeK8sAgent("default", "agent")), createErr: writeErr})
	err := adapter.CreateRevision(context.Background(), "Agent", "default", "agent", 2, []byte(`{"model":"gpt-4"}`), nil)
	if !errors.Is(err, writeErr) || !strings.Contains(err.Error(), "revision 2") {
		t.Fatalf("CreateRevision() error = %v, want wrapped update failure", err)
	}
}

func TestStoreAdapter_ListRevisions_Empty(t *testing.T) {
	fc := newFakeClient(t)
	sa := NewStoreAdapter(fc)

	revs, err := sa.ListRevisions(context.Background(), "Agent", "default", "x")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if revs != nil {
		t.Fatalf("revs = %v, want nil", revs)
	}
}

func TestStoreAdapter_ListRevisions_IgnoresMalformedAnnotations(t *testing.T) {
	tests := map[string]map[string]string{
		"invalid revision": {
			"kognition.dev/revision-count": "not-a-number",
			"kognition.dev/revision-spec":  base64.StdEncoding.EncodeToString([]byte(`{}`)),
		},
		"missing spec": {
			"kognition.dev/revision-count": "1",
		},
		"invalid spec": {
			"kognition.dev/revision-count": "1",
			"kognition.dev/revision-spec":  "not-base64!",
		},
	}

	for name, annotations := range tests {
		t.Run(name, func(t *testing.T) {
			agent := makeK8sAgent("default", "x")
			agent.SetAnnotations(annotations)
			sa := NewStoreAdapter(newFakeClient(t, agent))

			revisions, err := sa.ListRevisions(context.Background(), "Agent", "default", "x")
			if err != nil {
				t.Fatalf("ListRevisions() error = %v", err)
			}
			if revisions != nil {
				t.Fatalf("revisions = %v, want nil", revisions)
			}
		})
	}
}

func TestStoreAdapter_GetRevision_NotFound(t *testing.T) {
	fc := newFakeClient(t)
	sa := NewStoreAdapter(fc)

	_, err := sa.GetRevision(context.Background(), "Agent", "default", "x", 1)
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestStoreAdapter_GetRevisionFallsBackToLegacyAnnotations(t *testing.T) {
	agent := makeK8sAgent("default", "x")
	agent.Annotations = map[string]string{
		"kognition.dev/revision-count": "3",
		"kognition.dev/revision-spec":  base64.StdEncoding.EncodeToString([]byte(`{"model":"legacy"}`)),
		"kognition.dev/revision-data":  base64.StdEncoding.EncodeToString([]byte(`{"source":"annotation"}`)),
	}
	adapter := NewStoreAdapter(newFakeClient(t, agent))
	revision, err := adapter.GetRevision(context.Background(), "Agent", "default", "x", 3)
	if err != nil || revision.Revision != 3 || string(revision.Spec) != `{"model":"legacy"}` || string(revision.Data) != `{"source":"annotation"}` {
		t.Fatalf("GetRevision() = (%#v, %v), want legacy revision", revision, err)
	}
}

func TestStoreAdapter_RevisionAndEventListFailuresAreWrapped(t *testing.T) {
	listErr := errors.New("list unavailable")
	adapter := NewStoreAdapter(&storeAdapterFaultClient{Client: newFakeClient(t, makeK8sAgent("default", "x")), listErr: listErr})
	if _, err := adapter.ListRevisions(context.Background(), "Agent", "default", "x"); !errors.Is(err, listErr) || !strings.Contains(err.Error(), "list Agent default/x revisions") {
		t.Fatalf("ListRevisions() error = %v, want wrapped list failure", err)
	}
	if _, err := adapter.ListEvents(context.Background(), "Agent", "default", "x"); !errors.Is(err, listErr) || !strings.Contains(err.Error(), "list Kubernetes events") {
		t.Fatalf("ListEvents() error = %v, want wrapped list failure", err)
	}
}

func TestStoreAdapter_EventsRoundTripInChronologicalOrder(t *testing.T) {
	instance := &k8s.AgentInstance{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "x", UID: "instance-uid"}}
	fc := newFakeClient(t, instance)
	sa := NewStoreAdapter(fc)
	want := []store.InstanceEvent{
		{Timestamp: "2026-01-02T03:04:05Z", Type: corev1.EventTypeNormal, Reason: "Started", Message: "session started", Details: json.RawMessage(`{"sessionID":"s1"}`)},
		{Timestamp: "2026-01-02T03:05:05Z", Type: corev1.EventTypeWarning, Reason: "Retrying", Message: "runtime busy"},
	}
	for index := len(want) - 1; index >= 0; index-- {
		if err := sa.AppendEvent(context.Background(), "AgentInstance", "default", "x", want[index]); err != nil {
			t.Fatalf("AppendEvent(%d) error = %v", index, err)
		}
	}
	events, err := sa.ListEvents(context.Background(), "AgentInstance", "default", "x")
	if err != nil {
		t.Fatalf("ListEvents() error = %v", err)
	}
	if len(events) != len(want) {
		t.Fatalf("events = %+v, want %d", events, len(want))
	}
	for index := range want {
		if events[index].Timestamp != want[index].Timestamp || events[index].Type != want[index].Type || events[index].Reason != want[index].Reason || events[index].Message != want[index].Message || string(events[index].Details) != string(want[index].Details) {
			t.Fatalf("event[%d] = %+v, want %+v", index, events[index], want[index])
		}
	}
}

func TestStoreAdapter_AppendEventRequiresExistingNamespacedResource(t *testing.T) {
	sa := NewStoreAdapter(newFakeClient(t))
	if err := sa.AppendEvent(context.Background(), "AgentInstance", "default", "missing", store.InstanceEvent{}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing resource error = %v, want ErrNotFound", err)
	}
	if err := sa.AppendEvent(context.Background(), "AgentInstance", "", "missing", store.InstanceEvent{}); err == nil || !strings.Contains(err.Error(), "namespace is required") {
		t.Fatalf("cluster-scoped event error = %v, want namespace error", err)
	}
}

func TestStoreAdapter_ListEvents_Empty(t *testing.T) {
	fc := newFakeClient(t)
	sa := NewStoreAdapter(fc)

	events, err := sa.ListEvents(context.Background(), "AgentInstance", "default", "x")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if events != nil {
		t.Fatalf("events = %v, want nil", events)
	}
}

func TestStoreAdapter_ListEventsUsesFallbackTimestampsAndIgnoresMalformedDetails(t *testing.T) {
	instance := &k8s.AgentInstance{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "x", UID: "instance-uid"}}
	first := metav1.NewTime(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC))
	created := metav1.NewTime(time.Date(2026, 1, 2, 3, 5, 5, 0, time.UTC))
	events := []client.Object{
		instance,
		&corev1.Event{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "first", CreationTimestamp: created, Annotations: map[string]string{instanceEventDetailsAnnotation: "not-json"}}, InvolvedObject: corev1.ObjectReference{APIVersion: api.APIVersion, Kind: "AgentInstance", Namespace: "default", Name: "x"}, FirstTimestamp: first, Reason: "First", Type: corev1.EventTypeNormal},
		&corev1.Event{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "ignored"}, InvolvedObject: corev1.ObjectReference{APIVersion: "v1", Kind: "Pod", Namespace: "default", Name: "x"}, Reason: "Ignored"},
	}
	adapter := NewStoreAdapter(newFakeClient(t, events...))
	listed, err := adapter.ListEvents(context.Background(), "AgentInstance", "default", "x")
	if err != nil || len(listed) != 1 || listed[0].Timestamp != first.UTC().Format(time.RFC3339) || listed[0].Reason != "First" || listed[0].Details != nil {
		t.Fatalf("ListEvents() = (%#v, %v), want one first-timestamp event without details", listed, err)
	}
}

func TestAgentServicePublisherCreatesUpdatesAndDeletesCoreService(t *testing.T) {
	for _, test := range []struct {
		name             string
		runtimeNamespace string
		wantNamespace    string
		wantName         string
	}{
		{name: "tenant namespace", wantNamespace: "tenant-a", wantName: "research"},
		{name: "shared runtime namespace", runtimeNamespace: "agent-runtimes", wantNamespace: "agent-runtimes"},
	} {
		t.Run(test.name, func(t *testing.T) {
			k8sClient := newFakeClient(t)
			adapter := NewAgentServiceReconcilerAdapter(nil, zap.NewNop(), test.runtimeNamespace)
			adapter.client = k8sClient
			service := &api.AgentService{
				ObjectMeta: api.ObjectMeta{Name: "research", Namespace: "tenant-a"},
				Spec:       api.AgentServiceSpec{Port: 9090},
			}
			agent := &api.Agent{ObjectMeta: api.ObjectMeta{Name: "researcher", Namespace: "tenant-a"}}
			endpointInfo, err := adapter.Ensure(context.Background(), service, agent)
			if err != nil {
				t.Fatalf("Ensure() error = %v", err)
			}
			name := test.wantName
			if name == "" {
				name = agentServiceEndpointName(service, test.wantNamespace)
			}
			wantEndpoint := "http://" + name + "." + test.wantNamespace + ".svc.cluster.local:9090/invoke"
			if endpointInfo.URL != wantEndpoint || endpointInfo.ReadyInstances != nil {
				t.Fatalf("published endpoint = %#v, want URL %q without readiness override", endpointInfo, wantEndpoint)
			}
			var publishedService corev1.Service
			if err := k8sClient.Get(context.Background(), client.ObjectKey{Namespace: test.wantNamespace, Name: name}, &publishedService); err != nil {
				t.Fatalf("get published Service: %v", err)
			}
			if publishedService.Spec.Selector["kognition.dev/agent-name"] != "researcher" || publishedService.Spec.Selector["kognition.dev/agent-namespace"] != "tenant-a" || publishedService.Spec.Ports[0].Port != 9090 || publishedService.Spec.Ports[0].TargetPort.IntVal != 8080 {
				t.Fatalf("published Service = %+v", publishedService.Spec)
			}
			for key, value := range publishedService.Spec.Selector {
				if publishedService.Labels[key] != value {
					t.Fatalf("Service selector %s=%s differs from identity labels", key, value)
				}
			}
			service.Spec.Port = 7070
			if _, err := adapter.Ensure(context.Background(), service, agent); err != nil {
				t.Fatalf("update Ensure() error = %v", err)
			}
			if err := k8sClient.Get(context.Background(), client.ObjectKey{Namespace: test.wantNamespace, Name: name}, &publishedService); err != nil || publishedService.Spec.Ports[0].Port != 7070 {
				t.Fatalf("updated Service port = %d, err = %v", publishedService.Spec.Ports[0].Port, err)
			}
			if err := adapter.Delete(context.Background(), service); err != nil {
				t.Fatalf("Delete() error = %v", err)
			}
			if err := k8sClient.Get(context.Background(), client.ObjectKey{Namespace: test.wantNamespace, Name: name}, &publishedService); !apierrors.IsNotFound(err) {
				t.Fatalf("Service after Delete() error = %v, want NotFound", err)
			}
		})
	}
}

func TestAgentServicePublisherPinnedRevisionUsesDedicatedSnapshotWorkload(t *testing.T) {
	scheme := newScheme(t)
	store := newFakeStore()
	current := &api.Agent{
		ObjectMeta: api.ObjectMeta{Name: "researcher", Namespace: "tenant-a", UID: "agent-uid"},
		Spec: api.AgentSpec{
			ModelRef:     api.LocalObjectReference{Name: "current-model"},
			Instructions: api.AgentInstructions{Inline: "current instructions"},
		},
	}
	pinned := *current
	pinned.Spec.ModelRef.Name = "pinned-model"
	pinned.Spec.Instructions.Inline = "pinned instructions"
	pinnedData, err := json.Marshal(pinned)
	if err != nil {
		t.Fatalf("marshal pinned Agent revision: %v", err)
	}
	if err := store.CreateRevision(context.Background(), "Agent", "tenant-a", "researcher", 1, nil, pinnedData); err != nil {
		t.Fatalf("store Agent revision: %v", err)
	}
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	revisionDispatcher := dispatcher.NewK8sDispatcher(k8sClient, store, zap.NewNop(), dispatcher.Config{AgentImage: "runtime:v1"})
	adapter := NewAgentServiceReconcilerAdapter(store, zap.NewNop())
	adapter.client = k8sClient
	adapter.dispatcher = revisionDispatcher
	revision := int64(1)
	service := &api.AgentService{
		ObjectMeta: api.ObjectMeta{Name: "research-api", Namespace: "tenant-a", UID: "service-uid"},
		Spec:       api.AgentServiceSpec{RevisionPin: &revision, Port: 9090},
	}

	publishedEndpoint, err := adapter.Ensure(context.Background(), service, &pinned)
	if err != nil {
		t.Fatalf("Ensure() error = %v", err)
	}
	if publishedEndpoint.URL != "http://"+agentServiceEndpointName(service, "tenant-a")+".tenant-a.svc.cluster.local:9090/invoke" || publishedEndpoint.ReadyInstances == nil || *publishedEndpoint.ReadyInstances != 0 {
		t.Fatalf("published endpoint = %#v", publishedEndpoint)
	}
	var deployments appsv1.DeploymentList
	if err := k8sClient.List(context.Background(), &deployments, client.InNamespace("tenant-a")); err != nil || len(deployments.Items) != 1 {
		t.Fatalf("revision Deployments = %#v, err = %v; want one", deployments.Items, err)
	}
	deployment := deployments.Items[0]
	workloadName := deployment.Name
	if deployment.Labels["kognition.dev/agent-service-revision"] != "1" || deployment.Annotations["kognition.dev/owner-agent-service"] != "tenant-a/research-api" {
		t.Fatalf("revision Deployment metadata = %#v %#v", deployment.Labels, deployment.Annotations)
	}
	env := agentServiceEnvMap(deployment.Spec.Template.Spec.Containers[0].Env)
	if env["KOGNITION_SYSTEM_PROMPT"] != "pinned instructions" || env["KOGNITION_MODEL_ID"] != "pinned-model" {
		t.Fatalf("revision Deployment env = %#v, want immutable revision values", env)
	}
	var published corev1.Service
	serviceName := agentServiceEndpointName(service, "tenant-a")
	if err := k8sClient.Get(context.Background(), client.ObjectKey{Namespace: "tenant-a", Name: serviceName}, &published); err != nil {
		t.Fatalf("get published Service: %v", err)
	}
	if published.Spec.Selector["kognition.dev/agent-service-revision"] != "1" || published.Spec.Selector["kognition.dev/agent-service-workload"] != workloadName {
		t.Fatalf("published Service selector = %#v, want dedicated revision workload", published.Spec.Selector)
	}
	deployment.Annotations["example.com/external"] = "preserve-me"
	if err := k8sClient.Update(context.Background(), &deployment); err != nil {
		t.Fatalf("add external deployment annotation: %v", err)
	}
	resourceVersion := deployment.ResourceVersion
	if _, err := adapter.Ensure(context.Background(), service, &pinned); err != nil {
		t.Fatalf("idempotent Ensure() error = %v", err)
	}
	if err := k8sClient.Get(context.Background(), client.ObjectKey{Namespace: "tenant-a", Name: workloadName}, &deployment); err != nil {
		t.Fatalf("get idempotent revision Deployment: %v", err)
	}
	if deployment.ResourceVersion != resourceVersion {
		t.Fatalf("idempotent revision Deployment resourceVersion = %q, want %q", deployment.ResourceVersion, resourceVersion)
	}
	if deployment.Annotations["example.com/external"] != "preserve-me" {
		t.Fatalf("external annotation = %q, want preserved value", deployment.Annotations["example.com/external"])
	}
}

func TestAgentServicePublisherMigratesAndCleansRevisionWorkloads(t *testing.T) {
	scheme := newScheme(t)
	store := newFakeStore()
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	revisionDispatcher := dispatcher.NewK8sDispatcher(k8sClient, store, zap.NewNop(), dispatcher.Config{AgentImage: "runtime:v1"})
	adapter := NewAgentServiceReconcilerAdapter(store, zap.NewNop())
	adapter.client = k8sClient
	adapter.dispatcher = revisionDispatcher
	current := &api.Agent{ObjectMeta: api.ObjectMeta{Name: "researcher", Namespace: "tenant-a", UID: "agent-uid"}, Spec: api.AgentSpec{ModelRef: api.LocalObjectReference{Name: "current-model"}}}
	first := *current
	first.Spec.Instructions.Inline = "revision one"
	second := *current
	second.Spec.Instructions.Inline = "revision two"
	firstRevision := int64(1)
	service := &api.AgentService{ObjectMeta: api.ObjectMeta{Name: "research-api", Namespace: "tenant-a", UID: "service-uid"}, Spec: api.AgentServiceSpec{RevisionPin: &firstRevision}}

	if _, err := adapter.Ensure(context.Background(), service, &first); err != nil {
		t.Fatalf("Ensure() revision 1 error = %v", err)
	}
	secondRevision := int64(2)
	service.Spec.RevisionPin = &secondRevision
	if _, err := adapter.Ensure(context.Background(), service, &second); err != nil {
		t.Fatalf("Ensure() revision 2 error = %v", err)
	}
	var deployments appsv1.DeploymentList
	if err := k8sClient.List(context.Background(), &deployments, client.InNamespace("tenant-a")); err != nil || len(deployments.Items) != 2 {
		t.Fatalf("Deployments after pin migration = %#v, err = %v; want two retained revisions", deployments.Items, err)
	}
	revisions := map[string]bool{}
	for _, deployment := range deployments.Items {
		revisions[deployment.Labels["kognition.dev/agent-service-revision"]] = true
	}
	if !revisions["1"] || !revisions["2"] {
		t.Fatalf("retained revision workload labels = %#v, want revisions 1 and 2", revisions)
	}

	service.Spec.RevisionPin = nil
	if _, err := adapter.Ensure(context.Background(), service, current); err != nil {
		t.Fatalf("Ensure() unpinned error = %v", err)
	}
	if err := k8sClient.List(context.Background(), &deployments, client.InNamespace("tenant-a")); err != nil || len(deployments.Items) != 0 {
		t.Fatalf("Deployments after unpin = %#v, err = %v; want none", deployments.Items, err)
	}
	var published corev1.Service
	if err := k8sClient.Get(context.Background(), client.ObjectKey{Namespace: "tenant-a", Name: agentServiceEndpointName(service, "tenant-a")}, &published); err != nil {
		t.Fatalf("get unpinned Service: %v", err)
	}
	if published.Spec.Selector["kognition.dev/agent-name"] != "researcher" || published.Spec.Selector["kognition.dev/agent-service-workload"] != "" {
		t.Fatalf("unpinned Service selector = %#v, want shared agent selector", published.Spec.Selector)
	}

	service.Spec.RevisionPin = &secondRevision
	if _, err := adapter.Ensure(context.Background(), service, &second); err != nil {
		t.Fatalf("Ensure() revision 2 recreation error = %v", err)
	}
	if err := adapter.Delete(context.Background(), service); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	if err := k8sClient.List(context.Background(), &deployments, client.InNamespace("tenant-a")); err != nil || len(deployments.Items) != 0 {
		t.Fatalf("Deployments after service deletion = %#v, err = %v; want none", deployments.Items, err)
	}
}

func agentServiceEnvMap(envs []corev1.EnvVar) map[string]string {
	values := make(map[string]string, len(envs))
	for _, env := range envs {
		values[env.Name] = env.Value
	}
	return values
}

// --- Convert round-trip tests ---

func TestStoreAdapter_RoundTrip_Agent(t *testing.T) {
	// Create via internal, Get back, verify round-trip.
	fc := newFakeClient(t)
	sa := NewStoreAdapter(fc)

	internal := api.Agent{
		TypeMeta:   api.TypeMeta{APIVersion: api.APIVersion, Kind: "Agent"},
		ObjectMeta: api.ObjectMeta{Namespace: "ns1", Name: "rt-agent"},
		Spec: api.AgentSpec{
			ModelRef:     api.LocalObjectReference{Name: "model-x"},
			Instructions: api.AgentInstructions{Inline: "test instructions"},
		},
	}
	data, _ := json.Marshal(internal)
	if err := sa.Create(context.Background(), "Agent", "ns1", "rt-agent", data); err != nil {
		t.Fatalf("create: %v", err)
	}

	gotData, err := sa.Get(context.Background(), "Agent", "ns1", "rt-agent")
	if err != nil {
		t.Fatalf("get: %v", err)
	}

	var got api.Agent
	if err := json.Unmarshal(gotData, &got); err != nil {
		t.Fatalf("unmarshal round-trip: %v", err)
	}
	if got.ObjectMeta.Name != "rt-agent" {
		t.Errorf("name = %q, want %q", got.ObjectMeta.Name, "rt-agent")
	}
	if got.Spec.ModelRef.Name != "model-x" {
		t.Errorf("modelRef = %q, want %q", got.Spec.ModelRef.Name, "model-x")
	}
	if got.Spec.Instructions.Inline != "test instructions" {
		t.Errorf("instructions = %q, want %q", got.Spec.Instructions.Inline, "test instructions")
	}
}

// --- ResourceVersion conversion test ---

func TestStoreAdapter_Get_ResourceVersion_Converted(t *testing.T) {
	agent := makeK8sAgent("default", "rv-agent")
	agent.ResourceVersion = "42"
	fc := newFakeClient(t, agent)
	sa := NewStoreAdapter(fc)

	data, err := sa.Get(context.Background(), "Agent", "default", "rv-agent")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var internal api.Agent
	if err := json.Unmarshal(data, &internal); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// Internal uses int64 for ResourceVersion. "42" should become 42.
	if internal.ObjectMeta.ResourceVersion != 42 {
		t.Errorf("resourceVersion = %d, want 42", internal.ObjectMeta.ResourceVersion)
	}
}

// --- Multi-kind tests ---

func TestStoreAdapter_Kinds_Supported(t *testing.T) {
	fc := newFakeClient(t)
	sa := NewStoreAdapter(fc)

	kinds := []string{
		"Agent", "AgentInstance", "AgentJob", "Workflow", "Tool", "ToolClass", "Model", "ModelClass", "AutonomyClass", "ModelAutoscaler",
		"MemoryClass", "MemoryClaim", "MemoryVolume",
		"AgentPolicy", "CognitiveAdmissionPolicy", "CognitiveQuota", "CognitiveLimitRange",
		"Workspace", "AgentIdentity", "AgentRole", "AgentRoleBinding", "WorkflowRun",
		"SecretRef", "KnowledgeBaseClass", "KnowledgeBase", "KnowledgeBaseClaim",
		"KnowledgeBaseIndexRun", "KnowledgeBaseIndexSchedule", "ApprovalDecision",
	}
	for _, kind := range kinds {
		t.Run(kind, func(t *testing.T) {
			obj, err := sa.newObject(kind)
			if err != nil {
				t.Fatalf("newObject(%q): %v", kind, err)
			}
			if obj == nil {
				t.Fatal("newObject returned nil")
			}

			listObj, err := sa.newListObject(kind)
			if err != nil {
				t.Fatalf("newListObject(%q): %v", kind, err)
			}
			if listObj == nil {
				t.Fatal("newListObject returned nil")
			}

			internalData, err := sa.marshalToInternal(kind, obj)
			if err != nil {
				t.Fatalf("marshalToInternal(%q): %v", kind, err)
			}
			if !json.Valid(internalData) {
				t.Fatalf("marshalToInternal(%q) returned invalid JSON: %q", kind, internalData)
			}

			roundTripped, err := sa.unmarshalFromInternal(kind, internalData)
			if err != nil {
				t.Fatalf("unmarshalFromInternal(%q): %v", kind, err)
			}
			if roundTripped == nil {
				t.Fatal("unmarshalFromInternal returned nil")
			}

			items, err := sa.marshalListToInternal(kind, listObj)
			if err != nil {
				t.Fatalf("marshalListToInternal(%q): %v", kind, err)
			}
			if len(items) != 0 {
				t.Fatalf("empty list converted to %d items", len(items))
			}
		})
	}
}

// --- Ping test ---

func TestStoreAdapter_Ping(t *testing.T) {
	fc := newFakeClient(t)
	sa := NewStoreAdapter(fc)

	// Ping should succeed with the fake client.
	err := sa.Ping(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// --- MemoryClaim round-trip test ---

func TestStoreAdapter_RoundTrip_MemoryClaim(t *testing.T) {
	fc := newFakeClient(t)
	sa := NewStoreAdapter(fc)

	internal := api.MemoryClaim{
		TypeMeta:   api.TypeMeta{APIVersion: api.APIVersion, Kind: "MemoryClaim"},
		ObjectMeta: api.ObjectMeta{Namespace: "ns1", Name: "claim-1"},
		Spec: api.MemoryClaimSpec{
			MemoryClassName: "fast-memory",
			Type:            api.MemoryTypeEpisodic,
			Capacity:        "1Gi",
			AccessModes:     []string{"ReadWrite"},
		},
	}
	data, _ := json.Marshal(internal)
	if err := sa.Create(context.Background(), "MemoryClaim", "ns1", "claim-1", data); err != nil {
		t.Fatalf("create: %v", err)
	}

	gotData, err := sa.Get(context.Background(), "MemoryClaim", "ns1", "claim-1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}

	var got api.MemoryClaim
	if err := json.Unmarshal(gotData, &got); err != nil {
		t.Fatalf("unmarshal round-trip: %v", err)
	}
	if got.ObjectMeta.Name != "claim-1" {
		t.Errorf("name = %q, want %q", got.ObjectMeta.Name, "claim-1")
	}
	if got.Spec.MemoryClassName != "fast-memory" {
		t.Errorf("memoryClassName = %q, want %q", got.Spec.MemoryClassName, "fast-memory")
	}
	if got.Spec.Type != api.MemoryTypeEpisodic {
		t.Errorf("type = %q, want %q", got.Spec.Type, api.MemoryTypeEpisodic)
	}
	if got.Spec.Capacity != "1Gi" {
		t.Errorf("capacity = %q, want %q", got.Spec.Capacity, "1Gi")
	}
	if len(got.Spec.AccessModes) != 1 || got.Spec.AccessModes[0] != "ReadWrite" {
		t.Errorf("accessModes = %v, want [ReadWrite]", got.Spec.AccessModes)
	}
}

func TestStoreAdapter_List_MemoryClaim(t *testing.T) {
	claim := &k8s.MemoryClaim{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:       "ns1",
			Name:            "claim-list",
			ResourceVersion: "5",
		},
		Spec: k8s.MemoryClaimSpec{
			MemoryClassName: "default-class",
			Type:            k8s.MemoryTypeEpisodic,
		},
	}
	fc := newFakeClient(t, claim)
	sa := NewStoreAdapter(fc)

	results, err := sa.List(context.Background(), "MemoryClaim", store.ListOptions{Namespace: "ns1"})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}

	var got api.MemoryClaim
	if err := json.Unmarshal(results[0], &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.ObjectMeta.Name != "claim-list" {
		t.Errorf("name = %q, want %q", got.ObjectMeta.Name, "claim-list")
	}
	if got.Spec.MemoryClassName != "default-class" {
		t.Errorf("memoryClassName = %q, want %q", got.Spec.MemoryClassName, "default-class")
	}
}

// --- MemoryVolume round-trip test ---

func TestStoreAdapter_RoundTrip_MemoryVolume(t *testing.T) {
	fc := newFakeClient(t)
	sa := NewStoreAdapter(fc)

	internal := api.MemoryVolume{
		TypeMeta:   api.TypeMeta{APIVersion: api.APIVersion, Kind: "MemoryVolume"},
		ObjectMeta: api.ObjectMeta{Namespace: "ns1", Name: "vol-1"},
		Spec: api.MemoryVolumeSpec{
			MemoryClassName: "fast-memory",
			ClaimRef:        &api.LocalObjectReference{Name: "claim-1"},
			BackendHandle:   "handle-abc",
			Capacity:        "2Gi",
		},
		Status: api.MemoryVolumeStatus{
			Phase:    api.PhaseBound,
			Capacity: "2Gi",
		},
	}
	data, _ := json.Marshal(internal)
	if err := sa.Create(context.Background(), "MemoryVolume", "ns1", "vol-1", data); err != nil {
		t.Fatalf("create: %v", err)
	}

	gotData, err := sa.Get(context.Background(), "MemoryVolume", "ns1", "vol-1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}

	var got api.MemoryVolume
	if err := json.Unmarshal(gotData, &got); err != nil {
		t.Fatalf("unmarshal round-trip: %v", err)
	}
	if got.ObjectMeta.Name != "vol-1" {
		t.Errorf("name = %q, want %q", got.ObjectMeta.Name, "vol-1")
	}
	if got.Spec.MemoryClassName != "fast-memory" {
		t.Errorf("memoryClassName = %q, want %q", got.Spec.MemoryClassName, "fast-memory")
	}
	if got.Spec.ClaimRef == nil || got.Spec.ClaimRef.Name != "claim-1" {
		t.Errorf("claimRef = %v, want &{claim-1}", got.Spec.ClaimRef)
	}
	if got.Spec.BackendHandle != "handle-abc" {
		t.Errorf("backendHandle = %q, want %q", got.Spec.BackendHandle, "handle-abc")
	}
	if got.Spec.Capacity != "2Gi" {
		t.Errorf("capacity = %q, want %q", got.Spec.Capacity, "2Gi")
	}
}

func TestStoreAdapter_List_MemoryVolume(t *testing.T) {
	vol := &k8s.MemoryVolume{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:       "ns1",
			Name:            "vol-list",
			ResourceVersion: "7",
		},
		Spec: k8s.MemoryVolumeSpec{
			MemoryClassName: "default-class",
			ClaimRef:        &k8s.LocalObjectReference{Name: "claim-2"},
			BackendHandle:   "handle-xyz",
			Capacity:        "4Gi",
		},
		Status: k8s.MemoryVolumeStatus{
			Phase:    "Bound",
			Capacity: "4Gi",
		},
	}
	fc := newFakeClient(t, vol)
	sa := NewStoreAdapter(fc)

	results, err := sa.List(context.Background(), "MemoryVolume", store.ListOptions{Namespace: "ns1"})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}

	var got api.MemoryVolume
	if err := json.Unmarshal(results[0], &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.ObjectMeta.Name != "vol-list" {
		t.Errorf("name = %q, want %q", got.ObjectMeta.Name, "vol-list")
	}
	if got.Spec.MemoryClassName != "default-class" {
		t.Errorf("memoryClassName = %q, want %q", got.Spec.MemoryClassName, "default-class")
	}
	if got.Spec.ClaimRef == nil || got.Spec.ClaimRef.Name != "claim-2" {
		t.Errorf("claimRef = %v, want &{claim-2}", got.Spec.ClaimRef)
	}
	if got.Spec.BackendHandle != "handle-xyz" {
		t.Errorf("backendHandle = %q, want %q", got.Spec.BackendHandle, "handle-xyz")
	}
}

// --- AgentPolicy round-trip tests ---

func TestStoreAdapter_RoundTrip_AgentPolicy(t *testing.T) {
	fc := newFakeClient(t)
	sa := NewStoreAdapter(fc)

	internal := api.AgentPolicy{
		TypeMeta:   api.TypeMeta{APIVersion: api.APIVersion, Kind: "AgentPolicy"},
		ObjectMeta: api.ObjectMeta{Namespace: "ns1", Name: "policy-1"},
		Spec: api.AgentPolicySpec{
			Rules:   "package kognition\ndefault allow = false",
			Enforce: true,
			Scope:   "workspace",
		},
	}
	data, _ := json.Marshal(internal)
	if err := sa.Create(context.Background(), "AgentPolicy", "ns1", "policy-1", data); err != nil {
		t.Fatalf("create: %v", err)
	}

	gotData, err := sa.Get(context.Background(), "AgentPolicy", "ns1", "policy-1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}

	var got api.AgentPolicy
	if err := json.Unmarshal(gotData, &got); err != nil {
		t.Fatalf("unmarshal round-trip: %v", err)
	}
	if got.ObjectMeta.Name != "policy-1" {
		t.Errorf("name = %q, want %q", got.ObjectMeta.Name, "policy-1")
	}
	if got.Spec.Rules != "package kognition\ndefault allow = false" {
		t.Errorf("rules = %q, want %q", got.Spec.Rules, "package kognition\ndefault allow = false")
	}
	if !got.Spec.Enforce {
		t.Error("enforce = false, want true")
	}
	if got.Spec.Scope != "workspace" {
		t.Errorf("scope = %q, want %q", got.Spec.Scope, "workspace")
	}
}

func TestStoreAdapter_List_AgentPolicy(t *testing.T) {
	pol := &k8s.AgentPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:       "ns1",
			Name:            "pol-list",
			ResourceVersion: "3",
		},
		Spec: k8s.AgentPolicySpec{
			Rules:   "package test",
			Enforce: true,
		},
	}
	fc := newFakeClient(t, pol)
	sa := NewStoreAdapter(fc)

	results, err := sa.List(context.Background(), "AgentPolicy", store.ListOptions{Namespace: "ns1"})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}

	var got api.AgentPolicy
	if err := json.Unmarshal(results[0], &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.ObjectMeta.Name != "pol-list" {
		t.Errorf("name = %q, want %q", got.ObjectMeta.Name, "pol-list")
	}
}

// --- CognitiveAdmissionPolicy round-trip tests ---

func TestStoreAdapter_RoundTrip_CognitiveAdmissionPolicy(t *testing.T) {
	fc := newFakeClient(t)
	sa := NewStoreAdapter(fc)

	internal := api.CognitiveAdmissionPolicy{
		TypeMeta:   api.TypeMeta{APIVersion: api.APIVersion, Kind: "CognitiveAdmissionPolicy"},
		ObjectMeta: api.ObjectMeta{Namespace: "ns1", Name: "cap-1"},
		Spec: api.CognitiveAdmissionPolicySpec{
			Rego:            "package admission\ndefault deny = false",
			MatchKinds:      []string{"Agent", "Tool"},
			MatchOperations: []string{"CREATE", "UPDATE"},
			FailurePolicy:   "Deny",
		},
	}
	data, _ := json.Marshal(internal)
	if err := sa.Create(context.Background(), "CognitiveAdmissionPolicy", "ns1", "cap-1", data); err != nil {
		t.Fatalf("create: %v", err)
	}

	gotData, err := sa.Get(context.Background(), "CognitiveAdmissionPolicy", "ns1", "cap-1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}

	var got api.CognitiveAdmissionPolicy
	if err := json.Unmarshal(gotData, &got); err != nil {
		t.Fatalf("unmarshal round-trip: %v", err)
	}
	if got.ObjectMeta.Name != "cap-1" {
		t.Errorf("name = %q, want %q", got.ObjectMeta.Name, "cap-1")
	}
	if got.Spec.Rego != "package admission\ndefault deny = false" {
		t.Errorf("rego = %q, want expected", got.Spec.Rego)
	}
	if len(got.Spec.MatchKinds) != 2 {
		t.Errorf("matchKinds len = %d, want 2", len(got.Spec.MatchKinds))
	}
	if got.Spec.FailurePolicy != "Deny" {
		t.Errorf("failurePolicy = %q, want %q", got.Spec.FailurePolicy, "Deny")
	}
}

// --- CognitiveQuota round-trip tests ---

func TestStoreAdapter_RoundTrip_CognitiveQuota(t *testing.T) {
	fc := newFakeClient(t)
	sa := NewStoreAdapter(fc)

	internal := api.CognitiveQuota{
		TypeMeta:   api.TypeMeta{APIVersion: api.APIVersion, Kind: "CognitiveQuota"},
		ObjectMeta: api.ObjectMeta{Namespace: "ns1", Name: "quota-1"},
		Spec: api.CognitiveQuotaSpec{
			Hard: map[string]int64{"agents": 10, "tools": 20},
			TokenBudget: &api.TokenBudget{
				MaxTotalTokens: 1000000,
			},
		},
	}
	data, _ := json.Marshal(internal)
	if err := sa.Create(context.Background(), "CognitiveQuota", "ns1", "quota-1", data); err != nil {
		t.Fatalf("create: %v", err)
	}

	gotData, err := sa.Get(context.Background(), "CognitiveQuota", "ns1", "quota-1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}

	var got api.CognitiveQuota
	if err := json.Unmarshal(gotData, &got); err != nil {
		t.Fatalf("unmarshal round-trip: %v", err)
	}
	if got.ObjectMeta.Name != "quota-1" {
		t.Errorf("name = %q, want %q", got.ObjectMeta.Name, "quota-1")
	}
	if got.Spec.Hard["agents"] != 10 {
		t.Errorf("hard[agents] = %d, want 10", got.Spec.Hard["agents"])
	}
	if got.Spec.Hard["tools"] != 20 {
		t.Errorf("hard[tools] = %d, want 20", got.Spec.Hard["tools"])
	}
	if got.Spec.TokenBudget == nil || got.Spec.TokenBudget.MaxTotalTokens != 1000000 {
		t.Errorf("tokenBudget.maxTotalTokens unexpected")
	}
}

func TestStoreAdapter_ConcurrentTokenReservationsCannotOvercommit(t *testing.T) {
	quota := &k8s.CognitiveQuota{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "team-a",
			Name:      "default",
			UID:       types.UID("quota-uid-1"),
		},
		Spec: k8s.CognitiveQuotaSpec{
			TokenBudget: &k8s.TokenBudget{MaxTotalTokens: 100},
		},
	}
	sharedClient := newFakeClient(t, quota)
	first := NewStoreAdapterWithControlNamespace(sharedClient, "kognition-system")
	second := NewStoreAdapterWithControlNamespace(sharedClient, "kognition-system")
	ctx := context.Background()

	type result struct {
		id  string
		err error
	}
	start := make(chan struct{})
	results := make(chan result, 2)
	var group sync.WaitGroup
	for index, adapter := range []*StoreAdapter{first, second} {
		group.Add(1)
		go func() {
			defer group.Done()
			<-start
			id := fmt.Sprintf("run-%d", index+1)
			_, err := adapter.CreateTokenReservation(ctx, store.TokenReservation{
				ID: id, Namespace: "team-a", ReservedInput: 60,
			})
			results <- result{id: id, err: err}
		}()
	}
	close(start)
	group.Wait()
	close(results)

	var winner string
	var succeeded, exhausted int
	for result := range results {
		switch {
		case result.err == nil:
			succeeded++
			winner = result.id
		case errors.Is(result.err, store.ErrQuotaExceeded):
			exhausted++
		default:
			t.Fatalf("CreateTokenReservation(%q) unexpected error = %v", result.id, result.err)
		}
	}
	if succeeded != 1 || exhausted != 1 {
		t.Fatalf("concurrent results = %d success, %d exhausted; want 1 each", succeeded, exhausted)
	}
	if err := sharedClient.Get(ctx, first.tokenLedgerObjectKey("team-a"), &corev1.ConfigMap{}); err != nil {
		t.Fatalf("get control-plane token ledger: %v", err)
	}
	var tenantConfigMaps corev1.ConfigMapList
	if err := sharedClient.List(ctx, &tenantConfigMaps, client.InNamespace("team-a"), client.MatchingLabels{
		tokenLedgerManagedLabel: tokenLedgerManagedLabelValue,
	}); err != nil {
		t.Fatalf("list tenant token ledgers: %v", err)
	}
	if len(tenantConfigMaps.Items) != 0 {
		t.Fatalf("tenant namespace contains %d token ledgers, want 0", len(tenantConfigMaps.Items))
	}
	assertK8sQuotaTokenUsage(t, sharedClient, "team-a", 60, 0, 60)

	found, err := second.SettleTokenReservation(ctx, winner, 20, 0)
	if err != nil || !found {
		t.Fatalf("cross-adapter SettleTokenReservation() = (%v, %v), want (true, nil)", found, err)
	}
	assertK8sQuotaTokenUsage(t, sharedClient, "team-a", 20, 0, 20)
}

func TestStoreAdapter_ToolCallPreparationIsSharedAcrossAdapters(t *testing.T) {
	quota := &k8s.CognitiveQuota{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "default", UID: types.UID("quota-uid-1")},
		Spec: k8s.CognitiveQuotaSpec{
			CostBudget: &k8s.CostBudget{MaxToolCallsPerPeriod: 1},
		},
	}
	sharedClient := newFakeClient(t, quota)
	first := NewStoreAdapterWithControlNamespace(sharedClient, "kognition-system")
	second := NewStoreAdapterWithControlNamespace(sharedClient, "kognition-system")
	deadline := time.Now().Add(5 * time.Minute).UTC().Truncate(time.Microsecond)
	reservation := store.ToolCallReservation{
		Namespace: "team-a", InvocationID: "invocation-1", Binding: "binding-1",
		PreparationToken: "token-1", PreparationDeadline: deadline,
	}

	created, err := first.PrepareToolCallReservation(context.Background(), reservation)
	if err != nil || !created {
		t.Fatalf("PrepareToolCallReservation() = (%v, %v), want (true, nil)", created, err)
	}
	created, err = second.PrepareToolCallReservation(context.Background(), reservation)
	if err != nil || created {
		t.Fatalf("replayed PrepareToolCallReservation() = (%v, %v), want (false, nil)", created, err)
	}
	conflicting := reservation
	conflicting.Binding = "binding-2"
	if created, err = second.PrepareToolCallReservation(context.Background(), conflicting); created || !errors.Is(err, store.ErrConflict) {
		t.Fatalf("conflicting PrepareToolCallReservation() = (%v, %v), want (false, ErrConflict)", created, err)
	}

	reservation.InvocationID = "invocation-2"
	reservation.PreparationToken = "token-2"
	if created, err = second.PrepareToolCallReservation(context.Background(), reservation); created || !errors.Is(err, store.ErrQuotaExceeded) {
		t.Fatalf("exhausted PrepareToolCallReservation() = (%v, %v), want (false, ErrQuotaExceeded)", created, err)
	}

	got := &k8s.CognitiveQuota{}
	if err := sharedClient.Get(context.Background(), client.ObjectKeyFromObject(quota), got); err != nil {
		t.Fatalf("get CognitiveQuota: %v", err)
	}
	if got.Status.CostUsage == nil || got.Status.CostUsage.ToolCalls != 1 {
		t.Fatalf("cost usage = %#v, want one Tool call", got.Status.CostUsage)
	}
}

func TestStoreAdapter_ToolCallPreparationRejectsExpiredDeadline(t *testing.T) {
	sharedClient := newFakeClient(t, &k8s.CognitiveQuota{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "default"},
		Spec: k8s.CognitiveQuotaSpec{
			CostBudget: &k8s.CostBudget{MaxToolCallsPerPeriod: 1},
		},
	})
	adapter := NewStoreAdapterWithControlNamespace(sharedClient, "kognition-system")
	adapter.now = func() time.Time { return time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC) }

	created, err := adapter.PrepareToolCallReservation(context.Background(), store.ToolCallReservation{
		Namespace: "team-a", InvocationID: "invocation-1", Binding: "binding-1",
		PreparationToken: "token-1", PreparationDeadline: adapter.now(),
	})
	if err == nil || created || !strings.Contains(err.Error(), "must be in the future") {
		t.Fatalf("PrepareToolCallReservation() = (%v, %v), want expired deadline error", created, err)
	}
	if err := sharedClient.Get(context.Background(), adapter.toolCallLedgerObjectKey("team-a"), &corev1.ConfigMap{}); !apierrors.IsNotFound(err) {
		t.Fatalf("Tool-call ledger get error = %v, want NotFound", err)
	}
}

func TestStoreAdapter_ToolCallFinalizationIsSharedAcrossAdapters(t *testing.T) {
	quota := &k8s.CognitiveQuota{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "default", UID: types.UID("quota-uid-1")},
		Spec: k8s.CognitiveQuotaSpec{
			CostBudget: &k8s.CostBudget{MaxToolCallsPerPeriod: 2},
		},
	}
	sharedClient := newFakeClient(t, quota)
	first := NewStoreAdapterWithControlNamespace(sharedClient, "kognition-system")
	second := NewStoreAdapterWithControlNamespace(sharedClient, "kognition-system")
	deadline := time.Now().Add(5 * time.Minute)
	settled := store.ToolCallReservation{Namespace: "team-a", InvocationID: "settled", Binding: "binding-1", PreparationToken: "token-1", PreparationDeadline: deadline}
	released := store.ToolCallReservation{Namespace: "team-a", InvocationID: "released", Binding: "binding-2", PreparationToken: "token-2", PreparationDeadline: deadline}
	for _, reservation := range []store.ToolCallReservation{settled, released} {
		if created, err := first.PrepareToolCallReservation(context.Background(), reservation); err != nil || !created {
			t.Fatalf("PrepareToolCallReservation(%q) = (%v, %v), want (true, nil)", reservation.InvocationID, created, err)
		}
	}

	if found, err := second.SettleToolCallReservation(context.Background(), settled.Namespace, settled.InvocationID, settled.PreparationToken); err != nil || !found {
		t.Fatalf("SettleToolCallReservation() = (%v, %v), want (true, nil)", found, err)
	}
	if releasedCall, err := second.ReleaseToolCallReservation(context.Background(), released.Namespace, released.InvocationID, "wrong-token"); releasedCall || !errors.Is(err, store.ErrConflict) {
		t.Fatalf("mismatched ReleaseToolCallReservation() = (%v, %v), want (false, ErrConflict)", releasedCall, err)
	}
	if releasedCall, err := second.ReleaseToolCallReservation(context.Background(), released.Namespace, released.InvocationID, released.PreparationToken); err != nil || !releasedCall {
		t.Fatalf("ReleaseToolCallReservation() = (%v, %v), want (true, nil)", releasedCall, err)
	}
	if found, err := first.SettleToolCallReservation(context.Background(), settled.Namespace, settled.InvocationID, settled.PreparationToken); err != nil || found {
		t.Fatalf("replayed SettleToolCallReservation() = (%v, %v), want (false, nil)", found, err)
	}
	if releasedCall, err := first.ReleaseToolCallReservation(context.Background(), released.Namespace, released.InvocationID, released.PreparationToken); err != nil || releasedCall {
		t.Fatalf("replayed ReleaseToolCallReservation() = (%v, %v), want (false, nil)", releasedCall, err)
	}

	got := &k8s.CognitiveQuota{}
	if err := sharedClient.Get(context.Background(), client.ObjectKeyFromObject(quota), got); err != nil {
		t.Fatalf("get CognitiveQuota: %v", err)
	}
	if got.Status.CostUsage == nil || got.Status.CostUsage.ToolCalls != 1 {
		t.Fatalf("cost usage = %#v, want one settled Tool call", got.Status.CostUsage)
	}
}

func TestStoreAdapter_ToolCallReleaseDoesNotRefundNewQuotaPeriod(t *testing.T) {
	quota := &k8s.CognitiveQuota{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "default", UID: types.UID("quota-uid-1")},
		Spec: k8s.CognitiveQuotaSpec{
			CostBudget: &k8s.CostBudget{MaxToolCallsPerPeriod: 2},
		},
		Status: k8s.CognitiveQuotaStatus{LastResetTime: "period-1"},
	}
	sharedClient := newFakeClient(t, quota)
	adapter := NewStoreAdapterWithControlNamespace(sharedClient, "kognition-system")
	reservation := store.ToolCallReservation{
		Namespace: "team-a", InvocationID: "invocation-1", Binding: "binding-1",
		PreparationToken: "token-1", PreparationDeadline: time.Now().Add(5 * time.Minute),
	}
	if created, err := adapter.PrepareToolCallReservation(context.Background(), reservation); err != nil || !created {
		t.Fatalf("PrepareToolCallReservation() = (%v, %v), want (true, nil)", created, err)
	}
	latest := &k8s.CognitiveQuota{}
	if err := sharedClient.Get(context.Background(), client.ObjectKeyFromObject(quota), latest); err != nil {
		t.Fatalf("get CognitiveQuota: %v", err)
	}
	latest.Status.LastResetTime = "period-2"
	latest.Status.CostUsage = &k8s.QuotaCostUsage{ToolCalls: 1}
	if err := sharedClient.Status().Update(context.Background(), latest); err != nil {
		t.Fatalf("reset CognitiveQuota status: %v", err)
	}

	if released, err := adapter.ReleaseToolCallReservation(context.Background(), reservation.Namespace, reservation.InvocationID, reservation.PreparationToken); err != nil || !released {
		t.Fatalf("ReleaseToolCallReservation() = (%v, %v), want (true, nil)", released, err)
	}
	got := &k8s.CognitiveQuota{}
	if err := sharedClient.Get(context.Background(), client.ObjectKeyFromObject(quota), got); err != nil {
		t.Fatalf("get released CognitiveQuota: %v", err)
	}
	if got.Status.CostUsage == nil || got.Status.CostUsage.ToolCalls != 1 {
		t.Fatalf("new-period cost usage = %#v, want one Tool call", got.Status.CostUsage)
	}
}

func TestStoreAdapter_ConcurrentToolCallPreparationsCannotOvercommit(t *testing.T) {
	quota := &k8s.CognitiveQuota{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "default", UID: types.UID("quota-uid-1")},
		Spec: k8s.CognitiveQuotaSpec{
			CostBudget: &k8s.CostBudget{MaxToolCallsPerPeriod: 1},
		},
	}
	sharedClient := newFakeClient(t, quota)
	first := NewStoreAdapterWithControlNamespace(sharedClient, "kognition-system")
	second := NewStoreAdapterWithControlNamespace(sharedClient, "kognition-system")
	deadline := time.Now().Add(5 * time.Minute)

	start := make(chan struct{})
	results := make(chan error, 2)
	var group sync.WaitGroup
	for index, adapter := range []*StoreAdapter{first, second} {
		group.Add(1)
		go func() {
			defer group.Done()
			<-start
			_, err := adapter.PrepareToolCallReservation(context.Background(), store.ToolCallReservation{
				Namespace: "team-a", InvocationID: fmt.Sprintf("invocation-%d", index+1), Binding: "binding-1",
				PreparationToken: fmt.Sprintf("token-%d", index+1), PreparationDeadline: deadline,
			})
			results <- err
		}()
	}
	close(start)
	group.Wait()
	close(results)

	var succeeded, exhausted int
	for err := range results {
		switch {
		case err == nil:
			succeeded++
		case errors.Is(err, store.ErrQuotaExceeded):
			exhausted++
		default:
			t.Fatalf("unexpected preparation error: %v", err)
		}
	}
	if succeeded != 1 || exhausted != 1 {
		t.Fatalf("concurrent results = %d success, %d exhausted; want 1 each", succeeded, exhausted)
	}
	got := &k8s.CognitiveQuota{}
	if err := sharedClient.Get(context.Background(), client.ObjectKeyFromObject(quota), got); err != nil {
		t.Fatalf("get CognitiveQuota: %v", err)
	}
	if got.Status.CostUsage == nil || got.Status.CostUsage.ToolCalls != 1 {
		t.Fatalf("cost usage = %#v, want one Tool call", got.Status.CostUsage)
	}
}

func TestStoreAdapter_CanaryReservationProtectionAndSettlement(t *testing.T) {
	quota := &k8s.CognitiveQuota{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "default", UID: types.UID("quota-uid-1")},
		Spec:       k8s.CognitiveQuotaSpec{TokenBudget: &k8s.TokenBudget{MaxTotalTokens: 100}},
	}
	sharedClient := newFakeClient(t, quota)
	first := NewStoreAdapterWithControlNamespace(sharedClient, "kognition-system")
	second := NewStoreAdapterWithControlNamespace(sharedClient, "kognition-system")
	protected, ok := any(first).(store.CanaryReservationStore)
	if !ok {
		t.Fatal("Kubernetes quotas must support protected canary reservations")
	}
	ctx := context.Background()
	reservation := store.TokenReservation{ID: "canary-1", Namespace: "team-a", ReservedInput: 12, ReservedOutput: 16}
	if created, err := protected.CreateCanaryReservation(ctx, reservation); err != nil || !created {
		t.Fatalf("create protected reservation = %t, %v", created, err)
	}
	if created, err := protected.CreateCanaryReservation(ctx, reservation); err != nil || created {
		t.Fatalf("replayed protected reservation = %t, %v", created, err)
	}
	if _, err := first.CreateTokenReservation(ctx, reservation); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("generic reservation must not adopt protected claim: %v", err)
	}
	if _, err := first.SettleTokenReservation(ctx, reservation.ID, 0, 0); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("generic settlement must not refund protected claim: %v", err)
	}
	if reaped, err := second.ReapExpiredTokenReservations(ctx, time.Now().Add(time.Hour), 100); err != nil || reaped != 0 {
		t.Fatalf("protected unknown claim reaped: %d, %v", reaped, err)
	}
	assertK8sQuotaTokenUsage(t, sharedClient, "team-a", 12, 16, 28)
	restarted := any(second).(store.CanaryReservationStore)
	for attempt := 0; attempt < 2; attempt++ {
		disposition, err := restarted.SettleCanaryReservation(ctx, reservation.ID, 12, 6)
		if err != nil || disposition.State != store.CanaryReservationSettled || disposition.InputTokens != 12 || disposition.OutputTokens != 6 {
			t.Fatalf("durable exact settlement = %+v, %v", disposition, err)
		}
		assertK8sQuotaTokenUsage(t, sharedClient, "team-a", 12, 6, 18)
	}
	if _, err := restarted.SettleCanaryReservation(ctx, reservation.ID, 12, 5); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("changed settlement must conflict: %v", err)
	}
	if _, err := restarted.ReleaseCanaryReservation(ctx, reservation.ID); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("settled usage must not be refunded: %v", err)
	}
	if _, err := protected.CreateCanaryReservation(ctx, reservation); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("terminal reservation must not be recreated: %v", err)
	}
	reservation.ID = "canary-2"
	if _, err := protected.CreateCanaryReservation(ctx, reservation); err != nil {
		t.Fatal(err)
	}
	for attempt := 0; attempt < 2; attempt++ {
		disposition, err := restarted.ReleaseCanaryReservation(ctx, reservation.ID)
		if err != nil || disposition.State != store.CanaryReservationReleased {
			t.Fatalf("durable pre-call release = %+v, %v", disposition, err)
		}
	}
	assertK8sQuotaTokenUsage(t, sharedClient, "team-a", 12, 6, 18)
}

func TestStoreAdapter_TokenReservationIdempotencyAndExpiry(t *testing.T) {
	quota := &k8s.CognitiveQuota{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "team-a",
			Name:      "default",
			UID:       types.UID("quota-uid-1"),
		},
		Spec: k8s.CognitiveQuotaSpec{
			TokenBudget: &k8s.TokenBudget{MaxTotalTokens: 1000},
		},
	}
	sharedClient := newFakeClient(t, quota)
	first := NewStoreAdapterWithControlNamespace(sharedClient, "kognition-system")
	second := NewStoreAdapterWithControlNamespace(sharedClient, "kognition-system")
	ctx := context.Background()
	reservation := store.TokenReservation{
		ID: "run-1", Namespace: "team-a", ReservedInput: 400, ReservedOutput: 300,
	}
	created, err := first.CreateTokenReservation(ctx, reservation)
	if err != nil || !created {
		t.Fatalf("CreateTokenReservation() = (%v, %v), want (true, nil)", created, err)
	}
	created, err = second.CreateTokenReservation(ctx, reservation)
	if err != nil || created {
		t.Fatalf("idempotent CreateTokenReservation() = (%v, %v), want (false, nil)", created, err)
	}
	assertK8sQuotaTokenUsage(t, sharedClient, "team-a", 400, 300, 700)

	configMap := &corev1.ConfigMap{}
	key := first.tokenLedgerObjectKey("team-a")
	if err := sharedClient.Get(ctx, key, configMap); err != nil {
		t.Fatalf("get token ledger: %v", err)
	}
	ledger, err := decodeTokenLedger(configMap)
	if err != nil {
		t.Fatalf("decode token ledger: %v", err)
	}
	entry := ledger.Reservations[reservation.ID]
	entry.CreatedAt = time.Now().Add(-25 * time.Hour)
	ledger.Reservations[reservation.ID] = entry
	data, err := json.Marshal(ledger)
	if err != nil {
		t.Fatalf("marshal token ledger: %v", err)
	}
	configMap.Data[tokenLedgerDataKey] = string(data)
	if err := sharedClient.Update(ctx, configMap); err != nil {
		t.Fatalf("backdate token reservation: %v", err)
	}

	reaped, err := second.ReapExpiredTokenReservations(ctx, time.Now().Add(-24*time.Hour), 100)
	if err != nil || reaped != 1 {
		t.Fatalf("ReapExpiredTokenReservations() = (%d, %v), want (1, nil)", reaped, err)
	}
	assertK8sQuotaTokenUsage(t, sharedClient, "team-a", 0, 0, 0)
}

func TestStoreAdapter_ReapsExpiredTokenReservationsAcrossNamespacesWithinLimit(t *testing.T) {
	quotas := []client.Object{
		&k8s.CognitiveQuota{ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "default", UID: types.UID("quota-a")}, Spec: k8s.CognitiveQuotaSpec{TokenBudget: &k8s.TokenBudget{MaxTotalTokens: 1000}}},
		&k8s.CognitiveQuota{ObjectMeta: metav1.ObjectMeta{Namespace: "team-b", Name: "default", UID: types.UID("quota-b")}, Spec: k8s.CognitiveQuotaSpec{TokenBudget: &k8s.TokenBudget{MaxTotalTokens: 1000}}},
	}
	sharedClient := newFakeClient(t, quotas...)
	adapter := NewStoreAdapterWithControlNamespace(sharedClient, "kognition-system")
	ctx := context.Background()
	for _, namespace := range []string{"team-a", "team-b"} {
		if created, err := adapter.CreateTokenReservation(ctx, store.TokenReservation{ID: "expired-" + namespace, Namespace: namespace, ReservedInput: 100}); err != nil || !created {
			t.Fatalf("CreateTokenReservation(%q) = (%v, %v), want (true, nil)", namespace, created, err)
		}
		configMap := &corev1.ConfigMap{}
		if err := sharedClient.Get(ctx, adapter.tokenLedgerObjectKey(namespace), configMap); err != nil {
			t.Fatal(err)
		}
		ledger, err := decodeTokenLedger(configMap)
		if err != nil {
			t.Fatal(err)
		}
		reservation := ledger.Reservations["expired-"+namespace]
		reservation.CreatedAt = time.Now().Add(-2 * time.Hour)
		ledger.Reservations["expired-"+namespace] = reservation
		data, err := json.Marshal(ledger)
		if err != nil {
			t.Fatal(err)
		}
		configMap.Data[tokenLedgerDataKey] = string(data)
		if err := sharedClient.Update(ctx, configMap); err != nil {
			t.Fatal(err)
		}
	}

	reaped, err := adapter.ReapExpiredTokenReservations(ctx, time.Now().Add(-time.Hour), 1)
	if err != nil || reaped != 1 {
		t.Fatalf("ReapExpiredTokenReservations(limit=1) = (%d, %v), want (1, nil)", reaped, err)
	}
	remaining := 0
	for _, namespace := range []string{"team-a", "team-b"} {
		quota := &k8s.CognitiveQuota{}
		if err := sharedClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: "default"}, quota); err != nil {
			t.Fatal(err)
		}
		if quota.Status.TokenUsage != nil && quota.Status.TokenUsage.TotalTokens > 0 {
			remaining++
			if quota.Status.TokenUsage.TotalTokens != 100 {
				t.Fatalf("%s token usage = %#v, want 100 while reservation remains", namespace, quota.Status.TokenUsage)
			}
		}
	}
	if remaining != 1 {
		t.Fatalf("remaining quota projections = %d, want exactly one", remaining)
	}

	reaped, err = adapter.ReapExpiredTokenReservations(ctx, time.Now().Add(-time.Hour), 1)
	if err != nil || reaped != 1 {
		t.Fatalf("second ReapExpiredTokenReservations(limit=1) = (%d, %v), want (1, nil)", reaped, err)
	}
	assertK8sQuotaTokenUsage(t, sharedClient, "team-a", 0, 0, 0)
	assertK8sQuotaTokenUsage(t, sharedClient, "team-b", 0, 0, 0)
}

func TestStoreAdapter_ToolInvocationReceiptPersistsAcrossAdapters(t *testing.T) {
	sharedClient := newFakeClient(t)
	first := NewStoreAdapterWithControlNamespace(sharedClient, "kognition-system")
	second := NewStoreAdapterWithControlNamespace(sharedClient, "kognition-system")
	ctx := context.Background()
	receipt := store.ToolInvocationReceipt{
		Namespace: "team-a", InvocationID: "tool-1", Binding: "binding-a", Status: store.ToolInvocationPending, CreatedAt: time.Now().UTC(),
	}
	claimed, existing, err := first.ClaimToolInvocation(ctx, receipt)
	if err != nil || !claimed || existing.InvocationID != receipt.InvocationID {
		t.Fatalf("ClaimToolInvocation() = (%v, %#v, %v), want claimed receipt", claimed, existing, err)
	}
	claimed, existing, err = second.ClaimToolInvocation(ctx, receipt)
	if err != nil || claimed || existing.Status != store.ToolInvocationPending {
		t.Fatalf("duplicate ClaimToolInvocation() = (%v, %#v, %v), want pending receipt", claimed, existing, err)
	}
	now := time.Now().UTC()
	receipt.Status = store.ToolInvocationSucceeded
	receipt.Output = json.RawMessage(`{"updated":true}`)
	receipt.ExecutionMode = "sandbox"
	receipt.DurationMS = 12
	receipt.CompletedAt = &now
	if err := second.CompleteToolInvocation(ctx, receipt); err != nil {
		t.Fatalf("CompleteToolInvocation() error = %v", err)
	}
	claimed, existing, err = first.ClaimToolInvocation(ctx, receipt)
	if err != nil || claimed || existing.Status != store.ToolInvocationSucceeded || string(existing.Output) != `{"updated":true}` {
		t.Fatalf("completed duplicate ClaimToolInvocation() = (%v, %#v, %v)", claimed, existing, err)
	}
	if err := sharedClient.Get(ctx, first.toolInvocationReceiptObjectKey("team-a"), &corev1.ConfigMap{}); err != nil {
		t.Fatalf("get control-plane tool invocation ledger: %v", err)
	}
}

func TestStoreAdapter_ToolInvocationReceiptRejectsConflictingBinding(t *testing.T) {
	adapter := NewStoreAdapterWithControlNamespace(newFakeClient(t), "kognition-system")
	ctx := context.Background()
	if _, _, err := adapter.ClaimToolInvocation(ctx, store.ToolInvocationReceipt{Namespace: "team-a", InvocationID: "tool-1", Binding: "binding-a", Status: store.ToolInvocationPending, CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	_, _, err := adapter.ClaimToolInvocation(ctx, store.ToolInvocationReceipt{Namespace: "team-a", InvocationID: "tool-1", Binding: "binding-b", Status: store.ToolInvocationPending, CreatedAt: time.Now().UTC()})
	if !errors.Is(err, store.ErrConflict) {
		t.Fatalf("conflicting ClaimToolInvocation() error = %v, want store.ErrConflict", err)
	}
}

func TestStoreAdapter_ToolInvocationReceiptReclaimsAwaitingApproval(t *testing.T) {
	sharedClient := newFakeClient(t)
	first := NewStoreAdapterWithControlNamespace(sharedClient, "kognition-system")
	second := NewStoreAdapterWithControlNamespace(sharedClient, "kognition-system")
	ctx := context.Background()
	receipt := store.ToolInvocationReceipt{
		Namespace: "team-a", InvocationID: "tool-awaiting-1", Binding: "binding-a", Status: store.ToolInvocationPending, CreatedAt: time.Now().UTC(),
	}
	if claimed, _, err := first.ClaimToolInvocation(ctx, receipt); err != nil || !claimed {
		t.Fatalf("initial claim = (%t, %v), want claimed", claimed, err)
	}
	receipt.Status = store.ToolInvocationAwaitingApproval
	if err := first.CompleteToolInvocation(ctx, receipt); err != nil {
		t.Fatalf("park awaiting approval: %v", err)
	}
	claimed, reclaimed, err := second.ClaimToolInvocation(ctx, receipt)
	if err != nil || !claimed || reclaimed.Status != store.ToolInvocationPending {
		t.Fatalf("approval reclaim = (%t, %#v, %v), want claimed pending", claimed, reclaimed, err)
	}
	reclaimed.Status = store.ToolInvocationSucceeded
	if err := second.CompleteToolInvocation(ctx, reclaimed); err != nil {
		t.Fatalf("complete reclaimed invocation: %v", err)
	}
	claimed, stored, err := first.ClaimToolInvocation(ctx, receipt)
	if err != nil || claimed || stored.Status != store.ToolInvocationSucceeded {
		t.Fatalf("terminal replay = (%t, %#v, %v)", claimed, stored, err)
	}
}

func TestStoreAdapter_ToolInvocationReceiptPreparationResumesExactToken(t *testing.T) {
	ctx := context.Background()
	sharedClient := newFakeClient(t)
	first := NewStoreAdapterWithControlNamespace(sharedClient, "kognition-system")
	second := NewStoreAdapterWithControlNamespace(sharedClient, "kognition-system")
	deadline := time.Now().UTC().Add(5 * time.Minute).Truncate(time.Second)
	receipt := store.ToolInvocationReceipt{
		Namespace: "team-a", InvocationID: "tool-prepared-1", Binding: "binding-a",
		Status: store.ToolInvocationPrepared, PreparationToken: "preparation-token-00000000000001",
		PreparationDeadline: &deadline, CreatedAt: time.Now().UTC(),
	}

	prepared, stored, err := first.PrepareToolInvocation(ctx, receipt)
	if err != nil || !prepared {
		t.Fatalf("PrepareToolInvocation() = (%v, %#v, %v), want prepared receipt", prepared, stored, err)
	}
	prepared, resumed, err := second.PrepareToolInvocation(ctx, receipt)
	if err != nil || !prepared || resumed.PreparationToken != receipt.PreparationToken || resumed.PreparationDeadline == nil || !resumed.PreparationDeadline.Equal(deadline) {
		t.Fatalf("resumed PrepareToolInvocation() = (%v, %#v, %v), want exact preparation", prepared, resumed, err)
	}

	changed := receipt
	changed.PreparationToken = "preparation-token-00000000000002"
	if _, _, err := first.PrepareToolInvocation(ctx, changed); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("changed-token PrepareToolInvocation() error = %v, want store.ErrConflict", err)
	}
	if released, err := second.ReleasePreparedToolInvocation(ctx, receipt.Namespace, receipt.InvocationID, changed.PreparationToken); released || !errors.Is(err, store.ErrConflict) {
		t.Fatalf("changed-token ReleasePreparedToolInvocation() = (%v, %v), want (false, ErrConflict)", released, err)
	}
	if released, err := second.ReleasePreparedToolInvocation(ctx, receipt.Namespace, receipt.InvocationID, receipt.PreparationToken); err != nil || !released {
		t.Fatalf("ReleasePreparedToolInvocation() = (%v, %v), want (true, nil)", released, err)
	}
	if released, err := first.ReleasePreparedToolInvocation(ctx, receipt.Namespace, receipt.InvocationID, receipt.PreparationToken); err != nil || released {
		t.Fatalf("replayed ReleasePreparedToolInvocation() = (%v, %v), want (false, nil)", released, err)
	}
}

func TestStoreAdapter_ToolInvocationReceiptPromotesPendingPreparationAndCompletes(t *testing.T) {
	ctx := context.Background()
	adapter := NewStoreAdapterWithControlNamespace(newFakeClient(t), "kognition-system")
	deadline := time.Now().UTC().Add(5 * time.Minute).Truncate(time.Second)
	receipt := store.ToolInvocationReceipt{
		Namespace: "team-a", InvocationID: "tool-promoted-1", Binding: "binding-a",
		Status: store.ToolInvocationPending, CreatedAt: time.Now().UTC(),
	}
	if claimed, _, err := adapter.ClaimToolInvocation(ctx, receipt); err != nil || !claimed {
		t.Fatalf("ClaimToolInvocation() = (%v, %v), want (true, nil)", claimed, err)
	}
	receipt.Status = store.ToolInvocationPrepared
	receipt.PreparationToken = "preparation-token-00000000000001"
	receipt.PreparationDeadline = &deadline
	if prepared, stored, err := adapter.PrepareToolInvocation(ctx, receipt); err != nil || !prepared || stored.Status != store.ToolInvocationPrepared {
		t.Fatalf("PrepareToolInvocation() = (%v, %#v, %v), want promoted preparation", prepared, stored, err)
	}
	receipt.Status = store.ToolInvocationSucceeded
	completedAt := time.Now().UTC()
	receipt.CompletedAt = &completedAt
	if err := adapter.CompleteToolInvocation(ctx, receipt); err != nil {
		t.Fatalf("CompleteToolInvocation(): %v", err)
	}
	claimed, stored, err := adapter.ClaimToolInvocation(ctx, receipt)
	if err != nil || claimed || stored.Status != store.ToolInvocationSucceeded || stored.PreparationToken != "" || stored.PreparationDeadline != nil {
		t.Fatalf("terminal receipt = (%v, %#v, %v), want completed receipt without preparation", claimed, stored, err)
	}
}

func TestStoreAdapter_ToolInvocationReceiptReleasePreservesNonPreparedReceipt(t *testing.T) {
	ctx := context.Background()
	adapter := NewStoreAdapterWithControlNamespace(newFakeClient(t), "kognition-system")
	receipt := store.ToolInvocationReceipt{
		Namespace: "team-a", InvocationID: "tool-pending-1", Binding: "binding-a",
		Status: store.ToolInvocationPending, CreatedAt: time.Now().UTC(),
	}
	if claimed, _, err := adapter.ClaimToolInvocation(ctx, receipt); err != nil || !claimed {
		t.Fatalf("ClaimToolInvocation() = (%v, %v), want (true, nil)", claimed, err)
	}
	if released, err := adapter.ReleasePreparedToolInvocation(ctx, receipt.Namespace, receipt.InvocationID, "preparation-token-00000000000001"); released || !errors.Is(err, store.ErrConflict) {
		t.Fatalf("ReleasePreparedToolInvocation() = (%v, %v), want (false, ErrConflict)", released, err)
	}
	if claimed, existing, err := adapter.ClaimToolInvocation(ctx, receipt); err != nil || claimed || existing.Status != store.ToolInvocationPending {
		t.Fatalf("preserved ClaimToolInvocation() = (%v, %#v, %v), want existing pending receipt", claimed, existing, err)
	}
}

func TestStoreAdapter_ToolInvocationReceiptConcurrentApprovalReclaimHasSingleWinner(t *testing.T) {
	sharedClient := newFakeClient(t)
	first := NewStoreAdapterWithControlNamespace(sharedClient, "kognition-system")
	ctx := context.Background()
	receipt := store.ToolInvocationReceipt{
		Namespace: "team-a", InvocationID: "tool-awaiting-concurrent", Binding: "binding-a", Status: store.ToolInvocationPending, CreatedAt: time.Now().UTC(),
	}
	if claimed, _, err := first.ClaimToolInvocation(ctx, receipt); err != nil || !claimed {
		t.Fatalf("initial claim = (%t, %v), want claimed", claimed, err)
	}
	receipt.Status = store.ToolInvocationAwaitingApproval
	if err := first.CompleteToolInvocation(ctx, receipt); err != nil {
		t.Fatalf("park awaiting approval: %v", err)
	}

	start := make(chan struct{})
	type result struct {
		claimed bool
		status  store.ToolInvocationReceiptStatus
		err     error
	}
	results := make(chan result, 2)
	for range 2 {
		go func(adapter *StoreAdapter) {
			<-start
			claimed, stored, err := adapter.ClaimToolInvocation(ctx, receipt)
			results <- result{claimed: claimed, status: stored.Status, err: err}
		}(NewStoreAdapterWithControlNamespace(sharedClient, "kognition-system"))
	}
	close(start)
	winners := 0
	for range 2 {
		result := <-results
		if result.err != nil || result.status != store.ToolInvocationPending {
			t.Fatalf("concurrent reclaim = (%t, %q, %v), want pending without error", result.claimed, result.status, result.err)
		}
		if result.claimed {
			winners++
		}
	}
	if winners != 1 {
		t.Fatalf("concurrent approval reclaim winners = %d, want 1", winners)
	}
}

func TestStoreAdapter_ToolInvocationReceiptCompletionValidatesAndIsIdempotent(t *testing.T) {
	adapter := NewStoreAdapterWithControlNamespace(newFakeClient(t), "kognition-system")
	ctx := context.Background()
	pending := store.ToolInvocationReceipt{
		Namespace: "team-a", InvocationID: "tool-1", Binding: "binding-a", Status: store.ToolInvocationPending, CreatedAt: time.Now().UTC(),
	}
	for _, receipt := range []store.ToolInvocationReceipt{
		{},
		{Namespace: "team-a", InvocationID: "tool-1", Binding: "binding-a", Status: store.ToolInvocationPending},
	} {
		if err := adapter.CompleteToolInvocation(ctx, receipt); err == nil {
			t.Fatalf("CompleteToolInvocation(%#v) error = nil, want validation error", receipt)
		}
	}
	if err := adapter.CompleteToolInvocation(ctx, store.ToolInvocationReceipt{Namespace: "team-a", InvocationID: "missing", Binding: "binding-a", Status: store.ToolInvocationSucceeded}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("CompleteToolInvocation() error = %v, want store.ErrNotFound", err)
	}
	if _, _, err := adapter.ClaimToolInvocation(ctx, pending); err != nil {
		t.Fatal(err)
	}
	completedAt := time.Now().UTC()
	completed := pending
	completed.Status = store.ToolInvocationSucceeded
	completed.Output = json.RawMessage(`{"answer":42}`)
	completed.CompletedAt = &completedAt
	if err := adapter.CompleteToolInvocation(ctx, completed); err != nil {
		t.Fatal(err)
	}
	if err := adapter.CompleteToolInvocation(ctx, completed); err != nil {
		t.Fatalf("identical completion error = %v", err)
	}
	conflicting := completed
	conflicting.Binding = "binding-b"
	if err := adapter.CompleteToolInvocation(ctx, conflicting); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("conflicting completion error = %v, want store.ErrConflict", err)
	}
	claimed, stored, err := adapter.ClaimToolInvocation(ctx, pending)
	if err != nil || claimed || stored.Status != store.ToolInvocationSucceeded || string(stored.Output) != `{"answer":42}` {
		t.Fatalf("stored terminal receipt = (%t, %#v, %v)", claimed, stored, err)
	}
}

func TestStoreAdapter_ToolInvocationReceiptRejectsMalformedLedger(t *testing.T) {
	client := newFakeClient(t)
	adapter := NewStoreAdapterWithControlNamespace(client, "kognition-system")
	key := adapter.toolInvocationReceiptObjectKey("team-a")
	ledger := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: key.Namespace, Name: key.Name},
		Data:       map[string]string{toolInvocationReceiptDataKey: "{"},
	}
	if err := client.Create(context.Background(), ledger); err != nil {
		t.Fatal(err)
	}
	_, _, err := adapter.ClaimToolInvocation(context.Background(), store.ToolInvocationReceipt{
		Namespace: "team-a", InvocationID: "tool-1", Binding: "binding-a", Status: store.ToolInvocationPending,
	})
	if err == nil || !strings.Contains(err.Error(), "decode tool invocation receipt ledger") {
		t.Fatalf("ClaimToolInvocation() error = %v, want ledger decode error", err)
	}
}

func TestStoreAdapter_ToolInvocationReceiptRetriesConfigMapConflicts(t *testing.T) {
	ctx := context.Background()
	retryingClient := &retryingReceiptClient{Client: newFakeClient(t), remainingCreateConflicts: 1}
	adapter := NewStoreAdapterWithControlNamespace(retryingClient, "kognition-system")
	pending := store.ToolInvocationReceipt{
		Namespace: "team-a", InvocationID: "tool-1", Binding: "binding-a", Status: store.ToolInvocationPending, CreatedAt: time.Now().UTC(),
	}
	claimed, stored, err := adapter.ClaimToolInvocation(ctx, pending)
	if err != nil || !claimed || stored.InvocationID != pending.InvocationID || retryingClient.createCalls != 2 {
		t.Fatalf("ClaimToolInvocation() = (%t, %#v, %v), create calls=%d; want claimed receipt after two creates", claimed, stored, err, retryingClient.createCalls)
	}

	retryingClient.remainingUpdateConflicts = 1
	completedAt := time.Now().UTC()
	completed := pending
	completed.Status = store.ToolInvocationSucceeded
	completed.CompletedAt = &completedAt
	if err := adapter.CompleteToolInvocation(ctx, completed); err != nil || retryingClient.updateCalls != 2 {
		t.Fatalf("CompleteToolInvocation() error = %v, update calls=%d; want nil after two updates", err, retryingClient.updateCalls)
	}
	claimed, stored, err = adapter.ClaimToolInvocation(ctx, pending)
	if err != nil || claimed || stored.Status != store.ToolInvocationSucceeded {
		t.Fatalf("stored receipt = (%t, %#v, %v), want terminal receipt", claimed, stored, err)
	}
}

func TestStoreAdapter_TokenLedgerRejectsNamespaceAnnotationTampering(t *testing.T) {
	quota := &k8s.CognitiveQuota{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "team-a",
			Name:      "default",
			UID:       types.UID("quota-uid-1"),
		},
		Spec: k8s.CognitiveQuotaSpec{
			TokenBudget: &k8s.TokenBudget{MaxTotalTokens: 1000},
		},
	}
	sharedClient := newFakeClient(t, quota)
	adapter := NewStoreAdapterWithControlNamespace(sharedClient, "kognition-system")
	ctx := context.Background()
	if _, err := adapter.CreateTokenReservation(ctx, store.TokenReservation{
		ID: "run-1", Namespace: "team-a", ReservedInput: 100,
	}); err != nil {
		t.Fatalf("CreateTokenReservation() error = %v", err)
	}

	configMap := &corev1.ConfigMap{}
	if err := sharedClient.Get(ctx, adapter.tokenLedgerObjectKey("team-a"), configMap); err != nil {
		t.Fatalf("get token ledger: %v", err)
	}
	configMap.Annotations[tokenLedgerNamespaceAnnotation] = "victim"
	if err := sharedClient.Update(ctx, configMap); err != nil {
		t.Fatalf("tamper token ledger annotation: %v", err)
	}
	if _, err := adapter.SettleTokenReservation(ctx, "run-1", 25, 0); err == nil || !strings.Contains(err.Error(), "invalid workload namespace binding") {
		t.Fatalf("SettleTokenReservation() error = %v, want invalid namespace binding", err)
	}
	assertK8sQuotaTokenUsage(t, sharedClient, "team-a", 100, 0, 100)
}

func TestStoreAdapter_SettlementRejectsUnrelatedInvalidTokenLedger(t *testing.T) {
	quota := &k8s.CognitiveQuota{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "default", UID: types.UID("quota-a")},
		Spec:       k8s.CognitiveQuotaSpec{TokenBudget: &k8s.TokenBudget{MaxTotalTokens: 1000}},
	}
	sharedClient := newFakeClient(t, quota)
	adapter := NewStoreAdapterWithControlNamespace(sharedClient, "kognition-system")
	ctx := context.Background()
	if created, err := adapter.CreateTokenReservation(ctx, store.TokenReservation{ID: "run-1", Namespace: "team-a", ReservedInput: 100}); err != nil || !created {
		t.Fatalf("CreateTokenReservation() = (%v, %v), want (true, nil)", created, err)
	}

	forgedKey := adapter.tokenLedgerObjectKey("team-z")
	forged := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: forgedKey.Namespace,
			Name:      forgedKey.Name,
			Labels:    map[string]string{tokenLedgerManagedLabel: tokenLedgerManagedLabelValue},
			Annotations: map[string]string{
				tokenLedgerNamespaceAnnotation: "team-z",
			},
		},
		Data: map[string]string{tokenLedgerDataKey: "{"},
	}
	if err := sharedClient.Create(ctx, forged); err != nil {
		t.Fatal(err)
	}

	found, err := adapter.SettleTokenReservation(ctx, "run-1", 25, 0)
	if err == nil || found || !strings.Contains(err.Error(), "decode token reservation ledger") {
		t.Fatalf("SettleTokenReservation() = (%v, %v), want malformed unrelated ledger error", found, err)
	}
	assertK8sQuotaTokenUsage(t, sharedClient, "team-a", 100, 0, 100)

	realLedger := &corev1.ConfigMap{}
	if err := sharedClient.Get(ctx, adapter.tokenLedgerObjectKey("team-a"), realLedger); err != nil {
		t.Fatal(err)
	}
	ledger, err := decodeTokenLedger(realLedger)
	if err != nil || ledger.Reservations["run-1"].ReservedInput != 100 {
		t.Fatalf("real token ledger = (%#v, %v), want untouched run-1 reservation", ledger, err)
	}
}

func TestStoreAdapter_TokenSettlementAcrossQuotaGeneration(t *testing.T) {
	for _, test := range []struct {
		name       string
		transition func(*testing.T, client.Client, *k8s.CognitiveQuota)
	}{
		{
			name: "reset",
			transition: func(t *testing.T, k8sClient client.Client, quota *k8s.CognitiveQuota) {
				t.Helper()
				quota.Status.TokenUsage = nil
				quota.Status.LastResetTime = time.Now().UTC().Format(time.RFC3339Nano)
				if err := k8sClient.Status().Update(context.Background(), quota); err != nil {
					t.Fatalf("reset CognitiveQuota status: %v", err)
				}
			},
		},
		{
			name: "recreated",
			transition: func(t *testing.T, k8sClient client.Client, quota *k8s.CognitiveQuota) {
				t.Helper()
				if err := k8sClient.Delete(context.Background(), quota); err != nil {
					t.Fatalf("delete CognitiveQuota: %v", err)
				}
				replacement := quota.DeepCopy()
				replacement.ResourceVersion = ""
				replacement.UID = types.UID("quota-uid-2")
				replacement.Status = k8s.CognitiveQuotaStatus{}
				if err := k8sClient.Create(context.Background(), replacement); err != nil {
					t.Fatalf("recreate CognitiveQuota: %v", err)
				}
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			quota := &k8s.CognitiveQuota{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: "team-a",
					Name:      "default",
					UID:       types.UID("quota-uid-1"),
				},
				Spec: k8s.CognitiveQuotaSpec{
					TokenBudget: &k8s.TokenBudget{MaxTotalTokens: 2000},
				},
			}
			sharedClient := newFakeClient(t, quota)
			adapter := NewStoreAdapterWithControlNamespace(sharedClient, "kognition-system")
			ctx := context.Background()
			if _, err := adapter.CreateTokenReservation(ctx, store.TokenReservation{
				ID: "run-1", Namespace: "team-a", ReservedInput: 400, ReservedOutput: 300,
			}); err != nil {
				t.Fatalf("CreateTokenReservation() error = %v", err)
			}
			latest := &k8s.CognitiveQuota{}
			if err := sharedClient.Get(ctx, client.ObjectKeyFromObject(quota), latest); err != nil {
				t.Fatalf("get CognitiveQuota before transition: %v", err)
			}
			test.transition(t, sharedClient, latest)

			found, err := adapter.SettleTokenReservation(ctx, "run-1", 125, 75)
			if err != nil || !found {
				t.Fatalf("SettleTokenReservation() = (%v, %v), want (true, nil)", found, err)
			}
			assertK8sQuotaTokenUsage(t, sharedClient, "team-a", 125, 75, 200)
		})
	}
}

func TestStoreAdapter_ReservationReplayAfterResetDoesNotRecharge(t *testing.T) {
	quota := &k8s.CognitiveQuota{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "team-a",
			Name:      "default",
			UID:       types.UID("quota-uid-1"),
		},
		Spec: k8s.CognitiveQuotaSpec{
			TokenBudget: &k8s.TokenBudget{MaxTotalTokens: 2000},
		},
	}
	sharedClient := newFakeClient(t, quota)
	adapter := NewStoreAdapterWithControlNamespace(sharedClient, "kognition-system")
	ctx := context.Background()
	reservation := store.TokenReservation{
		ID: "run-1", Namespace: "team-a", ReservedInput: 400, ReservedOutput: 300,
	}
	if created, err := adapter.CreateTokenReservation(ctx, reservation); err != nil || !created {
		t.Fatalf("CreateTokenReservation() = (%v, %v), want (true, nil)", created, err)
	}
	latest := &k8s.CognitiveQuota{}
	if err := sharedClient.Get(ctx, client.ObjectKeyFromObject(quota), latest); err != nil {
		t.Fatalf("get CognitiveQuota before reset: %v", err)
	}
	latest.Status.TokenUsage = nil
	latest.Status.LastResetTime = time.Now().UTC().Format(time.RFC3339Nano)
	if err := sharedClient.Status().Update(ctx, latest); err != nil {
		t.Fatalf("reset CognitiveQuota status: %v", err)
	}

	created, err := adapter.CreateTokenReservation(ctx, reservation)
	if err != nil || created {
		t.Fatalf("replayed CreateTokenReservation() = (%v, %v), want (false, nil)", created, err)
	}
	assertK8sQuotaTokenUsage(t, sharedClient, "team-a", 0, 0, 0)
	found, err := adapter.SettleTokenReservation(ctx, reservation.ID, 125, 75)
	if err != nil || !found {
		t.Fatalf("SettleTokenReservation() = (%v, %v), want (true, nil)", found, err)
	}
	assertK8sQuotaTokenUsage(t, sharedClient, "team-a", 125, 75, 200)
}

func TestStoreAdapter_SyncTokenQuotaStatusRetriesConflictAndSkipsStalePeriod(t *testing.T) {
	ctx := context.Background()
	quota := &k8s.CognitiveQuota{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "default", UID: types.UID("quota-uid-1")},
		Spec:       k8s.CognitiveQuotaSpec{TokenBudget: &k8s.TokenBudget{MaxTotalTokens: 1000}},
	}
	retryingClient := &retryingStatusClient{Client: newFakeClient(t, quota), remainingConflicts: 1}
	adapter := NewStoreAdapterWithControlNamespace(retryingClient, "kognition-system")
	usage := tokenLedgerQuota{Period: k8sQuotaPeriod(quota), InputTokens: 120, OutputTokens: 30, TotalTokens: 150}
	if err := adapter.syncTokenQuotaStatus(ctx, quota, usage); err != nil {
		t.Fatalf("syncTokenQuotaStatus() error = %v", err)
	}
	if retryingClient.statusUpdates != 2 {
		t.Fatalf("status updates = %d, want 2 after one conflict", retryingClient.statusUpdates)
	}
	assertK8sQuotaTokenUsage(t, retryingClient.Client, "team-a", 120, 30, 150)

	latest := &k8s.CognitiveQuota{}
	if err := retryingClient.Get(ctx, client.ObjectKeyFromObject(quota), latest); err != nil {
		t.Fatal(err)
	}
	latest.Status.LastResetTime = "new-period"
	if err := retryingClient.Status().Update(ctx, latest); err != nil {
		t.Fatal(err)
	}
	updatesBefore := retryingClient.statusUpdates
	if err := adapter.syncTokenQuotaStatus(ctx, latest, usage); err != nil {
		t.Fatalf("stale-period syncTokenQuotaStatus() error = %v", err)
	}
	if retryingClient.statusUpdates != updatesBefore {
		t.Fatalf("stale-period status updates = %d, want no additional projection write", retryingClient.statusUpdates)
	}
	assertK8sQuotaTokenUsage(t, retryingClient.Client, "team-a", 120, 30, 150)
}

func TestStoreAdapter_SyncTokenLedgerStatusRetriesWhenLedgerChanges(t *testing.T) {
	ctx := context.Background()
	quota := &k8s.CognitiveQuota{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "default", UID: types.UID("quota-uid-1")},
		Spec:       k8s.CognitiveQuotaSpec{TokenBudget: &k8s.TokenBudget{MaxTotalTokens: 1000}},
	}
	baseClient := newFakeClient(t, quota)
	adapter := NewStoreAdapterWithControlNamespace(baseClient, "kognition-system")
	key := adapter.tokenLedgerObjectKey("team-a")
	period := k8sQuotaPeriod(quota)
	initialLedger := &tokenLedger{Reservations: map[string]tokenLedgerReservation{}, Quotas: map[string]tokenLedgerQuota{
		"default": {Period: period, InputTokens: 100, OutputTokens: 20, TotalTokens: 120},
	}}
	updatedLedger := &tokenLedger{Reservations: map[string]tokenLedgerReservation{}, Quotas: map[string]tokenLedgerQuota{
		"default": {Period: period, InputTokens: 125, OutputTokens: 25, TotalTokens: 150},
	}}
	initialData, err := json.Marshal(initialLedger)
	if err != nil {
		t.Fatal(err)
	}
	updatedData, err := json.Marshal(updatedLedger)
	if err != nil {
		t.Fatal(err)
	}
	ledger := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: key.Namespace,
			Name:      key.Name,
			Labels:    map[string]string{tokenLedgerManagedLabel: tokenLedgerManagedLabelValue},
			Annotations: map[string]string{
				tokenLedgerNamespaceAnnotation: "team-a",
			},
		},
		Data: map[string]string{tokenLedgerDataKey: string(initialData)},
	}
	if err := baseClient.Create(ctx, ledger); err != nil {
		t.Fatal(err)
	}
	changingClient := &changingTokenLedgerClient{Client: baseClient, ledgerKey: key, replacementData: string(updatedData)}
	adapter = NewStoreAdapterWithControlNamespace(changingClient, "kognition-system")

	if err := adapter.syncTokenLedgerStatus(ctx, "team-a"); err != nil {
		t.Fatalf("syncTokenLedgerStatus() error = %v", err)
	}
	if changingClient.ledgerGets < 4 {
		t.Fatalf("ledger reads = %d, want retry after ConfigMap version change", changingClient.ledgerGets)
	}
	assertK8sQuotaTokenUsage(t, baseClient, "team-a", 125, 25, 150)
}

func TestStoreAdapter_MultiQuotaReservationRejectsAtomically(t *testing.T) {
	firstQuota := &k8s.CognitiveQuota{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "first", UID: types.UID("quota-uid-1")},
		Spec:       k8s.CognitiveQuotaSpec{TokenBudget: &k8s.TokenBudget{MaxTotalTokens: 1000}},
	}
	secondQuota := &k8s.CognitiveQuota{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "second", UID: types.UID("quota-uid-2")},
		Spec:       k8s.CognitiveQuotaSpec{TokenBudget: &k8s.TokenBudget{MaxTotalTokens: 50}},
	}
	sharedClient := newFakeClient(t, firstQuota, secondQuota)
	adapter := NewStoreAdapterWithControlNamespace(sharedClient, "kognition-system")
	_, err := adapter.CreateTokenReservation(context.Background(), store.TokenReservation{
		ID: "run-1", Namespace: "team-a", ReservedInput: 60,
	})
	if !errors.Is(err, store.ErrQuotaExceeded) {
		t.Fatalf("CreateTokenReservation() error = %v, want ErrQuotaExceeded", err)
	}
	if err := sharedClient.Get(context.Background(), adapter.tokenLedgerObjectKey("team-a"), &corev1.ConfigMap{}); !apierrors.IsNotFound(err) {
		t.Fatalf("token ledger after rejected reservation error = %v, want not found", err)
	}
	for _, name := range []string{"first", "second"} {
		quota := &k8s.CognitiveQuota{}
		if err := sharedClient.Get(context.Background(), client.ObjectKey{Namespace: "team-a", Name: name}, quota); err != nil {
			t.Fatalf("get CognitiveQuota %q: %v", name, err)
		}
		if quota.Status.TokenUsage != nil {
			t.Fatalf("CognitiveQuota %q usage = %+v, want nil", name, quota.Status.TokenUsage)
		}
	}
}

func TestStoreAdapter_SettlementRetryRepairsStatusProjection(t *testing.T) {
	quota := &k8s.CognitiveQuota{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "team-a",
			Name:      "default",
			UID:       types.UID("quota-uid-1"),
		},
		Spec: k8s.CognitiveQuotaSpec{
			TokenBudget: &k8s.TokenBudget{MaxTotalTokens: 1000},
		},
	}
	sharedClient := newFakeClient(t, quota)
	adapter := NewStoreAdapterWithControlNamespace(sharedClient, "kognition-system")
	ctx := context.Background()
	if _, err := adapter.CreateTokenReservation(ctx, store.TokenReservation{
		ID: "run-1", Namespace: "team-a", ReservedInput: 400, ReservedOutput: 300,
	}); err != nil {
		t.Fatalf("CreateTokenReservation() error = %v", err)
	}

	configMap := &corev1.ConfigMap{}
	if err := sharedClient.Get(ctx, adapter.tokenLedgerObjectKey("team-a"), configMap); err != nil {
		t.Fatalf("get token ledger: %v", err)
	}
	ledger, err := decodeTokenLedger(configMap)
	if err != nil {
		t.Fatalf("decode token ledger: %v", err)
	}
	delete(ledger.Reservations, "run-1")
	usage := ledger.Quotas["default"]
	usage.InputTokens = 125
	usage.OutputTokens = 75
	usage.TotalTokens = 200
	ledger.Quotas["default"] = usage
	data, err := json.Marshal(ledger)
	if err != nil {
		t.Fatalf("marshal token ledger: %v", err)
	}
	configMap.Data[tokenLedgerDataKey] = string(data)
	if err := sharedClient.Update(ctx, configMap); err != nil {
		t.Fatalf("persist settled token ledger: %v", err)
	}
	assertK8sQuotaTokenUsage(t, sharedClient, "team-a", 400, 300, 700)

	found, err := adapter.SettleTokenReservation(ctx, "run-1", 125, 75)
	if err != nil || found {
		t.Fatalf("idempotent SettleTokenReservation() = (%v, %v), want (false, nil)", found, err)
	}
	assertK8sQuotaTokenUsage(t, sharedClient, "team-a", 125, 75, 200)
}

func TestTokenLedgerHelpersHandleDefaultsAndInvalidData(t *testing.T) {
	configMap := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "ledger", Namespace: "team-a"}}
	ledger, err := decodeTokenLedger(configMap)
	if err != nil {
		t.Fatalf("decode empty token ledger: %v", err)
	}
	if ledger.Reservations == nil || ledger.Quotas == nil || len(ledger.Reservations) != 0 || len(ledger.Quotas) != 0 {
		t.Fatalf("empty decoded ledger = %#v, want initialized empty maps", ledger)
	}

	configMap.Data = map[string]string{tokenLedgerDataKey: `{"reservations":null,"quotas":null}`}
	ledger, err = decodeTokenLedger(configMap)
	if err != nil {
		t.Fatalf("decode null-map token ledger: %v", err)
	}
	if ledger.Reservations == nil || ledger.Quotas == nil {
		t.Fatalf("null-map decoded ledger = %#v, want initialized maps", ledger)
	}

	configMap.Data[tokenLedgerDataKey] = `{not-json}`
	if _, err := decodeTokenLedger(configMap); err == nil || !strings.Contains(err.Error(), "decode token reservation ledger team-a/ledger") {
		t.Fatalf("decode malformed token ledger error = %v, want contextual decode error", err)
	}
}

func TestK8sQuotaPeriodUsesStableFallbacks(t *testing.T) {
	createdAt := metav1.NewTime(time.Date(2025, 3, 1, 2, 3, 4, 5, time.UTC))
	tests := []struct {
		name  string
		quota k8s.CognitiveQuota
		want  string
	}{
		{
			name:  "uid",
			quota: k8s.CognitiveQuota{ObjectMeta: metav1.ObjectMeta{UID: types.UID("quota-uid")}, Status: k8s.CognitiveQuotaStatus{LastResetTime: "reset-1"}},
			want:  "quota-uid|reset-1",
		},
		{
			name:  "creation timestamp",
			quota: k8s.CognitiveQuota{ObjectMeta: metav1.ObjectMeta{Name: "default", CreationTimestamp: createdAt}, Status: k8s.CognitiveQuotaStatus{LastResetTime: "reset-2"}},
			want:  createdAt.UTC().Format(time.RFC3339Nano) + "|reset-2",
		},
		{
			name:  "name",
			quota: k8s.CognitiveQuota{ObjectMeta: metav1.ObjectMeta{Name: "default"}},
			want:  "default|",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := k8sQuotaPeriod(&tt.quota); got != tt.want {
				t.Fatalf("k8sQuotaPeriod() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestTokenLedgerArithmeticRejectsInvalidValues(t *testing.T) {
	maxInt64 := int64(^uint64(0) >> 1)
	if got, err := addK8sTokenUsage(2, 3); err != nil || got != 5 {
		t.Fatalf("addK8sTokenUsage(2, 3) = (%d, %v), want (5, nil)", got, err)
	}
	for _, tt := range []struct {
		name  string
		left  int64
		right int64
	}{
		{name: "negative left", left: -1, right: 1},
		{name: "negative right", left: 1, right: -1},
		{name: "overflow", left: maxInt64, right: 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := addK8sTokenUsage(tt.left, tt.right); err == nil {
				t.Fatalf("addK8sTokenUsage(%d, %d) error = nil, want error", tt.left, tt.right)
			}
		})
	}

	if got, err := addK8sTokenDelta(5, -3); err != nil || got != 2 {
		t.Fatalf("addK8sTokenDelta(5, -3) = (%d, %v), want (2, nil)", got, err)
	}
	for _, tt := range []struct {
		name    string
		current int64
		delta   int64
	}{
		{name: "negative result", current: 2, delta: -3},
		{name: "overflow", current: maxInt64, delta: 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := addK8sTokenDelta(tt.current, tt.delta); err == nil {
				t.Fatalf("addK8sTokenDelta(%d, %d) error = nil, want error", tt.current, tt.delta)
			}
		})
	}
}

func assertK8sQuotaTokenUsage(t *testing.T, k8sClient client.Client, namespace string, input, output, total int64) {
	t.Helper()
	quota := &k8s.CognitiveQuota{}
	if err := k8sClient.Get(context.Background(), client.ObjectKey{Namespace: namespace, Name: "default"}, quota); err != nil {
		t.Fatalf("get CognitiveQuota: %v", err)
	}
	if quota.Status.TokenUsage == nil ||
		quota.Status.TokenUsage.InputTokens != input ||
		quota.Status.TokenUsage.OutputTokens != output ||
		quota.Status.TokenUsage.TotalTokens != total {
		t.Fatalf("CognitiveQuota token usage = %+v, want %d/%d/%d", quota.Status.TokenUsage, input, output, total)
	}
}

// --- CognitiveLimitRange round-trip tests ---

func TestStoreAdapter_RoundTrip_CognitiveLimitRange(t *testing.T) {
	fc := newFakeClient(t)
	sa := NewStoreAdapter(fc)

	internal := api.CognitiveLimitRange{
		TypeMeta:   api.TypeMeta{APIVersion: api.APIVersion, Kind: "CognitiveLimitRange"},
		ObjectMeta: api.ObjectMeta{Namespace: "ns1", Name: "lr-1"},
		Spec: api.CognitiveLimitRangeSpec{
			Limits: []api.LimitRangeItem{
				{
					Type:             "Agent",
					TokensPerSession: 50000,
					TimeoutSeconds:   300,
				},
			},
		},
	}
	data, _ := json.Marshal(internal)
	if err := sa.Create(context.Background(), "CognitiveLimitRange", "ns1", "lr-1", data); err != nil {
		t.Fatalf("create: %v", err)
	}

	gotData, err := sa.Get(context.Background(), "CognitiveLimitRange", "ns1", "lr-1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}

	var got api.CognitiveLimitRange
	if err := json.Unmarshal(gotData, &got); err != nil {
		t.Fatalf("unmarshal round-trip: %v", err)
	}
	if got.ObjectMeta.Name != "lr-1" {
		t.Errorf("name = %q, want %q", got.ObjectMeta.Name, "lr-1")
	}
	if len(got.Spec.Limits) != 1 {
		t.Fatalf("limits len = %d, want 1", len(got.Spec.Limits))
	}
	if got.Spec.Limits[0].TokensPerSession != 50000 {
		t.Errorf("tokensPerSession = %d, want 50000", got.Spec.Limits[0].TokensPerSession)
	}
	if got.Spec.Limits[0].TimeoutSeconds != 300 {
		t.Errorf("timeoutSeconds = %d, want 300", got.Spec.Limits[0].TimeoutSeconds)
	}
}

// --- Workspace round-trip tests ---

func TestStoreAdapter_RoundTrip_Workspace(t *testing.T) {
	fc := newFakeClient(t)
	sa := NewStoreAdapter(fc)

	internal := api.Workspace{
		TypeMeta:   api.TypeMeta{APIVersion: api.APIVersion, Kind: "Workspace"},
		ObjectMeta: api.ObjectMeta{Namespace: "ns1", Name: "ws-1"},
		Spec: api.WorkspaceSpec{
			DisplayName:            "Test Workspace",
			RequireSignedArtifacts: true,
		},
	}
	data, _ := json.Marshal(internal)
	if err := sa.Create(context.Background(), "Workspace", "ns1", "ws-1", data); err != nil {
		t.Fatalf("create: %v", err)
	}

	gotData, err := sa.Get(context.Background(), "Workspace", "ns1", "ws-1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}

	var got api.Workspace
	if err := json.Unmarshal(gotData, &got); err != nil {
		t.Fatalf("unmarshal round-trip: %v", err)
	}
	if got.ObjectMeta.Name != "ws-1" {
		t.Errorf("name = %q, want %q", got.ObjectMeta.Name, "ws-1")
	}
	if got.Spec.DisplayName != "Test Workspace" {
		t.Errorf("displayName = %q, want %q", got.Spec.DisplayName, "Test Workspace")
	}
	if !got.Spec.RequireSignedArtifacts {
		t.Error("requireSignedArtifacts = false, want true")
	}
}

// --- AgentIdentity round-trip tests ---

func TestStoreAdapter_RoundTrip_AgentIdentity(t *testing.T) {
	fc := newFakeClient(t)
	sa := NewStoreAdapter(fc)

	internal := api.AgentIdentity{
		TypeMeta:   api.TypeMeta{APIVersion: api.APIVersion, Kind: "AgentIdentity"},
		ObjectMeta: api.ObjectMeta{Namespace: "ns1", Name: "id-1"},
		Spec: api.AgentIdentitySpec{
			SPIFFEID: "spiffe://cluster.local/ns/ns1/sa/agent-1",
			AgentRef: api.LocalObjectReference{Name: "agent-1"},
		},
		Status: api.AgentIdentityStatus{WorkloadIdentity: &api.WorkloadIdentityStatus{
			AgentUID: "agent-uid", SPIFFEID: "spiffe://cluster.local/ns/ns1/sa/agent-1", TrustDomain: "cluster.local",
			NotBefore: time.Now().UTC().Truncate(time.Second), ExpiresAt: time.Now().UTC().Add(time.Hour).Truncate(time.Second),
			SourceGeneration: 2, Ready: true, Reason: "Ready",
		}},
	}
	createdIdentity := internal
	createdIdentity.Status = api.AgentIdentityStatus{}
	data, _ := json.Marshal(createdIdentity)
	if err := sa.Create(context.Background(), "AgentIdentity", "ns1", "id-1", data); err != nil {
		t.Fatalf("create: %v", err)
	}

	gotData, err := sa.Get(context.Background(), "AgentIdentity", "ns1", "id-1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}

	var got api.AgentIdentity
	if err := json.Unmarshal(gotData, &got); err != nil {
		t.Fatalf("unmarshal round-trip: %v", err)
	}
	if got.Status.WorkloadIdentity != nil {
		t.Fatal("resource creation must not publish controller certificate status")
	}
	got.Status = internal.Status
	published, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if err := sa.Update(context.Background(), "AgentIdentity", "ns1", "id-1", published, got.ObjectMeta.ResourceVersion); err != nil {
		t.Fatal(err)
	}
	gotData, err = sa.Get(context.Background(), "AgentIdentity", "ns1", "id-1")
	got = api.AgentIdentity{}
	if err != nil || json.Unmarshal(gotData, &got) != nil {
		t.Fatal("cannot read published certificate status")
	}
	if got.ObjectMeta.Name != "id-1" {
		t.Errorf("name = %q, want %q", got.ObjectMeta.Name, "id-1")
	}
	if got.Spec.SPIFFEID != "spiffe://cluster.local/ns/ns1/sa/agent-1" {
		t.Errorf("spiffeID = %q, want expected", got.Spec.SPIFFEID)
	}
	if got.Spec.AgentRef.Name != "agent-1" {
		t.Errorf("agentRef = %q, want %q", got.Spec.AgentRef.Name, "agent-1")
	}
	if got.Status.WorkloadIdentity == nil {
		t.Fatal("Kubernetes store dropped certificate evidence")
	}
	observedStatus := *got.Status.WorkloadIdentity
	expectedStatus := *internal.Status.WorkloadIdentity
	if !observedStatus.NotBefore.Equal(expectedStatus.NotBefore) || !observedStatus.ExpiresAt.Equal(expectedStatus.ExpiresAt) {
		t.Fatal("Kubernetes store changed certificate validity")
	}
	observedStatus.NotBefore, observedStatus.ExpiresAt = time.Time{}, time.Time{}
	expectedStatus.NotBefore, expectedStatus.ExpiresAt = time.Time{}, time.Time{}
	if observedStatus != expectedStatus {
		t.Fatal("Kubernetes store changed certificate metadata")
	}
	got.Status.WorkloadIdentity.Ready = false
	got.Status.WorkloadIdentity.Reason = "IssuerUnavailable"
	updated, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if err := sa.Update(context.Background(), "AgentIdentity", "ns1", "id-1", updated, got.ObjectMeta.ResourceVersion); err != nil {
		t.Fatal(err)
	}
	raw, err := sa.Get(context.Background(), "AgentIdentity", "ns1", "id-1")
	var unready api.AgentIdentity
	if err != nil || json.Unmarshal(raw, &unready) != nil || unready.Status.WorkloadIdentity == nil || unready.Status.WorkloadIdentity.Ready || unready.Status.WorkloadIdentity.Reason != "IssuerUnavailable" {
		t.Fatal("Kubernetes status update retained stale certificate readiness")
	}
}

// --- AgentRole round-trip tests ---

func TestStoreAdapter_RoundTrip_AgentRole(t *testing.T) {
	fc := newFakeClient(t)
	sa := NewStoreAdapter(fc)

	internal := api.AgentRole{
		TypeMeta:   api.TypeMeta{APIVersion: api.APIVersion, Kind: "AgentRole"},
		ObjectMeta: api.ObjectMeta{Namespace: "ns1", Name: "role-1"},
		Rules: []api.PolicyRule{
			{Resources: []string{"Tool", "Model"}, Verbs: []string{"get", "list"}},
		},
	}
	data, _ := json.Marshal(internal)
	if err := sa.Create(context.Background(), "AgentRole", "ns1", "role-1", data); err != nil {
		t.Fatalf("create: %v", err)
	}

	gotData, err := sa.Get(context.Background(), "AgentRole", "ns1", "role-1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}

	var got api.AgentRole
	if err := json.Unmarshal(gotData, &got); err != nil {
		t.Fatalf("unmarshal round-trip: %v", err)
	}
	if got.ObjectMeta.Name != "role-1" {
		t.Errorf("name = %q, want %q", got.ObjectMeta.Name, "role-1")
	}
	if len(got.Rules) != 1 {
		t.Fatalf("rules len = %d, want 1", len(got.Rules))
	}
	if len(got.Rules[0].Resources) != 2 {
		t.Errorf("resources = %v, want [Tool Model]", got.Rules[0].Resources)
	}
	if len(got.Rules[0].Verbs) != 2 {
		t.Errorf("verbs = %v, want [get list]", got.Rules[0].Verbs)
	}
}

// --- AgentRoleBinding round-trip tests ---

func TestStoreAdapter_RoundTrip_AgentRoleBinding(t *testing.T) {
	fc := newFakeClient(t)
	sa := NewStoreAdapter(fc)

	internal := api.AgentRoleBinding{
		TypeMeta:   api.TypeMeta{APIVersion: api.APIVersion, Kind: "AgentRoleBinding"},
		ObjectMeta: api.ObjectMeta{Namespace: "ns1", Name: "binding-1"},
		Spec: api.AgentRoleBindingSpec{
			RoleRef:  api.LocalObjectReference{Name: "role-1"},
			Subjects: []api.Subject{{Kind: "Agent", Name: "agent-1"}},
		},
	}
	data, _ := json.Marshal(internal)
	if err := sa.Create(context.Background(), "AgentRoleBinding", "ns1", "binding-1", data); err != nil {
		t.Fatalf("create: %v", err)
	}

	gotData, err := sa.Get(context.Background(), "AgentRoleBinding", "ns1", "binding-1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}

	var got api.AgentRoleBinding
	if err := json.Unmarshal(gotData, &got); err != nil {
		t.Fatalf("unmarshal round-trip: %v", err)
	}
	if got.ObjectMeta.Name != "binding-1" {
		t.Errorf("name = %q, want %q", got.ObjectMeta.Name, "binding-1")
	}
	if got.Spec.RoleRef.Name != "role-1" {
		t.Errorf("roleRef = %q, want %q", got.Spec.RoleRef.Name, "role-1")
	}
	if len(got.Spec.Subjects) != 1 {
		t.Fatalf("subjects len = %d, want 1", len(got.Spec.Subjects))
	}
	if got.Spec.Subjects[0].Kind != "Agent" || got.Spec.Subjects[0].Name != "agent-1" {
		t.Errorf("subject = %+v, want {Kind:Agent Name:agent-1}", got.Spec.Subjects[0])
	}
}

// --- WorkflowRun round-trip tests ---

func TestStoreAdapter_RoundTrip_WorkflowRun(t *testing.T) {
	fc := newFakeClient(t)
	sa := NewStoreAdapter(fc)

	internal := api.WorkflowRun{
		TypeMeta:   api.TypeMeta{APIVersion: api.APIVersion, Kind: "WorkflowRun"},
		ObjectMeta: api.ObjectMeta{Namespace: "ns1", Name: "run-1"},
		Spec: api.WorkflowRunSpec{
			WorkflowRef: api.LocalObjectReference{Name: "my-workflow"},
			Input:       "process this data",
			Parameters:  map[string]string{"timeout": "60s"},
		},
	}
	data, _ := json.Marshal(internal)
	if err := sa.Create(context.Background(), "WorkflowRun", "ns1", "run-1", data); err != nil {
		t.Fatalf("create: %v", err)
	}

	gotData, err := sa.Get(context.Background(), "WorkflowRun", "ns1", "run-1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}

	var got api.WorkflowRun
	if err := json.Unmarshal(gotData, &got); err != nil {
		t.Fatalf("unmarshal round-trip: %v", err)
	}
	if got.ObjectMeta.Name != "run-1" {
		t.Errorf("name = %q, want %q", got.ObjectMeta.Name, "run-1")
	}
	if got.Spec.WorkflowRef.Name != "my-workflow" {
		t.Errorf("workflowRef = %q, want %q", got.Spec.WorkflowRef.Name, "my-workflow")
	}
	if got.Spec.Input != "process this data" {
		t.Errorf("input = %q, want %q", got.Spec.Input, "process this data")
	}
	if got.Spec.Parameters["timeout"] != "60s" {
		t.Errorf("parameters[timeout] = %q, want %q", got.Spec.Parameters["timeout"], "60s")
	}
}

// --- Model round-trip test ---

func TestStoreAdapter_RoundTrip_Model(t *testing.T) {
	fc := newFakeClient(t)
	sa := NewStoreAdapter(fc)

	rateLimit := int64(120)
	internal := api.Model{
		TypeMeta:   api.TypeMeta{APIVersion: api.APIVersion, Kind: "Model"},
		ObjectMeta: api.ObjectMeta{Namespace: "ns1", Name: "gpt-prod"},
		Spec: api.ModelSpec{
			Provider:      "openai-compatible",
			ClassName:     "gpt-class",
			ModelID:       "gpt-4o",
			Endpoint:      "https://api.example.com/v1",
			Capabilities:  []string{"chat", "tools"},
			CredentialRef: &api.LocalObjectReference{Name: "gpt-secret"},
			RateLimit:     &rateLimit,
			Parameters:    map[string]string{"temperature": "0.2"},
			Signature:     "sig-abc",
		},
	}
	data, _ := json.Marshal(internal)
	if err := sa.Create(context.Background(), "Model", "ns1", "gpt-prod", data); err != nil {
		t.Fatalf("create: %v", err)
	}

	gotData, err := sa.Get(context.Background(), "Model", "ns1", "gpt-prod")
	if err != nil {
		t.Fatalf("get: %v", err)
	}

	var got api.Model
	if err := json.Unmarshal(gotData, &got); err != nil {
		t.Fatalf("unmarshal round-trip: %v", err)
	}
	if got.ObjectMeta.Name != "gpt-prod" {
		t.Errorf("name = %q, want %q", got.ObjectMeta.Name, "gpt-prod")
	}
	if got.Spec.Provider != "openai-compatible" {
		t.Errorf("provider = %q, want openai-compatible", got.Spec.Provider)
	}
	if got.Spec.ClassName != "gpt-class" {
		t.Errorf("className = %q, want gpt-class", got.Spec.ClassName)
	}
	if got.Spec.ModelID != "gpt-4o" {
		t.Errorf("modelID = %q, want gpt-4o", got.Spec.ModelID)
	}
	if got.Spec.Endpoint != "https://api.example.com/v1" {
		t.Errorf("endpoint = %q", got.Spec.Endpoint)
	}
	if len(got.Spec.Capabilities) != 2 || got.Spec.Capabilities[0] != "chat" || got.Spec.Capabilities[1] != "tools" {
		t.Errorf("capabilities = %v", got.Spec.Capabilities)
	}
	if got.Spec.CredentialRef == nil || got.Spec.CredentialRef.Name != "gpt-secret" {
		t.Errorf("credentialRef = %+v", got.Spec.CredentialRef)
	}
	if got.Spec.RateLimit == nil || *got.Spec.RateLimit != 120 {
		t.Errorf("rateLimit = %v, want 120", got.Spec.RateLimit)
	}
	if got.Spec.Parameters["temperature"] != "0.2" {
		t.Errorf("parameters[temperature] = %q", got.Spec.Parameters["temperature"])
	}
	if got.Spec.Signature != "sig-abc" {
		t.Errorf("signature = %q", got.Spec.Signature)
	}
}

func TestStoreAdapter_List_Model(t *testing.T) {
	model := &k8s.Model{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:       "ns1",
			Name:            "gpt-list",
			ResourceVersion: "7",
		},
		Spec: k8s.ModelSpec{
			Provider:  "openai-compatible",
			ClassName: "gpt-class",
			ModelID:   "gpt-4o-mini",
		},
	}
	fc := newFakeClient(t, model)
	sa := NewStoreAdapter(fc)

	results, err := sa.List(context.Background(), "Model", store.ListOptions{Namespace: "ns1"})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}

	var got api.Model
	if err := json.Unmarshal(results[0], &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.ObjectMeta.Name != "gpt-list" {
		t.Errorf("name = %q, want %q", got.ObjectMeta.Name, "gpt-list")
	}
	if got.Spec.ModelID != "gpt-4o-mini" {
		t.Errorf("modelID = %q, want gpt-4o-mini", got.Spec.ModelID)
	}
}

// --- SecretRef round-trip test ---

func TestStoreAdapter_RoundTrip_SecretRef(t *testing.T) {
	fc := newFakeClient(t)
	sa := NewStoreAdapter(fc)

	internal := api.SecretRef{
		TypeMeta:   api.TypeMeta{APIVersion: api.APIVersion, Kind: "SecretRef"},
		ObjectMeta: api.ObjectMeta{Namespace: "ns1", Name: "openai-cred"},
		Spec: api.SecretRefSpec{
			BackendType: "kubernetes",
			BackendRef:  "openai-secret",
			Key:         "apiKey",
			Provider:    "kubernetes",
			Path:        "openai-secret",
			LegacyKey:   "apiKey",
		},
	}
	data, _ := json.Marshal(internal)
	if err := sa.Create(context.Background(), "SecretRef", "ns1", "openai-cred", data); err != nil {
		t.Fatalf("create: %v", err)
	}

	gotData, err := sa.Get(context.Background(), "SecretRef", "ns1", "openai-cred")
	if err != nil {
		t.Fatalf("get: %v", err)
	}

	var got api.SecretRef
	if err := json.Unmarshal(gotData, &got); err != nil {
		t.Fatalf("unmarshal round-trip: %v", err)
	}
	if got.ObjectMeta.Name != "openai-cred" {
		t.Errorf("name = %q, want %q", got.ObjectMeta.Name, "openai-cred")
	}
	if got.Spec.BackendType != "kubernetes" {
		t.Errorf("backendType = %q", got.Spec.BackendType)
	}
	if got.Spec.BackendRef != "openai-secret" {
		t.Errorf("backendRef = %q", got.Spec.BackendRef)
	}
	if got.Spec.Key != "apiKey" {
		t.Errorf("key = %q, want apiKey", got.Spec.Key)
	}
	if got.Spec.Provider != "kubernetes" {
		t.Errorf("provider = %q", got.Spec.Provider)
	}
	if got.Spec.Path != "openai-secret" {
		t.Errorf("path = %q", got.Spec.Path)
	}
	if got.Spec.LegacyKey != "apiKey" {
		t.Errorf("legacyKey = %q, want apiKey", got.Spec.LegacyKey)
	}
}

func TestStoreAdapter_List_SecretRef(t *testing.T) {
	sr := &k8s.SecretRef{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:       "ns1",
			Name:            "sr-list",
			ResourceVersion: "9",
		},
		Spec: k8s.SecretRefSpec{
			BackendType: "kubernetes",
			BackendRef:  "my-secret",
			Key:         "token",
		},
	}
	fc := newFakeClient(t, sr)
	sa := NewStoreAdapter(fc)

	results, err := sa.List(context.Background(), "SecretRef", store.ListOptions{Namespace: "ns1"})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}

	var got api.SecretRef
	if err := json.Unmarshal(results[0], &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.ObjectMeta.Name != "sr-list" {
		t.Errorf("name = %q, want %q", got.ObjectMeta.Name, "sr-list")
	}
	if got.Spec.BackendRef != "my-secret" {
		t.Errorf("backendRef = %q", got.Spec.BackendRef)
	}
	if got.Spec.Key != "token" {
		t.Errorf("key = %q", got.Spec.Key)
	}
}

// --- interface compliance ---

var _ store.Store = &StoreAdapter{}
