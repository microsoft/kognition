package controller

import (
	"context"
	"fmt"
	"strings"

	authenticationv1 "k8s.io/api/authentication/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	api "github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/controlplaneauth"
	"github.com/kognition-ai/kognition/internal/runtimeapproval"
	"github.com/kognition-ai/kognition/internal/store"
)

const WorkflowExecutorKubernetesAudience = "kognition.dev/workflow-executor"

type WorkflowExecutorTokenReviewer interface {
	Create(context.Context, *authenticationv1.TokenReview, metav1.CreateOptions) (*authenticationv1.TokenReview, error)
}

type WorkflowExecutorAccessReviewer interface {
	Create(context.Context, *authorizationv1.SubjectAccessReview, metav1.CreateOptions) (*authorizationv1.SubjectAccessReview, error)
}

type WorkflowExecutorKubernetesAuthorization struct {
	TokenReviews      WorkflowExecutorTokenReviewer
	AccessReviews     WorkflowExecutorAccessReviewer
	Credential        func(context.Context) (string, error)
	Subject           string
	WorkloadUID       string
	ServiceAccountUID string
	PodNamespace      string
	PodName           string
	Pods              client.Reader
}

func (s *StoreAdapter) ConfigureWorkflowExecutorAuthorization(config WorkflowExecutorKubernetesAuthorization) error {
	if config.TokenReviews == nil || config.AccessReviews == nil || config.Credential == nil ||
		config.Subject == "" || config.Subject == "system:anonymous" || config.WorkloadUID == "" || config.ServiceAccountUID == "" ||
		config.PodNamespace == "" || config.PodName == "" || config.Pods == nil {
		return runtimeapproval.ErrWorkflowDispatchUnsupported
	}
	s.workflowExecutorAuthorization = &config
	return nil
}

func (s *StoreAdapter) workflowExecutorUser(ctx context.Context) (authenticationv1.UserInfo, error) {
	if err := ctx.Err(); err != nil {
		return authenticationv1.UserInfo{}, err
	}
	config := s.workflowExecutorAuthorization
	if config == nil {
		return authenticationv1.UserInfo{}, runtimeapproval.ErrWorkflowDispatchUnsupported
	}
	token, err := config.Credential(ctx)
	if err != nil {
		return authenticationv1.UserInfo{}, err
	}
	if token == "" {
		return authenticationv1.UserInfo{}, controlplaneauth.ErrForbidden
	}
	review, err := config.TokenReviews.Create(ctx, &authenticationv1.TokenReview{Spec: authenticationv1.TokenReviewSpec{
		Token: token, Audiences: []string{WorkflowExecutorKubernetesAudience}}}, metav1.CreateOptions{})
	if err != nil {
		return authenticationv1.UserInfo{}, fmt.Errorf("executor authentication unavailable: %w", err)
	}
	if review == nil || !review.Status.Authenticated || review.Status.Error != "" ||
		review.Status.User.Username != config.Subject || review.Status.User.UID != config.ServiceAccountUID {
		return authenticationv1.UserInfo{}, controlplaneauth.ErrForbidden
	}
	matchedAudience := false
	for _, audience := range review.Status.Audiences {
		matchedAudience = matchedAudience || audience == WorkflowExecutorKubernetesAudience
	}
	if !matchedAudience {
		return authenticationv1.UserInfo{}, controlplaneauth.ErrForbidden
	}
	for _, group := range review.Status.User.Groups {
		if group == "system:unauthenticated" {
			return authenticationv1.UserInfo{}, controlplaneauth.ErrForbidden
		}
	}
	podNames := review.Status.User.Extra["authentication.kubernetes.io/pod-name"]
	podUIDs := review.Status.User.Extra["authentication.kubernetes.io/pod-uid"]
	if len(podNames) != 1 || podNames[0] != config.PodName || len(podUIDs) != 1 || podUIDs[0] != config.WorkloadUID {
		return authenticationv1.UserInfo{}, controlplaneauth.ErrForbidden
	}
	subject := strings.Split(config.Subject, ":")
	if len(subject) != 4 || subject[0] != "system" || subject[1] != "serviceaccount" || subject[2] != config.PodNamespace || subject[3] == "" {
		return authenticationv1.UserInfo{}, controlplaneauth.ErrForbidden
	}
	pod := &corev1.Pod{}
	if err := config.Pods.Get(ctx, client.ObjectKey{Namespace: config.PodNamespace, Name: config.PodName}, pod); err != nil {
		return authenticationv1.UserInfo{}, fmt.Errorf("executor pod proof unavailable: %w", err)
	}
	ready := false
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue {
			ready = true
		}
	}
	if string(pod.UID) != config.WorkloadUID || pod.DeletionTimestamp != nil || pod.Spec.ServiceAccountName != subject[3] ||
		pod.Status.Phase != corev1.PodRunning || !ready {
		return authenticationv1.UserInfo{}, controlplaneauth.ErrForbidden
	}
	return review.Status.User, ctx.Err()
}

func (s *StoreAdapter) AuthenticatedWorkflowControllerUID(ctx context.Context) (string, error) {
	if _, err := s.workflowExecutorUser(ctx); err != nil {
		return "", err
	}
	return s.workflowExecutorAuthorization.WorkloadUID, nil
}

