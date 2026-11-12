// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package apiserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"go.uber.org/zap"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/auth"
	"github.com/kognition-ai/kognition/internal/observability"
	"github.com/kognition-ai/kognition/internal/requestid"
	"github.com/kognition-ai/kognition/internal/runtimeapproval"
	"github.com/kognition-ai/kognition/internal/store"
)

type recordingRuntimeApprovalStore struct {
	store.RuntimeApprovalExecutionStore
	approval       v1alpha1.RuntimeApproval
	decision       store.RuntimeApprovalDecision
	getErr         error
	decisionErr    error
	reconcileErr   error
	reconciliation struct {
		disposition string
		evidenceRef string
		issuer      string
		subject     string
	}
}

func (s *recordingRuntimeApprovalStore) GetRuntimeApprovalByInvocation(context.Context, string, string) (v1alpha1.RuntimeApproval, error) {
	return s.approval, s.getErr
}

func (s *recordingRuntimeApprovalStore) DecideRuntimeApproval(_ context.Context, _, _ string, decision store.RuntimeApprovalDecision) (v1alpha1.RuntimeApproval, error) {
	if s.decisionErr != nil {
		return v1alpha1.RuntimeApproval{}, s.decisionErr
	}
	s.decision = decision
	s.approval.Status.DecisionState = decision.Decision
	return s.approval, nil
}

func (s *recordingRuntimeApprovalStore) ReconcileRuntimeApprovalExecution(_ context.Context, _, _ string, _ int64, disposition, evidenceRef, issuer, subject string) (v1alpha1.RuntimeApproval, error) {
	if s.reconcileErr != nil {
		return v1alpha1.RuntimeApproval{}, s.reconcileErr
	}
	s.reconciliation.disposition = disposition
	s.reconciliation.evidenceRef = evidenceRef
	s.reconciliation.issuer = issuer
	s.reconciliation.subject = subject
	return s.approval, nil
}

type recordingAuthenticator struct {
	identities map[string]*auth.Identity
	tokens     []string
	requestIDs []string
}

func (a *recordingAuthenticator) Authenticate(ctx context.Context, token string) (*auth.Identity, error) {
	a.tokens = append(a.tokens, token)
	a.requestIDs = append(a.requestIDs, requestid.FromContext(ctx))
	identity, ok := a.identities[token]
	if !ok {
		return nil, auth.ErrNoToken
	}
	return identity, nil
}

func TestServerServeHTTPEnforcesConfiguredAuthentication(t *testing.T) {
	authenticator := &recordingAuthenticator{identities: map[string]*auth.Identity{
		"viewer-token": {Subject: "viewer", Username: "viewer", Groups: []string{"viewers"}, Method: "token"},
		"admin-token":  {Subject: "admin", Username: "admin", Groups: []string{"admins"}, Method: "token"},
	}}
	server := New(
		Config{Addr: ":0"},
		newMemStore(),
		zap.NewNop(),
		WithAuthenticator(authenticator, []string{"admins"}),
		WithMetricsHandler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("test_metric 1\n"))
		})),
	)

	healthResponse := httptest.NewRecorder()
	server.ServeHTTP(healthResponse, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if healthResponse.Code != http.StatusOK {
		t.Fatalf("GET /healthz status = %d, want %d", healthResponse.Code, http.StatusOK)
	}
	if len(authenticator.tokens) != 0 {
		t.Fatalf("health request authenticated with tokens %q", authenticator.tokens)
	}

	metricsResponse := httptest.NewRecorder()
	server.metricsServer.Handler.ServeHTTP(metricsResponse, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if metricsResponse.Code != http.StatusOK {
		t.Fatalf("GET /metrics status = %d, want %d", metricsResponse.Code, http.StatusOK)
	}
	if metricsResponse.Body.String() != "test_metric 1\n" {
		t.Fatalf("GET /metrics body = %q, want test metric", metricsResponse.Body.String())
	}
	if len(authenticator.tokens) != 0 {
		t.Fatalf("metrics request authenticated with tokens %q", authenticator.tokens)
	}
	if server.metricsServer.Addr != "127.0.0.1:9090" {
		t.Fatalf("metrics must default to loopback, got %q", server.metricsServer.Addr)
	}
	publicMetrics := httptest.NewRecorder()
	publicRequest := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	publicRequest.Header.Set("Authorization", "Bearer admin-token")
	server.ServeHTTP(publicMetrics, publicRequest)
	if publicMetrics.Code != http.StatusNotFound {
		t.Fatalf("metrics exposed on API listener: %d", publicMetrics.Code)
	}

	resourcePath := "/apis/kognition.dev/v1alpha1/namespaces/default/agents"
	unauthenticatedResponse := httptest.NewRecorder()
	server.ServeHTTP(unauthenticatedResponse, httptest.NewRequest(http.MethodGet, resourcePath, nil))
	if unauthenticatedResponse.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status = %d, want %d", unauthenticatedResponse.Code, http.StatusUnauthorized)
	}
	if unauthenticatedResponse.Header().Get("WWW-Authenticate") != "Bearer" {
		t.Fatalf("WWW-Authenticate = %q, want Bearer", unauthenticatedResponse.Header().Get("WWW-Authenticate"))
	}

	viewerRequest := httptest.NewRequest(http.MethodGet, resourcePath, nil)
	viewerRequest.Header.Set("Authorization", "Bearer viewer-token")
	viewerResponse := httptest.NewRecorder()
	server.ServeHTTP(viewerResponse, viewerRequest)
	if viewerResponse.Code != http.StatusForbidden {
		t.Fatalf("viewer status = %d, want %d", viewerResponse.Code, http.StatusForbidden)
	}

	adminRequest := httptest.NewRequest(http.MethodGet, resourcePath, nil)
	adminRequest.Header.Set("Authorization", "Bearer admin-token")
	adminResponse := httptest.NewRecorder()
	server.ServeHTTP(adminResponse, adminRequest)
	if adminResponse.Code != http.StatusOK {
		t.Fatalf("admin status = %d, want %d; body = %s", adminResponse.Code, http.StatusOK, adminResponse.Body.String())
	}

	if got, want := authenticator.tokens, []string{"admin-token", "", "viewer-token", "admin-token"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("authenticated tokens = %q, want %q", got, want)
	}
	for index, id := range authenticator.requestIDs {
		if id == "" {
			t.Fatalf("authentication call %d did not receive a request ID", index)
		}
	}
}

func TestServerServeHTTPRejectsInvalidBearerToken(t *testing.T) {
	authenticator := &recordingAuthenticator{identities: map[string]*auth.Identity{}}
	server := New(Config{Addr: ":0"}, newMemStore(), zap.NewNop(), WithAuthenticator(authenticator, nil))
	request := httptest.NewRequest(http.MethodGet, "/apis/kognition.dev/v1alpha1/namespaces/default/agents", nil)
	request.Header.Set("Authorization", "Bearer invalid")
	response := httptest.NewRecorder()

	server.ServeHTTP(response, request)

	if response.Code != http.StatusUnauthorized {
		t.Fatalf("invalid token status = %d, want %d", response.Code, http.StatusUnauthorized)
	}
	if got, want := authenticator.tokens, []string{"invalid"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("authenticated tokens = %q, want %q", got, want)
	}
	if len(authenticator.requestIDs) != 1 || authenticator.requestIDs[0] == "" {
		t.Fatalf("authentication request IDs = %q, want one non-empty ID", authenticator.requestIDs)
	}
}

