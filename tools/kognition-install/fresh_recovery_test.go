// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"context"
	"testing"

	"github.com/kognition-ai/kognition/internal/k8s/activation"
	appsv1 "k8s.io/api/apps/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestVerifyFreshNeverActivated(t *testing.T) {
	manifest := activation.Manifest{Namespace: "control", Deployment: "controller"}
	controller := func(replicas int32, running int32) *appsv1.Deployment {
		return &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Namespace: "control", Name: "controller"},
			Spec:       appsv1.DeploymentSpec{Replicas: ptr.To(replicas)},
			Status:     appsv1.DeploymentStatus{Replicas: running},
		}
	}
	grant := &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "grant", Annotations: map[string]string{activationTransactionAnnotation: "uid"}}}
	roleGrant := &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Namespace: "tenant", Name: "grant", Annotations: map[string]string{activationTransactionAnnotation: "uid"}}}
	for name, test := range map[string]struct {
		objects []client.Object
		wantErr bool
	}{
		"controller never created":       {},
		"controller stopped":             {objects: []client.Object{controller(0, 0)}},
		"controller scaled up":           {objects: []client.Object{controller(1, 0)}, wantErr: true},
		"controller pods still running":  {objects: []client.Object{controller(0, 1)}, wantErr: true},
		"activation ClusterRoleBinding":  {objects: []client.Object{controller(0, 0), grant}, wantErr: true},
		"activation RoleBinding present": {objects: []client.Object{roleGrant}, wantErr: true},
	} {
		t.Run(name, func(t *testing.T) {
			admin := fake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(test.objects...).Build()
			if err := verifyFreshNeverActivated(context.Background(), admin, manifest); (err != nil) != test.wantErr {
				t.Fatalf("unexpected result: %v", err)
			}
		})
	}
}
