// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package auth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	api "github.com/kognition-ai/kognition/api/v1alpha1"
	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestIdentityContext(t *testing.T) {
	ctx := context.Background()
	if _, ok := IdentityFromContext(ctx); ok {
		t.Fatal("empty context should have no identity")
	}
	if IsAuthenticated(ctx) {
		t.Error("empty context should not be authenticated")
	}

	id := &Identity{Subject: "u1", Username: "alice", Method: "oidc"}
	ctx = WithIdentity(ctx, id)
	got, ok := IdentityFromContext(ctx)
	if !ok || got.Username != "alice" {
		t.Fatalf("IdentityFromContext = %v, %v", got, ok)
	}
	if !IsAuthenticated(ctx) {
		t.Error("should be authenticated")
	}

	anon := WithIdentity(context.Background(), Anonymous())
	if IsAuthenticated(anon) {
		t.Error("anonymous should not count as authenticated")
	}
}

func TestStaticTokenAuthenticator(t *testing.T) {
	a := NewStaticTokenAuthenticator(map[string]string{"secret-abc": "svc-1"})
	ctx := context.Background()

	if _, err := a.Authenticate(ctx, ""); !errors.Is(err, ErrNoToken) {
		t.Errorf("empty token: want ErrNoToken, got %v", err)
	}
	id, err := a.Authenticate(ctx, "secret-abc")
	if err != nil {
		t.Fatalf("valid token: %v", err)
	}
	if id.Subject != "svc-1" || id.Method != "token" {
		t.Errorf("identity = %+v", id)
	}
	if _, err := a.Authenticate(ctx, "wrong"); !errors.Is(err, ErrInvalidToken) {
		t.Errorf("wrong token: want ErrInvalidToken, got %v", err)
	}
}

func TestChain(t *testing.T) {
	static := NewStaticTokenAuthenticator(map[string]string{"tok": "svc"})
	chain := Chain{static}
	ctx := context.Background()

	if id, err := chain.Authenticate(ctx, "tok"); err != nil || id.Subject != "svc" {
		t.Errorf("valid: %v, %v", id, err)
	}
	if _, err := chain.Authenticate(ctx, ""); !errors.Is(err, ErrNoToken) {
		t.Errorf("empty: want ErrNoToken, got %v", err)
	}
	if _, err := chain.Authenticate(ctx, "nope"); !errors.Is(err, ErrInvalidToken) {
		t.Errorf("invalid: want ErrInvalidToken, got %v", err)
	}
}

// fakeAuth is a test Authenticator: empty token -> ErrNoToken, otherwise returns
// the configured identity/error.
type fakeAuth struct {
	id  *Identity
	err error
}

func (f fakeAuth) Authenticate(_ context.Context, token string) (*Identity, error) {
	if token == "" {
		return nil, ErrNoToken
	}
	if f.err != nil {
		return nil, f.err
	}
	return f.id, nil
}

func newRequest(token string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/apis/kognition.dev/v1alpha1/namespaces/default/agents", nil)
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	return r
}

func TestMiddleware_RejectsWithoutToken(t *testing.T) {
	h := Middleware(fakeAuth{id: &Identity{Username: "x"}}, MiddlewareConfig{})(
		http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("handler must not run") }),
	)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newRequest(""))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
	if rec.Header().Get("WWW-Authenticate") == "" {
		t.Error("expected WWW-Authenticate header on 401")
	}
}

func TestMiddleware_PassesAndSetsIdentity(t *testing.T) {
	want := &Identity{Subject: "u1", Username: "alice", Method: "oidc"}
	var seen *Identity
	h := Middleware(fakeAuth{id: want}, MiddlewareConfig{})(
		http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			seen, _ = IdentityFromContext(r.Context())
		}),
	)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newRequest("valid"))
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	if seen == nil || seen.Username != "alice" {
		t.Errorf("identity in context = %v", seen)
	}
}

func TestMiddleware_SkipPaths(t *testing.T) {
	called := false
	h := Middleware(fakeAuth{}, MiddlewareConfig{SkipPaths: map[string]bool{"/healthz": true}})(
		http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }),
	)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if !called || rec.Code != http.StatusOK {
		t.Errorf("health path should bypass auth: called=%v code=%d", called, rec.Code)
	}
}