func TestControlPlanePolicyManagementRequiresSeparateOIDCAdminGroup(t *testing.T) {
	memory := newMemStore()
	authenticator := &recordingAuthenticator{identities: map[string]*auth.Identity{
		"runtime-operator": {Method: "oidc", Issuer: "https://issuer.example", Subject: "operator", Groups: []string{"runtime-approvers"}},
		"bootstrap-admin":  {Method: "token", Subject: "bootstrap", Groups: []string{"control-plane-admins"}},
		"policy-admin":     {Method: "oidc", Issuer: "https://issuer.example", Subject: "administrator", Groups: []string{"control-plane-admins"}},
	}}
	server := New(Config{}, memory, zap.NewNop(),
		WithAuthenticator(authenticator, nil),
		WithControlPlaneAdminGroups([]string{"control-plane-admins"}),
	)
	path := "/apis/kognition.dev/v1alpha1/namespaces/control/controlplaneroles"
	body := []byte(`{"metadata":{"name":"decision"},"spec":{"rules":[{"effect":"allow","resources":["RuntimeApproval"],"verbs":["approve"],"workspaces":["team-a"],"runtimeApprovalNames":["approval-a"]}]}}`)

	for _, test := range []struct {
		name   string
		token  string
		status int
	}{
		{name: "runtime grant group", token: "runtime-operator", status: http.StatusForbidden},
		{name: "bootstrap token", token: "bootstrap-admin", status: http.StatusForbidden},
		{name: "separate oidc administrator", token: "policy-admin", status: http.StatusCreated},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
			request.Header.Set("Authorization", "Bearer "+test.token)
			response := httptest.NewRecorder()
			server.ServeHTTP(response, request)
			if response.Code != test.status {
				t.Fatalf("status = %d, want %d: %s", response.Code, test.status, response.Body.String())
			}
		})
	}

	for _, token := range []string{"runtime-operator", "bootstrap-admin"} {
		request := httptest.NewRequest(http.MethodGet, path, nil)
		request.Header.Set("Authorization", "Bearer "+token)
		response := httptest.NewRecorder()
		server.ServeHTTP(response, request)
		if response.Code != http.StatusForbidden {
			t.Fatalf("policy read with %s status = %d, want %d", token, response.Code, http.StatusForbidden)
		}
	}

	if _, err := memory.Get(context.Background(), "ControlPlaneRole", "control", "decision"); err != nil {
		t.Fatalf("created policy: %v", err)
	}
}

func TestControlPlanePolicyManagementRejectsUnknownFields(t *testing.T) {
	memory := newMemStore()
	authenticator := &recordingAuthenticator{identities: map[string]*auth.Identity{
		"policy-admin": {Method: "oidc", Issuer: "https://issuer.example", Subject: "administrator", Groups: []string{"control-plane-admins"}},
	}}
	server := New(Config{}, memory, zap.NewNop(),
		WithAuthenticator(authenticator, nil),
		WithControlPlaneAdminGroups([]string{"control-plane-admins"}),
	)
	request := httptest.NewRequest(http.MethodPost, "/apis/kognition.dev/v1alpha1/namespaces/control/controlplaneroles", bytes.NewBufferString(
		`{"metadata":{"name":"unknown-field"},"spec":{"rules":[],"unexpected":true}}`,
	))
	request.Header.Set("Authorization", "Bearer policy-admin")
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)

	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d: %s", response.Code, http.StatusBadRequest, response.Body.String())
	}
	if _, err := memory.Get(context.Background(), "ControlPlaneRole", "control", "unknown-field"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("stored rejected policy error = %v, want %v", err, store.ErrNotFound)
	}
}

