// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build integration

// Package webhook integration tests against a real kube-apiserver via envtest.
//
// To run:
//
//	# 1. Install envtest binaries (one time):
//	go install sigs.k8s.io/controller-runtime/tools/setup-envtest@latest
//	setup-envtest use 1.30.x
//
//	# 2. Point KUBEBUILDER_ASSETS at the binary directory, e.g.:
//	$env:KUBEBUILDER_ASSETS = (setup-envtest use 1.30.x -p path)
//
//	# 3. Run:
//	go test -tags=integration ./internal/k8s/webhook/... -count=1 -v
//
// The harness:
//   - boots a real kube-apiserver+etcd via controller-runtime/pkg/envtest,
//   - installs all 18 Kognition CRDs from deploy/helm/kognition/crds,
//   - serves the webhook Handler over HTTPS with a self-signed cert,
//   - registers ValidatingWebhookConfiguration and MutatingWebhookConfiguration
//     pointed at the test server with the self-signed CA bundle,
//   - exercises end-to-end admission scenarios through the real apiserver.

package webhook

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	admissionv1 "k8s.io/api/admission/v1"
	admissionregv1 "k8s.io/api/admissionregistration/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	k8sapi "github.com/kognition-ai/kognition/api/k8s/v1alpha1"
	intadmission "github.com/kognition-ai/kognition/internal/admission"
)

// envtestState holds resources shared across the integration tests.
type envtestState struct {
	testEnv    *envtest.Environment
	cfg        *rest.Config
	kube       kubernetes.Interface
	ctrlClient client.Client

	server     *httptest.Server
	caPEM      []byte
	mu         sync.Mutex
	mutators   []intadmission.MutatingHook
	validators []intadmission.ValidatingHook
}

var envState *envtestState

// crdDir returns the absolute path to the Helm CRD directory.
// Test working dir is the package dir: internal/k8s/webhook.
func crdDir() (string, error) {
	return filepath.Abs(filepath.Join("..", "..", "..", "deploy", "helm", "kognition", "crds"))
}

// generateSelfSignedCert creates an ECDSA self-signed cert valid for 127.0.0.1.
// Returns the leaf cert (also used as CA) PEM, key PEM, and a tls.Certificate.
func generateSelfSignedCert() (certPEM, keyPEM []byte, tlsCert tls.Certificate, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, tls.Certificate{}, fmt.Errorf("generate key: %w", err)
	}
	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	tmpl := x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "kognition-webhook-test"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
		DNSNames:              []string{"localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, tls.Certificate{}, fmt.Errorf("create cert: %w", err)
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, nil, tls.Certificate{}, fmt.Errorf("marshal key: %w", err)
	}
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	tlsCert, err = tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, nil, tls.Certificate{}, fmt.Errorf("load keypair: %w", err)
	}
	return certPEM, keyPEM, tlsCert, nil
}

// webhookHTTPHandler wraps the Kognition admission Handler into a plain http.Handler
// that speaks the AdmissionReview protocol. The chain it uses is rebuilt per-request
// from envState so individual tests can install their own stub hooks.
type webhookHTTPHandler struct{ mode string }

