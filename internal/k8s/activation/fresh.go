// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package activation

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const FreshApprovalVersion = "kognition.dev/fresh-install/v1"

// FreshBootstrapEvidence identifies a completed first-install journal, not release certification.
type FreshBootstrapEvidence struct {
	Version       string            `json:"version"`
	ClaimName     string            `json:"claimName"`
	ClaimUID      string            `json:"claimUID"`
	PlanSHA256    string            `json:"planSHA256"`
	NamespaceUIDs map[string]string `json:"namespaceUIDs"`
}

// ActivateFresh uses the normal behavioral probes, inventory checks, journal and compensation.
func (candidate Candidate) ActivateFresh(ctx context.Context, admin client.Client, config *rest.Config, options ActivationOptions) (Audit, error) {
	if candidate.manifest.StdioIsolationProfile != "cilium" {
		return Audit{}, errors.New("automatic fresh installation requires the Cilium isolation profile")
	}
	return candidate.activate(ctx, admin, config, options, true)
}

func ValidateFreshAuthorization(approval Approval, target ActivationTarget, now time.Time) error {
	if approval.Version != FreshApprovalVersion || approval.Approver != "kognition-install/fresh" || len(approval.Gates) != 0 {
		return errors.New("fresh installation requires its own authorization, without synthetic release-gate reports")
	}
	if approval.IssuedAt.IsZero() || approval.IssuedAt.After(now) || !approval.ExpiresAt.After(now) || !approval.ExpiresAt.After(approval.IssuedAt) || approval.ExpiresAt.Sub(approval.IssuedAt) > ApprovalLifetime {
		return errors.New("fresh-install authorization is expired, future-dated or too long-lived")
	}
	if err := validateTarget(target); err != nil {
		return err
	}
	if !reflect.DeepEqual(approval.Target, target) {
		return errors.New("fresh-install target changed")
	}
	return nil
}

func VerifyFreshBootstrap(ctx context.Context, admin client.Client, record *corev1.ConfigMap, approval Approval) error {
	if record == nil || record.UID == "" || record.Immutable == nil || !*record.Immutable || record.DeletionTimestamp != nil || len(record.OwnerReferences) != 0 ||
		record.Namespace != approval.Target.AuthorityNamespace {
		return errors.New("fresh-install authorization record is required")
	}
	if err := ValidateFreshAuthorization(approval, approval.Target, time.Now().UTC()); err != nil {
		return err
	}
	var proof FreshBootstrapEvidence
	if err := decodeStrict([]byte(record.Data["fresh-bootstrap.json"]), &proof); err != nil {
		return err
	}
	if proof.Version != FreshApprovalVersion || proof.ClaimName == "" || proof.ClaimUID == "" || !validSHA256(proof.PlanSHA256) {
		return errors.New("fresh-install bootstrap evidence is incomplete")
	}
	expected := map[string]string{}
	for _, object := range approval.Target.Objects {
		if object.APIVersion == "v1" && object.Kind == "Namespace" {
			expected[object.Name] = object.UID
		}
	}
	if len(expected) != len(approval.Target.Namespaces)+3 || !reflect.DeepEqual(expected, proof.NamespaceUIDs) || expected["kube-system"] != approval.Target.ClusterUID {
		return errors.New("fresh-install namespace identities differ from the activation target")
	}
	journal := &corev1.ConfigMap{}
	if err := admin.Get(ctx, client.ObjectKey{Namespace: record.Namespace, Name: proof.ClaimName}, journal); err != nil {
		return err
	}
	if string(journal.UID) != proof.ClaimUID || journal.ResourceVersion == "" || journal.DeletionTimestamp != nil || len(journal.OwnerReferences) != 0 || len(journal.Finalizers) != 0 ||
		journal.Data["version"] != "1" || journal.Data["mode"] != "fresh" || journal.Data["planSHA256"] != proof.PlanSHA256 ||
		journal.Data["clusterUID"] != approval.Target.ClusterUID || journal.Data["namespaceUID"] != expected[approval.Target.Namespace] || journal.Data["release"] != approval.Target.Release ||
		!strings.HasSuffix(journal.Data["events"], "\nverified-inactive\n") {
		return errors.New("fresh installation has no matching completed bootstrap claim")
	}
	for name, uid := range expected {
		namespace := &corev1.Namespace{}
		if err := admin.Get(ctx, client.ObjectKey{Name: name}, namespace); err != nil {
			return err
		}
		if string(namespace.UID) != uid || namespace.DeletionTimestamp != nil {
			return fmt.Errorf("fresh-install namespace changed: %s", name)
		}
	}
	return nil
}
