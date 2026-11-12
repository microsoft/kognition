// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package dispatcher

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	stderrors "errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	strictjson "sigs.k8s.io/json"

	k8sapi "github.com/kognition-ai/kognition/api/k8s/v1alpha1"
	"github.com/kognition-ai/kognition/api/v1alpha1"
	intctrl "github.com/kognition-ai/kognition/internal/controller"
	"github.com/kognition-ai/kognition/internal/convert"
	"github.com/kognition-ai/kognition/internal/cpi"
	k8scost "github.com/kognition-ai/kognition/internal/k8s/cost"
	"github.com/kognition-ai/kognition/internal/modelinvocation"
	"github.com/kognition-ai/kognition/internal/observability"
	"github.com/kognition-ai/kognition/internal/plugin"
	"github.com/kognition-ai/kognition/internal/runtime/recovery"
	"github.com/kognition-ai/kognition/internal/runtimeauth"
	"github.com/kognition-ai/kognition/internal/store"
	pluginv1alpha1 "github.com/kognition-ai/kognition/proto/gen/kognition/v1alpha1"
)

const (
	labelManagedBy              = "app.kubernetes.io/managed-by"
	labelAgent                  = "kognition.dev/agent"
	labelAgentName              = "kognition.dev/agent-name"
	labelAgentNamespace         = "kognition.dev/agent-namespace"
	labelInstance               = "kognition.dev/agent-instance"
	managedByValue              = "kognition"
	agentPort                   = 8080
	defaultImage                = "localhost:5000/kognition/agent-runtime:latest"
	knowledgeGatewayCAConfigMap = "kognition-knowledge-gateway-ca"
	knowledgeGatewayCAFile      = "/var/run/kognition/knowledge-gateway/ca.crt"
	statusPollTimeout           = 5 * time.Second
	invokeReadyTimeout          = 90 * time.Second
	invokeBusyTimeout           = 10 * time.Minute
	invokePollInterval          = 2 * time.Second
	invokeRequestTimeout        = 30 * time.Second
	capabilityActivationTimeout = 30 * time.Second
	maxRuntimeResponseBodyBytes = 1 << 20
	AnnotationOwnerAgent        = "kognition.dev/owner-agent"
	annotationOwnerAgentService = "kognition.dev/owner-agent-service"
	labelAgentServiceWorkload   = "kognition.dev/agent-service-workload"
	labelAgentServiceRevision   = "kognition.dev/agent-service-revision"
)

// Config holds configuration for the K8s deployment dispatcher.
type Config struct {
	// AgentImage is the container image for agent runtime pods.
	AgentImage string
	// RuntimeImages maps non-native runtime classes to their adapter images.
	RuntimeImages            map[string]string
	RuntimeTLSSecretName     string
	RuntimeCAFile            string
	RuntimeRecoveryKeysFile  string
	RuntimeRecoveryRetention time.Duration
	// AgentNamespace overrides the namespace for agent deployments.
	// If empty, uses the AgentInstance's namespace.
	AgentNamespace string
	// MemoryServiceURL is the in-cluster base URL of the memory service. When
	// set and an agent enables memory, it is injected into the runtime pod so
	// the memory_recall/memory_store tools can reach the service.
	MemoryServiceURL string
	// MemoryPolicyGatewayURL is the runtime-facing URL for authenticated memory
	// data-plane requests. When set, it replaces MemoryServiceURL in agent pods.
	MemoryPolicyGatewayURL string
	// MemoryPolicyGatewayPort is the port allowed to the memory policy gateway
	// by dynamically managed runtime NetworkPolicies.
	MemoryPolicyGatewayPort int
	// MemoryCapabilityIssuer signs per-session capabilities verified by the
	// memory policy gateway. It must be configured whenever the gateway URL is
	// configured.
	MemoryCapabilityIssuer *runtimeauth.MemoryCapabilityIssuer
	// ToolGatewayURL is the runtime-facing Tool Gateway capability and
	// invocation endpoint.
	ToolGatewayURL       string
	ToolGatewayPort      int
	ToolGatewayNamespace string
	// ToolSessionCapabilityIssuer signs non-executable capabilities exchanged
	// by the Tool Gateway after model selection.
	ToolSessionCapabilityIssuer *runtimeauth.ToolSessionCapabilityIssuer
	// ModelGatewayURL is the runtime-facing Model Gateway endpoint. When set,
	// the dispatcher issues a model-revision-bound capability per session.
	ModelGatewayURL              string
	ModelGatewayPort             int
	ModelGatewayNamespace        string
	ModelSessionCapabilityIssuer *runtimeauth.ModelSessionCapabilityIssuer
	// KnowledgeGatewayURL is the runtime-facing provider-neutral query endpoint.
	KnowledgeGatewayURL              string
	KnowledgeGatewayPort             int
	KnowledgeGatewayNamespace        string
	KnowledgeGatewayCASecretName     string
	KnowledgeGatewayCASecretKey      string
	KnowledgeSessionCapabilityIssuer *runtimeauth.KnowledgeSessionCapabilityIssuer
	// AgentCommunicationGatewayURL is the runtime-facing capability gateway
	// for AgentService delegation.
	AgentCommunicationGatewayURL           string
	AgentCommunicationGatewayPort          int
	AgentCommunicationGatewayNamespace     string
	AgentDelegationSessionCapabilityIssuer *runtimeauth.AgentDelegationSessionCapabilityIssuer
	// RuntimeAuthToken authenticates controller requests to runtime /invoke and
	// /status endpoints. It is static per Agent Deployment, never per session.
	RuntimeAuthToken string
	// RuntimeAuthTokenSecretName and RuntimeAuthTokenSecretKey identify the
	// same token for pod-side injection without embedding the secret value in a
	// Deployment template readable through the Kubernetes API.
	RuntimeAuthTokenSecretName string
	RuntimeAuthTokenSecretKey  string
	WorkloadIdentityMode       string
	WorkloadProxyURL           string
	WorkloadIdentityDelivery   *WorkloadIdentityDelivery
	// RuntimeNetworkPolicyEnabled controls dynamic runtime NetworkPolicies.
	// Enforcing-CNI deployments must also provide KubeletProbeIngressCIDRs so
	// host-originated readiness and liveness probes remain reachable.
	RuntimeNetworkPolicyEnabled bool
	KubeletProbeIngressCIDRs    []string
	CiliumHostProbeIngress      bool
	// ControllerNamespace and MemoryPolicyGatewayNamespace constrain dynamic
	// NetworkPolicy peers to the namespaces that host those control-plane pods.
	ControllerNamespace          string
	MemoryPolicyGatewayNamespace string
}

// K8sDispatcher implements RuntimeDispatcher by creating K8s Deployments.
// It creates one Deployment per Agent (shared across instances) and dispatches
// sessions via HTTP to the running pods.
type K8sDispatcher struct {
	client                client.Client
	store                 store.Store
	logger                *zap.Logger
	config                Config
	http                  *http.Client
	invokeHTTP            *http.Client
	runtimePluginSelector RuntimePluginSelector
	invocationsMu         sync.Mutex
	invocations           map[string]*invocationControl
	invocationTimings     map[string]invocationTiming
	externalEndpoints     map[string]string
	autogenSessions       sync.Map
	recoveryStore         store.RuntimeRecoveryStore
	recoveryCipher        *recovery.Cipher
	recoveryRetention     time.Duration
}

type invocationControl struct {
	cancel context.CancelFunc
	done   chan struct{}

	mu                  sync.Mutex
	accepted            bool
	authoritativelyBusy bool
}

type invocationTiming struct {
	pending    bool
	acceptedAt time.Time
}

// RuntimePluginSelector resolves ready runtime plugins by their declared name.
type RuntimePluginSelector interface {
	GetReadyPlugin(capability plugin.CapabilityType, name string) (*plugin.PluginConnection, error)
}

// RevisionWorkload identifies the dedicated Deployment created for an
// AgentService pinned to an immutable Agent revision.
type RevisionWorkload struct {
	Name          string
	Namespace     string
	Selector      map[string]string
	ReadyReplicas int32
}

// NewK8sDispatcher creates a new K8s deployment dispatcher.
func NewK8sDispatcher(c client.Client, s store.Store, logger *zap.Logger, cfg Config) *K8sDispatcher {
	if cfg.AgentImage == "" {
		cfg.AgentImage = defaultImage
	}
	dispatcher := &K8sDispatcher{
		client: c,
		store:  s,
		logger: logger,
		config: cfg,
		http: &http.Client{
			Timeout: statusPollTimeout,
		},
		invokeHTTP: &http.Client{
			Timeout: invokeRequestTimeout,
		},
		invocations: make(map[string]*invocationControl),
	}
	if cfg.RuntimeRecoveryKeysFile != "" {
		var recoveryStorage store.RuntimeRecoveryStore
		if persistent, ok := s.(store.RuntimeRecoveryStore); ok {
			recoveryStorage = persistent
		}
		if c != nil {
			if storage, err := k8scost.NewConfigMapStore(c, cfg.ControllerNamespace); err == nil {
				recoveryStorage = storage
			}
		}
		if err := dispatcher.ConfigureRuntimeRecoveryFile(recoveryStorage, cfg.RuntimeRecoveryKeysFile, cfg.RuntimeRecoveryRetention); err != nil {
			logger.Error("runtime recovery unavailable; AutoGen effects remain disabled")
		}
	}
	return dispatcher
}

// SetRuntimePluginSelector enables runtime classes to resolve to ready gRPC plugins.
func (d *K8sDispatcher) SetRuntimePluginSelector(selector RuntimePluginSelector) {
	d.runtimePluginSelector = selector
}

// UsesModelGateway reports whether dispatches must carry a model-session
// capability and therefore require an automatic AgentIdentity.
func (d *K8sDispatcher) UsesModelGateway() bool {
	return d != nil && d.config.ModelGatewayURL != ""
}

// UsesAgentCommunicationGateway reports whether dispatches carry a
// delegation-session capability and therefore require an automatic AgentIdentity.
func (d *K8sDispatcher) UsesAgentCommunicationGateway() bool {
	return d != nil && d.config.AgentCommunicationGatewayURL != ""
}

// UsesKnowledgeGateway reports whether dispatches may carry a knowledge-session
// capability and therefore require an automatic AgentIdentity.
func (d *K8sDispatcher) UsesKnowledgeGateway() bool {
	return d != nil && d.config.KnowledgeGatewayURL != ""
}

// Dispatch ensures the Agent's Deployment and Service exist, then invokes a
// session on the agent pods via HTTP.
func (d *K8sDispatcher) Dispatch(ctx context.Context, instance *v1alpha1.AgentInstance) (string, error) {
	return d.dispatch(ctx, instance, "")
}

// DispatchTo dispatches with a best-effort preference for one same-Agent pod.
func (d *K8sDispatcher) DispatchTo(ctx context.Context, instance *v1alpha1.AgentInstance, preferredReplica string) (string, error) {
	return d.dispatch(ctx, instance, preferredReplica)
}

func (d *K8sDispatcher) dispatch(ctx context.Context, instance *v1alpha1.AgentInstance, preferredReplica string) (string, error) {
	// CR lookups always use the instance's own namespace (Agent/Model/etc. live
	// alongside the AgentInstance). The runtime Deployment/Service may live in
	// a different namespace when AgentNamespace is configured.
	crNamespace := instance.ObjectMeta.Namespace
	runtimeNamespace := crNamespace
	if d.config.AgentNamespace != "" {
		runtimeNamespace = d.config.AgentNamespace
	}

	// Look up parent Agent.
	if d.externalEndpoints != nil {
		if _, _, err := d.agentServiceEndpoint(ctx, instance); err != nil {
			return "", err
		}
	}
	d.logger.Info("dispatcher: looking up agent",
		zap.String("crNamespace", crNamespace),
		zap.String("agentRefName", instance.Spec.AgentRef.Name),
		zap.String("instanceNamespace", instance.ObjectMeta.Namespace),
		zap.String("instanceName", instance.ObjectMeta.Name),
	)
	agent, err := intctrl.ResolveExecutionAgent(ctx, d.store, crNamespace, instance)
	if err != nil {
		if d.client == nil {
			return "", fmt.Errorf("lookup agent %q: %w", instance.Spec.AgentRef.Name, err)
		}
		// Store lookup can miss when the controller cache has not yet observed a
		// freshly-admitted Agent. Fall back to a direct client.Get and, if the
		// Agent is actually present, convert and use it. Only surface the error
		// when neither path finds it.
		var direct k8sapi.Agent
		directErr := d.client.Get(ctx, client.ObjectKey{Namespace: crNamespace, Name: instance.Spec.AgentRef.Name}, &direct)
		if directErr == nil {
			d.logger.Info("dispatcher: agent found via direct client after store miss",
				zap.String("agent", instance.Spec.AgentRef.Name),
				zap.String("namespace", crNamespace),
				zap.NamedError("storeErr", err),
			)
			agent = convert.AgentFromK8s(&direct)
			if instance.Spec.AgentRevision != nil {
				revision, revisionErr := d.store.GetRevision(ctx, "Agent", crNamespace, agent.ObjectMeta.Name, *instance.Spec.AgentRevision)
				if revisionErr != nil {
					return "", fmt.Errorf("lookup Agent revision %d for %q: %w", *instance.Spec.AgentRevision, agent.ObjectMeta.Name, revisionErr)
				}
				var snapshot v1alpha1.Agent
				if unmarshalErr := json.Unmarshal(revision.Data, &snapshot); unmarshalErr != nil {
					return "", fmt.Errorf("unmarshal Agent revision %d for %q: %w", *instance.Spec.AgentRevision, agent.ObjectMeta.Name, unmarshalErr)
				}
				snapshot.ObjectMeta = agent.ObjectMeta
				snapshot.Status = agent.Status
				agent = &snapshot
			}
		} else {
			d.logger.Error("dispatcher: agent lookup failed",
				zap.String("crNamespace", crNamespace),
				zap.String("agentRefName", instance.Spec.AgentRef.Name),
				zap.Error(err),
				zap.NamedError("directClientErr", directErr),
			)
			return "", fmt.Errorf("lookup agent %q: %w", instance.Spec.AgentRef.Name, err)
		}
	}
	if err := d.validateModelGatewayInvocationLifetime(agent); err != nil {
		return "", err
	}
	if _, err := d.resolveAgentKBs(ctx, crNamespace, agent.Spec.KnowledgeBaseClaims); err != nil {
		return "", err
	}
	if err := d.validateAgentDelegationGatewayInvocationLifetime(agent); err != nil {
		return "", err
	}
	if sessionID, handled, err := d.dispatchRuntimePlugin(ctx, crNamespace, agent, instance); handled {
		return sessionID, err
	}
	solution, solutionRev, err := d.solutionAttribution(ctx, crNamespace, agent)
	if err != nil {
		return "", err
	}

	// Look up Model (may not exist as a K8s resource; use defaults if missing).
	model, err := getResource[v1alpha1.Model](ctx, d.store, "Model", crNamespace, agent.Spec.ModelRef.Name)
	if err != nil && !stderrors.Is(err, store.ErrNotFound) {
		return "", fmt.Errorf("lookup model %q: %w", agent.Spec.ModelRef.Name, err)
	}
	if model == nil {
		// Model resource not found; synthesize from agent's modelRef name.
		model = &v1alpha1.Model{}
		model.Spec.ModelID = agent.Spec.ModelRef.Name
	}

	// Determine provider: prefer ModelSpec.Provider, fallback to ModelClass.Provisioner.
	// ModelClass is cluster-scoped, so namespace is empty for that lookup.
	provider := model.Spec.Provider
	if provider == "" && model.Spec.ClassName != "" {
		modelClass, err := getResource[v1alpha1.ModelClass](ctx, d.store, "ModelClass", "", model.Spec.ClassName)
		if err != nil && !stderrors.Is(err, store.ErrNotFound) {
			return "", fmt.Errorf("lookup model class %q: %w", model.Spec.ClassName, err)
		}
		if modelClass != nil {
			provider = modelClass.Spec.Provisioner
		}
	}
	provider = normalizeModelGatewayProvider(provider)

	apiKey := ""
	var credEnvs []corev1.EnvVar
	if d.config.ModelGatewayURL != "" {
		if err := requireModelGatewayProvider(provider, model); err != nil {
			return "", err
		}
	} else {
		// Direct runtime execution is the compatibility path when Model Gateway
		// is disabled. Gateway mode never projects provider credentials into Pods.
		for key, value := range instance.Spec.SessionContext {
			if strings.HasPrefix(key, "model.") {
				apiKey = value
				break
			}
		}
		credEnvs = d.resolveCredentialEnv(ctx, crNamespace, model)
	}
	if endpoint, routed, routeErr := d.agentServiceEndpoint(ctx, instance); routeErr != nil {
		return "", routeErr
	} else if routed {
		runtimeConfiguration := ""
		if d.externalEndpoints != nil {
			var configErr error
			runtimeConfiguration, configErr = d.externalConfiguration(ctx, endpoint, crNamespace, agent, model)
			if configErr != nil {
				return "", configErr
			}
		}
		sessionID := d.sessionID(instance)
		memoryCapability, capabilityErr := d.issueMemoryCapability(ctx, crNamespace, agent, instance, sessionID)
		if capabilityErr != nil {
			return "", capabilityErr
		}
		toolSessionCapability, capabilityErr := d.issueToolSessionCapability(agent, instance, sessionID, solution, solutionRev)
		if capabilityErr != nil {
			return "", capabilityErr
		}
		modelSessionCapability, capabilityErr := d.issueModelSessionCapability(ctx, agent, model, instance, sessionID, solution, solutionRev)
		if capabilityErr != nil {
			return "", capabilityErr
		}
		knowledgeSessionCapability, capabilityErr := d.issueKnowledgeSessionCapability(ctx, crNamespace, agent, instance, sessionID)
		if capabilityErr != nil {
			return "", capabilityErr
		}
		delegationSessionCapability, capabilityErr := d.issueAgentDelegationSessionCapability(agent, instance, sessionID, solution, solutionRev)
		if capabilityErr != nil {
			return "", capabilityErr
		}
		input := instance.Spec.SessionContext["input"]
		if input == "" {
			d.logger.Info("dispatch skipping /invoke: empty input",
				zap.String("agent", agent.ObjectMeta.Name),
				zap.String("instance", instance.ObjectMeta.Name),
			)
			return sessionID, nil
		}
		if d.externalEndpoints != nil {
			if agent.Spec.Runtime != nil && strings.TrimSpace(agent.Spec.Runtime.Class) == "autogen" {
				return d.dispatchAutoGen(ctx, instance, agent, model, endpoint, sessionID, runtimeConfiguration, solution, solutionRev)
			}
			d.invokeExternal(instance, endpoint, sessionID, runtimeConfiguration, memoryCapability, toolSessionCapability, modelSessionCapability, knowledgeSessionCapability, delegationSessionCapability)
			return sessionID, nil
		}
		go d.invokeAfterSessionActivation(crNamespace, instance.ObjectMeta.Name, endpoint, agent.ObjectMeta.Name, instance.ObjectMeta.Name, sessionID, input,
			instance.ObjectMeta.Annotations[observability.AnnotationTraceparent],
			instance.Spec.SessionContext[intctrl.AgentIdentityContextKey],
			memoryCapability,
			toolSessionCapability,
			modelSessionCapability,
			delegationSessionCapability,
			instance.Spec.SessionContext["conversation_id"],
			knowledgeSessionCapability,
		)
		return sessionID, nil
	}

	// Build environment variables for the agent pod.
	envVars, err := d.buildEnvVars(ctx, crNamespace, agent, model, instance, provider, apiKey)
	if err != nil {
		return "", err
	}
	envVars = mergeEnvVars(envVars, credEnvs)
	workloadKey := d.workloadKey(crNamespace, agent.ObjectMeta.Name)
	if err := d.ensureRuntimeResources(ctx, runtimeNamespace, agent, envVars); err != nil {
		return "", err
	}

	sessionID := d.sessionID(instance)
	if agent.Spec.Runtime != nil && strings.TrimSpace(agent.Spec.Runtime.Class) == "autogen" {
		endpoint := fmt.Sprintf("https://%s.%s.svc.cluster.local:%d/invoke", serviceName(workloadKey), runtimeNamespace, agentPort)
		configuration, err := d.externalConfiguration(ctx, endpoint, crNamespace, agent, model)
		if err != nil {
			return "", err
		}
		return d.dispatchAutoGen(ctx, instance, agent, model, endpoint, sessionID, configuration, solution, solutionRev)
	}
	memoryCapability, err := d.issueMemoryCapability(ctx, crNamespace, agent, instance, sessionID)
	if err != nil {
		return "", err
	}
	toolSessionCapability, err := d.issueToolSessionCapability(agent, instance, sessionID, solution, solutionRev)
	if err != nil {
		return "", err
	}
	modelSessionCapability, err := d.issueModelSessionCapability(ctx, agent, model, instance, sessionID, solution, solutionRev)
	if err != nil {
		return "", err
	}
	knowledgeSessionCapability, err := d.issueKnowledgeSessionCapability(ctx, crNamespace, agent, instance, sessionID)
	if err != nil {
		return "", err
	}
	delegationSessionCapability, err := d.issueAgentDelegationSessionCapability(agent, instance, sessionID, solution, solutionRev)
	if err != nil {
		return "", err
	}
	d.logger.Info("dispatched agent instance",
		zap.String("agent", agent.ObjectMeta.Name),
		zap.String("instance", instance.ObjectMeta.Name),
		zap.String("sessionID", sessionID),
		zap.String("namespace", runtimeNamespace),
	)

	input := instance.Spec.SessionContext["input"]
	if input == "" {
		// Warm-pool / idle instance: deployment is materialized but do not invoke.
		// Invoking with an empty message would burn a session on a no-op LLM call and
		// race subsequent work-driven instances for the same Deployment.
		d.logger.Info("dispatch skipping /invoke: empty input",
			zap.String("agent", agent.ObjectMeta.Name),
			zap.String("instance", instance.ObjectMeta.Name),
		)
		return sessionID, nil
	}
	d.trackInvocation(sessionID)
	go d.invokeWhenReady(
		runtimeNamespace,
		workloadKey,
		agent.ObjectMeta.Name,
		instance.ObjectMeta.Name,
		sessionID,
		input,
		instance.ObjectMeta.Annotations[observability.AnnotationTraceparent],
		preferredReplica,
		instance.Spec.SessionContext[intctrl.AgentIdentityContextKey],
		memoryCapability,
		toolSessionCapability,
		modelSessionCapability,
		delegationSessionCapability,
		instance.Spec.SessionContext["conversation_id"],
		knowledgeSessionCapability,
	)

	return sessionID, nil
}