func (h webhookHTTPHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	defer r.Body.Close()

	var review admissionv1.AdmissionReview
	if err := json.Unmarshal(body, &review); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if review.Request == nil {
		http.Error(w, "missing admission request", http.StatusBadRequest)
		return
	}

	envState.mu.Lock()
	var mutators []intadmission.MutatingHook
	var validators []intadmission.ValidatingHook
	switch h.mode {
	case "mutate":
		mutators = append([]intadmission.MutatingHook{}, envState.mutators...)
	case "validate":
		validators = append([]intadmission.ValidatingHook{}, envState.validators...)
	}
	envState.mu.Unlock()

	chain := intadmission.NewChain(mutators, validators)
	handler := NewHandler(chain, testScheme(), testLogger())
	req := admission.Request{AdmissionRequest: *review.Request}
	resp := handler.Handle(r.Context(), req)
	// Complete marshals Patches into the Patch byte field; without it the
	// apiserver rejects responses that have patchType set but no patch body.
	if err := resp.Complete(req); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	out := admissionv1.AdmissionReview{
		TypeMeta: metav1.TypeMeta{APIVersion: "admission.k8s.io/v1", Kind: "AdmissionReview"},
		Response: &resp.AdmissionResponse,
	}
	if out.Response != nil {
		out.Response.UID = review.Request.UID
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

// installHooks resets the per-test hook lists used by webhookHTTPHandler.
func installHooks(mutators []intadmission.MutatingHook, validators []intadmission.ValidatingHook) {
	envState.mu.Lock()
	defer envState.mu.Unlock()
	envState.mutators = mutators
	envState.validators = validators
}

// failurePolicyFail is shared by webhook registrations so misconfiguration is loud.
var (
	failPolicy = admissionregv1.Fail
	sideNone   = admissionregv1.SideEffectClassNone
	scopeAll   = admissionregv1.AllScopes
	equiv      = admissionregv1.Equivalent
)

func registerWebhooks(ctx context.Context, kube kubernetes.Interface, baseURL string, caBundle []byte) error {
	validateURL := baseURL + "/validate"
	mutateURL := baseURL + "/mutate"

	rules := []admissionregv1.RuleWithOperations{{
		Operations: []admissionregv1.OperationType{admissionregv1.Create, admissionregv1.Update},
		Rule: admissionregv1.Rule{
			APIGroups:   []string{"kognition.dev"},
			APIVersions: []string{"v1alpha1"},
			Resources:   []string{"agents", "trustprofiles", "runtimeapprovals", "runtimeapprovals/status"},
			Scope:       &scopeAll,
		},
	}}

	timeout := int32(10)

	vwh := &admissionregv1.ValidatingWebhookConfiguration{
		ObjectMeta: metav1.ObjectMeta{Name: "kognition-validate-envtest"},
		Webhooks: []admissionregv1.ValidatingWebhook{{
			Name: "validate.kognition.dev",
			ClientConfig: admissionregv1.WebhookClientConfig{
				URL:      &validateURL,
				CABundle: caBundle,
			},
			Rules:                   rules,
			FailurePolicy:           &failPolicy,
			MatchPolicy:             &equiv,
			SideEffects:             &sideNone,
			AdmissionReviewVersions: []string{"v1"},
			TimeoutSeconds:          &timeout,
		}},
	}

	mwh := &admissionregv1.MutatingWebhookConfiguration{
		ObjectMeta: metav1.ObjectMeta{Name: "kognition-mutate-envtest"},
		Webhooks: []admissionregv1.MutatingWebhook{{
			Name: "mutate.kognition.dev",
			ClientConfig: admissionregv1.WebhookClientConfig{
				URL:      &mutateURL,
				CABundle: caBundle,
			},
			Rules:                   rules,
			FailurePolicy:           &failPolicy,
			MatchPolicy:             &equiv,
			SideEffects:             &sideNone,
			AdmissionReviewVersions: []string{"v1"},
			TimeoutSeconds:          &timeout,
		}},
	}

	if _, err := kube.AdmissionregistrationV1().ValidatingWebhookConfigurations().Create(ctx, vwh, metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("create validating webhook: %w", err)
	}
	if _, err := kube.AdmissionregistrationV1().MutatingWebhookConfigurations().Create(ctx, mwh, metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("create mutating webhook: %w", err)
	}
	return nil
}

func TestMain(m *testing.M) {
	// Bail out cleanly if envtest binaries aren't installed.
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		fmt.Fprintln(os.Stderr, "KUBEBUILDER_ASSETS is not set; skipping envtest integration suite")
		fmt.Fprintln(os.Stderr, "Hint: setup-envtest use 1.30.x; $env:KUBEBUILDER_ASSETS = (setup-envtest use 1.30.x -p path)")
		os.Exit(0)
	}

	state := &envtestState{}
	envState = state

	crdPath, err := crdDir()
	if err != nil {
		fmt.Fprintf(os.Stderr, "resolve crd dir: %v\n", err)
		os.Exit(1)
	}
	state.testEnv = &envtest.Environment{
		CRDDirectoryPaths:     []string{crdPath},
		ErrorIfCRDPathMissing: true,
	}

	cfg, err := state.testEnv.Start()
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to start envtest: %v\n", err)
		os.Exit(1)
	}
	state.cfg = cfg

	kube, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		_ = state.testEnv.Stop()
		fmt.Fprintf(os.Stderr, "kubernetes client: %v\n", err)
		os.Exit(1)
	}
	state.kube = kube

	ctrlClient, err := client.New(cfg, client.Options{Scheme: scheme()})
	if err != nil {
		_ = state.testEnv.Stop()
		fmt.Fprintf(os.Stderr, "ctrl client: %v\n", err)
		os.Exit(1)
	}
	state.ctrlClient = ctrlClient

	// Build TLS server.
	certPEM, _, tlsCert, err := generateSelfSignedCert()
	if err != nil {
		_ = state.testEnv.Stop()
		fmt.Fprintf(os.Stderr, "tls cert: %v\n", err)
		os.Exit(1)
	}
	state.caPEM = certPEM

	mux := http.NewServeMux()
	mux.Handle("/validate", webhookHTTPHandler{mode: "validate"})
	mux.Handle("/mutate", webhookHTTPHandler{mode: "mutate"})

	srv := httptest.NewUnstartedServer(mux)
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{tlsCert}, MinVersion: tls.VersionTLS12}
	srv.StartTLS()
	state.server = srv

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := registerWebhooks(ctx, kube, srv.URL, certPEM); err != nil {
		srv.Close()
		_ = state.testEnv.Stop()
		fmt.Fprintf(os.Stderr, "register webhooks: %v\n", err)
		os.Exit(1)
	}

	code := m.Run()

	srv.Close()
	_ = state.testEnv.Stop()
	os.Exit(code)
}

