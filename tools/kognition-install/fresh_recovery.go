// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/kognition-ai/kognition/internal/k8s/activation"
	"github.com/kognition-ai/kognition/internal/k8s/installation"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func runFreshRecovery(arguments []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("fresh-recover", flag.ContinueOnError)
	flags.SetOutput(stderr)
	plan := flags.String("plan", "", "Original retained first-install plan")
	approval := flags.String("record", "", "Fresh-install authorization record printed during installation")
	frozen := flags.Bool("writers-frozen", false, "Confirm no release or workload writers are running")
	stopped := flags.Bool("executors-stopped", false, "Confirm all previous installation executors have stopped")
	if err := flags.Parse(arguments); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if flags.NArg() != 0 || *plan == "" || *approval == "" || !*frozen || !*stopped {
		_, _ = fmt.Fprintln(stderr, "fresh-recover requires --plan, --record, --writers-frozen and --executors-stopped; it never reactivates a controller")
		return 2
	}
	if err := execute("fresh-recover", *plan, *approval, "", "", stdout, stderr); err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}

const activationTransactionAnnotation = "kognition.dev/activation-transaction"

// verifyFreshNeverActivated proves the controller is stopped and holds no activation grants
// when no activation journal exists to certify that state.
func verifyFreshNeverActivated(ctx context.Context, admin client.Client, manifest activation.Manifest) error {
	deployment := &appsv1.Deployment{}
	if err := admin.Get(ctx, client.ObjectKey{Namespace: manifest.Namespace, Name: manifest.Deployment}, deployment); err != nil {
		if !apierrors.IsNotFound(err) {
			return err
		}
	} else if (deployment.Spec.Replicas != nil && *deployment.Spec.Replicas != 0) || deployment.Status.Replicas != 0 {
		return errors.New("fresh recovery found a running controller without an activation transaction; scale it down and use the existing-release recovery workflow")
	}
	roles := &rbacv1.RoleBindingList{}
	if err := admin.List(ctx, roles); err != nil {
		return err
	}
	for _, binding := range roles.Items {
		if _, found := binding.Annotations[activationTransactionAnnotation]; found {
			return fmt.Errorf("fresh recovery found controller grant RoleBinding %s/%s without an activation transaction", binding.Namespace, binding.Name)
		}
	}
	clusters := &rbacv1.ClusterRoleBindingList{}
	if err := admin.List(ctx, clusters); err != nil {
		return err
	}
	for _, binding := range clusters.Items {
		if _, found := binding.Annotations[activationTransactionAnnotation]; found {
			return fmt.Errorf("fresh recovery found controller grant ClusterRoleBinding %s without an activation transaction", binding.Name)
		}
	}
	return nil
}

func recoverFresh(ctx context.Context, operations *installation.Kubernetes, config *rest.Config, originalPath, planDigest, authorization string, stdout, stderr io.Writer) error {
	if err := operations.VerifyTarget(ctx); err != nil {
		return err
	}
	expected, err := installation.ReadArtifact(operations.Plan.Manifest)
	if err != nil {
		return err
	}
	record := &corev1.ConfigMap{}
	key := client.ObjectKey{Namespace: operations.Plan.AuthorityNamespace, Name: authorization}
	if err := operations.Admin.Get(ctx, key, record); err != nil {
		return err
	}
	var proof activation.FreshBootstrapEvidence
	if err := json.Unmarshal([]byte(record.Data["fresh-bootstrap.json"]), &proof); err != nil {
		return err
	}
	if proof.Version != activation.FreshApprovalVersion || proof.PlanSHA256 != planDigest ||
		proof.ClaimName != installation.JournalKey(operations.Plan).Name || proof.ClaimUID == "" {
		return errors.New("fresh recovery authorization does not match the retained installation plan")
	}
	var authorizationRecord activation.Approval
	if err := json.Unmarshal([]byte(record.Data["approval.json"]), &authorizationRecord); err != nil {
		return err
	}
	if authorizationRecord.Version != activation.FreshApprovalVersion || authorizationRecord.Approver != "kognition-install/fresh" || len(authorizationRecord.Gates) != 0 {
		return errors.New("fresh recovery requires a first-install authorization record")
	}
	transaction := &corev1.ConfigMap{}
	if err := operations.Admin.Get(ctx, activation.TransactionKey(record.Namespace, authorizationRecord.Target), transaction); apierrors.IsNotFound(err) {
		// Activation mutates the cluster only after it creates the transaction journal, so a
		// missing journal means the controller was never activated (or recovery already completed).
		if err := verifyFreshNeverActivated(ctx, operations.Admin, operations.Manifest); err != nil {
			return err
		}
		if err := operations.FreshWorkloadsEmpty(ctx, expected, nil, "", false); err != nil {
			return err
		}
		_, _ = fmt.Fprintln(stderr, "No activation transaction exists; retiring the installation claim without reactivating the controller.")
		return execute("recover", originalPath, proof.ClaimUID, "", "", stdout, stderr)
	} else if err != nil {
		return err
	}
	var persisted activation.Audit
	if err := json.Unmarshal([]byte(transaction.Data["audit.json"]), &persisted); err != nil {
		return err
	}
	if persisted.ApprovalUID != string(record.UID) || transaction.UID == "" || persisted.ResourceJournalVersion != 1 {
		return errors.New("fresh recovery transaction does not match the authorization record")
	}
	if err := operations.FreshWorkloadsEmpty(ctx, expected, persisted.Resources, string(transaction.UID), false); err != nil {
		return err
	}
	candidate, err := activation.ParseCandidate(expected, operations.Plan.Release)
	if err != nil {
		return err
	}
	replicas, err := freshReplicaCount(operations.Plan.Values)
	if err != nil {
		return err
	}
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	binary, err := os.ReadFile(executable)
	if err != nil {
		return err
	}
	version, err := operations.Discovery.ServerVersion()
	if err != nil {
		return err
	}
	audit, err := candidate.Recover(ctx, operations.Admin, activation.ActivationOptions{
		Approval: key, Endpoint: config.Host, ServerVersion: version.GitVersion,
		ChartSHA256: operations.Plan.Chart.SHA256, ToolVersion: fmt.Sprintf("sha256:%x", sha256.Sum256(binary)),
		Replicas: replicas, WritersFrozen: true,
	}, true)
	if err != nil {
		return err
	}
	if err := json.NewEncoder(stdout).Encode(audit); err != nil {
		return err
	}
	return execute("recover", originalPath, proof.ClaimUID, "", "", stdout, stderr)
}
