// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package activation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func (candidate Candidate) checkOptions(target ActivationTarget, options ActivationOptions) error {
	if candidate.controller == nil || target.AuthorityNamespace != options.Approval.Namespace || target.Deployment != candidate.manifest.Deployment || target.Guard != candidate.manifest.Guard || !reflect.DeepEqual(target.Namespaces, candidate.manifest.Namespaces) {
		return errors.New("candidate identity or approval authority mismatch")
	}
	if target.Endpoint != options.Endpoint || target.ServerVersion != options.ServerVersion || target.ChartSHA256 != options.ChartSHA256 || target.ToolVersion != options.ToolVersion || target.Replicas != options.Replicas || target.ManifestSHA256 != candidate.sha256 || target.Release != candidate.release || target.Namespace != candidate.manifest.Namespace || target.Controller != candidate.manifest.Controller {
		return errors.New("requested activation does not match recorded target")
	}
	return nil
}

func (candidate Candidate) completed(ctx context.Context, admin client.Client, record *corev1.ConfigMap, approval Approval, options ActivationOptions) (Audit, bool, error) {
	var audit Audit
	journal := &corev1.ConfigMap{}
	if err := admin.Get(ctx, installationKey(record.Namespace, approval.Target), journal); apierrors.IsNotFound(err) {
		return audit, false, nil
	} else if err != nil {
		return audit, false, err
	}
	if journal.UID == "" || journal.DeletionTimestamp != nil || len(journal.OwnerReferences) != 0 {
		return audit, false, errors.New("invalid installation journal")
	}
	if err := decodeStrict([]byte(journal.Data["audit.json"]), &audit); err != nil {
		return audit, false, err
	}
	if audit.ResourceJournalVersion != 1 {
		return audit, false, errors.New("journal lacks durable resource inventory; completion cannot be certified")
	}
	if audit.Stage == "recovered-inactive" && audit.ApprovalUID != string(record.UID) {
		return audit, false, nil
	}
	if audit.Stage != "active" || audit.ApprovalUID != string(record.UID) {
		return audit, false, errors.New("installation has an unfinished or different transaction; explicit recovery is required")
	}
	if !approvalsEqual(audit.Approval, approval) {
		return audit, false, errors.New("completed transaction approval mismatch")
	}
	if err := candidate.checkOptions(approval.Target, options); err != nil {
		return audit, false, err
	}
	if err := compareInventory(ctx, admin, approval.Target, true); err != nil {
		return audit, false, err
	}
	if err := candidate.verifyBindings(ctx, admin, journal); err != nil {
		return audit, false, err
	}
	if ready, err := candidate.rolloutReady(ctx, admin, approval.Target); err != nil || !ready {
		return audit, false, errors.Join(errors.New("completed controller rollout is no longer ready"), err)
	}
	return audit, true, nil
}

func (candidate Candidate) rolloutReady(ctx context.Context, admin client.Client, target ActivationTarget) (bool, error) {
	deployment := &appsv1.Deployment{}
	if err := admin.Get(ctx, client.ObjectKeyFromObject(candidate.controller), deployment); err != nil {
		return false, err
	}
	guard := &appsv1.Deployment{}
	if err := admin.Get(ctx, client.ObjectKey{Namespace: target.Namespace, Name: target.Guard}, guard); err != nil {
		return false, err
	}
	if guard.Spec.Replicas == nil || *guard.Spec.Replicas < 1 || guard.DeletionTimestamp != nil || guard.Status.ObservedGeneration != guard.Generation || guard.Status.Replicas != *guard.Spec.Replicas || guard.Status.ReadyReplicas != *guard.Spec.Replicas || guard.Status.UpdatedReplicas != *guard.Spec.Replicas || guard.Status.AvailableReplicas != *guard.Spec.Replicas {
		return false, errors.New("guard rollout is no longer ready")
	}
	for _, expected := range candidate.resourceAdmission {
		if expected.GetKind() != "Deployment" {
			continue
		}
		server := &appsv1.Deployment{}
		if err := admin.Get(ctx, client.ObjectKeyFromObject(expected), server); err != nil {
			return false, err
		}
		if server.Spec.Replicas == nil || *server.Spec.Replicas < 1 || server.DeletionTimestamp != nil || server.Status.ObservedGeneration != server.Generation || server.Status.Replicas != *server.Spec.Replicas || server.Status.ReadyReplicas != *server.Spec.Replicas || server.Status.UpdatedReplicas != *server.Spec.Replicas || server.Status.AvailableReplicas != *server.Spec.Replicas {
			return false, errors.New("resource admission rollout is no longer ready")
		}
	}
	return deployment.DeletionTimestamp == nil && deployment.Status.ObservedGeneration == deployment.Generation && deployment.Status.Replicas == target.Replicas && deployment.Status.ReadyReplicas == target.Replicas && deployment.Status.UpdatedReplicas == target.Replicas && deployment.Status.AvailableReplicas == target.Replicas, nil
}

