// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package postgres

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/kognition-ai/kognition/internal/runtimeapproval"
)

func TestStandaloneGatewayCommandsStartWithoutKubernetes(t *testing.T) {
	_ = newTestStore(t)
	material := make([]byte, 32)
	if _, err := rand.Read(material); err != nil {
		t.Fatal(err)
	}
	key := base64.StdEncoding.EncodeToString(material)
	serviceMaterial := make([]byte, 32)
	if _, err := rand.Read(serviceMaterial); err != nil {
		t.Fatal(err)
	}
	serviceToken := base64.StdEncoding.EncodeToString(serviceMaterial)
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v1/memory/health" || request.Header.Get("Authorization") != "Bearer "+key {
			writer.WriteHeader(http.StatusUnauthorized)
			return
		}
		writer.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()
	bindingKeys, err := json.Marshal([]map[string]any{{"version": 1, "epoch": 1, "algorithm": runtimeapproval.BindingAlgorithmV1, "domain": runtimeapproval.BindingDomainV1, "status": runtimeapproval.BindingKeyActive, "material": key}})
	if err != nil {
		t.Fatal(err)
	}
	root, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	for _, component := range []string{"kognition-agent-communication-gateway", "kognition-memory-policy-gateway", "kognition-tool-gateway", "kognition-model-gateway"} {
		t.Run(component, func(t *testing.T) {
			buildCtx, cancelBuild := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancelBuild()
			binary := filepath.Join(t.TempDir(), component)
			if runtime.GOOS == "windows" {
				binary += ".exe"
			}
			build := exec.CommandContext(buildCtx, "go", "build", "-o", binary, "./cmd/"+component)
			build.Dir = root
			if output, err := build.CombinedOutput(); err != nil {
				t.Fatalf("build: %v (context: %v)\n%s", err, buildCtx.Err(), output)
			}
			cancelBuild()
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			address := listener.Addr().String()
			if err := listener.Close(); err != nil {
				t.Fatal(err)
			}
			command := exec.CommandContext(ctx, binary)
			for _, name := range []string{"SYSTEMROOT", "WINDIR", "TEMP", "TMP", "HOME", "USERPROFILE"} {
				if value := os.Getenv(name); value != "" {
					command.Env = append(command.Env, name+"="+value)
				}
			}
			command.Env = append(command.Env,
				"KOGNITION_MODE=standalone",
				"DATABASE_URL=postgres://postgres:postgres@localhost:15432/postgres?sslmode=disable",
				"KUBECONFIG="+filepath.Join(t.TempDir(), "missing-kubeconfig"),
				"LISTEN_ADDR="+address,
				"AGENT_DELEGATION_CAPABILITY_HMAC_KEY="+key,
				"MEMORY_CAPABILITY_HMAC_KEY="+key,
				"TOOL_CAPABILITY_HMAC_KEY="+key,
				"MODEL_CAPABILITY_HMAC_KEY="+key,
				"KOGNITION_RUNTIME_AUTH_TOKEN="+key,
				"KOGNITION_API_SERVER_URL=http://127.0.0.1:1",
				"KOGNITION_API_SERVER_TOKEN="+serviceToken,
				"KOGNITION_MEMORY_SERVICE_URL="+upstream.URL,
				"MEMORY_SERVICE_INTERNAL_TOKEN="+key,
				runtimeapproval.BindingKeysEnvironmentVariable+"="+string(bindingKeys),
			)
			var output bytes.Buffer
			command.Stdout, command.Stderr = &output, &output
			if err := command.Start(); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { done <- command.Wait() }()
			finished := false
			defer func() {
				cancel()
				if !finished {
					<-done
				}
			}()
			ticker := time.NewTicker(100 * time.Millisecond)
			defer ticker.Stop()
			client := &http.Client{Timeout: time.Second}
			healthPath := "/healthz"
			if component == "kognition-memory-policy-gateway" {
				healthPath = "/v1/memory/health"
			}
			for {
				select {
				case err := <-done:
					finished = true
					t.Fatalf("gateway exited before health check: %v\n%s", err, output.String())
				case <-ctx.Done():
					t.Fatal("gateway did not become healthy before deadline")
				case <-ticker.C:
					response, err := client.Get("http://" + address + healthPath)
					if err != nil {
						continue
					}
					_, _ = io.Copy(io.Discard, response.Body)
					_ = response.Body.Close()
					if response.StatusCode == http.StatusOK {
						return
					}
				}
			}
		})
	}
}