func scheme() *runtime.Scheme {
	s := runtime.NewScheme()
	utilruntime.Must(k8sapi.AddToScheme(s))
	return s
}

// waitForWebhookReady creates a throwaway namespace then attempts the dummy admission
// path until the apiserver successfully reaches our webhook (or times out).
func waitForWebhookReady(t *testing.T, ctx context.Context) {
	t.Helper()
	installHooks(nil, []intadmission.ValidatingHook{
		&stubValidator{
			name:  "ready-probe",
			kinds: map[string]bool{"Agent": true},
			validateFn: func(_ context.Context, _ *intadmission.Request) *intadmission.Response {
				return &intadmission.Response{Allowed: true}
			},
		},
	})
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		agent := &k8sapi.Agent{
			TypeMeta:   metav1.TypeMeta{Kind: "Agent", APIVersion: "kognition.dev/v1alpha1"},
			ObjectMeta: metav1.ObjectMeta{Name: "probe", Namespace: "default"},
			Spec:       k8sapi.AgentSpec{ModelRef: k8sapi.LocalObjectReference{Name: "m"}, Instructions: k8sapi.AgentInstructions{Inline: "hi"}},
		}
		err := envState.ctrlClient.Create(ctx, agent)
		if err == nil {
			_ = envState.ctrlClient.Delete(ctx, agent)
			return
		}
		// Surface only non-webhook errors; retry on webhook plumbing errors.
		var statusErr *apierrors.StatusError
		if errors.As(err, &statusErr) && statusErr.ErrStatus.Code >= 500 {
			time.Sleep(500 * time.Millisecond)
			continue
		}
		t.Fatalf("unexpected error during readiness probe: %v", err)
	}
	t.Fatal("webhook did not become ready within 20s")
}