func (candidate Candidate) Recover(ctx context.Context, admin client.Client, options ActivationOptions, executorsStopped bool) (Audit, error) {
	var audit Audit
	if !options.WritersFrozen || !executorsStopped || options.DryRun {
		return audit, errors.New("recovery requires frozen writers and all earlier executors stopped; dry-run recovery is unsupported")
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	record, approval, err := readApproval(ctx, admin, options.Approval, candidate.manifest)
	if err != nil {
		return audit, err
	}
	if err := candidate.checkOptions(approval.Target, options); err != nil {
		return audit, err
	}
	cluster := &corev1.Namespace{}
	if err := admin.Get(ctx, client.ObjectKey{Name: "kube-system"}, cluster); err != nil {
		return audit, err
	}
	if string(cluster.UID) != approval.Target.ClusterUID {
		return audit, errors.New("recovery cluster identity mismatch")
	}
	journal := &corev1.ConfigMap{}
	if err := admin.Get(ctx, installationKey(options.Approval.Namespace, approval.Target), journal); err != nil {
		return audit, err
	}
	if journal.UID == "" || journal.DeletionTimestamp != nil || len(journal.OwnerReferences) != 0 {
		return audit, errors.New("invalid recovery journal")
	}
	if err := decodeStrict([]byte(journal.Data["audit.json"]), &audit); err != nil {
		return audit, err
	}
	if audit.ApprovalUID != string(record.UID) || !approvalsEqual(audit.Approval, approval) {
		return audit, errors.New("recovery approval does not match the journal")
	}
	if err := candidate.checkOptions(audit.Approval.Target, options); err != nil {
		return audit, err
	}
	if err := saveStage(ctx, admin, journal, "recovering", nil); err != nil {
		return audit, err
	}
	err = candidate.deactivate(ctx, admin, journal, audit.Approval.Target)
	stage := "recovered-inactive"
	if err != nil {
		stage = "recovery-required"
	}
	err = errors.Join(err, saveStage(ctx, admin, journal, stage, err))
	return auditFromJournal(journal, err)
}

func auditFromJournal(journal *corev1.ConfigMap, failure error) (Audit, error) {
	var audit Audit
	err := json.Unmarshal([]byte(journal.Data["audit.json"]), &audit)
	return audit, errors.Join(failure, err)
}

func releaseRecovered(ctx context.Context, admin client.Client, key client.ObjectKey) error {
	journal := &corev1.ConfigMap{}
	if err := admin.Get(ctx, key, journal); apierrors.IsNotFound(err) {
		return nil
	} else if err != nil {
		return err
	}
	var audit Audit
	if err := decodeStrict([]byte(journal.Data["audit.json"]), &audit); err != nil {
		return err
	}
	if audit.Stage != "recovered-inactive" || audit.ResourceJournalVersion != 1 || journal.UID == "" || journal.DeletionTimestamp != nil || len(journal.OwnerReferences) != 0 {
		return errors.New("installation journal requires explicit recovery")
	}
	archive := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: key.Namespace, Name: fmt.Sprintf("activation-history-%s", journal.UID)}, Immutable: ptr.To(true), Data: journal.Data}
	if err := admin.Create(ctx, archive); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return err
		}
		live := &corev1.ConfigMap{}
		if err := admin.Get(ctx, client.ObjectKeyFromObject(archive), live); err != nil {
			return err
		}
		if live.Immutable == nil || !*live.Immutable || live.Data["audit.json"] != journal.Data["audit.json"] || live.DeletionTimestamp != nil || len(live.OwnerReferences) != 0 {
			return errors.New("recovery archive collision")
		}
	}
	uid, version := journal.UID, journal.ResourceVersion
	return admin.Delete(ctx, journal, &client.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid, ResourceVersion: &version}})
}