func TestControlPlanePolicyManagementEnforcesConflictsAndReferences(t *testing.T) {
	memory := newMemStore()
	authenticator := &recordingAuthenticator{identities: map[string]*auth.Identity{
		"policy-admin": {Method: "oidc", Issuer: "https://issuer.example", Subject: "administrator", Groups: []string{"control-plane-admins"}},
	}}
	server := New(Config{}, memory, zap.NewNop(),
		WithAuthenticator(authenticator, nil),
		WithControlPlaneAdminGroups([]string{"control-plane-admins"}),
	)
	rolesPath := "/apis/kognition.dev/v1alpha1/namespaces/control/controlplaneroles"
	bindingsPath := "/apis/kognition.dev/v1alpha1/namespaces/control/controlplanerolebindings"
	request := func(method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, bytes.NewBufferString(body))
		req.Header.Set("Authorization", "Bearer policy-admin")
		response := httptest.NewRecorder()
		server.ServeHTTP(response, req)
		return response
	}
	roleBody := `{"metadata":{"name":"decision"},"spec":{"rules":[{"effect":"allow","resources":["RuntimeApproval"],"verbs":["approve"],"workspaces":["team-a"],"runtimeApprovalNames":["approval-a"]}]}}`
	if response := request(http.MethodPost, rolesPath, roleBody); response.Code != http.StatusCreated {
		t.Fatalf("create role status = %d, want %d: %s", response.Code, http.StatusCreated, response.Body.String())
	}
	bindingBody := `{"metadata":{"name":"operators"},"spec":{"roleRef":{"name":"decision"},"subjects":[{"method":"oidc","issuer":"https://issuer.example","subject":"operator"}]}}`
	if response := request(http.MethodPost, bindingsPath, bindingBody); response.Code != http.StatusCreated {
		t.Fatalf("create binding status = %d, want %d: %s", response.Code, http.StatusCreated, response.Body.String())
	}
	if response := request(http.MethodGet, rolesPath+"/decision", ""); response.Code != http.StatusOK {
		t.Fatalf("get role status = %d, want %d: %s", response.Code, http.StatusOK, response.Body.String())
	}
	if response := request(http.MethodGet, bindingsPath, ""); response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"name":"operators"`) {
		t.Fatalf("list bindings response = %d %s, want operators binding", response.Code, response.Body.String())
	}
	otherNamespaceRole := strings.Replace(roleBody, `"name":"decision"`, `"name":"other-decision"`, 1)
	if response := request(http.MethodPost, strings.Replace(rolesPath, "/control/", "/other/", 1), otherNamespaceRole); response.Code != http.StatusCreated {
		t.Fatalf("create other namespace role status = %d, want %d: %s", response.Code, http.StatusCreated, response.Body.String())
	}
	if response := request(http.MethodGet, rolesPath, ""); response.Code != http.StatusOK || strings.Contains(response.Body.String(), `"name":"other-decision"`) {
		t.Fatalf("control namespace role list leaked another namespace: %d %s", response.Code, response.Body.String())
	}
	deniedRequest := httptest.NewRequest(http.MethodGet, rolesPath, nil)
	deniedResponse := httptest.NewRecorder()
	server.ServeHTTP(deniedResponse, deniedRequest)
	if deniedResponse.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated policy list status = %d, want %d", deniedResponse.Code, http.StatusUnauthorized)
	}

	staleRoleBody := `{"metadata":{"name":"decision","resourceVersion":99},"spec":{"rules":[{"effect":"allow","resources":["RuntimeApproval"],"verbs":["approve"],"workspaces":["team-a"],"runtimeApprovalNames":["approval-a"]}]}}`
	if response := request(http.MethodPut, rolesPath+"/decision", staleRoleBody); response.Code != http.StatusConflict {
		t.Fatalf("stale update status = %d, want %d: %s", response.Code, http.StatusConflict, response.Body.String())
	}
	if response := request(http.MethodDelete, rolesPath+"/decision", ""); response.Code != http.StatusConflict {
		t.Fatalf("missing-version role delete status = %d, want %d: %s", response.Code, http.StatusConflict, response.Body.String())
	}
	if response := request(http.MethodDelete, rolesPath+"/decision?resourceVersion=99", ""); response.Code != http.StatusConflict {
		t.Fatalf("stale role delete status = %d, want %d: %s", response.Code, http.StatusConflict, response.Body.String())
	}
	if response := request(http.MethodDelete, rolesPath+"/decision?resourceVersion=1", ""); response.Code != http.StatusConflict {
		t.Fatalf("referenced role delete status = %d, want %d: %s", response.Code, http.StatusConflict, response.Body.String())
	}
	if response := request(http.MethodDelete, bindingsPath+"/operators?resourceVersion=99", ""); response.Code != http.StatusConflict {
		t.Fatalf("stale binding delete status = %d, want %d: %s", response.Code, http.StatusConflict, response.Body.String())
	}
	if response := request(http.MethodDelete, bindingsPath+"/operators?resourceVersion=1", ""); response.Code != http.StatusNoContent {
		t.Fatalf("binding delete status = %d, want %d: %s", response.Code, http.StatusNoContent, response.Body.String())
	}
	if response := request(http.MethodDelete, rolesPath+"/decision?resourceVersion=1", ""); response.Code != http.StatusNoContent {
		t.Fatalf("unreferenced role delete status = %d, want %d: %s", response.Code, http.StatusNoContent, response.Body.String())
	}
}

func TestControlPlanePolicyCreateReturnsPersistedIdentity(t *testing.T) {
	memory := &backendIdentityStore{memStore: newMemStore()}
	authenticator := &recordingAuthenticator{identities: map[string]*auth.Identity{
		"policy-admin": {Method: "oidc", Issuer: "https://issuer.example", Subject: "administrator", Groups: []string{"control-plane-admins"}},
	}}
	server := New(Config{}, memory, zap.NewNop(),
		WithAuthenticator(authenticator, nil),
		WithControlPlaneAdminGroups([]string{"control-plane-admins"}),
	)
	request := func(method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, bytes.NewBufferString(body))
		req.Header.Set("Authorization", "Bearer policy-admin")
		response := httptest.NewRecorder()
		server.ServeHTTP(response, req)
		return response
	}

	tests := []struct {
		name string
		path string
		body string
	}{
		{
			name: "role",
			path: "/apis/kognition.dev/v1alpha1/namespaces/control/controlplaneroles",
			body: `{"metadata":{"name":"decision"},"spec":{"rules":[{"effect":"allow","resources":["RuntimeApproval"],"verbs":["approve"],"workspaces":["team-a"],"runtimeApprovalNames":["approval-a"]}]}}`,
		},
		{
			name: "binding",
			path: "/apis/kognition.dev/v1alpha1/namespaces/control/controlplanerolebindings",
			body: `{"metadata":{"name":"operators"},"spec":{"roleRef":{"name":"decision"},"subjects":[{"method":"oidc","issuer":"https://issuer.example","subject":"operator"}]}}`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			created := request(http.MethodPost, test.path, test.body)
			if created.Code != http.StatusCreated {
				t.Fatalf("create status = %d, want %d: %s", created.Code, http.StatusCreated, created.Body.String())
			}
			var createdObject struct {
				Metadata v1alpha1.ObjectMeta `json:"metadata"`
			}
			if err := json.Unmarshal(created.Body.Bytes(), &createdObject); err != nil {
				t.Fatal(err)
			}
			persisted := request(http.MethodGet, test.path+"/"+createdObject.Metadata.Name, "")
			if persisted.Code != http.StatusOK {
				t.Fatalf("get status = %d, want %d: %s", persisted.Code, http.StatusOK, persisted.Body.String())
			}
			var persistedObject struct {
				Metadata v1alpha1.ObjectMeta `json:"metadata"`
			}
			if err := json.Unmarshal(persisted.Body.Bytes(), &persistedObject); err != nil {
				t.Fatal(err)
			}
			if createdObject.Metadata.UID != persistedObject.Metadata.UID {
				t.Fatalf("POST UID = %q, GET UID = %q", createdObject.Metadata.UID, persistedObject.Metadata.UID)
			}
		})
	}
}

func TestControlPlanePolicyMutationsWriteDurableAuditEntries(t *testing.T) {
	memory := newMemStore()
	auditStore := &memoryAuditStore{}
	authenticator := &recordingAuthenticator{identities: map[string]*auth.Identity{
		"policy-admin": {Method: "oidc", Issuer: "https://issuer.example", Subject: "administrator", Groups: []string{"control-plane-admins"}},
	}}
	server := New(Config{}, memory, zap.NewNop(),
		WithAuthenticator(authenticator, nil),
		WithControlPlaneAdminGroups([]string{"control-plane-admins"}),
		WithAuditLog(observability.NewCognitiveAuditLog(auditStore)),
	)
	path := "/apis/kognition.dev/v1alpha1/namespaces/control/controlplaneroles"
	request := func(method, requestPath string, body []byte) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, requestPath, bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer policy-admin")
		response := httptest.NewRecorder()
		server.ServeHTTP(response, req)
		return response
	}

	created := request(http.MethodPost, path, []byte(`{"metadata":{"name":"audited-role"},"spec":{"rules":[{"effect":"allow","resources":["RuntimeApproval"],"verbs":["approve"],"workspaces":["team-a"],"runtimeApprovalNames":["approval-a"]}]}}`))
	if created.Code != http.StatusCreated {
		t.Fatalf("create status = %d, want %d: %s", created.Code, http.StatusCreated, created.Body.String())
	}
	updated := request(http.MethodPut, path+"/audited-role", created.Body.Bytes())
	if updated.Code != http.StatusOK {
		t.Fatalf("update status = %d, want %d: %s", updated.Code, http.StatusOK, updated.Body.String())
	}
	var updatedRole v1alpha1.ControlPlaneRole
	if err := json.Unmarshal(updated.Body.Bytes(), &updatedRole); err != nil {
		t.Fatal(err)
	}
	deleted := request(http.MethodDelete, path+"/audited-role?resourceVersion="+strconv.FormatInt(updatedRole.ResourceVersion, 10), nil)
	if deleted.Code != http.StatusNoContent {
		t.Fatalf("delete status = %d, want %d: %s", deleted.Code, http.StatusNoContent, deleted.Body.String())
	}

	entries, err := auditStore.Query(context.Background(), observability.AuditFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 {
		t.Fatalf("audit entries = %#v, want create/update/delete", entries)
	}
	for index, verb := range []string{"create", "update", "delete"} {
		entry := entries[index]
		if entry.EventType != observability.AuditSpecChange || entry.Verb != verb || entry.SubjectKind != "ControlPlaneRole" || entry.SubjectRef != "control/audited-role" {
			t.Fatalf("audit entry %d = %#v, want ControlPlaneRole %s", index, entry, verb)
		}
		if entry.AgentIdentity != "administrator" {
			t.Fatalf("audit actor = %q, want verified subject", entry.AgentIdentity)
		}
	}
}

func TestControlPlanePolicyMutationsFailBeforePersistenceWhenAuditWriteFails(t *testing.T) {
	memory := newMemStore()
	existing := []byte(`{"apiVersion":"kognition.dev/v1alpha1","kind":"ControlPlaneRole","metadata":{"name":"existing-role","namespace":"control","uid":"existing-uid","resourceVersion":1},"spec":{"rules":[{"effect":"allow","resources":["RuntimeApproval"],"verbs":["approve"],"workspaces":["team-a"],"runtimeApprovalNames":["approval-a"]}]}}`)
	if err := memory.Create(context.Background(), "ControlPlaneRole", "control", "existing-role", existing); err != nil {
		t.Fatal(err)
	}
	authenticator := &recordingAuthenticator{identities: map[string]*auth.Identity{
		"policy-admin": {Method: "oidc", Issuer: "https://issuer.example", Subject: "administrator", Groups: []string{"control-plane-admins"}},
	}}
	server := New(Config{}, memory, zap.NewNop(),
		WithAuthenticator(authenticator, nil),
		WithControlPlaneAdminGroups([]string{"control-plane-admins"}),
		WithAuditLog(observability.NewCognitiveAuditLog(&memoryAuditStore{err: errors.New("audit unavailable")})),
	)
	path := "/apis/kognition.dev/v1alpha1/namespaces/control/controlplaneroles"
	request := func(method, requestPath string, body []byte) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, requestPath, bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer policy-admin")
		response := httptest.NewRecorder()
		server.ServeHTTP(response, req)
		return response
	}

	created := request(http.MethodPost, path, []byte(`{"metadata":{"name":"blocked-role"},"spec":{"rules":[{"effect":"allow","resources":["RuntimeApproval"],"verbs":["approve"],"workspaces":["team-a"],"runtimeApprovalNames":["approval-a"]}]}}`))
	if created.Code != http.StatusInternalServerError {
		t.Fatalf("create status = %d, want %d: %s", created.Code, http.StatusInternalServerError, created.Body.String())
	}
	if _, err := memory.Get(context.Background(), "ControlPlaneRole", "control", "blocked-role"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("role created after audit failure: %v", err)
	}

	updated := request(http.MethodPut, path+"/existing-role", existing)
	if updated.Code != http.StatusInternalServerError {
		t.Fatalf("update status = %d, want %d: %s", updated.Code, http.StatusInternalServerError, updated.Body.String())
	}
	afterUpdate, err := memory.Get(context.Background(), "ControlPlaneRole", "control", "existing-role")
	if err != nil || !bytes.Equal(afterUpdate, existing) {
		t.Fatalf("role changed after update audit failure: data=%s err=%v", afterUpdate, err)
	}

	deleted := request(http.MethodDelete, path+"/existing-role?resourceVersion=1", nil)
	if deleted.Code != http.StatusInternalServerError {
		t.Fatalf("delete status = %d, want %d: %s", deleted.Code, http.StatusInternalServerError, deleted.Body.String())
	}
	if _, err := memory.Get(context.Background(), "ControlPlaneRole", "control", "existing-role"); err != nil {
		t.Fatalf("role deleted after audit failure: %v", err)
	}
}

func TestControlPlanePolicyManagementRejectsWhitespaceValues(t *testing.T) {
	memory := newMemStore()
	authenticator := &recordingAuthenticator{identities: map[string]*auth.Identity{
		"policy-admin": {Method: "oidc", Issuer: "https://issuer.example", Subject: "administrator", Groups: []string{"control-plane-admins"}},
	}}
	server := New(Config{}, memory, zap.NewNop(),
		WithAuthenticator(authenticator, nil),
		WithControlPlaneAdminGroups([]string{"control-plane-admins"}),
	)
	rolesPath := "/apis/kognition.dev/v1alpha1/namespaces/control/controlplaneroles"
	bindingsPath := "/apis/kognition.dev/v1alpha1/namespaces/control/controlplanerolebindings"
	request := func(path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, path, bytes.NewBufferString(body))
		req.Header.Set("Authorization", "Bearer policy-admin")
		response := httptest.NewRecorder()
		server.ServeHTTP(response, req)
		return response
	}
	seed := request(rolesPath, `{"metadata":{"name":"decision"},"spec":{"rules":[{"effect":"allow","resources":["RuntimeApproval"],"verbs":["approve"],"workspaces":["team-a"],"runtimeApprovalNames":["approval-a"]}]}}`)
	if seed.Code != http.StatusCreated {
		t.Fatalf("seed role status = %d: %s", seed.Code, seed.Body.String())
	}

	tests := []struct {
		name        string
		path        string
		body        string
		wantMessage string
	}{
		{
			name:        "workspace",
			path:        rolesPath,
			body:        `{"metadata":{"name":"blank-workspace"},"spec":{"rules":[{"effect":"allow","resources":["RuntimeApproval"],"verbs":["approve"],"workspaces":[" "],"runtimeApprovalNames":["approval-a"]}]}}`,
			wantMessage: "explicit and supported",
		},
		{
			name:        "exact approval name",
			path:        rolesPath,
			body:        `{"metadata":{"name":"blank-name"},"spec":{"rules":[{"effect":"allow","resources":["RuntimeApproval"],"verbs":["approve"],"workspaces":["team-a"],"runtimeApprovalNames":[" "]}]}}`,
			wantMessage: "bound RuntimeApproval names",
		},
		{
			name:        "selector prefix alongside exact name",
			path:        rolesPath,
			body:        `{"metadata":{"name":"blank-prefix"},"spec":{"rules":[{"effect":"allow","resources":["RuntimeApproval"],"verbs":["approve"],"workspaces":["team-a"],"runtimeApprovalNames":["approval-a"],"runtimeApprovalNameSelector":{"prefix":" "}}]}}`,
			wantMessage: "bound RuntimeApproval names",
		},
		{
			name:        "role reference",
			path:        bindingsPath,
			body:        `{"metadata":{"name":"blank-role-ref"},"spec":{"roleRef":{"name":" "},"subjects":[{"method":"oidc","issuer":"https://issuer.example","subject":"operator"}]}}`,
			wantMessage: "roleRef and at least one subject",
		},
		{
			name:        "subject method",
			path:        bindingsPath,
			body:        `{"metadata":{"name":"blank-method"},"spec":{"roleRef":{"name":"decision"},"subjects":[{"method":" ","issuer":"https://issuer.example","subject":"operator"}]}}`,
			wantMessage: "canonical non-bootstrap identities",
		},
		{
			name:        "subject issuer",
			path:        bindingsPath,
			body:        `{"metadata":{"name":"blank-issuer"},"spec":{"roleRef":{"name":"decision"},"subjects":[{"method":"oidc","issuer":" ","subject":"operator"}]}}`,
			wantMessage: "canonical non-bootstrap identities",
		},
		{
			name:        "subject identifier",
			path:        bindingsPath,
			body:        `{"metadata":{"name":"blank-subject"},"spec":{"roleRef":{"name":"decision"},"subjects":[{"method":"oidc","issuer":"https://issuer.example","subject":" "}]}}`,
			wantMessage: "canonical non-bootstrap identities",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := request(test.path, test.body)
			if response.Code != http.StatusUnprocessableEntity || !strings.Contains(response.Body.String(), test.wantMessage) {
				t.Fatalf("status/body = %d %q, want 422 containing %q", response.Code, response.Body.String(), test.wantMessage)
			}
		})
	}
}

type backendIdentityStore struct {
	*memStore
}

func (s *backendIdentityStore) CreateAndGet(ctx context.Context, kind, namespace, name string, data []byte) ([]byte, error) {
	stored := replaceResourceUID(data, "backend-"+kind+"-uid")
	if err := s.memStore.Create(ctx, kind, namespace, name, stored); err != nil {
		return nil, err
	}
	return stored, nil
}

func (s *backendIdentityStore) Create(ctx context.Context, kind, namespace, name string, data []byte) error {
	_, err := s.CreateAndGet(ctx, kind, namespace, name, data)
	return err
}

func (s *backendIdentityStore) CreateControlPlaneRoleBinding(ctx context.Context, namespace, name, roleName string, data []byte) ([]byte, error) {
	return s.memStore.CreateControlPlaneRoleBinding(ctx, namespace, name, roleName, replaceResourceUID(data, "backend-ControlPlaneRoleBinding-uid"))
}

func replaceResourceUID(data []byte, uid string) []byte {
	var object map[string]any
	if err := json.Unmarshal(data, &object); err != nil {
		return data
	}
	metadata, ok := object["metadata"].(map[string]any)
	if !ok {
		return data
	}
	metadata["uid"] = uid
	replaced, err := json.Marshal(object)
	if err != nil {
		return data
	}
	return replaced
}

func TestRuntimeApprovalDecisionAuthorizesCanonicalIdentityAndPersistsActor(t *testing.T) {
	memory := newMemStore()
	invocationID := "invoke-42"
	name := runtimeapproval.CanonicalName("team-a", invocationID)
	role := v1alpha1.ControlPlaneRole{
		ObjectMeta: v1alpha1.ObjectMeta{Name: "runtime-approver", Namespace: "team-a"},
		Spec: v1alpha1.ControlPlaneRoleSpec{Rules: []v1alpha1.ControlPlanePolicyRule{{
			Effect: "allow", Resources: []string{"RuntimeApproval"}, Verbs: []string{"approve"}, Workspaces: []string{"team-a"}, RuntimeApprovalNames: []string{name},
		}}},
	}
	binding := v1alpha1.ControlPlaneRoleBinding{
		ObjectMeta: v1alpha1.ObjectMeta{Name: "runtime-approver", Namespace: "team-a"},
		Spec: v1alpha1.ControlPlaneRoleBindingSpec{
			RoleRef:  v1alpha1.LocalObjectReference{Name: role.Name},
			Subjects: []v1alpha1.ControlPlaneRoleSubject{{Method: "oidc", Issuer: "https://issuer.example", Subject: "operator-1"}},
		},
	}
	for kind, resource := range map[string]any{"ControlPlaneRole": role, "ControlPlaneRoleBinding": binding} {
		data, err := json.Marshal(resource)
		if err != nil {
			t.Fatal(err)
		}
		if err := memory.Create(context.Background(), kind, "team-a", "runtime-approver", data); err != nil {
			t.Fatal(err)
		}
	}

	operations := &recordingRuntimeApprovalStore{approval: v1alpha1.RuntimeApproval{
		ObjectMeta: v1alpha1.ObjectMeta{Name: name, Namespace: "team-a", ResourceVersion: 7},
		Spec:       v1alpha1.RuntimeApprovalSpec{Workspace: "team-a", InvocationID: invocationID, AgentRef: v1alpha1.RuntimeApprovalReference{Name: "requesting-agent", UID: "agent-uid"}},
	}}
	authenticator := &recordingAuthenticator{identities: map[string]*auth.Identity{
		"operator-token": {Method: "oidc", Issuer: "https://issuer.example", Subject: "operator-1", Username: "display-name"},
		"other-token":    {Method: "oidc", Issuer: "https://issuer.example", Subject: "operator-2", Username: "operator-1"},
	}}
	server := New(Config{}, memory, zap.NewNop(), WithAuthenticator(authenticator, nil))
	server.runtimeApprovals = operations
	path := "/apis/kognition.dev/v1alpha1/namespaces/team-a/runtimeapprovals/" + name + "/decision"

	deniedRequest := httptest.NewRequest(http.MethodPost, path, bytes.NewBufferString(`{"invocationID":"invoke-42","decision":"approve","expectedResourceVersion":7}`))
	deniedRequest.Header.Set("Authorization", "Bearer other-token")
	denied := httptest.NewRecorder()
	server.ServeHTTP(denied, deniedRequest)
	if denied.Code != http.StatusForbidden {
		t.Fatalf("unauthorized identity status = %d, want 403: %s", denied.Code, denied.Body.String())
	}

	allowedRequest := httptest.NewRequest(http.MethodPost, path, bytes.NewBufferString(`{"invocationID":"invoke-42","decision":"approve","expectedResourceVersion":7}`))
	allowedRequest.Header.Set("Authorization", "Bearer operator-token")
	allowed := httptest.NewRecorder()
	server.ServeHTTP(allowed, allowedRequest)
	if allowed.Code != http.StatusOK {
		t.Fatalf("authorized identity status = %d, want 200: %s", allowed.Code, allowed.Body.String())
	}
	if operations.decision.Issuer != "https://issuer.example" || operations.decision.Subject != "operator-1" || operations.decision.Decision != v1alpha1.RuntimeApprovalDecisionApproved {
		t.Fatalf("persisted decision = %+v, want canonical authenticated actor", operations.decision)
	}
}

func TestRuntimeApprovalEndpointsRejectInvalidRequestsBeforeTransition(t *testing.T) {
	invocationID := "invoke-validation"
	name := runtimeapproval.CanonicalName("team-a", invocationID)
	for _, test := range []struct {
		name string
		path string
		body string
	}{
		{name: "malformed decision", path: "/decision", body: `{`},
		{name: "unknown decision field", path: "/decision", body: `{"invocationID":"invoke-validation","decision":"approve","expectedResourceVersion":7,"actor":"forged"}`},
		{name: "wrong canonical decision target", path: "/decision", body: `{"invocationID":"other","decision":"approve","expectedResourceVersion":7}`},
		{name: "invalid decision", path: "/decision", body: `{"invocationID":"invoke-validation","decision":"allow","expectedResourceVersion":7}`},
		{name: "missing decision resource version", path: "/decision", body: `{"invocationID":"invoke-validation","decision":"approve"}`},
		{name: "malformed reconciliation", path: "/reconcile", body: `{`},
		{name: "wrong canonical reconciliation target", path: "/reconcile", body: `{"invocationID":"other","disposition":"ReconciledCompleted","evidenceRef":"audit://receipt/42","expectedResourceVersion":7}`},
		{name: "invalid reconciliation disposition", path: "/reconcile", body: `{"invocationID":"invoke-validation","disposition":"Completed","evidenceRef":"audit://receipt/42","expectedResourceVersion":7}`},
		{name: "missing reconciliation evidence", path: "/reconcile", body: `{"invocationID":"invoke-validation","disposition":"ReconciledCompleted","expectedResourceVersion":7}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			operations := &recordingRuntimeApprovalStore{}
			server := New(Config{}, newMemStore(), zap.NewNop())
			server.runtimeApprovals = operations
			request := httptest.NewRequest(http.MethodPost, "/apis/kognition.dev/v1alpha1/namespaces/team-a/runtimeapprovals/"+name+test.path, bytes.NewBufferString(test.body))
			response := httptest.NewRecorder()
			server.ServeHTTP(response, request)
			if response.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want %d: %s", response.Code, http.StatusBadRequest, response.Body.String())
			}
			if operations.decision.Decision != "" || operations.reconciliation.disposition != "" {
				t.Fatalf("invalid request recorded transition: decision=%+v reconciliation=%+v", operations.decision, operations.reconciliation)
			}
		})
	}
}

