// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package ingestion

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"go.uber.org/zap"

	api "github.com/kognition-ai/kognition/api/v1alpha1"
	intctrl "github.com/kognition-ai/kognition/internal/controller"
	"github.com/kognition-ai/kognition/internal/store"
)

const maxResponseBytes = 64 * 1024

type taskPayloadBuilder interface {
	Build(context.Context, *api.KnowledgeBase, *api.KnowledgeBaseClass) (map[string]any, []EnvVar, error)
}

type HTTPLauncher struct {
	endpoint  string
	client    *http.Client
	builder   taskPayloadBuilder
	logger    *zap.Logger
	authToken string
}

type taskRequest struct {
	ID          string            `json:"id"`
	Payload     map[string]any    `json:"payload"`
	Environment map[string]string `json:"environment,omitempty"`
}

type taskStatus struct {
	ID           string `json:"id"`
	State        string `json:"state"`
	Documents    int64  `json:"documents"`
	Chunks       int64  `json:"chunks"`
	SourceDigest string `json:"sourceDigest"`
	Error        string `json:"error"`
}

var errTaskNotFound = errors.New("ingestion task not found")

func NewHTTPLauncher(st store.Store, endpoint string, client *http.Client, logger *zap.Logger) *HTTPLauncher {
	if client == nil {
		// Never follow redirects: the bearer token and task payload must reach only the configured endpoint.
		client = &http.Client{
			Timeout:       15 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		}
	}
	if logger == nil {
		logger = zap.NewNop()
	}
	return &HTTPLauncher{
		endpoint: strings.TrimRight(endpoint, "/"),
		client:   client,
		builder:  NewPayloadBuilder(st, nil),
		logger:   logger.With(zap.String("component", "standalone-kb-ingester")),
	}
}

// WithBearerToken makes the launcher authenticate every request to the ingester.
func (l *HTTPLauncher) WithBearerToken(token string) *HTTPLauncher {
	l.authToken = strings.TrimSpace(token)
	return l
}

func (l *HTTPLauncher) authorize(request *http.Request) {
	if l.authToken != "" {
		request.Header.Set("Authorization", "Bearer "+l.authToken)
	}
}

func (l *HTTPLauncher) Ensure(ctx context.Context, kb *api.KnowledgeBase, kbc *api.KnowledgeBaseClass) (intctrl.IngestState, intctrl.IngestStats, error) {
	taskID := knowledgeBaseTaskID(kb)
	status, err := l.getOrSubmit(ctx, taskID, kb, kbc)
	if err != nil {
		return intctrl.IngestSkipped, intctrl.IngestStats{}, err
	}
	return stateFromTask(status)
}

func (l *HTTPLauncher) Reset(ctx context.Context, kb *api.KnowledgeBase) error {
	return l.deleteTask(ctx, knowledgeBaseTaskID(kb))
}

func (l *HTTPLauncher) LaunchRun(
	ctx context.Context,
	kb *api.KnowledgeBase,
	kbc *api.KnowledgeBaseClass,
	run *api.KnowledgeBaseIndexRun,
) (intctrl.IngestState, intctrl.IngestStats, string, error) {
	taskID := indexRunTaskID(run)
	status, err := l.getOrSubmit(ctx, taskID, kb, kbc)
	if err != nil {
		return intctrl.IngestSkipped, intctrl.IngestStats{}, taskID, err
	}
	state, stats, err := stateFromTask(status)
	return state, stats, taskID, err
}

