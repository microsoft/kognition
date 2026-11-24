// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cpi

import (
	"fmt"
	"sync"
)

// Registry holds registered CPI drivers keyed by provisioner name.
// It is safe for concurrent use.
type Registry struct {
	mu      sync.RWMutex
	drivers map[string]Driver
	// observed holds recorder-wrapped views of drivers; it is nil until Observe
	// is called with a recorder.
	observed map[string]Driver
	recorder CallRecorder
}

// NewRegistry creates an empty driver registry.
func NewRegistry() *Registry {
	return &Registry{
		drivers: make(map[string]Driver),
	}
}

// Observe reports the outcome of every Complete and Embed call made through
// drivers returned by Get to recorder, including drivers registered later.
// Calling it again replaces the recorder; a nil recorder stops observation.
func (r *Registry) Observe(recorder CallRecorder) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.recorder = recorder
	r.observed = nil
	if recorder == nil {
		return
	}
	r.observed = make(map[string]Driver, len(r.drivers))
	for name, driver := range r.drivers {
		r.observed[name] = Observe(driver, recorder)
	}
}

// Observed reports whether a recorder is currently observing the registry's drivers.
func (r *Registry) Observed() bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.observed != nil
}

// Register adds a driver to the registry. Returns an error if a driver
// with the same provisioner name is already registered.
func (r *Registry) Register(d Driver) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	name := d.Name()
	if _, exists := r.drivers[name]; exists {
		return fmt.Errorf("cpi: driver %q already registered", name)
	}
	r.drivers[name] = d
	if r.observed != nil {
		r.observed[name] = Observe(d, r.recorder)
	}
	return nil
}

// MustRegister adds a driver to the registry, panicking on conflict.
// Use during init() for built-in drivers.
func (r *Registry) MustRegister(d Driver) {
	if err := r.Register(d); err != nil {
		panic(err)
	}
}

// Get returns the driver for the given provisioner name.
func (r *Registry) Get(provisioner string) (Driver, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	drivers := r.drivers
	if r.observed != nil {
		drivers = r.observed
	}
	d, ok := drivers[provisioner]
	if !ok {
		d, ok = drivers[CanonicalProvisioner(provisioner)]
	}
	return d, ok
}

// CanonicalProvisioner maps supported aliases to their registered CPI driver name.
func CanonicalProvisioner(provisioner string) string {
	switch provisioner {
	case "azure-openai", "openai":
		return "openai-compatible"
	default:
		return provisioner
	}
}

// List returns the names of all registered drivers.
func (r *Registry) List() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	names := make([]string, 0, len(r.drivers))
	for name := range r.drivers {
		names = append(names, name)
	}
	return names
}
