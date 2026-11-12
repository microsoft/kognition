// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package activation

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	authv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestRecoveryApprovalMatchesPersistedInstantsWithoutAcceptingChanges(test *testing.T) {
	for _, scenario := range []string{"same-instants", "changed-approver", "changed-issued", "changed-expiry", "changed-gate-time", "changed-report", "changed-test-version", "removed-gate", "extra-gate", "changed-target"} {
		test.Run(scenario, func(test *testing.T) {
			approval := approvalFixture(time.Now().UTC())
			journalApproval := approvalFixture(approval.IssuedAt.Add(time.Minute))
			switch scenario {
			case "changed-approver":
				journalApproval.Approver = "other"
			case "changed-issued":
				journalApproval.IssuedAt = journalApproval.IssuedAt.Add(time.Nanosecond)
			case "changed-expiry":
				journalApproval.ExpiresAt = journalApproval.ExpiresAt.Add(time.Nanosecond)
			case "changed-gate-time", "changed-report", "changed-test-version":
				gate := journalApproval.Gates["SG-03"]
				if scenario == "changed-gate-time" {
					gate.AcceptedAt = gate.AcceptedAt.Add(time.Nanosecond)
				} else if scenario == "changed-report" {
					gate.ReportSHA256 = strings.Repeat("b", 64)
				} else {
					gate.TestVersion = "other"
				}
				journalApproval.Gates["SG-03"] = gate
			case "removed-gate":
				delete(journalApproval.Gates, "SG-03")
			case "extra-gate":
				journalApproval.Gates["other"] = journalApproval.Gates["SG-03"]
			case "changed-target":
				journalApproval.Target.Objects[0].UID = "replacement"
			}
			encoded, err := json.Marshal(approval)
			if err != nil {
				test.Fatal(err)
			}
			record := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "approved", Namespace: "authority", UID: "approval-uid"}, Immutable: ptr.To(true), Data: map[string]string{"approval.json": strings.ReplaceAll(string(encoded), "Z\"", "+00:00\"")}}
			encoded, err = json.Marshal(Audit{ApprovalUID: string(record.UID), Approval: journalApproval, ResourceJournalVersion: 1, Stage: "active"})
			if err != nil {
				test.Fatal(err)
			}
			key := installationKey(record.Namespace, approval.Target)
			journal := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace, UID: "journal-uid"}, Data: map[string]string{"audit.json": string(encoded)}}
			checkpoint := errors.New("reviewed recovery stage reached")
			updates := 0
			admin := fake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(record, journal, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "kube-system", UID: "cluster-uid"}}).WithInterceptorFuncs(interceptor.Funcs{
				Create: func(ctx context.Context, underlying client.WithWatch, object client.Object, options ...client.CreateOption) error {
					if _, ok := object.(*authv1.SubjectAccessReview); ok {
						return nil
					}
					return underlying.Create(ctx, object, options...)
				},
				Update: func(context.Context, client.WithWatch, client.Object, ...client.UpdateOption) error {
					updates++
					return checkpoint
				},
			}).Build()
			candidate := Candidate{sha256: approval.Target.ManifestSHA256, release: approval.Target.Release, manifest: Manifest{Namespace: "control", Controller: "controller", Deployment: "controller-deployment", Guard: "guard", Namespaces: []string{"tenant"}}, controller: &unstructured.Unstructured{}}
			options := ActivationOptions{Approval: client.ObjectKeyFromObject(record), Endpoint: approval.Target.Endpoint, ServerVersion: approval.Target.ServerVersion, ChartSHA256: approval.Target.ChartSHA256, ToolVersion: approval.Target.ToolVersion, Replicas: 1, WritersFrozen: true}
			_, err = candidate.Recover(context.Background(), admin, options, true)
			if scenario == "same-instants" {
				if !errors.Is(err, checkpoint) || updates != 1 {
					test.Fatalf("same approval instant rejected after journal reload: updates=%d error=%v", updates, err)
				}
			} else if err == nil || errors.Is(err, checkpoint) || updates != 0 {
				test.Fatalf("changed approval authorized recovery: updates=%d error=%v", updates, err)
			}
		})
	}
}

func TestDeactivatePreservesForeignBindings(test *testing.T) {
	for _, scenario := range []string{"replaced", "unowned", "owner-reference"} {
		test.Run(scenario, func(test *testing.T) {
			candidate := Candidate{manifest: Manifest{Namespace: "control", Controller: "controller"}, controller: &unstructured.Unstructured{}, roles: []*unstructured.Unstructured{{Object: map[string]interface{}{"kind": "Role", "metadata": map[string]interface{}{"name": "controller", "namespace": "control"}}}}}
			candidate.controller.SetName("controller")
			binding := candidate.bindings("journal")[0]
			binding.SetUID("original")
			audit := Audit{Bindings: map[string]string{bindingKey(binding): "original"}}
			encoded, err := json.Marshal(audit)
			if err != nil {
				test.Fatal(err)
			}
			journal := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{UID: "journal"}, Data: map[string]string{"audit.json": string(encoded)}}
			switch scenario {
			case "replaced":
				binding.SetUID("replacement")
			case "unowned":
				binding.SetAnnotations(nil)
			case "owner-reference":
				binding.SetOwnerReferences([]metav1.OwnerReference{{UID: "foreign"}})
			}
			admin := fake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(binding).Build()
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			err = candidate.deactivate(ctx, admin, journal, ActivationTarget{})
			if err == nil || !strings.Contains(err.Error(), "refusing to delete") {
				test.Fatalf("foreign binding not rejected: %v", err)
			}
			if err := admin.Get(context.Background(), client.ObjectKeyFromObject(binding), binding); err != nil {
				test.Fatalf("foreign binding removed: %v", err)
			}
		})
	}
}
