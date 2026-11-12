// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package installation

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type Journal struct {
	admin  client.Client
	ctx    context.Context
	record *corev1.ConfigMap
}

type mutationIntent struct {
	Original     ObjectRef `json:"original"`
	ResultSHA256 string    `json:"resultSHA256"`
}

func mutationKey(action string, ref ObjectRef) string {
	digest := sha256.Sum256([]byte(action + "/" + ref.APIVersion + "/" + ref.Kind + "/" + ref.Namespace + "/" + ref.Name + "/" + ref.UID))
	return fmt.Sprintf("mutation-%x", digest[:])
}

func (journal *Journal) mutation(action string, ref ObjectRef) (mutationIntent, bool, error) {
	var intent mutationIntent
	content, found := journal.record.Data[mutationKey(action, ref)]
	if !found {
		return intent, false, nil
	}
	if err := json.Unmarshal([]byte(content), &intent); err != nil {
		return intent, true, err
	}
	if intent.Original != ref {
		return intent, true, errors.New("mutation intent differs from reviewed object")
	}
	return intent, true, nil
}

func (journal *Journal) prepareMutation(action string, ref ObjectRef, result string) error {
	if (action != "stop" && action != "delete" && action != "helm-failed") || ref.UID == "" || len(ref.SHA256) != 64 || (action != "delete" && len(result) != 64) || (action == "delete" && result != "") {
		return errors.New("invalid installation mutation intent")
	}
	intent, found, err := journal.mutation(action, ref)
	if err != nil {
		return err
	}
	if found {
		if intent.ResultSHA256 != result {
			return errors.New("mutation result differs from durable intent")
		}
		return nil
	}
	content, err := json.Marshal(mutationIntent{Original: ref, ResultSHA256: result})
	if err != nil {
		return err
	}
	next := journal.record.DeepCopy()
	next.Data[mutationKey(action, ref)] = string(content)
	if err := journal.admin.Update(journal.ctx, next); err != nil {
		return fmt.Errorf("mutation intent persistence uncertain; recover before retrying: %w", err)
	}
	journal.record = next
	return nil
}

func JournalKey(plan Plan) client.ObjectKey {
	digest := sha256.Sum256([]byte(plan.ClusterUID + "/" + plan.NamespaceUID + "/" + plan.Release))
	return client.ObjectKey{Namespace: plan.AuthorityNamespace, Name: fmt.Sprintf("kognition-install-%x", digest[:16])}
}

func Claim(ctx context.Context, admin client.Client, plan Plan, digest string) (*Journal, error) {
	if err := verifyAuthorityRoot(ctx, admin, plan, true); err != nil {
		return nil, err
	}
	key := JournalKey(plan)
	record := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: key.Namespace, Name: key.Name}, Data: map[string]string{"version": "1", "planSHA256": digest, "events": "claimed\n"}}
	if err := admin.Create(ctx, record); err != nil {
		return nil, fmt.Errorf("target claim was not acquired; do not retry mutations until settled-request recovery: %w", err)
	}
	if record.UID == "" || record.ResourceVersion == "" {
		return nil, errors.New("claim response lacks server identity; recover before retrying")
	}
	return &Journal{admin: admin, ctx: ctx, record: record}, nil
}

func OpenJournal(ctx context.Context, admin client.Client, plan Plan, digest, uid string) (*Journal, error) {
	if err := verifyAuthorityRoot(ctx, admin, plan, false); err != nil {
		return nil, err
	}
	record := &corev1.ConfigMap{}
	if err := admin.Get(ctx, JournalKey(plan), record); err != nil {
		return nil, err
	}
	if uid == "" || record.UID != types.UID(uid) || record.ResourceVersion == "" || record.Data["version"] != "1" || record.Data["planSHA256"] != digest || len(record.OwnerReferences) != 0 || len(record.Finalizers) != 0 || record.DeletionTimestamp != nil {
		return nil, errors.New("installation journal identity, version or reviewed plan differs")
	}
	if !validHistory(record.Data["events"]) {
		return nil, errors.New("invalid installation journal history")
	}
	return &Journal{admin: admin, ctx: ctx, record: record}, nil
}

func verifyAuthorityRoot(ctx context.Context, admin client.Client, plan Plan, bind bool) error {
	root := &corev1.Namespace{}
	if err := admin.Get(ctx, client.ObjectKey{Name: "kube-system"}, root); err != nil {
		return err
	}
	if string(root.UID) != plan.ClusterUID || root.DeletionTimestamp != nil || plan.AuthorityNamespace == "" || plan.AuthorityNamespaceUID == "" {
		return errors.New("installation authority root identity differs")
	}
	key := "kognition.dev/" + JournalKey(plan).Name
	value := plan.AuthorityNamespace + "/" + plan.AuthorityNamespaceUID
	if root.Annotations[key] == value {
		return nil
	}
	if root.Annotations[key] != "" || !bind {
		return errors.New("installation target is pinned to another authority or has no reviewed authority pin")
	}
	before := root.DeepCopy()
	if root.Annotations == nil {
		root.Annotations = map[string]string{}
	}
	root.Annotations[key] = value
	if err := admin.Patch(ctx, root, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
		return fmt.Errorf("authority pin uncertain; no installation claim acquired: %w", err)
	}
	return nil
}

func (journal *Journal) UID() string { return string(journal.record.UID) }

