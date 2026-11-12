// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/kognition-ai/kognition/internal/controller"
	"github.com/kognition-ai/kognition/internal/k8s/dispatcher"
	"github.com/kognition-ai/kognition/internal/runtimeauth"
	"github.com/kognition-ai/kognition/internal/store"
)

func standaloneRuntimeDispatcher(storage store.Store, logger *zap.Logger, compatibility controller.RuntimeDispatcher) (controller.RuntimeDispatcher, error) {
	switch mode := strings.TrimSpace(os.Getenv("KOGNITION_RUNTIME_DISPATCH_MODE")); mode {
	case "", "cpi":
		return compatibility, nil
	case "http":
	default:
		return nil, fmt.Errorf("KOGNITION_RUNTIME_DISPATCH_MODE must be cpi or http")
	}
	var endpoints map[string]string
	if err := json.Unmarshal([]byte(os.Getenv("KOGNITION_RUNTIME_ENDPOINTS")), &endpoints); err != nil || len(endpoints) == 0 {
		return nil, fmt.Errorf("KOGNITION_RUNTIME_ENDPOINTS must be a nonempty JSON namespace/Agent to /invoke endpoint map")
	}
	config := dispatcher.Config{
		RuntimeAuthToken:             os.Getenv("KOGNITION_RUNTIME_AUTH_TOKEN"),
		RuntimeRecoveryKeysFile:      os.Getenv("KOGNITION_RUNTIME_RECOVERY_KEYS_FILE"),
		WorkloadIdentityMode:         os.Getenv("KOGNITION_WORKLOAD_IDENTITY_MODE"),
		WorkloadProxyURL:             os.Getenv("KOGNITION_WORKLOAD_PROXY_URL"),
		MemoryPolicyGatewayURL:       os.Getenv("KOGNITION_MEMORY_POLICY_GATEWAY_URL"),
		ToolGatewayURL:               os.Getenv("KOGNITION_TOOL_GATEWAY_URL"),
		ModelGatewayURL:              os.Getenv("KOGNITION_MODEL_GATEWAY_URL"),
		AgentCommunicationGatewayURL: os.Getenv("KOGNITION_AGENT_COMMUNICATION_GATEWAY_URL"),
	}
	if config.MemoryPolicyGatewayURL == "" || config.ToolGatewayURL == "" || config.ModelGatewayURL == "" || config.AgentCommunicationGatewayURL == "" {
		return nil, fmt.Errorf("http runtime dispatch requires all four gateway URLs")
	}
	var err error
	config.MemoryCapabilityIssuer, err = standaloneCapabilityIssuer("MEMORY", runtimeauth.NewMemoryCapabilityIssuerWithKeyRing)
	if err != nil {
		return nil, err
	}
	config.ToolSessionCapabilityIssuer, err = standaloneCapabilityIssuer("TOOL", runtimeauth.NewToolSessionCapabilityIssuerWithKeyRing)
	if err != nil {
		return nil, err
	}
	config.ModelSessionCapabilityIssuer, err = standaloneCapabilityIssuer("MODEL", runtimeauth.NewModelSessionCapabilityIssuerWithKeyRing)
	if err != nil {
		return nil, err
	}
	config.AgentDelegationSessionCapabilityIssuer, err = standaloneCapabilityIssuer("AGENT_DELEGATION", runtimeauth.NewAgentDelegationSessionCapabilityIssuerWithKeyRing)
	if err != nil {
		return nil, err
	}
	return dispatcher.NewExternalDispatcher(storage, logger.Named("http-runtime-dispatcher"), config, endpoints)
}

func standaloneCapabilityIssuer[Issuer any](plane string, create func(*runtimeauth.CapabilityKeyRing, time.Duration) (Issuer, error)) (Issuer, error) {
	prefix := "KOGNITION_" + plane + "_CAPABILITY_HMAC_"
	keys, err := runtimeauth.NewConfiguredCapabilityKeyRing(os.Getenv(prefix+"KEY_ID"), []byte(os.Getenv(prefix+"KEY")), os.Getenv(prefix+"PREVIOUS_KEY_ID"), []byte(os.Getenv(prefix+"PREVIOUS_KEY")))
	if err != nil {
		var empty Issuer
		return empty, fmt.Errorf("configure %s capability keys: %w", plane, err)
	}
	return create(keys, 5*time.Minute)
}
