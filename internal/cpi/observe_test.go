// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cpi

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"sync"
	"testing"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/modelinvocation"
)

type recordingRecorder struct {
	mu       sync.Mutex
	outcomes []CallOutcome
}

func (r *recordingRecorder) RecordCall(outcome CallOutcome) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.outcomes = append(r.outcomes, outcome)
}

func (r *recordingRecorder) all() []CallOutcome {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]CallOutcome(nil), r.outcomes...)
}

type fakeDriver struct {
	name        string
	completeErr error
	embedErr    error
	completed   *CompletionResponse
	embedded    *EmbedResponse
}

func newFakeDriver(name string) *fakeDriver {
	return &fakeDriver{name: name, completed: &CompletionResponse{}, embedded: &EmbedResponse{}}
}

func (d *fakeDriver) Name() string { return d.name }
func (*fakeDriver) Validate(context.Context, *v1alpha1.ModelSpec, *v1alpha1.ModelClassSpec) error {
	return nil
}
func (*fakeDriver) Probe(context.Context, *v1alpha1.ModelSpec, *Credentials) (*HealthResult, error) {
	return &HealthResult{Available: true}, nil
}
func (d *fakeDriver) Complete(context.Context, *CompletionRequest) (*CompletionResponse, error) {
	return d.completed, d.completeErr
}
func (d *fakeDriver) Embed(context.Context, *EmbedRequest) (*EmbedResponse, error) {
	return d.embedded, d.embedErr
}

type fakeContractDriver struct{ *fakeDriver }

func (fakeContractDriver) InvocationContract(name string) (modelinvocation.Contract, error) {
	return modelinvocation.Contract{Name: name}, nil
}

type fakeCounterDriver struct{ *fakeDriver }

func (fakeCounterDriver) CountInputTokens(*CompletionRequest) (int, error) { return 7, nil }

type fakeFullDriver struct{ *fakeDriver }

func (fakeFullDriver) InvocationContract(name string) (modelinvocation.Contract, error) {
	return modelinvocation.Contract{Name: name}, nil
}
func (fakeFullDriver) CountInputTokens(*CompletionRequest) (int, error) { return 7, nil }

type timeoutError struct{}

func (timeoutError) Error() string   { return "i/o timeout" }
func (timeoutError) Timeout() bool   { return true }
func (timeoutError) Temporary() bool { return false }

func TestClassifyCallError(t *testing.T) {
	providerError := func(status int, reason string) error {
		return fmt.Errorf("dispatch: %w", &ProviderError{Provider: "p", Status: status, Reason: reason})
	}
	tests := []struct {
		name      string
		err       error
		want      string
		wantFault bool
	}{
		{"rate limited", providerError(http.StatusTooManyRequests, ReasonRateLimited), ReasonRateLimited, true},
		{"quota or billing", providerError(http.StatusPaymentRequired, ReasonQuotaOrBilling), ReasonQuotaOrBilling, true},
		{"authentication", providerError(http.StatusUnauthorized, ReasonAuthenticationFailed), ReasonAuthenticationFailed, true},
		{"provider unavailable", providerError(http.StatusServiceUnavailable, ReasonEndpointError), ReasonEndpointError, true},
		{"invalid request is the caller's fault", providerError(http.StatusBadRequest, ReasonInvalidRequest), ReasonInvalidRequest, false},
		{"unknown provider reason is bounded", providerError(http.StatusTeapot, "SomethingNew"), ReasonOther, false},
		{"canceled", fmt.Errorf("request failed: %w", context.Canceled), ReasonCanceled, false},
		{"canceled inside url error", &url.Error{Op: "Post", URL: "http://x", Err: context.Canceled}, ReasonCanceled, false},
		{"deadline exceeded", fmt.Errorf("request failed: %w", context.DeadlineExceeded), ReasonTimeout, true},
		{"client timeout", &url.Error{Op: "Post", URL: "http://x", Err: timeoutError{}}, ReasonTimeout, true},
		{"connection refused", &url.Error{Op: "Post", URL: "http://x", Err: errors.New("connection refused")}, ReasonUnreachable, true},
		{"network error", &net.OpError{Op: "dial", Err: errors.New("no route")}, ReasonUnreachable, true},
		{"malformed endpoint is a local error", parseURLError("https://example.com/%zz"), ReasonOther, false},
		{"wrapped malformed endpoint is a local error", fmt.Errorf("build request: %w", &url.Error{Op: "parse", URL: "https://example.com/%zz", Err: errors.New("invalid URL escape")}), ReasonOther, false},
		{"local or decode error", errors.New("anthropic: decode response: bad json"), ReasonOther, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			reason, fault := ClassifyCallError(test.err)
			if reason != test.want || fault != test.wantFault {
				t.Errorf("ClassifyCallError() = (%q, %t), want (%q, %t)", reason, fault, test.want, test.wantFault)
			}
		})
	}
}

