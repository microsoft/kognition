package apiserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	api "github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/auth"
	"github.com/kognition-ai/kognition/internal/controlplaneauth"
	"github.com/kognition-ai/kognition/internal/observability"
	"github.com/kognition-ai/kognition/internal/runtimeapproval"
	"github.com/kognition-ai/kognition/internal/store"
	"go.uber.org/zap"
)

type workflowBindingAPITestStore struct {
	APIServerStore
	binding         runtimeapproval.WorkflowExecutorAuthority
	acceptance      runtimeapproval.WorkflowRunAcceptance
	acceptanceError error
}

func (storage *workflowBindingAPITestStore) CreateWorkflowExecutorAuthority(_ context.Context, binding runtimeapproval.WorkflowExecutorAuthority) (runtimeapproval.WorkflowExecutorAuthority, error) {
	if storage.binding.Reference.UID != "" {
		return runtimeapproval.WorkflowExecutorAuthority{}, store.ErrAlreadyExists
	}
	binding.Reference.UID, binding.Reference.ResourceVersion = "persisted-binding-uid", 1
	storage.binding = binding
	return binding, nil
}

func (storage *workflowBindingAPITestStore) UpdateWorkflowExecutorAuthority(_ context.Context, binding runtimeapproval.WorkflowExecutorAuthority) (runtimeapproval.WorkflowExecutorAuthority, error) {
	if binding.Reference != storage.binding.Reference {
		return runtimeapproval.WorkflowExecutorAuthority{}, store.ErrConflict
	}
	binding.Reference.ResourceVersion++
	storage.binding = binding
	return binding, nil
}

func (storage *workflowBindingAPITestStore) GetWorkflowExecutorAuthority(_ context.Context, reference api.RuntimeApprovalReference) (runtimeapproval.WorkflowExecutorAuthority, error) {
	if storage.binding.Reference.Namespace != reference.Namespace || storage.binding.Reference.Name != reference.Name {
		return runtimeapproval.WorkflowExecutorAuthority{}, store.ErrNotFound
	}
	return storage.binding, nil
}

func (storage *workflowBindingAPITestStore) CreateWorkflowRunAcceptance(_ context.Context, acceptance runtimeapproval.WorkflowRunAcceptance) (runtimeapproval.WorkflowRunAcceptance, error) {
	storage.acceptance = acceptance
	return acceptance, nil
}

func (storage *workflowBindingAPITestStore) GetWorkflowRunAcceptance(_ context.Context, workspace, runUID string) (runtimeapproval.WorkflowRunAcceptance, error) {
	if storage.acceptance.Workspace != workspace || storage.acceptance.RunUID != runUID {
		return runtimeapproval.WorkflowRunAcceptance{}, store.ErrNotFound
	}
	return storage.acceptance, nil
}

func (storage *workflowBindingAPITestStore) CreateAndGet(ctx context.Context, kind, namespace, name string, raw []byte) ([]byte, error) {
	var run api.WorkflowRun
	if err := json.Unmarshal(raw, &run); err != nil {
		return nil, err
	}
	run.UID, run.ResourceVersion = "persisted-run-uid", 1
	encoded, err := json.Marshal(run)
	if err != nil {
		return nil, err
	}
	return encoded, storage.Create(ctx, kind, namespace, name, encoded)
}

