// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package dispatcher

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	intctrl "github.com/kognition-ai/kognition/internal/controller"
	"github.com/kognition-ai/kognition/internal/observability"
	platformruntime "github.com/kognition-ai/kognition/internal/runtime"
	"github.com/kognition-ai/kognition/internal/runtime/autogen"
	"github.com/kognition-ai/kognition/internal/runtime/recovery"
	"github.com/kognition-ai/kognition/internal/store"
)

func NewExternalDispatcher(storage store.Store, logger *zap.Logger, config Config, endpoints map[string]string) (*K8sDispatcher, error) {
	if storage == nil || strings.TrimSpace(config.RuntimeAuthToken) == "" || len(endpoints) == 0 {
		return nil, fmt.Errorf("external runtime requires a store, runtime authentication token, and Agent endpoints")
	}
	if config.ModelGatewayURL == "" || config.ModelSessionCapabilityIssuer == nil {
		return nil, fmt.Errorf("external runtime requires Model Gateway and a session capability issuer")
	}
	if config.WorkloadIdentityMode != "" && config.WorkloadIdentityMode != "capability" && config.WorkloadIdentityMode != "spire" {
		return nil, fmt.Errorf("invalid external runtime workload identity profile")
	}
	if config.WorkloadIdentityMode == "spire" {
		proxy, err := url.Parse(config.WorkloadProxyURL)
		if err != nil || proxy == nil || proxy.Scheme != "http" || proxy.User != nil || proxy.RawQuery != "" || proxy.Fragment != "" || (proxy.Path != "" && proxy.Path != "/") {
			return nil, fmt.Errorf("workload proxy requires a literal loopback origin")
		}
		address := net.ParseIP(proxy.Hostname())
		if address == nil || !address.IsLoopback() {
			return nil, fmt.Errorf("workload proxy requires a literal loopback origin")
		}
	}
	validated := make(map[string]string, len(endpoints))
	seen := make(map[string]bool, len(endpoints))
	for agent, endpoint := range endpoints {
		parts := strings.Split(agent, "/")
		if len(parts) != 2 || strings.TrimSpace(parts[0]) == "" || strings.TrimSpace(parts[1]) == "" {
			return nil, fmt.Errorf("external runtime mapping requires namespace/Agent keys")
		}
		parsed, err := url.Parse(endpoint)
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Hostname() == "" || parsed.User != nil || parsed.Path != "/invoke" || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.RawPath != "" {
			return nil, fmt.Errorf("external runtime for Agent %s requires an HTTPS /invoke endpoint without credentials, query, or fragment", agent)
		}
		if parsed.Scheme == "http" {
			address := net.ParseIP(parsed.Hostname())
			if address == nil || !address.IsLoopback() {
				return nil, fmt.Errorf("external runtime for Agent %s requires HTTPS except for literal loopback addresses", agent)
			}
		}
		if seen[parsed.String()] {
			return nil, fmt.Errorf("each Agent requires a dedicated external runtime endpoint")
		}
		seen[parsed.String()] = true
		validated[agent] = parsed.String()
	}
	dispatcher := NewK8sDispatcher(nil, storage, logger, config)
	dispatcher.externalEndpoints = validated
	dispatcher.http.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	dispatcher.invokeHTTP.CheckRedirect = dispatcher.http.CheckRedirect
	return dispatcher, nil
}

