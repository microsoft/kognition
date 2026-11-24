// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"fmt"
	"os"
	"strings"

	"go.uber.org/zap"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"

	apiv1alpha1 "github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/admission"
	"github.com/kognition-ai/kognition/internal/collaboration/jobtransport"
	"github.com/kognition-ai/kognition/internal/collaboration/runner"
	intctrl "github.com/kognition-ai/kognition/internal/controller"
	"github.com/kognition-ai/kognition/internal/store"
)

// workflowCollaborationEnabledEnvironmentVariable enables collaboration steps. It is disabled unless "true".
const workflowCollaborationEnabledEnvironmentVariable = "KOGNITION_WORKFLOW_COLLABORATION_ENABLED"

// workflowCollaborationEnabled reports whether collaboration steps are enabled.
func workflowCollaborationEnabled(lookup func(string) string) (bool, error) {
	switch strings.TrimSpace(lookup(workflowCollaborationEnabledEnvironmentVariable)) {
	case "", "false":
		return false, nil
	case "true":
		return true, nil
	}
	return false, fmt.Errorf("%s must be true or false", workflowCollaborationEnabledEnvironmentVariable)
}

// workflowCollaborationRunnerFromEnv returns nil, nil unless collaboration is enabled. Every member task runs as
// an AgentJob through the same path as ordinary workflow steps, and the strategy state lives in a ConfigMap
// ledger beside the run.
func workflowCollaborationRunnerFromEnv(mgr ctrl.Manager, st store.Store, policy intctrl.CommunicationPolicyAuthorizer, logger *zap.Logger) (intctrl.WorkflowCollaborationRunner, error) {
	enabled, err := workflowCollaborationEnabled(os.Getenv)
	if err != nil || !enabled {
		return nil, err
	}
	if st == nil {
		return nil, fmt.Errorf("workflow collaboration requires the resource store")
	}
	clientset, err := kubernetes.NewForConfig(mgr.GetConfig())
	if err != nil {
		return nil, fmt.Errorf("create collaboration clientset: %w", err)
	}
	instance, err := newWorkflowCollaborationRunner(st, clientset.CoreV1().ConfigMaps, policy)
	if err != nil {
		return nil, err
	}
	logger.Info("workflow collaboration enabled")
	return instance, nil
}

func newWorkflowCollaborationRunner(st store.Store, configMaps func(string) corev1client.ConfigMapInterface, policy intctrl.CommunicationPolicyAuthorizer) (*runner.Runner, error) {
	factory := jobtransport.Factory{Store: st}
	if policy != nil {
		factory.Authorize = policy.EvaluateDispatch
	}
	return runner.New(runner.Config{ConfigMaps: configMaps, Transports: factory, Owners: ledgerOwners})
}

// ledgerOwners makes the ledger ConfigMap a dependent of the Workflow or WorkflowRun, so it is garbage-collected
// with it, exactly like the member AgentJobs.
func ledgerOwners(run intctrl.WorkflowCollaborationRun) []metav1.OwnerReference {
	return []metav1.OwnerReference{{APIVersion: apiv1alpha1.APIVersion, Kind: run.OwnerKind, Name: run.RunName, UID: types.UID(run.RunUID)}}
}

// enableCollaborationAdmission lets the Workflow validator admit collaboration steps. It reports whether a
// validator was found.
func enableCollaborationAdmission(hooks []admission.ValidatingHook) bool {
	found := false
	for i, hook := range hooks {
		if _, ok := hook.(*admission.WorkflowValidator); ok {
			hooks[i] = admission.NewWorkflowValidator(admission.WithCollaborationEnabled(true))
			found = true
		}
	}
	return found
}