func (s *StoreAdapter) AuthenticatedWorkflowExecutorUser(ctx context.Context) (authenticationv1.UserInfo, error) {
	return s.workflowExecutorUser(ctx)
}

func (s *StoreAdapter) AuthenticatedWorkflowExecutor(ctx context.Context) (api.ControlPlaneRoleSubject, error) {
	user, err := s.workflowExecutorUser(ctx)
	if err != nil {
		return api.ControlPlaneRoleSubject{}, err
	}
	return api.ControlPlaneRoleSubject{Method: "tokenreview", Issuer: "kubernetes.io/tokenreview", Subject: user.Username}, nil
}

func (s *StoreAdapter) AuthorizeWorkflowExecutorTarget(ctx context.Context, action runtimeapproval.WorkflowExecutorAction) error {
	return runtimeapproval.AuthorizeWorkflowExecutorTarget(ctx, s, action,
		func(ctx context.Context, action runtimeapproval.WorkflowExecutorAction) error {
			user, err := s.workflowExecutorUser(ctx)
			if err != nil {
				return err
			}
			executor := api.ControlPlaneRoleSubject{Method: "tokenreview", Issuer: "kubernetes.io/tokenreview", Subject: user.Username}
			if executor != action.Executor {
				return controlplaneauth.ErrForbidden
			}
			for _, reference := range []api.RuntimeApprovalReference{action.Target, action.Service} {
				if err := runtimeapproval.AuthorizeWorkflowExecutorReference(ctx, s, action.Activation.Workspace, reference); err != nil {
					return err
				}
			}
			extra := make(map[string]authorizationv1.ExtraValue, len(user.Extra))
			for key, values := range user.Extra {
				extra[key] = authorizationv1.ExtraValue(values)
			}
			review, err := s.workflowExecutorAuthorization.AccessReviews.Create(ctx, &authorizationv1.SubjectAccessReview{
				Spec: authorizationv1.SubjectAccessReviewSpec{User: user.Username, UID: user.UID, Groups: user.Groups, Extra: extra,
					ResourceAttributes: &authorizationv1.ResourceAttributes{Namespace: action.Target.Namespace, Verb: "invoke",
						Group: "kognition.dev", Resource: "agents", Name: action.Target.Name}}}, metav1.CreateOptions{})
			if err != nil {
				return fmt.Errorf("executor invocation authorization unavailable: %w", err)
			}
			if review == nil || review.Status.EvaluationError != "" {
				return controlplaneauth.ErrUnavailable
			}
			if !review.Status.Allowed || review.Status.Denied {
				return controlplaneauth.ErrForbidden
			}
			return ctx.Err()
		})
}

func (s *StoreAdapter) ConfigureWorkflowExecutorReader(reader client.Reader) error {
	if reader == nil {
		return fmt.Errorf("workflow executor requires an uncached resource reader")
	}
	s.workflowExecutorReader = reader
	return nil
}

func (s *StoreAdapter) GetWorkflowExecutorResource(ctx context.Context, kind, namespace, name string) ([]byte, error) {
	if s.workflowExecutorReader == nil {
		return nil, fmt.Errorf("workflow executor uncached resource reader is not configured")
	}
	if kind != "Agent" && kind != "AgentService" && kind != "Workflow" {
		return nil, fmt.Errorf("unsupported workflow executor target kind %q", kind)
	}
	object, err := s.newObject(kind)
	if err != nil {
		return nil, err
	}
	if err := s.workflowExecutorReader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, object); err != nil {
		if client.IgnoreNotFound(err) == nil {
			return nil, store.ErrNotFound
		}
		return nil, err
	}
	return s.marshalToInternal(kind, object)
}

func (s *StoreAdapter) AuthorizeWorkflowExecutorStep(ctx context.Context, authority runtimeapproval.WorkflowExecutorAuthority, action runtimeapproval.WorkflowExecutorAction) error {
	return runtimeapproval.AuthorizeWorkflowExecutorStep(ctx, s, authority, action, s.AuthorizeWorkflowExecutorTarget)
}

func (s *StoreAdapter) ListWorkflowExecutorResources(ctx context.Context, kind, namespace string) ([][]byte, error) {
	if s.workflowExecutorReader == nil || namespace == "" || kind != "AgentService" && kind != "CrossNamespaceGrant" {
		return nil, runtimeapproval.ErrWorkflowDispatchUnsupported
	}
	list, err := s.newListObject(kind)
	if err != nil {
		return nil, err
	}
	if err := s.workflowExecutorReader.List(ctx, list, client.InNamespace(namespace), client.Limit(257)); err != nil {
		return nil, err
	}
	if list.GetContinue() != "" {
		return nil, fmt.Errorf("workflow executor discovery exceeds its finite bound")
	}
	result, err := s.marshalListToInternal(kind, list)
	if err != nil {
		return nil, err
	}
	if len(result) > 256 {
		return nil, fmt.Errorf("workflow executor discovery exceeds its finite bound")
	}
	return result, nil
}
