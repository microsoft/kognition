// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build integration

package traceability

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	k8saudit "github.com/kognition-ai/kognition/internal/k8s/audit"
	k8scontroller "github.com/kognition-ai/kognition/internal/k8s/controller"
	k8scost "github.com/kognition-ai/kognition/internal/k8s/cost"
	"github.com/kognition-ai/kognition/internal/observability"
	"github.com/kognition-ai/kognition/internal/runtimeapproval"
	"github.com/kognition-ai/kognition/internal/store"

	admissionv1 "k8s.io/api/admissionregistration/v1"
	appsv1 "k8s.io/api/apps/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	batchv1 "k8s.io/api/batch/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	coordinationclient "k8s.io/client-go/kubernetes/typed/coordination/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

func TestControllerInstallationConfigMapAdmission(test *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		test.Skip("KUBEBUILDER_ASSETS is not set")
	}
	environment := &envtest.Environment{}
	environment.ControlPlane.GetAPIServer().Configure().Set("authorization-mode", "RBAC")
	config, err := environment.Start()
	if err != nil {
		test.Fatal(err)
	}
	test.Cleanup(func() {
		if err := stopControllerEnvtest(environment); err != nil {
			test.Errorf("stop envtest: %v", err)
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	scheme := runtime.NewScheme()
	for _, register := range []func(*runtime.Scheme) error{corev1.AddToScheme, rbacv1.AddToScheme, admissionv1.AddToScheme, networkingv1.AddToScheme, appsv1.AddToScheme, batchv1.AddToScheme, coordinationv1.AddToScheme, authorizationv1.AddToScheme} {
		if err := register(scheme); err != nil {
			test.Fatal(err)
		}
	}
	admin, err := client.New(config, client.Options{Scheme: scheme})
	if err != nil {
		test.Fatal(err)
	}
	if err := admin.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "control"}}); err != nil {
		test.Fatal(err)
	}
	if err := admin.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "tenant"}}); err != nil {
		test.Fatal(err)
	}
	if err := admin.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "outside"}}); err != nil {
		test.Fatal(err)
	}
	// Seed malformed names before admission so UPDATE probes retain immutable names.
	healthNameFixtures := seedModelCallHealthNameFixtures(test, ctx, admin)
	controllerRoles := 0
	var gatewayMigrationFixture corev1.ConfigMap
	for _, document := range renderControllerRBAC(test, "--kube-version", "1.30.3", "--set", "k8sController.managedNamespaces[0]=tenant", "--set", "agentCommunicationGateway.enabled=true", "--set", "modelGateway.enabled=true",
		"--set", "knowledgeGateway.enabled=true", "--set", "knowledgeGateway.tls.secretName=provider-client-tls", "--set", "knowledgeGateway.runtimeTLS.secretName=knowledge-runtime-tls",
		"--set", "knowledgeGateway.provider.address=provider:9443", "--set", "knowledgeGateway.provider.serverName=provider", "--set", "knowledgeGateway.provider.peerURI=spiffe://kognition.dev/knowledge/provider") {
		var metadata metav1.TypeMeta
		if err := json.Unmarshal(document.Raw, &metadata); err != nil {
			test.Fatal(err)
		}
		var object client.Object
		switch metadata.Kind {
		case "ConfigMap":
			var value corev1.ConfigMap
			if err := json.Unmarshal(document.Raw, &value); err != nil {
				test.Fatal(err)
			}
			if value.Name == authorityProductionRelease+"-migrations" {
				gatewayMigrationFixture = value
			}
			continue
		case "Role":
			object = &rbacv1.Role{}
		case "RoleBinding":
			object = &rbacv1.RoleBinding{}
		case "ValidatingAdmissionPolicy":
			object = &admissionv1.ValidatingAdmissionPolicy{}
		case "ValidatingAdmissionPolicyBinding":
			object = &admissionv1.ValidatingAdmissionPolicyBinding{}
		default:
			continue
		}
		if err := json.Unmarshal(document.Raw, object); err != nil {
			test.Fatal(err)
		}
		if err := admin.Create(ctx, object); err != nil {
			test.Fatal(err)
		}
		if role, ok := object.(*rbacv1.Role); ok && role.Labels["app.kubernetes.io/component"] == "k8s-controller" {
			controllerRoles++
			if err := admin.Create(ctx, &rbacv1.RoleBinding{
				ObjectMeta: metav1.ObjectMeta{Name: "fixture-" + role.Name, Namespace: role.Namespace},
				RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: role.Name},
				Subjects:   []rbacv1.Subject{{Kind: "ServiceAccount", Name: "security-test-kognition-controller", Namespace: "control"}},
			}); err != nil {
				test.Fatal(err)
			}
		}
	}
	if controllerRoles != 3 {
		test.Fatalf("expected installation, workload, and quota controller Roles, got %d", controllerRoles)
	}
	controllerConfig := rest.CopyConfig(config)
	controllerConfig.Impersonate = rest.ImpersonationConfig{
		UserName: "system:serviceaccount:control:security-test-kognition-controller",
		Groups:   []string{"system:serviceaccounts", "system:serviceaccounts:control", "system:authenticated"},
	}
	controller, err := client.New(controllerConfig, client.Options{Scheme: scheme})
	if err != nil {
		test.Fatal(err)
	}
	unrelated := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "installation-settings", Namespace: "control"}}
	var admissionError error
	if err := wait.PollUntilContextTimeout(ctx, 250*time.Millisecond, 30*time.Second, true, func(ctx context.Context) (bool, error) {
		admissionError = controller.Create(ctx, unrelated.DeepCopy(), client.DryRunAll)
		return isControllerPolicyDenial(admissionError), nil
	}); err != nil {
		test.Fatalf("controller installation ConfigMap admission: %v; last response: %#v", err, admissionError)
	}
	test.Run("resource-webhook-audit", func(test *testing.T) {
		verifyResourceWebhookAuditConfigMapBoundary(test, ctx, admin, config, scheme)
	})
	test.Run("model-call-health", func(test *testing.T) {
		verifyModelCallHealthConfigMapBoundary(test, ctx, admin, config, scheme, healthNameFixtures)
	})
	test.Run("gateway-configmaps", func(test *testing.T) {
		verifyGatewayConfigMapBoundary(test, ctx, admin, config, scheme, gatewayMigrationFixture)
	})
	test.Run("rbac-enforced", func(test *testing.T) {
		secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "installation-credential", Namespace: "control"}, Type: corev1.SecretTypeOpaque}
		if err := admin.Create(ctx, secret); err != nil {
			test.Fatal(err)
		}
		err := controller.Get(ctx, client.ObjectKeyFromObject(secret), &corev1.Secret{})
		if !apierrors.IsForbidden(err) || isControllerPolicyDenial(err) {
			test.Fatalf("read installation credential: got %v, want RBAC denial", err)
		}
	})
	test.Run("native-rbac-boundaries", func(test *testing.T) {
		verifyControllerNativeRBACBoundaries(test, ctx, admin, controller, controllerConfig.Impersonate)
	})
	test.Run("production-rate-reservations", func(test *testing.T) {
		first := k8scontroller.NewStoreAdapterWithControlNamespace(controller, "control")
		second := k8scontroller.NewStoreAdapterWithControlNamespace(controller, "control")
		reservation := store.ToolGrantRateReservation{Key: "tenant/grant@1/agent/tool", Namespace: "tenant", Now: time.Now().UTC(), CallsPerMinute: 2}
		if err := first.ReserveToolGrantInvocation(ctx, reservation); err != nil {
			test.Fatal(err)
		}
		if err := second.ReserveToolGrantInvocation(ctx, reservation); err != nil {
			test.Fatalf("renew production rate Lease: %v", err)
		}
		if err := first.ReserveToolGrantInvocation(ctx, reservation); !errors.Is(err, store.ErrQuotaExceeded) {
			test.Fatalf("exhausted shared rate limit: %v", err)
		}
		reservation.Now = reservation.Now.Add(time.Minute)
		if err := second.ReserveToolGrantInvocation(ctx, reservation); err != nil {
			test.Fatalf("reset production rate window: %v", err)
		}
	})
	test.Run("production-binding-key-rotation", func(test *testing.T) {
		first := k8scontroller.NewStoreAdapterWithControlNamespace(controller, "tenant")
		second := k8scontroller.NewStoreAdapterWithControlNamespace(controller, "tenant")
		initial, err := runtimeapproval.NewStaticBindingKeyStore([]runtimeapproval.BindingKey{{
			Version: 1, Algorithm: runtimeapproval.BindingAlgorithmV1, Domain: runtimeapproval.BindingDomainV1,
			Status: runtimeapproval.BindingKeyActive, Material: []byte("0123456789abcdef0123456789abcdef"),
		}})
		if err != nil {
			test.Fatal(err)
		}
		rotated, err := runtimeapproval.NewStaticBindingKeyStoreAtEpoch(2, []runtimeapproval.BindingKey{
			{Version: 1, Epoch: 1, Algorithm: runtimeapproval.BindingAlgorithmV1, Domain: runtimeapproval.BindingDomainV1, Status: runtimeapproval.BindingKeyVerifyOnly, Material: []byte("0123456789abcdef0123456789abcdef")},
			{Version: 2, Algorithm: runtimeapproval.BindingAlgorithmV1, Domain: runtimeapproval.BindingDomainV1, Status: runtimeapproval.BindingKeyActive, Material: []byte("abcdef0123456789abcdef0123456789")},
		})
		if err != nil {
			test.Fatal(err)
		}
		if err := first.ActivateRuntimeApprovalBindingKeys(ctx, initial); err != nil {
			test.Fatalf("activate initial keys: %v", err)
		}
		if err := first.RuntimeApprovalBindingKeysReady(ctx); err != nil {
			test.Fatalf("initial readiness: %v", err)
		}
		if err := second.ActivateRuntimeApprovalBindingKeys(ctx, rotated); err != nil {
			test.Fatalf("rotate keys: %v", err)
		}
		if err := second.ActivateRuntimeApprovalBindingKeys(ctx, rotated); err != nil {
			test.Fatalf("idempotent rotation: %v", err)
		}
		if err := first.RuntimeApprovalBindingKeysReady(ctx); !errors.Is(err, runtimeapproval.ErrBindingKeyUnavailable) {
			test.Fatalf("stale replica readiness: %v", err)
		}
		if err := first.ActivateRuntimeApprovalBindingKeys(ctx, initial); err == nil {
			test.Fatal("accepted epoch rollback")
		}
		if err := first.ActivateRuntimeApprovalBindingKeys(ctx, rotated); err != nil {
			test.Fatalf("replica convergence: %v", err)
		}
		if err := first.RuntimeApprovalBindingKeysReady(ctx); err != nil {
			test.Fatalf("converged readiness: %v", err)
		}
	})
	if err := admin.Create(ctx, unrelated); err != nil {
		test.Fatal(err)
	}
	for _, name := range []string{"installation-settings", "kognition-cost-existing-unowned"} {
		test.Run(name, func(test *testing.T) {
			original := unrelated.DeepCopy()
			if name != unrelated.Name {
				original = &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "control"}}
				if err := admin.Create(ctx, original); err != nil {
					test.Fatal(err)
				}
			}
			forged := original.DeepCopy()
			forged.Labels = map[string]string{"kognition.dev/cost-record": "true"}
			forged.Data = map[string]string{"payload": "replacement"}
			if err := controller.Update(ctx, forged); !isControllerPolicyDenial(err) {
				test.Fatalf("installation takeover: got %v, want Forbidden", err)
			}
			if err := controller.Delete(ctx, original); !isControllerPolicyDenial(err) {
				test.Fatalf("installation deletion: got %v, want Forbidden", err)
			}
		})
	}
	state := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
		Name: "kognition-cost-admission-test", Namespace: "control",
		Labels: map[string]string{"kognition.dev/cost-record": "true"},
	}, Data: map[string]string{"payload": "initial"}}
	if err := controller.Create(ctx, state); err != nil {
		test.Fatalf("create controller state: %v", err)
	}
	verifyNativeMetadataTransitions(test, ctx, admin, controller, state, false, true)
	state.Data["payload"] = "updated"
	if err := controller.Update(ctx, state); err != nil {
		test.Fatalf("update controller state: %v", err)
	}
	if err := controller.Delete(ctx, state); err != nil {
		test.Fatalf("delete controller state: %v", err)
	}
	for _, metadata := range []metav1.ObjectMeta{
		{Name: "kognition-cost-missing-label"},
		{Name: "unreserved-name", Labels: map[string]string{"kognition.dev/cost-record": "true"}},
		{Name: "kognition-cost-helm", Labels: map[string]string{"kognition.dev/cost-record": "true", "app.kubernetes.io/managed-by": "Helm"}},
		{Name: "kognition-cost-helm-annotation", Labels: map[string]string{"kognition.dev/cost-record": "true"}, Annotations: map[string]string{"meta.helm.sh/release-name": "security-test"}},
	} {
		test.Run(metadata.Name, func(test *testing.T) {
			metadata.Namespace = "control"
			configMap := &corev1.ConfigMap{ObjectMeta: metadata}
			if err := controller.Create(ctx, configMap.DeepCopy()); !isControllerPolicyDenial(err) {
				test.Fatalf("create protected object: got %v, want policy denial", err)
			}
			if err := admin.Create(ctx, configMap); err != nil {
				test.Fatal(err)
			}
			if err := controller.Delete(ctx, configMap); !isControllerPolicyDenial(err) {
				test.Fatalf("delete protected object: got %v, want policy denial", err)
			}
		})
	}
	platformConfig := rest.CopyConfig(config)
	platformConfig.Impersonate = rest.ImpersonationConfig{
		UserName: "system:serviceaccount:control:security-test-kognition",
		Groups:   []string{"system:serviceaccounts", "system:serviceaccounts:control", "system:authenticated"},
	}
	platform, err := client.New(platformConfig, client.Options{Scheme: scheme})
	if err != nil {
		test.Fatal(err)
	}
	auditStore, err := k8saudit.NewConfigMapStore(platform, "control")
	if err != nil {
		test.Fatal(err)
	}
	entry := observability.AuditEntry{ID: "admission-audit", Workspace: "tenant", EventType: observability.AuditMemoryOp, Verb: "store"}
	for attempt := 0; attempt < 2; attempt++ {
		if err := auditStore.Append(ctx, entry); err != nil {
			test.Fatalf("append production audit: %v", err)
		}
	}
	entries, err := auditStore.Query(ctx, observability.AuditFilter{Workspace: "tenant", Limit: 10})
	if err != nil || len(entries) != 1 || entries[0].ID != entry.ID {
		test.Fatalf("read idempotent audit: entries=%v error=%v", entries, err)
	}
	if err := platform.Create(ctx, unrelated.DeepCopy(), client.DryRunAll); !isControllerPolicyDenial(err) {
		test.Fatalf("platform installation write: got %v, want policy denial", err)
	}
	test.Run("model-canary-receipts", func(test *testing.T) {
		forged := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "kognition-md-forged-claim", Namespace: "control", Labels: map[string]string{"kognition.dev/model-dispatch-record": "true"}}, Immutable: ptr.To(true)}
		if err := wait.PollUntilContextTimeout(ctx, 100*time.Millisecond, 10*time.Second, true, func(ctx context.Context) (bool, error) {
			return isControllerPolicyDenial(admin.Create(ctx, forged.DeepCopy(), client.DryRunAll)), nil
		}); err != nil {
			test.Fatal("receipt writer policy did not become active")
		}
		for _, writer := range []client.Client{platform, admin} {
			if err := writer.Create(ctx, forged.DeepCopy(), client.DryRunAll); !isControllerPolicyDenial(err) {
				test.Fatalf("forged receipt writer accepted: %v", err)
			}
		}
		ledger, err := k8scost.NewConfigMapStore(controller, "control")
		if err != nil {
			test.Fatal(err)
		}
		claimed, err := ledger.ClaimModelDispatch(ctx, "tenant", "canary-operation", "canary-session")
		if err != nil || !claimed {
			test.Fatalf("controller receipt claim=%t error=%v", claimed, err)
		}
		if err := ledger.CompleteModelDispatch(ctx, "tenant", "canary-operation", "canary-session", json.RawMessage(`{"version":1,"passed":true}`)); err != nil {
			test.Fatal(err)
		}
		record, err := ledger.GetModelDispatch(ctx, "tenant", "canary-operation")
		if err != nil || len(record.Response) == 0 {
			test.Fatal("controller could not recover receipt")
		}
		var receipts corev1.ConfigMapList
		if err := admin.List(ctx, &receipts, client.InNamespace("control"), client.MatchingLabels{"kognition.dev/model-dispatch-record": "true"}); err != nil || len(receipts.Items) != 2 {
			test.Fatal("expected immutable claim and terminal receipts")
		}
		for _, receipt := range receipts.Items {
			if err := controller.Delete(ctx, receipt.DeepCopy(), client.DryRunAll); !isControllerPolicyDenial(err) {
				test.Fatalf("receipt deletion allowed paid-call replay: %v", err)
			}
		}
	})
	test.Run("workload-network", func(test *testing.T) {
		ledger, err := k8scost.NewConfigMapStore(controller, "control")
		if err != nil {
			test.Fatal(err)
		}
		record := store.RuntimeRecoveryRecord{Workspace: "tenant", SessionID: "recovery-session", BindingDigest: fmt.Sprintf("%064x", 1), Ciphertext: json.RawMessage(`{"ciphertext":"encrypted-api-fixture"}`), ExpiresAt: time.Now().Add(time.Hour)}
		prepared, err := ledger.PrepareRuntimeRecovery(ctx, record)
		if err != nil {
			test.Fatal(err)
		}
		claimed, err := ledger.ClaimRuntimeRecovery(ctx, "tenant", "recovery-session", prepared.Version, "controller-owner")
		if err != nil {
			test.Fatal(err)
		}
		if err := ledger.CompleteRuntimeRecovery(ctx, claimed); err != nil {
			test.Fatal(err)
		}
		if _, err := ledger.ExpireRuntimeRecovery(ctx, time.Now().Add(2*time.Hour), 100); err != nil {
			test.Fatal(err)
		}
		var recoveries corev1.ConfigMapList
		if err := admin.List(ctx, &recoveries, client.InNamespace("control"), client.MatchingLabels{"kognition.dev/runtime-recovery": "true"}); err != nil || len(recoveries.Items) != 1 {
			test.Fatal("protected recovery record unavailable")
		}
		for _, writer := range []client.Client{platform, admin} {
			forged := recoveries.Items[0].DeepCopy()
			forged.Name = "kognition-rr-forged"
			forged.ResourceVersion = ""
			if err := writer.Create(ctx, forged, client.DryRunAll); !isControllerPolicyDenial(err) {
				test.Fatalf("recovery forgery accepted: %v", err)
			}
		}
		if err := controller.Delete(ctx, recoveries.Items[0].DeepCopy(), client.DryRunAll); !isControllerPolicyDenial(err) {
			test.Fatalf("recovery replay fence deletion accepted: %v", err)
		}
		verifyControllerNetworkAdmission(test, ctx, admin, controller)
	})
	test.Run("state-classifications", func(test *testing.T) {
		for _, classification := range []struct{ prefix, label string }{
			{"audit", "cognitive-audit"}, {"cost", "cost-record"}, {"usage-receipt", "usage-receipt"},
			{"token-reservations", "token-ledger"}, {"tool-call-reservations", "tool-call-ledger"}, {"tool-invocations", "tool-invocation-ledger"},
		} {
			state := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "kognition-" + classification.prefix + "-matrix", Namespace: "control",
				Labels: map[string]string{"kognition.dev/" + classification.label: "true"}}, Data: map[string]string{"payload": "initial"}}
			if err := controller.Create(ctx, state); err != nil {
				test.Fatal(err)
			}
			verifyNativeMetadataTransitions(test, ctx, admin, controller, state, false, true)
			state.Data["payload"] = "updated"
			if err := controller.Update(ctx, state); err != nil {
				test.Fatal(err)
			}
			if err := controller.Delete(ctx, state); err != nil {
				test.Fatal(err)
			}
		}
		revision := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "kognition-revision-matrix", Namespace: "control",
			Labels:          map[string]string{"kognition.dev/revision-parent": "researcher", "kognition.dev/revision-kind": "Agent"},
			OwnerReferences: []metav1.OwnerReference{{APIVersion: "kognition.dev/v1alpha1", Kind: "Agent", Name: "researcher", UID: "parent-uid"}}},
			Immutable: ptr.To(true), Data: map[string]string{"payload": "snapshot"}}
		if err := controller.Create(ctx, revision); err != nil {
			test.Fatal(err)
		}
		verifyNativeMetadataTransitions(test, ctx, admin, controller, revision, false, true)
		for name, mutate := range map[string]func(*corev1.ConfigMap){
			"owner-removed":           func(object *corev1.ConfigMap) { object.OwnerReferences = nil },
			"owner-api":               func(object *corev1.ConfigMap) { object.OwnerReferences[0].APIVersion = "other/v1" },
			"revision-kind-removed":   func(object *corev1.ConfigMap) { delete(object.Labels, "kognition.dev/revision-kind") },
			"revision-parent-removed": func(object *corev1.ConfigMap) { delete(object.Labels, "kognition.dev/revision-parent") },
		} {
			test.Run(name, func(test *testing.T) {
				forged := revision.DeepCopy()
				mutate(forged)
				if err := controller.Update(ctx, forged.DeepCopy(), client.DryRunAll); !isControllerPolicyDenial(err) {
					test.Fatalf("revision classification change: %v", err)
				}
				if err := admin.Update(ctx, forged); err != nil {
					test.Fatal(err)
				}
				revision.ResourceVersion = forged.ResourceVersion
				if err := controller.Update(ctx, revision.DeepCopy(), client.DryRunAll); !isControllerPolicyDenial(err) {
					test.Fatalf("revision adoption: %v", err)
				}
				if err := controller.Delete(ctx, forged, client.DryRunAll); !isControllerPolicyDenial(err) {
					test.Fatalf("unclassified revision deletion: %v", err)
				}
				if err := admin.Update(ctx, revision); err != nil {
					test.Fatal(err)
				}
			})
		}
		changed := revision.DeepCopy()
		changed.Data["payload"] = "changed"
		if err := controller.Update(ctx, changed, client.DryRunAll); !apierrors.IsInvalid(err) {
			test.Fatalf("immutable revision data: want Kubernetes validation denial, got %v", err)
		}
		revision.OwnerReferences[0].UID = "replacement-parent"
		if err := controller.Update(ctx, revision); err != nil {
			test.Fatalf("owner UID is classification, not authenticated provenance: %v", err)
		}
		if err := controller.Delete(ctx, revision); err != nil {
			test.Fatal(err)
		}
	})
	test.Run("network-compatibility", func(test *testing.T) {
		verifyControllerNetworkCompatibility(test, ctx, controller)
	})
	test.Run("sandbox-baseline-cannot-be-removed", func(test *testing.T) {
		for _, document := range renderControllerRBAC(test, "--set", "k8sController.managedNamespaces[0]=tenant") {
			var policy networkingv1.NetworkPolicy
			if err := json.Unmarshal(document.Raw, &policy); err != nil {
				test.Fatal(err)
			}
			if policy.Kind != "NetworkPolicy" || policy.Name != "security-test-kognition-stdio-baseline" {
				continue
			}
			if err := admin.Create(ctx, &policy); err != nil {
				test.Fatal(err)
			}
			if err := controller.Delete(ctx, &policy); !isControllerPolicyDenial(err) {
				test.Fatalf("delete isolation baseline: %v", err)
			}
			forged := policy.DeepCopy()
			forged.Annotations = nil
			forged.Labels = map[string]string{"app.kubernetes.io/managed-by": "kognition", "kognition.dev/agent": "forged"}
			forged.Spec.PodSelector = metav1.LabelSelector{MatchLabels: forged.Labels}
			forged.Spec.Egress = []networkingv1.NetworkPolicyEgressRule{{}}
			if err := controller.Update(ctx, forged); !isControllerPolicyDenial(err) {
				test.Fatalf("adopt isolation baseline: %v", err)
			}
			if err := admin.Get(ctx, client.ObjectKeyFromObject(&policy), &policy); err != nil {
				test.Fatal(err)
			}
			if len(policy.Spec.Egress) != 0 {
				test.Fatal("baseline isolation changed")
			}
			return
		}
		test.Fatal("baseline missing from chart")
	})
	test.Run("workload-isolation", func(test *testing.T) {
		verifyControllerWorkloadAdmission(test, ctx, admin, controller)
	})
	test.Run("workload-credentials", func(test *testing.T) {
		labels := map[string]string{"app.kubernetes.io/managed-by": "kognition"}
		for _, desired := range []client.Object{
			&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "runtime-credentials", Namespace: "tenant", Labels: labels}, Type: corev1.SecretTypeOpaque, Data: map[string][]byte{"token": []byte("test")}},
			&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "kognition-knowledge-gateway-ca", Namespace: "tenant", Labels: labels}, BinaryData: map[string][]byte{"ca.crt": []byte("test")}},
		} {
			unowned := desired.DeepCopyObject().(client.Object)
			unowned.SetLabels(nil)
			if err := admin.Create(ctx, unowned); err != nil {
				test.Fatal(err)
			}
			forged := unowned.DeepCopyObject().(client.Object)
			forged.SetLabels(labels)
			if err := controller.Update(ctx, forged); !isControllerPolicyDenial(err) {
				test.Fatalf("credential takeover: got %v", err)
			}
			if err := controller.Delete(ctx, unowned); !isControllerPolicyDenial(err) {
				test.Fatalf("credential deletion: got %v", err)
			}
			if err := admin.Delete(ctx, unowned); err != nil {
				test.Fatal(err)
			}
			if err := controller.Create(ctx, desired); err != nil {
				test.Fatal(err)
			}
			_, secret := desired.(*corev1.Secret)
			verifyNativeMetadataTransitions(test, ctx, admin, controller, desired, secret, true)
			switch credential := desired.(type) {
			case *corev1.Secret:
				credential.Data["token"] = []byte("rotated-token")
			case *corev1.ConfigMap:
				credential.BinaryData["ca.crt"] = []byte("rotated-ca")
			}
			if err := controller.Update(ctx, desired); err != nil {
				test.Fatalf("rotate owned credential: %v", err)
			}
			persisted := desired.DeepCopyObject().(client.Object)
			if err := controller.Get(ctx, client.ObjectKeyFromObject(desired), persisted); err != nil {
				test.Fatal(err)
			}
			switch credential := persisted.(type) {
			case *corev1.Secret:
				if string(credential.Data["token"]) != "rotated-token" {
					test.Fatal("token rotation was not persisted")
				}
			case *corev1.ConfigMap:
				if string(credential.BinaryData["ca.crt"]) != "rotated-ca" {
					test.Fatal("CA rotation was not persisted")
				}
			}
			if err := controller.Delete(ctx, desired); err != nil {
				test.Fatal(err)
			}
		}
		token := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "forged-token", Namespace: "tenant", Labels: labels, Annotations: map[string]string{"kubernetes.io/service-account.name": "default"}}, Type: corev1.SecretTypeServiceAccountToken}
		if err := controller.Create(ctx, token, client.DryRunAll); !isControllerPolicyDenial(err) {
			test.Fatalf("token Secret minting: got %v", err)
		}
	})
	test.Run("gateway-coordination-leases", func(test *testing.T) {
		role := &rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: "fixture-gateway-leases"}, Rules: []rbacv1.PolicyRule{{APIGroups: []string{"coordination.k8s.io"}, Resources: []string{"leases"}, Verbs: []string{"get", "create", "update", "delete"}}}}
		if err := admin.Create(ctx, role); err != nil {
			test.Fatal(err)
		}
		for _, component := range []string{"tool-gateway", "memory-policy-gateway", "agent-communication-gateway"} {
			name := "security-test-kognition-" + component
			binding := &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "fixture-" + component + "-leases"}, RoleRef: rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: role.Name}, Subjects: []rbacv1.Subject{{Kind: "ServiceAccount", Namespace: "control", Name: name}}}
			if err := admin.Create(ctx, binding); err != nil {
				test.Fatal(err)
			}
			gatewayConfig := rest.CopyConfig(config)
			gatewayConfig.Impersonate = rest.ImpersonationConfig{UserName: "system:serviceaccount:control:" + name, Groups: []string{"system:serviceaccounts", "system:serviceaccounts:control", "system:authenticated"}}
			gateway, err := client.New(gatewayConfig, client.Options{Scheme: scheme})
			if err != nil {
				test.Fatal(err)
			}
			for _, scenario := range []struct {
				namespace, lease string
				allow            bool
			}{{"tenant", "kognition-runtime-approval-records", true}, {"control", "kognition-runtime-approval-binding-keys", true}, {"control", "kognition-tool-grant-rate-gateway", true}, {"control", "kognition-usage-lock-gateway", true}, {"outside", "kognition-runtime-approval-records", false}, {"tenant", "kognition-runtime-approval-binding-keys", false}, {"tenant", "other-leader", false}} {
				lease := &coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{Namespace: scenario.namespace, Name: scenario.lease, Labels: map[string]string{"app.kubernetes.io/managed-by": "kognition"}}}
				admitted := gateway.Create(ctx, lease.DeepCopy(), client.DryRunAll)
				if scenario.allow && admitted != nil {
					test.Fatalf("legitimate gateway coordination denied: %s %s/%s: %v", component, scenario.namespace, scenario.lease, admitted)
				}
				if !scenario.allow && !isControllerPolicyDenial(admitted) {
					test.Fatalf("gateway escaped Lease boundary: %s %s/%s: %v", component, scenario.namespace, scenario.lease, admitted)
				}
			}
			unowned := &coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{Namespace: "tenant", Name: "kognition-runtime-approval-records"}}
			if err := admin.Create(ctx, unowned); err != nil {
				test.Fatal(err)
			}
			unowned.Labels = map[string]string{"app.kubernetes.io/managed-by": "kognition"}
			if err := gateway.Update(ctx, unowned, client.DryRunAll); !isControllerPolicyDenial(err) {
				test.Fatalf("gateway Lease takeover admitted: %v", err)
			}
			if err := gateway.Delete(ctx, unowned, client.DryRunAll); !isControllerPolicyDenial(err) {
				test.Fatalf("gateway Lease deletion admitted: %v", err)
			}
			if err := admin.Delete(ctx, unowned); err != nil {
				test.Fatal(err)
			}
		}
	})
	test.Run("coordination-leases", func(test *testing.T) {
		for _, name := range []string{"kognition-runtime-approval-binding-keys", "kognition-runtime-approval-records", "kognition-tool-grant-rate-test", "kognition-usage-lock-test"} {
			lease := &coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "control"}}
			if err := admin.Create(ctx, lease); err != nil {
				test.Fatal(err)
			}
			lease.Labels = map[string]string{"app.kubernetes.io/managed-by": "kognition"}
			if err := controller.Update(ctx, lease); !isControllerPolicyDenial(err) {
				test.Fatalf("Lease takeover: got %v", err)
			}
			if err := admin.Delete(ctx, lease); err != nil {
				test.Fatal(err)
			}
			lease.ResourceVersion = ""
			lease.UID = ""
			if err := controller.Create(ctx, lease); err != nil {
				test.Fatal(err)
			}
			verifyNativeMetadataTransitions(test, ctx, admin, controller, lease, false, false)
			lease.Spec.HolderIdentity = ptr.To("next-holder")
			if err := controller.Update(ctx, lease); err != nil {
				test.Fatalf("owned Lease renewal: %v", err)
			}
			unlabeled := lease.DeepCopy()
			unlabeled.Labels = nil
			if err := controller.Update(ctx, unlabeled); !isControllerPolicyDenial(err) {
				test.Fatalf("Lease ownership removal: got %v", err)
			}
		}
		leaseClient, err := coordinationclient.NewForConfig(controllerConfig)
		if err != nil {
			test.Fatal(err)
		}
		lock := &resourcelock.LeaseLock{
			LeaseMeta: metav1.ObjectMeta{Name: "kognition-k8s-controller", Namespace: "control"},
			Client:    leaseClient,
			Labels:    map[string]string{"app.kubernetes.io/managed-by": "kognition", "kognition.dev/leader-election": "true"},
		}
		record := resourcelock.LeaderElectionRecord{HolderIdentity: "replica-one", LeaseDurationSeconds: 15, AcquireTime: metav1.Now(), RenewTime: metav1.Now()}
		unowned := &coordinationv1.Lease{ObjectMeta: lock.LeaseMeta}
		if err := admin.Create(ctx, unowned); err != nil {
			test.Fatal(err)
		}
		if _, _, err := lock.Get(ctx); err != nil {
			test.Fatal(err)
		}
		if err := lock.Update(ctx, record); !isControllerPolicyDenial(err) {
			test.Fatalf("leader election takeover: got %v", err)
		}
		if err := admin.Delete(ctx, unowned); err != nil {
			test.Fatal(err)
		}
		if err := lock.Create(ctx, record); err != nil {
			test.Fatalf("leader election acquire: %v", err)
		}
		if _, _, err := lock.Get(ctx); err != nil {
			test.Fatal(err)
		}
		record.RenewTime = metav1.Now()
		if err := lock.Update(ctx, record); err != nil {
			test.Fatalf("leader election renew: %v", err)
		}
	})
}

