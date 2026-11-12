// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cmd

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kubernetesfake "k8s.io/client-go/kubernetes/fake"
)

func TestRunGoal_SubmitsJoinedGoal(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if r.URL.Path != "/apis/kognition.dev/v1alpha1/namespaces/team-a/intentgateways/primary/goals" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("content type = %q", got)
		}
		var payload map[string]string
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		if payload["goal"] != "prepare release notes" {
			t.Errorf("payload = %#v", payload)
		}
		_, _ = w.Write([]byte(`{"status":"accepted"}`))
	}))
	defer server.Close()

	restore := configureGetTest(server)
	defer restore()

	output := captureStdout(t, func() error {
		return runGoal(nil, []string{"primary", "prepare", "release", "notes"})
	})
	if !strings.Contains(output, `goal submitted to intentgateway "primary"`) {
		t.Fatalf("output = %q", output)
	}
}

func TestRunGoal_PropagatesServerError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "gateway paused", http.StatusConflict)
	}))
	defer server.Close()

	restore := configureGetTest(server)
	defer restore()

	err := runGoal(nil, []string{"primary", "do work"})
	if err == nil || !strings.Contains(err.Error(), "409") || !strings.Contains(err.Error(), "gateway paused") {
		t.Fatalf("error = %v, want HTTP 409 response", err)
	}
}

func TestRunChatStandalone_SubmitsSessionAndPrintsResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/apis/kognition.dev/v1alpha1/namespaces/team-a/agents/planner/chat" {
			t.Errorf("path = %q", r.URL.Path)
		}
		var payload map[string]string
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		if payload["message"] != "review this plan" || payload["sessionID"] != "session-4" {
			t.Errorf("payload = %#v", payload)
		}
		_, _ = w.Write([]byte(`{"sessionID":"session-4","response":"Plan reviewed."}`))
	}))
	defer server.Close()

	restore := configureInteractionTest(server)
	defer restore()
	chatSessionID = "session-4"

	output := captureStdout(t, func() error {
		return runChat(nil, []string{"planner", "review", "this", "plan"})
	})
	if strings.TrimSpace(output) != "Plan reviewed." {
		t.Fatalf("output = %q", output)
	}
}

func TestRunChatStandalone_RendersStructuredOutput(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"sessionID":"session-5","response":"Ready."}`))
	}))
	defer server.Close()

	restore := configureInteractionTest(server)
	defer restore()
	outputFormat = "json"

	output := captureStdout(t, func() error {
		return runChat(nil, []string{"planner", "hello"})
	})
	if !strings.Contains(output, `"sessionID": "session-5"`) || !strings.Contains(output, `"response": "Ready."`) {
		t.Fatalf("output = %s", output)
	}
}

func TestRunChatStandalone_PropagatesServerError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "agent unavailable", http.StatusServiceUnavailable)
	}))
	defer server.Close()

	restore := configureInteractionTest(server)
	defer restore()

	err := runChat(nil, []string{"planner", "hello"})
	if err == nil || !strings.Contains(err.Error(), "503") || !strings.Contains(err.Error(), "agent unavailable") {
		t.Fatalf("error = %v, want HTTP 503 response", err)
	}
}

func TestRunChatStandalone_HandlesBackendResponseVariants(t *testing.T) {
	tests := []struct {
		name       string
		response   string
		wantOutput string
		wantStderr string
	}{
		{name: "non JSON response", response: "plain response", wantOutput: "plain response"},
		{name: "server assigned session", response: `{"sessionID":"session-new","response":"Ready."}`, wantOutput: "Ready.", wantStderr: "session: session-new"},
		{name: "empty response field", response: `{"sessionID":"session-existing"}`, wantOutput: `{"sessionID":"session-existing"}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(test.response))
			}))
			defer server.Close()
			restore := configureInteractionTest(server)
			defer restore()

			var output string
			stderr := captureStderr(t, func() error {
				output = captureStdout(t, func() error {
					return runChat(nil, []string{"planner", "hello"})
				})
				return nil
			})
			if !strings.Contains(output, test.wantOutput) || !strings.Contains(stderr, test.wantStderr) {
				t.Fatalf("stdout = %q, stderr = %q", output, stderr)
			}
		})
	}
}

