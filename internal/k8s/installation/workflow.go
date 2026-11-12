// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package installation

import (
	"context"
	"errors"
	"fmt"
)

type Operations interface {
	Preflight(context.Context) error
	WorkloadsDrained(context.Context) error
	StopControllers(context.Context) error
	ControllersStopped(context.Context) error
	RevokeGrants(context.Context) error
	VerifyDrained(context.Context) error
	RemoveLegacyPolicies(context.Context) error
	RemoveCompletedMigrations(context.Context) error
	Bootstrap(context.Context) error
	VerifyBootstrap(context.Context) error
}

type Recorder interface {
	Record(stage string) error
}

func Bootstrap(ctx context.Context, operations Operations, recorder Recorder, frozen bool) error {
	if !frozen || operations == nil || recorder == nil {
		return errors.New("explicit writer freeze, operations and durable journal are required")
	}
	steps := []struct {
		name string
		run  func(context.Context) error
	}{
		{"preflight", operations.Preflight},
		{"workloads-drained", operations.WorkloadsDrained},
		{"stop-controllers", operations.StopControllers},
		{"controllers-stopped", operations.ControllersStopped},
		{"revoke-grants", operations.RevokeGrants},
		{"verify-drained", operations.VerifyDrained},
		{"remove-legacy-policies", operations.RemoveLegacyPolicies},
		{"remove-completed-migrations", operations.RemoveCompletedMigrations},
		{"admission-bootstrap", operations.Bootstrap},
		{"verify-bootstrap", operations.VerifyBootstrap},
	}
	for _, step := range steps {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := recorder.Record(step.name + ":pending"); err != nil {
			return err
		}
		if err := step.run(ctx); err != nil {
			return errors.Join(fmt.Errorf("%s: %w; keep writers frozen; no rollback or activation was attempted", step.name, err), recorder.Record(step.name+":failed"))
		}
		if err := recorder.Record(step.name + ":complete"); err != nil {
			return err
		}
	}
	return recorder.Record("verified-inactive")
}
