// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package jobtransport implements the collaboration driver's Transport over the
// existing AgentJob execution path. Every member dispatch is one AgentJob, so it
// reuses the agent dispatcher, the runtime credentials, the model gateway and the
// token metering that already run workflow steps. A dispatch is mapped to an A2A
// task state so the A2A task semantics (working, completed, failed, canceled)
// and the member-output parsing are shared with the A2A transport.
package jobtransport

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"

	api "github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/collaboration"
	"github.com/kognition-ai/kognition/internal/collaboration/a2atransport"
	"github.com/kognition-ai/kognition/internal/collaboration/driver"
	"github.com/kognition-ai/kognition/internal/controller"
	"github.com/kognition-ai/kognition/internal/store"
)

// Labels and annotations applied to every collaboration AgentJob. The jobs carry no
// workflow-step label, so the workflow reconciler never mistakes them for step jobs.
const (
	LabelComponent     = "app.kubernetes.io/component"
	ComponentValue     = "workflow-collaboration"
	LabelMember        = "kognition.dev/collaboration-member"
	AnnotationDispatch = controller.CollaborationDispatchAnnotation
)

// Authorizer reports whether sourceAgent may send work to targetAgent in namespace.
// It is the AgentCommunicationPolicy check that already guards workflow steps.
type Authorizer func(ctx context.Context, namespace, sourceAgent, targetAgent string) (bool, error)

// Config configures a Transport for one collaboration activation.
type Config struct {
	Store store.Store
	// Run identifies the activation. Run.OwnerKind, RunName and RunUID own every job.
	Run controller.WorkflowCollaborationRun
	// Authorize is optional. It is evaluated for a dispatch requested by a member.
	Authorize Authorizer
	Now       func() time.Time
}

// Transport implements driver.Transport.
type Transport struct {
	cfg     Config
	members map[string]string
	refused map[string]string
}

var _ driver.Transport = (*Transport)(nil)

// New validates the configuration.
func New(cfg Config) (*Transport, error) {
	run := cfg.Run
	if cfg.Store == nil || run.Spec == nil || run.Namespace == "" || run.RunName == "" || run.RunUID == "" || run.OwnerKind == "" {
		return nil, errors.New("jobtransport: store, run identity, owner kind and collaboration spec are required")
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	t := &Transport{cfg: cfg, members: map[string]string{}, refused: map[string]string{}}
	for _, m := range run.Spec.Members {
		switch {
		case m.AgentRef.Namespace != "" && m.AgentRef.Namespace != run.Namespace:
			t.refused[m.Name] = "members must be Agents in the Workflow namespace"
		case m.ServiceRef != nil:
			t.refused[m.Name] = "serviceRef is not supported"
		default:
			t.members[m.Name] = m.AgentRef.Name
		}
	}
	return t, nil
}

func refused(format string, args ...any) error {
	return fmt.Errorf("%w: %s", driver.ErrRefused, fmt.Sprintf(format, args...))
}

// JobName derives the deterministic AgentJob name of one dispatch.
func JobName(run controller.WorkflowCollaborationRun, dispatchID string) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%s\x00%s\x00%d\x00%s", run.Namespace, run.RunUID, run.Step, run.Attempt, dispatchID)))
	return "wfcj-" + hex.EncodeToString(sum[:12])
}

// Admit implements driver.Transport.
func (t *Transport) Admit(ctx context.Context, msg collaboration.Message) error {
	if reason, bad := t.refused[msg.Recipient]; bad {
		return refused("member %q: %s", msg.Recipient, reason)
	}
	target, ok := t.members[msg.Recipient]
	if !ok {
		return refused("unknown member %q", msg.Recipient)
	}
	if msg.Requester == "" || t.cfg.Authorize == nil {
		return nil
	}
	source, ok := t.members[msg.Requester]
	if !ok {
		return refused("unknown requesting member %q", msg.Requester)
	}
	allowed, err := t.cfg.Authorize(ctx, t.cfg.Run.Namespace, source, target)
	if err != nil {
		return err
	}
	if !allowed {
		return refused("communication policy denies %s to %s", source, target)
	}
	return nil
}