func TestRuntimeApprovalDecisionMapsUnavailableAndStoreFailures(t *testing.T) {
	invocationID := "invoke-failure"
	name := runtimeapproval.CanonicalName("team-a", invocationID)
	approval := v1alpha1.RuntimeApproval{
		ObjectMeta: v1alpha1.ObjectMeta{Name: name, Namespace: "team-a", ResourceVersion: 7},
		Spec:       v1alpha1.RuntimeApprovalSpec{Workspace: "team-a", InvocationID: invocationID},
	}
	for _, test := range []struct {
		name        string
		getErr      error
		decisionErr error
		status      int
	}{
		{name: "missing approval", getErr: store.ErrNotFound, status: http.StatusNotFound},
		{name: "lookup failure", getErr: errors.New("lookup failed"), status: http.StatusInternalServerError},
		{name: "decision conflict", decisionErr: store.ErrConflict, status: http.StatusConflict},
		{name: "decision storage failure", decisionErr: errors.New("write failed"), status: http.StatusInternalServerError},
	} {
		t.Run(test.name, func(t *testing.T) {
			operations := &recordingRuntimeApprovalStore{approval: approval, getErr: test.getErr, decisionErr: test.decisionErr}
			server := newAuthorizedRuntimeApprovalTestServer(t, operations, "approve")
			request := httptest.NewRequest(http.MethodPost, "/apis/kognition.dev/v1alpha1/namespaces/team-a/runtimeapprovals/"+name+"/decision", bytes.NewBufferString(
				`{"invocationID":"invoke-failure","decision":"approve","expectedResourceVersion":7}`,
			))
			request.Header.Set("Authorization", "Bearer operator-token")
			response := httptest.NewRecorder()
			server.ServeHTTP(response, request)
			if response.Code != test.status {
				t.Fatalf("status = %d, want %d: %s", response.Code, test.status, response.Body.String())
			}
			if operations.decision.Decision != "" {
				t.Fatalf("failed request recorded decision %+v", operations.decision)
			}
		})
	}

	operations := &recordingRuntimeApprovalStore{approval: approval}
	server := newAuthorizedRuntimeApprovalTestServer(t, operations, "approve")
	server.approvalAuthorizer = nil
	request := httptest.NewRequest(http.MethodPost, "/apis/kognition.dev/v1alpha1/namespaces/team-a/runtimeapprovals/"+name+"/decision", bytes.NewBufferString(
		`{"invocationID":"invoke-failure","decision":"approve","expectedResourceVersion":7}`,
	))
	request.Header.Set("Authorization", "Bearer operator-token")
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("unavailable operations status = %d, want %d: %s", response.Code, http.StatusServiceUnavailable, response.Body.String())
	}
}

