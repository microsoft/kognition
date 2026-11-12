// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/kognition-ai/kognition/api/v1alpha1"
)

type fakeDockerProbeClient struct {
	request     dockerContainerCreateRequest
	pulledImage string
	startedID   string
	removedID   string
	exitCode    int
	output      []byte
	waitError   error
}

type fakeDockerStdioClient struct {
	*fakeDockerProbeClient
	stream io.ReadWriteCloser
}

func (f *fakeDockerStdioClient) Attach(_ context.Context, _ string) (io.ReadWriteCloser, error) {
	return f.stream, nil
}

func (f *fakeDockerProbeClient) Pull(_ context.Context, image string) error {
	f.pulledImage = image
	return nil
}

func (f *fakeDockerProbeClient) Create(_ context.Context, request dockerContainerCreateRequest) (string, error) {
	f.request = request
	return "sandbox-123", nil
}
func (f *fakeDockerProbeClient) Start(_ context.Context, id string) error {
	f.startedID = id
	return nil
}
func (f *fakeDockerProbeClient) Wait(_ context.Context, _ string) (int, error) {
	return f.exitCode, f.waitError
}
func (f *fakeDockerProbeClient) Logs(_ context.Context, _ string) ([]byte, error) {
	return f.output, nil
}
func (f *fakeDockerProbeClient) Remove(_ context.Context, id string) error {
	f.removedID = id
	return nil
}

func TestDockerExecProbeRunnerUsesRestrictedDirectArgv(t *testing.T) {
	client := &fakeDockerProbeClient{}
	runner := &DockerExecProbeRunner{client: client}
	target := ExecProbeTarget{Image: "registry.example/probe:1.0", CPUMilli: 250, MemoryMB: 128}
	command := []string{"/usr/local/bin/healthcheck", "--ready;not-a-shell"}
	if err := runner.RunExecProbe(context.Background(), &v1alpha1.Tool{}, target, command); err != nil {
		t.Fatalf("RunExecProbe: %v", err)
	}
	if client.pulledImage != target.Image || client.request.Image != target.Image || client.request.User != "65532:65532" || len(client.request.Cmd) != 2 || client.request.Cmd[0] != command[0] || client.request.Cmd[1] != command[1] {
		t.Fatalf("Docker request = %#v", client.request)
	}
	config := client.request.HostConfig
	if config.NetworkMode != "none" || !config.ReadonlyRoot || len(config.CapDrop) != 1 || config.CapDrop[0] != "ALL" || len(config.SecurityOpt) != 1 || config.SecurityOpt[0] != "no-new-privileges" || config.PidsLimit != 64 || config.Memory != 128*1024*1024 || config.NanoCPUs != 250*1_000_000 {
		t.Fatalf("Docker host config = %#v", config)
	}
	if client.startedID != "sandbox-123" || client.removedID != "sandbox-123" {
		t.Fatalf("sandbox lifecycle started=%q removed=%q", client.startedID, client.removedID)
	}
}

func TestDockerExecProbeRunnerBoundsFailureOutput(t *testing.T) {
	client := &fakeDockerProbeClient{exitCode: 42, output: []byte(strings.Repeat("x", maxExecProbeOutputBytes+100))}
	runner := &DockerExecProbeRunner{client: client}
	err := runner.RunExecProbe(context.Background(), &v1alpha1.Tool{}, ExecProbeTarget{Image: "registry.example/probe:1.0"}, []string{"healthcheck"})
	if err == nil || !strings.Contains(err.Error(), "exited 42") || !strings.HasSuffix(err.Error(), "...") {
		t.Fatalf("RunExecProbe error = %v, want bounded command failure", err)
	}
	if len(err.Error()) > maxExecProbeOutputBytes+128 {
		t.Fatalf("RunExecProbe error length = %d, want bounded output", len(err.Error()))
	}
}

func TestDockerExecProbeRunnerPropagatesWaitError(t *testing.T) {
	client := &fakeDockerProbeClient{waitError: errors.New("Docker daemon disconnected")}
	runner := &DockerExecProbeRunner{client: client}
	err := runner.RunExecProbe(context.Background(), &v1alpha1.Tool{}, ExecProbeTarget{Image: "registry.example/probe:1.0"}, []string{"healthcheck"})
	if err == nil || !strings.Contains(err.Error(), "Docker daemon disconnected") {
		t.Fatalf("RunExecProbe error = %v", err)
	}
}