func verifyControllerNativeRBACBoundaries(test *testing.T, ctx context.Context, admin, controller client.Client, identity rest.ImpersonationConfig) {
	test.Helper()
	for _, namespace := range []string{"control", "tenant", "outside"} {
		for _, capability := range []struct{ group, resource string }{
			{"", "configmaps"}, {"", "secrets"}, {"", "services"}, {"", "pods"},
			{"apps", "deployments"}, {"batch", "jobs"}, {"networking.k8s.io", "networkpolicies"},
			{"coordination.k8s.io", "leases"}, {"", "events"},
		} {
			for _, verb := range []string{"create", "update", "patch", "delete", "deletecollection"} {
				review := &authorizationv1.SubjectAccessReview{Spec: authorizationv1.SubjectAccessReviewSpec{
					User: identity.UserName, Groups: identity.Groups,
					ResourceAttributes: &authorizationv1.ResourceAttributes{Namespace: namespace, Group: capability.group, Resource: capability.resource, Verb: verb},
				}}
				if err := admin.Create(ctx, review); err != nil {
					test.Fatal(err)
				}
				want := false
				if namespace != "outside" && verb != "deletecollection" {
					switch capability.resource {
					case "events":
						want = verb == "create" || verb == "patch"
					case "leases":
						want = verb == "create" || verb == "update"
					case "configmaps":
						want = verb == "create" || verb == "delete" || (namespace == "control" && verb == "update")
					default:
						want = namespace == "tenant"
					}
				}
				if review.Status.Allowed != want || review.Status.EvaluationError != "" {
					test.Errorf("%s %s %s: want allowed=%t, got %+v", namespace, verb, capability.resource, want, review.Status)
				}
			}
		}
		test.Run("events-"+namespace, func(test *testing.T) {
			event := &corev1.Event{ObjectMeta: metav1.ObjectMeta{Name: "controller-matrix", Namespace: namespace},
				InvolvedObject: corev1.ObjectReference{APIVersion: "v1", Kind: "Pod", Namespace: namespace, Name: "subject", UID: "subject-uid"},
				Reason:         "Matrix", Message: "untrusted telemetry", Type: corev1.EventTypeNormal, Count: 1}
			err := controller.Create(ctx, event)
			if namespace == "outside" {
				if !apierrors.IsForbidden(err) || isControllerPolicyDenial(err) {
					test.Fatalf("unenrolled Event creation: want RBAC denial, got %v", err)
				}
				return
			}
			if err != nil {
				test.Fatal(err)
			}
			original := event.DeepCopy()
			event.Count++
			if err := controller.Patch(ctx, event, client.MergeFrom(original)); err != nil {
				test.Fatalf("aggregate Event: %v", err)
			}
			for _, err := range []error{controller.Update(ctx, event, client.DryRunAll), controller.Delete(ctx, event, client.DryRunAll)} {
				if !apierrors.IsForbidden(err) || isControllerPolicyDenial(err) {
					test.Fatalf("ungranted Event mutation: want RBAC denial, got %v", err)
				}
			}
		})
		for _, capability := range []struct{ group, resource, subresource, verb string }{
			{"", "pods", "attach", "create"},
			{"", "pods", "exec", "create"},
			{"", "pods", "portforward", "create"},
			{"", "pods", "ephemeralcontainers", "update"},
			{"", "pods", "ephemeralcontainers", "patch"},
			{"", "pods", "status", "update"},
			{"", "pods", "eviction", "create"},
			{"", "pods", "binding", "create"},
			{"apps", "deployments", "scale", "update"},
			{"apps", "deployments", "status", "patch"},
			{"batch", "jobs", "status", "update"},
			{"", "serviceaccounts", "token", "create"},
			{"", "secrets", "", "create"},
			{"", "pods", "", "deletecollection"},
		} {
			review := &authorizationv1.SubjectAccessReview{Spec: authorizationv1.SubjectAccessReviewSpec{
				User: identity.UserName, Groups: identity.Groups,
				ResourceAttributes: &authorizationv1.ResourceAttributes{Namespace: namespace, Group: capability.group, Resource: capability.resource, Subresource: capability.subresource, Verb: capability.verb},
			}}
			if err := admin.Create(ctx, review); err != nil {
				test.Fatal(err)
			}
			want := namespace == "tenant" && (capability.subresource == "attach" || capability.resource == "secrets")
			if review.Status.Allowed != want || review.Status.EvaluationError != "" {
				test.Errorf("%s %s %s/%s: want allowed=%t, got %+v", namespace, capability.verb, capability.resource, capability.subresource, want, review.Status)
			}
		}
	}
}

