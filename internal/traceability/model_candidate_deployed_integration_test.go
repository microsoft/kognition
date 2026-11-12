// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build integration

package traceability

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	api "github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/admission"
	"github.com/kognition-ai/kognition/internal/controller"
	"github.com/kognition-ai/kognition/internal/cpi"
	"github.com/kognition-ai/kognition/internal/cpi/drivers/openai"
	k8saudit "github.com/kognition-ai/kognition/internal/k8s/audit"
	k8sctrl "github.com/kognition-ai/kognition/internal/k8s/controller"
	k8scost "github.com/kognition-ai/kognition/internal/k8s/cost"
	"github.com/kognition-ai/kognition/internal/observability"
	"github.com/kognition-ai/kognition/internal/runtimeauth"
	"github.com/kognition-ai/kognition/internal/store"
	"github.com/kognition-ai/kognition/internal/store/postgres"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type candidateUsageFault struct {
	writer observability.UsageEventWriter
	fail   bool
}

func (fault *candidateUsageFault) ApplyUsageEvent(ctx context.Context, event observability.CostRecord) (bool, error) {
	if fault.fail {
		return false, errors.New("injected accounting outage")
	}
	return fault.writer.ApplyUsageEvent(ctx, event)
}

func TestDeployedModelCandidatePromotionRollbackRecovery(test *testing.T) {
	if os.Getenv("KOGNITION_MODEL_CANDIDATE_ACCEPTANCE") != "true" {
		test.Skip("explicit isolated candidate acceptance required")
	}
	selectedMode := os.Getenv("KOGNITION_CANDIDATE_MODE")
	if selectedMode != "" && selectedMode != "standalone" && selectedMode != "kubernetes" {
		test.Fatal("invalid explicit candidate backend")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	configuration, err := rest.InClusterConfig()
	if err != nil {
		test.Fatal(err)
	}
	configuration.Timeout = 10 * time.Second
	admin, err := client.New(configuration, client.Options{Scheme: k8sctrl.SetupScheme()})
	if err != nil {
		test.Fatal(err)
	}
	var nodes corev1.NodeList
	var root corev1.Namespace
	if err := admin.List(ctx, &nodes); err != nil {
		test.Fatal(err)
	}
	if err := admin.Get(ctx, client.ObjectKey{Name: "kube-system"}, &root); err != nil {
		test.Fatal(err)
	}
	runID := os.Getenv("KOGNITION_CANDIDATE_RUN_ID")
	if err := validateInstallationCluster(nodes.Items, "kognition-"+runID+"-control-plane", &root, os.Getenv("KOGNITION_CANDIDATE_CLUSTER_UID"), runID); err != nil {
		test.Fatal(err)
	}
	if err := k8sctrl.NewHostClockHealthCheck(30 * time.Second)(); err != nil {
		test.Fatal(err)
	}
	pool, err := pgxpool.New(ctx, os.Getenv("KOGNITION_CANDIDATE_DATABASE_URL"))
	if err != nil {
		test.Fatal(err)
	}
	defer pool.Close()
	if err := pool.Ping(ctx); err != nil {
		test.Fatal(err)
	}
	if selectedMode == "" {
		if err := postgres.Migrate(ctx, pool, ""); err != nil {
			test.Fatal(err)
		}
		test.Log("normal migration runner applied the full schema to the fresh isolated database")
	} else {
		test.Log("explicit backend continuation uses the unchanged migrated database")
	}
	persistent := postgres.New(pool, zap.NewNop())
	clusterStore := k8sctrl.NewStoreAdapterWithControlNamespace(admin, "candidate-acceptance")
	clusterUsage, err := k8scost.NewConfigMapStore(admin, "candidate-acceptance")
	if err != nil {
		test.Fatal(err)
	}
	clusterAudit, err := k8saudit.NewConfigMapStore(admin, "candidate-acceptance")
	if err != nil {
		test.Fatal(err)
	}
	for _, mode := range []string{"standalone", "kubernetes"} {
		if selectedMode != "" && selectedMode != mode {
			continue
		}
		test.Run(mode, func(test *testing.T) {
			var storage store.Store = persistent
			var ledger store.ModelDispatchStore = persistent
			var usage observability.UsageEventWriter = persistent
			var auditStore observability.AuditStore = persistent
			if mode == "kubernetes" {
				storage = clusterStore
				ledger = clusterUsage
				usage = clusterUsage
				auditStore = clusterAudit
			}
			namespace := "candidate-" + mode
			suffix := os.Getenv("KOGNITION_CANDIDATE_ATTEMPT")
			className := "candidate-" + mode + "-" + runID[:8] + suffix
			read := func(kind, scope, name string, value any) {
				test.Helper()
				body, err := storage.Get(ctx, kind, scope, name)
				if err != nil {
					test.Fatal(err)
				}
				if err := json.Unmarshal(body, value); err != nil {
					test.Fatal(err)
				}
			}
			write := func(kind, scope, name string, value any) {
				test.Helper()
				body, err := json.Marshal(value)
				if err != nil {
					test.Fatal(err)
				}
				if err := storage.Create(ctx, kind, scope, name, body); err != nil {
					test.Fatal(err)
				}
			}
			key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
			if err != nil {
				test.Fatal(err)
			}
			public, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
			if err != nil {
				test.Fatal(err)
			}
			trust, err := json.Marshal(map[string]map[string]string{namespace: {"release": string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: public}))}})
			if err != nil {
				test.Fatal(err)
			}
			test.Setenv("KOGNITION_ARTIFACT_SIGNING_KEYS_JSON", string(trust))
			test.Setenv("KOGNITION_ARTIFACT_SIGNING_KEYS_FILE", "")
			driver := openai.New(&http.Client{Timeout: 5 * time.Second})
			registry := cpi.NewRegistry()
			registry.MustRegister(driver)
			counts := map[string]*atomic.Int32{}
			bindings := map[string]api.ModelVersionBinding{}
			for _, version := range []string{"v1", "v2", "bad", "recover", "denied", "unknown"} {
				counter := &atomic.Int32{}
				counts[version] = counter
				server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
					if request.Method != http.MethodPost || request.URL.Path != "/v1/chat/completions" {
						writer.WriteHeader(http.StatusNotFound)
						return
					}
					var body struct {
						Model     string        `json:"model"`
						Messages  []cpi.Message `json:"messages"`
						MaxTokens int           `json:"max_tokens"`
					}
					if err := json.NewDecoder(request.Body).Decode(&body); err != nil || body.MaxTokens != 16 || body.Model != "gpt-4-0613" {
						writer.WriteHeader(http.StatusBadRequest)
						return
					}
					counter.Add(1)
					if version == "unknown" {
						writer.WriteHeader(http.StatusServiceUnavailable)
						return
					}
					input, err := driver.CountInputTokens(&cpi.CompletionRequest{Model: body.Model, Messages: body.Messages})
					if err != nil {
						writer.WriteHeader(http.StatusInternalServerError)
						return
					}
					content := "KOGNITION_MODEL_CANARY_OK"
					if version == "bad" {
						content = "KOGNITION_MODEL_CANARY_BAD"
					}
					writer.Header().Set("Content-Type", "application/json")
					_ = json.NewEncoder(writer).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": content}, "finish_reason": "stop"}}, "usage": map[string]int{"prompt_tokens": input, "completion_tokens": 6, "total_tokens": input + 6}})
				}))
				test.Cleanup(server.Close)
				name := version + suffix
				model := api.Model{TypeMeta: api.TypeMeta{APIVersion: api.APIVersion, Kind: "Model"}, ObjectMeta: api.ObjectMeta{Name: name, Namespace: namespace}, Spec: api.ModelSpec{ClassName: className, Provider: "openai-compatible", ModelID: "gpt-4-0613", Endpoint: server.URL}}
				body, err := json.Marshal(model)
				if err != nil {
					test.Fatal(err)
				}
				payload, err := admission.ArtifactSignaturePayload("Model", namespace, name, body)
				if err != nil {
					test.Fatal(err)
				}
				digest := sha256.Sum256(payload)
				signature, err := ecdsa.SignASN1(rand.Reader, key, digest[:])
				if err != nil {
					test.Fatal(err)
				}
				envelope, err := json.Marshal(map[string]any{"version": 1, "keyID": "release", "signature": base64.StdEncoding.EncodeToString(signature)})
				if err != nil {
					test.Fatal(err)
				}
				model.Spec.Signature = string(envelope)
				write("Model", namespace, name, model)
				read("Model", namespace, name, &model)
				configurationDigest, err := runtimeauth.ModelConfigurationDigest(&model)
				if err != nil {
					test.Fatal(err)
				}
				bindings[version] = api.ModelVersionBinding{Namespace: namespace, ModelName: name, ModelUID: model.UID, ModelResourceVersion: model.ResourceVersion, SpecDigest: configurationDigest}
			}
			class := api.ModelClass{TypeMeta: api.TypeMeta{APIVersion: api.APIVersion, Kind: "ModelClass"}, ObjectMeta: api.ObjectMeta{Name: className}, Spec: api.ModelClassSpec{Provisioner: "openai-compatible", VersionPin: "v1", VersionCandidates: bindings}}
			write("ModelClass", "", className, class)
			quotaName := "tokens" + suffix
			var quota api.CognitiveQuota
			if _, err := storage.Get(ctx, "CognitiveQuota", namespace, quotaName); !errors.Is(err, store.ErrNotFound) {
				test.Fatal("fresh quota fixture required", err)
			}
			quota = api.CognitiveQuota{TypeMeta: api.TypeMeta{APIVersion: api.APIVersion, Kind: "CognitiveQuota"}, ObjectMeta: api.ObjectMeta{Name: quotaName, Namespace: namespace}, Spec: api.CognitiveQuotaSpec{Hard: map[string]int64{}, TokenBudget: &api.TokenBudget{MaxOutputTokens: 1000}}}
			write("CognitiveQuota", namespace, quotaName, quota)
			fault := &candidateUsageFault{writer: usage}
			newReconciler := func() *controller.ModelClassReconciler {
				reconciler := controller.NewModelClassReconciler(storage, registry, zaptest.NewLogger(test))
				reconciler.SetCanaryAccounting(ledger, fault, observability.NewCognitiveAuditLog(auditStore))
				return reconciler
			}
			reconciler := newReconciler()
			reconcile := func(iterations int) {
				test.Helper()
				for attempt := 0; attempt < iterations; attempt++ {
					if _, err := reconciler.Reconcile(ctx, controller.Request{Name: className}); err != nil {
						test.Fatal(err)
					}
				}
				read("ModelClass", "", className, &class)
			}
			pin := func(version string) {
				test.Helper()
				read("ModelClass", "", className, &class)
				class.Spec.VersionPin = version
				body, err := json.Marshal(class)
				if err != nil {
					test.Fatal(err)
				}
				if err := storage.Update(ctx, "ModelClass", "", className, body, class.ResourceVersion); err != nil {
					test.Fatal(err)
				}
			}
			assertServing := func(version string) {
				test.Helper()
				if class.Status.ActiveVersion != version || class.Status.ActiveBinding == nil || *class.Status.ActiveBinding != bindings[version] {
					test.Fatalf("serving binding changed unexpectedly: %+v", class.Status)
				}
				if _, err := runtimeauth.ResolveModelBinding(ctx, storage, *class.Status.ActiveBinding); err != nil {
					test.Fatal(err)
				}
			}
			reconcile(4)
			assertServing("v1")
			if class.Status.Phase != api.PhaseReady || counts["v1"].Load() != 1 {
				test.Fatal("initial signed serving binding was not established through bounded inference")
			}
			pin("v2")
			reconcile(1)
			assertServing("v2")
			reconciler = newReconciler()
			reconcile(1)
			if counts["v2"].Load() != 1 {
				test.Fatal("restart repeated the promoted candidate")
			}
			pin("bad")
			reconcile(1)
			assertServing("v2")
			if class.Status.Phase != api.PhaseDegraded || counts["bad"].Load() != 1 {
				test.Fatal("failed candidate did not retain the previous serving binding")
			}
			reconciler = newReconciler()
			reconcile(1)
			assertServing("v2")
			if counts["bad"].Load() != 1 {
				test.Fatal("failed candidate replayed inference after restart")
			}
			fault.fail = true
			pin("recover")
			reconcile(1)
			assertServing("v2")
			fault.fail = false
			reconciler = newReconciler()
			reconcile(1)
			assertServing("recover")
			if counts["recover"].Load() != 1 {
				test.Fatal("known accounting failure recovery repeated inference")
			}
			read("CognitiveQuota", namespace, quotaName, &quota)
			quota.Spec.TokenBudget.MaxOutputTokens = 1
			body, err := json.Marshal(quota)
			if err != nil {
				test.Fatal(err)
			}
			if err := storage.Update(ctx, "CognitiveQuota", namespace, quotaName, body, quota.ResourceVersion); err != nil {
				test.Fatal(err)
			}
			pin("denied")
			reconcile(1)
			assertServing("recover")
			if counts["denied"].Load() != 0 {
				test.Fatal("insufficient token budget reached provider")
			}
			read("CognitiveQuota", namespace, quotaName, &quota)
			quota.Spec.TokenBudget.MaxOutputTokens = 1000
			body, err = json.Marshal(quota)
			if err != nil {
				test.Fatal(err)
			}
			if err := storage.Update(ctx, "CognitiveQuota", namespace, quotaName, body, quota.ResourceVersion); err != nil {
				test.Fatal(err)
			}
			reconciler = newReconciler()
			reconcile(1)
			assertServing("recover")
			if counts["denied"].Load() != 0 {
				test.Fatal("restored quota replayed a terminal denied claim")
			}
			pin("unknown")
			reconcile(1)
			assertServing("recover")
			reconciler = newReconciler()
			reconcile(1)
			assertServing("recover")
			if counts["unknown"].Load() != 1 {
				test.Fatal("unknown provider outcome was retried")
			}
			events, err := usage.(observability.UsageEventApplyQueryStore).QueryUsageEvents(ctx)
			if err != nil {
				test.Fatal(err)
			}
			seen := map[string]bool{}
			var knownInput, knownOutput int64
			protected := storage.(store.CanaryReservationStore)
			for _, event := range events {
				if event.Workspace != namespace || event.ModelClass != className {
					continue
				}
				if seen[event.ModelClassVersion] || event.Purpose != observability.UsagePurposeModelCanary || !event.UsageReported || event.CompletionTokens != 6 {
					test.Fatal("duplicated or invalid durable candidate usage")
				}
				seen[event.ModelClassVersion] = true
				knownInput += event.PromptTokens
				knownOutput += event.CompletionTokens
				disposition, err := protected.SettleCanaryReservation(ctx, "reasoning-canary:"+event.ID, event.PromptTokens, event.CompletionTokens)
				if err != nil || disposition.State != store.CanaryReservationSettled {
					test.Fatalf("missing exact immutable settlement: %+v %v", disposition, err)
				}
			}
			if len(seen) != 4 || !seen["v1"] || !seen["v2"] || !seen["bad"] || !seen["recover"] {
				test.Fatal("known outcomes did not each produce exactly one usage event")
			}
			input, err := driver.CountInputTokens(&cpi.CompletionRequest{Model: "gpt-4-0613", Messages: []cpi.Message{{Role: "user", Content: "Return exactly KOGNITION_MODEL_CANARY_OK"}}})
			if err != nil {
				test.Fatal(err)
			}
			if reaped, err := storage.(store.ExpiredTokenReservationStore).ReapExpiredTokenReservations(ctx, time.Now().Add(time.Hour), 100); err != nil || reaped != 0 {
				test.Fatalf("unknown protected reservation was reaped: %d %v", reaped, err)
			}
			read("CognitiveQuota", namespace, quotaName, &quota)
			if quota.Status.TokenUsage == nil || quota.Status.TokenUsage.InputTokens != knownInput+int64(input) || quota.Status.TokenUsage.OutputTokens != knownOutput+16 || quota.Status.TokenUsage.TotalTokens != knownInput+knownOutput+int64(input)+16 {
				test.Fatalf("quota settlement or protected unknown ceiling changed: %+v", quota.Status.TokenUsage)
			}
			test.Logf("mode=%s promotion=v1->v2 bad-canary-retains=v2 known-recovery-calls=1 token-denied-calls=0 unknown-calls=1 usage-events=4 settlements=4 protected-unknown=1 final-serving=recover", mode)
		})
	}
}