func TestDockerExecProbeRunnerFunctional(t *testing.T) {
	if os.Getenv("KOGNITION_DOCKER_EXEC_PROBE_INTEGRATION") != "1" {
		t.Skip("set KOGNITION_DOCKER_EXEC_PROBE_INTEGRATION=1 to run against Docker Engine")
	}
	runner, err := NewDockerExecProbeRunner()
	if err != nil {
		t.Fatalf("NewDockerExecProbeRunner: %v", err)
	}
	target := ExecProbeTarget{Image: "python:3.12-alpine", CPUMilli: 100, MemoryMB: 64}
	command := []string{"/usr/local/bin/python", "-c", "import os, socket; assert os.geteuid() == 65532\ntry: socket.create_connection(('1.1.1.1', 443), timeout=2)\nexcept OSError: pass\nelse: raise AssertionError('sandbox network unexpectedly reachable')"}
	if err := runner.RunExecProbe(context.Background(), &v1alpha1.Tool{}, target, command); err != nil {
		t.Fatalf("RunExecProbe against Docker Engine: %v", err)
	}
}

func TestDockerStdioInvocationRunnerUsesClassOwnedSandbox(t *testing.T) {
	clientStream, serverStream := net.Pipe()
	defer serverStream.Close()
	go func() {
		reader := bufio.NewScanner(serverStream)
		encoder := json.NewEncoder(serverStream)
		if !reader.Scan() {
			return
		}
		_ = encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "result": map[string]any{}})
		if !reader.Scan() {
			return
		}
		if !reader.Scan() {
			return
		}
		_ = encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": 2, "result": map[string]any{"content": []any{}, "result": "ok"}})
	}()
	probe := &fakeDockerProbeClient{}
	runner := &DockerStdioInvocationRunner{client: &fakeDockerStdioClient{fakeDockerProbeClient: probe, stream: clientStream}}
	result, err := runner.RunStdioInvocation(context.Background(), &v1alpha1.Tool{ObjectMeta: v1alpha1.ObjectMeta{Name: "shell"}}, StdioInvocationTarget{Image: "registry.example/shell:1", Command: []string{"/srv/mcp", "--stdio"}, CPUMilli: 100, MemoryMB: 64}, map[string]any{"command": "status"})
	if err != nil || result.Output["result"] != "ok" || result.ExecutionMode != "sandbox" {
		t.Fatalf("RunStdioInvocation result=%#v err=%v", result, err)
	}
	if probe.request.Image != "registry.example/shell:1" || len(probe.request.Cmd) != 2 || probe.request.Cmd[0] != "/srv/mcp" || !probe.request.OpenStdin || !probe.request.AttachStdin || !probe.request.AttachStdout || probe.request.Tty || probe.request.HostConfig.NetworkMode != "none" || !probe.request.HostConfig.ReadonlyRoot {
		t.Fatalf("Docker stdio request = %#v", probe.request)
	}
}

func TestDockerStdioInvocationRunnerFunctional(t *testing.T) {
	if os.Getenv("KOGNITION_DOCKER_STDIO_INTEGRATION") != "1" {
		t.Skip("set KOGNITION_DOCKER_STDIO_INTEGRATION=1 to run against Docker Engine")
	}
	runner, err := NewDockerStdioInvocationRunner()
	if err != nil {
		t.Fatalf("NewDockerStdioInvocationRunner: %v", err)
	}
	server := `import json, os, sys
assert os.geteuid() == 65532
for line in sys.stdin:
 request=json.loads(line)
 if request.get("method") == "initialize": print(json.dumps({"jsonrpc":"2.0","id":request["id"],"result":{"protocolVersion":"2025-03-26"}}), flush=True)
 elif request.get("method") == "tools/call": print(json.dumps({"jsonrpc":"2.0","id":request["id"],"result":{"content":[],"status":"ok"}}), flush=True); break`
	result, err := runner.RunStdioInvocation(context.Background(), &v1alpha1.Tool{ObjectMeta: v1alpha1.ObjectMeta{Name: "shell"}}, StdioInvocationTarget{Image: "python:3.12-alpine", Command: []string{"/usr/local/bin/python", "-u", "-c", server}, CPUMilli: 100, MemoryMB: 64, Timeout: 10 * time.Second}, map[string]any{"command": "status"})
	if err != nil || result.Output["status"] != "ok" {
		t.Fatalf("RunStdioInvocation against Docker Engine result=%#v err=%v", result, err)
	}
}