func TestRunChatRuntimeDirect_AsyncProtocolCompletion(t *testing.T) {
	for _, tc := range []struct {
		name         string
		outputFormat string
		want         []string
	}{
		{name: "plain", want: []string{"runtime completed"}},
		{name: "structured", outputFormat: "json", want: []string{`"output": "runtime completed"`, `"conversationID": "conversation-7"`, `"toolCalls": 2`, `"promptTokens": 8`}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var sessionMu sync.Mutex
			var sessionID string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodPost && r.URL.Path == "/invoke":
					var payload map[string]string
					if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
						t.Error(err)
						http.Error(w, "invalid payload", http.StatusBadRequest)
						return
					}
					if payload["message"] != "review this plan" || payload["conversation_id"] != "conversation-7" || payload["session_id"] == "" {
						t.Errorf("invoke payload = %#v", payload)
					}
					sessionMu.Lock()
					sessionID = payload["session_id"]
					sessionMu.Unlock()
					w.WriteHeader(http.StatusAccepted)
				case r.Method == http.MethodGet && r.URL.Path == "/status":
					sessionMu.Lock()
					currentSession := sessionID
					sessionMu.Unlock()
					_ = json.NewEncoder(w).Encode(map[string]any{
						"phase":             "Completed",
						"session_id":        currentSession,
						"output":            "runtime completed",
						"token_usage":       map[string]any{"promptTokens": 8, "completionTokens": 3},
						"tool_calls":        2,
						"tool_calls_detail": []map[string]any{{"name": "search"}},
					})
				default:
					t.Errorf("unexpected runtime request %s %s", r.Method, r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer server.Close()

			restore := configureInteractionTest(server)
			defer restore()
			k8sMode = true
			chatSessionID = "conversation-7"
			outputFormat = tc.outputFormat
			seedAgentPortForward(t, server, "team-a", "planner")
			command := &cobra.Command{}
			command.SetContext(context.Background())

			output := captureStdout(t, func() error {
				return runChatRuntimeDirect(command, "planner", "review this plan")
			})
			for _, want := range tc.want {
				if !strings.Contains(output, want) {
					t.Fatalf("output missing %q:\n%s", want, output)
				}
			}
		})
	}
}

func TestRunChatRuntimeDirect_SendsRuntimeAuthorization(t *testing.T) {
	var sessionID string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer controller-token" {
			http.Error(w, "missing runtime authorization", http.StatusUnauthorized)
			return
		}
		if r.URL.Path == "/invoke" {
			var payload map[string]string
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Error(err)
				return
			}
			sessionID = payload["session_id"]
			w.WriteHeader(http.StatusAccepted)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"phase": "Completed", "session_id": sessionID, "output": "authorized"})
	}))
	defer server.Close()

	restore := configureInteractionTest(server)
	defer restore()
	k8sMode = true
	oldCoreClient := k8sCoreClient
	k8sCoreClient = kubernetesfake.NewSimpleClientset(
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "agent-planner", Namespace: "team-a", Labels: map[string]string{"kognition.dev/agent-name": "planner", "kognition.dev/agent-namespace": "team-a", "app.kubernetes.io/managed-by": "kognition"}}, Spec: corev1.PodSpec{Containers: []corev1.Container{{Env: []corev1.EnvVar{{Name: "KOGNITION_RUNTIME_AUTH_TOKEN", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "runtime-auth"}, Key: "runtime-control-token"}}}}}}}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "runtime-auth", Namespace: "team-a"}, Data: map[string][]byte{"runtime-control-token": []byte("controller-token")}},
	)
	t.Cleanup(func() { k8sCoreClient = oldCoreClient })
	seedAgentPortForward(t, server, "team-a", "planner")
	command := &cobra.Command{}
	command.SetContext(context.Background())

	output := captureStdout(t, func() error {
		return runChatRuntimeDirect(command, "planner", "hello")
	})
	if strings.TrimSpace(output) != "authorized" {
		t.Fatalf("output = %q, want authorized", output)
	}
}