func (storage *workflowBindingAPITestStore) CreateAcceptedWorkflowRun(ctx context.Context, reference api.RuntimeApprovalReference, raw []byte, duration int64) (api.WorkflowRun, runtimeapproval.WorkflowRunAcceptance, error) {
	var run api.WorkflowRun
	if storage.acceptanceError != nil {
		return run, runtimeapproval.WorkflowRunAcceptance{}, storage.acceptanceError
	}
	if reference != storage.binding.Reference {
		return run, runtimeapproval.WorkflowRunAcceptance{}, store.ErrConflict
	}
	encoded, err := storage.CreateAndGet(ctx, "WorkflowRun", reference.Namespace, "accepted-run", raw)
	if err != nil {
		return run, runtimeapproval.WorkflowRunAcceptance{}, err
	}
	if err := json.Unmarshal(encoded, &run); err != nil {
		return run, runtimeapproval.WorkflowRunAcceptance{}, err
	}
	initiator, err := runtimeapproval.AuthenticatedWorkflowExecutor(ctx)
	if err != nil {
		return run, runtimeapproval.WorkflowRunAcceptance{}, err
	}
	now := storage.binding.NotBefore.Add(time.Minute)
	acceptance := runtimeapproval.WorkflowRunAcceptance{Workspace: run.Namespace, RunUID: run.UID,
		Run:      api.RuntimeApprovalReference{Kind: "WorkflowRun", Scope: "Namespaced", Namespace: run.Namespace, Name: run.Name, UID: run.UID, ResourceVersion: run.ResourceVersion},
		Workflow: storage.binding.Workflow, WorkflowRevisionDigest: storage.binding.WorkflowRevisionDigest, ExecutionBinding: storage.binding.Reference, Executor: storage.binding.Executor, Initiator: initiator, AcceptedAt: now, ExpiresAt: now.Add(time.Duration(duration) * time.Second)}
	acceptance.RequestDigest, err = runtimeapproval.WorkflowExecutorSpecDigest(encoded)
	if err != nil {
		return run, acceptance, err
	}
	acceptance.BindingDigest, err = runtimeapproval.WorkflowRunExecutionScopeDigest(acceptance, storage.binding)
	if err != nil {
		return run, acceptance, err
	}
	storage.acceptance = acceptance
	return run, acceptance, nil
}

