// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"fmt"
	"os"
	"strings"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/kognition-ai/kognition/internal/admission"
	"github.com/kognition-ai/kognition/internal/k8s/webhook"
	"github.com/kognition-ai/kognition/internal/store"
)

// solutionTargetAuthorizationHook authorizes the author of a Solution target
// with the cluster's authorizers (T-0469).
func solutionTargetAuthorizationHook(config *rest.Config, st store.Store) (admission.ValidatingHook, error) {
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("configure Solution target access reviews: %w", err)
	}
	reviewer := webhook.NewSubjectAccessReviewer(clientset.AuthorizationV1().SubjectAccessReviews())
	return admission.NewSolutionTargetAuthorizer(reviewer, st, solutionTargetExemptUsernamesFromEnv()...), nil
}

// solutionTargetExemptUsernamesFromEnv names the operator-bound service accounts
// (for example the intent gateway) that may create Solution targets without
// holding the run permissions themselves. Only unqualified names in the
// controller namespace are accepted.
func solutionTargetExemptUsernamesFromEnv() []string {
	namespace := strings.TrimSpace(os.Getenv("KOGNITION_CONTROLLER_NAMESPACE"))
	if namespace == "" {
		return nil
	}
	var usernames []string
	for _, account := range strings.Split(os.Getenv("KOGNITION_SOLUTION_TARGET_EXEMPT_SERVICE_ACCOUNTS"), ",") {
		account = strings.TrimSpace(account)
		if account == "" || strings.ContainsAny(account, ":/") {
			continue
		}
		usernames = append(usernames, fmt.Sprintf("system:serviceaccount:%s:%s", namespace, account))
	}
	return usernames
}
