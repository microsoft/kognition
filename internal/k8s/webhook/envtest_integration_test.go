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
	"bytes"
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
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	admissionv1 "k8s.io/api/admission/v1"
	admissionregv1 "k8s.io/api/admissionregistration/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	k8sapi "github.com/kognition-ai/kognition/api/k8s/v1alpha1"
	api "github.com/kognition-ai/kognition/api/v1alpha1"
	intadmission "github.com/kognition-ai/kognition/internal/admission"
	"github.com/kognition-ai/kognition/internal/auth"
	"github.com/kognition-ai/kognition/internal/convert"
	k8saudit "github.com/kognition-ai/kognition/internal/k8s/audit"
	nativecontroller "github.com/kognition-ai/kognition/internal/k8s/controller"
	"github.com/kognition-ai/kognition/internal/observability"
	"github.com/kognition-ai/kognition/internal/runtimeapproval"
	"github.com/kognition-ai/kognition/internal/store"
	"github.com/kognition-ai/kognition/internal/store/workflowactivationtest"
)

// envtestState holds resources shared across the integration tests.
type envtestState struct {
	testEnv    *envtest.Environment
	cfg        *rest.Config
	kube       kubernetes.Interface
	ctrlClient client.Client

	server      *httptest.Server
	caPEM       []byte
	mu          sync.Mutex
	mutators    []intadmission.MutatingHook
	validators  []intadmission.ValidatingHook
	submissions *WorkflowRunSubmissionService
	options     []Option
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
	service := envState.submissions
	options := append([]Option(nil), envState.options...)
	mode := modeCombined
	switch h.mode {
	case "mutate":
		mode = modeMutate
		mutators = append([]intadmission.MutatingHook{}, envState.mutators...)
	case "validate":
		mode = modeValidate
		validators = append([]intadmission.ValidatingHook{}, envState.validators...)
	}
	envState.mu.Unlock()

	chain := intadmission.NewChain(mutators, validators)
	options = append(options, WithWorkflowRunSubmissions(service))
	handler := newHandlerForMode(chain, testScheme(), testLogger(), mode, options...)
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
	sideDryRun = admissionregv1.SideEffectClassNoneOnDryRun
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
			Resources:   []string{"agents", "trustprofiles", "runtimeapprovals", "runtimeapprovals/status", "workflowruns"},
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
			SideEffects:             &sideDryRun,
			AdmissionReviewVersions: []string{"v1"},
			TimeoutSeconds:          &timeout,
		}},
	}
	vwh.Webhooks = append(vwh.Webhooks, admissionregv1.ValidatingWebhook{
		Name:         "workflow-records.kognition.dev",
		ClientConfig: admissionregv1.WebhookClientConfig{URL: &validateURL, CABundle: caBundle},
		Rules: []admissionregv1.RuleWithOperations{{
			Operations: []admissionregv1.OperationType{admissionregv1.Create, admissionregv1.Update, admissionregv1.Delete},
			Rule:       admissionregv1.Rule{APIGroups: []string{""}, APIVersions: []string{"v1"}, Resources: []string{"configmaps"}, Scope: &scopeAll},
		}},
		FailurePolicy: &failPolicy, MatchPolicy: &equiv, SideEffects: &sideNone,
		AdmissionReviewVersions: []string{"v1"}, TimeoutSeconds: &timeout,
	})

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
	state.testEnv.ControlPlane.GetAPIServer().Configure().Set("api-audiences", "kognition.dev/envtest-api")

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

func TestEnvtest_WorkflowActivationStoreContract(t *testing.T) {
	workflowactivationtest.Run(t, func(t *testing.T) (store.WorkflowActivationStore, store.WorkflowActivationStore) {
		t.Helper()
		namespace, err := envState.kube.CoreV1().Namespaces().Create(t.Context(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "workflow-gate-contract-"}}, metav1.CreateOptions{})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := envState.kube.CoreV1().ConfigMaps(namespace.Name).DeleteCollection(ctx, metav1.DeleteOptions{}, metav1.ListOptions{}); err != nil {
				t.Error(err)
			}
			if err := envState.kube.CoreV1().Namespaces().Delete(ctx, namespace.Name, metav1.DeleteOptions{}); err != nil {
				t.Error(err)
			}
		})
		secondClient, err := client.New(envState.cfg, client.Options{Scheme: scheme()})
		if err != nil {
			t.Fatal(err)
		}
		clock := func() (time.Time, error) { return time.Now().UTC(), nil }
		first, err := nativecontroller.NewWorkflowActivationGateStore(envState.ctrlClient, envState.ctrlClient, namespace.Name, clock)
		if err != nil {
			t.Fatal(err)
		}
		second, err := nativecontroller.NewWorkflowActivationGateStore(secondClient, secondClient, namespace.Name, clock)
		if err != nil {
			t.Fatal(err)
		}
		return first, second
	})
}

func scheme() *runtime.Scheme {
	s := runtime.NewScheme()
	utilruntime.Must(k8sapi.AddToScheme(s))
	utilruntime.Must(corev1.AddToScheme(s))
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

func TestEnvtest_WorkflowSubmissionRecordGuardRejectsUnqualifiedWrites(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	waitForWebhookReady(t, ctx)
	installHooks(nil, nil)
	immutable := true
	for _, name := range []string{
		runtimeapproval.NativeWorkflowSubmissionObjectName("team-a", "unqualified-run-uid"),
		runtimeapproval.WorkflowRunAcceptanceObjectName("team-a", "unqualified-run-uid"),
	} {
		record := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: name}, Immutable: &immutable, Data: map[string]string{"receipt.json": "{}"}}
		if err := envState.ctrlClient.Create(ctx, record); !apierrors.IsForbidden(err) {
			t.Fatalf("installed private-record admission accepted unqualified writer: %s %v", name, err)
		}
		if err := envState.ctrlClient.Get(ctx, client.ObjectKeyFromObject(record), &corev1.ConfigMap{}); !apierrors.IsNotFound(err) {
			t.Fatalf("denied private record was persisted: %s %v", name, err)
		}
	}
	ordinary := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "ordinary-workflow-guard-probe"}, Data: map[string]string{"value": "ordinary"}}
	if err := envState.ctrlClient.Create(ctx, ordinary); err != nil {
		t.Fatalf("private-record guard rejected ordinary ConfigMap: %v", err)
	}
	t.Cleanup(func() { _ = envState.ctrlClient.Delete(context.Background(), ordinary) })
}

