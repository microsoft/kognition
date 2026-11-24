package auth

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"time"

	api "github.com/kognition-ai/kognition/api/v1alpha1"
	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const WorkflowExecutorTokenAudience = "kognition.dev/workflow-executor"

type WorkflowExecutorTokenReviewer interface {
	Create(context.Context, *authenticationv1.TokenReview, metav1.CreateOptions) (*authenticationv1.TokenReview, error)
}

type workflowExecutorAuthenticator struct {
	base       Authenticator
	principals map[api.ControlPlaneRoleSubject]bool
	reviews    WorkflowExecutorTokenReviewer
	reader     client.Reader
}

func NewWorkflowExecutorAuthenticator(base Authenticator, principals []api.ControlPlaneRoleSubject, reviews WorkflowExecutorTokenReviewer, reader client.Reader) (Authenticator, error) {
	if base == nil || reviews == nil || reader == nil || len(principals) == 0 || len(principals) > 16 {
		return nil, fmt.Errorf("executor attestation requires authentication, bounded principals and live Kubernetes proof")
	}
	configured := make(map[api.ControlPlaneRoleSubject]bool, len(principals))
	for _, principal := range principals {
		issuer, err := url.Parse(principal.Issuer)
		parts := strings.Split(principal.Subject, ":")
		if principal.Method != "oidc" || err != nil || issuer.Scheme != "https" || issuer.Host == "" || issuer.User != nil || issuer.RawQuery != "" || issuer.Fragment != "" ||
			len(parts) != 4 || parts[0] != "system" || parts[1] != "serviceaccount" || len(validation.IsDNS1123Label(parts[2])) != 0 || len(validation.IsDNS1123Subdomain(parts[3])) != 0 || configured[principal] {
			return nil, fmt.Errorf("executor attestation requires unique exact Kubernetes OIDC principals")
		}
		configured[principal] = true
	}
	return &workflowExecutorAuthenticator{base: base, principals: configured, reviews: reviews, reader: reader}, nil
}

func (authenticator *workflowExecutorAuthenticator) Authenticate(ctx context.Context, token string) (*Identity, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	id, err := authenticator.base.Authenticate(ctx, token)
	if err != nil || id == nil {
		if err == nil {
			err = ErrInvalidToken
		}
		return nil, err
	}
	identity := *id
	identity.workflowControllerProof = nil
	principal := api.ControlPlaneRoleSubject{Method: identity.Method, Issuer: identity.Issuer, Subject: identity.Subject}
	if !authenticator.principals[principal] {
		return &identity, nil
	}
	if len(token) == 0 || len(token) > 64<<10 {
		return nil, ErrInvalidToken
	}
	proof := func(ctx context.Context) (string, error) {
		return authenticator.verifyController(ctx, token, principal.Subject)
	}
	if _, err := proof(ctx); err != nil {
		return nil, err
	}
	identity.workflowControllerProof = proof
	return &identity, nil
}

func (authenticator *workflowExecutorAuthenticator) verifyController(ctx context.Context, token, subject string) (string, error) {
	return verifyPodBoundServiceAccountToken(ctx, authenticator.reviews, authenticator.reader, WorkflowExecutorTokenAudience, token, subject)
}

// verifyPodBoundServiceAccountToken proves a live, Ready, pod-bound token of exactly the named ServiceAccount minted for audience.
func verifyPodBoundServiceAccountToken(ctx context.Context, reviews WorkflowExecutorTokenReviewer, reader client.Reader, audience, token, subject string) (string, error) {
	bounded, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := bounded.Err(); err != nil {
		return "", err
	}
	review, err := reviews.Create(bounded, &authenticationv1.TokenReview{Spec: authenticationv1.TokenReviewSpec{
		Token: token, Audiences: []string{audience},
	}}, metav1.CreateOptions{})
	if err != nil {
		return "", fmt.Errorf("executor TokenReview unavailable: %w", err)
	}
	if review == nil || !review.Status.Authenticated || review.Status.Error != "" || review.Status.User.Username != subject || review.Status.User.UID == "" {
		return "", ErrInvalidToken
	}
	matchedAudience := false
	for _, reviewed := range review.Status.Audiences {
		matchedAudience = matchedAudience || reviewed == audience
	}
	if !matchedAudience {
		return "", ErrInvalidToken
	}
	for _, group := range review.Status.User.Groups {
		if group == "system:unauthenticated" {
			return "", ErrInvalidToken
		}
	}
	names := review.Status.User.Extra["authentication.kubernetes.io/pod-name"]
	uids := review.Status.User.Extra["authentication.kubernetes.io/pod-uid"]
	if len(names) != 1 || len(validation.IsDNS1123Subdomain(names[0])) != 0 || len(uids) != 1 || uids[0] == "" || len(uids[0]) > 128 || strings.ContainsAny(uids[0], "\x00\r\n\t /\\*") {
		return "", ErrInvalidToken
	}
	parts := strings.Split(subject, ":")
	pod := &corev1.Pod{}
	if err := reader.Get(bounded, client.ObjectKey{Namespace: parts[2], Name: names[0]}, pod); err != nil {
		return "", fmt.Errorf("executor Pod proof unavailable: %w", err)
	}
	ready := false
	for _, condition := range pod.Status.Conditions {
		ready = ready || condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue
	}
	if pod.Namespace != parts[2] || pod.Name != names[0] || string(pod.UID) != uids[0] || pod.DeletionTimestamp != nil ||
		pod.Spec.ServiceAccountName != parts[3] || pod.Status.Phase != corev1.PodRunning || !ready {
		return "", ErrInvalidToken
	}
	account := &corev1.ServiceAccount{}
	if err := reader.Get(bounded, client.ObjectKey{Namespace: parts[2], Name: parts[3]}, account); err != nil {
		return "", fmt.Errorf("executor ServiceAccount proof unavailable: %w", err)
	}
	if account.Namespace != parts[2] || account.Name != parts[3] || account.DeletionTimestamp != nil || string(account.UID) != review.Status.User.UID || account.UID == pod.UID {
		return "", ErrInvalidToken
	}
	return uids[0], bounded.Err()
}