func ConfirmRetired(ctx context.Context, admin client.Client, plan Plan, digest, uid string) error {
	if uid == "" || digest == "" {
		return errors.New("retirement requires exact claim and plan identity")
	}
	if err := verifyAuthorityRoot(ctx, admin, plan, false); err != nil {
		return err
	}
	record := &corev1.ConfigMap{}
	if err := admin.Get(ctx, JournalKey(plan), record); !apierrors.IsNotFound(err) {
		if err != nil {
			return err
		}
		return errors.New("installation claim still exists or has been replaced")
	}
	receipts := &corev1.ConfigMapList{}
	if err := admin.List(ctx, receipts, client.InNamespace(plan.AuthorityNamespace)); err != nil {
		return err
	}
	for _, receipt := range receipts.Items {
		if receipt.Data["claimUID"] != uid || receipt.Data["planSHA256"] != digest {
			continue
		}
		identity := sha256.Sum256([]byte(uid + "/" + receipt.Data["claimResourceVersion"] + "/" + receipt.Data["outcome"]))
		if receipt.Name != fmt.Sprintf("kognition-install-receipt-%x", identity[:16]) || receipt.Data["claimResourceVersion"] == "" || receipt.Data["version"] != "1" || receipt.Data["status"] != "retirement-intent" || receipt.Immutable == nil || !*receipt.Immutable || receipt.DeletionTimestamp != nil || len(receipt.OwnerReferences) != 0 || len(receipt.Finalizers) != 0 {
			continue
		}
		switch receipt.Data["outcome"] {
		case "aborted-before-mutation", "drained-retry", "verified-inactive":
			return nil
		}
	}
	return errors.New("absent claim has no matching immutable retirement receipt")
}

func validStage(stage string) bool {
	if stage == "verified-inactive" {
		return true
	}
	name, status, found := strings.Cut(stage, ":")
	if !found || (status != "pending" && status != "complete" && status != "failed") {
		return false
	}
	name, recovery := strings.CutPrefix(name, "recovery-")
	switch name {
	case "workloads-drained", "stop-controllers", "controllers-stopped", "revoke-grants", "verify-drained", "remove-legacy-policies", "remove-completed-migrations":
		return !recovery || status != "failed"
	case "preflight", "admission-bootstrap", "verify-bootstrap":
		return !recovery
	default:
		return false
	}
}

func validHistory(history string) bool {
	if !strings.HasPrefix(history, "claimed\n") || !strings.HasSuffix(history, "\n") {
		return false
	}
	stages := strings.Split(strings.TrimSuffix(history, "\n"), "\n")
	for _, stage := range stages[1:] {
		if !validStage(stage) {
			return false
		}
	}
	return true
}

func (journal *Journal) Record(stage string) error {
	if !validStage(stage) {
		return errors.New("invalid journal stage")
	}
	next := journal.record.DeepCopy()
	next.Data["events"] += stage + "\n"
	if err := journal.admin.Update(journal.ctx, next); err != nil {
		return fmt.Errorf("journal persistence uncertain; keep writers frozen and recover: %w", err)
	}
	journal.record = next
	return nil
}

func (journal *Journal) BeforeMutation() bool {
	for _, stage := range strings.Split(journal.record.Data["events"], "\n") {
		switch stage {
		case "", "claimed", "preflight:pending", "preflight:complete", "preflight:failed", "workloads-drained:pending", "workloads-drained:complete", "workloads-drained:failed":
		default:
			return false
		}
	}
	return true
}

func (journal *Journal) AdmissionStarted() bool {
	for _, stage := range strings.Split(journal.record.Data["events"], "\n") {
		if strings.HasPrefix(stage, "admission-bootstrap:") || strings.HasPrefix(stage, "verify-bootstrap:") || stage == "verified-inactive" {
			return true
		}
	}
	return false
}

func (journal *Journal) Retire(outcome string) error {
	if outcome != "aborted-before-mutation" && outcome != "drained-retry" && outcome != "verified-inactive" {
		return errors.New("invalid installation recovery outcome")
	}
	digest := sha256.Sum256([]byte(journal.UID() + "/" + journal.record.ResourceVersion + "/" + outcome))
	receipt := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: journal.record.Namespace, Name: fmt.Sprintf("kognition-install-receipt-%x", digest[:16])}, Immutable: ptr.To(true), Data: map[string]string{"version": "1", "claimUID": journal.UID(), "claimResourceVersion": journal.record.ResourceVersion, "planSHA256": journal.record.Data["planSHA256"], "events": journal.record.Data["events"], "outcome": outcome, "status": "retirement-intent"}}
	if err := journal.admin.Create(journal.ctx, receipt); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return err
		}
		live := &corev1.ConfigMap{}
		if err := journal.admin.Get(journal.ctx, client.ObjectKeyFromObject(receipt), live); err != nil {
			return err
		}
		if live.Immutable == nil || !*live.Immutable || live.DeletionTimestamp != nil || len(live.OwnerReferences) != 0 || len(live.Finalizers) != 0 || !reflect.DeepEqual(live.Data, receipt.Data) {
			return errors.New("recovery receipt differs; claim retained")
		}
	}
	uid, version := journal.record.UID, journal.record.ResourceVersion
	return journal.admin.Delete(journal.ctx, journal.record, &client.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid, ResourceVersion: &version}})
}
