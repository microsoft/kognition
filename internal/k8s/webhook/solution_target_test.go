// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package webhook

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	admissionv1 "k8s.io/api/admission/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	intadmission "github.com/kognition-ai/kognition/internal/admission"
)

type recordedAccessReview struct {
	response *authorizationv1.SubjectAccessReview
	err      error
	specs    []authorizationv1.SubjectAccessReviewSpec
}

func (r *recordedAccessReview) Create(_ context.Context, review *authorizationv1.SubjectAccessReview, _ metav1.CreateOptions) (*authorizationv1.SubjectAccessReview, error) {
	r.specs = append(r.specs, review.Spec)
	return r.response, r.err
}

func TestSubjectAccessReviewerAsksTheClusterAboutTheCaller(t *testing.T) {
	reviews := &recordedAccessReview{response: &authorizationv1.SubjectAccessReview{Status: authorizationv1.SubjectAccessReviewStatus{Allowed: true}}}
	allowed, err := NewSubjectAccessReviewer(reviews).Allowed(context.Background(),
		intadmission.UserInfo{Username: "alice", Groups: []string{"builders"}, Extra: map[string][]string{"scopes": {"a"}}},
		intadmission.AccessAttributes{Namespace: "team-a", Group: "kognition.dev", Resource: "workflows", Verb: "use", Name: "flow"})
	if err != nil || !allowed {
		t.Fatalf("allowed = %v, err = %v", allowed, err)
	}
	spec := reviews.specs[0]
	want := authorizationv1.ResourceAttributes{Namespace: "team-a", Group: "kognition.dev", Resource: "workflows", Verb: "use", Name: "flow"}
	if spec.User != "alice" || len(spec.Groups) != 1 || spec.ResourceAttributes == nil || *spec.ResourceAttributes != want || len(spec.Extra["scopes"]) != 1 {
		t.Fatalf("SubjectAccessReview spec = %+v", spec)
	}
}

func TestSubjectAccessReviewerNeverTurnsUncertaintyIntoAnAllow(t *testing.T) {
	tests := []struct {
		name        string
		response    *authorizationv1.SubjectAccessReview
		err         error
		wantAllowed bool
		wantError   bool
	}{
		{name: "denied", response: &authorizationv1.SubjectAccessReview{}},
		{name: "explicit denial overrides allow", response: &authorizationv1.SubjectAccessReview{Status: authorizationv1.SubjectAccessReviewStatus{Allowed: true, Denied: true}}},
		{name: "evaluation error even when allowed", response: &authorizationv1.SubjectAccessReview{Status: authorizationv1.SubjectAccessReviewStatus{Allowed: true, EvaluationError: "authorizer down"}}, wantError: true},
		{name: "call failure", err: errors.New("unavailable"), wantError: true},
		{name: "nil response", wantError: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			allowed, err := NewSubjectAccessReviewer(&recordedAccessReview{response: test.response, err: test.err}).Allowed(
				context.Background(), intadmission.UserInfo{Username: "alice"}, intadmission.AccessAttributes{Resource: "agents", Verb: "invoke"})
			if allowed != test.wantAllowed || (err != nil) != test.wantError {
				t.Fatalf("allowed = %v, err = %v", allowed, err)
			}
		})
	}
}

func TestHandleDeniesASolutionTargetTheCallerMayNotStart(t *testing.T) {
	solution := func(username string) admission.Request {
		raw, err := json.Marshal(map[string]any{
			"apiVersion": "kognition.dev/v1alpha1", "kind": "Solution",
			"metadata": map[string]any{"name": "s", "namespace": "team-a"},
			"spec":     map[string]any{"goal": "run it", "target": map[string]any{"kind": "Workflow", "ref": map[string]any{"name": "flow"}}},
		})
		if err != nil {
			t.Fatal(err)
		}
		return admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{
			UID: "solution", Operation: admissionv1.Create, Namespace: "team-a", Name: "s",
			Kind:     metav1.GroupVersionKind{Group: "kognition.dev", Version: "v1alpha1", Kind: "Solution"},
			Object:   runtime.RawExtension{Raw: raw},
			UserInfo: authenticationv1.UserInfo{Username: username, Groups: []string{"builders"}},
		}}
	}
	chain := func(reviews *recordedAccessReview) *intadmission.Chain {
		return intadmission.NewChain(nil, []intadmission.ValidatingHook{
			intadmission.NewSolutionTargetAuthorizer(NewSubjectAccessReviewer(reviews), nil),
		})
	}

	denying := &recordedAccessReview{response: &authorizationv1.SubjectAccessReview{}}
	if response := NewHandler(chain(denying), testScheme(), testLogger()).Handle(context.Background(), solution("builder")); response.Allowed {
		t.Fatal("a Solution target the caller may not start was admitted")
	}
	if len(denying.specs) == 0 || denying.specs[0].User != "builder" {
		t.Fatalf("the caller was not the subject of the review: %+v", denying.specs)
	}

	allowing := &recordedAccessReview{response: &authorizationv1.SubjectAccessReview{Status: authorizationv1.SubjectAccessReviewStatus{Allowed: true}}}
	if response := NewHandler(chain(allowing), testScheme(), testLogger()).Handle(context.Background(), solution("consumer")); !response.Allowed {
		t.Fatalf("an authorized Solution target was denied: %v", response.Result)
	}
}
