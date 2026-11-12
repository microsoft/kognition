// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package installation

import (
	"context"
	"errors"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestRetirementRetryCanReflectNewVerification(test *testing.T) {
	ctx := context.Background()
	plan := Plan{ClusterUID: "cluster", NamespaceUID: "control", Release: "release", AuthorityNamespace: "authority", AuthorityNamespaceUID: "authority-uid"}
	key := JournalKey(plan)
	record := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace, UID: "claim", ResourceVersion: "1"}, Data: map[string]string{"version": "1", "planSHA256": "digest", "events": "claimed\nstop-controllers:pending\n"}}
	root := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "kube-system", UID: "cluster", Annotations: map[string]string{"kognition.dev/" + key.Name: "authority/authority-uid"}}}
	lost := errors.New("lost receipt response")
	loseResponse := true
	admin := fake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(record, root).WithInterceptorFuncs(interceptor.Funcs{Create: func(ctx context.Context, underlying client.WithWatch, object client.Object, options ...client.CreateOption) error {
		if err := underlying.Create(ctx, object, options...); err != nil {
			return err
		}
		if loseResponse {
			loseResponse = false
			return lost
		}
		return nil
	}}).Build()
	journal, err := OpenJournal(ctx, admin, plan, "digest", "claim")
	if err != nil {
		test.Fatal(err)
	}
	if err := journal.Retire("drained-retry"); !errors.Is(err, lost) {
		test.Fatal(err)
	}
	if err := admin.Get(ctx, key, record); err != nil {
		test.Fatal("lost response released claim", err)
	}
	if err := journal.Retire("verified-inactive"); err != nil {
		test.Fatal(err)
	}
	if err := ConfirmRetired(ctx, admin, plan, "digest", "claim"); err != nil {
		test.Fatal(err)
	}
}

func TestJournalProtectsTargetAndRetainsRecoveryReceipt(test *testing.T) {
	ctx := context.Background()
	plan := Plan{ClusterUID: "cluster", NamespaceUID: "control", Release: "release", AuthorityNamespace: "authority", AuthorityNamespaceUID: "authority-uid"}
	digest := strings.Repeat("a", 64)
	key := JournalKey(plan)
	record := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace, UID: "claim-uid", ResourceVersion: "1"}, Data: map[string]string{"version": "1", "planSHA256": digest, "events": "claimed\n"}}
	root := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "kube-system", UID: "cluster", Annotations: map[string]string{"kognition.dev/" + key.Name: "authority/authority-uid"}}}
	admin := fake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(record, root).Build()
	if _, err := Claim(ctx, admin, plan, digest); err == nil {
		test.Fatal("second claim accepted")
	}
	if _, err := OpenJournal(ctx, admin, plan, digest, "replacement"); err == nil {
		test.Fatal("replacement claim accepted")
	}
	if _, err := OpenJournal(ctx, admin, plan, strings.Repeat("b", 64), "claim-uid"); err == nil {
		test.Fatal("different plan accepted")
	}
	journal, err := OpenJournal(ctx, admin, plan, digest, "claim-uid")
	if err != nil {
		test.Fatal(err)
	}
	if !journal.BeforeMutation() {
		test.Fatal("new claim classified as mutation")
	}
	if err := journal.Record("stop-controllers:pending"); err != nil {
		test.Fatal(err)
	}
	if journal.BeforeMutation() {
		test.Fatal("pending mutation classified as safe abort")
	}
	if err := journal.Retire("drained-retry"); err != nil {
		test.Fatal(err)
	}
	if err := admin.Get(ctx, key, &corev1.ConfigMap{}); !apierrors.IsNotFound(err) {
		test.Fatalf("claim retained: %v", err)
	}
	receipts := &corev1.ConfigMapList{}
	if err := admin.List(ctx, receipts, client.InNamespace("authority")); err != nil {
		test.Fatal(err)
	}
	if len(receipts.Items) != 1 || receipts.Items[0].Immutable == nil || !*receipts.Items[0].Immutable || receipts.Items[0].Data["claimUID"] != "claim-uid" || receipts.Items[0].Data["outcome"] != "drained-retry" {
		test.Fatalf("missing durable receipt: %+v", receipts.Items)
	}
}

func TestStaleJournalCannotRetireChangedClaim(test *testing.T) {
	ctx := context.Background()
	plan := Plan{ClusterUID: "cluster", NamespaceUID: "control", Release: "release", AuthorityNamespace: "authority", AuthorityNamespaceUID: "authority-uid"}
	key := JournalKey(plan)
	record := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace, UID: "claim", ResourceVersion: "1"}, Data: map[string]string{"version": "1", "planSHA256": "digest", "events": "claimed\n"}}
	root := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "kube-system", UID: "cluster", Annotations: map[string]string{"kognition.dev/" + key.Name: "authority/authority-uid"}}}
	admin := fake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(record, root).Build()
	journal, err := OpenJournal(ctx, admin, plan, "digest", "claim")
	if err != nil {
		test.Fatal(err)
	}
	record.Data["events"] += "stop-controllers:pending\n"
	if err := admin.Update(ctx, record); err != nil {
		test.Fatal(err)
	}
	if err := journal.Retire("aborted-before-mutation"); err == nil {
		test.Fatal("stale recovery retired a changed claim")
	}
	if err := admin.Get(ctx, key, record); err != nil {
		test.Fatal("claim lost", err)
	}
}

