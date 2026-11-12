//go:build integration

// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package traceability

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	k8saudit "github.com/kognition-ai/kognition/internal/k8s/audit"
	k8scontroller "github.com/kognition-ai/kognition/internal/k8s/controller"
	k8scost "github.com/kognition-ai/kognition/internal/k8s/cost"
	"github.com/kognition-ai/kognition/internal/observability"
	"github.com/kognition-ai/kognition/internal/store"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func verifyGatewayConfigMapBoundary(test *testing.T, ctx context.Context, admin client.Client, config *rest.Config, scheme *runtime.Scheme, rendered corev1.ConfigMap) {
	test.Helper()
	migrations := *rendered.DeepCopy()
	if migrations.Name != authorityProductionRelease+"-migrations" || migrations.Namespace != "control" || len(migrations.Data) == 0 {
		test.Fatal("the actual rendered migration ConfigMap is required")
	}
	if err := admin.Create(ctx, &migrations); err != nil {
		test.Fatal(err)
	}
	test.Cleanup(func() {
		if err := admin.Delete(context.Background(), &migrations, client.Preconditions{UID: &migrations.UID}); err != nil {
			test.Errorf("remove exact migration fixture: %v", err)
		}
	})
	for _, component := range []string{"tool-gateway", "memory-policy-gateway", "agent-communication-gateway", "model-gateway"} {
		test.Run(component, func(test *testing.T) {
			account := authorityProductionRelease + "-" + component
			credential := rest.CopyConfig(config)
			credential.Impersonate = rest.ImpersonationConfig{UserName: "system:serviceaccount:control:" + account,
				Groups: []string{"system:serviceaccounts", "system:serviceaccounts:control", "system:authenticated"}}
			gateway, err := client.New(credential, client.Options{Scheme: scheme})
			if err != nil {
				test.Fatal(err)
			}
			var prior corev1.ConfigMapList
			if err := admin.List(ctx, &prior, client.InNamespace("control")); err != nil {
				test.Fatal(err)
			}
			before := map[types.UID]bool{}
			for _, object := range prior.Items {
				before[object.UID] = true
			}
			test.Cleanup(func() {
				cleanupContext, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				var current corev1.ConfigMapList
				if err := admin.List(cleanupContext, &current, client.InNamespace("control")); err != nil {
					test.Errorf("inventory gateway fixture outputs: %v", err)
					return
				}
				for _, object := range current.Items {
					if !before[object.UID] {
						if err := admin.Delete(cleanupContext, &object, client.Preconditions{UID: &object.UID}); err != nil {
							test.Errorf("remove exact gateway output: %v", err)
						}
					}
				}
			})
			for _, namespace := range []string{"control", "tenant", "outside"} {
				role := &rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Name: "gateway-state-fixture-" + component, Namespace: namespace},
					Rules: []rbacv1.PolicyRule{
						{APIGroups: []string{""}, Resources: []string{"configmaps"}, Verbs: []string{"get", "list", "create", "update", "patch", "delete"}},
						{APIGroups: []string{"coordination.k8s.io"}, Resources: []string{"leases"}, Verbs: []string{"get", "create", "update", "delete"}},
					}}
				if err := admin.Create(ctx, role); err != nil {
					test.Fatal(err)
				}
				binding := &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: role.Name, Namespace: namespace},
					RoleRef:  rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: role.Name},
					Subjects: []rbacv1.Subject{{Kind: "ServiceAccount", Name: account, Namespace: "control"}}}
				if err := admin.Create(ctx, binding); err != nil {
					test.Fatal(err)
				}
				test.Cleanup(func() {
					for _, object := range []client.Object{binding, role} {
						uid := object.GetUID()
						if err := admin.Delete(context.Background(), object, client.Preconditions{UID: &uid}); err != nil {
							test.Errorf("remove exact gateway fixture: %v", err)
						}
					}
				})
			}
			modified := migrations.DeepCopy()
			modified.Data["999_boundary_probe.sql"] = "SELECT 1;"
			for name, operation := range map[string]func() error{
				"migration-update": func() error { return gateway.Update(ctx, modified.DeepCopy(), client.DryRunAll) },
				"migration-patch": func() error {
					return gateway.Patch(ctx, modified.DeepCopy(), client.MergeFrom(&migrations), client.DryRunAll)
				},
				"migration-delete": func() error { return gateway.Delete(ctx, migrations.DeepCopy(), client.DryRunAll) },
				"unrelated-create": func() error {
					return gateway.Create(ctx, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "unrelated-settings", Namespace: "control"}}, client.DryRunAll)
				},
				"tenant-create": func() error {
					return gateway.Create(ctx, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "unrelated-settings", Namespace: "tenant"}}, client.DryRunAll)
				},
				"outside-create": func() error {
					return gateway.Create(ctx, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "unrelated-settings", Namespace: "outside"}}, client.DryRunAll)
				},
			} {
				test.Run(name, func(test *testing.T) {
					if err := operation(); !isControllerPolicyDenial(err) {
						test.Errorf("independent gateway state boundary must deny migration/unrelated writes: %v", err)
					}
				})
			}
			audits, err := k8saudit.NewConfigMapStore(gateway, "control")
			if err != nil {
				test.Fatal(err)
			}
			entry := observability.AuditEntry{ID: "state-boundary-" + component, Workspace: "tenant", EventType: observability.AuditToolCall, Verb: "invoke"}
			for attempt := range 2 {
				applied, err := audits.ApplyAuditEntry(ctx, entry)
				if err != nil || applied != (attempt == 0) {
					test.Fatalf("real gateway audit append/dedup failed: applied=%t err=%v", applied, err)
				}
			}
			digest := sha256.Sum256([]byte("tenant"))
			legacyReceipt := store.ToolInvocationReceipt{Namespace: "tenant", InvocationID: "retained-native-boundary", Binding: component, Status: store.ToolInvocationPending}
			payload, err := json.Marshal(map[string]any{"receipts": map[string]store.ToolInvocationReceipt{legacyReceipt.InvocationID: legacyReceipt}})
			if err != nil {
				test.Fatal(err)
			}
			legacy := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("kognition-tool-invocations-%x", digest), Namespace: "control",
				Labels: map[string]string{"kognition.dev/tool-invocation-ledger": "true"}, Annotations: map[string]string{"kognition.dev/workload-namespace": "tenant"}}, Data: map[string]string{"receipts.json": string(payload)}}
			if err := admin.Create(ctx, legacy); err != nil {
				test.Fatal(err)
			}
			if err := gateway.Update(ctx, legacy.DeepCopy(), client.DryRunAll); !isControllerPolicyDenial(err) {
				test.Fatal("legacy ledger was not independently frozen", err)
			}
			ledger := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
				Name: fmt.Sprintf("kognition-tool-invocations-%s-%x", component, digest), Namespace: "control",
				Labels: map[string]string{"kognition.dev/tool-invocation-ledger": "true"}, Annotations: map[string]string{"kognition.dev/workload-namespace": "tenant"},
			}, Data: map[string]string{"receipts.json": `{"receipts":{}}`}}
			mutable := component == "tool-gateway" || component == "agent-communication-gateway" || component == "model-gateway"
			if component == "model-gateway" {
				ledger.Name = fmt.Sprintf("kognition-token-reservations-%x", digest)
				ledger.Labels = map[string]string{"kognition.dev/token-ledger": "true"}
				ledger.Annotations = map[string]string{"kognition.dev/quota-namespace": "tenant"}
				ledger.Data = map[string]string{"ledger.json": `{"reservations":{},"quotas":{}}`}
			}
			if component == "tool-gateway" || component == "agent-communication-gateway" {
				adapter := k8scontroller.NewStoreAdapterWithControlNamespace(gateway, "control")
				if err := adapter.ConfigureToolInvocationReceiptWriter(component); err != nil {
					test.Fatal(err)
				}
				if created, _, err := adapter.ClaimToolInvocation(ctx, legacyReceipt); created || !errors.Is(err, store.ErrConflict) {
					test.Fatal("unresolved retained native invocation became executable", err)
				}
				receipt := store.ToolInvocationReceipt{Namespace: "tenant", InvocationID: "isolated-ledger-boundary", Binding: component, Status: store.ToolInvocationPending}
				if created, _, err := adapter.ClaimToolInvocation(ctx, receipt); !created || err != nil {
					test.Fatalf("real isolated gateway receipt create=%t err=%v", created, err)
				}
				now := time.Now().UTC()
				receipt.Status, receipt.CompletedAt = store.ToolInvocationSucceeded, &now
				if err := adapter.CompleteToolInvocation(ctx, receipt); err != nil {
					test.Fatal("real gateway receipt completion", err)
				}
				if created, result, err := adapter.ClaimToolInvocation(ctx, receipt); created || err != nil || result.Status != store.ToolInvocationSucceeded {
					test.Fatal("real gateway receipt replay crossed its owner", err)
				}
				if err := admin.Get(ctx, client.ObjectKeyFromObject(ledger), ledger); err != nil {
					test.Fatal(err)
				}
			} else if mutable {
				if err := gateway.Create(ctx, ledger); err != nil {
					test.Fatal("model token ledger create", err)
				}
				ledger.Data["ledger.json"] = `{"reservations":{},"quotas":{"fixture":{}}}`
				if err := gateway.Update(ctx, ledger); err != nil {
					test.Fatal("model token ledger update", err)
				}
			}
			for name, mutate := range map[string]func(*corev1.ConfigMap){
				"foreign-plane": func(value *corev1.ConfigMap) {
					peer := "tool-gateway"
					if component == peer {
						peer = "agent-communication-gateway"
					}
					value.Name = fmt.Sprintf("kognition-tool-invocations-%s-%x", peer, digest)
				},
				"legacy-shared": func(value *corev1.ConfigMap) { value.Name = fmt.Sprintf("kognition-tool-invocations-%x", digest) },
				"unowned":       func(value *corev1.ConfigMap) { value.Labels = nil },
				"foreign-namespace-annotation": func(value *corev1.ConfigMap) {
					for key := range value.Annotations {
						value.Annotations[key] = "outside"
					}
				},
				"foreign-payload": func(value *corev1.ConfigMap) { value.Data["other-config"] = "unrelated" },
				"helm-owned":      func(value *corev1.ConfigMap) { value.Labels["app.kubernetes.io/managed-by"] = "Helm" },
				"helm-adoption":   func(value *corev1.ConfigMap) { value.Annotations["meta.helm.sh/release-name"] = "security-test" },
				"tenant-ledger":   func(value *corev1.ConfigMap) { value.Namespace = "tenant" },
			} {
				test.Run(name, func(test *testing.T) {
					value := ledger.DeepCopy()
					value.ResourceVersion, value.UID = "", ""
					mutate(value)
					if err := gateway.Create(ctx, value, client.DryRunAll); !isControllerPolicyDenial(err) {
						test.Errorf("foreign gateway state must be denied independently: %v", err)
					}
				})
			}
			if mutable {
				for name, operation := range map[string]func() error{
					"delete-ledger": func() error { return gateway.Delete(ctx, ledger.DeepCopy(), client.DryRunAll) },
					"adopt-unowned-ledger": func() error {
						original := ledger.DeepCopy()
						unowned := ledger.DeepCopy()
						unowned.Labels = nil
						if err := admin.Update(ctx, unowned); err != nil {
							return err
						}
						original.ResourceVersion = unowned.ResourceVersion
						err := gateway.Update(ctx, original, client.DryRunAll)
						if restoreErr := admin.Update(ctx, original); restoreErr != nil {
							test.Fatal(restoreErr)
						}
						return err
					},
				} {
					test.Run(name, func(test *testing.T) {
						if err := operation(); !isControllerPolicyDenial(err) {
							test.Errorf("existing foreign/retained ledger mutation escaped admission: %v", err)
						}
						if err := admin.Get(ctx, client.ObjectKeyFromObject(ledger), ledger); err != nil {
							test.Fatal(err)
						}
					})
				}
			}
			if component == "tool-gateway" || component == "model-gateway" {
				costs, err := k8scost.NewConfigMapStore(gateway, "control")
				if err != nil {
					test.Fatal(err)
				}
				if err := costs.RecordCost(ctx, observability.CostRecord{Workspace: "tenant", OperationType: observability.CostOpToolExecution, TraceID: component, SpanID: "native-boundary"}); err != nil {
					test.Fatal("production cost append denied", err)
				}
			}
			if component == "model-gateway" {
				for _, name := range []string{"unrelated-lock", "kognition-runtime-approval-binding-keys", "kognition-runtime-approval-records", "kognition-tool-grant-rate-foreign"} {
					value := &coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "control", Labels: map[string]string{"app.kubernetes.io/managed-by": "kognition"}}}
					if err := gateway.Create(ctx, value, client.DryRunAll); !isControllerPolicyDenial(err) {
						test.Fatalf("model accounting authority escaped its usage lock: %v", err)
					}
				}
				lock := &coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{Name: "kognition-usage-lock-" + strings.Repeat("a", 24), Namespace: "control", Labels: map[string]string{"app.kubernetes.io/managed-by": "kognition"}}}
				if err := gateway.Create(ctx, lock, client.DryRunAll); err != nil {
					test.Fatal("model usage lock denied", err)
				}
				usage := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "kognition-usage-receipt-" + strings.Repeat("a", 24), Namespace: "control",
					Labels:      map[string]string{"kognition.dev/usage-receipt": "true", "kognition.dev/cost-workspace": strings.Repeat("a", 24)},
					Annotations: map[string]string{"kognition.dev/workspace": "tenant"}}, Immutable: ptr.To(true), Data: map[string]string{"fingerprint": strings.Repeat("b", 64)}}
				if err := gateway.Create(ctx, usage); err != nil {
					test.Fatal("immutable model usage receipt denied", err)
				}
				if err := gateway.Update(ctx, usage.DeepCopy(), client.DryRunAll); !isControllerPolicyDenial(err) {
					test.Fatal("model usage receipt rewrite allowed", err)
				}
			}
		})
	}
}