func TestRunChatRuntimeDirect_ProtocolFailures(t *testing.T) {
	for _, tc := range []struct {
		name         string
		invokeStatus int
		statusStatus int
		phase        string
		runtimeError string
		want         string
	}{
		{name: "invoke rejected", invokeStatus: http.StatusServiceUnavailable, want: "agent returned 503"},
		{name: "status rejected", invokeStatus: http.StatusAccepted, statusStatus: http.StatusBadGateway, want: "status returned 502"},
		{name: "run failed", invokeStatus: http.StatusAccepted, statusStatus: http.StatusOK, phase: "Failed", runtimeError: "model unavailable", want: "model unavailable"},
		{name: "run failed without detail", invokeStatus: http.StatusAccepted, statusStatus: http.StatusOK, phase: "Failed", want: "agent run failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var sessionMu sync.Mutex
			var sessionID string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/invoke":
					var payload map[string]string
					if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
						t.Error(err)
						return
					}
					sessionMu.Lock()
					sessionID = payload["session_id"]
					sessionMu.Unlock()
					w.WriteHeader(tc.invokeStatus)
					if tc.invokeStatus >= 400 {
						_, _ = w.Write([]byte("runtime unavailable"))
					}
				case "/status":
					w.WriteHeader(tc.statusStatus)
					if tc.statusStatus >= 400 {
						_, _ = w.Write([]byte("status unavailable"))
						return
					}
					sessionMu.Lock()
					currentSession := sessionID
					sessionMu.Unlock()
					_ = json.NewEncoder(w).Encode(map[string]any{"phase": tc.phase, "session_id": currentSession, "error": tc.runtimeError})
				default:
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			restore := configureInteractionTest(server)
			defer restore()
			k8sMode = true
			seedAgentPortForward(t, server, "team-a", "planner")
			command := &cobra.Command{}
			command.SetContext(context.Background())

			err := runChatRuntimeDirect(command, "planner", "hello")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("runChat() error = %v, want message containing %q", err, tc.want)
			}
		})
	}
}

func TestRunChatRuntimeDirect_RetriesMalformedAndStaleStatus(t *testing.T) {
	var sessionID string
	statusCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/invoke":
			var payload map[string]string
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Error(err)
				return
			}
			sessionID = payload["session_id"]
			w.WriteHeader(http.StatusAccepted)
		case "/status":
			statusCalls++
			switch statusCalls {
			case 1:
				_, _ = w.Write([]byte("not-json"))
			case 2:
				_ = json.NewEncoder(w).Encode(map[string]any{"phase": "Completed", "session_id": "previous-turn"})
			default:
				_ = json.NewEncoder(w).Encode(map[string]any{"phase": "Completed", "session_id": sessionID, "output": "current turn"})
			}
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	restore := configureInteractionTest(server)
	defer restore()
	k8sMode = true
	seedAgentPortForward(t, server, "team-a", "planner")
	command := &cobra.Command{}
	command.SetContext(context.Background())

	output := captureStdout(t, func() error {
		return runChatRuntimeDirect(command, "planner", "hello")
	})
	if strings.TrimSpace(output) != "current turn" || statusCalls != 3 {
		t.Fatalf("output = %q, status calls = %d", output, statusCalls)
	}
}

func TestRunChatRuntimeDirect_ReportsTunnelResolutionFailure(t *testing.T) {
	previousMode, previousNamespace, previousCoreClient := k8sMode, namespace, k8sCoreClient
	k8sMode, namespace, k8sCoreClient = true, "team-a", nil
	t.Cleanup(func() {
		k8sMode, namespace, k8sCoreClient = previousMode, previousNamespace, previousCoreClient
	})
	key := "team-a/planner"
	pfCacheMu.Lock()
	delete(pfCache, key)
	delete(pfInflight, key)
	pfCacheMu.Unlock()
	command := &cobra.Command{}
	command.SetContext(context.Background())

	err := runChatRuntimeDirect(command, "planner", "hello")
	if err == nil || !strings.Contains(err.Error(), "resolve agent URL") || !strings.Contains(err.Error(), "kubernetes client not initialized") {
		t.Fatalf("runChat() error = %v, want tunnel resolution failure", err)
	}
}

func seedAgentPortForward(t *testing.T, server *httptest.Server, namespace, agentName string) {
	t.Helper()
	hostPort := strings.TrimPrefix(server.URL, "http://")
	_, portText, err := net.SplitHostPort(hostPort)
	if err != nil {
		t.Fatalf("split test server address: %v", err)
	}
	port, err := strconv.ParseUint(portText, 10, 16)
	if err != nil {
		t.Fatalf("parse test server port: %v", err)
	}
	key := namespace + "/" + agentName
	forward := &agentPortForward{LocalPort: uint16(port), stopChan: make(chan struct{})}
	pfCacheMu.Lock()
	pfCache[key] = forward
	delete(pfInflight, key)
	pfCacheMu.Unlock()
	t.Cleanup(func() {
		pfCacheMu.Lock()
		cached := pfCache[key]
		delete(pfCache, key)
		delete(pfInflight, key)
		pfCacheMu.Unlock()
		if cached != nil {
			select {
			case <-cached.stopChan:
			default:
				close(cached.stopChan)
			}
		}
	})
}

func configureInteractionTest(server *httptest.Server) func() {
	restore := configureGetTest(server)
	oldSessionID := chatSessionID
	chatSessionID = ""
	return func() {
		chatSessionID = oldSessionID
		restore()
	}
}