func TestRuntimeApprovalDecisionRejectsRequestingWorkload(t *testing.T) {
	memory := newMemStore()
	invocationID := "invoke-self"
	name := runtimeapproval.CanonicalName("team-a", invocationID)
	spiffeID := "spiffe://kognition.dev/workspace/team-a/agent/requesting-agent"
	resources := map[string]any{
		"ControlPlaneRole": v1alpha1.ControlPlaneRole{
			ObjectMeta: v1alpha1.ObjectMeta{Name: "self-grant", Namespace: "team-a"},
			Spec: v1alpha1.ControlPlaneRoleSpec{Rules: []v1alpha1.ControlPlanePolicyRule{{
				Effect: "allow", Resources: []string{"RuntimeApproval"}, Verbs: []string{"approve"}, Workspaces: []string{"team-a"}, RuntimeApprovalNames: []string{name},
			}}},
		},
		"ControlPlaneRoleBinding": v1alpha1.ControlPlaneRoleBinding{
			ObjectMeta: v1alpha1.ObjectMeta{Name: "self-grant", Namespace: "team-a"},
			Spec: v1alpha1.ControlPlaneRoleBindingSpec{
				RoleRef:  v1alpha1.LocalObjectReference{Name: "self-grant"},
				Subjects: []v1alpha1.ControlPlaneRoleSubject{{Method: "oidc", Issuer: "https://issuer.example", Subject: spiffeID}},
			},
		},
		"AgentIdentity": v1alpha1.AgentIdentity{
			ObjectMeta: v1alpha1.ObjectMeta{Name: "requesting-identity", Namespace: "team-a"},
			Spec:       v1alpha1.AgentIdentitySpec{SPIFFEID: spiffeID},
		},
	}
	for kind, resource := range resources {
		data, err := json.Marshal(resource)
		if err != nil {
			t.Fatal(err)
		}
		resourceName := "self-grant"
		if kind == "AgentIdentity" {
			resourceName = "requesting-identity"
		}
		if err := memory.Create(context.Background(), kind, "team-a", resourceName, data); err != nil {
			t.Fatal(err)
		}
	}

	operations := &recordingRuntimeApprovalStore{approval: v1alpha1.RuntimeApproval{
		ObjectMeta: v1alpha1.ObjectMeta{Name: name, Namespace: "team-a", ResourceVersion: 7},
		Spec: v1alpha1.RuntimeApprovalSpec{
			Workspace: "team-a", InvocationID: invocationID,
			AgentRef:         v1alpha1.RuntimeApprovalReference{Name: "requesting-agent", UID: "agent-uid"},
			AgentIdentityRef: v1alpha1.RuntimeApprovalReference{Name: "requesting-identity", UID: "identity-uid"},
			AgentInstanceRef: v1alpha1.RuntimeApprovalReference{Name: "requesting-instance", UID: "instance-uid"},
		},
	}}
	authenticator := &recordingAuthenticator{identities: map[string]*auth.Identity{
		"self-token": {Method: "oidc", Issuer: "https://issuer.example", Subject: spiffeID},
	}}
	server := New(Config{}, memory, zap.NewNop(), WithAuthenticator(authenticator, nil))
	server.runtimeApprovals = operations
	request := httptest.NewRequest(http.MethodPost, "/apis/kognition.dev/v1alpha1/namespaces/team-a/runtimeapprovals/"+name+"/decision", bytes.NewBufferString(
		`{"invocationID":"invoke-self","decision":"approve","expectedResourceVersion":7}`,
	))
	request.Header.Set("Authorization", "Bearer self-token")
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)

	if response.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d: %s", response.Code, http.StatusForbidden, response.Body.String())
	}
	if operations.decision.Decision != "" {
		t.Fatalf("persisted self-decision = %+v, want none", operations.decision)
	}
}

