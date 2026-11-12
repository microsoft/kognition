// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package admission

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/kognition-ai/kognition/api/v1alpha1"
)

const (
	automaticAgentIdentityNamePrefix = "agent-identity-"
	automaticAgentIdentityLabel      = "kognition.dev/auto-created-for-agent"
)

// AgentIdentityValidator reserves controller-managed identity provenance.
// Manual identities may be declared, but cannot claim controller ownership,
// readiness, or an automatic identity name.
type AgentIdentityValidator struct {
	controllerUsername string
}

func NewAgentIdentityValidator(controllerUsername string) *AgentIdentityValidator {
	return &AgentIdentityValidator{controllerUsername: controllerUsername}
}

func (*AgentIdentityValidator) Name() string { return "agent-identity-validator" }
func (*AgentIdentityValidator) Handles(kind string, operation Operation) bool {
	return kind == "AgentIdentity" && (operation == OperationCreate || operation == OperationUpdate || operation == OperationDelete)
}

func (v *AgentIdentityValidator) Validate(_ context.Context, req *Request) *Response {
	var identity v1alpha1.AgentIdentity
	object := req.Object
	if req.Operation == OperationDelete {
		object = req.OldObject
	}
	if err := json.Unmarshal(object, &identity); err != nil {
		return denyAgentIdentity(fmt.Sprintf("invalid AgentIdentity: %v", err))
	}
	if v.isController(req.UserInfo) {
		return &Response{Allowed: true}
	}
	if req.Operation == OperationDelete {
		if isAutomaticAgentIdentity(&identity) {
			return denyAgentIdentity("automatic AgentIdentity deletion is reserved for the controller")
		}
		return &Response{Allowed: true}
	}
	if strings.HasPrefix(identity.ObjectMeta.Name, automaticAgentIdentityNamePrefix) {
		return denyAgentIdentity("automatic AgentIdentity names are reserved for the controller")
	}
	if identity.ObjectMeta.Labels[automaticAgentIdentityLabel] != "" {
		return denyAgentIdentity("automatic AgentIdentity label is reserved for the controller")
	}
	if len(identity.ObjectMeta.OwnerReferences) != 0 {
		return denyAgentIdentity("AgentIdentity owner references are reserved for the controller")
	}
	if req.Operation == OperationCreate && identity.Status.Phase != "" {
		return denyAgentIdentity("AgentIdentity status is reserved for the controller")
	}
	if req.Operation == OperationUpdate {
		var previous v1alpha1.AgentIdentity
		if err := json.Unmarshal(req.OldObject, &previous); err != nil {
			return denyAgentIdentity(fmt.Sprintf("invalid existing AgentIdentity: %v", err))
		}
		oldStatus, _ := json.Marshal(previous.Status)
		newStatus, _ := json.Marshal(identity.Status)
		if string(oldStatus) != string(newStatus) {
			return denyAgentIdentity("AgentIdentity status is reserved for the controller")
		}
	}
	return &Response{Allowed: true}
}

func isAutomaticAgentIdentity(identity *v1alpha1.AgentIdentity) bool {
	return strings.HasPrefix(identity.ObjectMeta.Name, automaticAgentIdentityNamePrefix) ||
		identity.ObjectMeta.Labels[automaticAgentIdentityLabel] != ""
}

func (v *AgentIdentityValidator) isController(user UserInfo) bool {
	return v.controllerUsername != "" && user.Username == v.controllerUsername
}

func denyAgentIdentity(reason string) *Response {
	return &Response{Allowed: false, Reason: reason}
}
