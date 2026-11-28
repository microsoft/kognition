// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package webhook

import (
	"context"
	"errors"

	authorizationv1 "k8s.io/api/authorization/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	intadmission "github.com/kognition-ai/kognition/internal/admission"
)

// SubjectAccessReviewer answers admission authorization questions about the
// calling user with the cluster's configured authorizers.
type SubjectAccessReviewer struct {
	reviews WorkflowUseAccessReviewer
}

// NewSubjectAccessReviewer wraps a SubjectAccessReview client.
func NewSubjectAccessReviewer(reviews WorkflowUseAccessReviewer) *SubjectAccessReviewer {
	return &SubjectAccessReviewer{reviews: reviews}
}

// Allowed reports the cluster's decision. An evaluation error, a failed call or
// an empty response is an error, never an allow.
func (r *SubjectAccessReviewer) Allowed(ctx context.Context, user intadmission.UserInfo, attributes intadmission.AccessAttributes) (bool, error) {
	if r == nil || r.reviews == nil {
		return false, errors.New("SubjectAccessReview client is not configured")
	}
	extra := make(map[string]authorizationv1.ExtraValue, len(user.Extra))
	for key, values := range user.Extra {
		extra[key] = append(authorizationv1.ExtraValue(nil), values...)
	}
	review, err := r.reviews.Create(ctx, &authorizationv1.SubjectAccessReview{Spec: authorizationv1.SubjectAccessReviewSpec{
		User: user.Username, Groups: append([]string(nil), user.Groups...), Extra: extra,
		ResourceAttributes: &authorizationv1.ResourceAttributes{
			Namespace: attributes.Namespace, Group: attributes.Group, Resource: attributes.Resource, Verb: attributes.Verb, Name: attributes.Name,
		},
	}}, metav1.CreateOptions{})
	if err != nil {
		return false, err
	}
	if review == nil || review.Status.EvaluationError != "" {
		return false, errors.New("SubjectAccessReview could not be evaluated")
	}
	return review.Status.Allowed && !review.Status.Denied, nil
}

var _ intadmission.AccessReviewer = (*SubjectAccessReviewer)(nil)