func TestRuntimeApprovalRequestingWorkloadMatchesReferenceNamesAndUIDs(t *testing.T) {
	approval := v1alpha1.RuntimeApproval{Spec: v1alpha1.RuntimeApprovalSpec{
		AgentRef:         v1alpha1.RuntimeApprovalReference{Name: "agent", UID: "agent-uid"},
		AgentIdentityRef: v1alpha1.RuntimeApprovalReference{Name: "identity", UID: "identity-uid"},
		AgentInstanceRef: v1alpha1.RuntimeApprovalReference{Name: "instance", UID: "instance-uid"},
	}}
	server := &Server{}
	for _, subject := range []string{"agent", "agent-uid", "identity", "identity-uid", "instance", "instance-uid"} {
		requesting, err := server.isRequestingWorkload(context.Background(), subject, approval)
		if err != nil || !requesting {
			t.Errorf("subject %q: requesting = %t, err = %v; want true, nil", subject, requesting, err)
		}
	}
}

func TestRuntimeApprovalReconciliationRequiresSeparateVerbAndPersistsActor(t *testing.T) {
	memory := newMemStore()
	invocationID := "invoke-reconcile"
	name := runtimeapproval.CanonicalName("team-a", invocationID)
	for _, fixture := range []struct {
		name    string
		verb    string
		subject string
	}{
		{name: "approver", verb: "approve", subject: "approve-only"},
		{name: "reconciler", verb: "reconcile", subject: "reconcile-operator"},
	} {
		role := v1alpha1.ControlPlaneRole{
			ObjectMeta: v1alpha1.ObjectMeta{Name: fixture.name, Namespace: "team-a"},
			Spec: v1alpha1.ControlPlaneRoleSpec{Rules: []v1alpha1.ControlPlanePolicyRule{{
				Effect: "allow", Resources: []string{"RuntimeApproval"}, Verbs: []string{fixture.verb}, Workspaces: []string{"team-a"}, RuntimeApprovalNames: []string{name},
			}}},
		}
		binding := v1alpha1.ControlPlaneRoleBinding{
			ObjectMeta: v1alpha1.ObjectMeta{Name: fixture.name, Namespace: "team-a"},
			Spec: v1alpha1.ControlPlaneRoleBindingSpec{
				RoleRef:  v1alpha1.LocalObjectReference{Name: fixture.name},
				Subjects: []v1alpha1.ControlPlaneRoleSubject{{Method: "oidc", Issuer: "https://issuer.example", Subject: fixture.subject}},
			},
		}
		for kind, resource := range map[string]any{"ControlPlaneRole": role, "ControlPlaneRoleBinding": binding} {
			data, err := json.Marshal(resource)
			if err != nil {
				t.Fatal(err)
			}
			if err := memory.Create(context.Background(), kind, "team-a", fixture.name, data); err != nil {
				t.Fatal(err)
			}
		}
	}

	operations := &recordingRuntimeApprovalStore{approval: v1alpha1.RuntimeApproval{
		ObjectMeta: v1alpha1.ObjectMeta{Name: name, Namespace: "team-a", ResourceVersion: 9},
		Spec:       v1alpha1.RuntimeApprovalSpec{Workspace: "team-a", InvocationID: invocationID},
	}}
	authenticator := &recordingAuthenticator{identities: map[string]*auth.Identity{
		"approve-token":   {Method: "oidc", Issuer: "https://issuer.example", Subject: "approve-only"},
		"reconcile-token": {Method: "oidc", Issuer: "https://issuer.example", Subject: "reconcile-operator", Username: "forged-display"},
	}}
	server := New(Config{}, memory, zap.NewNop(), WithAuthenticator(authenticator, nil))
	server.runtimeApprovals = operations
	path := "/apis/kognition.dev/v1alpha1/namespaces/team-a/runtimeapprovals/" + name + "/reconcile"
	body := `{"invocationID":"invoke-reconcile","disposition":"ReconciledCompleted","evidenceRef":"audit://receipt/42","expectedResourceVersion":9}`

	deniedRequest := httptest.NewRequest(http.MethodPost, path, bytes.NewBufferString(body))
	deniedRequest.Header.Set("Authorization", "Bearer approve-token")
	denied := httptest.NewRecorder()
	server.ServeHTTP(denied, deniedRequest)
	if denied.Code != http.StatusForbidden {
		t.Fatalf("approve-only status = %d, want %d: %s", denied.Code, http.StatusForbidden, denied.Body.String())
	}

	wrongSubresourceRequest := httptest.NewRequest(http.MethodPost, "/apis/kognition.dev/v1alpha1/namespaces/team-a/runtimeapprovals/"+name+"/decision", bytes.NewBufferString(
		`{"invocationID":"invoke-reconcile","decision":"approve","expectedResourceVersion":9}`,
	))
	wrongSubresourceRequest.Header.Set("Authorization", "Bearer reconcile-token")
	wrongSubresource := httptest.NewRecorder()
	server.ServeHTTP(wrongSubresource, wrongSubresourceRequest)
	if wrongSubresource.Code != http.StatusForbidden {
		t.Fatalf("reconcile-only decision status = %d, want %d: %s", wrongSubresource.Code, http.StatusForbidden, wrongSubresource.Body.String())
	}

	allowedRequest := httptest.NewRequest(http.MethodPost, path, bytes.NewBufferString(body))
	allowedRequest.Header.Set("Authorization", "Bearer reconcile-token")
	allowed := httptest.NewRecorder()
	server.ServeHTTP(allowed, allowedRequest)
	if allowed.Code != http.StatusOK {
		t.Fatalf("reconciler status = %d, want %d: %s", allowed.Code, http.StatusOK, allowed.Body.String())
	}
	got := operations.reconciliation
	if got.disposition != runtimeapproval.ExecutionDispositionReconciledCompleted || got.evidenceRef != "audit://receipt/42" || got.issuer != "https://issuer.example" || got.subject != "reconcile-operator" {
		t.Fatalf("persisted reconciliation = %+v, want canonical authenticated actor and request evidence", got)
	}
}

