// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cpi

import (
	"context"
	"math"
	"slices"
	"strings"
	"testing"

	"github.com/kognition-ai/kognition/api/v1alpha1"
)

func TestNormalizeCompletionUsage(t *testing.T) {
	total := 3
	usage, valid := NormalizeCompletionUsage(2, 1, &total)
	if !valid || !usage.Reported || usage.TotalTokens != 3 {
		t.Fatalf("valid usage = (%+v, %t)", usage, valid)
	}
	usage, valid = NormalizeCompletionUsage(2, 1, nil)
	if !valid || usage.TotalTokens != 3 {
		t.Fatalf("derived usage = (%+v, %t)", usage, valid)
	}
	for _, test := range []struct {
		prompt     int
		completion int
		total      *int
	}{
		{prompt: -1},
		{prompt: math.MaxInt, completion: 1},
		{prompt: 2, completion: 1, total: intPtr(0)},
		{prompt: 2, completion: 1, total: intPtr(1)},
	} {
		if usage, valid := NormalizeCompletionUsage(test.prompt, test.completion, test.total); valid || usage != (Usage{}) {
			t.Fatalf("invalid usage %+v = (%+v, %t)", test, usage, valid)
		}
	}
}

func intPtr(value int) *int { return &value }

type registryTestDriver struct{}

func (*registryTestDriver) Name() string { return "openai-compatible" }

func (*registryTestDriver) Validate(context.Context, *v1alpha1.ModelSpec, *v1alpha1.ModelClassSpec) error {
	return nil
}

func (*registryTestDriver) Probe(context.Context, *v1alpha1.ModelSpec, *Credentials) (*HealthResult, error) {
	return nil, nil
}

func (*registryTestDriver) Complete(context.Context, *CompletionRequest) (*CompletionResponse, error) {
	return nil, nil
}

func (*registryTestDriver) Embed(context.Context, *EmbedRequest) (*EmbedResponse, error) {
	return nil, nil
}

type namedRegistryTestDriver struct{ name string }

func (d *namedRegistryTestDriver) Name() string { return d.name }
func (*namedRegistryTestDriver) Validate(context.Context, *v1alpha1.ModelSpec, *v1alpha1.ModelClassSpec) error {
	return nil
}
func (*namedRegistryTestDriver) Probe(context.Context, *v1alpha1.ModelSpec, *Credentials) (*HealthResult, error) {
	return nil, nil
}
func (*namedRegistryTestDriver) Complete(context.Context, *CompletionRequest) (*CompletionResponse, error) {
	return nil, nil
}
func (*namedRegistryTestDriver) Embed(context.Context, *EmbedRequest) (*EmbedResponse, error) {
	return nil, nil
}

func TestRegistryGetCanonicalizesProvisionerAliases(t *testing.T) {
	registry := NewRegistry()
	driver := &registryTestDriver{}
	registry.MustRegister(driver)

	for _, provisioner := range []string{"openai-compatible", "openai", "azure-openai"} {
		got, ok := registry.Get(provisioner)
		if !ok || got != driver {
			t.Errorf("Get(%q) = (%v, %v), want registered canonical driver", provisioner, got, ok)
		}
	}
	if _, ok := registry.Get("anthropic"); ok {
		t.Fatal("Get(anthropic) unexpectedly resolved an unregistered driver")
	}
}

func TestRegistryGetPrefersExactProvisionerRegistration(t *testing.T) {
	registry := NewRegistry()
	canonical := &registryTestDriver{}
	exact := &namedRegistryTestDriver{name: "openai"}
	registry.MustRegister(canonical)
	registry.MustRegister(exact)

	got, ok := registry.Get("openai")
	if !ok || got != exact {
		t.Fatalf("Get(openai) = (%v, %v), want exact registered driver", got, ok)
	}
	got, ok = registry.Get("azure-openai")
	if !ok || got != canonical {
		t.Fatalf("Get(azure-openai) = (%v, %v), want canonical fallback driver", got, ok)
	}
}

func TestRegistryRejectsDuplicatesAndListsDrivers(t *testing.T) {
	registry := NewRegistry()
	registry.MustRegister(&namedRegistryTestDriver{name: "zeta"})
	registry.MustRegister(&namedRegistryTestDriver{name: "alpha"})

	names := registry.List()
	if len(names) != 2 || !slices.Contains(names, "alpha") || !slices.Contains(names, "zeta") {
		t.Fatalf("List() = %v, want alpha and zeta", names)
	}
	if err := registry.Register(&namedRegistryTestDriver{name: "alpha"}); err == nil || !strings.Contains(err.Error(), "already registered") {
		t.Fatalf("duplicate Register() error = %v", err)
	}
}

func TestRegistryMustRegisterPanicsOnDuplicate(t *testing.T) {
	registry := NewRegistry()
	registry.MustRegister(&namedRegistryTestDriver{name: "duplicate"})
	defer func() {
		if recover() == nil {
			t.Fatal("MustRegister() did not panic on duplicate driver")
		}
	}()
	registry.MustRegister(&namedRegistryTestDriver{name: "duplicate"})
}
