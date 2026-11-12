// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/kognition-ai/kognition/api/v1alpha1"
)

const (
	defaultExecProbeCPUMilli int64 = 250
	defaultExecProbeMemoryMB int64 = 128
	maxExecProbeOutputBytes        = 4096
	dockerAPIVersion               = "v1.44"
)

// dockerProbeClient is the minimal Docker Engine API surface needed for a
// one-shot exec probe sandbox.
type dockerProbeClient interface {
	Pull(context.Context, string) error
	Create(context.Context, dockerContainerCreateRequest) (string, error)
	Start(context.Context, string) error
	Wait(context.Context, string) (int, error)
	Logs(context.Context, string) ([]byte, error)
	Remove(context.Context, string) error
}

type dockerStdioClient interface {
	Pull(context.Context, string) error
	Create(context.Context, dockerContainerCreateRequest) (string, error)
	Start(context.Context, string) error
	Wait(context.Context, string) (int, error)
	Logs(context.Context, string) ([]byte, error)
	Remove(context.Context, string) error
	Attach(context.Context, string) (io.ReadWriteCloser, error)
}

// DockerExecProbeRunner executes probes in a one-shot Docker Engine container.
// It never invokes a shell or the Docker CLI from the controller process.
type DockerExecProbeRunner struct {
	client dockerProbeClient
}

// DockerStdioInvocationRunner executes a single MCP stdio call in a
// class-owned Docker Engine sandbox. It never invokes a host shell or Docker
// CLI.
type DockerStdioInvocationRunner struct {
	client dockerStdioClient
}

func NewDockerStdioInvocationRunner() (*DockerStdioInvocationRunner, error) {
	host := strings.TrimSpace(os.Getenv("DOCKER_HOST"))
	if host == "" {
		host = "unix:///var/run/docker.sock"
	}
	client, err := newDockerEngineClient(host)
	if err != nil {
		return nil, err
	}
	return &DockerStdioInvocationRunner{client: client}, nil
}

func (r *DockerStdioInvocationRunner) RunStdioInvocation(ctx context.Context, tool *v1alpha1.Tool, target StdioInvocationTarget, input map[string]any) (StdioInvocationResult, error) {
	if r == nil || r.client == nil {
		return StdioInvocationResult{}, fmt.Errorf("Docker stdio invocation runner is not configured")
	}
	if tool == nil || strings.TrimSpace(target.Image) == "" || len(target.Command) == 0 {
		return StdioInvocationResult{}, fmt.Errorf("stdio invocation Tool, target image, and command are required")
	}
	timeout := target.Timeout
	if timeout <= 0 {
		timeout = defaultProbeTimeout
	}
	sandboxCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if err := r.client.Pull(sandboxCtx, target.Image); err != nil {
		return StdioInvocationResult{}, fmt.Errorf("pull stdio sandbox image: %w", err)
	}
	containerID, err := r.client.Create(sandboxCtx, dockerContainerCreateRequest{
		Image: target.Image, Cmd: append([]string(nil), target.Command...), User: "65532:65532",
		OpenStdin: true, AttachStdin: true, AttachStdout: true, AttachStderr: true,
		HostConfig: restrictedDockerHostConfig(target.CPUMilli, target.MemoryMB),
	})
	if err != nil {
		return StdioInvocationResult{}, fmt.Errorf("create stdio sandbox container: %w", err)
	}
	defer func() { _ = r.client.Remove(context.Background(), containerID) }()
	if err := r.client.Start(sandboxCtx, containerID); err != nil {
		return StdioInvocationResult{}, fmt.Errorf("start stdio sandbox container: %w", err)
	}
	stream, err := r.client.Attach(sandboxCtx, containerID)
	if err != nil {
		return StdioInvocationResult{}, fmt.Errorf("attach stdio sandbox container: %w", err)
	}
	output, invokeErr := InvokeMCPStdio(sandboxCtx, stream, tool, input)
	exitCode, waitErr := r.client.Wait(sandboxCtx, containerID)
	if invokeErr != nil {
		return StdioInvocationResult{}, fmt.Errorf("invoke stdio sandbox: %w", invokeErr)
	}
	if waitErr != nil {
		return StdioInvocationResult{}, fmt.Errorf("wait for stdio sandbox container: %w", waitErr)
	}
	if exitCode != 0 {
		logs, _ := r.client.Logs(sandboxCtx, containerID)
		return StdioInvocationResult{}, fmt.Errorf("stdio sandbox container exited %d: %s", exitCode, truncateStdioOutput(logs))
	}
	return StdioInvocationResult{Output: output, ExecutionMode: "sandbox"}, nil
}

// NewDockerExecProbeRunner creates the standalone production runner. DOCKER_HOST
// must be a Unix socket URL, for example unix:///var/run/docker.sock.
func NewDockerExecProbeRunner() (*DockerExecProbeRunner, error) {
	host := strings.TrimSpace(os.Getenv("DOCKER_HOST"))
	if host == "" {
		host = "unix:///var/run/docker.sock"
	}
	client, err := newDockerEngineClient(host)
	if err != nil {
		return nil, err
	}
	return &DockerExecProbeRunner{client: client}, nil
}

