// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package activation

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"

	authv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func readApproval(ctx context.Context, admin client.Client, key client.ObjectKey, manifest Manifest) (*corev1.ConfigMap, Approval, error) {
	var approval Approval
	if key.Name == "" || key.Namespace == "" || key.Namespace == manifest.Namespace || slices.Contains(manifest.Namespaces, key.Namespace) {
		return nil, approval, errors.New("approval must reside in a separate administrator-owned namespace")
	}
	record := &corev1.ConfigMap{}
	if err := admin.Get(ctx, key, record); err != nil {
		return nil, approval, err
	}
	if record.UID == "" || record.Immutable == nil || !*record.Immutable || record.DeletionTimestamp != nil || len(record.OwnerReferences) != 0 {
		return nil, approval, errors.New("approval requires an immutable, independent, live ConfigMap with a UID")
	}
	decoder := json.NewDecoder(bytes.NewBufferString(record.Data["approval.json"]))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&approval); err != nil {
		return nil, approval, err
	}
	if err := decoder.Decode(new(interface{})); err != io.EOF {
		return nil, approval, errors.New("approval must contain exactly one JSON object")
	}
	if approval.Target.AuthorityNamespace != key.Namespace {
		return nil, approval, errors.New("approval authority does not match target")
	}
	journal := &corev1.ConfigMap{}
	if err := admin.Get(ctx, installationKey(key.Namespace, approval.Target), journal); err != nil && !apierrors.IsNotFound(err) {
		return nil, approval, err
	}
	for _, resource := range []string{"configmaps", "namespaces"} {
		names := []string{"", key.Name, installationKey(key.Namespace, approval.Target).Name, fmt.Sprintf("approval-used-%x", sha256.Sum256([]byte(record.UID)))}
		if journal.UID != "" {
			names = append(names, fmt.Sprintf("activation-history-%s", journal.UID))
		}
		if resource == "namespaces" {
			names = []string{key.Namespace}
		}
		for _, name := range names {
			for _, verb := range []string{"create", "update", "patch", "delete", "deletecollection"} {
				attributes := &authv1.ResourceAttributes{Namespace: key.Namespace, Resource: resource, Name: name, Verb: verb}
				if resource == "namespaces" {
					attributes.Namespace = ""
					attributes.Name = key.Namespace
				}
				review := &authv1.SubjectAccessReview{Spec: authv1.SubjectAccessReviewSpec{
					User:               "system:serviceaccount:" + manifest.Namespace + ":" + manifest.Controller,
					Groups:             []string{"system:serviceaccounts", "system:serviceaccounts:" + manifest.Namespace, "system:authenticated"},
					ResourceAttributes: attributes,
				}}
				if err := admin.Create(ctx, review); err != nil {
					return nil, approval, err
				}
				if review.Status.Allowed || review.Status.EvaluationError != "" {
					return nil, approval, fmt.Errorf("controller can modify approval authority or authorization is indeterminate: %s %s", verb, resource)
				}
			}
		}
	}
	return record, approval, nil
}