func TestEnvtest_WorkflowBindingRequiresAuthenticatedOperator(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	waitForWebhookReady(t, ctx)
	installHooks(nil, nil)
	if _, err := envState.kube.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "team-a"}}, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatal(err)
	}
	definition := workflowBindingDefinitionFixture()
	definition.Name, definition.UID, definition.ResourceVersion = "installed-binding-operator", "", ""
	if err := envState.ctrlClient.Create(ctx, definition); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = envState.ctrlClient.Delete(context.Background(), definition) })
	operator, err := envState.testEnv.AddUser(envtest.User{Name: "installed-workflow-operator", Groups: []string{"workflow-operators"}}, envState.cfg)
	if err != nil {
		t.Fatal(err)
	}
	operatorClient, err := client.New(operator.Config(), client.Options{Scheme: scheme()})
	if err != nil {
		t.Fatal(err)
	}
	operatorRole, err := envState.kube.RbacV1().Roles("default").Create(ctx, &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{Name: "installed-binding-operator"},
		Rules:      []rbacv1.PolicyRule{{APIGroups: []string{""}, Resources: []string{"configmaps"}, Verbs: []string{"create", "get", "update", "delete"}}},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = envState.kube.RbacV1().Roles("default").Delete(context.Background(), operatorRole.Name, metav1.DeleteOptions{})
	})
	roleBinding, err := envState.kube.RbacV1().RoleBindings("default").Create(ctx, &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: operatorRole.Name},
		RoleRef:    rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "Role", Name: operatorRole.Name},
		Subjects:   []rbacv1.Subject{{Kind: "Group", APIGroup: "rbac.authorization.k8s.io", Name: "workflow-operators"}},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = envState.kube.RbacV1().RoleBindings("default").Delete(context.Background(), roleBinding.Name, metav1.DeleteOptions{})
	})
	executorSubject := "system:serviceaccount:default:installed-binding-executor"
	auditBackend, err := k8saudit.NewConfigMapStore(envState.ctrlClient, "default")
	if err != nil {
		t.Fatal(err)
	}
	envState.mu.Lock()
	envState.options = []Option{
		WithWorkflowBindingAdministrators("default", []string{"workflow-operators"}, []string{executorSubject}),
		WithWorkflowBindingReader(envState.ctrlClient),
		WithAuditLog(observability.NewCognitiveAuditLog(auditBackend)),
	}
	envState.mu.Unlock()
	t.Cleanup(func() {
		envState.mu.Lock()
		envState.options = nil
		envState.mu.Unlock()
	})
	record := workflowBindingConfigurationFixture(t)
	authority, err := runtimeapproval.DecodeWorkflowExecutorAuthority([]byte(record.Data["binding.json"]))
	if err != nil {
		t.Fatal(err)
	}
	authority.Reference.Name = "installed-operator"
	authority.Workflow.Name, authority.Workflow.UID = definition.Name, string(definition.UID)
	if _, err := fmt.Sscan(definition.ResourceVersion, &authority.Workflow.ResourceVersion); err != nil {
		t.Fatal(err)
	}
	definitionJSON, err := json.Marshal(convert.WorkflowFromK8s(definition))
	if err != nil {
		t.Fatal(err)
	}
	authority.WorkflowRevisionDigest, err = runtimeapproval.WorkflowExecutorSpecDigest(definitionJSON)
	if err != nil {
		t.Fatal(err)
	}
	authority.Executor.Subject, authority.ApprovedBy.Subject = executorSubject, "installed-workflow-operator"
	record.Namespace, record.Name = "default", runtimeapproval.WorkflowExecutorBindingObjectName("team-a", authority.Reference.Name)
	encode := func(destination *corev1.ConfigMap, value runtimeapproval.WorkflowExecutorAuthority) {
		t.Helper()
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		destination.Data = map[string]string{"binding.json": string(raw)}
	}
	encode(&record, authority)
	forged := record.DeepCopy()
	forgedAuthority := authority
	forgedAuthority.ApprovedBy.Subject = "forged-operator"
	encode(forged, forgedAuthority)
	if err := operatorClient.Create(ctx, forged); !apierrors.IsBadRequest(err) {
		t.Fatalf("installed binding accepted forged approving operator: %v", err)
	}
	if err := operatorClient.Create(ctx, &record); err != nil {
		t.Fatalf("authenticated operator could not create installed binding: %v", err)
	}
	if record.UID == "" || record.ResourceVersion == "" {
		t.Fatal("installed binding has no server-owned identity")
	}
	executor, err := envState.testEnv.AddUser(envtest.User{Name: executorSubject, Groups: []string{"workflow-operators"}}, envState.cfg)
	if err != nil {
		t.Fatal(err)
	}
	executorClient, err := client.New(executor.Config(), client.Options{Scheme: scheme()})
	if err != nil {
		t.Fatal(err)
	}
	selfApproved := record.DeepCopy()
	selfAuthority := authority
	selfAuthority.ApprovedBy.Subject = executorSubject
	selfAuthority.Enabled = false
	encode(selfApproved, selfAuthority)
	if err := executorClient.Update(ctx, selfApproved); !apierrors.IsForbidden(err) {
		t.Fatalf("executor in operator group administered its own binding: %v", err)
	}
	if err := operatorClient.Delete(ctx, &record); !apierrors.IsForbidden(err) {
		t.Fatalf("operator physically deleted protected binding: %v", err)
	}
	stale := record.DeepCopy()
	staleAuthority := authority
	staleAuthority.Workflow.ResourceVersion++
	encode(stale, staleAuthority)
	if err := operatorClient.Update(ctx, stale); !apierrors.IsConflict(err) {
		t.Fatalf("operator substituted binding Workflow revision: %v", err)
	}
	if err := envState.ctrlClient.Delete(ctx, definition); err != nil {
		t.Fatal(err)
	}
	authority.Enabled = false
	encode(&record, authority)
	if err := operatorClient.Update(ctx, &record); err != nil {
		t.Fatalf("operator could not revoke binding after source deletion: %v", err)
	}
	persisted := &corev1.ConfigMap{}
	if err := operatorClient.Get(ctx, client.ObjectKeyFromObject(&record), persisted); err != nil {
		t.Fatal(err)
	}
	retained, err := runtimeapproval.DecodeWorkflowExecutorAuthority([]byte(persisted.Data["binding.json"]))
	if err != nil || retained.Enabled || retained.ApprovedBy.Subject != "installed-workflow-operator" || persisted.UID != record.UID {
		t.Fatalf("revocation lost binding identity or operator attribution: %+v %v", retained, err)
	}
	entries, err := auditBackend.Query(ctx, observability.AuditFilter{Workspace: "default", Limit: 100})
	if err != nil {
		t.Fatalf("read installed operator audit: %v", err)
	}
	verbs := make(map[string]int)
	for _, entry := range entries {
		if entry.SubjectRef != "default/"+record.Name {
			continue
		}
		if entry.AgentIdentity != "installed-workflow-operator" || entry.SubjectKind != "ConfigMap" {
			t.Fatalf("installed audit attributed binding administration to another actor: %+v", entry)
		}
		verbs[entry.Verb]++
	}
	if len(verbs) != 2 || verbs["attempt_create"] != 1 || verbs["attempt_update"] != 1 {
		t.Fatalf("installed audit includes denied writes or omits operator writes: %v", verbs)
	}
}