// RuntimeReplicas returns pods associated with the immutable Agent reference.
// Readiness remains explicit so the scheduler can enforce eligibility itself.
func (d *K8sDispatcher) RuntimeReplicas(ctx context.Context, instance *v1alpha1.AgentInstance) ([]intctrl.RuntimeReplica, error) {
	if d.externalEndpoints != nil || instance == nil || instance.Spec.AgentRef.Name == "" || instance.ObjectMeta.Annotations[intctrl.AgentServiceExecutionAnnotation] != "" {
		return nil, nil
	}
	runtimeNamespace := instance.ObjectMeta.Namespace
	if d.config.AgentNamespace != "" {
		runtimeNamespace = d.config.AgentNamespace
	}
	workloadKey := d.workloadKey(instance.ObjectMeta.Namespace, instance.Spec.AgentRef.Name)
	var pods corev1.PodList
	if err := d.client.List(ctx, &pods,
		client.InNamespace(runtimeNamespace),
		client.MatchingLabels{
			labelManagedBy: managedByValue,
			labelAgent:     workloadKey,
			labelAgentName: instance.Spec.AgentRef.Name,
		}); err != nil {
		return nil, fmt.Errorf("list runtime replicas: %w", err)
	}
	replicas := make([]intctrl.RuntimeReplica, 0, len(pods.Items))
	for i := range pods.Items {
		pod := &pods.Items[i]
		replicas = append(replicas, intctrl.RuntimeReplica{
			Name:      pod.Name,
			AgentName: pod.Labels[labelAgentName],
			Ready:     runtimePodReady(pod),
		})
	}
	return replicas, nil
}

func (d *K8sDispatcher) dispatchRuntimePlugin(ctx context.Context, namespace string, agent *v1alpha1.Agent, instance *v1alpha1.AgentInstance) (string, bool, error) {
	if d.runtimePluginSelector == nil || agent.Spec.Runtime == nil {
		return "", false, nil
	}
	runtimeClass := strings.TrimSpace(agent.Spec.Runtime.Class)
	if runtimeClass == "" || runtimeClass == "default" || runtimeClass == "native" {
		return "", false, nil
	}
	connection, err := d.runtimePluginSelector.GetReadyPlugin(plugin.CapabilityRuntime, runtimeClass)
	if err != nil {
		return "", false, nil
	}
	if connection == nil || connection.Conn == nil {
		return "", true, fmt.Errorf("runtime plugin %q has no active gRPC connection", runtimeClass)
	}
	response, err := pluginv1alpha1.NewRuntimeClient(connection.Conn).CreateSession(ctx, &pluginv1alpha1.CreateSessionRequest{
		AgentName: agent.ObjectMeta.Name,
		Namespace: namespace,
		Context:   instance.Spec.SessionContext,
	})
	if err != nil {
		return "", true, fmt.Errorf("runtime plugin %q create session: %w", runtimeClass, err)
	}
	if response.GetSessionId() == "" {
		return "", true, fmt.Errorf("runtime plugin %q returned an empty session ID", runtimeClass)
	}
	return response.GetSessionId(), true, nil
}

func (d *K8sDispatcher) issueToolSessionCapability(agent *v1alpha1.Agent, instance *v1alpha1.AgentInstance, sessionID, solution string, solutionRev int64) (string, error) {
	if d.config.ToolGatewayURL == "" || len(agent.Spec.Tools) == 0 {
		return "", nil
	}
	if d.config.ToolSessionCapabilityIssuer == nil {
		return "", stderrors.New("tool gateway URL requires a tool session capability issuer")
	}
	identityName := instance.Spec.SessionContext[intctrl.ToolCapabilityIdentityNameContextKey]
	if identityName == "" {
		return "", stderrors.New("tool gateway requires a controller-managed AgentIdentity")
	}
	capability, err := d.config.ToolSessionCapabilityIssuer.Issue(runtimeauth.ToolSessionCapabilityBinding{
		Namespace: instance.ObjectMeta.Namespace, AgentName: agent.ObjectMeta.Name, AgentIdentityName: identityName,
		InstanceName: instance.ObjectMeta.Name, InstanceUID: instance.ObjectMeta.UID, SessionID: sessionID, Solution: solution, SolutionRev: solutionRev,
	})
	if err != nil {
		return "", fmt.Errorf("issue runtime tool session capability: %w", err)
	}
	return capability, nil
}

func (d *K8sDispatcher) issueModelSessionCapability(ctx context.Context, agent *v1alpha1.Agent, model *v1alpha1.Model, instance *v1alpha1.AgentInstance, sessionID, solution string, solutionRev int64) (string, error) {
	if d.config.ModelGatewayURL == "" {
		return "", nil
	}
	if d.config.ModelSessionCapabilityIssuer == nil {
		return "", stderrors.New("model gateway URL requires a model session capability issuer")
	}
	if model == nil || model.ObjectMeta.Name == "" || model.ObjectMeta.ResourceVersion <= 0 {
		return "", stderrors.New("model gateway requires a persisted Model resource with a resource version")
	}
	identityName := instance.Spec.SessionContext[intctrl.ModelCapabilityIdentityNameContextKey]
	if identityName == "" {
		return "", stderrors.New("model gateway requires a controller-managed AgentIdentity")
	}
	var candidateBinding *v1alpha1.ModelVersionBinding
	if model.Spec.ClassName != "" {
		raw, err := d.store.Get(ctx, "ModelClass", "", model.Spec.ClassName)
		if err != nil {
			return "", stderrors.New("model class is unavailable for capability binding")
		}
		var class v1alpha1.ModelClass
		if json.Unmarshal(raw, &class) != nil {
			return "", stderrors.New("invalid model class for capability binding")
		}
		if len(class.Spec.VersionCandidates) != 0 {
			if class.Status.ActiveBinding == nil || class.Status.ActiveBinding.Namespace != instance.ObjectMeta.Namespace {
				return "", stderrors.New("no serving candidate is active in this workspace")
			}
			if _, err := runtimeauth.ResolveModelBinding(ctx, d.store, *class.Status.ActiveBinding); err != nil {
				return "", err
			}
			candidateBinding = class.Status.ActiveBinding
		}
	}
	servingModel := model
	if candidateBinding != nil {
		resolved, err := runtimeauth.ResolveModelBinding(ctx, d.store, *candidateBinding)
		if err != nil {
			return "", err
		}
		servingModel = resolved
	}
	invocation, err := modelinvocation.Load(ctx, d.store, servingModel)
	if err != nil {
		return "", fmt.Errorf("resolve runtime invocation profile: %w", err)
	}
	var invocationBinding *v1alpha1.ModelInvocationProfileBinding
	if invocation != nil {
		invocationBinding = &invocation.Binding
	}
	capability, err := d.config.ModelSessionCapabilityIssuer.Issue(runtimeauth.ModelSessionCapabilityBinding{
		Namespace: instance.ObjectMeta.Namespace, AgentName: agent.ObjectMeta.Name, AgentIdentityName: identityName,
		InstanceName: instance.ObjectMeta.Name, InstanceUID: instance.ObjectMeta.UID, SessionID: sessionID, Solution: solution, SolutionRev: solutionRev,
		WorkflowName: instance.ObjectMeta.Annotations[observability.AnnotationWorkflowName], StepName: instance.ObjectMeta.Annotations[observability.AnnotationStepName],
		ModelName: model.ObjectMeta.Name, ModelResourceVersion: model.ObjectMeta.ResourceVersion,
		CandidateBinding:         candidateBinding,
		InvocationProfileBinding: invocationBinding,
	})
	if err != nil {
		return "", fmt.Errorf("issue runtime model session capability: %w", err)
	}
	return capability, nil
}

func (d *K8sDispatcher) issueKnowledgeSessionCapability(ctx context.Context, namespace string, agent *v1alpha1.Agent, instance *v1alpha1.AgentInstance, sessionID string) (string, error) {
	if d.config.KnowledgeGatewayURL == "" || len(agent.Spec.KnowledgeBaseClaims) == 0 {
		return "", nil
	}
	if d.config.KnowledgeSessionCapabilityIssuer == nil {
		return "", stderrors.New("knowledge gateway URL requires a knowledge session capability issuer")
	}
	resolved, err := d.resolveAgentKBs(ctx, namespace, agent.Spec.KnowledgeBaseClaims)
	if err != nil {
		return "", err
	}
	knowledgeBases := make([]string, 0, len(resolved))
	for _, record := range resolved {
		if record.ProviderBacked {
			knowledgeBases = append(knowledgeBases, record.Name)
		}
	}
	if len(knowledgeBases) == 0 {
		return "", nil
	}
	sort.Strings(knowledgeBases)
	identityName := instance.Spec.SessionContext[intctrl.KnowledgeCapabilityIdentityNameContextKey]
	if identityName == "" {
		return "", stderrors.New("knowledge gateway requires a controller-managed AgentIdentity")
	}
	identity, err := getResource[v1alpha1.AgentIdentity](ctx, d.store, "AgentIdentity", namespace, identityName)
	if err != nil {
		return "", fmt.Errorf("resolve Knowledge Gateway AgentIdentity: %w", err)
	}
	if identity == nil || identity.ObjectMeta.UID == "" || identity.Spec.AgentRef.Name != agent.ObjectMeta.Name {
		return "", stderrors.New("knowledge gateway requires a persisted AgentIdentity for the active agent")
	}
	if agent.ObjectMeta.UID == "" {
		return "", stderrors.New("knowledge gateway requires a persisted Agent")
	}
	capability, err := d.config.KnowledgeSessionCapabilityIssuer.Issue(runtimeauth.KnowledgeSessionCapabilityBinding{
		Namespace: instance.ObjectMeta.Namespace, AgentName: agent.ObjectMeta.Name, AgentUID: agent.ObjectMeta.UID,
		AgentIdentityName: identityName, AgentIdentityUID: identity.ObjectMeta.UID,
		InstanceName: instance.ObjectMeta.Name, InstanceUID: instance.ObjectMeta.UID, SessionID: sessionID, KnowledgeBases: knowledgeBases,
	})
	if err != nil {
		return "", fmt.Errorf("issue runtime knowledge session capability: %w", err)
	}
	return capability, nil
}

func (d *K8sDispatcher) issueAgentDelegationSessionCapability(agent *v1alpha1.Agent, instance *v1alpha1.AgentInstance, sessionID, solution string, solutionRev int64) (string, error) {
	if d.config.AgentCommunicationGatewayURL == "" {
		return "", nil
	}
	if d.config.AgentDelegationSessionCapabilityIssuer == nil {
		return "", stderrors.New("agent communication gateway URL requires a delegation session capability issuer")
	}
	identityName := instance.Spec.SessionContext[intctrl.AgentDelegationCapabilityIdentityNameContextKey]
	if identityName == "" {
		return "", stderrors.New("agent communication gateway requires a controller-managed AgentIdentity")
	}
	capability, err := d.config.AgentDelegationSessionCapabilityIssuer.Issue(runtimeauth.AgentDelegationSessionCapabilityBinding{
		Namespace: instance.ObjectMeta.Namespace, AgentName: agent.ObjectMeta.Name, AgentIdentityName: identityName,
		InstanceName: instance.ObjectMeta.Name, InstanceUID: instance.ObjectMeta.UID, SessionID: sessionID, Solution: solution, SolutionRev: solutionRev,
	})
	if err != nil {
		return "", fmt.Errorf("issue runtime agent delegation session capability: %w", err)
	}
	return capability, nil
}

func (d *K8sDispatcher) solutionAttribution(ctx context.Context, namespace string, agent *v1alpha1.Agent) (string, int64, error) {
	if agent == nil || agent.ObjectMeta.Labels == nil || agent.ObjectMeta.Labels["kognition.dev/solution"] == "" {
		return "", 0, nil
	}
	solutionName := agent.ObjectMeta.Labels["kognition.dev/solution"]
	solution, err := getResource[v1alpha1.Solution](ctx, d.store, "Solution", namespace, solutionName)
	if err != nil {
		return "", 0, fmt.Errorf("lookup Solution %q for Agent %q: %w", solutionName, agent.ObjectMeta.Name, err)
	}
	return solutionName, solution.Status.ActiveRevision, nil
}

func (d *K8sDispatcher) validateModelGatewayInvocationLifetime(agent *v1alpha1.Agent) error {
	if d.config.ModelGatewayURL == "" {
		return nil
	}
	if d.config.ModelSessionCapabilityIssuer == nil {
		return stderrors.New("model gateway URL requires a model session capability issuer")
	}
	invocationTimeout := intctrl.DefaultAgentInvocationTimeout
	if agent.Spec.Runtime != nil && strings.TrimSpace(agent.Spec.Runtime.Timeout) != "" {
		parsed, err := intctrl.ParseAgentInvocationTimeout(agent.Spec.Runtime.Timeout)
		if err != nil {
			return fmt.Errorf("parse Agent runtime timeout %q: %w", agent.Spec.Runtime.Timeout, err)
		}
		invocationTimeout = parsed
	}
	if capabilityTTL := d.config.ModelSessionCapabilityIssuer.TTL(); invocationTimeout > capabilityTTL {
		return fmt.Errorf("Agent runtime timeout %s exceeds Model Gateway session capability lifetime %s", invocationTimeout, capabilityTTL)
	}
	return nil
}

func (d *K8sDispatcher) validateAgentDelegationGatewayInvocationLifetime(agent *v1alpha1.Agent) error {
	if d.config.AgentCommunicationGatewayURL == "" {
		return nil
	}
	if d.config.AgentDelegationSessionCapabilityIssuer == nil {
		return stderrors.New("agent communication gateway URL requires a delegation session capability issuer")
	}
	invocationTimeout := intctrl.DefaultAgentInvocationTimeout
	if agent.Spec.Runtime != nil && strings.TrimSpace(agent.Spec.Runtime.Timeout) != "" {
		parsed, err := intctrl.ParseAgentInvocationTimeout(agent.Spec.Runtime.Timeout)
		if err != nil {
			return fmt.Errorf("parse Agent runtime timeout %q: %w", agent.Spec.Runtime.Timeout, err)
		}
		invocationTimeout = parsed
	}
	if capabilityTTL := d.config.AgentDelegationSessionCapabilityIssuer.TTL(); invocationTimeout > capabilityTTL {
		return fmt.Errorf("Agent runtime timeout %s exceeds Agent Communication Gateway session capability lifetime %s", invocationTimeout, capabilityTTL)
	}
	return nil
}

func (d *K8sDispatcher) issueMemoryCapability(ctx context.Context, namespace string, agent *v1alpha1.Agent, instance *v1alpha1.AgentInstance, sessionID string) (string, error) {
	if agent.Spec.Memory == nil || len(agent.Spec.Memory.Claims) == 0 || d.config.MemoryPolicyGatewayURL == "" {
		return "", nil
	}
	if d.config.MemoryCapabilityIssuer == nil {
		return "", stderrors.New("memory policy gateway URL requires a capability issuer")
	}
	identityName := instance.Spec.SessionContext[intctrl.MemoryCapabilityIdentityNameContextKey]
	if identityName == "" {
		return "", stderrors.New("memory policy gateway requires a controller-managed AgentIdentity")
	}
	volumeHandle := d.resolveMemoryBackendHandle(ctx, namespace, agent.Spec.Memory.Claims)
	if volumeHandle == "" {
		return "", stderrors.New("memory policy gateway requires a bound memory backend handle")
	}
	capability, err := d.config.MemoryCapabilityIssuer.Issue(runtimeauth.MemoryCapabilityBinding{
		Namespace:         namespace,
		AgentName:         agent.ObjectMeta.Name,
		AgentIdentityName: identityName,
		InstanceName:      instance.ObjectMeta.Name,
		InstanceUID:       instance.ObjectMeta.UID,
		SessionID:         sessionID,
		VolumeHandle:      volumeHandle,
	})
	if err != nil {
		return "", fmt.Errorf("issue runtime memory capability: %w", err)
	}
	return capability, nil
}

func (d *K8sDispatcher) sessionID(instance *v1alpha1.AgentInstance) string {
	sessionID := uuid.New().String()
	if instance.ObjectMeta.UID == "" {
		return sessionID
	}
	generation := instance.Status.RestartCount
	if instance.Status.Phase == v1alpha1.PhaseRunning || instance.Status.Phase == v1alpha1.PhaseDegraded {
		generation++
	}
	key := fmt.Sprintf("kognition.dev/agent-instance/%s/restart/%d", instance.ObjectMeta.UID, generation)
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte(key)).String()
}