func verifyControllerWorkloadAdmission(test *testing.T, ctx context.Context, admin, controller client.Client) {
	test.Helper()
	if err := admin.Create(ctx, &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: "tenant"}}); err != nil {
		test.Fatal(err)
	}
	if err := admin.Create(ctx, &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "elevated", Namespace: "tenant"}}); err != nil {
		test.Fatal(err)
	}
	labels := map[string]string{"app.kubernetes.io/managed-by": "kognition", "kognition.dev/agent": "researcher"}
	pod := corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{Labels: labels},
		Spec: corev1.PodSpec{
			ServiceAccountName:           "default",
			AutomountServiceAccountToken: ptr.To(false),
			SecurityContext:              &corev1.PodSecurityContext{RunAsNonRoot: ptr.To(true), RunAsUser: ptr.To(int64(1000)), SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}},
			Containers:                   []corev1.Container{{Name: "agent", Image: "agent:test", SecurityContext: &corev1.SecurityContext{AllowPrivilegeEscalation: ptr.To(false), Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}}}}},
		},
	}
	jobPod := pod.DeepCopy()
	jobPod.Spec.RestartPolicy = corev1.RestartPolicyNever
	for _, desired := range []client.Object{
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "isolated-deployment", Namespace: "tenant", Labels: labels}, Spec: appsv1.DeploymentSpec{Selector: &metav1.LabelSelector{MatchLabels: labels}, Template: pod}},
		&batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "isolated-job", Namespace: "tenant", Labels: labels}, Spec: batchv1.JobSpec{Template: *jobPod}},
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "isolated-pod", Namespace: "tenant", Labels: labels}, Spec: pod.Spec},
	} {
		test.Run(desired.GetName(), func(test *testing.T) {
			for name, mutate := range map[string]func(*corev1.PodSpec){
				"elevated-service-account": func(spec *corev1.PodSpec) { spec.ServiceAccountName = "elevated" },
				"host-network":             func(spec *corev1.PodSpec) { spec.HostNetwork = true },
				"host-pid":                 func(spec *corev1.PodSpec) { spec.HostPID = true },
				"host-ipc":                 func(spec *corev1.PodSpec) { spec.HostIPC = true },
				"host-path": func(spec *corev1.PodSpec) {
					spec.Volumes = []corev1.Volume{{Name: "host", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/"}}}}
				},
				"api-token": func(spec *corev1.PodSpec) { spec.AutomountServiceAccountToken = ptr.To(true) },
				"projected-token": func(spec *corev1.PodSpec) {
					spec.Volumes = []corev1.Volume{{Name: "token", VolumeSource: corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{Sources: []corev1.VolumeProjection{{ServiceAccountToken: &corev1.ServiceAccountTokenProjection{Path: "token"}}}}}}}
				},
				"privileged": func(spec *corev1.PodSpec) {
					spec.Containers[0].SecurityContext.Privileged = ptr.To(true)
					spec.Containers[0].SecurityContext.AllowPrivilegeEscalation = ptr.To(true)
				},
				"capabilities": func(spec *corev1.PodSpec) {
					spec.Containers[0].SecurityContext.Capabilities.Add = []corev1.Capability{"SYS_ADMIN"}
				},
				"escalation": func(spec *corev1.PodSpec) { spec.Containers[0].SecurityContext.AllowPrivilegeEscalation = ptr.To(true) },
				"root":       func(spec *corev1.PodSpec) { spec.Containers[0].SecurityContext.RunAsUser = ptr.To(int64(0)) },
				"unconfined": func(spec *corev1.PodSpec) {
					spec.SecurityContext.SeccompProfile.Type = corev1.SeccompProfileTypeUnconfined
				},
				"init-container": func(spec *corev1.PodSpec) {
					spec.InitContainers = []corev1.Container{{Name: "init", Image: "agent:test", SecurityContext: &corev1.SecurityContext{Privileged: ptr.To(true)}}}
				},
			} {
				test.Run(name, func(test *testing.T) {
					forged := desired.DeepCopyObject().(client.Object)
					switch workload := forged.(type) {
					case *appsv1.Deployment:
						mutate(&workload.Spec.Template.Spec)
					case *batchv1.Job:
						mutate(&workload.Spec.Template.Spec)
					case *corev1.Pod:
						mutate(&workload.Spec)
					}
					if err := controller.Create(ctx, forged, client.DryRunAll); !isControllerPolicyDenial(err) {
						test.Fatalf("unsafe workload: got %v, want policy denial", err)
					}
				})
			}
			unowned := desired.DeepCopyObject().(client.Object)
			unowned.SetLabels(nil)
			if job, ok := unowned.(*batchv1.Job); ok {
				job.Spec.Template.Labels = nil
			}
			if err := admin.Create(ctx, unowned); err != nil {
				test.Fatal(err)
			}
			forged := unowned.DeepCopyObject().(client.Object)
			forged.SetLabels(labels)
			if err := controller.Update(ctx, forged); !isControllerPolicyDenial(err) {
				test.Fatalf("workload takeover: got %v, want policy denial", err)
			}
			if err := controller.Delete(ctx, unowned); !isControllerPolicyDenial(err) {
				test.Fatalf("unowned workload deletion: got %v, want policy denial", err)
			}
			desired.SetName(desired.GetName() + "-owned")
			if err := controller.Create(ctx, desired); err != nil {
				test.Fatalf("create restricted workload: %v", err)
			}
			verifyNativeMetadataTransitions(test, ctx, admin, controller, desired, true, true)
			desired.SetAnnotations(map[string]string{"test": "updated"})
			if err := controller.Update(ctx, desired); err != nil {
				test.Fatalf("update restricted workload: %v", err)
			}
			if err := controller.Delete(ctx, desired); err != nil {
				test.Fatalf("delete restricted workload: %v", err)
			}
		})
	}
}

