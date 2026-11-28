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

// submissionAuthorizationHooks authorize the author of anything the controller
// later starts on their behalf: a Solution target (T-0469), an AgentJob (T-0470)
// and every WorkflowRun (T-0467).
func submissionAuthorizationHooks(config *rest.Config, st store.Store) ([]admission.ValidatingHook, error) {
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("configure submission access reviews: %w", err)
	}
	reviewer := webhook.NewSubjectAccessReviewer(clientset.AuthorizationV1().SubjectAccessReviews())
	return []admission.ValidatingHook{
		admission.NewSolutionTargetAuthorizer(reviewer, st, solutionTargetExemptUsernamesFromEnv()...),
		admission.NewAgentJobAuthorizer(reviewer, controllerUsernamesFromEnv()...),
		admission.NewWorkflowRunAuthorizer(reviewer, controllerUsernamesFromEnv()...),
	}, nil
}

// controllerUsernamesFromEnv is the controller service account, the only platform
// identity that creates AgentJobs and WorkflowRuns (for Solutions, Workflows and
// Evaluations, after their own authorization).
func controllerUsernamesFromEnv() []string {
	namespace := strings.TrimSpace(os.Getenv("KOGNITION_CONTROLLER_NAMESPACE"))
	account := strings.TrimSpace(os.Getenv("KOGNITION_CONTROLLER_SERVICE_ACCOUNT"))
	if namespace == "" || account == "" || strings.ContainsAny(account, ":/") {
		return nil
	}
	return []string{fmt.Sprintf("system:serviceaccount:%s:%s", namespace, account)}
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