func (d *K8sDispatcher) agentServiceEndpoint(ctx context.Context, instance *v1alpha1.AgentInstance) (string, bool, error) {
	if d.externalEndpoints != nil {
		if instance.Spec.AgentRevision != nil {
			return "", true, fmt.Errorf("external runtime does not support pinned Agent revisions")
		}
		endpoint, ok := d.externalEndpoints[instance.ObjectMeta.Namespace+"/"+instance.Spec.AgentRef.Name]
		if !ok {
			return "", true, fmt.Errorf("no external runtime configured for Agent %s/%s", instance.ObjectMeta.Namespace, instance.Spec.AgentRef.Name)
		}
		return endpoint, true, nil
	}
	serviceName := instance.ObjectMeta.Annotations[intctrl.AgentServiceExecutionAnnotation]
	if serviceName == "" {
		return "", false, nil
	}
	if instance.Spec.AgentRevision != nil {
		namespace := instance.ObjectMeta.Namespace
		if d.config.AgentNamespace != "" {
			namespace = d.config.AgentNamespace
		}
		workloadName := agentServiceRevisionWorkloadName(instance.ObjectMeta.Namespace, serviceName, *instance.Spec.AgentRevision)
		return fmt.Sprintf("http://%s.%s.svc.cluster.local:%d/invoke", workloadName, namespace, agentPort), true, nil
	}
	service, err := getResource[v1alpha1.AgentService](ctx, d.store, "AgentService", instance.ObjectMeta.Namespace, serviceName)
	if err != nil {
		return "", true, fmt.Errorf("get routed AgentService %q: %w", serviceName, err)
	}
	if service.Status.Phase != v1alpha1.PhaseReady || service.Status.ReadyInstances < 1 || service.Status.Endpoint == "" {
		return "", true, fmt.Errorf("routed AgentService %q is not ready", serviceName)
	}
	if service.Spec.AgentRef.Name != instance.Spec.AgentRef.Name {
		return "", true, fmt.Errorf("routed AgentService %q targets Agent %q, want %q", serviceName, service.Spec.AgentRef.Name, instance.Spec.AgentRef.Name)
	}
	if service.Spec.RevisionPin != nil && (instance.Spec.AgentRevision == nil || *service.Spec.RevisionPin != *instance.Spec.AgentRevision) {
		return "", true, fmt.Errorf("routed AgentService %q revision does not match AgentInstance", serviceName)
	}
	endpoint, err := url.Parse(service.Status.Endpoint)
	if err != nil || endpoint.Scheme != "http" || endpoint.Host == "" || endpoint.Path != "/invoke" {
		return "", true, fmt.Errorf("routed AgentService %q has invalid endpoint %q", serviceName, service.Status.Endpoint)
	}
	return service.Status.Endpoint, true, nil
}

func (d *K8sDispatcher) ensureRuntimeResources(ctx context.Context, namespace string, agent *v1alpha1.Agent, envVars []corev1.EnvVar) error {
	if namespace != "" && namespace == d.config.ControllerNamespace {
		return fmt.Errorf("runtime workloads cannot use installation namespace %q; use a separate enrolled workload namespace", namespace)
	}
	if d.config.RuntimeAuthToken != "" && d.config.RuntimeAuthTokenSecretName == "" {
		return fmt.Errorf("runtime auth token requires RuntimeAuthTokenSecretName for pod injection")
	}
	if d.config.RuntimeAuthToken != "" && d.config.ControllerNamespace == "" {
		return fmt.Errorf("runtime auth token requires ControllerNamespace for NetworkPolicy ingress")
	}
	if err := d.ensureRuntimeAuthSecret(ctx, namespace); err != nil {
		return fmt.Errorf("ensure runtime auth secret: %w", err)
	}
	if err := d.ensureKnowledgeGatewayCA(ctx, namespace); err != nil {
		return fmt.Errorf("ensure Knowledge Gateway CA: %w", err)
	}
	if err := d.ensureDeployment(ctx, namespace, agent, envVars); err != nil {
		return fmt.Errorf("ensure deployment: %w", err)
	}
	if err := d.ensureService(ctx, namespace, agent); err != nil {
		return fmt.Errorf("ensure service: %w", err)
	}
	if err := d.ensureRuntimeNetworkPolicy(ctx, namespace, agent); err != nil {
		return fmt.Errorf("ensure runtime network policy: %w", err)
	}
	if err := d.removeOwnedLegacyResources(ctx, namespace, agent); err != nil {
		return fmt.Errorf("migrate legacy runtime resources: %w", err)
	}
	return nil
}

// invokeWhenReady polls the agent Deployment for readiness then POSTs the
// initial message to /invoke. Runs in its own goroutine so Dispatch can return
// promptly. The runtime's /invoke endpoint is idempotent, so duplicate calls
// across reconciler retries are safe.
func (d *K8sDispatcher) invokeWhenReady(namespace, workloadKey, agentName, instanceName, sessionID, input, traceparent, preferredReplica string, invocationValues ...string) {
	d.trackInvocation(sessionID)
	defer func() {
		d.invocationsMu.Lock()
		if timing, ok := d.invocationTimings[sessionID]; ok {
			timing.pending = false
			d.invocationTimings[sessionID] = timing
		}
		d.invocationsMu.Unlock()
	}()
	ctx, cancel := context.WithTimeout(context.Background(), invokeReadyTimeout)
	defer cancel()

	log := d.logger.With(
		zap.String("agent", agentName),
		zap.String("instance", instanceName),
		zap.String("sessionID", sessionID),
		zap.String("namespace", namespace),
	)

	deployKey := client.ObjectKey{Namespace: namespace, Name: deploymentName(workloadKey)}
	ticker := time.NewTicker(invokePollInterval)
	defer ticker.Stop()

	for {
		var deploy appsv1.Deployment
		if err := d.client.Get(ctx, deployKey, &deploy); err != nil {
			if !errors.IsNotFound(err) {
				log.Warn("invokeWhenReady: get deployment failed", zap.Error(err))
			}
		} else if deploymentRolloutComplete(&deploy) {
			break
		}

		select {
		case <-ctx.Done():
			log.Warn("invokeWhenReady: timed out waiting for pod readiness")
			return
		case <-ticker.C:
		}
	}

	endpoint := d.runtimeInvokeEndpoint(ctx, namespace, workloadKey, agentName, preferredReplica)
	spiffeID, memoryCapability, toolSessionCapability, modelSessionCapability, delegationSessionCapability, conversationID, knowledgeSessionCapability := invocationValuesAt(invocationValues)
	d.invokeAfterSessionActivation(namespace, instanceName, endpoint, agentName, instanceName, sessionID, input, traceparent, spiffeID, memoryCapability, toolSessionCapability, modelSessionCapability, delegationSessionCapability, conversationID, knowledgeSessionCapability)
}

func (d *K8sDispatcher) runtimeInvokeEndpoint(ctx context.Context, namespace, workloadKey, agentName, preferredReplica string) string {
	serviceEndpoint := fmt.Sprintf("http://%s.%s.svc.cluster.local:%d/invoke", serviceName(workloadKey), namespace, agentPort)
	if preferredReplica == "" {
		return serviceEndpoint
	}
	var pod corev1.Pod
	if err := d.client.Get(ctx, client.ObjectKey{Namespace: namespace, Name: preferredReplica}, &pod); err != nil {
		return serviceEndpoint
	}
	if pod.Labels[labelManagedBy] != managedByValue ||
		pod.Labels[labelAgent] != workloadKey ||
		pod.Labels[labelAgentName] != agentName ||
		!runtimePodReady(&pod) || pod.Status.PodIP == "" {
		return serviceEndpoint
	}
	return "http://" + net.JoinHostPort(pod.Status.PodIP, strconv.Itoa(agentPort)) + "/invoke"
}

func runtimePodReady(pod *corev1.Pod) bool {
	if pod == nil || pod.DeletionTimestamp != nil || pod.Status.Phase != corev1.PodRunning {
		return false
	}
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodReady {
			return condition.Status == corev1.ConditionTrue
		}
	}
	return false
}

func (d *K8sDispatcher) invokeAfterSessionActivation(crNamespace, instanceName, endpoint, agentName, runtimeInstanceName, sessionID, input, traceparent, spiffeID, memoryCapability, toolSessionCapability, modelSessionCapability, delegationSessionCapability string, invocationValues ...string) {
	knowledgeSessionCapability := ""
	if len(invocationValues) > 1 {
		knowledgeSessionCapability = invocationValues[1]
	}
	if (memoryCapability != "" || toolSessionCapability != "" || modelSessionCapability != "" || knowledgeSessionCapability != "" || delegationSessionCapability != "") && !d.waitForSessionActivation(crNamespace, instanceName, sessionID) {
		d.logger.Warn("capability invocation skipped because session activation was not persisted",
			zap.String("namespace", crNamespace),
			zap.String("instance", instanceName),
			zap.String("sessionID", sessionID),
		)
		return
	}
	d.invokeEndpoint(endpoint, agentName, runtimeInstanceName, sessionID, input, traceparent, spiffeID, memoryCapability, toolSessionCapability, modelSessionCapability, delegationSessionCapability, invocationValues...)
}

func (d *K8sDispatcher) waitForSessionActivation(namespace, instanceName, sessionID string) bool {
	return d.waitForSessionActivationContext(context.Background(), namespace, instanceName, sessionID)
}

func (d *K8sDispatcher) waitForSessionActivationContext(parent context.Context, namespace, instanceName, sessionID string) bool {
	if d.store == nil {
		return false
	}
	ctx, cancel := context.WithTimeout(parent, capabilityActivationTimeout)
	defer cancel()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		raw, err := d.store.Get(ctx, "AgentInstance", namespace, instanceName)
		if err == nil {
			var instance v1alpha1.AgentInstance
			if json.Unmarshal(raw, &instance) == nil && instance.Status.Phase == v1alpha1.PhaseRunning && instance.Status.SessionID == sessionID {
				return true
			}
		}
		select {
		case <-ctx.Done():
			return false
		case <-ticker.C:
		}
	}
}

func (d *K8sDispatcher) invokeEndpoint(endpoint, agentName, instanceName, sessionID, input, traceparent, spiffeID, memoryCapability, toolSessionCapability, modelSessionCapability, delegationSessionCapability string, invocationValues ...string) {
	log := d.logger.With(
		zap.String("agent", agentName),
		zap.String("instance", instanceName),
		zap.String("sessionID", sessionID),
		zap.String("endpoint", endpoint),
		zap.Bool("memoryCapability", memoryCapability != ""),
		zap.Bool("toolSessionCapability", toolSessionCapability != ""),
		zap.Bool("modelSessionCapability", modelSessionCapability != ""),
		zap.Bool("agentDelegationSessionCapability", delegationSessionCapability != ""),
	)
	conversationID := ""
	knowledgeSessionCapability := ""
	if len(invocationValues) > 0 {
		conversationID = invocationValues[0]
	}
	if len(invocationValues) > 1 {
		knowledgeSessionCapability = invocationValues[1]
	}
	body, err := json.Marshal(map[string]string{"message": input, "session_id": sessionID, "conversation_id": conversationID, "spiffe_id": spiffeID, "memory_capability": memoryCapability, "tool_session_capability": toolSessionCapability, "model_session_capability": modelSessionCapability, "knowledge_session_capability": knowledgeSessionCapability, "agent_delegation_session_capability": delegationSessionCapability})
	if err != nil {
		log.Error("invokeWhenReady: marshal request body failed", zap.Error(err))
		return
	}

	// Retry on 409 (another session currently RUNNING in the singleton pod) so
	// queued workflow steps don't silently abandon their invocation and let the
	// reconciler scrape the prior session's leftover output. Uses its own
	// longer-lived context: the 90s pod-readiness ctx has already expired by the
	// time we get here for the first attempt, and a queue of N parallel workers
	// hitting a singleton runtime can easily need several minutes of 409-retry
	// before the /invoke lands.
	busyCtx, busyCancel := context.WithTimeout(context.Background(), invokeBusyTimeout)
	control := &invocationControl{cancel: busyCancel, done: make(chan struct{})}
	d.invocationsMu.Lock()
	if d.invocations == nil {
		d.invocations = make(map[string]*invocationControl)
	}
	d.invocations[sessionID] = control
	d.invocationsMu.Unlock()
	defer func() {
		busyCancel()
		d.invocationsMu.Lock()
		delete(d.invocations, sessionID)
		d.invocationsMu.Unlock()
		close(control.done)
	}()
	ticker := time.NewTicker(invokePollInterval)
	defer ticker.Stop()
	for {
		// Body is rebuilt per attempt because the previous request consumed the reader.
		req, err := http.NewRequestWithContext(busyCtx, http.MethodPost, endpoint, strings.NewReader(string(body)))
		if err != nil {
			log.Error("invokeWhenReady: build request failed", zap.Error(err))
			return
		}
		req.Header.Set("Content-Type", "application/json")
		if traceparent != "" {
			req.Header.Set("traceparent", traceparent)
		}
		if d.config.RuntimeAuthToken != "" {
			req.Header.Set("Authorization", "Bearer "+d.config.RuntimeAuthToken)
		}

		resp, err := d.invokeHTTP.Do(req)
		if err != nil {
			log.Warn("invokeWhenReady: POST /invoke failed", zap.Error(err))
			return
		}
		status := resp.StatusCode
		if status == http.StatusConflict {
			control.mu.Lock()
			control.authoritativelyBusy = true
			control.mu.Unlock()
			_ = resp.Body.Close()
			select {
			case <-busyCtx.Done():
				log.Warn("invokeWhenReady: timed out waiting for prior session to free /invoke")
				return
			case <-ticker.C:
			}
			continue
		}
		if status >= 400 {
			_ = resp.Body.Close()
			log.Warn("invokeWhenReady: /invoke returned non-2xx",
				zap.Int("status", status),
			)
			return
		}
		_ = resp.Body.Close()
		control.mu.Lock()
		control.accepted = true
		control.authoritativelyBusy = false
		control.mu.Unlock()
		d.invocationsMu.Lock()
		if timing, ok := d.invocationTimings[sessionID]; ok {
			timing.pending = false
			if timing.acceptedAt.IsZero() {
				timing.acceptedAt = time.Now()
			}
			d.invocationTimings[sessionID] = timing
		}
		d.invocationsMu.Unlock()
		log.Info("invokeWhenReady: dispatched /invoke", zap.Int("status", status))
		return
	}
}

func (d *K8sDispatcher) trackInvocation(sessionID string) {
	d.invocationsMu.Lock()
	defer d.invocationsMu.Unlock()
	if d.invocationTimings == nil {
		d.invocationTimings = make(map[string]invocationTiming)
	}
	if _, exists := d.invocationTimings[sessionID]; !exists {
		d.invocationTimings[sessionID] = invocationTiming{pending: true}
	}
}

func (d *K8sDispatcher) forgetInvocation(sessionID string) {
	d.invocationsMu.Lock()
	defer d.invocationsMu.Unlock()
	delete(d.invocationTimings, sessionID)
}

func invocationValuesAt(values []string) (spiffeID, memoryCapability, toolSessionCapability, modelSessionCapability, delegationSessionCapability, conversationID, knowledgeSessionCapability string) {
	if len(values) > 0 {
		spiffeID = values[0]
	}
	if len(values) > 5 {
		conversationID = values[5]
	}
	if len(values) > 3 {
		modelSessionCapability = values[3]
	}
	if len(values) > 1 {
		memoryCapability = values[1]
	}
	if len(values) > 2 {
		toolSessionCapability = values[2]
	}
	if len(values) > 4 {
		delegationSessionCapability = values[4]
	}
	if len(values) > 6 {
		knowledgeSessionCapability = values[6]
	}
	return spiffeID, memoryCapability, toolSessionCapability, modelSessionCapability, delegationSessionCapability, conversationID, knowledgeSessionCapability
}

// Status checks the agent Deployment health and pod readiness.
func (d *K8sDispatcher) Status(ctx context.Context, instance *v1alpha1.AgentInstance) (intctrl.RuntimeStatus, error) {
	if session, ok := d.autogenSessions.Load(instance.Status.SessionID); ok {
		owned, valid := session.(*autoGenSession)
		if !valid {
			return intctrl.RuntimeStatus{}, fmt.Errorf("invalid AutoGen session ownership")
		}
		return d.autoGenStatus(ctx, instance, owned)
	}
	if handled, status, err := d.unownedAutoGenStatus(ctx, instance); handled {
		return status, err
	}
	if d.externalEndpoints != nil {
		return d.externalStatus(ctx, instance)
	}
	if endpoint, routed, routeErr := d.agentServiceEndpoint(ctx, instance); routed {
		if routeErr != nil {
			return intctrl.RuntimeStatus{Alive: false, Error: routeErr.Error()}, nil
		}
		statusURL, err := serviceStatusURL(endpoint, instance.Status.SessionID)
		if err != nil {
			return intctrl.RuntimeStatus{}, fmt.Errorf("routed AgentService status endpoint: %w", err)
		}
		status, err := d.queryAgentStatus(ctx, statusURL)
		if err != nil {
			d.logger.Debug("failed to query routed agent service status endpoint", zap.Error(err))
			return intctrl.RuntimeStatus{Alive: true}, nil
		}
		return status, nil
	}
	namespace := instance.ObjectMeta.Namespace
	if d.config.AgentNamespace != "" {
		namespace = d.config.AgentNamespace
	}

	agentName := instance.Spec.AgentRef.Name
	workloadKey := d.workloadKey(instance.ObjectMeta.Namespace, agentName)
	deployName := deploymentName(workloadKey)

	// Get the Deployment.
	var deploy appsv1.Deployment
	key := client.ObjectKey{Namespace: namespace, Name: deployName}
	if err := d.client.Get(ctx, key, &deploy); err != nil {
		if errors.IsNotFound(err) {
			d.forgetInvocation(instance.Status.SessionID)
			return intctrl.RuntimeStatus{Alive: false}, nil
		}
		return intctrl.RuntimeStatus{}, fmt.Errorf("get deployment: %w", err)
	}

	// Check if pods are available.
	available := deploy.Status.AvailableReplicas > 0
	if !available {
		// Deployment exists but no pods ready yet.
		return intctrl.RuntimeStatus{Alive: true, Pending: true}, nil
	}

	d.invocationsMu.Lock()
	timing, tracked := d.invocationTimings[instance.Status.SessionID]
	d.invocationsMu.Unlock()
	if tracked && timing.pending {
		return intctrl.RuntimeStatus{Alive: true, Pending: true}, nil
	}
	if tracked && timing.acceptedAt.IsZero() {
		d.forgetInvocation(instance.Status.SessionID)
		return intctrl.RuntimeStatus{Alive: false, Error: "initial runtime invocation was not accepted; session outcome is unavailable"}, nil
	}

	// Try to get detailed status from the agent's /status endpoint. Pass the
	// instance's sessionID so the runtime can avoid handing back another
	// session's leftover output from the shared pod state.
	svcURL := fmt.Sprintf("http://%s.%s.svc.cluster.local:%d/status", serviceName(workloadKey), namespace, agentPort)
	if sid := instance.Status.SessionID; sid != "" {
		svcURL += "?session_id=" + url.QueryEscape(sid)
	}
	status, err := d.queryAgentStatus(ctx, svcURL)
	if err != nil {
		d.logger.Debug("failed to query agent status endpoint", zap.Error(err))
		// Pod is running but status endpoint not reachable yet — still alive.
		return intctrl.RuntimeStatus{Alive: true, Pending: true}, nil
	}
	if status.Completed || !status.Alive {
		d.forgetInvocation(instance.Status.SessionID)
	}
	dispatchedAt := instance.Status.StartTime
	if tracked {
		dispatchedAt = &timing.acceptedAt
	}
	// Provisioning and queue time precede runtime acceptance and cannot prove a restart.
	if instance.Spec.SessionContext["input"] != "" && status.Alive && !status.Completed && status.RuntimeUptimeSeconds > 0 && dispatchedAt != nil &&
		(deploy.Spec.Replicas == nil || *deploy.Spec.Replicas == 1) &&
		time.Since(*dispatchedAt).Seconds() > status.RuntimeUptimeSeconds+5 {
		d.forgetInvocation(instance.Status.SessionID)
		return intctrl.RuntimeStatus{Alive: false, Error: "runtime restarted after this session was dispatched; session outcome is unavailable"}, nil
	}

	return status, nil
}

