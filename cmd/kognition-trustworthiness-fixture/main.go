// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/kognition-ai/kognition/internal/observability"
)

type fixtureStore struct {
	mu      sync.RWMutex
	entries []observability.AuditEntry
}

func (s *fixtureStore) Query(_ context.Context, filter observability.AuditFilter) ([]observability.AuditEntry, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	entries := make([]observability.AuditEntry, 0, len(s.entries))
	for _, entry := range s.entries {
		if filter.Workspace != "" && entry.Workspace != filter.Workspace {
			continue
		}
		if filter.AgentIdentity != "" && entry.AgentIdentity != filter.AgentIdentity {
			continue
		}
		if filter.SessionID != "" && entry.SessionID != filter.SessionID {
			continue
		}
		if !filter.Since.IsZero() && entry.Timestamp.Before(filter.Since) {
			continue
		}
		if !filter.Until.IsZero() && entry.Timestamp.After(filter.Until) {
			continue
		}
		entries = append(entries, entry)
	}
	sort.Slice(entries, func(left, right int) bool {
		if filter.Descending {
			return entries[left].Timestamp.After(entries[right].Timestamp)
		}
		return entries[left].Timestamp.Before(entries[right].Timestamp)
	})
	if filter.Limit > 0 && len(entries) > filter.Limit {
		entries = entries[:filter.Limit]
	}
	return entries, nil
}

func (s *fixtureStore) append(entry observability.AuditEntry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entries = append(s.entries, entry)
}

type fixtureState struct {
	mu        sync.Mutex
	workspace string
	runID     string
	grant     bool
	counters  map[string]int
	store     *fixtureStore
}

func main() {
	listenAddress := flag.String("listen", "127.0.0.1:0", "HTTP listen address")
	workspace := flag.String("workspace", "kognition-hack-standalone", "fixture workspace")
	runID := flag.String("run-id", "standalone-demo", "fixture run ID")
	flag.Parse()

	if strings.TrimSpace(*workspace) == "" || strings.TrimSpace(*runID) == "" {
		fmt.Fprintln(os.Stderr, "workspace and run-id are required")
		os.Exit(2)
	}
	listener, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", *listenAddress)
	if err != nil {
		fmt.Fprintf(os.Stderr, "listen: %v\n", err)
		os.Exit(1)
	}

	server := &http.Server{Handler: newFixtureHandler(*workspace, *runID), ReadHeaderTimeout: 5 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()

	fmt.Printf("Kognition trustworthiness fixture listening on http://%s\n", listener.Addr())
	if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
		fmt.Fprintf(os.Stderr, "serve: %v\n", err)
		os.Exit(1)
	}
}

func newFixtureHandler(workspace, runID string) http.Handler {
	state := &fixtureState{workspace: workspace, runID: runID, counters: map[string]int{}, store: &fixtureStore{}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /apis/kognition.dev/v1alpha1/namespaces/{namespace}/trust-decisions", func(w http.ResponseWriter, request *http.Request) {
		query, err := observability.ParseTrustDecisionQuery(request.PathValue("namespace"), request.URL.Query())
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		result, err := observability.QueryTrustDecisions(request.Context(), state.store, query, time.Now())
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, result)
	})
	mux.HandleFunc("POST /fixture/grant", func(w http.ResponseWriter, _ *http.Request) {
		state.mu.Lock()
		state.grant = true
		state.mu.Unlock()
		writeJSON(w, http.StatusCreated, map[string]bool{"active": true})
	})
	mux.HandleFunc("DELETE /fixture/grant", func(w http.ResponseWriter, _ *http.Request) {
		state.mu.Lock()
		state.grant = false
		state.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]bool{"active": false})
	})
	mux.HandleFunc("POST /fixture/operations/{plane}/{target}", func(w http.ResponseWriter, request *http.Request) {
		decision, status, err := state.execute(request.PathValue("plane"), request.PathValue("target"), time.Now().UTC())
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, status, decision)
	})
	mux.HandleFunc("POST /fixture/approval", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusCreated, map[string]string{
			"status": "approved", "kind": "StandaloneApprovalEvidence", "name": runID + "-approval",
		})
	})
	mux.HandleFunc("GET /fixture/counters", func(w http.ResponseWriter, _ *http.Request) {
		state.mu.Lock()
		defer state.mu.Unlock()
		writeJSON(w, http.StatusOK, state.counters)
	})
	return mux
}

