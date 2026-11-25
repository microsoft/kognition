// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package runner adapts the collaboration driver to the workflow controller. It
// keeps one durable ConfigMap ledger per workflow run, step and attempt, and
// resumes it on every reconcile pass; all member contact goes through the
// injected governed Transport factory.
package runner

import (
	"context"
	"errors"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"

	"github.com/kognition-ai/kognition/internal/collaboration"
	"github.com/kognition-ai/kognition/internal/collaboration/a2atransport"
	"github.com/kognition-ai/kognition/internal/collaboration/driver"
	"github.com/kognition-ai/kognition/internal/collaboration/driver/k8sledger"
	"github.com/kognition-ai/kognition/internal/controller"
)

// Label keys applied to every ledger object.
const (
	LabelComponent = collaboration.LabelComponent
	LabelRun       = "kognition.dev/collaboration-run"
	LabelStep      = "kognition.dev/collaboration-step"
	ComponentValue = collaboration.ComponentValue
)

// TransportFactory builds the governed transport of one activation. handles is
// the activation's durable remote-task handle store.
type TransportFactory interface {
	Transport(ctx context.Context, run controller.WorkflowCollaborationRun, handles a2atransport.Handles) (driver.Transport, error)
}

// Config configures a Runner.
type Config struct {
	// ConfigMaps returns the ConfigMap client of a namespace.
	ConfigMaps func(namespace string) corev1client.ConfigMapInterface
	Transports TransportFactory
	// Owners optionally returns the owner references that garbage-collect a ledger.
	Owners func(run controller.WorkflowCollaborationRun) []metav1.OwnerReference
}

// Runner implements controller.WorkflowCollaborationRunner.
type Runner struct{ cfg Config }

var _ controller.WorkflowCollaborationRunner = (*Runner)(nil)

// New validates the configuration.
func New(cfg Config) (*Runner, error) {
	if cfg.ConfigMaps == nil || cfg.Transports == nil {
		return nil, errors.New("runner: ConfigMap clients and a transport factory are required")
	}
	return &Runner{cfg: cfg}, nil
}

// LedgerName returns the ledger ConfigMap name of one step attempt.
func LedgerName(run controller.WorkflowCollaborationRun) string {
	return collaboration.LedgerName(run.Namespace, run.RunUID, run.RunName, run.Step, run.Attempt)
}

func hashLabel(s string) string { return collaboration.LabelHash(s) }

func (r *Runner) assemble(ctx context.Context, run controller.WorkflowCollaborationRun) (*driver.Driver, error) {
	if run.Spec == nil {
		return nil, errors.New("runner: collaboration spec is required")
	}
	def, err := collaboration.NewDefinition(run.Spec)
	if err != nil {
		return nil, err
	}
	engine, err := collaboration.New(def)
	if err != nil {
		return nil, err
	}
	cfg := k8sledger.Config{
		ConfigMaps: r.cfg.ConfigMaps(run.Namespace),
		Name:       LedgerName(run),
		Labels: map[string]string{
			LabelComponent: ComponentValue,
			LabelRun:       hashLabel(run.RunUID),
			LabelStep:      hashLabel(run.Step),
		},
	}
	if r.cfg.Owners != nil {
		cfg.Owners = r.cfg.Owners(run)
	}
	ledger, err := k8sledger.NewLedger(cfg)
	if err != nil {
		return nil, err
	}
	handles, err := k8sledger.NewHandles(cfg)
	if err != nil {
		return nil, err
	}
	transport, err := r.cfg.Transports.Transport(ctx, run, handles)
	if err != nil {
		return nil, fmt.Errorf("runner: build transport: %w", err)
	}
	return driver.New(engine, ledger, transport)
}

// Advance implements controller.WorkflowCollaborationRunner.
func (r *Runner) Advance(ctx context.Context, run controller.WorkflowCollaborationRun) (controller.WorkflowCollaborationProgress, error) {
	d, err := r.assemble(ctx, run)
	if err != nil {
		return controller.WorkflowCollaborationProgress{}, err
	}
	if err := d.Begin(ctx, run.Input); err != nil {
		return controller.WorkflowCollaborationProgress{}, err
	}
	st, done, err := d.Step(ctx)
	if err != nil {
		return controller.WorkflowCollaborationProgress{}, err
	}
	if !done {
		return controller.WorkflowCollaborationProgress{}, nil
	}
	return progress(st), nil
}

// Stop implements controller.WorkflowCollaborationRunner. A step that never
// started has no ledger and needs no stop.
func (r *Runner) Stop(ctx context.Context, run controller.WorkflowCollaborationRun, reason string) error {
	d, err := r.assemble(ctx, run)
	if err != nil {
		return err
	}
	if _, err := d.Stop(ctx, collaboration.PhaseCancelled, reason); err != nil {
		if errors.Is(err, driver.ErrNotFound) {
			return nil
		}
		return err
	}
	if _, _, err := d.Step(ctx); err != nil {
		return err
	}
	return nil
}

func progress(st collaboration.State) controller.WorkflowCollaborationProgress {
	if st.Phase == collaboration.PhaseSucceeded {
		out := ""
		if res := st.Result(); res != nil {
			out = res.Text
		}
		return controller.WorkflowCollaborationProgress{Done: true, Succeeded: true, Output: out}
	}
	reason := string(st.Phase)
	if st.Outcome != nil && st.Outcome.Reason != "" {
		reason = st.Outcome.Reason
	}
	return controller.WorkflowCollaborationProgress{Done: true, Reason: reason}
}
