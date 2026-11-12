// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"reflect"
	"testing"

	"go.uber.org/zap"
)

func TestManagerNamespaceOptions(test *testing.T) {
	for _, scenario := range []struct {
		name, controller, managed, agent string
		expected                         []string
	}{
		{name: "release only", controller: "control", expected: []string{"control"}},
		{name: "explicit with runtime", controller: " control ", managed: " tenant , other ", agent: " runtime ", expected: []string{"control", "tenant", "other", "runtime"}},
		{name: "missing controller remains invalid", expected: []string{""}},
		{name: "empty entry remains invalid", controller: "control", managed: "tenant,", expected: []string{"control", "tenant", ""}},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			test.Setenv("KOGNITION_CONTROLLER_NAMESPACE", scenario.controller)
			test.Setenv("KOGNITION_MANAGED_NAMESPACES", scenario.managed)
			test.Setenv("KOGNITION_AGENT_NAMESPACE", scenario.agent)
			options := managerOptions(controllerOptions{}, zap.NewNop())
			if !reflect.DeepEqual(options.ManagedNamespaces, scenario.expected) {
				test.Fatalf("managed namespaces = %q, want %q", options.ManagedNamespaces, scenario.expected)
			}
		})
	}
}
