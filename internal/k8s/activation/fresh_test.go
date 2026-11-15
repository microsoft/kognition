// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package activation

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	authv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func freshAuthorizationFixture(t *testing.T) (Approval, *corev1.ConfigMap, *corev1.ConfigMap, []client.Object) {
	t.Helper()
	approval := approvalFixture(time.Now().UTC())
	approval.Version, approval.Approver, approval.Gates = FreshApprovalVersion, "kognition-install/fresh", nil
	approval.Target.Objects = nil
	namespaces := map[string]string{
		"kube-system": approval.Target.ClusterUID, approval.Target.Namespace: "control-uid",
		approval.Target.AuthorityNamespace: "authority-uid", approval.Target.Namespaces[0]: "tenant-uid",
	}
	var objects []client.Object
	for name, uid := range namespaces {
		approval.Target.Objects = append(approval.Target.Objects, ObjectEvidence{
			APIVersion: "v1", Kind: "Namespace", Name: name, UID: uid, SHA256: strings.Repeat("a", 64),
		})
		objects = append(objects, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name, UID: types.UID(uid)}})
	}
	proof, err := json.Marshal(FreshBootstrapEvidence{
		Version: FreshApprovalVersion, ClaimName: "fresh-claim", ClaimUID: "claim-uid",
		PlanSHA256: strings.Repeat("b", 64), NamespaceUIDs: namespaces,
	})
	if err != nil {
		t.Fatal(err)
	}
	content, err := json.Marshal(approval)
	if err != nil {
		t.Fatal(err)
	}
	record := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "fresh-authorization", Namespace: approval.Target.AuthorityNamespace, UID: "authorization-uid", ResourceVersion: "1"},
		Immutable:  ptr.To(true),
		Data:       map[string]string{"approval.json": string(content), "fresh-bootstrap.json": string(proof)},
	}
	claim := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "fresh-claim", Namespace: record.Namespace, UID: "claim-uid", ResourceVersion: "1"},
		Data: map[string]string{
			"version": "1", "mode": "fresh", "planSHA256": strings.Repeat("b", 64),
			"clusterUID": approval.Target.ClusterUID, "namespaceUID": "control-uid", "release": approval.Target.Release,
			"events": "claimed\nverified-inactive\n",
		},
	}
	return approval, record, claim, objects
}

func TestFreshAuthorizationDoesNotMasqueradeAsReleaseApproval(t *testing.T) {
	approval, _, _, _ := freshAuthorizationFixture(t)
	now := time.Now().UTC()
	if err := ValidateFreshAuthorization(approval, approval.Target, now); err != nil {
		t.Fatal(err)
	}
	if err := ValidateApproval(approval, approval.Target, now); err == nil {
		t.Fatal("fresh authorization accepted as release certification")
	}
	for _, scenario := range []string{"release-version", "gate-report", "expired", "changed-target", "missing-images"} {
		t.Run(scenario, func(t *testing.T) {
			approval, _, _, _ := freshAuthorizationFixture(t)
			target := approval.Target
			switch scenario {
			case "release-version":
				approval.Version = ApprovalVersion
			case "gate-report":
				approval.Gates = map[string]GateEvidence{"SG-07": {}}
			case "expired":
				approval.ExpiresAt = now.Add(-time.Minute)
			case "changed-target":
				target.Replicas++
			case "missing-images":
				target.Images = nil
			}
			if err := ValidateFreshAuthorization(approval, target, time.Now().UTC()); err == nil {
				t.Fatal("invalid fresh authorization accepted")
			}
		})
	}
}

func TestFreshBootstrapRequiresCompletedClaimAndExactNamespaces(t *testing.T) {
	for _, scenario := range []string{"ready", "upgrade", "unfinished", "replaced-claim", "plan-drift", "namespace-replaced", "claim-missing", "mutable-record", "wrong-authority", "foreign-owner"} {
		t.Run(scenario, func(t *testing.T) {
			approval, record, claim, objects := freshAuthorizationFixture(t)
			switch scenario {
			case "mutable-record":
				record.Immutable = ptr.To(false)
			case "wrong-authority":
				record.Namespace = "other"
			case "foreign-owner":
				record.OwnerReferences = []metav1.OwnerReference{{UID: "foreign"}}
			case "upgrade":
				delete(claim.Data, "mode")
			case "unfinished":
				claim.Data["events"] = "claimed\nverify-bootstrap:pending\n"
			case "replaced-claim":
				claim.UID = "replacement"
			case "plan-drift":
				claim.Data["planSHA256"] = strings.Repeat("c", 64)
			case "namespace-replaced":
				for _, object := range objects {
					if object.GetName() == approval.Target.Namespace {
						object.SetUID("replacement")
					}
				}
			}
			if scenario != "claim-missing" {
				objects = append(objects, claim)
			}
			admin := fake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(objects...).Build()
			err := VerifyFreshBootstrap(context.Background(), admin, record, approval)
			if scenario == "ready" && err != nil {
				t.Fatal(err)
			}
			if scenario != "ready" && err == nil {
				t.Fatal("invalid bootstrap proof accepted")
			}
		})
	}
}

func TestStandardActivationRejectsFreshAuthorizationBeforeGrants(t *testing.T) {
	approval, record, _, _ := freshAuthorizationFixture(t)
	writes := 0
	admin := fake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(record).WithInterceptorFuncs(interceptor.Funcs{
		Create: func(ctx context.Context, underlying client.WithWatch, object client.Object, options ...client.CreateOption) error {
			if _, review := object.(*authv1.SubjectAccessReview); review {
				return nil
			}
			writes++
			return underlying.Create(ctx, object, options...)
		},
	}).Build()
	candidate := Candidate{manifest: Manifest{Namespace: approval.Target.Namespace, Namespaces: approval.Target.Namespaces, Controller: approval.Target.Controller}}
	_, err := candidate.Activate(context.Background(), admin, &rest.Config{Host: approval.Target.Endpoint}, ActivationOptions{
		Approval: client.ObjectKeyFromObject(record), Endpoint: approval.Target.Endpoint, WritersFrozen: true,
	})
	if err == nil || writes != 0 {
		t.Fatalf("ordinary activation accepted fresh authorization or mutated resources: error=%v writes=%d", err, writes)
	}
}