func verifyResourceWebhookAuditConfigMapBoundary(test *testing.T, ctx context.Context, admin client.Client, config *rest.Config, scheme *runtime.Scheme) {
	test.Helper()
	credential := rest.CopyConfig(config)
	credential.Impersonate = rest.ImpersonationConfig{
		UserName: "system:serviceaccount:control:security-test-kognition-webhook",
		Groups:   []string{"system:serviceaccounts", "system:serviceaccounts:control", "system:authenticated"},
	}
	webhook, err := client.New(credential, client.Options{Scheme: scheme})
	if err != nil {
		test.Fatal(err)
	}
	waitPermission := func(verb, namespace string) {
		test.Helper()
		if err := wait.PollUntilContextTimeout(ctx, 100*time.Millisecond, 10*time.Second, true, func(ctx context.Context) (bool, error) {
			review := &authorizationv1.SubjectAccessReview{Spec: authorizationv1.SubjectAccessReviewSpec{
				User: credential.Impersonate.UserName, Groups: credential.Impersonate.Groups,
				ResourceAttributes: &authorizationv1.ResourceAttributes{Namespace: namespace, Verb: verb, Resource: "configmaps"},
			}}
			if err := admin.Create(ctx, review); err != nil {
				return false, err
			}
			return review.Status.Allowed, nil
		}); err != nil {
			test.Fatalf("fixture ConfigMap %s permission in %s not ready: %v", verb, namespace, err)
		}
	}
	waitPermission("create", "control")
	retainCleanup := func(object client.Object) {
		test.Helper()
		uid := object.GetUID()
		test.Cleanup(func() {
			cleanupContext, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := admin.Delete(cleanupContext, object, client.Preconditions{UID: &uid}); err != nil {
				test.Errorf("remove exact audit-boundary fixture %T %s/%s: %v", object, object.GetNamespace(), object.GetName(), err)
			}
		})
	}
	var priorBatches corev1.ConfigMapList
	if err := webhook.List(ctx, &priorBatches, client.InNamespace("control"), client.MatchingLabels{"kognition.dev/cognitive-audit": "true"}); err != nil {
		test.Fatal(err)
	}
	priorUIDs := make(map[types.UID]bool, len(priorBatches.Items))
	for _, existing := range priorBatches.Items {
		priorUIDs[existing.UID] = true
	}
	auditStore, err := k8saudit.NewConfigMapStore(webhook, "control")
	if err != nil {
		test.Fatal(err)
	}
	entries := []observability.AuditEntry{
		{ID: "webhook-audit-boundary-first", Workspace: "tenant", SessionID: "webhook-audit-boundary", AgentIdentity: "spiffe://kognition.dev/tenant/first", EventType: observability.AuditMemoryOp, Verb: "store"},
		{ID: "webhook-audit-boundary-second", Workspace: "tenant", SessionID: "webhook-audit-boundary", AgentIdentity: "spiffe://kognition.dev/tenant/second", EventType: observability.AuditMemoryOp, Verb: "delete"},
	}
	for attempt := range 2 {
		applied, err := auditStore.ApplyAuditBatch(ctx, entries)
		if err != nil || len(applied) != len(entries) {
			test.Fatalf("production webhook audit batch: applied=%v err=%v", applied, err)
		}
		for _, created := range applied {
			if created != (attempt == 0) {
				test.Fatal("production audit deduplication changed", applied)
			}
		}
	}
	stored, err := auditStore.Query(ctx, observability.AuditFilter{Workspace: "tenant", SessionID: "webhook-audit-boundary", Limit: 10})
	if err != nil || len(stored) != len(entries) {
		test.Fatalf("production webhook audit read: entries=%v err=%v", stored, err)
	}
	var batches corev1.ConfigMapList
	if err := webhook.List(ctx, &batches, client.InNamespace("control"), client.MatchingLabels{"kognition.dev/cognitive-audit": "true"}); err != nil {
		test.Fatal(err)
	}
	var produced []corev1.ConfigMap
	for _, current := range batches.Items {
		if !priorUIDs[current.UID] {
			produced = append(produced, current)
		}
	}
	if len(produced) != 1 {
		test.Fatalf("production webhook audit fixture produced %d batches, want one", len(produced))
	}
	batch := produced[0]
	retainCleanup(&batch)
	test.Run("chart-rbac", func(test *testing.T) {
		for _, operation := range []func() error{
			func() error { return webhook.Update(ctx, batch.DeepCopy(), client.DryRunAll) },
			func() error {
				return webhook.Patch(ctx, batch.DeepCopy(), client.RawPatch(types.MergePatchType, []byte(`{"metadata":{"annotations":{"probe":"true"}}}`)), client.DryRunAll)
			},
			func() error { return webhook.Delete(ctx, batch.DeepCopy(), client.DryRunAll) },
		} {
			if err := operation(); !apierrors.IsForbidden(err) || isControllerPolicyDenial(err) {
				test.Fatalf("chart webhook mutation must be denied by RBAC: %v", err)
			}
		}
	})
	// Deliberately permissive fixture grants prove the independent admission boundary.
	for _, namespace := range []string{"control", "tenant", "outside"} {
		role := &rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Name: "webhook-audit-permissive-fixture", Namespace: namespace}, Rules: []rbacv1.PolicyRule{{APIGroups: []string{""}, Resources: []string{"configmaps"}, Verbs: []string{"get", "list", "create", "update", "patch", "delete"}}}}
		if err := admin.Create(ctx, role); err != nil {
			test.Fatal(err)
		}
		retainCleanup(role)
		binding := &rbacv1.RoleBinding{ObjectMeta: role.ObjectMeta, RoleRef: rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: role.Name}, Subjects: []rbacv1.Subject{{Kind: "ServiceAccount", Name: "security-test-kognition-webhook", Namespace: "control"}}}
		binding.ResourceVersion, binding.UID = "", ""
		if err := admin.Create(ctx, binding); err != nil {
			test.Fatal(err)
		}
		retainCleanup(binding)
		waitPermission("update", namespace)
	}
	prototype := batch.DeepCopy()
	prototype.ObjectMeta = metav1.ObjectMeta{Name: "kognition-audit-" + strings.Repeat("a", 32), Namespace: "control", Labels: batch.Labels}
	if err := webhook.Create(ctx, prototype.DeepCopy(), client.DryRunAll); err != nil {
		test.Fatal("valid audit-shaped creation denied", err)
	}
	maximum := prototype.DeepCopy()
	maximum.BinaryData["entries.json"] = []byte(strings.Repeat(" ", 900*1024))
	if err := webhook.Create(ctx, maximum, client.DryRunAll); err != nil {
		test.Fatal("900 KiB audit payload boundary denied", err)
	}
	for name, mutate := range map[string]func(*corev1.ConfigMap){
		"unrelated-name":    func(value *corev1.ConfigMap) { value.Name = "installation-settings" },
		"prefix-only":       func(value *corev1.ConfigMap) { value.Name = "kognition-audit-arbitrary" },
		"index-marker":      func(value *corev1.ConfigMap) { value.Name = "kognition-audit-index-" + strings.Repeat("a", 24) },
		"missing-immutable": func(value *corev1.ConfigMap) { value.Immutable = nil },
		"mutable":           func(value *corev1.ConfigMap) { value.Immutable = ptr.To(false) },
		"missing-labels":    func(value *corev1.ConfigMap) { value.Labels = nil },
		"false-audit-label": func(value *corev1.ConfigMap) { value.Labels["kognition.dev/cognitive-audit"] = "false" },
		"missing-workspace": func(value *corev1.ConfigMap) { delete(value.Labels, "kognition.dev/audit-workspace") },
		"invalid-workspace": func(value *corev1.ConfigMap) { value.Labels["kognition.dev/audit-workspace"] = "tenant" },
		"foreign-label":     func(value *corev1.ConfigMap) { value.Labels["kognition.dev/cost-record"] = "true" },
		"invalid-agent":     func(value *corev1.ConfigMap) { value.Labels["kognition.dev/audit-agent-invalid"] = "true" },
		"false-agent": func(value *corev1.ConfigMap) {
			value.Labels["kognition.dev/audit-agent-"+strings.Repeat("a", 24)] = "false"
		},
		"helm-managed":       func(value *corev1.ConfigMap) { value.Labels["app.kubernetes.io/managed-by"] = "Helm" },
		"helm-annotation":    func(value *corev1.ConfigMap) { value.Annotations = map[string]string{"meta.helm.sh/release-name": ""} },
		"foreign-annotation": func(value *corev1.ConfigMap) { value.Annotations = map[string]string{"configuration": "unrelated"} },
		"finalizer":          func(value *corev1.ConfigMap) { value.Finalizers = []string{"unrelated.example/retain"} },
		"owner": func(value *corev1.ConfigMap) {
			value.OwnerReferences = []metav1.OwnerReference{{APIVersion: "v1", Kind: "ConfigMap", Name: "unrelated", UID: "unrelated"}}
		},
		"plaintext-data": func(value *corev1.ConfigMap) {
			value.BinaryData = nil
			value.Data = map[string]string{"entries.json": "[]"}
		},
		"mixed-data":   func(value *corev1.ConfigMap) { value.Data = map[string]string{"configuration": "unrelated"} },
		"missing-data": func(value *corev1.ConfigMap) { value.BinaryData = nil },
		"empty-batch":  func(value *corev1.ConfigMap) { value.BinaryData["entries.json"] = nil },
		"foreign-data": func(value *corev1.ConfigMap) { value.BinaryData["configuration"] = []byte("unrelated") },
		"oversized": func(value *corev1.ConfigMap) {
			value.BinaryData["entries.json"] = []byte(strings.Repeat(" ", 900*1024+1))
		},
		"tenant-scope":  func(value *corev1.ConfigMap) { value.Namespace = "tenant" },
		"outside-scope": func(value *corev1.ConfigMap) { value.Namespace = "outside" },
	} {
		test.Run("independent/"+name, func(test *testing.T) {
			value := prototype.DeepCopy()
			mutate(value)
			if err := webhook.Create(ctx, value, client.DryRunAll); !isControllerPolicyDenial(err) {
				test.Errorf("native audit-only creation boundary: want policy denial, got %v", err)
			}
		})
	}
	test.Run("independent/mutation", func(test *testing.T) {
		for _, operation := range []func() error{
			func() error { return webhook.Update(ctx, batch.DeepCopy(), client.DryRunAll) },
			func() error {
				return webhook.Patch(ctx, batch.DeepCopy(), client.RawPatch(types.MergePatchType, []byte(`{"metadata":{"annotations":{"probe":"true"}}}`)), client.DryRunAll)
			},
			func() error { return webhook.Delete(ctx, batch.DeepCopy(), client.DryRunAll) },
		} {
			if err := operation(); !isControllerPolicyDenial(err) {
				test.Errorf("native audit immutability boundary: want policy denial, got %v", err)
			}
		}
	})
}