func TestNormalizeReason(t *testing.T) {
	for _, known := range []string{
		ReasonAuthenticationFailed, ReasonQuotaOrBilling, ReasonRateLimited, ReasonInvalidRequest,
		ReasonEndpointError, ReasonCanceled, ReasonTimeout, ReasonUnreachable, ReasonOther,
	} {
		if got := NormalizeReason(known); got != known {
			t.Errorf("NormalizeReason(%q) = %q, want it unchanged", known, got)
		}
	}
	for _, unknown := range []string{"", "ModelUnavailable", "free text with spaces"} {
		if got := NormalizeReason(unknown); got != ReasonOther {
			t.Errorf("NormalizeReason(%q) = %q, want %q", unknown, got, ReasonOther)
		}
	}
}

func TestObserveWithNilRecorderReturnsDriverUnchanged(t *testing.T) {
	driver := newFakeDriver("fake")
	if got := Observe(driver, nil); got != Driver(driver) {
		t.Fatalf("Observe(nil) = %v, want the original driver", got)
	}
}

func TestObserveRecordsCompleteAndEmbedOutcomes(t *testing.T) {
	recorder := &recordingRecorder{}
	driver := newFakeDriver("fake")
	observed := Observe(driver, recorder)

	if observed.Name() != "fake" {
		t.Fatalf("Name() = %q, want fake", observed.Name())
	}
	completed, err := observed.Complete(context.Background(), &CompletionRequest{Model: "model-a"})
	if err != nil || completed != driver.completed {
		t.Fatalf("Complete() = (%v, %v), want the driver's response and nil error", completed, err)
	}
	embedded, err := observed.Embed(context.Background(), &EmbedRequest{Model: "model-b"})
	if err != nil || embedded != driver.embedded {
		t.Fatalf("Embed() = (%v, %v), want the driver's response and nil error", embedded, err)
	}

	driver.completeErr = &ProviderError{Provider: "fake", Status: http.StatusTooManyRequests, Reason: ReasonRateLimited}
	driver.embedErr = context.Canceled
	if _, err := observed.Complete(context.Background(), &CompletionRequest{Model: "model-a"}); !errors.Is(err, driver.completeErr) {
		t.Fatalf("Complete() error = %v, want the driver's error returned unchanged", err)
	}
	if _, err := observed.Embed(context.Background(), &EmbedRequest{Model: "model-b"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("Embed() error = %v, want the driver's error returned unchanged", err)
	}

	got := recorder.all()
	want := []CallOutcome{
		{Provider: "fake", Operation: OperationComplete, Model: "model-a"},
		{Provider: "fake", Operation: OperationEmbed, Model: "model-b"},
		{Provider: "fake", Operation: OperationComplete, Model: "model-a", Reason: ReasonRateLimited, ProviderFault: true},
		{Provider: "fake", Operation: OperationEmbed, Model: "model-b", Reason: ReasonCanceled},
	}
	if len(got) != len(want) {
		t.Fatalf("recorded %d outcomes, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		got[i].Duration = 0
		if got[i] != want[i] {
			t.Errorf("outcome %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	if !got[0].Succeeded() || got[2].Succeeded() {
		t.Error("Succeeded() does not reflect the recorded reason")
	}
}

func TestObserveRecordsDurationAndToleratesNilRequests(t *testing.T) {
	recorder := &recordingRecorder{}
	observed := Observe(newFakeDriver("fake"), recorder)

	if _, err := observed.Complete(context.Background(), nil); err != nil {
		t.Fatalf("Complete(nil) error: %v", err)
	}
	if _, err := observed.Embed(context.Background(), nil); err != nil {
		t.Fatalf("Embed(nil) error: %v", err)
	}
	for _, outcome := range recorder.all() {
		if outcome.Model != "" || outcome.Duration < 0 {
			t.Errorf("outcome = %+v, want an empty model and a non-negative duration", outcome)
		}
	}
}

func TestObservePreservesOptionalCapabilities(t *testing.T) {
	tests := []struct {
		name          string
		driver        Driver
		wantContracts bool
		wantCounter   bool
	}{
		{"plain", newFakeDriver("fake"), false, false},
		{"invocation contracts", fakeContractDriver{newFakeDriver("fake")}, true, false},
		{"exact token counter", fakeCounterDriver{newFakeDriver("fake")}, false, true},
		{"both", fakeFullDriver{newFakeDriver("fake")}, true, true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			recorder := &recordingRecorder{}
			observed := Observe(test.driver, recorder)

			contracts, hasContracts := observed.(InvocationContractProvider)
			counter, hasCounter := observed.(ExactTokenCounter)
			if hasContracts != test.wantContracts || hasCounter != test.wantCounter {
				t.Fatalf("capabilities = (contracts %t, counter %t), want (%t, %t)", hasContracts, hasCounter, test.wantContracts, test.wantCounter)
			}
			if hasContracts {
				contract, err := contracts.InvocationContract("contract-v1")
				if err != nil || contract.Name != "contract-v1" {
					t.Errorf("InvocationContract() = (%+v, %v), want the delegated contract", contract, err)
				}
			}
			if hasCounter {
				if count, err := counter.CountInputTokens(&CompletionRequest{}); err != nil || count != 7 {
					t.Errorf("CountInputTokens() = (%d, %v), want (7, nil)", count, err)
				}
			}
			if _, err := observed.Complete(context.Background(), &CompletionRequest{Model: "m"}); err != nil {
				t.Fatalf("Complete() error: %v", err)
			}
			if len(recorder.all()) != 1 {
				t.Errorf("recorded %d outcomes, want 1", len(recorder.all()))
			}
		})
	}
}

func TestRegistryObserveWrapsExistingAndLaterDrivers(t *testing.T) {
	registry := NewRegistry()
	first := newFakeDriver("openai-compatible")
	registry.MustRegister(first)

	if got, _ := registry.Get("openai-compatible"); got != Driver(first) {
		t.Fatal("an unobserved registry must return the registered driver itself")
	}

	recorder := &recordingRecorder{}
	registry.Observe(recorder)
	later := fakeFullDriver{newFakeDriver("anthropic")}
	registry.MustRegister(later)

	for _, provisioner := range []string{"openai-compatible", "azure-openai", "anthropic"} {
		driver, ok := registry.Get(provisioner)
		if !ok {
			t.Fatalf("Get(%q) did not resolve", provisioner)
		}
		if _, err := driver.Complete(context.Background(), &CompletionRequest{Model: "m"}); err != nil {
			t.Fatalf("Complete() through %q: %v", provisioner, err)
		}
	}
	got := recorder.all()
	if len(got) != 3 || got[0].Provider != "openai-compatible" || got[1].Provider != "openai-compatible" || got[2].Provider != "anthropic" {
		t.Fatalf("recorded outcomes = %+v, want openai-compatible twice (alias included) and anthropic", got)
	}
	if driver, _ := registry.Get("anthropic"); driver == Driver(later) {
		t.Error("an observed registry must return the wrapped driver")
	}
	if _, ok := mustGet(t, registry, "anthropic").(InvocationContractProvider); !ok {
		t.Error("wrapping through the registry lost the invocation contract capability")
	}
	if names := registry.List(); len(names) != 2 {
		t.Errorf("List() = %v, want the two registered names", names)
	}
}

// parseURLError takes the URL as a parameter so staticcheck SA1007 does not flag the deliberately invalid input.
func parseURLError(raw string) error {
	_, err := url.Parse(raw)
	return err
}

func mustGet(t *testing.T, registry *Registry, provisioner string) Driver {
	t.Helper()
	driver, ok := registry.Get(provisioner)
	if !ok {
		t.Fatalf("Get(%q) did not resolve", provisioner)
	}
	return driver
}

func TestRegistryObserveReplacesAndClearsTheRecorder(t *testing.T) {
	registry := NewRegistry()
	driver := newFakeDriver("fake")
	registry.MustRegister(driver)
	if registry.Observed() {
		t.Fatal("a new registry must not be observed")
	}

	first, second := &recordingRecorder{}, &recordingRecorder{}
	registry.Observe(first)
	registry.Observe(second)
	if !registry.Observed() {
		t.Fatal("Observed() = false after Observe(recorder)")
	}
	if _, err := mustGet(t, registry, "fake").Complete(context.Background(), &CompletionRequest{}); err != nil {
		t.Fatalf("Complete() error: %v", err)
	}
	if len(first.all()) != 0 || len(second.all()) != 1 {
		t.Fatalf("recorded (first %d, second %d), want (0, 1): replacing the recorder must not double count", len(first.all()), len(second.all()))
	}

	registry.Observe(nil)
	if registry.Observed() {
		t.Error("Observed() = true after Observe(nil)")
	}
	if got := mustGet(t, registry, "fake"); got != Driver(driver) {
		t.Error("Observe(nil) must restore the unwrapped driver")
	}
	registry.MustRegister(newFakeDriver("added-after-clear"))
	if got := mustGet(t, registry, "added-after-clear"); got.(*fakeDriver).name != "added-after-clear" {
		t.Error("a driver registered after Observe(nil) must not be wrapped")
	}
}

func TestRegistryObserveAppliesToPreviouslyResolvedDrivers(t *testing.T) {
	registry := NewRegistry()
	registry.MustRegister(newFakeDriver("fake"))
	first, second := &recordingRecorder{}, &recordingRecorder{}
	registry.Observe(first)
	held := mustGet(t, registry, "fake")
	call := func() {
		t.Helper()
		if _, err := held.Complete(context.Background(), &CompletionRequest{}); err != nil {
			t.Fatalf("Complete() error: %v", err)
		}
	}

	call()
	registry.Observe(second)
	call()
	registry.Observe(nil)
	call()
	if len(first.all()) != 1 || len(second.all()) != 1 {
		t.Fatalf("recorded (first %d, second %d), want (1, 1): a held driver must follow replacement and clearing", len(first.all()), len(second.all()))
	}

	registry.Observe(second)
	call()
	if len(second.all()) != 2 {
		t.Errorf("second recorded %d outcomes, want 2 after observation resumed", len(second.all()))
	}
}

func TestRegistryObserveIsSafeForConcurrentUse(t *testing.T) {
	registry := NewRegistry()
	registry.MustRegister(newFakeDriver("fake"))
	registry.Observe(&recordingRecorder{})
	held := mustGet(t, registry, "fake")

	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				_, _ = held.Complete(context.Background(), &CompletionRequest{})
			}
		}()
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				registry.Observe(&recordingRecorder{})
				registry.Observe(nil)
			}
		}()
	}
	wg.Wait()
}
