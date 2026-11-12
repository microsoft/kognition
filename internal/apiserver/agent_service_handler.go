// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package apiserver

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"go.uber.org/zap"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/requestid"
	"github.com/kognition-ai/kognition/internal/store"
)

func (s *Server) handleAgentServiceInvoke(w http.ResponseWriter, r *http.Request) {
	namespace := r.PathValue("namespace")
	serviceName := r.PathValue("name")
	raw, err := s.store.Get(r.Context(), "AgentService", namespace, serviceName)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, fmt.Sprintf("AgentService %q not found in namespace %q", serviceName, namespace))
			return
		}
		s.logger.Error("get agent service failed", zap.Error(err), requestid.ZapField(r.Context()))
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	var service v1alpha1.AgentService
	if err := json.Unmarshal(raw, &service); err != nil {
		s.logger.Error("unmarshal agent service failed", zap.Error(err), requestid.ZapField(r.Context()))
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if service.Status.Phase != v1alpha1.PhaseReady || service.Status.ReadyInstances < 1 {
		writeError(w, http.StatusServiceUnavailable, fmt.Sprintf("AgentService %q is not ready", serviceName))
		return
	}
	if service.Spec.AgentRef.Name == "" {
		writeError(w, http.StatusUnprocessableEntity, "AgentService has no agentRef")
		return
	}
	if service.Spec.RevisionPin != nil {
		r = withAgentRevision(r, *service.Spec.RevisionPin)
	}
	r.SetPathValue("name", service.Spec.AgentRef.Name)
	s.handleChat(w, r)
}