func TestWorkflowRunAcceptanceUsesActualSubmitter(t *testing.T) {
	ctx := context.Background()
	storage := &workflowBindingAPITestStore{APIServerStore: newMemStore()}
	workflow := api.Workflow{ObjectMeta: api.ObjectMeta{Name: "investigate", Namespace: "team-a", UID: "workflow-uid", ResourceVersion: 1},
		Spec: api.WorkflowSpec{Steps: []api.WorkflowStep{{Name: "team", AgentRef: api.LocalObjectReference{Name: "analyst"}}}}}
	raw, err := json.Marshal(workflow)
	if err != nil {
		t.Fatal(err)
	}
	if err := storage.Create(ctx, "Workflow", workflow.Namespace, workflow.Name, raw); err != nil {
		t.Fatal(err)
	}
	digest, err := runtimeapproval.WorkflowExecutorSpecDigest(raw)
	if err != nil {
		t.Fatal(err)
	}
	initiator := api.ControlPlaneRoleSubject{Method: "oidc", Issuer: "https://issuer.example", Subject: "submitter"}
	executor := api.ControlPlaneRoleSubject{Method: "oidc", Issuer: initiator.Issuer, Subject: "executor"}
	ref := func(kind, name, uid string) api.RuntimeApprovalReference {
		return api.RuntimeApprovalReference{Kind: kind, Scope: "Namespaced", Namespace: "team-a", Name: name, UID: uid, ResourceVersion: 1}
	}
	now := time.Now().UTC()
	storage.binding = runtimeapproval.WorkflowExecutorAuthority{PolicyVersion: runtimeapproval.WorkflowExecutorPolicyVersion, Reference: ref("WorkflowExecutionBinding", "approved", "binding-uid"), Workflow: ref("Workflow", workflow.Name, workflow.UID),
		WorkflowRevisionDigest: digest, Executor: executor, ApprovedBy: api.ControlPlaneRoleSubject{Method: "oidc", Issuer: initiator.Issuer, Subject: "operator"},
		Enabled: true, ControllerUIDs: []string{"controller-uid"}, NotBefore: now.Add(-time.Minute), ExpiresAt: now.Add(time.Hour),
		Limits:  runtimeapproval.WorkflowExecutorLimits{MaxEffects: 8, MaxPayloadBytes: 32768, MaxDurationSeconds: 3600, MaxMembers: 8, MaxTurns: 100, MaxMessages: 500, MaxDelegations: 100, MaxHandoffDepth: 5, MaxParallelTasks: 8, ContentRetentionSeconds: 604800, ReplayRetentionSeconds: 7776000},
		Targets: []runtimeapproval.WorkflowExecutorTarget{{Agent: ref("Agent", "analyst", "agent-uid"), Service: ref("AgentService", "analyst", "service-uid"), Operation: runtimeapproval.WorkflowExecutorInitiate, InitialDecision: api.RuntimeApprovalDecisionPending}}}
	role := api.ControlPlaneRole{ObjectMeta: api.ObjectMeta{Name: "use", Namespace: "control"}, Spec: api.ControlPlaneRoleSpec{ExecutionRules: []api.ControlPlaneExecutionRule{{Effect: "allow", Operation: api.ControlPlaneWorkflowUse,
		Workspace: workflow.Namespace, ResourceName: workflow.Name, ResourceUID: workflow.UID, RevisionDigest: digest}}}}
	binding := api.ControlPlaneRoleBinding{ObjectMeta: api.ObjectMeta{Name: "use", Namespace: "control"}, Spec: api.ControlPlaneRoleBindingSpec{RoleRef: api.LocalObjectReference{Name: "use"}, Subjects: []api.ControlPlaneRoleSubject{initiator}}}
	for kind, value := range map[string]any{"ControlPlaneRole": role, "ControlPlaneRoleBinding": binding} {
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if err := storage.Create(ctx, kind, "control", "use", raw); err != nil {
			t.Fatal(err)
		}
	}
	identities := &recordingAuthenticator{identities: map[string]*auth.Identity{
		"submitter": {Method: initiator.Method, Issuer: initiator.Issuer, Subject: initiator.Subject},
		"executor":  {Method: executor.Method, Issuer: executor.Issuer, Subject: executor.Subject},
		"author":    {Method: "oidc", Issuer: initiator.Issuer, Subject: "author"},
	}}
	server := New(Config{}, storage, zap.NewNop(), WithAuthenticator(identities, nil), WithWorkflowExecutorPrincipals([]api.ControlPlaneRoleSubject{executor}),
		WithWorkflowExecutorClock(func(context.Context) (time.Time, error) { return now, nil }), WithAuditLog(observability.NewCognitiveAuditLog(&workflowBindingAuditTestStore{})))
	t.Cleanup(func() { _ = server.Shutdown(ctx) })
	input := workflowRunAcceptanceRequest{Binding: storage.binding.Reference, Run: api.WorkflowRun{ObjectMeta: api.ObjectMeta{Name: "accepted-run", Namespace: "team-a"}, Spec: api.WorkflowRunSpec{WorkflowRef: api.LocalObjectReference{Name: workflow.Name}}}, DurationSeconds: 1800}
	request := func(token string) *httptest.ResponseRecorder {
		raw, err := json.Marshal(input)
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(http.MethodPost, "/internal/workflow-execution/namespaces/team-a/runs", bytes.NewReader(raw))
		req.Header.Set("Authorization", "Bearer "+token)
		response := httptest.NewRecorder()
		server.ServeHTTP(response, req)
		return response
	}
	for _, token := range []string{"author", "executor"} {
		if response := request(token); response.Code != http.StatusForbidden {
			t.Fatalf("%s accepted a run: %d %s", token, response.Code, response.Body.String())
		}
	}
	if response := request("submitter"); response.Code != http.StatusCreated {
		t.Fatalf("submitter acceptance: %d %s", response.Code, response.Body.String())
	}
	if storage.acceptance.Initiator != initiator || storage.acceptance.RunUID != "persisted-run-uid" || storage.acceptance.Executor != executor || storage.acceptance.BindingDigest == "" {
		t.Fatalf("acceptance used untrusted or missing scope: %+v", storage.acceptance)
	}
	if err := runtimeapproval.ValidateWorkflowRunAcceptanceAuthority(storage.acceptance, storage.binding, now); err != nil {
		t.Fatal(err)
	}
	storage.acceptance = runtimeapproval.WorkflowRunAcceptance{}
	for _, scenario := range []struct {
		failure error
		status  int
	}{
		{controlplaneauth.ErrForbidden, http.StatusForbidden},
		{controlplaneauth.ErrUnavailable, http.StatusServiceUnavailable},
	} {
		storage.acceptanceError = errors.Join(errors.New("final verification failed"), scenario.failure)
		if response := request("submitter"); response.Code != scenario.status {
			t.Fatalf("final acceptance error %v: %d %s", scenario.failure, response.Code, response.Body.String())
		}
		if storage.acceptance.RunUID != "" {
			t.Fatal("failed final authorization retained acceptance")
		}
	}
}