func TestMutationIntentIsDurableAndExact(test *testing.T) {
	ctx := context.Background()
	plan := Plan{ClusterUID: "cluster", NamespaceUID: "control", Release: "release", AuthorityNamespace: "authority", AuthorityNamespaceUID: "authority-uid"}
	key := JournalKey(plan)
	record := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace, UID: "claim", ResourceVersion: "1"}, Data: map[string]string{"version": "1", "planSHA256": "digest", "events": "claimed\nstop-controllers:pending\n"}}
	root := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "kube-system", UID: "cluster", Annotations: map[string]string{"kognition.dev/" + key.Name: "authority/authority-uid"}}}
	admin := fake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(record, root).Build()
	journal, err := OpenJournal(ctx, admin, plan, "digest", "claim")
	if err != nil {
		test.Fatal(err)
	}
	ref := ObjectRef{APIVersion: "apps/v1", Kind: "Deployment", Namespace: "control", Name: "controller", UID: "controller-uid", SHA256: strings.Repeat("a", 64)}
	result := strings.Repeat("b", 64)
	if err := journal.prepareMutation("stop", ref, result); err != nil {
		test.Fatal(err)
	}
	reopened, err := OpenJournal(ctx, admin, plan, "digest", "claim")
	if err != nil {
		test.Fatal(err)
	}
	if err := reopened.prepareMutation("stop", ref, result); err != nil {
		test.Fatal(err)
	}
	if err := reopened.prepareMutation("stop", ref, strings.Repeat("c", 64)); err == nil {
		test.Fatal("changed expected result accepted")
	}
	ref.SHA256 = strings.Repeat("d", 64)
	if err := reopened.prepareMutation("stop", ref, result); err == nil {
		test.Fatal("changed reviewed object accepted")
	}
	if err := journal.Record("stop-controllers:complete"); err != nil {
		test.Fatal(err)
	}
	if err := reopened.prepareMutation("delete", ref, ""); err == nil {
		test.Fatal("stale executor wrote new intent")
	}
}

func TestJournalRejectsMalformedHistory(test *testing.T) {
	for _, scenario := range []struct {
		name    string
		history string
		valid   bool
	}{
		{"claimed", "claimed\n", true},
		{"interrupted", "claimed\nstop-controllers:pending\n", true},
		{"retry", "claimed\nstop-controllers:pending\nrecovery-stop-controllers:pending\nrecovery-stop-controllers:pending\nrecovery-stop-controllers:complete\n", true},
		{"unknown-stage", "claimed\nstop-controller:pending\n", false},
		{"unknown-status", "claimed\nstop-controllers:success\n", false},
		{"truncated", "claimed\nadmission-bootstrap:pending", false},
		{"empty-event", "claimed\n\n", false},
		{"repeated-claim", "claimed\nclaimed\n", false},
		{"recovery-admission", "claimed\nrecovery-admission-bootstrap:pending\n", false},
		{"carriage-return", "claimed\nstop-controllers:pending\r\n", false},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			ctx := context.Background()
			plan := Plan{ClusterUID: "cluster", NamespaceUID: "control", Release: "release", AuthorityNamespace: "authority", AuthorityNamespaceUID: "authority-uid"}
			key := JournalKey(plan)
			record := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace, UID: "claim", ResourceVersion: "1"}, Data: map[string]string{"version": "1", "planSHA256": "digest", "events": scenario.history}}
			root := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "kube-system", UID: "cluster", Annotations: map[string]string{"kognition.dev/" + key.Name: "authority/authority-uid"}}}
			admin := fake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(record, root).Build()
			journal, err := OpenJournal(ctx, admin, plan, "digest", "claim")
			if (err == nil) != scenario.valid {
				test.Fatalf("history validation: %v", err)
			}
			if !scenario.valid {
				return
			}
			for _, stage := range []string{"claimed", "unknown:pending", "stop-controllers:success", "recovery-admission-bootstrap:pending", ""} {
				if err := journal.Record(stage); err == nil {
					test.Fatalf("accepted stage %q", stage)
				}
			}
			live := &corev1.ConfigMap{}
			if err := admin.Get(ctx, key, live); err != nil {
				test.Fatal(err)
			}
			if live.Data["events"] != scenario.history {
				test.Fatal("invalid append changed durable history")
			}
		})
	}
}