func (s *fixtureState) execute(plane, target string, now time.Time) (observability.TrustDecisionV1, int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	operation := "invoke"
	targetKind := "Tool"
	if plane == "memory" {
		operation = "recall"
		targetKind = "MemoryVolume"
	} else if plane != "tool" {
		return observability.TrustDecisionV1{}, 0, fmt.Errorf("unsupported plane %q", plane)
	}

	outcome, reasonCode, executionStatus, status := "allowed", "authorized", "completed", http.StatusOK
	targetReached := true
	if !s.grant {
		outcome, reasonCode, executionStatus, status = "revoked", "grant_revoked", "blocked", http.StatusForbidden
		targetReached = false
	} else if plane == "tool" && target == "admin-write" {
		outcome, reasonCode, executionStatus, status = "denied", "policy_denied", "blocked", http.StatusForbidden
		targetReached = false
	}
	if targetReached {
		s.counters[plane+":"+target]++
	}

	suffix := plane + "-allow"
	if outcome == "denied" {
		suffix = plane + "-deny"
	} else if outcome == "revoked" {
		suffix = plane + "-revoked"
	}
	identity := "agent/standalone-auditor"
	sessionID := s.runID + ":session"
	grantRef := &observability.TrustDecisionResourceRef{Kind: "FixtureGrant", Namespace: s.workspace, Name: s.runID + "-grant"}
	policyRef := &observability.TrustDecisionResourceRef{Kind: "FixturePolicy", Namespace: s.workspace, Name: s.runID + "-policy"}
	decision := fixtureDecision(s.workspace, s.runID, suffix, now, plane, outcome, reasonCode, operation, targetKind, target, identity, sessionID, grantRef, policyRef, executionStatus, &targetReached)
	details, err := json.Marshal(decision)
	if err != nil {
		return observability.TrustDecisionV1{}, 0, err
	}
	eventType := observability.AuditToolCall
	if plane == "memory" {
		eventType = observability.AuditMemoryOp
	}
	s.store.append(observability.AuditEntry{
		ID: decision.ID, Timestamp: decision.Timestamp, AgentIdentity: identity, Workspace: s.workspace,
		SessionID: sessionID, EventType: eventType, SubjectKind: targetKind,
		SubjectRef: target, Verb: operation, Details: details,
	})
	return decision, status, nil
}

func fixtureDecision(workspace, runID, suffix string, timestamp time.Time, plane, outcome, reasonCode, operation, targetKind, targetName, identity, sessionID string, grantRef, policyRef *observability.TrustDecisionResourceRef, executionStatus string, targetReached *bool) observability.TrustDecisionV1 {
	return observability.TrustDecisionV1{
		SchemaVersion: 1, ID: runID + ":" + suffix, Timestamp: timestamp, Plane: plane, Outcome: outcome,
		ReasonCode: reasonCode, Workspace: workspace, RunID: runID,
		Identity:      &observability.TrustDecisionIdentity{AgentIdentity: identity},
		Session:       &observability.TrustDecisionSession{SessionID: sessionID},
		Request:       observability.TrustDecisionRequest{Operation: operation, InvocationID: runID + ":" + suffix, TargetRef: observability.TrustDecisionResourceRef{Kind: targetKind, Namespace: workspace, Name: targetName}},
		Authorization: observability.TrustDecisionAuthorization{Identity: "allowed", Role: "allowed", Grant: evidenceStatus(outcome, "grant"), Policy: evidenceStatus(outcome, "policy"), GrantRef: grantRef, PolicyRef: policyRef},
		Execution:     observability.TrustDecisionExecution{Status: executionStatus, TargetReached: targetReached},
	}
}

func evidenceStatus(outcome, check string) string {
	if outcome == "revoked" && check == "grant" {
		return "revoked"
	}
	if outcome == "denied" && check == "policy" {
		return "denied"
	}
	if outcome == "approved" {
		return "not_required"
	}
	return "allowed"
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