func TestWorkflowBindingAPIRequiresProtectedAdministrator(t *testing.T) {
	executor := api.ControlPlaneRoleSubject{Method: "oidc", Issuer: "https://issuer.example", Subject: "executor"}
	identities := &recordingAuthenticator{identities: map[string]*auth.Identity{
		"operator":  {Method: "oidc", Issuer: "https://issuer.example", Subject: "operator", Groups: []string{"binding-admins"}},
		"author":    {Method: "oidc", Issuer: "https://issuer.example", Subject: "author", Groups: []string{"authors"}},
		"executor":  {Method: executor.Method, Issuer: executor.Issuer, Subject: executor.Subject, Groups: []string{"binding-admins"}},
		"bootstrap": {Method: "token", Subject: "operator", Groups: []string{"binding-admins"}},
	}}
	storage := &workflowBindingAPITestStore{APIServerStore: newMemStore()}
	audit := &workflowBindingAuditTestStore{}
	server := New(Config{}, storage, zap.NewNop(), WithAuthenticator(identities, nil), WithControlPlaneAdminGroups([]string{"binding-admins"}),
		WithWorkflowExecutorPrincipals([]api.ControlPlaneRoleSubject{executor}), WithAuditLog(observability.NewCognitiveAuditLog(audit)))
	t.Cleanup(func() { _ = server.Shutdown(context.Background()) })
	reference := func(kind, name string) api.RuntimeApprovalReference {
		return api.RuntimeApprovalReference{Kind: kind, Scope: "Namespaced", Namespace: "team-a", Name: name, UID: name + "-uid", ResourceVersion: 1}
	}
	now := time.Now().UTC()
	binding := runtimeapproval.WorkflowExecutorAuthority{PolicyVersion: runtimeapproval.WorkflowExecutorPolicyVersion, Reference: api.RuntimeApprovalReference{Kind: "WorkflowExecutionBinding", Scope: "Namespaced", Namespace: "team-a", Name: "approved"},
		Workflow: reference("Workflow", "investigate"), WorkflowRevisionDigest: strings.Repeat("a", 64), Executor: executor, Enabled: true, ControllerUIDs: []string{"controller-uid"},
		NotBefore: now, ExpiresAt: now.Add(time.Hour), Limits: runtimeapproval.WorkflowExecutorLimits{MaxEffects: 8, MaxPayloadBytes: 32768, MaxDurationSeconds: 3600, MaxMembers: 8, MaxTurns: 100, MaxMessages: 500, MaxDelegations: 100, MaxHandoffDepth: 5, MaxParallelTasks: 8, ContentRetentionSeconds: 604800, ReplayRetentionSeconds: 7776000},
		Targets: []runtimeapproval.WorkflowExecutorTarget{{Agent: reference("Agent", "analyst"), Service: reference("AgentService", "analyst"), Operation: runtimeapproval.WorkflowExecutorInitiate, InitialDecision: api.RuntimeApprovalDecisionPending}}}
	workflow := api.Workflow{ObjectMeta: api.ObjectMeta{Name: binding.Workflow.Name, Namespace: binding.Workflow.Namespace, UID: binding.Workflow.UID, ResourceVersion: binding.Workflow.ResourceVersion},
		Spec: api.WorkflowSpec{Steps: []api.WorkflowStep{{Name: "team", AgentRef: api.LocalObjectReference{Name: "analyst"}}}}}
	workflowJSON, err := json.Marshal(workflow)
	if err != nil {
		t.Fatal(err)
	}
	if err := storage.Create(context.Background(), "Workflow", workflow.Namespace, workflow.Name, workflowJSON); err != nil {
		t.Fatal(err)
	}
	binding.WorkflowRevisionDigest, err = runtimeapproval.WorkflowExecutorSpecDigest(workflowJSON)
	if err != nil {
		t.Fatal(err)
	}
	path := "/internal/workflow-execution/namespaces/team-a/bindings"
	request := func(method, token, path string, value any) *httptest.ResponseRecorder {
		t.Helper()
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(method, path, bytes.NewReader(raw))
		req.Header.Set("Authorization", "Bearer "+token)
		response := httptest.NewRecorder()
		server.ServeHTTP(response, req)
		return response
	}
	for _, token := range []string{"author", "executor", "bootstrap"} {
		if response := request(http.MethodPost, token, path, binding); response.Code != http.StatusForbidden {
			t.Fatalf("%s created binding: %d %s", token, response.Code, response.Body.String())
		}
	}
	for _, scenario := range []struct {
		name   string
		status int
		change func(*runtimeapproval.WorkflowExecutorAuthority)
	}{
		{"missing Workflow", http.StatusNotFound, func(binding *runtimeapproval.WorkflowExecutorAuthority) { binding.Workflow.Name = "missing" }},
		{"recreated Workflow", http.StatusConflict, func(binding *runtimeapproval.WorkflowExecutorAuthority) { binding.Workflow.UID = "recreated-uid" }},
		{"changed Workflow revision", http.StatusConflict, func(binding *runtimeapproval.WorkflowExecutorAuthority) { binding.Workflow.ResourceVersion++ }},
		{"changed Workflow spec", http.StatusConflict, func(binding *runtimeapproval.WorkflowExecutorAuthority) {
			binding.WorkflowRevisionDigest = strings.Repeat("b", 64)
		}},
	} {
		invalid := binding
		scenario.change(&invalid)
		if response := request(http.MethodPost, "operator", path, invalid); response.Code != scenario.status {
			t.Fatalf("%s binding: %d %s", scenario.name, response.Code, response.Body.String())
		}
		if storage.binding.Reference.UID != "" || len(audit.entries) != 0 {
			t.Fatalf("%s reached binding persistence or audit", scenario.name)
		}
	}
	if storage.binding.Reference.UID != "" || len(audit.entries) != 0 {
		t.Fatal("unauthorized writes reached persistence or admin audit")
	}
	if response := request(http.MethodPost, "operator", path, binding); response.Code != http.StatusCreated {
		t.Fatalf("operator create: %d %s", response.Code, response.Body.String())
	}
	if storage.binding.ApprovedBy.Subject != "operator" || len(audit.entries) != 1 || audit.entries[0].AgentIdentity != "operator" {
		t.Fatal("binding lost actual operator attribution")
	}
	if response := request(http.MethodGet, "operator", path+"/approved", nil); response.Code != http.StatusOK {
		t.Fatalf("operator read: %d", response.Code)
	}
	retained := storage.binding
	if response := request(http.MethodDelete, "operator", path+"/approved", retained); response.Code != http.StatusOK {
		t.Fatalf("operator revoke: %d %s", response.Code, response.Body.String())
	}
	if storage.binding.Enabled || storage.binding.Reference.ResourceVersion != 2 {
		t.Fatal("revocation did not advance protected binding version")
	}
	if response := request(http.MethodPut, "operator", path+"/approved", retained); response.Code != http.StatusConflict {
		t.Fatalf("stale update: %d", response.Code)
	}
	if err := storage.Delete(context.Background(), "Workflow", workflow.Namespace, workflow.Name); err != nil {
		t.Fatal(err)
	}
	retained = storage.binding
	if response := request(http.MethodDelete, "operator", path+"/approved", retained); response.Code != http.StatusOK {
		t.Fatalf("revoke after Workflow deletion: %d %s", response.Code, response.Body.String())
	}
	if storage.binding.Enabled || storage.binding.Reference.ResourceVersion != 3 {
		t.Fatal("revocation after Workflow deletion did not retain and advance the protected binding")
	}
	server.auditLog = nil
	retained = storage.binding
	if response := request(http.MethodPut, "operator", path+"/approved", retained); response.Code != http.StatusServiceUnavailable {
		t.Fatalf("missing audit: %d", response.Code)
	}
}