// Send implements driver.Transport. A Cancel deletes the AgentJob of the dispatch it
// refers to; every other kind creates one AgentJob, once.
func (t *Transport) Send(ctx context.Context, msg collaboration.Message) error {
	run := t.cfg.Run
	if msg.Kind == collaboration.DispatchCancel {
		if msg.Target == nil {
			return errors.New("jobtransport: cancel has no target dispatch")
		}
		err := t.cfg.Store.Delete(ctx, "AgentJob", run.Namespace, JobName(run, msg.Target.DispatchID))
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		return err
	}
	agent, ok := t.members[msg.Recipient]
	if !ok {
		return fmt.Errorf("jobtransport: unknown member %q", msg.Recipient)
	}
	name := JobName(run, msg.DispatchID)
	job := api.AgentJob{
		TypeMeta: api.TypeMeta{APIVersion: api.APIVersion, Kind: "AgentJob"},
		ObjectMeta: api.ObjectMeta{
			Name:              name,
			Namespace:         run.Namespace,
			CreationTimestamp: t.cfg.Now().UTC(),
			Labels:            map[string]string{LabelComponent: ComponentValue, LabelMember: msg.Recipient},
			Annotations:       map[string]string{AnnotationDispatch: msg.DispatchID},
			OwnerReferences: []api.OwnerReference{{
				APIVersion: api.APIVersion, Kind: run.OwnerKind, Name: run.RunName, UID: run.RunUID,
			}},
		},
		Spec:   api.AgentJobSpec{AgentRef: &api.LocalObjectReference{Name: agent}, Input: a2atransport.Prompt(msg, t.reachable(msg.Recipient))},
		Status: api.AgentJobStatus{Phase: api.PhasePending},
	}
	data, err := json.Marshal(job)
	if err != nil {
		return fmt.Errorf("jobtransport: marshal job: %w", err)
	}
	if err := t.cfg.Store.Create(ctx, "AgentJob", run.Namespace, name, data); err != nil && !errors.Is(err, store.ErrAlreadyExists) {
		return err
	}
	return nil
}

// reachable returns the sorted names of the members that self may involve, as
// declared by the step's allowedInteractions. The engine enforces the same set.
func (t *Transport) reachable(self string) []string {
	return a2atransport.Reachable(t.cfg.Run.Spec.AllowedInteractions, self)
}

// Observe implements driver.Transport.
func (t *Transport) Observe(ctx context.Context, msg collaboration.Message) (driver.Observation, error) {
	run := t.cfg.Run
	dispatch := msg.DispatchID
	if msg.Kind == collaboration.DispatchCancel {
		if msg.Target == nil {
			return driver.Observation{}, errors.New("jobtransport: cancel has no target dispatch")
		}
		dispatch = msg.Target.DispatchID
	}
	raw, err := t.cfg.Store.Get(ctx, "AgentJob", run.Namespace, JobName(run, dispatch))
	if errors.Is(err, store.ErrNotFound) {
		if msg.Kind == collaboration.DispatchCancel {
			return driver.Observation{Settled: true, Actor: msg.Recipient, Status: collaboration.ReportCancelled}, nil
		}
		return driver.Observation{}, nil
	}
	if err != nil {
		return driver.Observation{}, err
	}
	var job api.AgentJob
	if err := json.Unmarshal(raw, &job); err != nil {
		return driver.Observation{}, fmt.Errorf("jobtransport: decode job: %w", err)
	}
	// The AgentJob cleanup finalizer keeps a deleted job visible while its runtime is torn down.
	if msg.Kind == collaboration.DispatchCancel && job.ObjectMeta.DeletionTimestamp != nil {
		return driver.Observation{Settled: true, Actor: msg.Recipient, Status: collaboration.ReportCancelled}, nil
	}
	return a2atransport.Interpret(msg, msg.Recipient, taskOf(job)), nil
}

// taskOf reads an AgentJob as the A2A task it stands for.
func taskOf(job api.AgentJob) *a2a.Task {
	task := &a2a.Task{ID: a2a.TaskID(job.ObjectMeta.Name)}
	switch job.Status.Phase {
	case api.PhaseReady:
		task.Status.State = a2a.TaskStateCompleted
		task.Artifacts = []*a2a.Artifact{{Parts: []*a2a.Part{a2a.NewTextPart(job.Status.Output)}}}
		if usage := job.Status.TokenUsage; usage != nil && usage.TotalTokens > 0 {
			task.Artifacts = append(task.Artifacts, &a2a.Artifact{
				Metadata: map[string]any{collaboration.UsageMetadataKey: map[string]any{"totalTokens": float64(usage.TotalTokens)}},
			})
		}
	case api.PhaseFailed, api.PhaseTimedOut, api.PhaseTerminated:
		task.Status.State = a2a.TaskStateFailed
	default:
		task.Status.State = a2a.TaskStateWorking
	}
	return task
}

// Factory builds one Transport per activation. It implements runner.TransportFactory.
type Factory struct {
	Store     store.Store
	Authorize Authorizer
}

// Transport implements runner.TransportFactory. The durable handle store is not
// needed: a dispatch's AgentJob name is derived from the dispatch itself.
func (f Factory) Transport(_ context.Context, run controller.WorkflowCollaborationRun, _ a2atransport.Handles) (driver.Transport, error) {
	return New(Config{Store: f.Store, Run: run, Authorize: f.Authorize})
}
