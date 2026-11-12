// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

func assertRetainedWorkflowGauge(t *testing.T, registry prometheus.Gatherer, name string, labels map[string]string, want float64) {
	t.Helper()
	families, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, family := range families {
		if family.GetName() != name {
			continue
		}
		for _, metric := range family.Metric {
			matched := 0
			for _, label := range metric.Label {
				if value, exists := labels[label.GetName()]; exists && value == label.GetValue() {
					matched++
				}
			}
			if matched == len(labels) {
				if metric.GetGauge().GetValue() != want {
					t.Fatalf("%s%v = %v, want %v", name, labels, metric.GetGauge().GetValue(), want)
				}
				return
			}
		}
	}
	t.Fatalf("missing %s%v", name, labels)
}
