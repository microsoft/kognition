// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package activation

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestApprovalClaimSingleUse(test *testing.T) {
	admin := fake.NewClientBuilder().WithScheme(scheme.Scheme).WithInterceptorFuncs(interceptor.Funcs{Create: func(ctx context.Context, delegate client.WithWatch, object client.Object, options ...client.CreateOption) error {
		object.SetUID(types.UID(object.GetName()))
		return delegate.Create(ctx, object, options...)
	}}).Build()
	now := time.Now().UTC()
	approval := approvalFixture(now)
	record := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: "authority", Name: "approved", UID: "approval-uid"}}
	var successes atomic.Int32
	var workers sync.WaitGroup
	for range 8 {
		workers.Go(func() {
			if _, err := claimApproval(context.Background(), admin, record, approval, now); err == nil {
				successes.Add(1)
			}
		})
	}
	workers.Wait()
	if successes.Load() != 1 {
		test.Fatalf("expected one executor, got %d", successes.Load())
	}
	journal := &corev1.ConfigMap{}
	if err := admin.Get(context.Background(), installationKey(record.Namespace, approval.Target), journal); err != nil {
		test.Fatal(err)
	}
	var audit Audit
	if err := json.Unmarshal([]byte(journal.Data["audit.json"]), &audit); err != nil {
		test.Fatal(err)
	}
	if audit.ApprovalUID != string(record.UID) || audit.Stage != "claimed" || audit.Approval.Target.ManifestSHA256 != approval.Target.ManifestSHA256 {
		test.Fatalf("incomplete audit: %+v", audit)
	}
	if err := admin.Delete(context.Background(), journal); err != nil {
		test.Fatal(err)
	}
	if _, err := claimApproval(context.Background(), admin, record, approval, now); err == nil {
		test.Fatal("replayed consumed approval after journal deletion")
	}
}

func TestJournalRejectsConcurrentUpdate(test *testing.T) {
	admin := fake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: "authority", Name: "journal", UID: "uid"}, Data: map[string]string{"audit.json": `{}`}}).Build()
	journal := &corev1.ConfigMap{}
	if err := admin.Get(context.Background(), client.ObjectKey{Namespace: "authority", Name: "journal"}, journal); err != nil {
		test.Fatal(err)
	}
	stale := journal.DeepCopy()
	if err := saveStage(context.Background(), admin, journal, "recovering", nil); err != nil {
		test.Fatal(err)
	}
	if err := saveStage(context.Background(), admin, stale, "starting", nil); err == nil {
		test.Fatal("stale executor overwrote recovery")
	}
}

func TestRecoveryArchiveCollision(test *testing.T) {
	for _, scenario := range []string{"new", "matching", "mutable", "different", "owned", "unfinished", "legacy"} {
		test.Run(scenario, func(test *testing.T) {
			data := map[string]string{"audit.json": `{"stage":"recovered-inactive","resourceJournalVersion":1}`}
			journal := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: "authority", Name: "journal", UID: "journal-uid"}, Data: data}
			archive := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: "authority", Name: "activation-history-journal-uid", UID: "archive-uid"}, Immutable: ptr.To(true), Data: map[string]string{"audit.json": data["audit.json"]}}
			switch scenario {
			case "mutable":
				archive.Immutable = ptr.To(false)
			case "different":
				archive.Data["audit.json"] = `{}`
			case "owned":
				archive.OwnerReferences = []metav1.OwnerReference{{UID: "foreign"}}
			case "unfinished":
				journal.Data["audit.json"] = `{"stage":"recovering"}`
			case "legacy":
				journal.Data["audit.json"] = `{"stage":"recovered-inactive"}`
			}
			builder := fake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(journal)
			if scenario != "new" {
				builder = builder.WithObjects(archive)
			}
			admin := builder.Build()
			err := releaseRecovered(context.Background(), admin, client.ObjectKeyFromObject(journal))
			success := scenario == "new" || scenario == "matching"
			if (err == nil) != success {
				test.Fatalf("archive result: %v", err)
			}
			getErr := admin.Get(context.Background(), client.ObjectKeyFromObject(journal), &corev1.ConfigMap{})
			if success && !apierrors.IsNotFound(getErr) || !success && getErr != nil {
				test.Fatalf("unexpected journal retention: %v", getErr)
			}
			if err := admin.Get(context.Background(), client.ObjectKeyFromObject(archive), archive); err != nil {
				test.Fatal(err)
			}
			if success && (archive.Immutable == nil || !*archive.Immutable || archive.Data["audit.json"] != data["audit.json"]) {
				test.Fatal("archive lost audit")
			}
		})
	}
}

func TestFailedJournalCreationConsumesApproval(test *testing.T) {
	now := time.Now().UTC()
	approval := approvalFixture(now)
	record := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: "authority", Name: "approved", UID: "approval-uid"}}
	admin := fake.NewClientBuilder().WithScheme(scheme.Scheme).WithInterceptorFuncs(interceptor.Funcs{Create: func(ctx context.Context, delegate client.WithWatch, object client.Object, options ...client.CreateOption) error {
		if strings.HasPrefix(object.GetName(), "activation-") {
			return errors.New("injected journal failure")
		}
		return delegate.Create(ctx, object, options...)
	}}).Build()
	if _, err := claimApproval(context.Background(), admin, record, approval, now); err == nil || !strings.Contains(err.Error(), "approval remains consumed") {
		test.Fatalf("unexpected claim failure: %v", err)
	}
	if _, err := claimApproval(context.Background(), admin, record, approval, now); err == nil || !strings.Contains(err.Error(), "approval consumption failed") {
		test.Fatalf("uncertain claim replayed: %v", err)
	}
}

func TestLegacyJournalCannotCertifyCompletion(test *testing.T) {
	approval := approvalFixture(time.Now().UTC())
	key := installationKey("authority", approval.Target)
	for _, stage := range []string{"active", "recovered-inactive"} {
		test.Run(stage, func(test *testing.T) {
			encoded, err := json.Marshal(Audit{Stage: stage, Approval: approval, ApprovalUID: "approval-uid"})
			if err != nil {
				test.Fatal(err)
			}
			journal := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace, UID: "journal-uid"}, Data: map[string]string{"audit.json": string(encoded)}}
			admin := fake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(journal).Build()
			record := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: key.Namespace, UID: "approval-uid"}}
			if _, completed, err := (Candidate{}).completed(context.Background(), admin, record, approval, ActivationOptions{}); err == nil || completed || !strings.Contains(err.Error(), "durable resource inventory") {
				test.Fatalf("legacy journal accepted: completed=%v error=%v", completed, err)
			}
		})
	}
}

func TestJournalRetainsFailedStage(test *testing.T) {
	journal := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: "authority", Name: "journal", UID: "journal"}, Data: map[string]string{"audit.json": `{"stage":"rollout"}`}}
	admin := fake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(journal).Build()
	if err := admin.Get(context.Background(), client.ObjectKeyFromObject(journal), journal); err != nil {
		test.Fatal(err)
	}
	for _, stage := range []string{"recovery-required", "recovering", "recovered-inactive"} {
		var failure error
		if stage == "recovery-required" {
			failure = errors.New("rollout failed")
		}
		if err := saveStage(context.Background(), admin, journal, stage, failure); err != nil {
			test.Fatal(err)
		}
	}
	audit, err := auditFromJournal(journal, nil)
	if err != nil || audit.Stage != "recovered-inactive" || audit.FailedStage != "rollout" || audit.Failure != "rollout failed" {
		test.Fatalf("lost failure provenance: %+v %v", audit, err)
	}
}
