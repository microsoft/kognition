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

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const transactionOwner = "kognition.dev/activation-transaction"

type ActivationOptions struct {
	Approval      client.ObjectKey
	Endpoint      string
	ServerVersion string
	ChartSHA256   string
	ToolVersion   string
	Replicas      int32
	DryRun        bool
	WritersFrozen bool
}

func (candidate Candidate) Activate(ctx context.Context, admin client.Client, config *rest.Config, options ActivationOptions) (result Audit, err error) {
	if !options.WritersFrozen || config == nil || config.Host != options.Endpoint {
		return result, errors.New("explicit writer freeze and matching API endpoint are required")
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	record, approval, err := readApproval(ctx, admin, options.Approval, candidate.manifest)
	if err != nil {
		return result, err
	}
	if err := validateReports(approval, record.Data); err != nil {
		return result, err
	}
	if err := candidate.checkOptions(approval.Target, options); err != nil {
		return result, err
	}
	if completed, found, err := candidate.completed(ctx, admin, record, approval, options); err != nil || found {
		return completed, err
	}
	target, err := candidate.Snapshot(ctx, admin, options.Endpoint, options.ServerVersion, options.ChartSHA256, options.ToolVersion, options.Approval.Namespace, options.Replicas)
	if err != nil {
		return result, err
	}
	if err := ValidateApproval(approval, target, time.Now().UTC()); err != nil {
		return result, err
	}
	result = Audit{ApprovalUID: string(record.UID), Approval: approval, Stage: "dry-run", Started: time.Now().UTC()}
	if options.DryRun {
		return result, nil
	}
	journal, err := claimApproval(ctx, admin, record, approval, time.Now().UTC())
	if err != nil {
		return result, err
	}
	defer func() {
		if err != nil {
			cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), time.Minute)
			defer cleanupCancel()
			cleanupErr := candidate.deactivate(cleanupCtx, admin, journal, target)
			stage := "failed-inactive"
			if cleanupErr != nil {
				stage = "recovery-required"
			}
			err = errors.Join(err, cleanupErr)
			err = errors.Join(err, saveStage(cleanupCtx, admin, journal, stage, err))
		}
		decodeErr := json.Unmarshal([]byte(journal.Data["audit.json"]), &result)
		err = errors.Join(err, decodeErr)
	}()
	if err = saveStage(ctx, admin, journal, "behavior-verification", nil); err != nil {
		return result, err
	}
	if _, err = verifyPolicyBehavior(ctx, config, candidate.manifest, admin, journal); err != nil {
		return result, err
	}
	current, err := candidate.Snapshot(ctx, admin, options.Endpoint, options.ServerVersion, options.ChartSHA256, options.ToolVersion, options.Approval.Namespace, options.Replicas)
	if err != nil {
		return result, err
	}
	if err = ValidateApproval(approval, current, time.Now().UTC()); err != nil {
		return result, err
	}
	currentRecord, _, err := readApproval(ctx, admin, options.Approval, candidate.manifest)
	if err != nil {
		return result, err
	}
	if currentRecord.UID != record.UID || currentRecord.ResourceVersion != record.ResourceVersion {
		return result, errors.New("approval changed after consumption")
	}
	for _, binding := range candidate.bindings(journal.UID) {
		if err = saveStage(ctx, admin, journal, "granting:"+binding.GetNamespace()+"/"+binding.GetName(), nil); err != nil {
			return result, err
		}
		if err = compareInventory(ctx, admin, target, false); err != nil {
			return result, err
		}
		if err = ValidateApproval(approval, target, time.Now().UTC()); err != nil {
			return result, err
		}
		if err = createJournaledResource(ctx, admin, journal, binding, false); err != nil {
			return result, fmt.Errorf("create reviewed controller grant: %w", err)
		}
		if binding.GetUID() == "" {
			return result, errors.New("created controller grant has no UID")
		}
		var audit Audit
		if err = json.Unmarshal([]byte(journal.Data["audit.json"]), &audit); err != nil {
			return result, err
		}
		if audit.Bindings == nil {
			audit.Bindings = map[string]string{}
		}
		audit.Bindings[bindingKey(binding)] = string(binding.GetUID())
		var encoded []byte
		if encoded, err = json.Marshal(audit); err != nil {
			return result, err
		}
		journal.Data["audit.json"] = string(encoded)
		if err = saveStage(ctx, admin, journal, "granted:"+bindingKey(binding), nil); err != nil {
			return result, err
		}
	}
	if err = saveStage(ctx, admin, journal, "starting", nil); err != nil {
		return result, err
	}
	if err = compareInventory(ctx, admin, target, false); err != nil {
		return result, err
	}
	if err = candidate.verifyBindings(ctx, admin, journal); err != nil {
		return result, err
	}
	if err = ValidateApproval(approval, target, time.Now().UTC()); err != nil {
		return result, err
	}
	if err = candidate.scale(ctx, admin, target, target.Replicas); err != nil {
		return result, err
	}
	if err = saveStage(ctx, admin, journal, "rollout", nil); err != nil {
		return result, err
	}
	err = wait.PollUntilContextTimeout(ctx, time.Second, time.Minute, true, func(ctx context.Context) (bool, error) {
		if err := ValidateApproval(approval, target, time.Now().UTC()); err != nil {
			return false, err
		}
		if err := compareInventory(ctx, admin, target, true); err != nil {
			return false, err
		}
		return candidate.rolloutReady(ctx, admin, target)
	})
	if err != nil {
		return result, fmt.Errorf("controller rollout: %w", err)
	}
	if err = candidate.verifyBindings(ctx, admin, journal); err != nil {
		return result, err
	}
	err = saveStage(ctx, admin, journal, "active", nil)
	return result, err
}