func isControllerPolicyDenial(err error) bool {
	return apierrors.IsForbidden(err) && strings.Contains(err.Error(), "ValidatingAdmissionPolicy 'security-test-kognition-control-")
}

func verifyNativeMetadataTransitions(test *testing.T, ctx context.Context, admin, controller client.Client, original client.Object, patchGranted, deleteGranted bool) {
	test.Helper()
	replaced := original.DeepCopyObject().(client.Object)
	replaced.SetUID("replacement-uid")
	if err := controller.Update(ctx, replaced, client.DryRunAll); !apierrors.IsInvalid(err) && !apierrors.IsConflict(err) {
		test.Errorf("UID replacement of %s: want Kubernetes identity denial, got %v", original.GetName(), err)
	}
	if patchGranted {
		patch := client.RawPatch(types.MergePatchType, []byte(`{"metadata":{"uid":"replacement-uid"}}`))
		if err := controller.Patch(ctx, original.DeepCopyObject().(client.Object), patch, client.DryRunAll); !apierrors.IsInvalid(err) && !apierrors.IsConflict(err) {
			test.Errorf("UID patch of %s: want Kubernetes identity denial, got %v", original.GetName(), err)
		}
	}
	for name, mutate := range map[string]func(client.Object){
		"remove-ownership": func(object client.Object) { object.SetLabels(map[string]string{"matrix": "unowned"}) },
		"helm-release": func(object client.Object) {
			object.SetAnnotations(map[string]string{"meta.helm.sh/release-name": "other"})
		},
		"helm-namespace": func(object client.Object) {
			object.SetAnnotations(map[string]string{"meta.helm.sh/release-namespace": "other"})
		},
		"empty-helm-release": func(object client.Object) { object.SetAnnotations(map[string]string{"meta.helm.sh/release-name": ""}) },
		"empty-helm-namespace": func(object client.Object) {
			object.SetAnnotations(map[string]string{"meta.helm.sh/release-namespace": ""})
		},
	} {
		test.Run(original.GetName()+"/metadata/"+name, func(test *testing.T) {
			forged := original.DeepCopyObject().(client.Object)
			mutate(forged)
			if err := controller.Update(ctx, forged.DeepCopyObject().(client.Object), client.DryRunAll); !isControllerPolicyDenial(err) {
				test.Errorf("new-object update: want policy denial, got %v", err)
			}
			if patchGranted {
				if err := controller.Patch(ctx, forged.DeepCopyObject().(client.Object), client.MergeFrom(original), client.DryRunAll); !isControllerPolicyDenial(err) {
					test.Errorf("new-object merge patch: want policy denial, got %v", err)
				}
			}
			if err := admin.Update(ctx, forged); err != nil {
				test.Fatal(err)
			}
			adopted := original.DeepCopyObject().(client.Object)
			adopted.SetResourceVersion(forged.GetResourceVersion())
			if err := controller.Update(ctx, adopted.DeepCopyObject().(client.Object), client.DryRunAll); !isControllerPolicyDenial(err) {
				test.Errorf("old-object adoption: want policy denial, got %v", err)
			}
			if patchGranted {
				if err := controller.Patch(ctx, adopted.DeepCopyObject().(client.Object), client.MergeFrom(forged), client.DryRunAll); !isControllerPolicyDenial(err) {
					test.Errorf("old-object patch adoption: want policy denial, got %v", err)
				}
			}
			err := controller.Delete(ctx, forged, client.DryRunAll)
			if deleteGranted && !isControllerPolicyDenial(err) || !deleteGranted && (!apierrors.IsForbidden(err) || isControllerPolicyDenial(err)) {
				test.Errorf("old-object deletion: unexpected enforcement result %v", err)
			}
			if err := admin.Update(ctx, adopted); err != nil {
				test.Fatal(err)
			}
			if err := admin.Get(ctx, client.ObjectKeyFromObject(original), original); err != nil {
				test.Fatal(err)
			}
		})
	}
	for _, patch := range []client.Patch{
		client.RawPatch(types.MergePatchType, []byte(`{"metadata":{"annotations":{"matrix":"allowed"}}}`)),
		client.RawPatch(types.JSONPatchType, []byte(`[{"op":"add","path":"/metadata/annotations","value":{"matrix":"allowed"}}]`)),
	} {
		err := controller.Patch(ctx, original.DeepCopyObject().(client.Object), patch, client.DryRunAll)
		if patchGranted && err != nil || !patchGranted && (!apierrors.IsForbidden(err) || isControllerPolicyDenial(err)) {
			test.Errorf("benign patch of %s: unexpected enforcement result %v", original.GetName(), err)
		}
	}
	if patchGranted {
		patch := client.RawPatch(types.JSONPatchType, []byte(`[{"op":"replace","path":"/metadata/labels","value":{"matrix":"unowned"}}]`))
		if err := controller.Patch(ctx, original.DeepCopyObject().(client.Object), patch, client.DryRunAll); !isControllerPolicyDenial(err) {
			test.Errorf("JSON patch ownership removal: want policy denial, got %v", err)
		}
	}
}

