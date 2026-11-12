// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package v1alpha1

import (
	"strings"
	"testing"
)

func TestToolSpecValidateExecHealthProbeRequiresCommand(t *testing.T) {
	spec := ToolSpec{HealthProbe: &ToolHealthProbe{Type: ProbeTypeExec}}

	err := spec.Validate()
	if err == nil || !strings.Contains(err.Error(), "command is required") {
		t.Fatalf("Validate() error = %v, want required exec command", err)
	}
}

func TestToolSpecValidateExecHealthProbeAcceptsArgv(t *testing.T) {
	spec := ToolSpec{HealthProbe: &ToolHealthProbe{
		Type:    ProbeTypeExec,
		Command: []string{"healthcheck", "--mode", "ready"},
	}}

	if err := spec.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
}

func TestToolSpecValidateNonExecHealthProbeRejectsCommand(t *testing.T) {
	spec := ToolSpec{HealthProbe: &ToolHealthProbe{
		Type:    ProbeTypeHTTPGet,
		Command: []string{"healthcheck"},
	}}

	err := spec.Validate()
	if err == nil || !strings.Contains(err.Error(), "only valid when type is exec") {
		t.Fatalf("Validate() error = %v, want command restricted to exec probes", err)
	}
}

func TestToolSpecValidateExecHealthProbeRejectsEmptyArgument(t *testing.T) {
	spec := ToolSpec{HealthProbe: &ToolHealthProbe{
		Type:    ProbeTypeExec,
		Command: []string{"healthcheck", ""},
	}}

	err := spec.Validate()
	if err == nil || !strings.Contains(err.Error(), "command[1] must not be empty") {
		t.Fatalf("Validate() error = %v, want empty argument rejection", err)
	}
}