func (candidate Candidate) bindings(owner types.UID) []client.Object {
	objects := []client.Object{}
	for _, role := range candidate.roles {
		metadata := metav1.ObjectMeta{Name: role.GetName(), Namespace: role.GetNamespace(), Annotations: map[string]string{transactionOwner: string(owner)}}
		reference := rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: role.GetKind(), Name: role.GetName()}
		subjects := []rbacv1.Subject{{Kind: "ServiceAccount", Name: candidate.manifest.Controller, Namespace: candidate.manifest.Namespace}}
		if role.GetKind() == "ClusterRole" {
			objects = append(objects, &rbacv1.ClusterRoleBinding{ObjectMeta: metadata, RoleRef: reference, Subjects: subjects})
		} else {
			objects = append(objects, &rbacv1.RoleBinding{ObjectMeta: metadata, RoleRef: reference, Subjects: subjects})
		}
	}
	return objects
}

func bindingKey(binding client.Object) string {
	return fmt.Sprintf("%T/%s/%s", binding, binding.GetNamespace(), binding.GetName())
}

func (candidate Candidate) verifyBindings(ctx context.Context, admin client.Client, journal *corev1.ConfigMap) error {
	var audit Audit
	if err := decodeStrict([]byte(journal.Data["audit.json"]), &audit); err != nil {
		return err
	}
	expectedBindings := candidate.bindings(journal.UID)
	if len(audit.Bindings) != len(expectedBindings) {
		return errors.New("incomplete recorded controller grants")
	}
	for _, expected := range expectedBindings {
		live, ok := expected.DeepCopyObject().(client.Object)
		if !ok {
			return errors.New("controller grant copy does not implement client.Object")
		}
		if err := admin.Get(ctx, client.ObjectKeyFromObject(expected), live); err != nil {
			return err
		}
		if string(live.GetUID()) != audit.Bindings[bindingKey(expected)] || live.GetUID() == "" || len(live.GetOwnerReferences()) != 0 || live.GetDeletionTimestamp() != nil || live.GetAnnotations()[transactionOwner] != string(journal.UID) {
			return errors.New("controller grant ownership changed")
		}
		switch actual := live.(type) {
		case *rbacv1.RoleBinding:
			wanted, ok := expected.(*rbacv1.RoleBinding)
			if !ok || !reflect.DeepEqual(actual.RoleRef, wanted.RoleRef) || !reflect.DeepEqual(actual.Subjects, wanted.Subjects) {
				return errors.New("controller RoleBinding drift")
			}
		case *rbacv1.ClusterRoleBinding:
			wanted, ok := expected.(*rbacv1.ClusterRoleBinding)
			if !ok || !reflect.DeepEqual(actual.RoleRef, wanted.RoleRef) || !reflect.DeepEqual(actual.Subjects, wanted.Subjects) {
				return errors.New("controller ClusterRoleBinding drift")
			}
		}
	}
	roles := &rbacv1.RoleBindingList{}
	if err := admin.List(ctx, roles); err != nil {
		return err
	}
	for _, binding := range roles.Items {
		if grantsController(binding.Subjects, binding.Namespace, candidate.manifest) && audit.Bindings[bindingKey(&binding)] != string(binding.UID) {
			return errors.New("unexpected controller RoleBinding")
		}
	}
	clusters := &rbacv1.ClusterRoleBindingList{}
	if err := admin.List(ctx, clusters); err != nil {
		return err
	}
	for _, binding := range clusters.Items {
		if !grantsController(binding.Subjects, "", candidate.manifest) || audit.Bindings[bindingKey(&binding)] == string(binding.UID) {
			continue
		}
		role := &rbacv1.ClusterRole{}
		if binding.RoleRef.Kind != "ClusterRole" || binding.RoleRef.APIGroup != rbacv1.GroupName {
			return errors.New("invalid inherited controller grant")
		}
		if err := admin.Get(ctx, client.ObjectKey{Name: binding.RoleRef.Name}, role); err != nil {
			return err
		}
		if !safeSharedRole(role) {
			return errors.New("unexpected inherited controller ClusterRoleBinding")
		}
	}
	return nil
}