func verifyControllerNetworkAdmission(test *testing.T, ctx context.Context, admin, controller client.Client) {
	test.Helper()
	labels := map[string]string{"app.kubernetes.io/managed-by": "kognition", "kognition.dev/agent": "researcher"}
	service := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "agent-researcher", Namespace: "tenant", Labels: labels},
		Spec:       corev1.ServiceSpec{Selector: labels, Ports: []corev1.ServicePort{{Port: 8080}}, Type: corev1.ServiceTypeClusterIP},
	}
	policy := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "agent-researcher-network", Namespace: "tenant", Labels: labels},
		Spec:       networkingv1.NetworkPolicySpec{PodSelector: metav1.LabelSelector{MatchLabels: labels}, PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress}},
	}
	for _, desired := range []client.Object{service, policy} {
		test.Run(desired.GetName(), func(test *testing.T) {
			unowned := desired.DeepCopyObject().(client.Object)
			unowned.SetLabels(nil)
			if err := admin.Create(ctx, unowned); err != nil {
				test.Fatal(err)
			}
			forged := unowned.DeepCopyObject().(client.Object)
			forged.SetLabels(labels)
			if err := controller.Update(ctx, forged); !isControllerPolicyDenial(err) {
				test.Fatalf("adopt unrelated network object: got %v, want policy denial", err)
			}
			if err := controller.Delete(ctx, unowned); !isControllerPolicyDenial(err) {
				test.Fatalf("delete unrelated network object: got %v, want policy denial", err)
			}
			if err := admin.Delete(ctx, unowned); err != nil {
				test.Fatal(err)
			}
			if err := controller.Create(ctx, desired); err != nil {
				test.Fatalf("create managed network object: %v", err)
			}
			verifyNativeMetadataTransitions(test, ctx, admin, controller, desired, true, true)
			wide := desired.DeepCopyObject().(client.Object)
			switch object := wide.(type) {
			case *corev1.Service:
				object.Spec.Selector = map[string]string{"app": "unrelated"}
			case *networkingv1.NetworkPolicy:
				object.Spec.PodSelector = metav1.LabelSelector{}
			}
			if err := controller.Update(ctx, wide); !isControllerPolicyDenial(err) {
				test.Fatalf("widen selector: got %v, want policy denial", err)
			}
			desired.SetAnnotations(map[string]string{"test": "updated"})
			if err := controller.Update(ctx, desired); err != nil {
				test.Fatalf("update managed network object: %v", err)
			}
			if err := controller.Delete(ctx, desired); err != nil {
				test.Fatalf("delete managed network object: %v", err)
			}
		})
	}
}