func serviceStatusURL(endpoint, sessionID string) (string, error) {
	parsed, err := url.Parse(endpoint)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.Path != "/invoke" {
		return "", fmt.Errorf("invalid service endpoint %q", endpoint)
	}
	parsed.Path = "/status"
	if sessionID != "" {
		query := parsed.Query()
		query.Set("session_id", sessionID)
		parsed.RawQuery = query.Encode()
	}
	return parsed.String(), nil
}

func serviceCancelURL(endpoint string) (string, error) {
	parsed, err := url.Parse(endpoint)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.Path != "/invoke" {
		return "", fmt.Errorf("invalid service endpoint %q", endpoint)
	}
	parsed.Path = "/cancel"
	parsed.RawQuery = ""
	return parsed.String(), nil
}

// Terminate cooperatively cancels a live runtime session through the runtime
// control endpoint. Quota settlement remains blocked until this call succeeds.
func (d *K8sDispatcher) Terminate(ctx context.Context, instance *v1alpha1.AgentInstance, reason string) error {
	if session, ok := d.autogenSessions.Load(instance.Status.SessionID); ok {
		owned, valid := session.(*autoGenSession)
		if !valid {
			return fmt.Errorf("invalid AutoGen session ownership")
		}
		return d.terminateAutoGen(ctx, instance, owned)
	}
	if d.store != nil {
		if agent, err := intctrl.ResolveExecutionAgent(ctx, d.store, instance.ObjectMeta.Namespace, instance); err == nil && agent.Spec.Runtime != nil && strings.TrimSpace(agent.Spec.Runtime.Class) == "autogen" {
			if terminal, err := d.recoverAutoGenTerminal(ctx, instance, agent); err == nil && terminal.Completed {
				return nil
			}
			return fmt.Errorf("%w: AutoGen runtime ownership requires recovery", intctrl.ErrDispatchAccountingUncertain)
		}
	}
	d.invocationsMu.Lock()
	control := d.invocations[instance.Status.SessionID]
	d.invocationsMu.Unlock()
	if control != nil {
		control.cancel()
		select {
		case <-control.done:
		case <-ctx.Done():
			return ctx.Err()
		}
		control.mu.Lock()
		accepted := control.accepted
		authoritativelyBusy := control.authoritativelyBusy
		control.mu.Unlock()
		if !accepted && authoritativelyBusy {
			return nil
		}
	}
	var cancelURL string
	if endpoint, routed, routeErr := d.agentServiceEndpoint(ctx, instance); routed {
		if routeErr != nil {
			return routeErr
		}
		var err error
		cancelURL, err = serviceCancelURL(endpoint)
		if err != nil {
			return fmt.Errorf("routed AgentService cancel endpoint: %w", err)
		}
	} else {
		namespace := instance.ObjectMeta.Namespace
		if d.config.AgentNamespace != "" {
			namespace = d.config.AgentNamespace
		}
		workloadKey := d.workloadKey(instance.ObjectMeta.Namespace, instance.Spec.AgentRef.Name)
		cancelURL = fmt.Sprintf("http://%s.%s.svc.cluster.local:%d/cancel", serviceName(workloadKey), namespace, agentPort)
	}
	body, err := json.Marshal(map[string]string{"session_id": instance.Status.SessionID, "reason": reason})
	if err != nil {
		return fmt.Errorf("marshal runtime cancel request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cancelURL, strings.NewReader(string(body)))
	if err != nil {
		return fmt.Errorf("build runtime cancel request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if d.config.RuntimeAuthToken != "" {
		req.Header.Set("Authorization", "Bearer "+d.config.RuntimeAuthToken)
	}
	resp, err := d.http.Do(req)
	if err != nil {
		return fmt.Errorf("cancel runtime session: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("runtime cancel endpoint returned %d", resp.StatusCode)
	}
	var ack struct {
		Accepted  bool   `json:"accepted"`
		SessionID string `json:"session_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&ack); err != nil {
		return fmt.Errorf("decode runtime cancel acknowledgment: %w", err)
	}
	if !ack.Accepted || ack.SessionID != instance.Status.SessionID {
		return fmt.Errorf("runtime did not acknowledge cancellation for session %q", instance.Status.SessionID)
	}
	d.forgetInvocation(instance.Status.SessionID)
	return nil
}

// queryAgentStatus calls the agent pod's /status HTTP endpoint.
func (d *K8sDispatcher) queryAgentStatus(ctx context.Context, url string) (intctrl.RuntimeStatus, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return intctrl.RuntimeStatus{}, err
	}
	if d.config.RuntimeAuthToken != "" {
		req.Header.Set("Authorization", "Bearer "+d.config.RuntimeAuthToken)
	}

	resp, err := d.http.Do(req)
	if err != nil {
		return intctrl.RuntimeStatus{}, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return intctrl.RuntimeStatus{}, fmt.Errorf("status endpoint returned %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxRuntimeResponseBodyBytes+1))
	if err != nil {
		return intctrl.RuntimeStatus{}, err
	}
	if len(body) > maxRuntimeResponseBodyBytes {
		return intctrl.RuntimeStatus{}, fmt.Errorf("status response exceeds %d bytes", maxRuntimeResponseBodyBytes)
	}

	var statusResp agentStatusResponse
	if err := json.Unmarshal(body, &statusResp); err != nil {
		return intctrl.RuntimeStatus{}, err
	}

	result := intctrl.RuntimeStatus{
		Alive:                !strings.EqualFold(statusResp.Phase, "failed"),
		Completed:            strings.EqualFold(statusResp.Phase, "completed"),
		ToolCalls:            statusResp.ToolCalls,
		ToolCallsDetail:      toolCallDetailsFromRuntime(statusResp.ToolCallsDetail),
		Output:               statusResp.Output,
		Error:                statusResp.Error,
		SPIFFEID:             statusResp.SPIFFEID,
		RuntimeUptimeSeconds: statusResp.UptimeSeconds,
	}
	if statusResp.TokenUsage != nil {
		result.TokenUsage = &v1alpha1.TokenUsage{
			PromptTokens:     statusResp.TokenUsage.PromptTokens,
			CompletionTokens: statusResp.TokenUsage.CompletionTokens,
			TotalTokens:      statusResp.TokenUsage.TotalTokens,
		}
	}
	return result, nil
}

type agentStatusResponse struct {
	Phase           string               `json:"phase"`
	ToolCalls       int32                `json:"tool_calls"`
	ToolCallsDetail []map[string]any     `json:"tool_calls_detail,omitempty"`
	Output          string               `json:"output"`
	Error           string               `json:"error,omitempty"`
	SPIFFEID        string               `json:"spiffe_id,omitempty"`
	TokenUsage      *agentTokenUsageResp `json:"token_usage,omitempty"`
	UptimeSeconds   float64              `json:"uptime_seconds"`
}

func toolCallDetailsFromRuntime(details []map[string]any) []v1alpha1.ToolCallDetail {
	if len(details) == 0 {
		return nil
	}
	converted := make([]v1alpha1.ToolCallDetail, 0, len(details))
	for _, detail := range details {
		name, _ := detail["name"].(string)
		if name == "" {
			continue
		}
		converted = append(converted, v1alpha1.ToolCallDetail{
			Name:       name,
			Args:       runtimeToolDetailValue(detail["args"]),
			Result:     runtimeToolDetailValue(detail["result"]),
			DurationMS: runtimeToolDetailDuration(detail["duration_ms"]),
			Error:      runtimeToolDetailValue(detail["error"]),
		})
	}
	return converted
}

func runtimeToolDetailValue(value any) string {
	if value == nil {
		return ""
	}
	if text, ok := value.(string); ok {
		return text
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return fmt.Sprint(value)
	}
	return string(encoded)
}

func runtimeToolDetailDuration(value any) int64 {
	switch duration := value.(type) {
	case float64:
		return int64(duration)
	case int64:
		return duration
	case int:
		return int64(duration)
	default:
		return 0
	}
}

type agentTokenUsageResp struct {
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	TotalTokens      int64 `json:"total_tokens"`
}

// deploymentRolloutComplete reports whether the Deployment has finished
// rolling out the current PodTemplate. This prevents invokeWhenReady from
// hitting a previous-generation pod while a rollout is still in progress
// (which would deliver /invoke to a pod that is about to be terminated).
func deploymentRolloutComplete(d *appsv1.Deployment) bool {
	if d.Status.ObservedGeneration < d.ObjectMeta.Generation {
		return false
	}
	desired := int32(1)
	if d.Spec.Replicas != nil {
		desired = *d.Spec.Replicas
	}
	if d.Status.UpdatedReplicas < desired {
		return false
	}
	if d.Status.AvailableReplicas < desired {
		return false
	}
	if d.Status.Replicas > d.Status.UpdatedReplicas {
		return false
	}
	return true
}

// ensureDeployment creates or updates the Deployment for an Agent.
func (d *K8sDispatcher) ensureDeployment(ctx context.Context, namespace string, agent *v1alpha1.Agent, envVars []corev1.EnvVar) error {
	runtimeImage, err := d.runtimeImage(agent)
	if err != nil {
		return err
	}
	workloadKey := d.workloadKey(agent.ObjectMeta.Namespace, agent.ObjectMeta.Name)
	name := deploymentName(workloadKey)
	replicas := int32(1)
	if agent.Spec.Replicas != nil {
		replicas = *agent.Spec.Replicas
	}

	labels := map[string]string{
		labelManagedBy:      managedByValue,
		labelAgent:          workloadKey,
		labelAgentName:      agent.ObjectMeta.Name,
		labelAgentNamespace: agent.ObjectMeta.Namespace,
	}
	annotations := map[string]string{
		AnnotationOwnerAgent: agent.ObjectMeta.Namespace + "/" + agent.ObjectMeta.Name,
	}
	volumes, volumeMounts := d.knowledgeGatewayTrustVolume()
	controlTLS := agent.Spec.Runtime != nil && strings.TrimSpace(agent.Spec.Runtime.Class) == "autogen"
	if controlTLS {
		if replicas != 1 {
			return fmt.Errorf("AutoGen contract workers require one pinned replica")
		}
		if d.config.RuntimeTLSSecretName == "" || d.config.RuntimeCAFile == "" {
			return fmt.Errorf("AutoGen Kubernetes control requires operator TLS Secret and CA trust")
		}
		volumes = append(volumes, corev1.Volume{Name: "runtime-control-tls", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: d.config.RuntimeTLSSecretName}}})
		volumeMounts = append(volumeMounts, corev1.VolumeMount{Name: "runtime-control-tls", MountPath: "/etc/kognition/runtime-control-tls", ReadOnly: true})
	}

	desired := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   namespace,
			Labels:      labels,
			Annotations: annotations,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{
				MatchLabels: labels,
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: labels,
				},
				Spec: corev1.PodSpec{
					AutomountServiceAccountToken: ptr.To(false),
					SecurityContext: &corev1.PodSecurityContext{
						RunAsNonRoot: ptr.To(true), RunAsUser: ptr.To(int64(1000)),
						SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
					},
					Volumes: volumes,
					Containers: []corev1.Container{
						{
							Name:  "agent",
							Image: runtimeImage,
							SecurityContext: &corev1.SecurityContext{
								AllowPrivilegeEscalation: ptr.To(false),
								Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
							},
							Ports: []corev1.ContainerPort{
								{
									Name:          "http",
									ContainerPort: int32(agentPort),
									Protocol:      corev1.ProtocolTCP,
								},
							},
							Env:          envVars,
							VolumeMounts: volumeMounts,
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse("100m"),
									corev1.ResourceMemory: resource.MustParse("128Mi"),
								},
								Limits: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse("500m"),
									corev1.ResourceMemory: resource.MustParse("512Mi"),
								},
							},
							LivenessProbe: &corev1.Probe{
								ProbeHandler: corev1.ProbeHandler{
									HTTPGet: &corev1.HTTPGetAction{
										Path: "/healthz",
										Port: intstr.FromInt(agentPort),
									},
								},
								InitialDelaySeconds: 5,
								PeriodSeconds:       10,
							},
							StartupProbe: &corev1.Probe{
								ProbeHandler:   corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: "/healthz", Port: intstr.FromInt(agentPort), Scheme: corev1.URISchemeHTTP}},
								TimeoutSeconds: 5, PeriodSeconds: 5, FailureThreshold: 24, SuccessThreshold: 1,
							},
							ReadinessProbe: &corev1.Probe{
								ProbeHandler: corev1.ProbeHandler{
									HTTPGet: &corev1.HTTPGetAction{
										Path: "/readyz",
										Port: intstr.FromInt(agentPort),
									},
								},
								InitialDelaySeconds: 3,
								PeriodSeconds:       5,
							},
						},
					},
				},
			},
		},
	}

	if controlTLS {
		container := &desired.Spec.Template.Spec.Containers[0]
		container.LivenessProbe.HTTPGet.Scheme = corev1.URISchemeHTTPS
		container.ReadinessProbe.HTTPGet.Scheme = corev1.URISchemeHTTPS
		container.LivenessProbe.TimeoutSeconds = 5
		container.ReadinessProbe.TimeoutSeconds = 5
		container.LivenessProbe.SuccessThreshold, container.LivenessProbe.FailureThreshold = 1, 3
		container.ReadinessProbe.SuccessThreshold, container.ReadinessProbe.FailureThreshold = 1, 3
		container.StartupProbe.HTTPGet.Scheme = corev1.URISchemeHTTPS
	}
	// Try to get existing deployment.
	if err := d.addWorkloadIdentityProxy(desired, agent); err != nil {
		return err
	}
	volumes = desired.Spec.Template.Spec.Volumes
	envVars = desired.Spec.Template.Spec.Containers[0].Env
	var existing appsv1.Deployment
	key := client.ObjectKey{Namespace: namespace, Name: name}
	err = d.client.Get(ctx, key, &existing)
	if err != nil {
		if errors.IsNotFound(err) {
			d.logger.Info("creating agent deployment",
				zap.String("deployment", name),
				zap.String("namespace", namespace),
				zap.Int32("replicas", replicas),
			)
			return d.client.Create(ctx, desired)
		}
		return fmt.Errorf("get deployment: %w", err)
	}
	if err := d.verifySharedResourceOwner(existing.Annotations, agent, "deployment", name); err != nil {
		return err
	}

	// Decide whether anything material actually changed before issuing an
	// Update. A Update on an unchanged Deployment is not free: it sends a Watch
	// event that re-enqueues every controller watching Deployments, and any
	// spec change bumps the generation and starts a new pod rollout. With a
	// shared Deployment behind many AgentInstances, blindly updating per
	// Dispatch keeps the pod in perpetual rollout and invokeWhenReady never
	// observes a stable replica.
	needUpdate := false
	if existing.Annotations == nil {
		existing.Annotations = make(map[string]string)
	}
	ownerRef := agent.ObjectMeta.Namespace + "/" + agent.ObjectMeta.Name
	if existing.Annotations[AnnotationOwnerAgent] != ownerRef {
		existing.Annotations[AnnotationOwnerAgent] = ownerRef
		needUpdate = true
	}
	if existing.Spec.Replicas == nil || *existing.Spec.Replicas != replicas {
		existing.Spec.Replicas = &replicas
		needUpdate = true
	}
	if len(existing.Spec.Template.Spec.Containers) == 0 {
		existing.Spec.Template.Spec.Containers = desired.Spec.Template.Spec.Containers
		needUpdate = true
	} else {
		c := &existing.Spec.Template.Spec.Containers[0]
		if c.Image != runtimeImage {
			c.Image = runtimeImage
			needUpdate = true
		}
		if !envVarsEqual(c.Env, envVars) {
			c.Env = envVars
			needUpdate = true
		}
		if !reflect.DeepEqual(c.VolumeMounts, volumeMounts) {
			c.VolumeMounts = volumeMounts
			needUpdate = true
		}
		if !reflect.DeepEqual(c.LivenessProbe, desired.Spec.Template.Spec.Containers[0].LivenessProbe) || !reflect.DeepEqual(c.ReadinessProbe, desired.Spec.Template.Spec.Containers[0].ReadinessProbe) || !reflect.DeepEqual(c.StartupProbe, desired.Spec.Template.Spec.Containers[0].StartupProbe) {
			c.LivenessProbe, c.ReadinessProbe, c.StartupProbe = desired.Spec.Template.Spec.Containers[0].LivenessProbe, desired.Spec.Template.Spec.Containers[0].ReadinessProbe, desired.Spec.Template.Spec.Containers[0].StartupProbe
			needUpdate = true
		}
	}
	if !reflect.DeepEqual(existing.Spec.Template.Spec.Volumes, volumes) {
		existing.Spec.Template.Spec.Volumes = volumes
		needUpdate = true
	}
	if d.updateWorkloadIdentityProxy(&existing, desired) {
		needUpdate = true
	}
	if !needUpdate {
		return nil
	}
	return d.client.Update(ctx, &existing)
}

func (d *K8sDispatcher) ensureRuntimeNetworkPolicy(ctx context.Context, namespace string, agent *v1alpha1.Agent) error {
	workloadKey := d.workloadKey(agent.ObjectMeta.Namespace, agent.ObjectMeta.Name)
	labels := map[string]string{labelManagedBy: managedByValue, labelAgent: workloadKey}
	annotations := map[string]string{AnnotationOwnerAgent: agent.ObjectMeta.Namespace + "/" + agent.ObjectMeta.Name}
	return d.ensureRuntimeWorkloadNetworkPolicy(ctx, namespace, deploymentName(workloadKey), labels, annotations, agent)
}

func (d *K8sDispatcher) ensureRuntimeWorkloadNetworkPolicy(ctx context.Context, namespace, workloadName string, labels, annotations map[string]string, agent *v1alpha1.Agent) error {
	name := workloadName + "-network"
	if d.config.WorkloadIdentityMode == "spire" && (!d.config.RuntimeNetworkPolicyEnabled || d.config.ControllerNamespace == "") {
		return fmt.Errorf("SPIRE status requires enforced runtime networking and a pinned controller namespace")
	}
	if !d.config.RuntimeNetworkPolicyEnabled {
		return d.removeRuntimeWorkloadNetworkPolicy(ctx, namespace, name, annotations, agent)
	}
	if len(d.config.KubeletProbeIngressCIDRs) == 0 && !d.config.CiliumHostProbeIngress {
		return fmt.Errorf("runtime NetworkPolicy requires kubelet probe ingress CIDRs")
	}
	if d.config.AgentCommunicationGatewayURL != "" && d.config.AgentCommunicationGatewayNamespace == "" {
		return fmt.Errorf("agent communication gateway URL requires a namespace for runtime ingress policy")
	}
	if d.config.KnowledgeGatewayURL != "" && d.config.KnowledgeGatewayNamespace == "" {
		return fmt.Errorf("knowledge gateway URL requires a namespace for runtime egress policy")
	}
	policyAnnotations := make(map[string]string, len(annotations))
	for key, value := range annotations {
		policyAnnotations[key] = value
	}
	ingressPeers := []networkingv1.NetworkPolicyPeer{{
		NamespaceSelector: namespaceSelector(d.config.ControllerNamespace),
		PodSelector:       &metav1.LabelSelector{MatchLabels: map[string]string{"app.kubernetes.io/component": "k8s-controller"}},
	}}
	for _, cidr := range d.config.KubeletProbeIngressCIDRs {
		ingressPeers = append(ingressPeers, networkingv1.NetworkPolicyPeer{IPBlock: &networkingv1.IPBlock{CIDR: cidr}})
	}
	if d.config.AgentCommunicationGatewayURL != "" {
		ingressPeers = append(ingressPeers, networkingv1.NetworkPolicyPeer{
			NamespaceSelector: namespaceSelector(d.config.AgentCommunicationGatewayNamespace),
			PodSelector:       &metav1.LabelSelector{MatchLabels: map[string]string{"app.kubernetes.io/component": "agent-communication-gateway"}},
		})
	}
	desired := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   namespace,
			Labels:      labels,
			Annotations: policyAnnotations,
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: labels},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress},
			Ingress: []networkingv1.NetworkPolicyIngressRule{{
				From:  ingressPeers,
				Ports: []networkingv1.NetworkPolicyPort{{Port: intstrPtr(agentPort)}},
			}},
			Egress: runtimeNetworkEgress(d.config.MemoryPolicyGatewayURL, d.config.MemoryPolicyGatewayNamespace, d.config.MemoryPolicyGatewayPort, d.config.ToolGatewayURL, d.config.ToolGatewayNamespace, d.config.ToolGatewayPort, d.config.ModelGatewayURL, d.config.ModelGatewayNamespace, d.config.ModelGatewayPort, d.config.KnowledgeGatewayURL, d.config.KnowledgeGatewayNamespace, d.config.KnowledgeGatewayPort, d.config.AgentCommunicationGatewayURL, d.config.AgentCommunicationGatewayNamespace, d.config.AgentCommunicationGatewayPort, d.config.ModelGatewayURL == ""),
		},
	}
	if d.config.WorkloadIdentityMode == "spire" {
		desired.Spec.Ingress = append(desired.Spec.Ingress, networkingv1.NetworkPolicyIngressRule{
			From:  []networkingv1.NetworkPolicyPeer{{NamespaceSelector: namespaceSelector(d.config.ControllerNamespace), PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"app.kubernetes.io/component": "k8s-controller"}}}},
			Ports: []networkingv1.NetworkPolicyPort{{Protocol: protocolPtr(corev1.ProtocolTCP), Port: intstrPtr(8097)}},
		})
	}
	var existing networkingv1.NetworkPolicy
	key := client.ObjectKey{Namespace: namespace, Name: name}
	if err := d.client.Get(ctx, key, &existing); err != nil {
		if errors.IsNotFound(err) {
			return d.client.Create(ctx, desired)
		}
		return err
	}
	if err := d.verifySharedResourceOwner(existing.Annotations, agent, "network policy", name); err != nil {
		return err
	}
	existing.Spec = desired.Spec
	existing.Labels = desired.Labels
	if existing.Annotations == nil {
		existing.Annotations = map[string]string{}
	}
	for key, value := range desired.Annotations {
		existing.Annotations[key] = value
	}
	return d.client.Update(ctx, &existing)
}

func (d *K8sDispatcher) removeRuntimeWorkloadNetworkPolicy(ctx context.Context, namespace, name string, annotations map[string]string, agent *v1alpha1.Agent) error {
	var existing networkingv1.NetworkPolicy
	if err := d.client.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &existing); err != nil {
		if errors.IsNotFound(err) {
			return nil
		}
		return err
	}
	if err := d.verifySharedResourceOwner(existing.Annotations, agent, "network policy", name); err != nil {
		return err
	}
	for key, value := range annotations {
		if existing.Annotations[key] != value {
			return fmt.Errorf("refusing to delete network policy %q with mismatched %s annotation", name, key)
		}
	}
	if err := d.client.Delete(ctx, &existing); err != nil && !errors.IsNotFound(err) {
		return err
	}
	return nil
}

func runtimeNetworkEgress(memoryGatewayURL, memoryGatewayNamespace string, memoryGatewayPort int, toolGatewayURL, toolGatewayNamespace string, toolGatewayPort int, modelGatewayURL, modelGatewayNamespace string, modelGatewayPort int, knowledgeGatewayURL, knowledgeGatewayNamespace string, knowledgeGatewayPort int, agentCommunicationGatewayURL, agentCommunicationGatewayNamespace string, agentCommunicationGatewayPort int, allowPublicHTTPS bool) []networkingv1.NetworkPolicyEgressRule {
	dnsPort := intstr.FromInt(53)
	httpsPort := intstr.FromInt(443)
	rules := []networkingv1.NetworkPolicyEgressRule{{Ports: []networkingv1.NetworkPolicyPort{{Protocol: protocolPtr(corev1.ProtocolUDP), Port: &dnsPort}, {Protocol: protocolPtr(corev1.ProtocolTCP), Port: &dnsPort}}}}
	if allowPublicHTTPS {
		rules = append(rules, networkingv1.NetworkPolicyEgressRule{To: []networkingv1.NetworkPolicyPeer{{IPBlock: &networkingv1.IPBlock{CIDR: "0.0.0.0/0"}}}, Ports: []networkingv1.NetworkPolicyPort{{Protocol: protocolPtr(corev1.ProtocolTCP), Port: &httpsPort}}})
	}
	if memoryGatewayURL != "" {
		if memoryGatewayPort == 0 {
			memoryGatewayPort = 8091
		}
		gatewayPort := intstr.FromInt(memoryGatewayPort)
		rules = append(rules, networkingv1.NetworkPolicyEgressRule{
			To:    []networkingv1.NetworkPolicyPeer{{NamespaceSelector: namespaceSelector(memoryGatewayNamespace), PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"app.kubernetes.io/component": "memory-policy-gateway"}}}},
			Ports: []networkingv1.NetworkPolicyPort{{Protocol: protocolPtr(corev1.ProtocolTCP), Port: &gatewayPort}},
		})
	}
	if toolGatewayURL != "" {
		if toolGatewayPort == 0 {
			toolGatewayPort = 8092
		}
		gatewayPort := intstr.FromInt(toolGatewayPort)
		rules = append(rules, networkingv1.NetworkPolicyEgressRule{
			To:    []networkingv1.NetworkPolicyPeer{{NamespaceSelector: namespaceSelector(toolGatewayNamespace), PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"app.kubernetes.io/component": "tool-gateway"}}}},
			Ports: []networkingv1.NetworkPolicyPort{{Protocol: protocolPtr(corev1.ProtocolTCP), Port: &gatewayPort}},
		})
	}
	if modelGatewayURL != "" {
		if modelGatewayPort == 0 {
			modelGatewayPort = 8093
		}
		gatewayPort := intstr.FromInt(modelGatewayPort)
		rules = append(rules, networkingv1.NetworkPolicyEgressRule{
			To:    []networkingv1.NetworkPolicyPeer{{NamespaceSelector: namespaceSelector(modelGatewayNamespace), PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"app.kubernetes.io/component": "model-gateway"}}}},
			Ports: []networkingv1.NetworkPolicyPort{{Protocol: protocolPtr(corev1.ProtocolTCP), Port: &gatewayPort}},
		})
	}
	if knowledgeGatewayURL != "" {
		if knowledgeGatewayPort == 0 {
			knowledgeGatewayPort = 8095
		}
		gatewayPort := intstr.FromInt(knowledgeGatewayPort)
		rules = append(rules, networkingv1.NetworkPolicyEgressRule{
			To:    []networkingv1.NetworkPolicyPeer{{NamespaceSelector: namespaceSelector(knowledgeGatewayNamespace), PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"app.kubernetes.io/component": "knowledge-gateway"}}}},
			Ports: []networkingv1.NetworkPolicyPort{{Protocol: protocolPtr(corev1.ProtocolTCP), Port: &gatewayPort}},
		})
	}
	if agentCommunicationGatewayURL != "" {
		if agentCommunicationGatewayPort == 0 {
			agentCommunicationGatewayPort = 8094
		}
		gatewayPort := intstr.FromInt(agentCommunicationGatewayPort)
		rules = append(rules, networkingv1.NetworkPolicyEgressRule{
			To:    []networkingv1.NetworkPolicyPeer{{NamespaceSelector: namespaceSelector(agentCommunicationGatewayNamespace), PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"app.kubernetes.io/component": "agent-communication-gateway"}}}},
			Ports: []networkingv1.NetworkPolicyPort{{Protocol: protocolPtr(corev1.ProtocolTCP), Port: &gatewayPort}},
		})
	}
	return rules
}

func (d *K8sDispatcher) ensureRuntimeAuthSecret(ctx context.Context, namespace string) error {
	if d.config.RuntimeAuthToken == "" || namespace == d.config.ControllerNamespace {
		return nil
	}
	key := d.config.RuntimeAuthTokenSecretKey
	if key == "" {
		key = "runtime-control-token"
	}
	var source corev1.Secret
	if err := d.client.Get(ctx, client.ObjectKey{Namespace: d.config.ControllerNamespace, Name: d.config.RuntimeAuthTokenSecretName}, &source); err != nil {
		return fmt.Errorf("get controller token secret: %w", err)
	}
	token, found := source.Data[key]
	if !found || len(token) == 0 {
		return fmt.Errorf("controller token secret %s/%s has no %q key", d.config.ControllerNamespace, d.config.RuntimeAuthTokenSecretName, key)
	}
	owner := d.config.ControllerNamespace + "/" + d.config.RuntimeAuthTokenSecretName
	desired := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: d.config.RuntimeAuthTokenSecretName, Namespace: namespace, Labels: map[string]string{labelManagedBy: managedByValue}, Annotations: map[string]string{"kognition.dev/runtime-auth-source": owner}}, Type: corev1.SecretTypeOpaque, Data: map[string][]byte{key: append([]byte(nil), token...)}}
	var existing corev1.Secret
	objectKey := client.ObjectKey{Namespace: namespace, Name: desired.Name}
	if err := d.client.Get(ctx, objectKey, &existing); err != nil {
		if errors.IsNotFound(err) {
			return d.client.Create(ctx, desired)
		}
		return err
	}
	if existing.Annotations["kognition.dev/runtime-auth-source"] != owner {
		return fmt.Errorf("refusing to modify runtime token secret %s/%s with foreign source", namespace, desired.Name)
	}
	existing.Data = desired.Data
	existing.Labels = desired.Labels
	if existing.Annotations == nil {
		existing.Annotations = map[string]string{}
	}
	existing.Annotations["kognition.dev/runtime-auth-source"] = owner
	return d.client.Update(ctx, &existing)
}

func (d *K8sDispatcher) ensureKnowledgeGatewayCA(ctx context.Context, namespace string) error {
	if d.config.KnowledgeGatewayURL == "" {
		return nil
	}
	if d.config.KnowledgeGatewayNamespace == "" || d.config.KnowledgeGatewayCASecretName == "" || d.config.KnowledgeGatewayCASecretKey == "" {
		return fmt.Errorf("Knowledge Gateway URL requires namespace and CA secret configuration")
	}
	var source corev1.Secret
	sourceKey := client.ObjectKey{Namespace: d.config.KnowledgeGatewayNamespace, Name: d.config.KnowledgeGatewayCASecretName}
	if err := d.client.Get(ctx, sourceKey, &source); err != nil {
		return fmt.Errorf("get gateway CA secret: %w", err)
	}
	ca, found := source.Data[d.config.KnowledgeGatewayCASecretKey]
	if !found || len(ca) == 0 {
		return fmt.Errorf("gateway CA secret %s/%s has no %q key", sourceKey.Namespace, sourceKey.Name, d.config.KnowledgeGatewayCASecretKey)
	}
	owner := sourceKey.Namespace + "/" + sourceKey.Name + ":" + d.config.KnowledgeGatewayCASecretKey
	desired := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name: knowledgeGatewayCAConfigMap, Namespace: namespace,
			Labels:      map[string]string{labelManagedBy: managedByValue},
			Annotations: map[string]string{"kognition.dev/knowledge-gateway-ca-source": owner},
		},
		BinaryData: map[string][]byte{"ca.crt": append([]byte(nil), ca...)},
	}
	var existing corev1.ConfigMap
	objectKey := client.ObjectKey{Namespace: namespace, Name: desired.Name}
	if err := d.client.Get(ctx, objectKey, &existing); err != nil {
		if errors.IsNotFound(err) {
			return d.client.Create(ctx, desired)
		}
		return err
	}
	if existing.Annotations["kognition.dev/knowledge-gateway-ca-source"] != owner {
		return fmt.Errorf("refusing to modify Knowledge Gateway CA ConfigMap %s/%s with foreign source", namespace, desired.Name)
	}
	existing.BinaryData = desired.BinaryData
	existing.Data = nil
	existing.Labels = desired.Labels
	return d.client.Update(ctx, &existing)
}

func (d *K8sDispatcher) knowledgeGatewayTrustVolume() ([]corev1.Volume, []corev1.VolumeMount) {
	if d.config.KnowledgeGatewayURL == "" {
		return nil, nil
	}
	return []corev1.Volume{{
		Name: "knowledge-gateway-ca",
		VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
			LocalObjectReference: corev1.LocalObjectReference{Name: knowledgeGatewayCAConfigMap},
			Items:                []corev1.KeyToPath{{Key: "ca.crt", Path: "ca.crt"}},
		}},
	}}, []corev1.VolumeMount{{Name: "knowledge-gateway-ca", MountPath: "/var/run/kognition/knowledge-gateway", ReadOnly: true}}
}

func namespaceSelector(namespace string) *metav1.LabelSelector {
	if namespace == "" {
		return &metav1.LabelSelector{}
	}
	return &metav1.LabelSelector{MatchLabels: map[string]string{"kubernetes.io/metadata.name": namespace}}
}

func intstrPtr(value int) *intstr.IntOrString {
	port := intstr.FromInt(value)
	return &port
}

func protocolPtr(value corev1.Protocol) *corev1.Protocol {
	return &value
}

func (d *K8sDispatcher) runtimeImage(agent *v1alpha1.Agent) (string, error) {
	runtimeClass := "default"
	if agent.Spec.Runtime != nil && agent.Spec.Runtime.Class != "" {
		runtimeClass = agent.Spec.Runtime.Class
	}
	if runtimeClass == "default" || runtimeClass == "native" {
		return d.config.AgentImage, nil
	}
	if image := d.config.RuntimeImages[runtimeClass]; image != "" {
		return image, nil
	}
	return "", fmt.Errorf("runtime class %q has no configured adapter image", runtimeClass)
}

// EnsureAgentRuntime ensures the long-lived runtime Deployment and Service for
// an Agent match the current dispatcher configuration. This keeps direct
// Studio/CLI agent interactions on the same runtime image and inherited
// OpenTelemetry settings as AgentInstance dispatches.
func (d *K8sDispatcher) EnsureAgentRuntime(ctx context.Context, agent *v1alpha1.Agent) error {
	if agent == nil {
		return fmt.Errorf("agent is required")
	}
	namespace := d.config.AgentNamespace
	if namespace == "" {
		namespace = agent.ObjectMeta.Namespace
	}
	if namespace != "" && namespace == d.config.ControllerNamespace {
		return fmt.Errorf("runtime workloads cannot use installation namespace %q; use a separate enrolled workload namespace", namespace)
	}
	envVars, err := d.runtimeEnv(ctx, agent)
	if err != nil {
		return err
	}
	return d.ensureRuntimeResources(ctx, namespace, agent, envVars)
}

// EnsureAgentServiceRevisionWorkload creates a dedicated runtime for a service
// pinned to an immutable Agent revision. It does not modify the shared current
// Agent runtime.
func (d *K8sDispatcher) EnsureAgentServiceRevisionWorkload(ctx context.Context, serviceNamespace, serviceName string, revision int64, agent *v1alpha1.Agent) (RevisionWorkload, error) {
	if agent == nil {
		return RevisionWorkload{}, fmt.Errorf("revision workload agent is required")
	}
	if revision <= 0 {
		return RevisionWorkload{}, fmt.Errorf("revision must be positive")
	}
	namespace := serviceNamespace
	if d.config.AgentNamespace != "" {
		namespace = d.config.AgentNamespace
	}
	if namespace != "" && namespace == d.config.ControllerNamespace {
		return RevisionWorkload{}, fmt.Errorf("runtime workloads cannot use installation namespace %q; use a separate enrolled workload namespace", namespace)
	}
	envVars, err := d.runtimeEnv(ctx, agent)
	if err != nil {
		return RevisionWorkload{}, err
	}
	name := agentServiceRevisionWorkloadName(serviceNamespace, serviceName, revision)
	selector := map[string]string{
		labelManagedBy:            managedByValue,
		labelAgentServiceWorkload: name,
		labelAgentServiceRevision: strconv.FormatInt(revision, 10),
	}
	annotations := map[string]string{
		annotationOwnerAgentService: serviceNamespace + "/" + serviceName,
		AnnotationOwnerAgent:        agent.ObjectMeta.Namespace + "/" + agent.ObjectMeta.Name,
	}
	if err := d.ensureRuntimeAuthSecret(ctx, namespace); err != nil {
		return RevisionWorkload{}, fmt.Errorf("ensure revision runtime auth secret: %w", err)
	}
	if err := d.ensureKnowledgeGatewayCA(ctx, namespace); err != nil {
		return RevisionWorkload{}, fmt.Errorf("ensure revision Knowledge Gateway CA: %w", err)
	}
	if err := d.ensureDedicatedRevisionDeployment(ctx, namespace, name, agent, selector, annotations, envVars); err != nil {
		return RevisionWorkload{}, err
	}
	if err := d.ensureDedicatedRevisionService(ctx, namespace, name, selector, annotations); err != nil {
		return RevisionWorkload{}, err
	}
	if err := d.ensureRuntimeWorkloadNetworkPolicy(ctx, namespace, name, selector, annotations, agent); err != nil {
		return RevisionWorkload{}, fmt.Errorf("ensure revision runtime network policy: %w", err)
	}
	var deployment appsv1.Deployment
	if err := d.client.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &deployment); err != nil {
		return RevisionWorkload{}, fmt.Errorf("get revision workload readiness: %w", err)
	}
	return RevisionWorkload{Name: name, Namespace: namespace, Selector: selector, ReadyReplicas: deployment.Status.AvailableReplicas}, nil
}

// CleanupAgentServiceRevisionWorkloads removes dedicated revision workloads
// owned by an AgentService. keepName preserves the active workload; an empty
// keepName removes every dedicated workload for the service.
func (d *K8sDispatcher) CleanupAgentServiceRevisionWorkloads(ctx context.Context, serviceNamespace, serviceName string, keepName string) error {
	namespace := serviceNamespace
	if d.config.AgentNamespace != "" {
		namespace = d.config.AgentNamespace
	}
	return d.cleanupAgentServiceRevisionWorkloads(ctx, namespace, serviceNamespace, serviceName, keepName)
}

func (d *K8sDispatcher) runtimeEnv(ctx context.Context, agent *v1alpha1.Agent) ([]corev1.EnvVar, error) {
	model, err := getResource[v1alpha1.Model](ctx, d.store, "Model", agent.ObjectMeta.Namespace, agent.Spec.ModelRef.Name)
	if err != nil && !stderrors.Is(err, store.ErrNotFound) {
		return nil, fmt.Errorf("resolve model %s/%s: %w", agent.ObjectMeta.Namespace, agent.Spec.ModelRef.Name, err)
	}
	if model == nil {
		model = &v1alpha1.Model{}
		model.Spec.ModelID = agent.Spec.ModelRef.Name
	}
	provider := model.Spec.Provider
	if provider == "" && model.Spec.ClassName != "" {
		modelClass, err := getResource[v1alpha1.ModelClass](ctx, d.store, "ModelClass", "", model.Spec.ClassName)
		if err != nil && !stderrors.Is(err, store.ErrNotFound) {
			return nil, fmt.Errorf("lookup model class %q: %w", model.Spec.ClassName, err)
		}
		if modelClass != nil {
			provider = modelClass.Spec.Provisioner
		}
	}
	provider = normalizeModelGatewayProvider(provider)
	if d.config.ModelGatewayURL != "" {
		if err := requireModelGatewayProvider(provider, model); err != nil {
			return nil, err
		}
	}
	envVars, err := d.buildEnvVars(ctx, agent.ObjectMeta.Namespace, agent, model, nil, provider, "")
	if err != nil {
		return nil, err
	}
	if d.config.ModelGatewayURL != "" {
		return envVars, nil
	}
	return mergeEnvVars(envVars, d.resolveCredentialEnv(ctx, agent.ObjectMeta.Namespace, model)), nil
}

// mergeEnvVars applies overrides by name while retaining stable order. Runtime
// base configuration may include an empty placeholder (such as
// KOGNITION_API_KEY); credential projection must replace it rather than append
// a duplicate, because Kubernetes resolves the first matching environment name.
func mergeEnvVars(base, overrides []corev1.EnvVar) []corev1.EnvVar {
	if len(overrides) == 0 {
		return base
	}
	merged := append([]corev1.EnvVar(nil), base...)
	indexes := make(map[string]int, len(merged))
	for index, env := range merged {
		indexes[env.Name] = index
	}
	for _, env := range overrides {
		if index, exists := indexes[env.Name]; exists {
			merged[index] = env
			continue
		}
		indexes[env.Name] = len(merged)
		merged = append(merged, env)
	}
	return merged
}

func (d *K8sDispatcher) ensureDedicatedRevisionDeployment(ctx context.Context, namespace, name string, agent *v1alpha1.Agent, labels, annotations map[string]string, envVars []corev1.EnvVar) error {
	runtimeImage, err := d.runtimeImage(agent)
	if err != nil {
		return err
	}
	replicas := int32(1)
	if agent.Spec.Replicas != nil && *agent.Spec.Replicas > 0 {
		replicas = *agent.Spec.Replicas
	}
	volumes, volumeMounts := d.knowledgeGatewayTrustVolume()
	desired := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Labels: labels, Annotations: annotations},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{AutomountServiceAccountToken: ptr.To(false), SecurityContext: &corev1.PodSecurityContext{
					RunAsNonRoot: ptr.To(true), RunAsUser: ptr.To(int64(1000)),
					SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
				}, Volumes: volumes, Containers: []corev1.Container{
					{
						Name:  "agent",
						Image: runtimeImage,
						SecurityContext: &corev1.SecurityContext{
							AllowPrivilegeEscalation: ptr.To(false),
							Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
						},
						Ports:        []corev1.ContainerPort{{Name: "http", ContainerPort: int32(agentPort), Protocol: corev1.ProtocolTCP}},
						Env:          envVars,
						VolumeMounts: volumeMounts,
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m"), corev1.ResourceMemory: resource.MustParse("128Mi")},
							Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m"), corev1.ResourceMemory: resource.MustParse("512Mi")},
						},
						LivenessProbe:  &corev1.Probe{ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: "/healthz", Port: intstr.FromInt(agentPort)}}, InitialDelaySeconds: 5, PeriodSeconds: 10},
						ReadinessProbe: &corev1.Probe{ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: "/readyz", Port: intstr.FromInt(agentPort)}}, InitialDelaySeconds: 3, PeriodSeconds: 5},
						StartupProbe: &corev1.Probe{
							ProbeHandler:   corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: "/healthz", Port: intstr.FromInt(agentPort), Scheme: corev1.URISchemeHTTP}},
							TimeoutSeconds: 5, PeriodSeconds: 5, FailureThreshold: 24, SuccessThreshold: 1,
						},
					},
				}},
			},
		},
	}
	var existing appsv1.Deployment
	if err := d.addWorkloadIdentityProxy(desired, agent); err != nil {
		return err
	}
	key := client.ObjectKey{Namespace: namespace, Name: name}
	if err := d.client.Get(ctx, key, &existing); err != nil {
		if errors.IsNotFound(err) {
			return d.client.Create(ctx, desired)
		}
		return fmt.Errorf("get revision deployment: %w", err)
	}
	if existing.Annotations[annotationOwnerAgentService] != annotations[annotationOwnerAgentService] {
		return fmt.Errorf("refusing to modify revision deployment %q owned by %q", name, existing.Annotations[annotationOwnerAgentService])
	}
	if !reflect.DeepEqual(existing.Spec.Selector, desired.Spec.Selector) {
		return fmt.Errorf("revision deployment %q selector does not match its immutable workload identity", name)
	}
	needUpdate := false
	if existing.Labels == nil {
		existing.Labels = make(map[string]string)
	}
	for key, value := range labels {
		if existing.Labels[key] != value {
			existing.Labels[key] = value
			needUpdate = true
		}
	}
	if existing.Annotations == nil {
		existing.Annotations = make(map[string]string)
	}
	for key, value := range annotations {
		if existing.Annotations[key] != value {
			existing.Annotations[key] = value
			needUpdate = true
		}
	}
	if existing.Spec.Replicas == nil || *existing.Spec.Replicas != replicas {
		existing.Spec.Replicas = &replicas
		needUpdate = true
	}
	if existing.Spec.Template.Labels == nil {
		existing.Spec.Template.Labels = make(map[string]string)
	}
	for key, value := range labels {
		if existing.Spec.Template.Labels[key] != value {
			existing.Spec.Template.Labels[key] = value
			needUpdate = true
		}
	}
	if len(existing.Spec.Template.Spec.Containers) == 0 {
		existing.Spec.Template.Spec.Containers = desired.Spec.Template.Spec.Containers
		needUpdate = true
	} else {
		container := &existing.Spec.Template.Spec.Containers[0]
		desiredContainer := desired.Spec.Template.Spec.Containers[0]
		if container.Image != desiredContainer.Image {
			container.Image = desiredContainer.Image
			needUpdate = true
		}
		if !envVarsEqual(container.Env, desiredContainer.Env) {
			container.Env = desiredContainer.Env
			needUpdate = true
		}
		if !reflect.DeepEqual(container.VolumeMounts, desiredContainer.VolumeMounts) {
			container.VolumeMounts = desiredContainer.VolumeMounts
			needUpdate = true
		}
		if !reflect.DeepEqual(container.StartupProbe, desiredContainer.StartupProbe) {
			container.StartupProbe = desiredContainer.StartupProbe
			needUpdate = true
		}
	}
	if !reflect.DeepEqual(existing.Spec.Template.Spec.Volumes, desired.Spec.Template.Spec.Volumes) {
		existing.Spec.Template.Spec.Volumes = desired.Spec.Template.Spec.Volumes
		needUpdate = true
	}
	if d.updateWorkloadIdentityProxy(&existing, desired) {
		needUpdate = true
	}
	if !needUpdate {
		return nil
	}
	return d.client.Update(ctx, &existing)
}

func (d *K8sDispatcher) cleanupAgentServiceRevisionWorkloads(ctx context.Context, namespace, serviceNamespace, serviceName, keepName string) error {
	owner := serviceNamespace + "/" + serviceName
	var deployments appsv1.DeploymentList
	if err := d.client.List(ctx, &deployments, client.InNamespace(namespace)); err != nil {
		return fmt.Errorf("list revision deployments: %w", err)
	}
	var services corev1.ServiceList
	if err := d.client.List(ctx, &services, client.InNamespace(namespace)); err != nil {
		return fmt.Errorf("list revision services: %w", err)
	}
	var policies networkingv1.NetworkPolicyList
	if err := d.client.List(ctx, &policies, client.InNamespace(namespace)); err != nil {
		return fmt.Errorf("list revision network policies: %w", err)
	}
	for index := range deployments.Items {
		deployment := &deployments.Items[index]
		if deployment.Labels[labelAgentServiceWorkload] == "" || deployment.Annotations[annotationOwnerAgentService] != owner || deployment.Name == keepName {
			continue
		}
		if err := d.client.Delete(ctx, deployment, client.Preconditions{UID: &deployment.UID}, client.PropagationPolicy(metav1.DeletePropagationForeground)); err != nil && !errors.IsNotFound(err) {
			return fmt.Errorf("delete stale revision deployment %q: %w", deployment.Name, err)
		}
		var remaining appsv1.Deployment
		if err := d.client.Get(ctx, client.ObjectKeyFromObject(deployment), &remaining); !errors.IsNotFound(err) {
			if err != nil {
				return fmt.Errorf("confirm stale revision deployment %q deletion: %w", deployment.Name, err)
			}
			return fmt.Errorf("stale revision deployment %q still exists; retaining network policies", deployment.Name)
		}
	}
	for index := range policies.Items {
		policy := &policies.Items[index]
		if policy.Labels[labelAgentServiceWorkload] == "" || policy.Annotations[annotationOwnerAgentService] != owner || policy.Name == keepName+"-network" {
			continue
		}
		if err := d.client.Delete(ctx, policy, client.Preconditions{UID: &policy.UID}); err != nil && !errors.IsNotFound(err) {
			return fmt.Errorf("delete stale revision network policy %q: %w", policy.Name, err)
		}
	}
	for index := range services.Items {
		service := &services.Items[index]
		if service.Labels[labelAgentServiceWorkload] == "" || service.Annotations[annotationOwnerAgentService] != owner || service.Name == keepName {
			continue
		}
		if err := d.client.Delete(ctx, service, client.Preconditions{UID: &service.UID}); err != nil && !errors.IsNotFound(err) {
			return fmt.Errorf("delete stale revision service %q: %w", service.Name, err)
		}
	}
	return nil
}

func (d *K8sDispatcher) ensureDedicatedRevisionService(ctx context.Context, namespace, name string, selector, annotations map[string]string) error {
	desired := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Labels: selector, Annotations: annotations},
		Spec: corev1.ServiceSpec{
			Selector: selector,
			Ports: []corev1.ServicePort{{
				Name:       "http",
				Port:       int32(agentPort),
				TargetPort: intstr.FromInt(agentPort),
				Protocol:   corev1.ProtocolTCP,
			}},
			Type: corev1.ServiceTypeClusterIP,
		},
	}
	desired.Spec.Ports = d.workloadIdentityServicePorts(desired.Spec.Ports)
	var existing corev1.Service
	if err := d.client.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &existing); err != nil {
		if errors.IsNotFound(err) {
			return d.client.Create(ctx, desired)
		}
		return fmt.Errorf("get revision service: %w", err)
	}
	if existing.Annotations[annotationOwnerAgentService] != annotations[annotationOwnerAgentService] || !reflect.DeepEqual(existing.Spec.Selector, selector) || !reflect.DeepEqual(existing.Spec.Ports, desired.Spec.Ports) {
		return fmt.Errorf("revision service %q does not match its immutable workload identity", name)
	}
	return nil
}

func agentServiceRevisionWorkloadName(serviceNamespace, serviceName string, revision int64) string {
	digest := sha256.Sum256([]byte(serviceNamespace + "\x00" + serviceName + "\x00" + strconv.FormatInt(revision, 10)))
	prefix := strings.ToLower(strings.ReplaceAll(serviceName, "_", "-"))
	prefix = strings.Trim(prefix, "-.")
	if prefix == "" {
		prefix = "service"
	}
	const suffixLength = 1 + 8
	const maxPrefixLength = 63 - len("agent-svc-") - suffixLength
	if len(prefix) > maxPrefixLength {
		prefix = prefix[:maxPrefixLength]
	}
	return fmt.Sprintf("agent-svc-%s-%x", strings.TrimRight(prefix, "-"), digest[:4])
}

// envVarsEqual reports whether two env-var slices are semantically identical,
// independent of ordering. Used to suppress no-op Deployment updates that
// would otherwise trigger pod rollouts on a shared Deployment.
func envVarsEqual(a, b []corev1.EnvVar) bool {
	if len(a) != len(b) {
		return false
	}
	index := make(map[string]corev1.EnvVar, len(a))
	for _, e := range a {
		index[e.Name] = e
	}
	for _, e := range b {
		prev, ok := index[e.Name]
		if !ok || prev.Value != e.Value || !reflect.DeepEqual(prev.ValueFrom, e.ValueFrom) {
			return false
		}
	}
	return true
}

// ensureService creates the ClusterIP Service for agent pods.
func (d *K8sDispatcher) ensureService(ctx context.Context, namespace string, agent *v1alpha1.Agent) error {
	workloadKey := d.workloadKey(agent.ObjectMeta.Namespace, agent.ObjectMeta.Name)
	name := serviceName(workloadKey)
	labels := map[string]string{
		labelManagedBy:      managedByValue,
		labelAgent:          workloadKey,
		labelAgentName:      agent.ObjectMeta.Name,
		labelAgentNamespace: agent.ObjectMeta.Namespace,
	}
	annotations := map[string]string{
		AnnotationOwnerAgent: agent.ObjectMeta.Namespace + "/" + agent.ObjectMeta.Name,
	}

	desired := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   namespace,
			Labels:      labels,
			Annotations: annotations,
		},
		Spec: corev1.ServiceSpec{
			Selector: labels,
			Ports: []corev1.ServicePort{
				{
					Name:       "http",
					Port:       int32(agentPort),
					TargetPort: intstr.FromInt(agentPort),
					Protocol:   corev1.ProtocolTCP,
				},
			},
			Type: corev1.ServiceTypeClusterIP,
		},
	}

	desired.Spec.Ports = d.workloadIdentityServicePorts(desired.Spec.Ports)
	var existing corev1.Service
	key := client.ObjectKey{Namespace: namespace, Name: name}
	err := d.client.Get(ctx, key, &existing)
	if err != nil {
		if errors.IsNotFound(err) {
			d.logger.Info("creating agent service",
				zap.String("service", name),
				zap.String("namespace", namespace),
			)
			return d.client.Create(ctx, desired)
		}
		return fmt.Errorf("get service: %w", err)
	}
	if err := d.verifySharedResourceOwner(existing.Annotations, agent, "service", name); err != nil {
		return err
	}
	ports := d.workloadIdentityServicePorts(existing.Spec.Ports)
	portsChanged := !reflect.DeepEqual(existing.Spec.Ports, ports)
	if portsChanged {
		existing.Spec.Ports = ports
	}

	// Service already exists; ensure annotation is set.
	if existing.Annotations == nil {
		existing.Annotations = make(map[string]string)
	}
	if existing.Annotations[AnnotationOwnerAgent] != annotations[AnnotationOwnerAgent] {
		existing.Annotations[AnnotationOwnerAgent] = annotations[AnnotationOwnerAgent]
		return d.client.Update(ctx, &existing)
	}
	if portsChanged {
		return d.client.Update(ctx, &existing)
	}
	return nil
}

// CleanupAgentResources deletes the Deployment and Service for an Agent.
// Used during Agent deletion to clean up cross-namespace resources.
func (d *K8sDispatcher) CleanupAgentResources(ctx context.Context, agentName, fallbackNamespace string) error {
	namespace := d.config.AgentNamespace
	if namespace == "" {
		namespace = fallbackNamespace
	}
	workloadKey := d.workloadKey(fallbackNamespace, agentName)

	agent := &v1alpha1.Agent{ObjectMeta: v1alpha1.ObjectMeta{Name: agentName, Namespace: fallbackNamespace}}
	var deploy appsv1.Deployment
	deployKey := client.ObjectKey{Namespace: namespace, Name: deploymentName(workloadKey)}
	if err := d.client.Get(ctx, deployKey, &deploy); err == nil {
		if err := d.verifySharedResourceOwner(deploy.Annotations, agent, "deployment", deploy.Name); err != nil {
			return err
		}
		if err := d.client.Delete(ctx, &deploy, client.Preconditions{UID: &deploy.UID}); err != nil && !errors.IsNotFound(err) {
			return fmt.Errorf("delete deployment %s/%s: %w", namespace, deploy.Name, err)
		}
	} else if !errors.IsNotFound(err) {
		return fmt.Errorf("get deployment %s/%s for deletion: %w", namespace, deployKey.Name, err)
	}

	var svc corev1.Service
	svcKey := client.ObjectKey{Namespace: namespace, Name: serviceName(workloadKey)}
	if err := d.client.Get(ctx, svcKey, &svc); err == nil {
		if err := d.verifySharedResourceOwner(svc.Annotations, agent, "service", svc.Name); err != nil {
			return err
		}
		if err := d.client.Delete(ctx, &svc, client.Preconditions{UID: &svc.UID}); err != nil && !errors.IsNotFound(err) {
			return fmt.Errorf("delete service %s/%s: %w", namespace, svc.Name, err)
		}
	} else if !errors.IsNotFound(err) {
		return fmt.Errorf("get service %s/%s for deletion: %w", namespace, svcKey.Name, err)
	}

	var policy networkingv1.NetworkPolicy
	policyKey := client.ObjectKey{Namespace: namespace, Name: deploymentName(workloadKey) + "-network"}
	if err := d.client.Get(ctx, policyKey, &policy); err == nil {
		if err := d.verifySharedResourceOwner(policy.Annotations, agent, "network policy", policy.Name); err != nil {
			return err
		}
		if err := d.client.Delete(ctx, &policy, client.Preconditions{UID: &policy.UID}); err != nil && !errors.IsNotFound(err) {
			return fmt.Errorf("delete network policy %s/%s: %w", namespace, policy.Name, err)
		}
	} else if !errors.IsNotFound(err) {
		return fmt.Errorf("get network policy %s/%s for deletion: %w", namespace, policyKey.Name, err)
	}
	if err := d.removeOwnedLegacyResources(ctx, namespace, agent); err != nil {
		return fmt.Errorf("delete legacy runtime resources: %w", err)
	}

	d.logger.Info("cleaned up agent deployment resources",
		zap.String("agent", agentName),
		zap.String("namespace", namespace),
	)
	return nil
}

// EnsureOwnerAnnotations patches the owner-agent annotation on an existing
// Deployment and Service for the given agent. This is called during reconciliation
// to ensure cross-namespace resources are linked back to their Agent CRD.
// If the Deployment or Service does not exist yet, it is silently skipped.
func (d *K8sDispatcher) EnsureOwnerAnnotations(ctx context.Context, agentName, agentNamespace string) error {
	namespace := d.config.AgentNamespace
	if namespace == "" {
		namespace = agentNamespace
	}
	ownerRef := agentNamespace + "/" + agentName
	workloadKey := d.workloadKey(agentNamespace, agentName)

	// Patch Deployment annotation.
	var deploy appsv1.Deployment
	deployKey := client.ObjectKey{Namespace: namespace, Name: deploymentName(workloadKey)}
	if err := d.client.Get(ctx, deployKey, &deploy); err != nil {
		if !errors.IsNotFound(err) {
			return fmt.Errorf("get deployment for annotation patch: %w", err)
		}
	} else {
		if deploy.Annotations[AnnotationOwnerAgent] != "" {
			if err := d.verifySharedResourceOwner(deploy.Annotations, &v1alpha1.Agent{ObjectMeta: v1alpha1.ObjectMeta{Name: agentName, Namespace: agentNamespace}}, "deployment", deploy.Name); err != nil {
				return err
			}
		}
		if deploy.Annotations[AnnotationOwnerAgent] != ownerRef {
			if deploy.Annotations == nil {
				deploy.Annotations = make(map[string]string)
			}
			deploy.Annotations[AnnotationOwnerAgent] = ownerRef
			if err := d.client.Update(ctx, &deploy); err != nil {
				return fmt.Errorf("update deployment annotation: %w", err)
			}
			d.logger.Info("patched owner annotation on deployment",
				zap.String("deployment", deploy.Name),
				zap.String("ownerRef", ownerRef),
			)
		}
	}

	// Patch Service annotation.
	var svc corev1.Service
	svcKey := client.ObjectKey{Namespace: namespace, Name: serviceName(workloadKey)}
	if err := d.client.Get(ctx, svcKey, &svc); err != nil {
		if !errors.IsNotFound(err) {
			return fmt.Errorf("get service for annotation patch: %w", err)
		}
	} else {
		if svc.Annotations[AnnotationOwnerAgent] != "" {
			if err := d.verifySharedResourceOwner(svc.Annotations, &v1alpha1.Agent{ObjectMeta: v1alpha1.ObjectMeta{Name: agentName, Namespace: agentNamespace}}, "service", svc.Name); err != nil {
				return err
			}
		}
		if svc.Annotations[AnnotationOwnerAgent] != ownerRef {
			if svc.Annotations == nil {
				svc.Annotations = make(map[string]string)
			}
			svc.Annotations[AnnotationOwnerAgent] = ownerRef
			if err := d.client.Update(ctx, &svc); err != nil {
				return fmt.Errorf("update service annotation: %w", err)
			}
			d.logger.Info("patched owner annotation on service",
				zap.String("service", svc.Name),
				zap.String("ownerRef", ownerRef),
			)
		}
	}

	return nil
}

// buildEnvVars constructs the environment variables for the agent container.
//
// IMPORTANT: every value here ends up baked into the Deployment's PodTemplate.
// Because one Deployment is shared by every AgentInstance of an Agent (and by
// every Agent CR of the same name across tenant namespaces), any value that
// varies per-instance would mutate the template on each Dispatch, triggering a
// pod rollout that never converges. Per-instance/session data is delivered via
// the /invoke request body instead.
func (d *K8sDispatcher) buildEnvVars(ctx context.Context, crNamespace string, agent *v1alpha1.Agent, model *v1alpha1.Model, _ *v1alpha1.AgentInstance, provider, apiKey string) ([]corev1.EnvVar, error) {
	if d.config.ModelGatewayURL != "" {
		return d.buildGatewayModelEnvVars(ctx, crNamespace, agent, model)
	}
	if model.Spec.InvocationProfileRef != nil {
		return nil, fmt.Errorf("profile-backed Python runtimes require the Model Gateway; direct provider execution is unsupported")
	}
	envs := []corev1.EnvVar{
		{Name: "KOGNITION_AGENT_NAME", Value: agent.ObjectMeta.Name},
		{Name: "KOGNITION_MODEL_PROVIDER", Value: provider},
		{Name: "KOGNITION_MODEL_ID", Value: model.Spec.ModelID},
		{Name: "KOGNITION_MODEL_ENDPOINT", Value: model.Spec.Endpoint},
		{Name: "KOGNITION_API_KEY", Value: apiKey},
	}
	if d.config.RuntimeAuthToken != "" && d.config.RuntimeAuthTokenSecretName != "" {
		key := d.config.RuntimeAuthTokenSecretKey
		if key == "" {
			key = "runtime-control-token"
		}
		envs = append(envs, corev1.EnvVar{Name: "KOGNITION_RUNTIME_AUTH_TOKEN", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: d.config.RuntimeAuthTokenSecretName}, Key: key}}})
	}
	envs = append(envs, inheritedOpenTelemetryEnv()...)

	// API version from model parameters (e.g. Azure OpenAI api-version).
	if v := model.Spec.Parameters["apiVersion"]; v != "" {
		envs = append(envs, corev1.EnvVar{Name: "KOGNITION_API_VERSION", Value: v})
	}

	// System prompt from Agent instructions.
	if agent.Spec.Instructions.Inline != "" {
		envs = append(envs, corev1.EnvVar{
			Name:  "KOGNITION_SYSTEM_PROMPT",
			Value: agent.Spec.Instructions.Inline,
		})
	}

	// Runtime configuration.
	if agent.Spec.Runtime != nil {
		if agent.Spec.Runtime.MaxTokens != nil {
			envs = append(envs, corev1.EnvVar{
				Name:  "KOGNITION_MAX_TOKENS",
				Value: strconv.FormatInt(*agent.Spec.Runtime.MaxTokens, 10),
			})
		}
		if agent.Spec.Runtime.Timeout != "" {
			envs = append(envs, corev1.EnvVar{
				Name:  "KOGNITION_TIMEOUT_SECONDS",
				Value: agent.Spec.Runtime.Timeout,
			})
		}
	}

	return d.appendRuntimeDataPlaneEnv(ctx, crNamespace, agent, envs)
}

func (d *K8sDispatcher) buildGatewayModelEnvVars(ctx context.Context, crNamespace string, agent *v1alpha1.Agent, model *v1alpha1.Model) ([]corev1.EnvVar, error) {
	envs := []corev1.EnvVar{
		{Name: "KOGNITION_NAMESPACE", Value: crNamespace},
		{Name: "KOGNITION_AGENT_NAME", Value: agent.ObjectMeta.Name},
		{Name: "KOGNITION_MODEL_PROVIDER", Value: "openai-compatible"},
		{Name: "KOGNITION_MODEL_ID", Value: model.Spec.ModelID},
		{Name: "KOGNITION_MODEL_GATEWAY_URL", Value: d.config.ModelGatewayURL},
	}
	servingModel := model
	if model.Spec.ClassName != "" {
		class, err := getResource[v1alpha1.ModelClass](ctx, d.store, "ModelClass", "", model.Spec.ClassName)
		if err != nil {
			return nil, fmt.Errorf("resolve invocation profile class: %w", err)
		}
		if class.Status.ActiveBinding != nil {
			servingModel, err = runtimeauth.ResolveModelBinding(ctx, d.store, *class.Status.ActiveBinding)
			if err != nil {
				return nil, err
			}
		}
	}
	if servingModel.Spec.InvocationProfileRef != nil {
		if _, err := modelinvocation.Load(ctx, d.store, servingModel); err != nil {
			return nil, err
		}
		envs = append(envs, corev1.EnvVar{Name: "KOGNITION_MODEL_INVOCATION_PROFILE", Value: "true"})
		// Use an opaque logical name so SDK model-family defaults cannot change intent.
		for index := range envs {
			if envs[index].Name == "KOGNITION_MODEL_ID" {
				envs[index].Value = "kognition-model"
			}
		}
	}
	if d.config.WorkloadIdentityMode == "spire" {
		envs = append(envs, corev1.EnvVar{Name: "KOGNITION_WORKLOAD_IDENTITY_MODE", Value: "spire"}, corev1.EnvVar{Name: "KOGNITION_WORKLOAD_PROXY_URL", Value: d.config.WorkloadProxyURL})
	}
	if d.config.RuntimeAuthToken != "" && d.config.RuntimeAuthTokenSecretName != "" {
		key := d.config.RuntimeAuthTokenSecretKey
		if key == "" {
			key = "runtime-control-token"
		}
		envs = append(envs, corev1.EnvVar{Name: "KOGNITION_RUNTIME_AUTH_TOKEN", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: d.config.RuntimeAuthTokenSecretName}, Key: key}}})
	}
	envs = append(envs, inheritedOpenTelemetryEnv()...)
	if agent.Spec.Instructions.Inline != "" {
		envs = append(envs, corev1.EnvVar{Name: "KOGNITION_SYSTEM_PROMPT", Value: agent.Spec.Instructions.Inline})
	}
	if agent.Spec.Runtime != nil {
		if strings.TrimSpace(agent.Spec.Runtime.Class) == "autogen" {
			envs = append(envs, corev1.EnvVar{Name: "KOGNITION_RUNTIME_CLASS", Value: "autogen"})
			envs = append(envs, corev1.EnvVar{Name: "KOGNITION_EXTERNAL_RUNTIME", Value: "true"})
			if agent.Spec.Memory == nil || len(agent.Spec.Memory.Claims) == 0 {
				envs = append(envs, corev1.EnvVar{Name: "KOGNITION_MEMORY_SERVICE_URL", Value: d.config.MemoryPolicyGatewayURL})
			}
			if d.externalEndpoints == nil {
				envs = append(envs, corev1.EnvVar{Name: "KOGNITION_CONTROL_TLS_CERT_FILE", Value: "/etc/kognition/runtime-control-tls/tls.crt"}, corev1.EnvVar{Name: "KOGNITION_CONTROL_TLS_KEY_FILE", Value: "/etc/kognition/runtime-control-tls/tls.key"})
			}
		}
		if agent.Spec.Runtime.MaxTokens != nil {
			envs = append(envs, corev1.EnvVar{Name: "KOGNITION_MAX_TOKENS", Value: strconv.FormatInt(*agent.Spec.Runtime.MaxTokens, 10)})
		}
		if agent.Spec.Runtime.Timeout != "" {
			envs = append(envs, corev1.EnvVar{Name: "KOGNITION_TIMEOUT_SECONDS", Value: agent.Spec.Runtime.Timeout})
		}
	}
	return d.appendRuntimeDataPlaneEnv(ctx, crNamespace, agent, envs)
}

func normalizeModelGatewayProvider(provider string) string {
	return cpi.CanonicalProvisioner(strings.ToLower(strings.TrimSpace(provider)))
}

// requireModelGatewayProvider admits OpenAI-compatible and Foundry Models
// directly. Any other provider is served through its CPI adapter only with an
// explicit ModelInvocationProfile, which the Model Gateway re-verifies and pins.
func requireModelGatewayProvider(provider string, model *v1alpha1.Model) error {
	if provider == "openai-compatible" || provider == "foundry" {
		return nil
	}
	if provider != "" && model != nil && model.Spec.InvocationProfileRef != nil {
		return nil
	}
	return fmt.Errorf("Model Gateway serves OpenAI-compatible or Foundry Models directly; provider %q requires an explicit ModelInvocationProfile", provider)
}

func (d *K8sDispatcher) appendRuntimeDataPlaneEnv(ctx context.Context, crNamespace string, agent *v1alpha1.Agent, envs []corev1.EnvVar) ([]corev1.EnvVar, error) {
	// Tools are injected twice: raw AgentToolRefs for compatibility and resolved
	// metadata for the runtime. Tool Gateway mode deliberately redacts endpoints.
	if len(agent.Spec.Tools) > 0 {
		if toolsJSON, err := json.Marshal(agent.Spec.Tools); err == nil {
			envs = append(envs, corev1.EnvVar{Name: "KOGNITION_TOOLS_JSON", Value: string(toolsJSON)})
		}
		resolved := d.resolveAgentTools(ctx, crNamespace, agent.Spec.Tools, d.config.ToolGatewayURL == "")
		if encoded := agent.Annotations["kognition.dev/kagent-tool-aliases"]; encoded != "" {
			var aliases map[string]string
			strictErrors, strictErr := strictjson.UnmarshalStrict([]byte(encoded), &aliases)
			decoder := json.NewDecoder(strings.NewReader(encoded))
			if len(encoded) > 8192 || strictErr != nil || len(strictErrors) != 0 || decoder.Decode(&aliases) != nil || len(aliases) != len(agent.Spec.Tools) || len(resolved) != len(agent.Spec.Tools) {
				return nil, fmt.Errorf("imported tool alias mapping is malformed or unresolved")
			}
			if err := decoder.Decode(new(any)); err != io.EOF {
				return nil, fmt.Errorf("imported tool alias mapping contains trailing data")
			}
			valid := regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,63}$`)
			seen := map[string]bool{}
			for index := range resolved {
				alias := aliases[resolved[index].Name]
				if !valid.MatchString(alias) || seen[alias] {
					return nil, fmt.Errorf("imported tool alias mapping is incomplete or ambiguous")
				}
				seen[alias] = true
				resolved[index].RuntimeName = alias
			}
		}
		if len(resolved) > 0 {
			if resolvedJSON, err := json.Marshal(resolved); err == nil {
				envs = append(envs, corev1.EnvVar{Name: "KOGNITION_TOOLS_RESOLVED", Value: string(resolvedJSON)})
			}
		}
	}
	memoryEnabled := agent.Spec.Memory != nil && len(agent.Spec.Memory.Claims) > 0
	envs = append(envs, corev1.EnvVar{Name: "KOGNITION_MEMORY_ENABLED", Value: strconv.FormatBool(memoryEnabled)})
	if memoryEnabled {
		if volume := d.resolveMemoryBackendHandle(ctx, crNamespace, agent.Spec.Memory.Claims); volume != "" {
			envs = append(envs, corev1.EnvVar{Name: "KOGNITION_MEMORY_VOLUME", Value: volume})
		}
		memoryURL := d.config.MemoryServiceURL
		if d.config.MemoryPolicyGatewayURL != "" {
			memoryURL = d.config.MemoryPolicyGatewayURL
		}
		if memoryURL != "" {
			envs = append(envs, corev1.EnvVar{Name: "KOGNITION_MEMORY_SERVICE_URL", Value: memoryURL})
		}
	}
	if d.config.ToolGatewayURL != "" {
		envs = append(envs, corev1.EnvVar{Name: "KOGNITION_TOOL_GATEWAY_URL", Value: d.config.ToolGatewayURL})
	}
	if d.config.AgentCommunicationGatewayURL != "" {
		envs = append(envs, corev1.EnvVar{Name: "KOGNITION_AGENT_COMMUNICATION_GATEWAY_URL", Value: d.config.AgentCommunicationGatewayURL})
	}
	if value, explicit := agent.Annotations["kognition.dev/agent-delegation-enabled"]; explicit {
		if value != "true" && value != "false" {
			return nil, fmt.Errorf("explicit Agent delegation setting must be true or false")
		}
		envs = append(envs, corev1.EnvVar{Name: "KOGNITION_AGENT_DELEGATION_ENABLED", Value: value})
	}
	if d.config.KnowledgeGatewayURL != "" {
		envs = append(envs, corev1.EnvVar{Name: "KOGNITION_KNOWLEDGE_GATEWAY_URL", Value: d.config.KnowledgeGatewayURL})
		envs = append(envs, corev1.EnvVar{Name: "KOGNITION_KNOWLEDGE_GATEWAY_CA_FILE", Value: knowledgeGatewayCAFile})
	}
	if len(agent.Spec.KnowledgeBaseClaims) > 0 {
		resolvedKBs, err := d.resolveAgentKBs(ctx, crNamespace, agent.Spec.KnowledgeBaseClaims)
		if err != nil {
			return nil, err
		}
		if len(resolvedKBs) > 0 {
			if kbJSON, err := json.Marshal(resolvedKBs); err == nil {
				envs = append(envs, corev1.EnvVar{Name: "KOGNITION_KB_RESOLVED", Value: string(kbJSON)})
			}
		}
	}
	return envs, nil
}

func inheritedOpenTelemetryEnv() []corev1.EnvVar {
	names := []string{
		"KOGNITION_OTEL_TRACES_ENABLED",
		"KOGNITION_OTEL_TRACES_ENDPOINT",
		"KOGNITION_OTEL_TRACES_PROTOCOL",
		"KOGNITION_OTEL_TRACES_SAMPLE_RATE",
		"KOGNITION_OTEL_TRACES_SERVICE_NAME",
		"KOGNITION_OTEL_TRACES_SERVICE_VERSION",
		"KOGNITION_OTEL_TRACES_ENVIRONMENT",
		"KOGNITION_OTEL_TRACES_HEADERS",
		"KOGNITION_OTEL_TRACES_RESOURCE_ATTRIBUTES",
	}
	envs := make([]corev1.EnvVar, 0, len(names))
	for _, name := range names {
		if value := os.Getenv(name); value != "" {
			envs = append(envs, corev1.EnvVar{Name: name, Value: value})
		}
	}
	return envs
}

// resolvedTool is the shape injected into KOGNITION_TOOLS_RESOLVED. It is a
// runtime contract — see runtime/agent/graph.py for the consumer.
type resolvedTool struct {
	Name            string         `json:"name"`
	RuntimeName     string         `json:"runtimeName,omitempty"`
	ResourceVersion int64          `json:"resourceVersion"`
	Description     string         `json:"description,omitempty"`
	Protocol        string         `json:"protocol,omitempty"`
	Endpoint        string         `json:"endpoint,omitempty"`
	InputSchema     map[string]any `json:"inputSchema,omitempty"`
	ClassName       string         `json:"className,omitempty"`
}

// resolveMemoryBackendHandle picks the provisioned backend handle to inject
// into the agent runtime so that two agents referencing the same MemoryClaim
// converge on the same backend bucket. Preference order:
//  1. The first claim of type=shared with a non-empty BoundVolumeName
//     (so shared claims actually share across agents).
//  2. The first claim of any type with a non-empty BoundVolumeName.
//
// Returns "" if no claim is bound yet; the runtime then falls back to
// using the agent name so older deployments keep working.
func (d *K8sDispatcher) resolveMemoryBackendHandle(ctx context.Context, namespace string, refs []v1alpha1.LocalObjectReference) string {
	var firstBound string
	for _, ref := range refs {
		claim, err := getResource[v1alpha1.MemoryClaim](ctx, d.store, "MemoryClaim", namespace, ref.Name)
		if err != nil {
			d.logger.Warn("failed to resolve agent memory claim",
				zap.String("claim", ref.Name),
				zap.Error(err),
			)
			continue
		}
		if claim == nil || claim.Status.BoundVolumeName == "" {
			continue
		}
		volume, err := getResource[v1alpha1.MemoryVolume](ctx, d.store, "MemoryVolume", namespace, claim.Status.BoundVolumeName)
		if err != nil {
			d.logger.Warn("failed to resolve bound memory volume", zap.String("claim", claim.ObjectMeta.Name), zap.String("volume", claim.Status.BoundVolumeName), zap.Error(err))
			continue
		}
		if volume == nil || volume.Status.Phase != v1alpha1.PhaseBound || volume.Spec.BackendHandle == "" {
			continue
		}
		if claim.Spec.Type == v1alpha1.MemoryTypeShared {
			return volume.Spec.BackendHandle
		}
		if firstBound == "" {
			firstBound = volume.Spec.BackendHandle
		}
	}
	return firstBound
}

// resolveAgentTools fetches each referenced Tool CR from the store and
// flattens its non-secret metadata into a runtime-friendly shape. Endpoint
// disclosure is retained only for direct execution; Tool Gateway runtimes
// exchange capabilities and never need a target endpoint.
func (d *K8sDispatcher) resolveAgentTools(ctx context.Context, namespace string, refs []v1alpha1.AgentToolRef, includeEndpoint bool) []resolvedTool {
	resolved := make([]resolvedTool, 0, len(refs))
	for _, ref := range refs {
		tool, err := getResource[v1alpha1.Tool](ctx, d.store, "Tool", namespace, ref.Ref.Name)
		if err != nil {
			d.logger.Warn("failed to resolve agent tool",
				zap.String("agent", ""),
				zap.String("tool", ref.Ref.Name),
				zap.Error(err),
			)
			continue
		}
		if tool == nil {
			continue
		}
		schema := tool.Spec.Schema
		if schema == nil {
			schema = tool.Spec.InputSchema
		}
		entry := resolvedTool{
			Name:            tool.ObjectMeta.Name,
			ResourceVersion: tool.ObjectMeta.ResourceVersion,
			Description:     tool.Spec.Description,
			Protocol:        string(tool.Spec.Protocol),
			InputSchema:     schema,
			ClassName:       tool.Spec.ClassName,
		}
		if includeEndpoint {
			entry.Endpoint = tool.Spec.Endpoint
		}
		resolved = append(resolved, entry)
	}
	return resolved
}

// resolvedKB is the shape injected into KOGNITION_KB_RESOLVED. It is a
// runtime contract — see runtime/agent/tools.py for the consumer.
type resolvedKB struct {
	Name           string             `json:"name"`
	Namespace      string             `json:"namespace,omitempty"`
	Description    string             `json:"description,omitempty"`
	ClassName      string             `json:"className,omitempty"`
	Sources        []resolvedKBSource `json:"sources,omitempty"`
	VectorStore    map[string]any     `json:"vectorStore,omitempty"`
	Embedding      map[string]any     `json:"embedding,omitempty"`
	Retrieval      map[string]any     `json:"retrieval,omitempty"`
	ProviderBacked bool               `json:"providerBacked,omitempty"`
}

// resolvedKBSource mirrors the inline subset of KBSource that the runtime can
// actually search today. Non-inline source types are emitted with metadata
// only so the runtime can report them honestly without inventing answers.
type resolvedKBSource struct {
	Type string `json:"type"`
	Data string `json:"data,omitempty"`
	URI  string `json:"uri,omitempty"`
}

// resolveAgentKBs fetches each referenced KnowledgeBaseClaim from the store,
// follows the claim to its bound KnowledgeBase, and flattens it into a
// runtime-friendly shape. Inline sources carry their content directly; other
// source types are passed through as metadata. When the KB's class binds a
// vector store (currently: pgvector), the connection info and embedding
// config are inlined so the runtime can query the store without going back
// through the API server.
func (d *K8sDispatcher) resolveAgentKBs(ctx context.Context, namespace string, refs []v1alpha1.KnowledgeBaseClaimRef) ([]resolvedKB, error) {
	resolved := make([]resolvedKB, 0, len(refs))
	for _, ref := range refs {
		claim, err := getResource[v1alpha1.KnowledgeBaseClaim](ctx, d.store, "KnowledgeBaseClaim", namespace, ref.Name)
		if err != nil {
			d.logger.Warn("failed to resolve agent knowledge base claim",
				zap.String("claim", ref.Name),
				zap.Error(err),
			)
			continue
		}
		if claim == nil {
			continue
		}
		if claim.Status.Phase != v1alpha1.PhaseBound || claim.Status.ObservedGeneration != claim.ObjectMeta.Generation || claim.Status.BoundKnowledgeBase == "" {
			return nil, fmt.Errorf("knowledge base claim %s/%s is not bound at its current generation", namespace, ref.Name)
		}
		kbName := claim.Status.BoundKnowledgeBase
		kb, err := getResource[v1alpha1.KnowledgeBase](ctx, d.store, "KnowledgeBase", namespace, kbName)
		if err != nil {
			d.logger.Warn("failed to resolve knowledge base bound to claim",
				zap.String("claim", ref.Name),
				zap.String("kb", kbName),
				zap.Error(err),
			)
			continue
		}
		if kb == nil {
			continue
		}
		sources := make([]resolvedKBSource, 0, len(kb.Spec.Sources))
		for _, s := range kb.Spec.Sources {
			sources = append(sources, resolvedKBSource{
				Type: s.Type,
				Data: s.Data,
				URI:  s.URI,
			})
		}
		record := resolvedKB{
			Name:      kb.ObjectMeta.Name,
			Namespace: kb.ObjectMeta.Namespace,
			ClassName: kb.Spec.KnowledgeBaseClassName,
			Sources:   sources,
		}
		queryMode := ""
		// Enrich with vectorStore + embedding + retrieval when possible.
		// KBClass is cluster-scoped, so its namespace lookup key is "".
		if kb.Spec.KnowledgeBaseClassName != "" {
			kbc, err := getResource[v1alpha1.KnowledgeBaseClass](ctx, d.store, "KnowledgeBaseClass", "", kb.Spec.KnowledgeBaseClassName)
			if err == nil && kbc != nil {
				queryMode = kbc.Spec.IndexMode
				if kbc.Spec.BackendRef != nil {
					if d.config.KnowledgeGatewayURL == "" {
						return nil, fmt.Errorf("knowledge base %s/%s requires the Knowledge Gateway", namespace, kbName)
					}
					record.ProviderBacked = true
					record.Sources = nil
				}
				if !record.ProviderBacked {
					if vs := d.buildKBVectorStoreBlock(ctx, kb, kbc); vs != nil {
						record.VectorStore = vs
						if emb := d.buildKBEmbeddingBlock(ctx, kb, kbc); emb != nil {
							record.Embedding = emb
						}
					}
				}
			} else {
				return nil, fmt.Errorf("knowledge base %s/%s class %q is unavailable", namespace, kbName, kb.Spec.KnowledgeBaseClassName)
			}
		}
		if claim.Spec.Retrieval != nil {
			record.Retrieval = buildKBRetrievalBlock(claim.Spec.Retrieval)
		}
		if record.ProviderBacked {
			if record.Retrieval == nil {
				record.Retrieval = make(map[string]any)
			}
			if record.Retrieval["mode"] == nil {
				mode := queryMode
				if mode == "" {
					mode = "keyword"
				}
				if mode != "keyword" && mode != "dense" && mode != "hybrid" {
					return nil, fmt.Errorf("knowledge retrieval class mode is unsupported")
				}
				record.Retrieval["mode"] = mode
			}
		}
		resolved = append(resolved, record)
	}
	return resolved, nil
}

// buildKBVectorStoreBlock returns the pgvector connection block the runtime
// needs to query the vector store, or nil when the class isn't backed by a
// vector store (or the DSN can't be resolved). Errors are logged and
// swallowed so the runtime falls back to keyword search over inline sources.
func (d *K8sDispatcher) buildKBVectorStoreBlock(ctx context.Context, kb *v1alpha1.KnowledgeBase, kbc *v1alpha1.KnowledgeBaseClass) map[string]any {
	vs := kbc.Spec.VectorStore
	if vs == nil || vs.Type != "pgvector" || vs.Pgvector == nil {
		return nil
	}
	pg := vs.Pgvector
	if pg.ConnectionSecretRef.Name == "" {
		return nil
	}
	dsn, err := d.readSecretRefKey(ctx, kb.ObjectMeta.Namespace, pg.ConnectionSecretRef.Name, "connectionString")
	if err != nil {
		d.logger.Warn("failed to resolve pgvector connection secret",
			zap.String("kb", kb.ObjectMeta.Name),
			zap.String("secretRef", pg.ConnectionSecretRef.Name),
			zap.Error(err),
		)
		return nil
	}
	table := pg.Table
	if table == "" {
		table = "kognition_kb_chunks"
	}
	dim := int(pg.Dimension)
	if dim == 0 {
		dim = 1536
	}
	return map[string]any{
		"provider":  "pgvector",
		"dsn":       dsn,
		"table":     table,
		"dimension": dim,
	}
}

// buildKBEmbeddingBlock resolves the embedding Model referenced by the KB's
// IndexConfig (with class fallback) and returns the runtime embedding block
// (provider, model, baseUrl, apiKey, apiVersion). Returns nil on any error;
// callers should treat that as "vector retrieval unavailable — fall back".
func (d *K8sDispatcher) buildKBEmbeddingBlock(ctx context.Context, kb *v1alpha1.KnowledgeBase, kbc *v1alpha1.KnowledgeBaseClass) map[string]any {
	var modelRef *v1alpha1.LocalObjectReference
	if kb.Spec.IndexConfig != nil && kb.Spec.IndexConfig.EmbeddingModelRef != nil {
		modelRef = kb.Spec.IndexConfig.EmbeddingModelRef
	} else if kbc.Spec.EmbeddingModelRef != nil {
		modelRef = kbc.Spec.EmbeddingModelRef
	}
	if modelRef == nil || modelRef.Name == "" {
		return nil
	}
	model, err := getResource[v1alpha1.Model](ctx, d.store, "Model", kb.ObjectMeta.Namespace, modelRef.Name)
	if err != nil || model == nil {
		if err != nil {
			d.logger.Warn("failed to resolve KB embedding model",
				zap.String("kb", kb.ObjectMeta.Name),
				zap.String("model", modelRef.Name),
				zap.Error(err),
			)
		}
		return nil
	}
	block := map[string]any{
		"model":   model.Spec.ModelID,
		"baseUrl": model.Spec.Endpoint,
	}
	switch model.Spec.Provider {
	case "azure-openai", "foundry":
		block["provider"] = "azure_openai"
		if v := model.Spec.Parameters["apiVersion"]; v != "" {
			block["apiVersion"] = v
		}
	case "openai-compatible", "openai", "":
		block["provider"] = "openai"
	default:
		d.logger.Warn("unsupported KB embedding provider",
			zap.String("kb", kb.ObjectMeta.Name),
			zap.String("provider", model.Spec.Provider),
		)
		return nil
	}
	if model.Spec.CredentialRef != nil && model.Spec.CredentialRef.Name != "" {
		values, err := d.readSecretRefAll(ctx, kb.ObjectMeta.Namespace, model.Spec.CredentialRef.Name)
		if err != nil {
			d.logger.Warn("failed to resolve KB embedding model credentials",
				zap.String("kb", kb.ObjectMeta.Name),
				zap.String("secretRef", model.Spec.CredentialRef.Name),
				zap.Error(err),
			)
			// Continue without credentials — the runtime will surface the auth error.
		} else if key := values["apiKey"]; key != "" {
			block["apiKey"] = key
		} else if values["type"] == "azure-identity" || values["credentialType"] != "" {
			// Azure OpenAI embeddings use the same pod-level AZURE_* credential
			// environment as agent model calls. Leaving apiKey absent makes the
			// runtime build an Azure AD token provider for this embedding request.
		} else {
			d.logger.Warn("KB embedding model credentials do not include apiKey or azure identity metadata",
				zap.String("kb", kb.ObjectMeta.Name),
				zap.String("secretRef", model.Spec.CredentialRef.Name),
			)
		}
	}
	return block
}

// readSecretRefAll resolves a SecretRef CR through the store and returns all
// values inside its internal-secret-data annotation.
func (d *K8sDispatcher) readSecretRefAll(ctx context.Context, namespace, name string) (map[string]string, error) {
	sr, err := getResource[v1alpha1.SecretRef](ctx, d.store, "SecretRef", namespace, name)
	if err != nil {
		return nil, err
	}
	if sr == nil || sr.ObjectMeta.Annotations == nil {
		return nil, fmt.Errorf("secretRef %s/%s has no annotations", namespace, name)
	}
	raw := sr.ObjectMeta.Annotations["kognition.dev/internal-secret-data"]
	if raw == "" {
		return nil, fmt.Errorf("secretRef %s/%s missing internal-secret-data", namespace, name)
	}
	var values map[string]string
	if err := json.Unmarshal([]byte(raw), &values); err != nil {
		return nil, fmt.Errorf("unmarshal secretRef %s/%s: %w", namespace, name, err)
	}
	return values, nil
}

// buildKBRetrievalBlock converts the claim's retrieval overrides into the
// runtime's optional retrieval block.
func buildKBRetrievalBlock(ov *v1alpha1.KBRetrievalOverride) map[string]any {
	out := map[string]any{}
	if ov.Mode != "" {
		out["mode"] = ov.Mode
	}
	if ov.TopK != nil {
		out["topK"] = int(*ov.TopK)
	}
	if ov.MinScore != "" {
		if f, err := strconv.ParseFloat(ov.MinScore, 64); err == nil {
			out["minScore"] = f
		}
	}
	if len(ov.Filter) > 0 {
		out["filter"] = ov.Filter
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// readSecretRefKey resolves a SecretRef CR through the store and returns the
// value of a single key inside its internal-secret-data annotation.
func (d *K8sDispatcher) readSecretRefKey(ctx context.Context, namespace, name, key string) (string, error) {
	values, err := d.readSecretRefAll(ctx, namespace, name)
	if err != nil {
		return "", err
	}
	v, ok := values[key]
	if !ok || v == "" {
		return "", fmt.Errorf("secretRef %s/%s has no %q key", namespace, name, key)
	}
	return v, nil
}

// resolveCredentialEnv looks up the Model's CredentialRef SecretRef and
// returns the provider-specific environment variables (e.g. AZURE_TENANT_ID,
// AZURE_CLIENT_ID, AZURE_CLIENT_SECRET for azure-identity, or KOGNITION_API_KEY
// for api-key). Failures are logged and an empty slice is returned so that the
// dispatch path can still proceed (the runtime will surface the auth error).
func (d *K8sDispatcher) resolveCredentialEnv(ctx context.Context, namespace string, model *v1alpha1.Model) []corev1.EnvVar {
	if model == nil || model.Spec.CredentialRef == nil || model.Spec.CredentialRef.Name == "" {
		return nil
	}
	credName := model.Spec.CredentialRef.Name
	secretRef, err := getResource[v1alpha1.SecretRef](ctx, d.store, "SecretRef", namespace, credName)
	if err != nil {
		d.logger.Warn("failed to resolve SecretRef for model credentials",
			zap.String("secretRef", credName),
			zap.String("namespace", namespace),
			zap.Error(err),
		)
		return nil
	}
	if secretRef == nil || secretRef.ObjectMeta.Annotations == nil {
		return nil
	}
	raw := secretRef.ObjectMeta.Annotations["kognition.dev/internal-secret-data"]
	if raw == "" {
		return nil
	}
	var values map[string]string
	if err := json.Unmarshal([]byte(raw), &values); err != nil {
		d.logger.Warn("failed to parse internal-secret-data annotation",
			zap.String("secretRef", credName),
			zap.Error(err),
		)
		return nil
	}
	credType := values["type"]
	switch credType {
	case "azure-identity":
		// Propagate the selected credential mode to the runtime, which builds
		// the matching azure-identity credential (see runtime/agent/azure_creds.py
		// and internal/cpi/drivers/foundry/token_provider.go). Secretless modes
		// (managed-identity, workload-identity, default) require no client
		// secret. Workload identity additionally needs the pod's ServiceAccount
		// annotated for the Azure Workload Identity webhook so it projects
		// AZURE_FEDERATED_TOKEN_FILE.
		credentialType := values["credentialType"]
		if credentialType == "" {
			credentialType = "client-secret" // backward-compatible default
		}
		switch credentialType {
		case "client-secret":
			return []corev1.EnvVar{
				{Name: "AZURE_CREDENTIAL_TYPE", Value: "client-secret"},
				{Name: "AZURE_TENANT_ID", Value: values["tenantId"]},
				{Name: "AZURE_CLIENT_ID", Value: values["clientId"]},
				{Name: "AZURE_CLIENT_SECRET", Value: values["clientSecret"]},
			}
		case "workload-identity":
			return []corev1.EnvVar{
				{Name: "AZURE_CREDENTIAL_TYPE", Value: "workload-identity"},
				{Name: "AZURE_TENANT_ID", Value: values["tenantId"]},
				{Name: "AZURE_CLIENT_ID", Value: values["clientId"]},
			}
		case "managed-identity":
			env := []corev1.EnvVar{{Name: "AZURE_CREDENTIAL_TYPE", Value: "managed-identity"}}
			if v := values["clientId"]; v != "" {
				env = append(env, corev1.EnvVar{Name: "AZURE_CLIENT_ID", Value: v})
			}
			return env
		case "default":
			env := []corev1.EnvVar{{Name: "AZURE_CREDENTIAL_TYPE", Value: "default"}}
			if v := values["tenantId"]; v != "" {
				env = append(env, corev1.EnvVar{Name: "AZURE_TENANT_ID", Value: v})
			}
			if v := values["clientId"]; v != "" {
				env = append(env, corev1.EnvVar{Name: "AZURE_CLIENT_ID", Value: v})
			}
			return env
		default:
			d.logger.Warn("unsupported azure-identity credentialType",
				zap.String("secretRef", credName),
				zap.String("credentialType", credentialType),
			)
			return nil
		}
	case "api-key":
		// Prefer the explicit "apiKey" field; fall back to any single non-type field.
		if v := values["apiKey"]; v != "" {
			return []corev1.EnvVar{{Name: "KOGNITION_API_KEY", Value: v}}
		}
		for k, v := range values {
			if k == "type" {
				continue
			}
			return []corev1.EnvVar{{Name: "KOGNITION_API_KEY", Value: v}}
		}
		return nil
	default:
		d.logger.Warn("unsupported credential type for runtime env injection",
			zap.String("secretRef", credName),
			zap.String("type", credType),
		)
		return nil
	}
}

func deploymentName(agentName string) string {
	return "agent-" + agentName
}

func serviceName(agentName string) string {
	return "agent-" + agentName
}

func (d *K8sDispatcher) workloadKey(agentNamespace, agentName string) string {
	if d.config.AgentNamespace == "" {
		return agentName
	}
	digest := sha256.Sum256([]byte(agentNamespace + "\x00" + agentName))
	suffix := fmt.Sprintf("-%x", digest[:4])
	prefix := agentNamespace + "-" + agentName
	const maxKeyLength = 57 // leaves room for the "agent-" resource-name prefix
	if len(prefix)+len(suffix) > maxKeyLength {
		prefix = prefix[:maxKeyLength-len(suffix)]
	}
	return prefix + suffix
}

func (d *K8sDispatcher) verifySharedResourceOwner(annotations map[string]string, agent *v1alpha1.Agent, kind, name string) error {
	if d.config.AgentNamespace == "" {
		return nil
	}
	want := agent.ObjectMeta.Namespace + "/" + agent.ObjectMeta.Name
	if got := annotations[AnnotationOwnerAgent]; got != want {
		return fmt.Errorf("refusing to modify shared runtime %s %q owned by %q; expected %q", kind, name, got, want)
	}
	return nil
}

func (d *K8sDispatcher) removeOwnedLegacyResources(ctx context.Context, namespace string, agent *v1alpha1.Agent) error {
	if d.config.AgentNamespace == "" {
		return nil
	}
	ownerRef := agent.ObjectMeta.Namespace + "/" + agent.ObjectMeta.Name
	legacyName := deploymentName(agent.ObjectMeta.Name)
	var deployment appsv1.Deployment
	if err := d.client.Get(ctx, client.ObjectKey{Namespace: namespace, Name: legacyName}, &deployment); err == nil {
		if deployment.Annotations[AnnotationOwnerAgent] == ownerRef {
			if err := d.client.Delete(ctx, &deployment, client.Preconditions{UID: &deployment.UID}); err != nil && !errors.IsNotFound(err) {
				return fmt.Errorf("delete legacy deployment %s/%s: %w", namespace, legacyName, err)
			}
		}
	} else if !errors.IsNotFound(err) {
		return fmt.Errorf("get legacy deployment %s/%s: %w", namespace, legacyName, err)
	}

	var service corev1.Service
	if err := d.client.Get(ctx, client.ObjectKey{Namespace: namespace, Name: legacyName}, &service); err == nil {
		if service.Annotations[AnnotationOwnerAgent] == ownerRef {
			if err := d.client.Delete(ctx, &service, client.Preconditions{UID: &service.UID}); err != nil && !errors.IsNotFound(err) {
				return fmt.Errorf("delete legacy service %s/%s: %w", namespace, legacyName, err)
			}
		}
	} else if !errors.IsNotFound(err) {
		return fmt.Errorf("get legacy service %s/%s: %w", namespace, legacyName, err)
	}
	return nil
}

func getResource[T any](ctx context.Context, s store.Store, kind, namespace, name string) (*T, error) {
	raw, err := s.Get(ctx, kind, namespace, name)
	if err != nil {
		return nil, err
	}
	var obj *T
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, fmt.Errorf("unmarshal %s %s/%s: %w", kind, namespace, name, err)
	}
	if obj == nil {
		return nil, fmt.Errorf("unmarshal %s %s/%s: resource is null", kind, namespace, name)
	}
	return obj, nil
}
