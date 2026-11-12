// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package modelgateway

import (
	"context"
	"errors"
	"fmt"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/modelinvocation"
	"github.com/kognition-ai/kognition/internal/observability"
	"github.com/kognition-ai/kognition/internal/runtimeauth"
	"github.com/kognition-ai/kognition/internal/store"
	protocol "github.com/kognition-ai/kognition/proto/gen/kognition/v1alpha1"
)

// AuthorizedModel is the live Model configuration permitted for an Agent.
type AuthorizedModel struct {
	Agent      *v1alpha1.Agent
	Model      *v1alpha1.Model
	ModelClass *v1alpha1.ModelClass
	Invocation *modelinvocation.Resolved
}

// ResourceResolutionError identifies a live resource lookup or decode failure
// without conflating it with an authorization denial.
type ResourceResolutionError struct {
	Resource string
	Err      error
}

func (e *ResourceResolutionError) Error() string {
	return fmt.Sprintf("load %s: %v", e.Resource, e.Err)
}

func (e *ResourceResolutionError) Unwrap() error {
	return e.Err
}

// AuthorizationDeniedError identifies a live model-access policy denial.
type AuthorizationDeniedError struct {
	Err error
}

func (e *AuthorizationDeniedError) Error() string {
	return fmt.Sprintf("model access denied: %v", e.Err)
}

func (e *AuthorizationDeniedError) Unwrap() error {
	return e.Err
}

// Core is the shared Model Gateway authorization and resource-resolution
// boundary used by standalone callers and the Kubernetes HTTP gateway.
type Core struct {
	store      store.Store
	authorizer ModelAccessAuthorizer
}

func NewCore(store store.Store, authorizer ModelAccessAuthorizer) (*Core, error) {
	if store == nil {
		return nil, errors.New("model gateway core requires a store")
	}
	if authorizer == nil {
		return nil, errors.New("model gateway core requires a model access authorizer")
	}
	return &Core{store: store, authorizer: authorizer}, nil
}

// Authorize resolves the live Agent, Model, and ModelClass and evaluates the
// current ModelGrant and AgentPolicy state immediately before provider use.
func (c *Core) Authorize(ctx context.Context, namespace, agentName, modelName string) (*AuthorizedModel, error) {
	return c.authorize(ctx, namespace, agentName, modelName, 0, currentModel)
}

// AuthorizeAtRevision additionally requires the live Model resource version
// when expectedModelResourceVersion is positive.
func (c *Core) AuthorizeAtRevision(ctx context.Context, namespace, agentName, modelName string, expectedModelResourceVersion int64) (*AuthorizedModel, error) {
	return c.authorize(ctx, namespace, agentName, modelName, expectedModelResourceVersion, currentModel)
}

// AuthorizeExecution permits a Model selected by a persisted Agent revision.
// The current Agent is still loaded for identity and policy evaluation, but its
// current ModelRef does not override the immutable execution configuration.
func (c *Core) AuthorizeExecution(ctx context.Context, namespace, agentName, modelName string) (*AuthorizedModel, error) {
	return c.authorize(ctx, namespace, agentName, modelName, 0, executionModel)
}

type modelPurpose int

const (
	currentModel modelPurpose = iota
	executionModel
	knowledgeEmbeddingModel
)

func (c *Core) authorize(ctx context.Context, namespace, agentName, modelName string, expectedModelResourceVersion int64, purpose modelPurpose) (*AuthorizedModel, error) {
	agent, err := loadResource[v1alpha1.Agent](ctx, c.store, "Agent", namespace, agentName)
	if err != nil {
		return nil, &ResourceResolutionError{Resource: "Agent", Err: err}
	}
	if agent.ObjectMeta.DeletionTimestamp != nil || (purpose == currentModel && agent.Spec.ModelRef.Name != modelName) {
		return nil, errors.New("requested Model is no longer assigned to the Agent")
	}
	model, err := loadResource[v1alpha1.Model](ctx, c.store, "Model", namespace, modelName)
	if err != nil {
		return nil, &ResourceResolutionError{Resource: "Model", Err: err}
	}
	if model.ObjectMeta.DeletionTimestamp != nil {
		return nil, errors.New("requested Model is deleted")
	}
	if expectedModelResourceVersion > 0 && model.ObjectMeta.ResourceVersion != expectedModelResourceVersion {
		return nil, errors.New("requested Model is not the expected revision")
	}
	var modelClass *v1alpha1.ModelClass
	if model.Spec.ClassName != "" {
		modelClass, err = loadResource[v1alpha1.ModelClass](ctx, c.store, "ModelClass", "", model.Spec.ClassName)
		if err != nil {
			return nil, &ResourceResolutionError{Resource: "ModelClass", Err: err}
		}
		if _, err := observability.ResolveModelPricing(modelClass.Spec.Parameters); err != nil {
			return nil, fmt.Errorf("ModelClass %q has invalid pricing: %w", modelClass.ObjectMeta.Name, err)
		}
	}
	if err := c.authorizer.EvaluateModelAccess(ctx, namespace, agentName, modelName); err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, err
		}
		return nil, &AuthorizationDeniedError{Err: err}
	}
	if purpose == executionModel && modelClass != nil && len(modelClass.Spec.VersionCandidates) != 0 {
		if modelClass.Status.ActiveBinding == nil || modelClass.Status.ActiveBinding.Namespace != namespace {
			return nil, errors.New("active candidate is not available in this workspace")
		}
		candidate, err := runtimeauth.ResolveModelBinding(ctx, c.store, *modelClass.Status.ActiveBinding)
		if err != nil || candidate.Spec.ClassName != modelClass.ObjectMeta.Name {
			return nil, errors.New("active candidate revision is unavailable")
		}
		if err := c.authorizer.EvaluateModelAccess(ctx, namespace, agentName, candidate.ObjectMeta.Name); err != nil {
			return nil, &AuthorizationDeniedError{Err: err}
		}
		model = candidate
	}
	var invocation *modelinvocation.Resolved
	if purpose != currentModel || modelClass == nil || len(modelClass.Spec.VersionCandidates) == 0 {
		invocation, err = modelinvocation.Load(ctx, c.store, model)
		if err != nil {
			return nil, &ResourceResolutionError{Resource: "ModelInvocationProfile", Err: err}
		}
	}
	return &AuthorizedModel{Agent: agent, Model: model, ModelClass: modelClass, Invocation: invocation}, nil
}