type workflowBindingAuditTestStore struct{ entries []observability.AuditEntry }

func TestWorkflowExecutorInitiationHTTPRequiresQualifiedBoundary(t *testing.T) {
	server := New(Config{}, newMemStore(), zap.NewNop())
	t.Cleanup(func() { _ = server.Shutdown(context.Background()) })
	for _, route := range []string{"initiation-capabilities", "initiations/resolve", "initiations/dispatch"} {
		request := httptest.NewRequest(http.MethodPost, "/internal/workflow-execution/namespaces/team-a/"+route, strings.NewReader(`{}`))
		response := httptest.NewRecorder()
		server.ServeHTTP(response, request)
		if response.Code != http.StatusServiceUnavailable {
			t.Fatalf("unqualified %s route status = %d", route, response.Code)
		}
	}
	server.workflowExecutorInitiation = &runtimeapproval.WorkflowExecutorInitiationService{}
	input := workflowExecutorInitiationRequest{Action: runtimeapproval.WorkflowExecutorAction{Executor: api.ControlPlaneRoleSubject{Method: "oidc", Issuer: "https://issuer.example", Subject: "executor"}, Activation: store.WorkflowActivationKey{Workspace: "team-a"}}}
	raw, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	for name, identity := range map[string]*auth.Identity{
		"shared bearer": {Method: "token", Subject: "executor"},
		"initiator":     {Method: "oidc", Issuer: "https://issuer.example", Subject: "submitter"},
	} {
		t.Run(name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "/internal/workflow-execution/namespaces/team-a/initiation-capabilities", bytes.NewReader(raw)).WithContext(auth.WithIdentity(context.Background(), identity))
			response := httptest.NewRecorder()
			server.ServeHTTP(response, request)
			if response.Code != http.StatusForbidden {
				t.Fatalf("%s entered executor initiation: %d", name, response.Code)
			}
		})
	}
	request := httptest.NewRequest(http.MethodPost, "/internal/workflow-execution/namespaces/other/initiation-capabilities", bytes.NewReader(raw)).WithContext(auth.WithIdentity(context.Background(), &auth.Identity{Method: "oidc", Issuer: "https://issuer.example", Subject: "executor"}))
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("foreign route accepted: %d", response.Code)
	}
}
func (storage *workflowBindingAuditTestStore) Append(_ context.Context, entry observability.AuditEntry) error {
	storage.entries = append(storage.entries, entry)
	return nil
}
func (storage *workflowBindingAuditTestStore) AppendBatch(ctx context.Context, entries []observability.AuditEntry) error {
	for _, entry := range entries {
		if err := storage.Append(ctx, entry); err != nil {
			return err
		}
	}
	return nil
}
func (storage *workflowBindingAuditTestStore) Query(context.Context, observability.AuditFilter) ([]observability.AuditEntry, error) {
	return storage.entries, nil
}