// TC-0215, TC-0247: end-to-end validating webhook rejection through the real apiserver.
func TestEnvtest_AgentMissingModelRef_RejectedByAPIServer(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	waitForWebhookReady(t, ctx)

	installHooks(nil, []intadmission.ValidatingHook{
		&stubValidator{
			name:  "require-model-ref",
			kinds: map[string]bool{"Agent": true},
			validateFn: func(_ context.Context, req *intadmission.Request) *intadmission.Response {
				var obj map[string]any
				_ = json.Unmarshal(req.Object, &obj)
				spec, _ := obj["spec"].(map[string]any)
				mr, _ := spec["modelRef"].(map[string]any)
				name, _ := mr["name"].(string)
				if name == "" {
					return &intadmission.Response{Allowed: false, Reason: "spec.modelRef.name is required"}
				}
				return &intadmission.Response{Allowed: true}
			},
		},
	})

	agent := &k8sapi.Agent{
		TypeMeta:   metav1.TypeMeta{Kind: "Agent", APIVersion: "kognition.dev/v1alpha1"},
		ObjectMeta: metav1.ObjectMeta{Name: "tc-0215", Namespace: "default"},
		Spec:       k8sapi.AgentSpec{Instructions: k8sapi.AgentInstructions{Inline: "hi"}},
	}
	err := envState.ctrlClient.Create(ctx, agent)
	if err == nil {
		t.Fatal("expected creation to be rejected by validating webhook")
	}
	if !apierrors.IsForbidden(err) && !apierrors.IsInvalid(err) {
		t.Fatalf("expected Forbidden/Invalid from apiserver, got: %v", err)
	}
}

// TC-0216, TC-0248: end-to-end mutating webhook applies a patch that the apiserver persists.
func TestEnvtest_AgentMutated_PatchPersisted(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	waitForWebhookReady(t, ctx)

	installHooks([]intadmission.MutatingHook{
		&stubMutator{
			name:  "agent-defaulter",
			kinds: map[string]bool{"Agent": true},
			mutateFn: func(_ context.Context, req *intadmission.Request) *intadmission.Response {
				var obj map[string]any
				_ = json.Unmarshal(req.Object, &obj)
				spec, _ := obj["spec"].(map[string]any)
				if spec == nil {
					spec = map[string]any{}
				}
				// Always override (CRD defaulting sets replicas=1 before mutator runs).
				spec["replicas"] = float64(3)
				obj["spec"] = spec
				patched, _ := json.Marshal(obj)
				return &intadmission.Response{Allowed: true, Patch: patched}
			},
		},
	}, nil)

	agent := &k8sapi.Agent{
		TypeMeta:   metav1.TypeMeta{Kind: "Agent", APIVersion: "kognition.dev/v1alpha1"},
		ObjectMeta: metav1.ObjectMeta{Name: "tc-0216", Namespace: "default"},
		Spec: k8sapi.AgentSpec{
			ModelRef:     k8sapi.LocalObjectReference{Name: "gpt-4"},
			Instructions: k8sapi.AgentInstructions{Inline: "hi"},
		},
	}
	if err := envState.ctrlClient.Create(ctx, agent); err != nil {
		t.Fatalf("create agent: %v", err)
	}
	t.Cleanup(func() { _ = envState.ctrlClient.Delete(context.Background(), agent) })

	fetched := &k8sapi.Agent{}
	if err := envState.ctrlClient.Get(ctx, client.ObjectKey{Namespace: "default", Name: "tc-0216"}, fetched); err != nil {
		t.Fatalf("get agent: %v", err)
	}
	if fetched.Spec.Replicas == nil || *fetched.Spec.Replicas != 3 {
		got := "<nil>"
		if fetched.Spec.Replicas != nil {
			got = fmt.Sprintf("%d", *fetched.Spec.Replicas)
		}
		t.Fatalf("expected replicas=3 after mutating webhook, got %s", got)
	}
}