func TestRuntimeApprovalReconciliationMapsStoreFailures(t *testing.T) {
	invocationID := "invoke-reconcile-failure"
	name := runtimeapproval.CanonicalName("team-a", invocationID)
	approval := v1alpha1.RuntimeApproval{
		ObjectMeta: v1alpha1.ObjectMeta{Name: name, Namespace: "team-a", ResourceVersion: 9},
		Spec:       v1alpha1.RuntimeApprovalSpec{Workspace: "team-a", InvocationID: invocationID},
	}
	for _, test := range []struct {
		name   string
		err    error
		status int
	}{
		{name: "conflict", err: store.ErrConflict, status: http.StatusConflict},
		{name: "storage failure", err: errors.New("write failed"), status: http.StatusInternalServerError},
	} {
		t.Run(test.name, func(t *testing.T) {
			operations := &recordingRuntimeApprovalStore{approval: approval, reconcileErr: test.err}
			server := newAuthorizedRuntimeApprovalTestServer(t, operations, "reconcile")
			request := httptest.NewRequest(http.MethodPost, "/apis/kognition.dev/v1alpha1/namespaces/team-a/runtimeapprovals/"+name+"/reconcile", bytes.NewBufferString(
				`{"invocationID":"invoke-reconcile-failure","disposition":"ReconciledCompleted","evidenceRef":"audit://receipt/42","expectedResourceVersion":9}`,
			))
			request.Header.Set("Authorization", "Bearer operator-token")
			response := httptest.NewRecorder()
			server.ServeHTTP(response, request)
			if response.Code != test.status {
				t.Fatalf("status = %d, want %d: %s", response.Code, test.status, response.Body.String())
			}
			if operations.reconciliation.disposition != "" {
				t.Fatalf("failed request recorded reconciliation %+v", operations.reconciliation)
			}
		})
	}
}