// RunExecProbe runs direct command argv in a restrictive OCI sandbox.
func (r *DockerExecProbeRunner) RunExecProbe(ctx context.Context, _ *v1alpha1.Tool, target ExecProbeTarget, command []string) error {
	if r == nil || r.client == nil {
		return fmt.Errorf("Docker exec probe runner is not configured")
	}
	if strings.TrimSpace(target.Image) == "" || len(command) == 0 {
		return fmt.Errorf("exec probe target image and command are required")
	}
	if err := r.client.Pull(ctx, target.Image); err != nil {
		return fmt.Errorf("pull sandbox image: %w", err)
	}
	containerID, err := r.client.Create(ctx, dockerContainerCreateRequest{
		Image: target.Image,
		Cmd:   append([]string(nil), command...),
		User:  "65532:65532",
		HostConfig: dockerHostConfig{
			NetworkMode:  "none",
			ReadonlyRoot: true,
			CapDrop:      []string{"ALL"},
			SecurityOpt:  []string{"no-new-privileges"},
			PidsLimit:    64,
			Memory:       target.MemoryMB * 1024 * 1024,
			NanoCPUs:     target.CPUMilli * 1_000_000,
			Tmpfs:        map[string]string{"/tmp": "rw,noexec,nosuid,size=16m"},
		},
	})
	if err != nil {
		return fmt.Errorf("create sandbox container: %w", err)
	}
	defer func() { _ = r.client.Remove(context.Background(), containerID) }()
	if err := r.client.Start(ctx, containerID); err != nil {
		return fmt.Errorf("start sandbox container: %w", err)
	}
	exitCode, err := r.client.Wait(ctx, containerID)
	if err != nil {
		return fmt.Errorf("wait for sandbox container: %w", err)
	}
	if exitCode == 0 {
		return nil
	}
	output, logErr := r.client.Logs(ctx, containerID)
	if logErr != nil {
		return fmt.Errorf("sandbox container exited %d", exitCode)
	}
	message := strings.TrimSpace(string(output))
	if len(message) > maxExecProbeOutputBytes {
		message = message[:maxExecProbeOutputBytes] + "..."
	}
	if message == "" {
		return fmt.Errorf("sandbox container exited %d", exitCode)
	}
	return fmt.Errorf("sandbox container exited %d: %s", exitCode, message)
}

type dockerContainerCreateRequest struct {
	Image        string           `json:"Image"`
	Cmd          []string         `json:"Cmd"`
	User         string           `json:"User"`
	OpenStdin    bool             `json:"OpenStdin,omitempty"`
	AttachStdin  bool             `json:"AttachStdin,omitempty"`
	AttachStdout bool             `json:"AttachStdout,omitempty"`
	AttachStderr bool             `json:"AttachStderr,omitempty"`
	Tty          bool             `json:"Tty,omitempty"`
	HostConfig   dockerHostConfig `json:"HostConfig"`
}

type dockerHostConfig struct {
	NetworkMode  string            `json:"NetworkMode"`
	ReadonlyRoot bool              `json:"ReadonlyRootfs"`
	CapDrop      []string          `json:"CapDrop"`
	SecurityOpt  []string          `json:"SecurityOpt"`
	PidsLimit    int64             `json:"PidsLimit"`
	Memory       int64             `json:"Memory"`
	NanoCPUs     int64             `json:"NanoCpus"`
	Tmpfs        map[string]string `json:"Tmpfs"`
}

type dockerEngineClient struct {
	http       *http.Client
	socketPath string
}

func newDockerEngineClient(host string) (*dockerEngineClient, error) {
	endpoint, err := url.Parse(host)
	if err != nil {
		return nil, fmt.Errorf("parse DOCKER_HOST: %w", err)
	}
	if endpoint.Scheme != "unix" || endpoint.Path == "" {
		return nil, fmt.Errorf("DOCKER_HOST must be a Unix socket URL")
	}
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", endpoint.Path)
		},
	}
	return &dockerEngineClient{http: &http.Client{Transport: transport}, socketPath: endpoint.Path}, nil
}

func restrictedDockerHostConfig(cpuMilli, memoryMB int64) dockerHostConfig {
	return dockerHostConfig{NetworkMode: "none", ReadonlyRoot: true, CapDrop: []string{"ALL"}, SecurityOpt: []string{"no-new-privileges"}, PidsLimit: 64, Memory: memoryMB * 1024 * 1024, NanoCPUs: cpuMilli * 1_000_000, Tmpfs: map[string]string{"/tmp": "rw,noexec,nosuid,size=16m"}}
}

func (c *dockerEngineClient) Create(ctx context.Context, request dockerContainerCreateRequest) (string, error) {
	data, err := json.Marshal(request)
	if err != nil {
		return "", err
	}
	var response struct {
		ID string `json:"Id"`
	}
	if err := c.doJSON(ctx, http.MethodPost, "/containers/create?name=kognition-exec-probe-"+uuid.NewString(), data, &response); err != nil {
		return "", err
	}
	if response.ID == "" {
		return "", fmt.Errorf("Docker Engine returned an empty container ID")
	}
	return response.ID, nil
}

