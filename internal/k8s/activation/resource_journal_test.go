// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package activation

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	admissionv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestJournaledCreateLatePersistence(test *testing.T) {
	ctx := context.Background()
	journal := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "journal", Namespace: "authority", UID: "journal-uid"}, Data: map[string]string{"audit.json": `{"stage":"granting"}`}}
	var delayed client.Object
	admin := fake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(journal).WithInterceptorFuncs(interceptor.Funcs{
		Create: func(ctx context.Context, delegate client.WithWatch, object client.Object, opts ...client.CreateOption) error {
			var ok bool
			delayed, ok = object.DeepCopyObject().(client.Object)
			if !ok {
				test.Fatal("invalid resource copy")
			}
			return errors.New("transport timeout before response")
		},
	}).Build()
	if err := admin.Get(ctx, client.ObjectKeyFromObject(journal), journal); err != nil {
		test.Fatal(err)
	}
	grant := &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "grant", Namespace: "control"}}
	if err := createJournaledResource(ctx, admin, journal, grant, false); err == nil {
		test.Fatal("uncertain Create succeeded")
	}
	if err := cleanupJournaledResources(ctx, admin, journal, false); err == nil || !strings.Contains(err.Error(), "unresolved Create") {
		test.Fatalf("NotFound incorrectly resolved in-flight Create: %v", err)
	}
	fresh := &corev1.ConfigMap{}
	if err := admin.Get(ctx, client.ObjectKeyFromObject(journal), fresh); err != nil {
		test.Fatal(err)
	}
	delayed.SetUID("late-uid")
	persisted := fake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(fresh, delayed).Build()
	if err := cleanupJournaledResources(ctx, persisted, fresh, false); err != nil {
		test.Fatal(err)
	}
	if err := persisted.Get(ctx, client.ObjectKeyFromObject(grant), grant); !apierrors.IsNotFound(err) {
		test.Fatalf("late grant retained: %v", err)
	}
}

func TestJournaledProbeRecovery(test *testing.T) {
	for _, scenario := range []string{"owned", "foreign", "replaced"} {
		test.Run(scenario, func(test *testing.T) {
			ctx := context.Background()
			journal := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "journal", Namespace: "authority", UID: "journal-uid"}, Data: map[string]string{"audit.json": `{"stage":"behavior-verification"}`}}
			admin := fake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(journal).WithInterceptorFuncs(interceptor.Funcs{Create: func(ctx context.Context, delegate client.WithWatch, object client.Object, opts ...client.CreateOption) error {
				object.SetUID("probe-uid")
				return delegate.Create(ctx, object, opts...)
			}}).Build()
			if err := admin.Get(ctx, client.ObjectKeyFromObject(journal), journal); err != nil {
				test.Fatal(err)
			}
			probe := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "probe", Namespace: "tenant"}}
			if err := createJournaledResource(ctx, admin, journal, probe, true); err != nil {
				test.Fatal(err)
			}
			fresh := &corev1.ConfigMap{}
			if err := admin.Get(ctx, client.ObjectKeyFromObject(journal), fresh); err != nil {
				test.Fatal(err)
			}
			if scenario == "foreign" {
				probe.SetAnnotations(nil)
			} else if scenario == "replaced" {
				probe.SetUID("replacement")
			}
			restarted := fake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(fresh, probe).Build()
			err := cleanupJournaledResources(ctx, restarted, fresh, true)
			if (err == nil) != (scenario == "owned") {
				test.Fatalf("unsafe cleanup result: %v", err)
			}
			getErr := restarted.Get(ctx, client.ObjectKeyFromObject(probe), probe)
			if scenario == "owned" && !apierrors.IsNotFound(getErr) || scenario != "owned" && getErr != nil {
				test.Fatalf("incorrect resource retention: %v", getErr)
			}
		})
	}
}