func verifyControllerNetworkCompatibility(test *testing.T, ctx context.Context, controller client.Client) {
	test.Helper()
	for _, identity := range []string{"kognition.dev/exec-probe", "kognition.dev/stdio-invocation"} {
		test.Run(identity, func(test *testing.T) {
			labels := map[string]string{"app.kubernetes.io/managed-by": "kognition", identity: "invocation"}
			policy := &networkingv1.NetworkPolicy{
				ObjectMeta: metav1.ObjectMeta{GenerateName: "sandbox-", Namespace: "tenant", Labels: labels},
				Spec:       networkingv1.NetworkPolicySpec{PodSelector: metav1.LabelSelector{MatchLabels: labels}, PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress}},
			}
			if err := controller.Create(ctx, policy); err != nil {
				test.Fatal(err)
			}
			opened := policy.DeepCopy()
			opened.Spec.Egress = []networkingv1.NetworkPolicyEgressRule{{}}
			if err := controller.Update(ctx, opened); !isControllerPolicyDenial(err) {
				test.Fatalf("open sandbox egress: got %v, want policy denial", err)
			}
			if err := controller.Delete(ctx, policy); err != nil {
				test.Fatalf("clean up sandbox policy: %v", err)
			}
			mixed := &networkingv1.NetworkPolicy{
				ObjectMeta: metav1.ObjectMeta{GenerateName: "mixed-sandbox-", Namespace: "tenant", Labels: map[string]string{"app.kubernetes.io/managed-by": "kognition", identity: "invocation", "kognition.dev/agent": "researcher"}},
				Spec:       networkingv1.NetworkPolicySpec{PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{"app.kubernetes.io/managed-by": "kognition", identity: "invocation", "kognition.dev/agent": "researcher"}}, PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress}},
			}
			if err := controller.Create(ctx, mixed); err != nil {
				test.Fatal(err)
			}
			opened = mixed.DeepCopy()
			delete(opened.Labels, identity)
			opened.Spec.Egress = []networkingv1.NetworkPolicyEgressRule{{}}
			if err := controller.Update(ctx, opened, client.DryRunAll); !isControllerPolicyDenial(err) {
				test.Errorf("reclassify sandbox to open egress: want policy denial, got %v", err)
			}
			if err := controller.Patch(ctx, opened, client.MergeFrom(mixed), client.DryRunAll); !isControllerPolicyDenial(err) {
				test.Errorf("patch sandbox classification: want policy denial, got %v", err)
			}
			opened.Name = ""
			opened.ResourceVersion = ""
			opened.UID = ""
			if err := controller.Create(ctx, opened, client.DryRunAll); !isControllerPolicyDenial(err) {
				test.Errorf("create mislabeled sandbox policy: want policy denial, got %v", err)
			}
			if err := controller.Delete(ctx, mixed); err != nil {
				test.Fatal(err)
			}
		})
	}
	service := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "published-agent", Namespace: "tenant", Labels: map[string]string{"app.kubernetes.io/managed-by": "kognition", "kognition.dev/agent-service": "published-agent", "kognition.dev/agent-name": "researcher", "kognition.dev/agent-namespace": "control"}},
		Spec: corev1.ServiceSpec{
			Type: corev1.ServiceTypeClusterIP, Ports: []corev1.ServicePort{{Port: 8080}},
			Selector: map[string]string{"app.kubernetes.io/managed-by": "kognition", "kognition.dev/agent-name": "researcher", "kognition.dev/agent-namespace": "control"},
		},
	}
	if err := controller.Create(ctx, service); err != nil {
		test.Fatalf("publish unpinned Service: %v", err)
	}
	for _, mutate := range []func(*corev1.Service){
		func(service *corev1.Service) { service.Spec.Selector["kognition.dev/agent-name"] = "another-agent" },
		func(service *corev1.Service) { delete(service.Labels, "kognition.dev/agent-name") },
		func(service *corev1.Service) { service.Spec.ExternalIPs = []string{"192.0.2.1"} },
		func(service *corev1.Service) { service.Spec.Type = corev1.ServiceTypeNodePort },
		func(service *corev1.Service) { service.Spec.Type = corev1.ServiceTypeLoadBalancer },
		func(service *corev1.Service) { service.Labels = nil },
		func(service *corev1.Service) {
			service.Annotations = map[string]string{"meta.helm.sh/release-name": "other"}
		},
	} {
		forged := service.DeepCopy()
		mutate(forged)
		if err := controller.Update(ctx, forged); !isControllerPolicyDenial(err) {
			test.Fatalf("unsafe published Service update: got %v, want policy denial", err)
		}
	}
	for _, revision := range []string{"revision-one", "revision-two"} {
		service.Spec.Selector = map[string]string{"app.kubernetes.io/managed-by": "kognition", "kognition.dev/agent-service-workload": revision, "kognition.dev/agent-service-revision": revision}
		service.Labels = map[string]string{"kognition.dev/agent-service": "published-agent"}
		for key, value := range service.Spec.Selector {
			service.Labels[key] = value
		}
		if err := controller.Update(ctx, service); err != nil {
			test.Fatalf("publish revision %s: %v", revision, err)
		}
	}
	if err := controller.Delete(ctx, service); err != nil {
		test.Fatalf("delete published Service: %v", err)
	}
}
