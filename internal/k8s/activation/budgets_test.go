// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package activation_test

import (
	"math"
	"testing"
	"time"

	"github.com/kognition-ai/kognition/internal/k8s/activation"
)

func TestActivationTransactionBudgetTracksReviewedNamespaceInventory(t *testing.T) {
	for _, scenario := range []struct {
		namespaces int
		expected   time.Duration
	}{
		{namespaces: 0, expected: 5 * time.Minute},
		{namespaces: 1, expected: 5 * time.Minute},
		{namespaces: 7, expected: 11 * time.Minute},
		{namespaces: 16, expected: 20 * time.Minute},
		{namespaces: math.MaxInt, expected: 20 * time.Minute},
	} {
		if got := activation.TransactionTimeout(scenario.namespaces); got != scenario.expected {
			t.Fatalf("%d reviewed namespaces: budget %s, expected %s", scenario.namespaces, got, scenario.expected)
		}
	}
}