// TC-0217, TC-0249: end-to-end happy path with no hooks installed — apiserver round-trip succeeds.
func TestEnvtest_AgentCreate_NoHooks_Succeeds(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	waitForWebhookReady(t, ctx)
	installHooks(nil, nil)

	agent := &k8sapi.Agent{
		TypeMeta:   metav1.TypeMeta{Kind: "Agent", APIVersion: "kognition.dev/v1alpha1"},
		ObjectMeta: metav1.ObjectMeta{Name: "tc-0217", Namespace: "default"},
		Spec: k8sapi.AgentSpec{
			ModelRef:     k8sapi.LocalObjectReference{Name: "gpt-4"},
			Instructions: k8sapi.AgentInstructions{Inline: "hi"},
		},
	}
	if err := envState.ctrlClient.Create(ctx, agent); err != nil {
		t.Fatalf("create agent: %v", err)
	}
	t.Cleanup(func() { _ = envState.ctrlClient.Delete(context.Background(), agent) })
}

func TestEnvtest_RuntimeApprovalStatusSubresourceCASAndImmutability(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	waitForWebhookReady(t, ctx)
	installHooks(nil, nil)

	approval := envtestRuntimeApproval("runtime-approval-foundation")
	approval.Status = k8sapi.RuntimeApprovalStatus{DecisionState: "Pending", ExecutionState: "NotStarted"}
	if err := envState.ctrlClient.Create(ctx, approval); err != nil {
		t.Fatalf("create RuntimeApproval: %v", err)
	}
	t.Cleanup(func() { _ = envState.ctrlClient.Delete(context.Background(), approval) })
	if approval.Status.DecisionState != "" {
		t.Fatalf("status was persisted through CREATE despite status subresource: %#v", approval.Status)
	}

	approval.Status = k8sapi.RuntimeApprovalStatus{DecisionState: "Pending", ExecutionState: "NotStarted"}
	if err := envState.ctrlClient.Status().Update(ctx, approval); err != nil {
		t.Fatalf("initialize RuntimeApproval status: %v", err)
	}
	stale := approval.DeepCopy()
	approval.Status.DecisionState = "Approved"
	approval.Status.DecidedByIssuer = "https://issuer.example"
	approval.Status.DecidedBySubject = "operator-1"
	decidedAt := metav1.NewTime(time.Now().UTC())
	approval.Status.DecidedAt = &decidedAt
	if err := envState.ctrlClient.Status().Update(ctx, approval); !apierrors.IsInvalid(err) {
		t.Fatalf("bare direct decision error = %v, want invalid", err)
	}
	if err := envState.ctrlClient.Get(ctx, client.ObjectKeyFromObject(approval), approval); err != nil {
		t.Fatal(err)
	}
	approval.Status.DecisionState = "Approved"
	approval.Status.DecidedByIssuer = "https://issuer.example"
	approval.Status.DecidedBySubject = "operator-1"
	approval.Status.DecidedAt = &decidedAt
	approval.Status.DecisionAudit = &k8sapi.RuntimeApprovalDecisionAudit{ID: "rad-test", Payload: k8sapi.RuntimeApprovalDecisionAuditPayload{
		Workspace: "default", InvocationID: approval.Spec.InvocationID, ApprovalUID: string(approval.UID), Decision: "Approved",
		Issuer: approval.Status.DecidedByIssuer, Subject: approval.Status.DecidedBySubject, DecidedAt: decidedAt,
	}}
	if err := envState.ctrlClient.Status().Update(ctx, approval); err != nil {
		t.Fatalf("decide RuntimeApproval with atomic evidence: %v", err)
	}
	stale.Status.DecisionState = "Rejected"
	if err := envState.ctrlClient.Status().Update(ctx, stale); !apierrors.IsConflict(err) {
		t.Fatalf("stale status CAS error = %v, want conflict", err)
	}

	latest := &k8sapi.RuntimeApproval{}
	if err := envState.ctrlClient.Get(ctx, client.ObjectKeyFromObject(approval), latest); err != nil {
		t.Fatal(err)
	}
	latest.Spec.Operation = "changed"
	if err := envState.ctrlClient.Update(ctx, latest); !apierrors.IsInvalid(err) {
		t.Fatalf("immutable spec update error = %v, want invalid", err)
	}
	if err := envState.ctrlClient.Get(ctx, client.ObjectKeyFromObject(approval), latest); err != nil {
		t.Fatal(err)
	}
	latest.Status.DecisionState = "Rejected"
	if err := envState.ctrlClient.Status().Update(ctx, latest); !apierrors.IsInvalid(err) {
		t.Fatalf("terminal decision update error = %v, want invalid", err)
	}
}