func TestMiddleware_RequiredGroups(t *testing.T) {
	authr := fakeAuth{id: &Identity{Username: "bob", Groups: []string{"devs"}, Method: "oidc"}}

	// Caller lacks the required group -> 403.
	denied := Middleware(authr, MiddlewareConfig{RequiredGroups: []string{"admins"}})(
		http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("handler must not run") }),
	)
	rec := httptest.NewRecorder()
	denied.ServeHTTP(rec, newRequest("valid"))
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", rec.Code)
	}

	// Caller has a required group -> allowed.
	ok := false
	allowed := Middleware(authr, MiddlewareConfig{RequiredGroups: []string{"devs", "admins"}})(
		http.HandlerFunc(func(http.ResponseWriter, *http.Request) { ok = true }),
	)
	rec2 := httptest.NewRecorder()
	allowed.ServeHTTP(rec2, newRequest("valid"))
	if !ok || rec2.Code != http.StatusOK {
		t.Errorf("caller with required group should pass: ok=%v code=%d", ok, rec2.Code)
	}
}

type workflowTokenReviewer struct {
	status authenticationv1.TokenReviewStatus
	err    error
	token  string
	calls  int
}

func TestWorkflowExecutorAttestationConfiguration(t *testing.T) {
	principal := api.ControlPlaneRoleSubject{Method: "oidc", Issuer: "https://cluster.example", Subject: "system:serviceaccount:team-a:executor"}
	principals := make([]api.ControlPlaneRoleSubject, 17)
	for index := range principals {
		principals[index] = principal
		principals[index].Subject += "-" + string(rune('a'+index))
	}
	for _, test := range []struct {
		name       string
		principals []api.ControlPlaneRoleSubject
		missing    string
		valid      bool
	}{
		{name: "one-executor", principals: []api.ControlPlaneRoleSubject{principal}, valid: true},
		{name: "sixteen-executors", principals: principals[:16], valid: true},
		{name: "seventeen-executors", principals: principals},
		{name: "no-executors"},
		{name: "duplicate-executor", principals: []api.ControlPlaneRoleSubject{principal, principal}},
		{name: "missing-authentication", principals: []api.ControlPlaneRoleSubject{principal}, missing: "authentication"},
		{name: "missing-reviewer", principals: []api.ControlPlaneRoleSubject{principal}, missing: "reviewer"},
		{name: "missing-reader", principals: []api.ControlPlaneRoleSubject{principal}, missing: "reader"},
		{name: "static-credential", principals: []api.ControlPlaneRoleSubject{{Method: "token", Issuer: principal.Issuer, Subject: principal.Subject}}},
		{name: "insecure-issuer", principals: []api.ControlPlaneRoleSubject{{Method: "oidc", Issuer: "http://cluster.example", Subject: principal.Subject}}},
		{name: "issuer-userinfo", principals: []api.ControlPlaneRoleSubject{{Method: "oidc", Issuer: "https://user@cluster.example", Subject: principal.Subject}}},
		{name: "issuer-query", principals: []api.ControlPlaneRoleSubject{{Method: "oidc", Issuer: "https://cluster.example?scope=executor", Subject: principal.Subject}}},
		{name: "issuer-fragment", principals: []api.ControlPlaneRoleSubject{{Method: "oidc", Issuer: "https://cluster.example#executor", Subject: principal.Subject}}},
		{name: "human-subject", principals: []api.ControlPlaneRoleSubject{{Method: "oidc", Issuer: principal.Issuer, Subject: "operator"}}},
		{name: "shared-group", principals: []api.ControlPlaneRoleSubject{{Method: "oidc", Issuer: principal.Issuer, Subject: "system:serviceaccounts:team-a"}}},
		{name: "empty-namespace", principals: []api.ControlPlaneRoleSubject{{Method: "oidc", Issuer: principal.Issuer, Subject: "system:serviceaccount::executor"}}},
		{name: "wildcard-account", principals: []api.ControlPlaneRoleSubject{{Method: "oidc", Issuer: principal.Issuer, Subject: "system:serviceaccount:team-a:*"}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			var base Authenticator = fakeAuth{id: &Identity{Method: "oidc", Subject: "operator", Issuer: principal.Issuer}}
			var reviewer WorkflowExecutorTokenReviewer = &workflowTokenReviewer{}
			reader := fake.NewClientBuilder().Build()
			switch test.missing {
			case "authentication":
				base = nil
			case "reviewer":
				reviewer = nil
			case "reader":
				reader = nil
			}
			authenticator, err := NewWorkflowExecutorAuthenticator(base, test.principals, reviewer, reader)
			if test.valid {
				if err != nil || authenticator == nil {
					t.Fatalf("valid bounded executor configuration rejected: %v", err)
				}
			} else if err == nil || authenticator != nil {
				t.Fatal("invalid executor configuration constructed an authenticator")
			}
		})
	}
}