func (d *K8sDispatcher) externalConfiguration(ctx context.Context, endpoint, namespace string, agent *v1alpha1.Agent, model *v1alpha1.Model) (string, error) {
	if len(agent.Spec.KnowledgeBaseClaims) != 0 {
		knowledgeBases, err := d.resolveAgentKBs(ctx, namespace, agent.Spec.KnowledgeBaseClaims)
		if err != nil {
			return "", err
		}
		for _, knowledgeBase := range knowledgeBases {
			if !knowledgeBase.ProviderBacked {
				return "", fmt.Errorf("external runtime does not support knowledge base claims with direct retrieval credentials")
			}
		}
	}
	if agent.Spec.Runtime != nil {
		class := strings.TrimSpace(agent.Spec.Runtime.Class)
		if class != "" && class != "default" && class != "native" && class != "autogen" {
			return "", fmt.Errorf("external runtime supports only native or autogen runtime classes")
		}
	}
	configuration := map[string]string{"KOGNITION_NAMESPACE": namespace}
	environment, err := d.buildGatewayModelEnvVars(ctx, namespace, agent, model)
	if err != nil {
		return "", err
	}
	for _, variable := range environment {
		if variable.ValueFrom == nil && !strings.HasPrefix(variable.Name, "KOGNITION_OTEL_") && !strings.HasPrefix(variable.Name, "KOGNITION_CONTROL_TLS_") && variable.Name != "KOGNITION_EXTERNAL_RUNTIME" && variable.Name != "KOGNITION_RUNTIME_AUTH_TOKEN" {
			configuration[variable.Name] = variable.Value
		}
	}
	body, err := json.Marshal(configuration)
	if err != nil {
		return "", err
	}
	if endpoint == "" {
		return string(body), nil
	}
	checkURL, _ := url.Parse(endpoint)
	checkURL.Path = "/configuration-check"
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, checkURL.String(), bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("build external configuration check: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+d.config.RuntimeAuthToken)
	request.Header.Set("Content-Type", "application/json")
	controlClient, err := d.runtimeControlClient()
	if err != nil {
		return "", err
	}
	response, err := controlClient.Do(request)
	if err != nil {
		return "", fmt.Errorf("external runtime configuration check failed")
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusNoContent {
		return "", fmt.Errorf("external runtime configuration check returned %d", response.StatusCode)
	}
	return string(body), nil
}

func (d *K8sDispatcher) invokeExternal(instance *v1alpha1.AgentInstance, endpoint, sessionID, configuration, memory, tool, model, knowledge, delegation string) {
	ctx, cancel := context.WithTimeout(context.Background(), invokeBusyTimeout)
	control := &invocationControl{cancel: cancel, done: make(chan struct{}), authoritativelyBusy: true}
	d.invocationsMu.Lock()
	if d.invocations[sessionID] != nil {
		d.invocationsMu.Unlock()
		cancel()
		return
	}
	d.invocations[sessionID] = control
	d.invocationsMu.Unlock()
	body, _ := json.Marshal(map[string]string{
		"message": instance.Spec.SessionContext["input"], "session_id": sessionID,
		"conversation_id":       instance.Spec.SessionContext["conversation_id"],
		"spiffe_id":             instance.Spec.SessionContext[intctrl.AgentIdentityContextKey],
		"runtime_configuration": configuration, "memory_capability": memory,
		"tool_session_capability": tool, "model_session_capability": model,
		"knowledge_session_capability":        knowledge,
		"agent_delegation_session_capability": delegation,
	})
	namespace, instanceName := instance.ObjectMeta.Namespace, instance.ObjectMeta.Name
	traceparent := instance.ObjectMeta.Annotations[observability.AnnotationTraceparent]
	go func() {
		defer func() {
			cancel()
			d.invocationsMu.Lock()
			if d.invocations[sessionID] == control {
				delete(d.invocations, sessionID)
			}
			close(control.done)
			d.invocationsMu.Unlock()
		}()
		if !d.waitForSessionActivationContext(ctx, namespace, instanceName, sessionID) {
			return
		}
		ticker := time.NewTicker(invokePollInterval)
		defer ticker.Stop()
		for ctx.Err() == nil {
			request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
			if err != nil {
				return
			}
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("Authorization", "Bearer "+d.config.RuntimeAuthToken)
			if traceparent != "" {
				request.Header.Set("traceparent", traceparent)
			}
			control.mu.Lock()
			control.authoritativelyBusy = false
			control.mu.Unlock()
			response, err := d.invokeHTTP.Do(request)
			if err != nil {
				return
			}
			_ = response.Body.Close()
			control.mu.Lock()
			control.accepted = response.StatusCode == http.StatusAccepted
			control.authoritativelyBusy = response.StatusCode == http.StatusConflict
			control.mu.Unlock()
			if response.StatusCode != http.StatusConflict {
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
}

func (d *K8sDispatcher) externalStatus(ctx context.Context, instance *v1alpha1.AgentInstance) (intctrl.RuntimeStatus, error) {
	endpoint, _, err := d.agentServiceEndpoint(ctx, instance)
	if err != nil {
		return intctrl.RuntimeStatus{}, fmt.Errorf("%w: external runtime route unavailable", intctrl.ErrDispatchAccountingUncertain)
	}
	d.invocationsMu.Lock()
	pending := d.invocations[instance.Status.SessionID] != nil
	d.invocationsMu.Unlock()
	if pending {
		return intctrl.RuntimeStatus{Alive: true}, nil
	}
	sessionID := instance.Status.SessionID
	if instance.Spec.SessionContext["input"] == "" {
		sessionID = ""
	}
	statusURL, err := serviceStatusURL(endpoint, sessionID)
	if err != nil {
		return intctrl.RuntimeStatus{}, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, statusURL, nil)
	if err != nil {
		return intctrl.RuntimeStatus{}, err
	}
	request.Header.Set("Authorization", "Bearer "+d.config.RuntimeAuthToken)
	response, err := d.http.Do(request)
	if err != nil {
		return intctrl.RuntimeStatus{}, fmt.Errorf("%w: external runtime status unavailable", intctrl.ErrDispatchAccountingUncertain)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return intctrl.RuntimeStatus{}, fmt.Errorf("%w: external runtime status returned %d", intctrl.ErrDispatchAccountingUncertain, response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxRuntimeResponseBodyBytes+1))
	var status struct {
		agentStatusResponse
		SessionID string `json:"session_id"`
		AgentName string `json:"agent_name"`
	}
	if err != nil || len(body) > maxRuntimeResponseBodyBytes || json.Unmarshal(body, &status) != nil || status.AgentName != instance.Spec.AgentRef.Name || (sessionID != "" && status.SessionID != sessionID) {
		return intctrl.RuntimeStatus{}, fmt.Errorf("%w: invalid external runtime status binding", intctrl.ErrDispatchAccountingUncertain)
	}
	if sessionID == "" {
		return intctrl.RuntimeStatus{Alive: status.Phase == "Ready" || status.Phase == "Running" || status.Phase == "Completed"}, nil
	}
	if status.Phase != "Running" && status.Phase != "Completed" && status.Phase != "Failed" {
		return intctrl.RuntimeStatus{}, fmt.Errorf("%w: external runtime has no active session", intctrl.ErrDispatchAccountingUncertain)
	}
	result := intctrl.RuntimeStatus{Alive: status.Phase != "Failed", Completed: status.Phase == "Completed", Output: status.Output, Error: status.Error, SPIFFEID: status.SPIFFEID, ToolCalls: status.ToolCalls, ToolCallsDetail: toolCallDetailsFromRuntime(status.ToolCallsDetail)}
	if status.TokenUsage != nil {
		result.TokenUsage = &v1alpha1.TokenUsage{PromptTokens: status.TokenUsage.PromptTokens, CompletionTokens: status.TokenUsage.CompletionTokens, TotalTokens: status.TokenUsage.TotalTokens}
	}
	return result, nil
}

type autoGenSession struct {
	mu                 sync.Mutex
	executionErr       error
	namespace          string
	agentName          string
	instanceUID        string
	configuration      string
	client             *autogen.HTTPAgentClient
	runtime            *autogen.AutoGenRuntime
	control            *invocationControl
	ready              chan struct{}
	initErr            error
	recoveryRecord     store.RuntimeRecoveryRecord
	recoveryBinding    recovery.Binding
	terminalStatus     *autogen.HTTPAgentStatus
	preparedCheckpoint []byte
}

func (d *K8sDispatcher) dispatchAutoGen(ctx context.Context, instance *v1alpha1.AgentInstance, agent *v1alpha1.Agent, model *v1alpha1.Model, endpoint, sessionID, configuration, solution string, solutionRevision int64) (string, error) {
	if instance.ObjectMeta.UID == "" {
		return "", fmt.Errorf("AutoGen dispatch requires an immutable AgentInstance UID")
	}
	if d.recoveryStore == nil || d.recoveryCipher == nil {
		return "", recovery.ErrUnavailable
	}
	if record, err := d.recoveryStore.GetRuntimeRecovery(ctx, instance.ObjectMeta.Namespace, sessionID); err == nil && record.Phase == store.RuntimeRecoveryTerminal {
		terminalInstance := *instance
		terminalInstance.Status.SessionID = sessionID
		if terminal, err := d.recoverAutoGenTerminal(ctx, &terminalInstance, agent); err == nil && terminal.Completed {
			return sessionID, nil
		}
		return "", intctrl.ErrDispatchAccountingUncertain
	}
	session := &autoGenSession{namespace: instance.ObjectMeta.Namespace, agentName: agent.ObjectMeta.Name, instanceUID: instance.ObjectMeta.UID, configuration: configuration, ready: make(chan struct{})}
	actual, loaded := d.autogenSessions.LoadOrStore(sessionID, session)
	if loaded {
		owned, valid := actual.(*autoGenSession)
		if !valid {
			return "", fmt.Errorf("invalid AutoGen session ownership")
		}
		session = owned
		select {
		case <-session.ready:
		case <-ctx.Done():
			return "", ctx.Err()
		}
		if session.namespace != instance.ObjectMeta.Namespace || session.agentName != instance.Spec.AgentRef.Name || session.instanceUID != instance.ObjectMeta.UID || session.configuration != configuration {
			return "", fmt.Errorf("AutoGen session ownership conflict")
		}
		return sessionID, session.initErr
	}
	defer func() {
		close(session.ready)
		if session.initErr != nil {
			d.autogenSessions.CompareAndDelete(sessionID, session)
		}
	}()
	origin, _ := url.Parse(endpoint)
	origin.Path = ""
	controlClient, err := d.runtimeControlClient()
	if err != nil {
		session.initErr = err
		return "", err
	}
	client, err := autogen.NewHTTPAgentClient(autogen.HTTPClientConfig{
		Endpoint: origin.String(), AgentName: agent.ObjectMeta.Name, Namespace: instance.ObjectMeta.Namespace, RuntimeConfiguration: configuration,
		AuthToken: d.config.RuntimeAuthToken, SessionID: sessionID, InstanceUID: instance.ObjectMeta.UID, Client: controlClient,
		Capabilities: func(capabilityContext context.Context, _ string) (map[string]string, error) {
			values := map[string]string{}
			var err error
			values["model_session_capability"], err = d.issueModelSessionCapability(capabilityContext, agent, model, instance, sessionID, solution, solutionRevision)
			if err != nil {
				return nil, err
			}
			values["memory_capability"], err = d.issueMemoryCapability(capabilityContext, instance.ObjectMeta.Namespace, agent, instance, sessionID)
			if err != nil {
				return nil, err
			}
			values["tool_session_capability"], err = d.issueToolSessionCapability(agent, instance, sessionID, solution, solutionRevision)
			if err != nil {
				return nil, err
			}
			values["agent_delegation_session_capability"], err = d.issueAgentDelegationSessionCapability(agent, instance, sessionID, solution, solutionRevision)
			if err != nil {
				return nil, err
			}
			values["knowledge_session_capability"], err = d.issueKnowledgeSessionCapability(capabilityContext, instance.ObjectMeta.Namespace, agent, instance, sessionID)
			return values, err
		},
	})
	if err != nil {
		session.initErr = err
		return "", err
	}
	session.client, session.runtime = client, autogen.New(client)
	created, err := session.runtime.CreateSession(ctx, platformruntime.CreateSessionRequest{AgentName: agent.ObjectMeta.Name, Namespace: instance.ObjectMeta.Namespace, InitialCredits: 16})
	if err != nil || created.SessionID != sessionID {
		session.initErr = fmt.Errorf("AutoGen contract session creation failed")
		return "", session.initErr
	}
	if err := d.prepareAutoGenRecovery(ctx, session, recoveryBinding(instance, agent, sessionID, configuration)); err != nil {
		session.initErr = err
		return "", err
	}
	executionContext, cancel := context.WithTimeout(context.Background(), invokeBusyTimeout)
	traceHeaders := http.Header{}
	traceHeaders.Set("traceparent", instance.ObjectMeta.Annotations[observability.AnnotationTraceparent])
	executionContext = observability.ContextWithTraceHeaders(executionContext, traceHeaders)
	session.control = &invocationControl{cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(session.control.done)
		defer cancel()
		fail := func(message string) {
			session.mu.Lock()
			session.executionErr = fmt.Errorf("%s", message)
			session.mu.Unlock()
		}
		if !d.waitForSessionActivationContext(executionContext, session.namespace, instance.ObjectMeta.Name, sessionID) {
			fail("AutoGen session activation did not complete")
			return
		}
		status, err := client.Status(executionContext, sessionID)
		if err != nil {
			fail("AutoGen worker state unavailable")
			return
		}
		if status.Phase != "Ready" {
			return
		}
		if session.terminalStatus != nil {
			return
		}
		if err := d.claimAutoGenRecovery(executionContext, session); err != nil {
			fail("AutoGen durable execution claim unavailable")
			return
		}
		if err := d.restoreAutoGenPrepared(executionContext, session); err != nil {
			fail("AutoGen prepared checkpoint restoration unavailable")
			return
		}
		stream, err := session.runtime.Step(executionContext, platformruntime.StepRequest{SessionID: sessionID, Input: instance.Spec.SessionContext["input"]})
		if err != nil {
			fail("AutoGen contract turn could not start")
			return
		}
		defer stream.Close()
		for event := range stream.Events {
			if event.Error != nil {
				fail("AutoGen contract execution requires controller recovery")
			}
			stream.GrantCredits(1)
		}
		accountingContext, stop := context.WithTimeout(context.WithoutCancel(executionContext), 20*time.Second)
		defer stop()
		if err := d.completeAutoGenRecovery(accountingContext, session); err != nil {
			fail("AutoGen durable terminal checkpoint unavailable")
		}
	}()
	return sessionID, nil
}

func (d *K8sDispatcher) autoGenStatus(ctx context.Context, instance *v1alpha1.AgentInstance, session *autoGenSession) (intctrl.RuntimeStatus, error) {
	select {
	case <-session.ready:
	case <-ctx.Done():
		return intctrl.RuntimeStatus{}, ctx.Err()
	}
	if session.initErr != nil || session.namespace != instance.ObjectMeta.Namespace || session.agentName != instance.Spec.AgentRef.Name || session.instanceUID != instance.ObjectMeta.UID {
		return intctrl.RuntimeStatus{}, fmt.Errorf("%w: AutoGen session ownership unavailable", intctrl.ErrDispatchAccountingUncertain)
	}
	session.mu.Lock()
	terminal := session.terminalStatus
	session.mu.Unlock()
	var status autogen.HTTPAgentStatus
	var err error
	if terminal != nil {
		status = *terminal
	} else {
		status, err = session.client.Status(ctx, instance.Status.SessionID)
	}
	if err != nil {
		return intctrl.RuntimeStatus{}, fmt.Errorf("%w: AutoGen status unavailable", intctrl.ErrDispatchAccountingUncertain)
	}
	if status.Phase == "Completed" && terminal == nil {
		return intctrl.RuntimeStatus{Alive: true, TokenUsage: tokenUsageFromAutoGen(status)}, nil
	}
	session.mu.Lock()
	executionErr := session.executionErr
	session.mu.Unlock()
	if executionErr != nil && terminal == nil {
		return intctrl.RuntimeStatus{}, fmt.Errorf("%w: %w", intctrl.ErrDispatchAccountingUncertain, executionErr)
	}
	result := intctrl.RuntimeStatus{Alive: status.Phase != "Failed", Completed: status.Phase == "Completed", Output: status.Output, Error: status.Error, ToolCalls: status.ToolCalls}
	if status.RuntimeApprovalState != "" {
		result.Alive, result.Completed, result.Output, result.Error = false, false, "", "AutoGen execution requires controller approval recovery"
	}
	if status.TokenUsage != nil {
		result.TokenUsage = &v1alpha1.TokenUsage{PromptTokens: status.TokenUsage.PromptTokens, CompletionTokens: status.TokenUsage.CompletionTokens, TotalTokens: status.TokenUsage.TotalTokens}
	}
	var details []map[string]any
	encoded, err := json.Marshal(status.ToolCallsDetail)
	if err != nil || json.Unmarshal(encoded, &details) != nil {
		return intctrl.RuntimeStatus{}, fmt.Errorf("%w: invalid AutoGen tool details", intctrl.ErrDispatchAccountingUncertain)
	}
	result.ToolCallsDetail = toolCallDetailsFromRuntime(details)
	return result, nil
}

func (d *K8sDispatcher) terminateAutoGen(ctx context.Context, instance *v1alpha1.AgentInstance, session *autoGenSession) error {
	if session.namespace != instance.ObjectMeta.Namespace || session.agentName != instance.Spec.AgentRef.Name || session.instanceUID != instance.ObjectMeta.UID {
		return fmt.Errorf("AutoGen cancellation ownership conflict")
	}
	select {
	case <-session.ready:
	case <-ctx.Done():
		return ctx.Err()
	}
	if session.initErr != nil {
		return session.initErr
	}
	session.mu.Lock()
	terminal := session.terminalStatus
	session.mu.Unlock()
	if terminal != nil {
		return nil
	}
	session.control.cancel()
	response, err := session.runtime.Cancel(ctx, platformruntime.CancelRequest{SessionID: instance.Status.SessionID, TimeoutMS: 1000})
	if err != nil || response.State != platformruntime.CancelAcknowledged {
		return fmt.Errorf("AutoGen cancellation was not acknowledged")
	}
	select {
	case <-session.control.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (d *K8sDispatcher) runtimeControlClient() (*http.Client, error) {
	if d.config.RuntimeCAFile == "" {
		return d.http, nil
	}
	file, err := os.Open(d.config.RuntimeCAFile)
	if err != nil {
		return nil, fmt.Errorf("AutoGen control TLS trust is unavailable")
	}
	defer func() { _ = file.Close() }()
	trust, err := io.ReadAll(io.LimitReader(file, (256<<10)+1))
	roots := x509.NewCertPool()
	if err != nil || len(trust) > 256<<10 || !roots.AppendCertsFromPEM(trust) {
		return nil, fmt.Errorf("AutoGen control TLS trust is unavailable")
	}
	base, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return nil, fmt.Errorf("runtime HTTP transport unavailable")
	}
	transport := base.Clone()
	transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: roots}
	return &http.Client{Transport: transport, Timeout: statusPollTimeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, nil
}

func (d *K8sDispatcher) unownedAutoGenStatus(ctx context.Context, instance *v1alpha1.AgentInstance) (bool, intctrl.RuntimeStatus, error) {
	if d.store == nil {
		return false, intctrl.RuntimeStatus{}, nil
	}
	agent, err := intctrl.ResolveExecutionAgent(ctx, d.store, instance.ObjectMeta.Namespace, instance)
	if err != nil || agent.Spec.Runtime == nil || strings.TrimSpace(agent.Spec.Runtime.Class) != "autogen" {
		return false, intctrl.RuntimeStatus{}, nil
	}
	if instance.Spec.SessionContext["input"] != "" {
		status, err := d.recoverAutoGenTerminal(ctx, instance, agent)
		return true, status, err
	}
	endpoint, routed, err := d.agentServiceEndpoint(ctx, instance)
	if err != nil {
		return true, intctrl.RuntimeStatus{}, err
	}
	if !routed {
		namespace := instance.ObjectMeta.Namespace
		if d.config.AgentNamespace != "" {
			namespace = d.config.AgentNamespace
		}
		endpoint = fmt.Sprintf("https://%s.%s.svc.cluster.local:%d/invoke", serviceName(d.workloadKey(instance.ObjectMeta.Namespace, instance.Spec.AgentRef.Name)), namespace, agentPort)
	}
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return true, intctrl.RuntimeStatus{}, err
	}
	parsed.Path = "/readyz"
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		return true, intctrl.RuntimeStatus{}, err
	}
	request.Header.Set("Authorization", "Bearer "+d.config.RuntimeAuthToken)
	client, err := d.runtimeControlClient()
	if err != nil {
		return true, intctrl.RuntimeStatus{}, err
	}
	response, err := client.Do(request)
	if err != nil {
		return true, intctrl.RuntimeStatus{}, err
	}
	defer func() { _ = response.Body.Close() }()
	return true, intctrl.RuntimeStatus{Alive: response.StatusCode == http.StatusOK}, nil
}

func tokenUsageFromAutoGen(status autogen.HTTPAgentStatus) *v1alpha1.TokenUsage {
	if status.TokenUsage == nil {
		return nil
	}
	return &v1alpha1.TokenUsage{PromptTokens: status.TokenUsage.PromptTokens, CompletionTokens: status.TokenUsage.CompletionTokens, TotalTokens: status.TokenUsage.TotalTokens}
}