func TestEnvtest_WorkflowRunAdmissionBindsActualResourceUID(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	waitForWebhookReady(t, ctx)
	service, _, bindings, _ := workflowSubmissionFixture(t)
	account, err := envState.kube.CoreV1().ServiceAccounts("default").Create(ctx,
		&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "native-receipt-executor"}}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = envState.kube.CoreV1().ServiceAccounts("default").Delete(context.Background(), account.Name, metav1.DeleteOptions{})
	})
	pod, err := envState.kube.CoreV1().Pods("default").Create(ctx, &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "native-receipt-executor"},
		Spec:       corev1.PodSpec{ServiceAccountName: account.Name, Containers: []corev1.Container{{Name: "executor", Image: "registry.k8s.io/pause:3.9"}}},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = envState.kube.CoreV1().Pods("default").Delete(context.Background(), pod.Name, metav1.DeleteOptions{})
	})
	pod.Status.Phase = corev1.PodRunning
	pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	pod, err = envState.kube.CoreV1().Pods("default").UpdateStatus(ctx, pod, metav1.UpdateOptions{})
	if err != nil || pod.UID == "" || account.UID == "" || pod.UID == account.UID {
		t.Fatalf("persist distinct bound workload identities: %v", err)
	}
	role, err := envState.kube.RbacV1().Roles("default").Create(ctx, &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{Name: "native-receipt-executor"},
		Rules:      []rbacv1.PolicyRule{{APIGroups: []string{""}, Resources: []string{"configmaps"}, Verbs: []string{"create", "get"}}},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = envState.kube.RbacV1().Roles("default").Delete(context.Background(), role.Name, metav1.DeleteOptions{})
	})
	roleBinding, err := envState.kube.RbacV1().RoleBindings("default").Create(ctx, &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: role.Name}, RoleRef: rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "Role", Name: role.Name},
		Subjects: []rbacv1.Subject{{Kind: "ServiceAccount", Namespace: "default", Name: account.Name}},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = envState.kube.RbacV1().RoleBindings("default").Delete(context.Background(), roleBinding.Name, metav1.DeleteOptions{})
	})
	expiration := int64(600)
	mintToken := func(audience string, bound bool) string {
		t.Helper()
		request := &authenticationv1.TokenRequest{Spec: authenticationv1.TokenRequestSpec{Audiences: []string{audience}, ExpirationSeconds: &expiration}}
		if bound {
			request.Spec.BoundObjectRef = &authenticationv1.BoundObjectReference{APIVersion: "v1", Kind: "Pod", Name: pod.Name, UID: pod.UID}
		}
		response, err := envState.kube.CoreV1().ServiceAccounts("default").CreateToken(ctx, account.Name, request, metav1.CreateOptions{})
		if err != nil || response.Status.Token == "" {
			t.Fatalf("issue bound workload credential: %v", err)
		}
		return response.Status.Token
	}
	credential := mintToken(nativecontroller.WorkflowExecutorKubernetesAudience, true)
	apiToken := mintToken("kognition.dev/envtest-api", true)
	executorConfig := rest.AnonymousClientConfig(envState.cfg)
	executorConfig.BearerToken = apiToken
	executorClient, err := client.New(executorConfig, client.Options{Scheme: scheme()})
	if err != nil {
		t.Fatal(err)
	}
	profile, err := json.Marshal(map[string]any{
		"mode": "kubernetes",
		"k8sController": map[string]any{"replicas": 1, "serviceAccount": map[string]any{"name": account.Name},
			"workflowExecutor": map[string]any{"enabled": true, "bindingAdminGroups": []string{"workflow-operators"},
				"principals": []api.ControlPlaneRoleSubject{{Method: "tokenreview", Issuer: "kubernetes.io/tokenreview", Subject: "system:serviceaccount:default:" + account.Name}}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	chart := filepath.Join("..", "..", "..", "deploy", "helm", "kognition")
	render := exec.CommandContext(ctx, "helm", "template", "receipt-policy", chart, "--namespace", "default", "--kube-version", "1.30.3",
		"--values", "-", "--show-only", "templates/controller-state-admission.yaml", "--show-only", "templates/validating-webhook-configuration.yaml")
	render.Stdin = bytes.NewReader(profile)
	manifest, err := render.CombinedOutput()
	if err != nil {
		t.Fatalf("render installed Workflow state policy: %v\n%s", err, manifest)
	}
	decoder := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(manifest), 4096)
	installedPolicy, installedBinding, installedGuard := false, false, false
	for {
		var document json.RawMessage
		if err := decoder.Decode(&document); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		var metadata struct {
			metav1.TypeMeta `json:",inline"`
			Metadata        metav1.ObjectMeta `json:"metadata"`
		}
		if err := json.Unmarshal(document, &metadata); err != nil {
			t.Fatal(err)
		}
		if metadata.Metadata.Name != "receipt-policy-kognition-default-state" && metadata.Kind != "ValidatingWebhookConfiguration" {
			continue
		}
		switch metadata.Kind {
		case "ValidatingWebhookConfiguration":
			rendered := &admissionregv1.ValidatingWebhookConfiguration{}
			if err := json.Unmarshal(document, rendered); err != nil {
				t.Fatal(err)
			}
			for _, webhook := range rendered.Webhooks {
				if webhook.Name != "workflow-records.kognition.dev" {
					continue
				}
				if len(webhook.MatchConditions) != 1 {
					t.Fatal("rendered private guard must scope protected execution records")
				}
				configuration, err := envState.kube.AdmissionregistrationV1().ValidatingWebhookConfigurations().Get(ctx, "kognition-validate-envtest", metav1.GetOptions{})
				if err != nil {
					t.Fatal(err)
				}
				for index := range configuration.Webhooks {
					if configuration.Webhooks[index].Name == webhook.Name {
						configuration.Webhooks[index].MatchConditions = webhook.MatchConditions
						installedGuard = true
					}
				}
				if _, err := envState.kube.AdmissionregistrationV1().ValidatingWebhookConfigurations().Update(ctx, configuration, metav1.UpdateOptions{}); err != nil {
					t.Fatalf("install rendered private guard scope: %v", err)
				}
				t.Cleanup(func() {
					configuration, err := envState.kube.AdmissionregistrationV1().ValidatingWebhookConfigurations().Get(context.Background(), "kognition-validate-envtest", metav1.GetOptions{})
					if err != nil {
						t.Error(err)
						return
					}
					for index := range configuration.Webhooks {
						if configuration.Webhooks[index].Name == "workflow-records.kognition.dev" {
							configuration.Webhooks[index].MatchConditions = nil
						}
					}
					if _, err := envState.kube.AdmissionregistrationV1().ValidatingWebhookConfigurations().Update(context.Background(), configuration, metav1.UpdateOptions{}); err != nil {
						t.Error(err)
					}
				})
			}
		case "ValidatingAdmissionPolicy":
			policy := &admissionregv1.ValidatingAdmissionPolicy{}
			if err := json.Unmarshal(document, policy); err != nil {
				t.Fatal(err)
			}
			if _, err := envState.kube.AdmissionregistrationV1().ValidatingAdmissionPolicies().Create(ctx, policy, metav1.CreateOptions{}); err != nil {
				t.Fatalf("install rendered Workflow state policy: %v", err)
			}
			installedPolicy = true
			t.Cleanup(func() {
				_ = envState.kube.AdmissionregistrationV1().ValidatingAdmissionPolicies().Delete(context.Background(), policy.Name, metav1.DeleteOptions{})
			})
		case "ValidatingAdmissionPolicyBinding":
			binding := &admissionregv1.ValidatingAdmissionPolicyBinding{}
			if err := json.Unmarshal(document, binding); err != nil {
				t.Fatal(err)
			}
			if _, err := envState.kube.AdmissionregistrationV1().ValidatingAdmissionPolicyBindings().Create(ctx, binding, metav1.CreateOptions{}); err != nil {
				t.Fatalf("install rendered Workflow state policy binding: %v", err)
			}
			installedBinding = true
			t.Cleanup(func() {
				_ = envState.kube.AdmissionregistrationV1().ValidatingAdmissionPolicyBindings().Delete(context.Background(), binding.Name, metav1.DeleteOptions{})
			})
		}
	}
	if !installedPolicy || !installedBinding || !installedGuard {
		t.Fatal("rendered Workflow state policy, binding, or private guard missing")
	}
	configuration, err := envState.kube.AdmissionregistrationV1().ValidatingWebhookConfigurations().Get(ctx, "kognition-validate-envtest", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var healthyGuard admissionregv1.WebhookClientConfig
	for index := range configuration.Webhooks {
		if configuration.Webhooks[index].Name == "workflow-records.kognition.dev" {
			healthyGuard = *configuration.Webhooks[index].ClientConfig.DeepCopy()
			unavailableURL := "https://127.0.0.1:1/validate"
			configuration.Webhooks[index].ClientConfig.URL = &unavailableURL
		}
	}
	restoreGuard := func() error {
		configuration, err := envState.kube.AdmissionregistrationV1().ValidatingWebhookConfigurations().Get(context.Background(), "kognition-validate-envtest", metav1.GetOptions{})
		if err != nil {
			return err
		}
		for index := range configuration.Webhooks {
			if configuration.Webhooks[index].Name == "workflow-records.kognition.dev" {
				configuration.Webhooks[index].ClientConfig = healthyGuard
			}
		}
		_, err = envState.kube.AdmissionregistrationV1().ValidatingWebhookConfigurations().Update(context.Background(), configuration, metav1.UpdateOptions{})
		return err
	}
	t.Cleanup(func() {
		if err := restoreGuard(); err != nil {
			t.Error(err)
		}
	})
	if _, err := envState.kube.AdmissionregistrationV1().ValidatingWebhookConfigurations().Update(ctx, configuration, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := wait.PollUntilContextTimeout(ctx, 100*time.Millisecond, 10*time.Second, true, func(ctx context.Context) (bool, error) {
		probe := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "kognition-workflow-run-submission-unavailable", Namespace: "default"}}
		err := envState.ctrlClient.Create(ctx, probe)
		return apierrors.IsInternalError(err) && strings.Contains(err.Error(), "workflow-records.kognition.dev"), nil
	}); err != nil {
		t.Fatalf("protected name bypassed unavailable executor admission: %v", err)
	}
	ordinary := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "workflow-ordinary-bootstrap", Namespace: "default"}, Data: map[string]string{"value": "bootstrap"}}
	if err := envState.ctrlClient.Create(ctx, ordinary); err != nil {
		t.Fatalf("ordinary initialization depends on unavailable executor admission: %v", err)
	}
	t.Cleanup(func() { _ = envState.ctrlClient.Delete(context.Background(), ordinary) })
	for _, probe := range []*corev1.ConfigMap{
		{ObjectMeta: metav1.ObjectMeta{Name: "workflow-labelled-private", Namespace: "default", Labels: map[string]string{"kognition.dev/internal-record": "workflow-run-acceptance"}}},
		{ObjectMeta: metav1.ObjectMeta{GenerateName: "kognition-workflow-run-acceptance-", Namespace: "default"}},
	} {
		if err := envState.ctrlClient.Create(ctx, probe); !apierrors.IsInternalError(err) || !strings.Contains(err.Error(), "workflow-records.kognition.dev") {
			t.Fatalf("protected metadata bypassed unavailable executor admission: %v", err)
		}
	}
	if err := restoreGuard(); err != nil {
		t.Fatal(err)
	}
	if err := wait.PollUntilContextTimeout(ctx, 100*time.Millisecond, 10*time.Second, true, func(ctx context.Context) (bool, error) {
		probe := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "workflow-unowned-state-probe", Namespace: "default"}, Data: map[string]string{"value": "unowned"}}
		err := executorClient.Create(ctx, probe)
		if apierrors.IsForbidden(err) {
			return true, nil
		}
		if err != nil {
			return false, err
		}
		return false, envState.ctrlClient.Delete(ctx, probe)
	}); err != nil {
		t.Fatalf("installed Workflow state policy did not reject unrelated executor ConfigMaps: %v", err)
	}
	identity := nativecontroller.NewStoreAdapter(executorClient)
	subject := "system:serviceaccount:default:" + account.Name
	if err := identity.ConfigureWorkflowExecutorAuthorization(nativecontroller.WorkflowExecutorKubernetesAuthorization{
		TokenReviews: envState.kube.AuthenticationV1().TokenReviews(), AccessReviews: envState.kube.AuthorizationV1().SubjectAccessReviews(),
		Credential: func(context.Context) (string, error) { return credential, nil }, Subject: subject,
		ServiceAccountUID: string(account.UID), WorkloadUID: string(pod.UID), PodNamespace: "default", PodName: pod.Name, Pods: envState.ctrlClient,
	}); err != nil {
		t.Fatal(err)
	}
	user, err := identity.AuthenticatedWorkflowExecutorUser(ctx)
	if err != nil || user.Username != subject || user.UID != string(account.UID) || len(user.Extra["authentication.kubernetes.io/pod-uid"]) != 1 || user.Extra["authentication.kubernetes.io/pod-uid"][0] != string(pod.UID) {
		t.Fatalf("actual bound TokenReview did not identify the approved workload: %v", err)
	}
	bindings.authority.Executor = api.ControlPlaneRoleSubject{Method: "tokenreview", Issuer: "kubernetes.io/tokenreview", Subject: subject}
	bindings.authority.ControllerUIDs = []string{string(pod.UID)}
	service.RecordNamespace, service.CurrentWriter = "default", identity.AuthenticatedWorkflowExecutorUser
	if _, err := envState.kube.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "team-a"}}, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatal(err)
	}
	definition := workflowBindingDefinitionFixture()
	definition.UID, definition.ResourceVersion = "", ""
	if err := envState.ctrlClient.Create(ctx, definition); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = envState.ctrlClient.Delete(context.Background(), definition) })
	workflowJSON, err := json.Marshal(convert.WorkflowFromK8s(definition))
	if err != nil {
		t.Fatal(err)
	}
	bindings.authority.Workflow.UID = string(definition.UID)
	_, err = fmt.Sscan(definition.ResourceVersion, &bindings.authority.Workflow.ResourceVersion)
	if err != nil {
		t.Fatal(err)
	}
	bindings.authority.WorkflowRevisionDigest, err = runtimeapproval.WorkflowExecutorSpecDigest(workflowJSON)
	if err != nil {
		t.Fatal(err)
	}
	service.Authorizer = &WorkflowUseAdmissionAuthorizer{Reader: envState.ctrlClient, AccessReviews: envState.kube.AuthorizationV1().SubjectAccessReviews()}
	records, err := nativecontroller.NewWorkflowExecutorRecordStore(executorClient, envState.ctrlClient, "default", service.Clock)
	if err != nil {
		t.Fatal(err)
	}
	service.Receipts = records
	envState.mu.Lock()
	envState.submissions = service
	envState.mu.Unlock()
	t.Cleanup(func() {
		envState.mu.Lock()
		envState.submissions = nil
		envState.mu.Unlock()
	})
	var captureMu sync.Mutex
	var mutatingUID, validatingUID string
	var captureErr error
	capture := func(request *intadmission.Request, mutation bool) *intadmission.Response {
		var run api.WorkflowRun
		captureMu.Lock()
		defer captureMu.Unlock()
		if err := json.Unmarshal(request.Object, &run); err != nil {
			captureErr = err
			return &intadmission.Response{Allowed: false, Reason: "invalid native run"}
		}
		if mutation {
			mutatingUID = run.UID
		} else {
			validatingUID = run.UID
		}
		return &intadmission.Response{Allowed: true}
	}
	installHooks([]intadmission.MutatingHook{&stubMutator{name: "capture-run-identity", kinds: map[string]bool{"WorkflowRun": true},
		mutateFn: func(_ context.Context, request *intadmission.Request) *intadmission.Response {
			return capture(request, true)
		}}},
		[]intadmission.ValidatingHook{&stubValidator{name: "verify-run-identity", kinds: map[string]bool{"WorkflowRun": true},
			validateFn: func(_ context.Context, request *intadmission.Request) *intadmission.Response {
				return capture(request, false)
			}}})
	t.Cleanup(func() { installHooks(nil, nil) })
	bindingJSON, err := json.Marshal(bindings.authority.Reference)
	if err != nil {
		t.Fatal(err)
	}
	run := &k8sapi.WorkflowRun{ObjectMeta: metav1.ObjectMeta{Name: "native-receipt-resource-uid", Namespace: "team-a", UID: "caller-selected-uid",
		Annotations: map[string]string{runtimeapproval.WorkflowRunAcceptanceRequiredAnnotation: "true", runtimeapproval.NativeWorkflowExecutionBindingAnnotation: string(bindingJSON)}},
		Spec: k8sapi.WorkflowRunSpec{WorkflowRef: k8sapi.LocalObjectReference{Name: "investigate"}}}
	beforeDryRun := &corev1.ConfigMapList{}
	if err := envState.ctrlClient.List(ctx, beforeDryRun, client.InNamespace("default")); err != nil {
		t.Fatal(err)
	}
	dryRun := run.DeepCopy()
	dryRun.Name += "-dry-run"
	if err := envState.ctrlClient.Create(ctx, dryRun, client.DryRunAll); !apierrors.IsForbidden(err) {
		t.Fatalf("installed admission accepted protected dry-run submission: %v", err)
	}
	afterDryRun := &corev1.ConfigMapList{}
	if err := envState.ctrlClient.List(ctx, afterDryRun, client.InNamespace("default")); err != nil || len(afterDryRun.Items) != len(beforeDryRun.Items) {
		t.Fatalf("denied dry-run submission wrote private evidence: %v", err)
	}
	if err := envState.ctrlClient.Create(ctx, run); err != nil {
		t.Fatalf("create native run through installed admission: %v", err)
	}
	t.Cleanup(func() { _ = envState.ctrlClient.Delete(context.Background(), run) })
	fetched := &k8sapi.WorkflowRun{}
	if err := envState.ctrlClient.Get(ctx, client.ObjectKeyFromObject(run), fetched); err != nil {
		t.Fatal(err)
	}
	captureMu.Lock()
	defer captureMu.Unlock()
	if captureErr != nil || mutatingUID != "" || validatingUID == "" || validatingUID == "caller-selected-uid" || string(fetched.UID) != validatingUID {
		t.Fatalf("receipt identity assumption failed: mutation=%q validation=%q persisted=%q error=%v", mutatingUID, validatingUID, fetched.UID, captureErr)
	}
	receipt, err := service.Receipts.GetNativeWorkflowSubmissionReceipt(ctx, fetched.Namespace, string(fetched.UID))
	if err != nil || receipt.Run.UID != string(fetched.UID) || receipt.AdmissionUID == string(fetched.UID) || receipt.Submitter.Username == "" || receipt.Submitter.Username == "actual-submitter" || fetched.Annotations[runtimeapproval.NativeWorkflowSubmissionAnnotation] != "" {
		t.Fatalf("installed validation did not retain actual private submission evidence: %+v %v", receipt, err)
	}
	runJSON, err := json.Marshal(convert.WorkflowRunFromK8s(fetched))
	if err != nil {
		t.Fatal(err)
	}
	acceptance, err := service.AuthorizeAcceptance(ctx, runJSON)
	if err != nil || acceptance.RunUID != string(fetched.UID) || acceptance.Initiator.Subject != receipt.Submitter.Username {
		t.Fatalf("installed receipt could not authorize exact persisted run: %+v %v", acceptance, err)
	}
	acceptanceJSON, err := json.Marshal(acceptance)
	if err != nil {
		t.Fatal(err)
	}
	immutable := true
	accepted := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: "default",
		Name: runtimeapproval.WorkflowRunAcceptanceObjectName(fetched.Namespace, string(fetched.UID)), Labels: map[string]string{"kognition.dev/internal-record": "workflow-run-acceptance"}},
		Immutable: &immutable, Data: map[string]string{"acceptance.json": string(acceptanceJSON)}}
	if err := executorClient.Create(ctx, accepted); err != nil {
		t.Fatalf("installed exact acceptance by bound executor denied: %v", err)
	}
	if retained, err := records.GetWorkflowRunAcceptance(ctx, fetched.Namespace, string(fetched.UID)); err != nil || retained.RunUID != acceptance.RunUID || retained.Initiator != acceptance.Initiator {
		t.Fatalf("installed acceptance was not retained: %v", err)
	}
	privateReceipt := &corev1.ConfigMap{}
	if err := envState.ctrlClient.Get(ctx, client.ObjectKey{Namespace: "default", Name: runtimeapproval.NativeWorkflowSubmissionObjectName(fetched.Namespace, string(fetched.UID))}, privateReceipt); err != nil {
		t.Fatal(err)
	}
	for _, record := range []*corev1.ConfigMap{privateReceipt, accepted} {
		copy := record.DeepCopy()
		copy.UID, copy.ResourceVersion = "", ""
		if err := envState.ctrlClient.Create(ctx, copy); !apierrors.IsForbidden(err) {
			t.Fatalf("privileged operator bypassed installed executor-only CREATE: %v", err)
		}
		copy = record.DeepCopy()
		copy.Annotations = map[string]string{"attempted-change": "true"}
		if err := envState.ctrlClient.Update(ctx, copy); !apierrors.IsForbidden(err) {
			t.Fatalf("privileged operator changed immutable execution evidence: %v", err)
		}
		if err := envState.ctrlClient.Delete(ctx, record); !apierrors.IsForbidden(err) {
			t.Fatalf("privileged operator deleted immutable execution evidence: %v", err)
		}
	}
	approvedCredential := credential
	for _, wrongCredential := range []string{apiToken, mintToken(nativecontroller.WorkflowExecutorKubernetesAudience, false)} {
		credential = wrongCredential
		if _, err := identity.AuthenticatedWorkflowExecutorUser(ctx); err == nil {
			t.Fatal("actual TokenReview accepted wrong audience or unbound service-account credential")
		}
		copy := privateReceipt.DeepCopy()
		copy.UID, copy.ResourceVersion = "", ""
		if err := executorClient.Create(ctx, copy); !apierrors.IsForbidden(err) {
			t.Fatalf("installed private admission accepted unqualified bound-writer proof: %v", err)
		}
	}
	credential = approvedCredential
	pod.Status.Conditions[0].Status = corev1.ConditionFalse
	if _, err := envState.kube.CoreV1().Pods("default").UpdateStatus(ctx, pod, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	copy := privateReceipt.DeepCopy()
	copy.UID, copy.ResourceVersion = "", ""
	if err := executorClient.Create(ctx, copy); !apierrors.IsForbidden(err) {
		t.Fatalf("installed private admission accepted an unready bound workload: %v", err)
	}
	pod, err = envState.kube.CoreV1().Pods("default").Get(ctx, pod.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	pod.Status.Conditions[0].Status = corev1.ConditionTrue
	if _, err := envState.kube.CoreV1().Pods("default").UpdateStatus(ctx, pod, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	testInstalledWorkflowAcceptanceRevocation(t, service, executorClient, identity, bindings.authority)
	testInstalledStandaloneWorkflowAttestation(t, pod.Name, account.Name, credential, apiToken, mintToken)
}

type installedWorkflowOIDCAuthenticator struct {
	verifier *oidc.IDTokenVerifier
}

func (authenticator installedWorkflowOIDCAuthenticator) Authenticate(ctx context.Context, token string) (*auth.Identity, error) {
	verified, err := authenticator.verifier.Verify(ctx, token)
	if err != nil {
		return nil, err
	}
	return &auth.Identity{Method: "oidc", Issuer: verified.Issuer, Subject: verified.Subject}, nil
}

func testInstalledStandaloneWorkflowAttestation(t *testing.T, podName, accountName, credential, apiToken string, mintToken func(string, bool) string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	discovery, err := envState.kube.CoreV1().RESTClient().Get().AbsPath("/.well-known/openid-configuration").Do(ctx).Raw()
	if err != nil {
		t.Fatalf("read actual Kubernetes OIDC issuer: %v", err)
	}
	var provider struct {
		Issuer     string   `json:"issuer"`
		Algorithms []string `json:"id_token_signing_alg_values_supported"`
		KeysURI    string   `json:"jwks_uri"`
	}
	if err := json.Unmarshal(discovery, &provider); err != nil || provider.Issuer == "" || len(provider.Algorithms) == 0 {
		t.Fatalf("invalid actual OIDC discovery: %v", err)
	}
	httpClient, err := rest.HTTPClientFor(envState.cfg)
	if err != nil {
		t.Fatal(err)
	}
	keysURI, err := url.Parse(provider.KeysURI)
	if err != nil || keysURI.Scheme != "https" || keysURI.Host == "" || keysURI.User != nil || keysURI.Fragment != "" {
		t.Fatalf("invalid installed JWKS discovery: %v", err)
	}
	if _, err := envState.kube.CoreV1().RESTClient().Get().AbsPath(keysURI.Path).Do(ctx).Raw(); err != nil {
		t.Fatalf("actual published JWKS path %q is unavailable through the API REST client: %v", keysURI.Path, err)
	}
	apiURL, err := url.Parse(envState.cfg.Host)
	if err != nil {
		t.Fatal(err)
	}
	keysURL := apiURL.ResolveReference(&url.URL{Path: keysURI.Path, RawPath: keysURI.RawPath, RawQuery: keysURI.RawQuery})
	keys := oidc.NewRemoteKeySet(oidc.ClientContext(ctx, httpClient), keysURL.String())
	base := installedWorkflowOIDCAuthenticator{verifier: oidc.NewVerifier(provider.Issuer, keys, &oidc.Config{ClientID: auth.WorkflowExecutorTokenAudience, SupportedSigningAlgs: provider.Algorithms})}
	principal := api.ControlPlaneRoleSubject{Method: "oidc", Issuer: provider.Issuer, Subject: "system:serviceaccount:default:" + accountName}
	authenticator, err := auth.NewWorkflowExecutorAuthenticator(base, []api.ControlPlaneRoleSubject{principal}, envState.kube.AuthenticationV1().TokenReviews(), envState.ctrlClient)
	if err != nil {
		t.Fatal(err)
	}
	identity, err := authenticator.Authenticate(ctx, credential)
	if err != nil {
		t.Fatalf("actual signed OIDC and bound-token attestation failed: %v", err)
	}
	pod, err := envState.kube.CoreV1().Pods("default").Get(ctx, podName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	account, err := envState.kube.CoreV1().ServiceAccounts("default").Get(ctx, accountName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	proofContext := auth.WithIdentity(ctx, identity)
	uid, err := auth.AuthenticatedWorkflowControllerUID(proofContext)
	if err != nil || uid != string(pod.UID) || uid == string(account.UID) || identity.Subject != principal.Subject {
		t.Fatalf("standalone proof lost actual principal or distinct controller UID: %q %v", uid, err)
	}
	interceptor := auth.WorkflowExecutorGRPCUnaryInterceptor(authenticator)
	checkGRPC := func(token string, allowed bool) {
		t.Helper()
		for _, method := range []string{"/lf.a2a.v1.A2AService/SendMessage", "/lf.a2a.v1.A2AService/GetTask"} {
			incoming := metadata.NewIncomingContext(ctx, metadata.Pairs("authorization", "Bearer "+token))
			calls := 0
			result, err := interceptor(incoming, "bound-request", &grpc.UnaryServerInfo{FullMethod: method}, func(admitted context.Context, request any) (any, error) {
				calls++
				actual, present := auth.IdentityFromContext(admitted)
				controller, err := auth.AuthenticatedWorkflowControllerUID(admitted)
				if !present || actual.Subject != principal.Subject || actual.Issuer != principal.Issuer || controller != string(pod.UID) || controller == string(account.UID) || err != nil || request != "bound-request" {
					t.Fatalf("gRPC admission lost signed installed identity: identity=%+v controller=%q err=%v", actual, controller, err)
				}
				return "admitted", nil
			})
			if allowed {
				if err != nil || calls != 1 || result != "admitted" {
					t.Fatalf("signed installed gRPC admission failed for %s: calls=%d err=%v", method, calls, err)
				}
			} else if status.Code(err) != codes.Unauthenticated || calls != 0 || result != nil {
				t.Fatalf("unqualified installed token reached %s: calls=%d err=%v", method, calls, err)
			}
		}
	}
	checkGRPC(credential, true)
	for _, token := range []string{apiToken, mintToken(auth.WorkflowExecutorTokenAudience, false)} {
		if _, err := authenticator.Authenticate(ctx, token); err == nil {
			t.Fatal("standalone attestation accepted wrong-audience or unbound actual token")
		}
		checkGRPC(token, false)
	}
	duration := int64(1200)
	rotated, err := envState.kube.CoreV1().ServiceAccounts("default").CreateToken(ctx, accountName, &authenticationv1.TokenRequest{Spec: authenticationv1.TokenRequestSpec{
		Audiences: []string{auth.WorkflowExecutorTokenAudience}, ExpirationSeconds: &duration,
		BoundObjectRef: &authenticationv1.BoundObjectReference{APIVersion: "v1", Kind: "Pod", Name: pod.Name, UID: pod.UID},
	}}, metav1.CreateOptions{})
	if err != nil || rotated.Status.Token == "" || rotated.Status.Token == credential {
		t.Fatalf("issue independently rotated actual credential: %v", err)
	}
	if _, err := authenticator.Authenticate(ctx, rotated.Status.Token); err != nil {
		t.Fatalf("same live incarnation rejected rotated signed credential: %v", err)
	}
	checkGRPC(rotated.Status.Token, true)
	pod.Status.Conditions[0].Status = corev1.ConditionFalse
	if _, err := envState.kube.CoreV1().Pods("default").UpdateStatus(ctx, pod, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	defer func() {
		current, err := envState.kube.CoreV1().Pods("default").Get(context.Background(), podName, metav1.GetOptions{})
		if err != nil {
			t.Error(err)
			return
		}
		current.Status.Conditions[0].Status = corev1.ConditionTrue
		if _, err := envState.kube.CoreV1().Pods("default").UpdateStatus(context.Background(), current, metav1.UpdateOptions{}); err != nil {
			t.Error(err)
		}
	}()
	if _, err := auth.AuthenticatedWorkflowControllerUID(proofContext); err == nil {
		t.Fatal("retained standalone OIDC identity bypassed live Pod readiness")
	}
	checkGRPC(credential, false)
	checkGRPC(rotated.Status.Token, false)
	canceled, stop := context.WithCancel(proofContext)
	stop()
	if _, err := auth.AuthenticatedWorkflowControllerUID(canceled); err == nil {
		t.Fatal("canceled standalone proof remained eligible")
	}
}

type pausedWorkflowAcceptanceClient struct {
	client.Client
	entered chan struct{}
	release chan struct{}
}

func (writer *pausedWorkflowAcceptanceClient) Create(ctx context.Context, object client.Object, options ...client.CreateOption) error {
	if object.GetLabels()["kognition.dev/internal-record"] == "workflow-run-acceptance" {
		close(writer.entered)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-writer.release:
		}
	}
	return writer.Client.Create(ctx, object, options...)
}

func testInstalledWorkflowAcceptanceRevocation(t *testing.T, service *WorkflowRunSubmissionService, executorClient client.Client, identity *nativecontroller.StoreAdapter, base runtimeapproval.WorkflowExecutorAuthority) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	installHooks(nil, nil)
	operatorName, submitterName := "installed-revocation-operator", "installed-revocation-submitter"
	operator, err := envState.testEnv.AddUser(envtest.User{Name: operatorName, Groups: []string{"workflow-operators"}}, envState.cfg)
	if err != nil {
		t.Fatal(err)
	}
	operatorClient, err := client.New(operator.Config(), client.Options{Scheme: scheme()})
	if err != nil {
		t.Fatal(err)
	}
	submitter, err := envState.testEnv.AddUser(envtest.User{Name: submitterName}, envState.cfg)
	if err != nil {
		t.Fatal(err)
	}
	submitterClient, err := client.New(submitter.Config(), client.Options{Scheme: scheme()})
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name            string
		binding         bool
		acceptanceFirst bool
	}{
		{name: "binding-revocation-first", binding: true},
		{name: "binding-acceptance-first", binding: true, acceptanceFirst: true},
		{name: "source-use-revocation-first"},
		{name: "source-use-acceptance-first", acceptanceFirst: true},
	}
	names := make([]string, len(cases))
	for index, test := range cases {
		names[index] = "installed-" + test.name
	}
	for _, permission := range []struct {
		namespace string
		name      string
		user      string
		rules     []rbacv1.PolicyRule
	}{
		{namespace: "default", name: operatorName, user: operatorName,
			rules: []rbacv1.PolicyRule{{APIGroups: []string{""}, Resources: []string{"configmaps"}, Verbs: []string{"create", "get", "update"}}}},
		{namespace: "team-a", name: submitterName, user: submitterName,
			rules: []rbacv1.PolicyRule{
				{APIGroups: []string{"kognition.dev"}, Resources: []string{"workflowruns"}, Verbs: []string{"create", "get"}},
				{APIGroups: []string{"kognition.dev"}, Resources: []string{"workflows"}, ResourceNames: names, Verbs: []string{"use"}},
			}},
	} {
		role, err := envState.kube.RbacV1().Roles(permission.namespace).Create(ctx, &rbacv1.Role{
			ObjectMeta: metav1.ObjectMeta{Name: permission.name}, Rules: permission.rules,
		}, metav1.CreateOptions{})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			_ = envState.kube.RbacV1().Roles(role.Namespace).Delete(context.Background(), role.Name, metav1.DeleteOptions{})
		})
		binding, err := envState.kube.RbacV1().RoleBindings(permission.namespace).Create(ctx, &rbacv1.RoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: permission.name},
			RoleRef:    rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "Role", Name: permission.name},
			Subjects:   []rbacv1.Subject{{Kind: "User", APIGroup: "rbac.authorization.k8s.io", Name: permission.user}},
		}, metav1.CreateOptions{})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			_ = envState.kube.RbacV1().RoleBindings(binding.Namespace).Delete(context.Background(), binding.Name, metav1.DeleteOptions{})
		})
	}
	auditBackend, err := k8saudit.NewConfigMapStore(envState.ctrlClient, "default")
	if err != nil {
		t.Fatal(err)
	}
	envState.mu.Lock()
	previousOptions := envState.options
	envState.options = []Option{
		WithWorkflowBindingAdministrators("default", []string{"workflow-operators"}, []string{base.Executor.Subject}),
		WithWorkflowBindingReader(envState.ctrlClient), WithAuditLog(observability.NewCognitiveAuditLog(auditBackend)),
	}
	envState.mu.Unlock()
	defer func() {
		envState.mu.Lock()
		envState.options = previousOptions
		envState.mu.Unlock()
	}()
	bindings, err := nativecontroller.NewWorkflowExecutorRecordStore(operatorClient, envState.ctrlClient, "default", service.Clock)
	if err != nil {
		t.Fatal(err)
	}
	previousBindings := service.Bindings
	service.Bindings = bindings
	defer func() { service.Bindings = previousBindings }()
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			childCtx, childCancel := context.WithTimeout(ctx, 20*time.Second)
			defer childCancel()
			definition := workflowBindingDefinitionFixture()
			definition.Name, definition.UID, definition.ResourceVersion = "installed-"+test.name, "", ""
			if err := envState.ctrlClient.Create(childCtx, definition); err != nil {
				t.Fatal(err)
			}
			defer func() { _ = envState.ctrlClient.Delete(context.Background(), definition) }()
			authority := base
			authority.Reference.Name, authority.Reference.UID, authority.Reference.ResourceVersion = definition.Name, "", 0
			authority.Workflow.Name, authority.Workflow.UID = definition.Name, string(definition.UID)
			if _, err := fmt.Sscan(definition.ResourceVersion, &authority.Workflow.ResourceVersion); err != nil {
				t.Fatal(err)
			}
			definitionJSON, err := json.Marshal(convert.WorkflowFromK8s(definition))
			if err != nil {
				t.Fatal(err)
			}
			authority.WorkflowRevisionDigest, err = runtimeapproval.WorkflowExecutorSpecDigest(definitionJSON)
			if err != nil {
				t.Fatal(err)
			}
			authority.ApprovedBy = api.ControlPlaneRoleSubject{Method: "tokenreview", Issuer: "kubernetes.io/tokenreview", Subject: operatorName}
			authority, err = bindings.CreateWorkflowExecutorAuthority(childCtx, authority)
			if err != nil {
				t.Fatalf("create installed operator-owned authority: %v", err)
			}
			if err := wait.PollUntilContextTimeout(childCtx, 50*time.Millisecond, 5*time.Second, true, func(ctx context.Context) (bool, error) {
				return service.Authorizer.Authorize(ctx, authenticationv1.UserInfo{Username: submitterName, Groups: []string{"system:authenticated"}}, authority.Workflow, authority.WorkflowRevisionDigest) == nil, nil
			}); err != nil {
				t.Fatalf("actual submitter lacks named Workflow/use permission: %v", err)
			}
			bindingJSON, err := json.Marshal(authority.Reference)
			if err != nil {
				t.Fatal(err)
			}
			run := &k8sapi.WorkflowRun{ObjectMeta: metav1.ObjectMeta{Name: definition.Name, Namespace: "team-a",
				Annotations: map[string]string{runtimeapproval.WorkflowRunAcceptanceRequiredAnnotation: "true", runtimeapproval.NativeWorkflowExecutionBindingAnnotation: string(bindingJSON)}},
				Spec: k8sapi.WorkflowRunSpec{WorkflowRef: k8sapi.LocalObjectReference{Name: definition.Name}}}
			if err := submitterClient.Create(childCtx, run); err != nil {
				t.Fatalf("authenticated submitter could not create native run: %v", err)
			}
			defer func() { _ = envState.ctrlClient.Delete(context.Background(), run) }()
			paused := &pausedWorkflowAcceptanceClient{Client: executorClient, entered: make(chan struct{}), release: make(chan struct{})}
			records, err := nativecontroller.NewWorkflowExecutorRecordStore(paused, envState.ctrlClient, "default", service.Clock)
			if err != nil {
				t.Fatal(err)
			}
			if err := records.ConfigureWorkflowRunSubmission(service, func(ctx context.Context) (api.ControlPlaneRoleSubject, string, error) {
				user, err := identity.AuthenticatedWorkflowExecutorUser(ctx)
				if err != nil {
					return api.ControlPlaneRoleSubject{}, "", err
				}
				return api.ControlPlaneRoleSubject{Method: "tokenreview", Issuer: "kubernetes.io/tokenreview", Subject: user.Username}, user.Extra["authentication.kubernetes.io/pod-uid"][0], nil
			}); err != nil {
				t.Fatal(err)
			}
			var released sync.Once
			release := func() { released.Do(func() { close(paused.release) }) }
			done := make(chan struct{})
			var accepted runtimeapproval.WorkflowRunAcceptance
			var acceptanceErr error
			go func() {
				defer close(done)
				accepted, acceptanceErr = records.FinalizeWorkflowRunSubmission(childCtx, run.Namespace, run.Name)
			}()
			defer func() { childCancel(); release(); <-done }()
			select {
			case <-paused.entered:
			case <-done:
				t.Fatalf("native finalization failed before protected persistence: %v", acceptanceErr)
			case <-childCtx.Done():
				t.Fatal("native finalization never reached protected persistence")
			}
			if test.acceptanceFirst {
				release()
				<-done
				if acceptanceErr != nil || accepted.RunUID != string(run.UID) || accepted.Initiator.Subject != submitterName {
					t.Fatalf("acceptance-first lost exact run or submitter: %+v %v", accepted, acceptanceErr)
				}
			}
			if test.binding {
				authority.Enabled = false
				if _, err := bindings.UpdateWorkflowExecutorAuthority(childCtx, authority); err != nil {
					t.Fatalf("installed operator revocation failed: %v", err)
				}
			} else {
				role, err := envState.kube.RbacV1().Roles("team-a").Get(childCtx, submitterName, metav1.GetOptions{})
				if err != nil {
					t.Fatal(err)
				}
				rules := role.Rules
				role.Rules = role.Rules[:1]
				role, err = envState.kube.RbacV1().Roles("team-a").Update(childCtx, role, metav1.UpdateOptions{})
				if err != nil {
					t.Fatal(err)
				}
				defer func() {
					role.Rules = rules
					if _, err := envState.kube.RbacV1().Roles("team-a").Update(context.Background(), role, metav1.UpdateOptions{}); err != nil {
						t.Error(err)
					}
				}()
				if err := wait.PollUntilContextTimeout(childCtx, 50*time.Millisecond, 5*time.Second, true, func(ctx context.Context) (bool, error) {
					return service.Authorizer.Authorize(ctx, authenticationv1.UserInfo{Username: submitterName, Groups: []string{"system:authenticated"}}, authority.Workflow, authority.WorkflowRevisionDigest) != nil, nil
				}); err != nil {
					t.Fatalf("actual Workflow/use revocation did not take effect: %v", err)
				}
			}
			if !test.acceptanceFirst {
				release()
				<-done
				if acceptanceErr == nil || accepted.RunUID != "" {
					t.Fatalf("installed admission persisted acceptance after completed revocation: %+v %v", accepted, acceptanceErr)
				}
			}
			retained, err := records.GetWorkflowRunAcceptance(childCtx, run.Namespace, string(run.UID))
			if test.acceptanceFirst {
				if err != nil || retained.RunUID != accepted.RunUID || retained.Initiator.Subject != submitterName {
					t.Fatalf("revocation erased retained acceptance or original attribution: %+v %v", retained, err)
				}
			} else if !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("revocation-first left private acceptance evidence: %+v %v", retained, err)
			}
			fresh := run.DeepCopy()
			fresh.Name, fresh.UID, fresh.ResourceVersion = run.Name+"-after-revocation", "", ""
			if err := submitterClient.Create(childCtx, fresh); err == nil {
				t.Fatal("completed revocation permitted a new native submission")
			}
			if err := envState.ctrlClient.Get(childCtx, client.ObjectKeyFromObject(fresh), &k8sapi.WorkflowRun{}); !apierrors.IsNotFound(err) {
				t.Fatalf("denied new submission persisted a public run: %v", err)
			}
		})
	}
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