func (l *HTTPLauncher) getOrSubmit(
	ctx context.Context,
	taskID string,
	kb *api.KnowledgeBase,
	kbc *api.KnowledgeBaseClass,
) (*taskStatus, error) {
	status, err := l.getTask(ctx, taskID)
	if err == nil {
		return status, nil
	}
	if !errors.Is(err, errTaskNotFound) {
		return nil, err
	}
	if kbc == nil {
		return nil, fmt.Errorf("cannot submit ingestion task after KnowledgeBaseClass deletion")
	}
	payload, environment, err := l.builder.Build(ctx, kb, kbc)
	if err != nil {
		return &taskStatus{ID: taskID, State: "failed", Error: err.Error()}, nil
	}
	environmentMap := make(map[string]string, len(environment))
	for _, variable := range environment {
		environmentMap[variable.Name] = variable.Value
	}
	requestBody, err := json.Marshal(taskRequest{ID: taskID, Payload: payload, Environment: environmentMap})
	if err != nil {
		return nil, fmt.Errorf("marshal ingestion task: %w", err)
	}
	if len(requestBody) > InputMaxSize {
		return &taskStatus{ID: taskID, State: "failed", Error: fmt.Sprintf("ingest input exceeds %d bytes", InputMaxSize)}, nil
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, l.endpoint+"/v1/tasks", bytes.NewReader(requestBody))
	if err != nil {
		return nil, fmt.Errorf("create ingestion submit request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	l.authorize(request)
	response, err := l.client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("submit ingestion task: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusAccepted && response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("submit ingestion task: service returned %s", response.Status)
	}
	status, err = decodeTaskStatus(response.Body)
	if err != nil {
		return nil, fmt.Errorf("decode ingestion submit response: %w", err)
	}
	l.logger.Info("submitted standalone knowledge-base ingestion", zap.String("task", taskID), zap.String("namespace", kb.ObjectMeta.Namespace), zap.String("knowledge-base", kb.ObjectMeta.Name))
	return status, nil
}

func (l *HTTPLauncher) getTask(ctx context.Context, taskID string) (*taskStatus, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, l.taskURL(taskID), nil)
	if err != nil {
		return nil, fmt.Errorf("create ingestion status request: %w", err)
	}
	l.authorize(request)
	response, err := l.client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("get ingestion task: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode == http.StatusNotFound {
		return nil, errTaskNotFound
	}
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("get ingestion task: service returned %s", response.Status)
	}
	status, err := decodeTaskStatus(response.Body)
	if err != nil {
		return nil, fmt.Errorf("decode ingestion status response: %w", err)
	}
	return status, nil
}

func (l *HTTPLauncher) deleteTask(ctx context.Context, taskID string) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodDelete, l.taskURL(taskID), nil)
	if err != nil {
		return fmt.Errorf("create ingestion reset request: %w", err)
	}
	l.authorize(request)
	response, err := l.client.Do(request)
	if err != nil {
		return fmt.Errorf("reset ingestion task: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusNoContent && response.StatusCode != http.StatusNotFound {
		return fmt.Errorf("reset ingestion task: service returned %s", response.Status)
	}
	return nil
}

func (l *HTTPLauncher) taskURL(taskID string) string {
	return l.endpoint + "/v1/tasks/" + url.PathEscape(taskID)
}

func decodeTaskStatus(reader io.Reader) (*taskStatus, error) {
	var status taskStatus
	decoder := json.NewDecoder(io.LimitReader(reader, maxResponseBytes))
	if err := decoder.Decode(&status); err != nil {
		return nil, err
	}
	if status.ID == "" || status.State == "" {
		return nil, errors.New("task response is missing id or state")
	}
	return &status, nil
}

func stateFromTask(status *taskStatus) (intctrl.IngestState, intctrl.IngestStats, error) {
	stats := intctrl.IngestStats{
		Documents:    status.Documents,
		Chunks:       status.Chunks,
		SourceDigest: status.SourceDigest,
		Error:        status.Error,
	}
	switch status.State {
	case "running":
		return intctrl.IngestRunning, stats, nil
	case "succeeded":
		return intctrl.IngestSucceeded, stats, nil
	case "failed":
		if stats.Error == "" {
			stats.Error = "ingestion task failed"
		}
		return intctrl.IngestFailed, stats, nil
	default:
		return intctrl.IngestSkipped, intctrl.IngestStats{}, fmt.Errorf("ingestion task %q returned unknown state %q", status.ID, status.State)
	}
}

func knowledgeBaseTaskID(kb *api.KnowledgeBase) string {
	return hashedTaskID("kb", kb.ObjectMeta.Namespace, kb.ObjectMeta.Name, fmt.Sprint(kb.Status.Generation+1))
}

func indexRunTaskID(run *api.KnowledgeBaseIndexRun) string {
	identity := run.ObjectMeta.UID
	if identity == "" {
		identity = run.ObjectMeta.Namespace + "/" + run.ObjectMeta.Name
	}
	return hashedTaskID("run", identity)
}

func hashedTaskID(parts ...string) string {
	hash := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return "ingest-" + hex.EncodeToString(hash[:12])
}