func (c *dockerEngineClient) Pull(ctx context.Context, image string) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://docker/"+dockerAPIVersion+"/images/create?fromImage="+url.QueryEscape(image), nil)
	if err != nil {
		return err
	}
	response, err := c.http.Do(request)
	if err != nil {
		return err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return dockerAPIError(response)
	}
	_, err = io.Copy(io.Discard, response.Body)
	return err
}

func (c *dockerEngineClient) Start(ctx context.Context, containerID string) error {
	return c.doJSON(ctx, http.MethodPost, "/containers/"+pathEscape(containerID)+"/start", nil, nil)
}

func (c *dockerEngineClient) Wait(ctx context.Context, containerID string) (int, error) {
	var response struct {
		StatusCode int `json:"StatusCode"`
	}
	if err := c.doJSON(ctx, http.MethodPost, "/containers/"+pathEscape(containerID)+"/wait?condition=not-running", nil, &response); err != nil {
		return 0, err
	}
	return response.StatusCode, nil
}

func (c *dockerEngineClient) Logs(ctx context.Context, containerID string) ([]byte, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://docker/"+dockerAPIVersion+"/containers/"+pathEscape(containerID)+"/logs?stdout=true&stderr=true", nil)
	if err != nil {
		return nil, err
	}
	response, err := c.http.Do(request)
	if err != nil {
		return nil, err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return nil, dockerAPIError(response)
	}
	return io.ReadAll(io.LimitReader(response.Body, maxExecProbeOutputBytes+1))
}

func (c *dockerEngineClient) Remove(ctx context.Context, containerID string) error {
	return c.doJSON(ctx, http.MethodDelete, "/containers/"+pathEscape(containerID)+"?force=true", nil, nil)
}

func (c *dockerEngineClient) Attach(ctx context.Context, containerID string) (io.ReadWriteCloser, error) {
	connection, err := (&net.Dialer{}).DialContext(ctx, "unix", c.socketPath)
	if err != nil {
		return nil, err
	}
	if deadline, ok := ctx.Deadline(); ok {
		_ = connection.SetDeadline(deadline)
	}
	request := fmt.Sprintf("POST /%s/containers/%s/attach?stream=1&stdin=1&stdout=1&stderr=1 HTTP/1.1\r\nHost: docker\r\nConnection: Upgrade\r\nUpgrade: tcp\r\nContent-Length: 0\r\n\r\n", dockerAPIVersion, pathEscape(containerID))
	if _, err := io.WriteString(connection, request); err != nil {
		_ = connection.Close()
		return nil, err
	}
	reader := bufio.NewReader(connection)
	status, err := reader.ReadString('\n')
	if err != nil {
		_ = connection.Close()
		return nil, err
	}
	if !strings.Contains(status, " 101 ") && !strings.Contains(status, " 200 ") {
		_ = connection.Close()
		return nil, fmt.Errorf("Docker Engine attach returned %s", strings.TrimSpace(status))
	}
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			_ = connection.Close()
			return nil, err
		}
		if line == "\r\n" {
			break
		}
	}
	_ = connection.SetDeadline(time.Time{})
	return &dockerAttachedStream{Conn: connection, reader: reader}, nil
}

type dockerAttachedStream struct {
	net.Conn
	reader    *bufio.Reader
	remaining uint32
}

func (s *dockerAttachedStream) Read(data []byte) (int, error) {
	for s.remaining == 0 {
		var header [8]byte
		if _, err := io.ReadFull(s.reader, header[:]); err != nil {
			return 0, err
		}
		s.remaining = uint32(header[4])<<24 | uint32(header[5])<<16 | uint32(header[6])<<8 | uint32(header[7])
	}
	if uint32(len(data)) > s.remaining {
		data = data[:s.remaining]
	}
	n, err := s.reader.Read(data)
	s.remaining -= uint32(n)
	return n, err
}

func truncateStdioOutput(output []byte) string {
	message := strings.TrimSpace(string(output))
	if len(message) > maxExecProbeOutputBytes {
		return message[:maxExecProbeOutputBytes] + "..."
	}
	return message
}

func (c *dockerEngineClient) doJSON(ctx context.Context, method, endpoint string, body []byte, output any) error {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	request, err := http.NewRequestWithContext(ctx, method, "http://docker/"+dockerAPIVersion+endpoint, reader)
	if err != nil {
		return err
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := c.http.Do(request)
	if err != nil {
		return err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return dockerAPIError(response)
	}
	if output == nil {
		return nil
	}
	return json.NewDecoder(response.Body).Decode(output)
}

func dockerAPIError(response *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(response.Body, maxExecProbeOutputBytes))
	message := strings.TrimSpace(string(body))
	if message == "" {
		return fmt.Errorf("Docker Engine returned HTTP %d", response.StatusCode)
	}
	return fmt.Errorf("Docker Engine returned HTTP %d: %s", response.StatusCode, message)
}

func pathEscape(value string) string {
	return strings.ReplaceAll(value, "/", "%2F")
}