func TestWorkflowExecutorAttestationPreservesOrdinaryAuthentication(t *testing.T) {
	principal := api.ControlPlaneRoleSubject{Method: "oidc", Issuer: "https://cluster.example", Subject: "system:serviceaccount:team-a:executor"}
	for _, identity := range []*Identity{
		{Method: "oidc", Issuer: principal.Issuer, Subject: "operator", Groups: []string{"operators"}},
		{Method: "oidc", Issuer: "https://other.example", Subject: principal.Subject},
		{Method: "token", Subject: "bootstrap"},
	} {
		t.Run(identity.Method+":"+identity.Issuer+":"+identity.Subject, func(t *testing.T) {
			reviewer := &workflowTokenReviewer{err: errors.New("workload proof must not run for ordinary callers")}
			authenticator, err := NewWorkflowExecutorAuthenticator(fakeAuth{id: identity}, []api.ControlPlaneRoleSubject{principal}, reviewer, fake.NewClientBuilder().Build())
			if err != nil {
				t.Fatal(err)
			}
			verified, err := authenticator.Authenticate(context.Background(), "ordinary-verified-token")
			if err != nil || verified == nil || verified.Method != identity.Method || verified.Issuer != identity.Issuer || verified.Subject != identity.Subject || len(verified.Groups) != len(identity.Groups) {
				t.Fatalf("ordinary authenticated caller changed: %+v %v", verified, err)
			}
			if _, err := AuthenticatedWorkflowControllerUID(WithIdentity(context.Background(), verified)); !errors.Is(err, ErrInvalidToken) {
				t.Fatalf("ordinary caller acquired controller proof: %v", err)
			}
			if reviewer.calls != 0 {
				t.Fatal("ordinary caller caused workload TokenReview")
			}
		})
	}
}

func (reviewer *workflowTokenReviewer) Create(_ context.Context, review *authenticationv1.TokenReview, _ metav1.CreateOptions) (*authenticationv1.TokenReview, error) {
	reviewer.calls++
	reviewer.token = review.Spec.Token
	if len(review.Spec.Audiences) != 1 || review.Spec.Audiences[0] != WorkflowExecutorTokenAudience {
		return nil, errors.New("incorrect attestation audience")
	}
	return &authenticationv1.TokenReview{Status: reviewer.status}, reviewer.err
}

