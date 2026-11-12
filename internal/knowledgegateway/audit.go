// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package knowledgegateway

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"
	"github.com/kognition-ai/kognition/internal/k8s/knowledge"
	"github.com/kognition-ai/kognition/internal/observability"
	"github.com/kognition-ai/kognition/internal/runtimeauth"
)

func WithQueryAuditLog(audit *observability.CognitiveAuditLog) Option {
	return func(server *Server) { server.audit = audit }
}

func (server *Server) recordQuery(ctx context.Context, claims *runtimeauth.KnowledgeSessionCapabilityClaims, binding *knowledge.ResolvedBinding, input queryRequest, outcome string, before bool) error {
	if server.audit == nil {
		return nil
	}
	details, err := json.Marshal(struct {
		Version             int    `json:"version"`
		RequestID           string `json:"requestID"`
		Outcome             string `json:"outcome"`
		Mode                string `json:"mode"`
		KnowledgeBaseUID    string `json:"knowledgeBaseUID"`
		Generation          int64  `json:"generation"`
		ConfigurationUID    string `json:"configurationUID"`
		ConfigurationDigest string `json:"configurationDigest"`
		InstanceUID         string `json:"instanceUID"`
		CapabilityID        string `json:"capabilityID"`
	}{1, input.RequestID, outcome, input.Mode, binding.Scope.KnowledgeBaseUid, binding.Scope.Generation, binding.Scope.ConfigurationUid, binding.Scope.ConfigurationDigest, claims.InstanceUID, claims.ID})
	if err != nil {
		return err
	}
	entry := observability.AuditEntry{
		ID: uuid.NewString(), Timestamp: time.Now().UTC(), AgentIdentity: claims.AgentIdentityName,
		Workspace: claims.Namespace, SessionID: claims.SessionID, EventType: observability.AuditKnowledgeQuery,
		SubjectKind: "KnowledgeBase", SubjectRef: input.KnowledgeBase, Verb: "query", Details: details,
	}
	if before {
		return server.audit.RecordBeforeExec(ctx, entry)
	}
	return server.audit.Record(ctx, entry)
}