func TestJournaledCreateWriteFailures(test *testing.T) {
	for _, scenario := range []string{"intent-not-saved", "uid-not-saved", "uid-response-lost", "collision"} {
		test.Run(scenario, func(test *testing.T) {
			ctx := context.Background()
			journal := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "journal", Namespace: "authority", UID: "journal-uid"}, Data: map[string]string{"audit.json": `{"resourceJournalVersion":1}`}}
			foreign := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "probe", Namespace: "tenant", UID: "foreign-uid"}}
			builder := fake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(journal)
			if scenario == "collision" {
				builder.WithObjects(foreign)
			}
			var creates, updates int
			admin := builder.WithInterceptorFuncs(interceptor.Funcs{
				Create: func(ctx context.Context, delegate client.WithWatch, object client.Object, opts ...client.CreateOption) error {
					creates++
					object.SetUID("probe-uid")
					return delegate.Create(ctx, object, opts...)
				},
				Update: func(ctx context.Context, delegate client.WithWatch, object client.Object, opts ...client.UpdateOption) error {
					updates++
					if scenario == "intent-not-saved" || updates == 2 && scenario == "uid-not-saved" {
						return errors.New("journal write failed")
					}
					if err := delegate.Update(ctx, object, opts...); err != nil {
						return err
					}
					if updates == 2 && scenario == "uid-response-lost" {
						return errors.New("journal response lost")
					}
					return nil
				},
			}).Build()
			if err := admin.Get(ctx, client.ObjectKeyFromObject(journal), journal); err != nil {
				test.Fatal(err)
			}
			probe := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: foreign.Name, Namespace: foreign.Namespace}}
			if err := createJournaledResource(ctx, admin, journal, probe, true); err == nil {
				test.Fatal("injected failure succeeded")
			}
			if scenario == "intent-not-saved" {
				if creates != 0 {
					test.Fatal("Create sent without durable intent")
				}
				return
			}
			if err := createJournaledResource(ctx, admin, journal, probe, true); err == nil || creates != 1 {
				test.Fatalf("Create retried: count=%d error=%v", creates, err)
			}
			fresh := &corev1.ConfigMap{}
			if err := admin.Get(ctx, client.ObjectKeyFromObject(journal), fresh); err != nil {
				test.Fatal(err)
			}
			if err := cleanupJournaledResources(ctx, admin, fresh, true); err != nil {
				test.Fatal(err)
			}
			live := &corev1.ConfigMap{}
			err := admin.Get(ctx, client.ObjectKeyFromObject(probe), live)
			if scenario == "collision" {
				if err != nil || live.UID != foreign.UID {
					test.Fatalf("foreign collision changed: %v", err)
				}
			} else if !apierrors.IsNotFound(err) {
				test.Fatalf("owned resource survived recovery: %v", err)
			}
		})
	}
}

func TestJournaledUnresolvedProbeRetainsProtection(test *testing.T) {
	ctx := context.Background()
	policy := &admissionv1.ValidatingAdmissionPolicy{ObjectMeta: metav1.ObjectMeta{Name: "protection", UID: "policy-uid", Annotations: map[string]string{transactionOwner: "journal-uid"}}}
	audit := Audit{ResourceJournalVersion: 1, Resources: []ResourceIntent{
		{APIVersion: admissionv1.SchemeGroupVersion.String(), Kind: "ValidatingAdmissionPolicy", Name: policy.Name, UID: string(policy.UID), Probe: true},
		{APIVersion: "v1", Kind: "ConfigMap", Namespace: "tenant", Name: "pending", Probe: true},
	}}
	encoded, err := json.Marshal(audit)
	if err != nil {
		test.Fatal(err)
	}
	journal := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "journal", Namespace: "authority", UID: "journal-uid"}, Data: map[string]string{"audit.json": string(encoded)}}
	admin := fake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(journal, policy).Build()
	if err := cleanupJournaledResources(ctx, admin, journal, true); err == nil || !strings.Contains(err.Error(), "retained dry-run protection") {
		test.Fatalf("pending probe cleanup succeeded: %v", err)
	}
	if err := admin.Get(ctx, client.ObjectKeyFromObject(policy), policy); err != nil {
		test.Fatalf("protection removed before uncertain request resolved: %v", err)
	}
}
