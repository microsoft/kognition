// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package activation

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	authv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestApprovalAuthority(test *testing.T) {
	for _, scenario := range []string{"valid", "mutable", "controller-write", "named-write", "archive-write", "wrong-authority", "indeterminate", "workload-namespace", "unknown-field", "trailing-json"} {
		test.Run(scenario, func(test *testing.T) {
			encoded, _ := json.Marshal(approvalFixture(time.Now()))
			record := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "approved", Namespace: "authority", UID: "approval-uid"}, Immutable: ptr.To(true), Data: map[string]string{"approval.json": string(encoded)}}
			switch scenario {
			case "mutable":
				record.Immutable = ptr.To(false)
			case "workload-namespace":
				record.Namespace = "tenant"
			case "wrong-authority":
				record.Namespace = "other-authority"
			case "unknown-field":
				record.Data["approval.json"] = `{"unexpected":true}`
			case "trailing-json":
				record.Data["approval.json"] += `{}`
			}
			key := installationKey("authority", approvalFixture(time.Now()).Target)
			journal := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: key.Namespace, Name: key.Name, UID: "journal-uid"}}
			admin := fake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(record, journal).WithInterceptorFuncs(interceptor.Funcs{Create: func(ctx context.Context, delegate client.WithWatch, object client.Object, options ...client.CreateOption) error {
				review, ok := object.(*authv1.SubjectAccessReview)
				if !ok {
					test.Fatalf("unexpected write %T", object)
				}
				if review.Spec.User != "system:serviceaccount:control:controller" || len(review.Spec.Groups) != 3 {
					test.Fatal("incorrect controller identity")
				}
				review.Status.Allowed = scenario == "controller-write"
				if scenario == "named-write" && review.Spec.ResourceAttributes.Name == "approved" {
					review.Status.Allowed = true
				}
				if scenario == "archive-write" && review.Spec.ResourceAttributes.Name == "activation-history-journal-uid" {
					review.Status.Allowed = true
				}
				if scenario == "indeterminate" {
					review.Status.EvaluationError = "authorizer unavailable"
				}
				return nil
			}}).Build()
			_, _, err := readApproval(context.Background(), admin, client.ObjectKeyFromObject(record), Manifest{Namespace: "control", Controller: "controller", Namespaces: []string{"tenant"}})
			if (err == nil) != (scenario == "valid") {
				test.Fatalf("unexpected result: %v", err)
			}
		})
	}
}