func compareInventory(ctx context.Context, admin client.Client, target ActivationTarget, active bool) error {
	for _, expected := range target.Objects {
		live := &unstructured.Unstructured{}
		live.SetAPIVersion(expected.APIVersion)
		live.SetKind(expected.Kind)
		if err := admin.Get(ctx, client.ObjectKey{Namespace: expected.Namespace, Name: expected.Name}, live); err != nil {
			return err
		}
		if live.GetDeletionTimestamp() != nil {
			return errors.New("activation inventory object is deleting")
		}
		if err := verifyPolicyStatus(live); err != nil {
			return err
		}
		if active && expected.Kind == "Deployment" && expected.Name == target.Deployment && expected.Namespace == target.Namespace {
			replicas, _, _ := unstructured.NestedInt64(live.Object, "spec", "replicas")
			if replicas != int64(target.Replicas) || live.GetGeneration() != expected.Generation+1 {
				return errors.New("controller replica or generation drift")
			}
			live.SetGeneration(expected.Generation)
			if err := unstructured.SetNestedField(live.Object, int64(0), "spec", "replicas"); err != nil {
				return err
			}
		}
		actual, err := snapshotObject(live)
		if err != nil {
			return err
		}
		if actual != expected {
			return fmt.Errorf("activation inventory drift: %s %s/%s", expected.Kind, expected.Namespace, expected.Name)
		}
	}
	return nil
}

func (candidate Candidate) scale(ctx context.Context, admin client.Client, target ActivationTarget, replicas int32) error {
	deployment := &unstructured.Unstructured{}
	deployment.SetAPIVersion("apps/v1")
	deployment.SetKind("Deployment")
	if err := admin.Get(ctx, client.ObjectKeyFromObject(candidate.controller), deployment); err != nil {
		return err
	}
	matched := false
	for _, object := range target.Objects {
		if object.Kind == "Deployment" && object.Name == deployment.GetName() && object.Namespace == deployment.GetNamespace() && object.UID == string(deployment.GetUID()) {
			matched = true
			if replicas != 0 {
				actual, err := snapshotObject(deployment)
				if err != nil {
					return err
				}
				if actual != object {
					return errors.New("controller changed before replica update")
				}
			}
		}
	}
	if !matched || deployment.GetDeletionTimestamp() != nil {
		return errors.New("controller Deployment UID changed or is deleting")
	}
	original := deployment.DeepCopy()
	if err := unstructured.SetNestedField(deployment.Object, int64(replicas), "spec", "replicas"); err != nil {
		return err
	}
	return admin.Patch(ctx, deployment, client.MergeFromWithOptions(original, client.MergeFromWithOptimisticLock{}))
}

func (candidate Candidate) deactivate(ctx context.Context, admin client.Client, journal *corev1.ConfigMap, target ActivationTarget) error {
	errorsFound := []error{}
	errorsFound = append(errorsFound, cleanupJournaledResources(ctx, admin, journal, false))
	errorsFound = append(errorsFound, cleanupJournaledResources(ctx, admin, journal, true))
	var audit Audit
	if err := decodeStrict([]byte(journal.Data["audit.json"]), &audit); err != nil {
		return err
	}
	if audit.ResourceJournalVersion != 1 {
		errorsFound = append(errorsFound, errors.New("journal lacks durable resource inventory; automatic recovery cannot certify inactive state"))
	}
	for _, expected := range candidate.bindings(journal.UID) {
		tracked := false
		for _, intent := range audit.Resources {
			if !intent.Probe && intent.Name == expected.GetName() && intent.Namespace == expected.GetNamespace() {
				tracked = true
				break
			}
		}
		if tracked {
			continue
		}
		live, ok := expected.DeepCopyObject().(client.Object)
		if !ok {
			errorsFound = append(errorsFound, errors.New("controller grant copy does not implement client.Object"))
			continue
		}
		if err := admin.Get(ctx, client.ObjectKeyFromObject(expected), live); apierrors.IsNotFound(err) {
			continue
		} else if err != nil {
			errorsFound = append(errorsFound, err)
			continue
		}
		if live.GetUID() == "" || len(live.GetOwnerReferences()) != 0 || live.GetAnnotations()[transactionOwner] != string(journal.UID) {
			errorsFound = append(errorsFound, errors.New("refusing to delete unowned controller grant"))
			continue
		}
		if uid, recorded := audit.Bindings[bindingKey(expected)]; recorded && uid != string(live.GetUID()) {
			errorsFound = append(errorsFound, errors.New("refusing to delete replaced controller grant"))
			continue
		}
		uid, version := live.GetUID(), live.GetResourceVersion()
		if err := admin.Delete(ctx, live, &client.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid, ResourceVersion: &version}}); err != nil && !apierrors.IsNotFound(err) {
			errorsFound = append(errorsFound, err)
		}
	}
	if err := candidate.scale(ctx, admin, target, 0); err != nil {
		errorsFound = append(errorsFound, err)
	}
	var drainErr error
	if err := wait.PollUntilContextTimeout(ctx, time.Second, 45*time.Second, true, func(ctx context.Context) (bool, error) {
		drainErr = VerifyDrained(ctx, admin, candidate.manifest)
		return drainErr == nil, nil
	}); err != nil {
		errorsFound = append(errorsFound, fmt.Errorf("inactive state not confirmed: %w", errors.Join(err, drainErr)))
	}
	return errors.Join(errorsFound...)
}
