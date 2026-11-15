// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package activation

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type Audit struct {
	ApprovalUID            string            `json:"approvalUID"`
	Approval               Approval          `json:"approval"`
	Stage                  string            `json:"stage"`
	Started                time.Time         `json:"started"`
	Updated                time.Time         `json:"updated"`
	Failure                string            `json:"failure,omitempty"`
	FailedStage            string            `json:"failedStage,omitempty"`
	Bindings               map[string]string `json:"bindings,omitempty"`
	Resources              []ResourceIntent  `json:"resources,omitempty"`
	ResourceJournalVersion int               `json:"resourceJournalVersion,omitempty"`
}

func installationKey(namespace string, target ActivationTarget) client.ObjectKey {
	identity := target.ClusterUID + "/" + target.Namespace + "/" + target.Controller
	return client.ObjectKey{Namespace: namespace, Name: fmt.Sprintf("activation-%x", sha256.Sum256([]byte(identity)))}
}

func TransactionKey(namespace string, target ActivationTarget) client.ObjectKey {
	return installationKey(namespace, target)
}

func claimApproval(ctx context.Context, admin client.Client, record *corev1.ConfigMap, approval Approval, now time.Time) (*corev1.ConfigMap, error) {
	return claimAuthorization(ctx, admin, record, approval, now, ValidateApproval)
}

func claimAuthorization(ctx context.Context, admin client.Client, record *corev1.ConfigMap, approval Approval, now time.Time, validate approvalValidator) (*corev1.ConfigMap, error) {
	if err := validate(approval, approval.Target, now); err != nil {
		return nil, err
	}
	if record == nil || record.UID == "" || record.Namespace == "" {
		return nil, errors.New("persisted approval identity is required")
	}
	encoded, err := json.Marshal(Audit{ApprovalUID: string(record.UID), Approval: approval, Stage: "claimed", Started: now, Updated: now, ResourceJournalVersion: 1})
	if err != nil {
		return nil, err
	}
	claim := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: record.Namespace, Name: fmt.Sprintf("approval-used-%x", sha256.Sum256([]byte(record.UID)))}, Immutable: ptr.To(true), Data: map[string]string{"audit.json": string(encoded)}}
	if err := admin.Create(ctx, claim); err != nil {
		return nil, fmt.Errorf("approval consumption failed; never retry this approval automatically: %w", err)
	}
	key := installationKey(record.Namespace, approval.Target)
	if err := releaseRecovered(ctx, admin, key); err != nil {
		return nil, err
	}
	journal := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: key.Namespace, Name: key.Name}, Data: map[string]string{"audit.json": string(encoded)}}
	if err := admin.Create(ctx, journal); err != nil {
		return nil, fmt.Errorf("installation is locked or claim outcome is uncertain; approval remains consumed: %w", err)
	}
	if journal.UID == "" {
		return nil, errors.New("installation journal has no UID")
	}
	return journal, nil
}

func saveStage(ctx context.Context, admin client.Client, journal *corev1.ConfigMap, stage string, failure error) error {
	var audit Audit
	if err := json.Unmarshal([]byte(journal.Data["audit.json"]), &audit); err != nil {
		return err
	}
	if failure != nil && audit.FailedStage == "" {
		audit.FailedStage = audit.Stage
	}
	audit.Stage = stage
	audit.Updated = time.Now().UTC()
	if failure != nil {
		audit.Failure = failure.Error()
	}
	encoded, err := json.Marshal(audit)
	if err != nil {
		return err
	}
	updated := journal.DeepCopy()
	updated.Data["audit.json"] = string(encoded)
	if err := admin.Update(ctx, updated); err != nil {
		return fmt.Errorf("persist activation stage %s: %w", stage, err)
	}
	*journal = *updated
	return nil
}