func TestEnvtest_TrustProfileAgentIncarnationIsImmutable(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	waitForWebhookReady(t, ctx)
	installHooks(nil, nil)

	agent := &k8sapi.Agent{TypeMeta: metav1.TypeMeta{Kind: "Agent", APIVersion: "kognition.dev/v1alpha1"}, ObjectMeta: metav1.ObjectMeta{Name: "trust-agent", Namespace: "default"}, Spec: k8sapi.AgentSpec{ModelRef: k8sapi.LocalObjectReference{Name: "model"}, Instructions: k8sapi.AgentInstructions{Inline: "test"}}}
	if err := envState.ctrlClient.Create(ctx, agent); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = envState.ctrlClient.Delete(context.Background(), agent) })
	profile := &k8sapi.TrustProfile{TypeMeta: metav1.TypeMeta{Kind: "TrustProfile", APIVersion: "kognition.dev/v1alpha1"}, ObjectMeta: metav1.ObjectMeta{Name: "trust-profile", Namespace: "default"}, Spec: k8sapi.TrustProfileSpec{AgentRef: k8sapi.LocalObjectReference{Name: agent.Name}, AgentIncarnationUID: string(agent.UID), InitialScore: 50}}
	if err := envState.ctrlClient.Create(ctx, profile); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = envState.ctrlClient.Delete(context.Background(), profile) })
	profile.Spec.AgentIncarnationUID = "forged-incarnation"
	if err := envState.ctrlClient.Update(ctx, profile); !apierrors.IsInvalid(err) {
		t.Fatalf("immutable TrustProfile incarnation error = %v, want invalid", err)
	}
}

func envtestRuntimeApproval(name string) *k8sapi.RuntimeApproval {
	namespaced := func(kind, resourceName string) k8sapi.RuntimeApprovalReference {
		return k8sapi.RuntimeApprovalReference{Kind: kind, Scope: "Namespaced", Namespace: "default", Name: resourceName, UID: resourceName + "-uid", ResourceVersion: 1}
	}
	now := time.Now().UTC()
	return &k8sapi.RuntimeApproval{
		TypeMeta: metav1.TypeMeta{Kind: "RuntimeApproval", APIVersion: "kognition.dev/v1alpha1"}, ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: k8sapi.RuntimeApprovalSpec{
			Workspace: "default", InvocationID: name, InitialDecision: "Pending", Plane: "Tool", Operation: "invoke", SessionRef: "session",
			AgentRef: namespaced("Agent", "agent"), AgentIdentityRef: namespaced("AgentIdentity", "identity"), AgentInstanceRef: namespaced("AgentInstance", "instance"),
			TargetRef: namespaced("Tool", "tool"), AutonomyClassRef: k8sapi.RuntimeApprovalReference{Kind: "AutonomyClass", Scope: "Cluster", Name: "supervised", UID: "class-uid", ResourceVersion: 1},
			Binding:   k8sapi.RuntimeActionBinding{Format: "kognition-runtime-action-v1", Algorithm: "HMAC-SHA-256", Domain: "kognition.dev/runtime-approval/action/v1", KeyVersion: 1, Epoch: 1, Tag: base64.RawURLEncoding.EncodeToString(make([]byte, 32))},
			CreatedAt: metav1.NewTime(now), ExpiresAt: metav1.NewTime(now.Add(time.Hour)),
		},
	}
}