func newAuthorizedRuntimeApprovalTestServer(t *testing.T, operations *recordingRuntimeApprovalStore, verbs ...string) *Server {
	t.Helper()
	memory := newMemStore()
	role := v1alpha1.ControlPlaneRole{
		ObjectMeta: v1alpha1.ObjectMeta{Name: "runtime-operator", Namespace: operations.approval.Spec.Workspace},
		Spec: v1alpha1.ControlPlaneRoleSpec{Rules: []v1alpha1.ControlPlanePolicyRule{{
			Effect: "allow", Resources: []string{"RuntimeApproval"}, Verbs: verbs, Workspaces: []string{operations.approval.Spec.Workspace}, RuntimeApprovalNames: []string{operations.approval.Name},
		}}},
	}
	binding := v1alpha1.ControlPlaneRoleBinding{
		ObjectMeta: v1alpha1.ObjectMeta{Name: "runtime-operator", Namespace: operations.approval.Spec.Workspace},
		Spec: v1alpha1.ControlPlaneRoleBindingSpec{
			RoleRef:  v1alpha1.LocalObjectReference{Name: role.Name},
			Subjects: []v1alpha1.ControlPlaneRoleSubject{{Method: "oidc", Issuer: "https://issuer.example", Subject: "operator-1"}},
		},
	}
	for kind, resource := range map[string]any{"ControlPlaneRole": role, "ControlPlaneRoleBinding": binding} {
		data, err := json.Marshal(resource)
		if err != nil {
			t.Fatal(err)
		}
		if err := memory.Create(context.Background(), kind, operations.approval.Spec.Workspace, "runtime-operator", data); err != nil {
			t.Fatal(err)
		}
	}
	authenticator := &recordingAuthenticator{identities: map[string]*auth.Identity{
		"operator-token": {Method: "oidc", Issuer: "https://issuer.example", Subject: "operator-1"},
	}}
	server := New(Config{}, memory, zap.NewNop(), WithAuthenticator(authenticator, nil))
	server.runtimeApprovals = operations
	return server
}

func TestAuthenticatedAdmissionRejectsForgedApprovalDecision(t *testing.T) {
	authenticator := &recordingAuthenticator{identities: map[string]*auth.Identity{
		"approver-token": {Subject: "approver-id", Username: "verified-approver", Groups: []string{"reviewers"}, Method: "token"},
	}}
	server := New(Config{Addr: ":0"}, newMemStore(), zap.NewNop(), WithAuthenticator(authenticator, nil))
	path := "/apis/kognition.dev/v1alpha1/namespaces/default/approvaldecisions"

	request := func(t *testing.T, name, approver string) *httptest.ResponseRecorder {
		t.Helper()
		body, err := json.Marshal(map[string]any{
			"metadata": map[string]any{"name": name},
			"spec": map[string]any{
				"workflowRunRef": map[string]any{"name": "run"},
				"step":           "review",
				"decision":       "approve",
				"approver":       approver,
			},
		})
		if err != nil {
			t.Fatalf("marshal ApprovalDecision: %v", err)
		}
		req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer approver-token")
		req.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		server.ServeHTTP(response, req)
		return response
	}

	forged := request(t, "forged-decision", "another-user")
	if forged.Code != http.StatusUnprocessableEntity {
		t.Fatalf("forged approver status = %d, want %d: %s", forged.Code, http.StatusUnprocessableEntity, forged.Body.String())
	}

	verified := request(t, "verified-decision", "verified-approver")
	if verified.Code != http.StatusCreated {
		t.Fatalf("verified approver status = %d, want %d: %s", verified.Code, http.StatusCreated, verified.Body.String())
	}
}

func TestAutonomyReviewDecisionRequiresAuthenticationAndUsesVerifiedReviewer(t *testing.T) {
	memory := newMemStore()
	reviewFixture := v1alpha1.AutonomyReview{
		ObjectMeta: v1alpha1.ObjectMeta{Name: "promotion", Namespace: "default", ResourceVersion: 1},
		Spec: v1alpha1.AutonomyReviewSpec{
			AgentRef: v1alpha1.LocalObjectReference{Name: "researcher"}, ActionDescription: "promotion",
		},
		Status: v1alpha1.AutonomyReviewStatus{Decision: "Pending"},
	}
	fixtureData, err := json.Marshal(reviewFixture)
	if err != nil {
		t.Fatal(err)
	}
	if err := memory.Create(context.Background(), "AutonomyReview", "default", "promotion", fixtureData); err != nil {
		t.Fatal(err)
	}
	authenticator := &recordingAuthenticator{identities: map[string]*auth.Identity{
		"reviewer-token": {Subject: "reviewer-id", Username: "verified-reviewer", Method: "token"},
	}}
	server := New(Config{}, memory, zap.NewNop(), WithAuthenticator(authenticator, nil))
	path := "/apis/kognition.dev/v1alpha1/namespaces/default/autonomyreviews/promotion/decision"

	unauthenticated := httptest.NewRecorder()
	server.handleAutonomyReviewDecision(unauthenticated, httptest.NewRequest(http.MethodPost, path, bytes.NewBufferString(`{"decision":"approve"}`)))
	if unauthenticated.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status = %d, want 401", unauthenticated.Code)
	}
	authenticatedRequest := httptest.NewRequest(http.MethodPost, path, bytes.NewBufferString(`{"decision":"approve","decidedBy":"forged"}`))
	authenticatedRequest.Header.Set("Authorization", "Bearer reviewer-token")
	authenticated := httptest.NewRecorder()
	server.ServeHTTP(authenticated, authenticatedRequest)
	if authenticated.Code != http.StatusOK {
		t.Fatalf("authenticated status = %d %s, want 200", authenticated.Code, authenticated.Body)
	}
	raw, err := memory.Get(context.Background(), "AutonomyReview", "default", "promotion")
	if err != nil {
		t.Fatal(err)
	}
	var review v1alpha1.AutonomyReview
	if err := json.Unmarshal(raw, &review); err != nil {
		t.Fatal(err)
	}
	if review.Status.Decision != "Approved" || review.Status.DecidedBy != "verified-reviewer" {
		t.Fatalf("review status = %+v, want verified reviewer approval", review.Status)
	}

	replayedRequest := httptest.NewRequest(http.MethodPost, path, bytes.NewBufferString(`{"decision":"deny"}`))
	replayedRequest.Header.Set("Authorization", "Bearer reviewer-token")
	replayed := httptest.NewRecorder()
	server.ServeHTTP(replayed, replayedRequest)
	if replayed.Code != http.StatusConflict {
		t.Fatalf("replayed status = %d, want 409: %s", replayed.Code, replayed.Body.String())
	}

	missingRequest := httptest.NewRequest(http.MethodPost, "/apis/kognition.dev/v1alpha1/namespaces/default/autonomyreviews/missing/decision", bytes.NewBufferString(`{"decision":"approve"}`))
	missingRequest.Header.Set("Authorization", "Bearer reviewer-token")
	missing := httptest.NewRecorder()
	server.ServeHTTP(missing, missingRequest)
	if missing.Code != http.StatusNotFound {
		t.Fatalf("missing status = %d, want 404: %s", missing.Code, missing.Body.String())
	}
}