func TestWorkflowExecutorAttestationUsesLiveBoundToken(t *testing.T) {
	principal := api.ControlPlaneRoleSubject{Method: "oidc", Issuer: "https://cluster.example", Subject: "system:serviceaccount:team-a:executor"}
	id := &Identity{Method: principal.Method, Issuer: principal.Issuer, Subject: principal.Subject,
		Claims: map[string]any{"controllerUID": "caller-chosen-uid"}}
	if _, err := AuthenticatedWorkflowControllerUID(WithIdentity(context.Background(), id)); err == nil {
		t.Fatal("OIDC claims alone established a controller incarnation")
	}
	for _, test := range []struct {
		name   string
		change func(*workflowTokenReviewer, *corev1.Pod, *corev1.ServiceAccount)
	}{
		{name: "bound-ready"},
		{name: "review-denied", change: func(review *workflowTokenReviewer, _ *corev1.Pod, _ *corev1.ServiceAccount) {
			review.status.Authenticated = false
		}},
		{name: "review-error", change: func(review *workflowTokenReviewer, _ *corev1.Pod, _ *corev1.ServiceAccount) {
			review.err = errors.New("unavailable")
		}},
		{name: "review-status-error", change: func(review *workflowTokenReviewer, _ *corev1.Pod, _ *corev1.ServiceAccount) {
			review.status.Error = "unavailable"
		}},
		{name: "wrong-audience", change: func(review *workflowTokenReviewer, _ *corev1.Pod, _ *corev1.ServiceAccount) {
			review.status.Audiences = []string{"api"}
		}},
		{name: "wrong-principal", change: func(review *workflowTokenReviewer, _ *corev1.Pod, _ *corev1.ServiceAccount) {
			review.status.User.Username = "system:serviceaccount:team-a:other"
		}},
		{name: "unbound", change: func(review *workflowTokenReviewer, _ *corev1.Pod, _ *corev1.ServiceAccount) {
			review.status.User.Extra = nil
		}},
		{name: "multiple-pods", change: func(review *workflowTokenReviewer, _ *corev1.Pod, _ *corev1.ServiceAccount) {
			review.status.User.Extra["authentication.kubernetes.io/pod-uid"] = authenticationv1.ExtraValue{"pod-uid", "other"}
		}},
		{name: "unauthenticated-group", change: func(review *workflowTokenReviewer, _ *corev1.Pod, _ *corev1.ServiceAccount) {
			review.status.User.Groups = []string{"system:unauthenticated"}
		}},
		{name: "replaced-pod", change: func(_ *workflowTokenReviewer, pod *corev1.Pod, _ *corev1.ServiceAccount) { pod.UID = "replacement" }},
		{name: "unready-pod", change: func(_ *workflowTokenReviewer, pod *corev1.Pod, _ *corev1.ServiceAccount) {
			pod.Status.Conditions[0].Status = corev1.ConditionFalse
		}},
		{name: "stopped-pod", change: func(_ *workflowTokenReviewer, pod *corev1.Pod, _ *corev1.ServiceAccount) {
			pod.Status.Phase = corev1.PodSucceeded
		}},
		{name: "wrong-account", change: func(_ *workflowTokenReviewer, pod *corev1.Pod, _ *corev1.ServiceAccount) {
			pod.Spec.ServiceAccountName = "other"
		}},
		{name: "replaced-account", change: func(_ *workflowTokenReviewer, _ *corev1.Pod, account *corev1.ServiceAccount) {
			account.UID = "replacement"
		}},
		{name: "shared-incarnation", change: func(review *workflowTokenReviewer, _ *corev1.Pod, account *corev1.ServiceAccount) {
			account.UID = "pod-uid"
			review.status.User.UID = "pod-uid"
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			scheme := runtime.NewScheme()
			if err := corev1.AddToScheme(scheme); err != nil {
				t.Fatal(err)
			}
			pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "executor-pod", UID: "pod-uid"},
				Spec:   corev1.PodSpec{ServiceAccountName: "executor"},
				Status: corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}}}
			account := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "executor", UID: "account-uid"}}
			reviewer := &workflowTokenReviewer{status: authenticationv1.TokenReviewStatus{Authenticated: true, Audiences: []string{WorkflowExecutorTokenAudience},
				User: authenticationv1.UserInfo{Username: principal.Subject, UID: "account-uid", Extra: map[string]authenticationv1.ExtraValue{
					"authentication.kubernetes.io/pod-name": {"executor-pod"}, "authentication.kubernetes.io/pod-uid": {"pod-uid"},
				}}}}
			if test.change != nil {
				test.change(reviewer, pod, account)
			}
			reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pod, account).Build()
			authenticator, err := NewWorkflowExecutorAuthenticator(fakeAuth{id: id}, []api.ControlPlaneRoleSubject{principal}, reviewer, reader)
			if err != nil {
				t.Fatal(err)
			}
			identity, err := authenticator.Authenticate(context.Background(), "same-verified-token")
			if test.change != nil {
				if err == nil || identity != nil {
					t.Fatal("invalid workload proof authenticated the executor")
				}
				return
			}
			if err != nil || identity == nil || reviewer.token != "same-verified-token" {
				t.Fatalf("same-token attestation failed: %+v %v", identity, err)
			}
			ctx := WithIdentity(context.Background(), identity)
			uid, err := AuthenticatedWorkflowControllerUID(ctx)
			if err != nil || uid != "pod-uid" || reviewer.calls != 2 {
				t.Fatalf("controller proof was not live and independently bound: %q %v calls=%d", uid, err, reviewer.calls)
			}
			pod.UID = types.UID("replacement")
			if err := reader.Update(context.Background(), pod); err != nil {
				t.Fatal(err)
			}
			if _, err := AuthenticatedWorkflowControllerUID(ctx); err == nil {
				t.Fatal("retained request identity survived controller replacement")
			}
			if id.workflowControllerProof != nil {
				t.Fatal("authentication mutated the upstream identity")
			}
		})
	}
}