func (c *Core) AuthorizeKnowledgeEmbedding(ctx context.Context, namespace, agentName, knowledgeBaseName string, expected *protocol.KnowledgeScope) (*AuthorizedModel, error) {
	agent, err := loadResource[v1alpha1.Agent](ctx, c.store, "Agent", namespace, agentName)
	if err != nil {
		return nil, &ResourceResolutionError{Resource: "Agent", Err: err}
	}
	if agent.DeletionTimestamp != nil || expected == nil || expected.Namespace != namespace {
		return nil, errors.New("knowledge embedding execution is not authorized")
	}
	claimed := false
	for _, reference := range agent.Spec.KnowledgeBaseClaims {
		claim, err := loadResource[v1alpha1.KnowledgeBaseClaim](ctx, c.store, "KnowledgeBaseClaim", namespace, reference.Name)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return nil, &ResourceResolutionError{Resource: "KnowledgeBaseClaim", Err: err}
		}
		if err == nil && claim.DeletionTimestamp == nil && claim.Spec.KnowledgeBaseName == knowledgeBaseName &&
			claim.Status.Phase == v1alpha1.PhaseBound && claim.Status.ObservedGeneration == claim.Generation && claim.Status.BoundKnowledgeBase == knowledgeBaseName {
			claimed = true
			break
		}
	}
	if !claimed {
		return nil, errors.New("knowledge embedding requires a current bound Agent claim")
	}
	kb, err := loadResource[v1alpha1.KnowledgeBase](ctx, c.store, "KnowledgeBase", namespace, knowledgeBaseName)
	if err != nil {
		return nil, &ResourceResolutionError{Resource: "KnowledgeBase", Err: err}
	}
	if kb.DeletionTimestamp != nil || kb.UID != expected.KnowledgeBaseUid || kb.Status.Generation != expected.Generation || kb.Status.BackendBinding == nil {
		return nil, errors.New("knowledge embedding index binding is unavailable")
	}
	backend := kb.Status.BackendBinding
	if backend.Generation != expected.Generation || backend.ConfigurationUID != expected.ConfigurationUid ||
		backend.ConfigurationDigest != expected.ConfigurationDigest || backend.OperationID != expected.OperationId || backend.EmbeddingModelBinding == nil {
		return nil, errors.New("knowledge embedding index binding changed")
	}
	class, err := loadResource[v1alpha1.KnowledgeBaseClass](ctx, c.store, "KnowledgeBaseClass", "", kb.Spec.KnowledgeBaseClassName)
	if err != nil {
		return nil, &ResourceResolutionError{Resource: "KnowledgeBaseClass", Err: err}
	}
	if class.DeletionTimestamp != nil || class.UID != backend.ClassUID || class.Generation != backend.ClassGeneration {
		return nil, errors.New("knowledge embedding class binding changed")
	}
	reference := class.Spec.EmbeddingModelRef
	if kb.Spec.IndexConfig != nil && kb.Spec.IndexConfig.EmbeddingModelRef != nil {
		reference = kb.Spec.IndexConfig.EmbeddingModelRef
	}
	binding := backend.EmbeddingModelBinding
	if reference == nil || reference.Name != binding.ModelName || binding.Namespace != namespace {
		return nil, errors.New("knowledge embedding Model reference changed")
	}
	if _, err := runtimeauth.ResolveModelConfigurationBinding(ctx, c.store, *binding); err != nil {
		return nil, err
	}
	authorized, err := c.authorize(ctx, namespace, agentName, binding.ModelName, 0, knowledgeEmbeddingModel)
	if err != nil {
		return nil, err
	}
	if authorized.ModelClass != nil && len(authorized.ModelClass.Spec.VersionCandidates) != 0 {
		return nil, errors.New("knowledge embeddings require a directly pinned Model rather than a moving class candidate")
	}
	digest, err := runtimeauth.ModelConfigurationDigest(authorized.Model)
	if err != nil || authorized.Model.UID != binding.ModelUID || digest != binding.SpecDigest ||
		(authorized.Invocation != nil && authorized.Invocation.Spec.Operation != "embedding") {
		return nil, errors.New("knowledge embedding configuration is incompatible with the index")
	}
	if authorized.Invocation == nil {
		if binding.InvocationProfileUID != "" || binding.InvocationProfileDigest != "" {
			return nil, errors.New("knowledge embedding invocation profile is unavailable")
		}
	} else if authorized.Invocation.Binding.UID != binding.InvocationProfileUID || authorized.Invocation.Binding.SpecDigest != binding.InvocationProfileDigest {
		return nil, errors.New("knowledge embedding invocation profile changed")
	}
	return authorized, nil
}

func (c *Core) Ping(ctx context.Context) error {
	return c.store.Ping(ctx)
}
