// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Command kognition-e2e-external-agent is a minimal A2A v1 remote agent used to
// exercise ExternalAgent interoperability. It serves its Agent Card without
// credentials and requires a bearer token on the JSON-RPC endpoint. TLS is
// expected to be terminated in front of it; the card advertises https://<Host>/.
// The card therefore depends on the Host header, so its digest includes the
// port the controller dials (for example :443); pin status.observedCardDigest.
package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"iter"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
)

const cardPath = "/.well-known/agent-card.json"

type executor struct{}

func (executor) Execute(_ context.Context, execCtx *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		if !yield(a2a.NewSubmittedTask(execCtx, execCtx.Message), nil) {
			return
		}
		var prompt strings.Builder
		if execCtx.Message != nil {
			for _, part := range execCtx.Message.Parts {
				if text, ok := part.Content.(a2a.Text); ok {
					prompt.WriteString(string(text))
				}
			}
		}
		summary := strings.Join(strings.Fields(prompt.String()), " ")
		if len(summary) > 240 {
			summary = summary[:240]
		}
		if !yield(a2a.NewArtifactEvent(execCtx, a2a.NewTextPart("External compliance review complete. Request: "+summary)), nil) {
			return
		}
		yield(a2a.NewStatusUpdateEvent(execCtx, a2a.TaskStateCompleted, nil), nil)
	}
}

func (executor) Cancel(_ context.Context, execCtx *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		yield(a2a.NewStatusUpdateEvent(execCtx, a2a.TaskStateCanceled, nil), nil)
	}
}

func card(host string) ([]byte, error) {
	return json.Marshal(a2a.AgentCard{
		Name:                "partner-compliance",
		Description:         "Interoperability test agent for Kognition ExternalAgent",
		Version:             "1",
		SupportedInterfaces: []*a2a.AgentInterface{a2a.NewAgentInterface("https://"+host+"/", a2a.TransportProtocolJSONRPC)},
		DefaultInputModes:   []string{"text/plain"},
		DefaultOutputModes:  []string{"text/plain"},
		Skills:              []a2a.AgentSkill{{ID: "review", Name: "review", Description: "compliance review", Tags: []string{"compliance"}}},
	})
}

func main() {
	token := os.Getenv("EXTERNAL_AGENT_TOKEN")
	if token == "" {
		log.Fatal("EXTERNAL_AGENT_TOKEN is required")
	}
	listen := os.Getenv("LISTEN")
	if listen == "" {
		listen = ":8080"
	}
	rpc := a2asrv.NewJSONRPCHandler(a2asrv.NewHandler(executor{}))
	mux := http.NewServeMux()
	mux.HandleFunc(cardPath, func(w http.ResponseWriter, r *http.Request) {
		raw, err := card(r.Host)
		if err != nil {
			http.Error(w, "card", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(raw)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if r.Method != http.MethodPost || r.URL.Path != "/" || subtle.ConstantTimeCompare([]byte(got), []byte(token)) != 1 {
			status := http.StatusUnauthorized
			if r.Method != http.MethodPost || r.URL.Path != "/" {
				status = http.StatusNotFound
			}
			http.Error(w, http.StatusText(status), status)
			return
		}
		rpc.ServeHTTP(w, r)
	})
	server := &http.Server{Addr: listen, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}
