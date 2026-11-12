// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package installation

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

type recordedOperations struct {
	calls []string
	fail  string
}

func (operations *recordedOperations) call(name string) error {
	operations.calls = append(operations.calls, name)
	if operations.fail == name {
		return errors.New("injected failure")
	}
	return nil
}

func (operations *recordedOperations) Preflight(context.Context) error {
	return operations.call("preflight")
}
func (operations *recordedOperations) WorkloadsDrained(context.Context) error {
	return operations.call("workloads-drained")
}
func (operations *recordedOperations) StopControllers(context.Context) error {
	return operations.call("stop-controllers")
}
func (operations *recordedOperations) ControllersStopped(context.Context) error {
	return operations.call("controllers-stopped")
}
func (operations *recordedOperations) RevokeGrants(context.Context) error {
	return operations.call("revoke-grants")
}
func (operations *recordedOperations) VerifyDrained(context.Context) error {
	return operations.call("verify-drained")
}
func (operations *recordedOperations) RemoveLegacyPolicies(context.Context) error {
	return operations.call("remove-legacy-policies")
}
func (operations *recordedOperations) RemoveCompletedMigrations(context.Context) error {
	return operations.call("remove-completed-migrations")
}
func (operations *recordedOperations) Bootstrap(context.Context) error {
	return operations.call("admission-bootstrap")
}
func (operations *recordedOperations) VerifyBootstrap(context.Context) error {
	return operations.call("verify-bootstrap")
}

type recordedJournal struct {
	stages []string
	fail   string
}

func (journal *recordedJournal) Record(stage string) error {
	journal.stages = append(journal.stages, stage)
	if stage == journal.fail {
		return errors.New("journal unavailable")
	}
	return nil
}

func TestBootstrapOrderingAndFailures(test *testing.T) {
	expected := []string{"preflight", "workloads-drained", "stop-controllers", "controllers-stopped", "revoke-grants", "verify-drained", "remove-legacy-policies", "remove-completed-migrations", "admission-bootstrap", "verify-bootstrap"}
	for _, failure := range append([]string{""}, expected...) {
		test.Run("failure="+failure, func(test *testing.T) {
			operations, journal := &recordedOperations{fail: failure}, &recordedJournal{}
			err := Bootstrap(context.Background(), operations, journal, true)
			if (err == nil) != (failure == "") {
				test.Fatalf("unexpected result: %v", err)
			}
			limit := len(expected)
			for index, name := range expected {
				if name == failure {
					limit = index + 1
				}
			}
			if !reflect.DeepEqual(operations.calls, expected[:limit]) {
				test.Fatalf("unsafe order: %v", operations.calls)
			}
			for index, call := range operations.calls {
				if journal.stages[index*2] != call+":pending" {
					test.Fatal("operation not journaled before execution")
				}
			}
			if failure != "" && !strings.Contains(err.Error(), "keep writers frozen") {
				test.Fatal(err)
			}
		})
	}
}

func TestBootstrapNoEffectWithoutFreezeOrJournal(test *testing.T) {
	operations := &recordedOperations{}
	if err := Bootstrap(context.Background(), operations, &recordedJournal{}, false); err == nil || len(operations.calls) != 0 {
		test.Fatal("missing freeze had effects")
	}
	journal := &recordedJournal{fail: "stop-controllers:pending"}
	if err := Bootstrap(context.Background(), operations, journal, true); err == nil || !reflect.DeepEqual(operations.calls, []string{"preflight", "workloads-drained"}) {
		test.Fatalf("unjournaled mutation: %v %v", operations.calls, err)
	}
}
