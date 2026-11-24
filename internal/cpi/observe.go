// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cpi

import (
	"context"
	"errors"
	"net"
	"net/url"
	"time"
)

// Operations reported in a CallOutcome.
const (
	OperationComplete = "complete"
	OperationEmbed    = "embed"
)

// Failure reasons for calls that did not produce an HTTP response from the
// provider, in addition to the provider-error reasons in provider_error.go.
const (
	ReasonCanceled    = "Canceled"
	ReasonTimeout     = "Timeout"
	ReasonUnreachable = "EndpointUnreachable"
	// ReasonOther covers failures that are not attributable to the provider,
	// such as invalid requests detected locally or undecodable responses.
	ReasonOther = "Other"
)

// CallOutcome describes one finished Complete or Embed call.
type CallOutcome struct {
	// Provider is the driver name.
	Provider string
	// Operation is OperationComplete or OperationEmbed.
	Operation string
	// Model is the provider model ID from the request.
	Model string
	// Duration is the wall-clock time of the driver call.
	Duration time.Duration
	// Reason classifies a failure and is empty on success.
	Reason string
	// ProviderFault reports whether a failure says something about the health
	// of the provider or its configuration (credentials, quota, availability),
	// as opposed to the caller's request or lifecycle.
	ProviderFault bool
}

// Succeeded reports whether the call completed without error.
func (o CallOutcome) Succeeded() bool { return o.Reason == "" }

// CallRecorder receives the outcome of every observed driver call. Implementations
// must be safe for concurrent use and must not block.
type CallRecorder interface {
	RecordCall(CallOutcome)
}

// ClassifyCallError maps an error returned by Complete or Embed to a failure
// reason and whether it is attributable to the provider.
func ClassifyCallError(err error) (reason string, providerFault bool) {
	if providerErr, ok := ProviderFailure(err); ok {
		reason = NormalizeReason(providerErr.Reason)
		return reason, reason != ReasonInvalidRequest && reason != ReasonOther
	}
	var netErr net.Error
	switch {
	case errors.Is(err, context.Canceled):
		return ReasonCanceled, false
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &netErr) && netErr.Timeout():
		return ReasonTimeout, true
	case errors.As(err, &netErr), isURLError(err):
		return ReasonUnreachable, true
	default:
		return ReasonOther, false
	}
}

func isURLError(err error) bool {
	var urlErr *url.Error
	return errors.As(err, &urlErr)
}

// NormalizeReason returns reason if it is one of the known failure reasons and
// ReasonOther otherwise, which keeps downstream label sets bounded.
func NormalizeReason(reason string) string {
	switch reason {
	case ReasonAuthenticationFailed, ReasonQuotaOrBilling, ReasonRateLimited, ReasonInvalidRequest,
		ReasonEndpointError, ReasonCanceled, ReasonTimeout, ReasonUnreachable, ReasonOther:
		return reason
	default:
		return ReasonOther
	}
}

// Observe returns a driver that reports the outcome of every Complete and Embed
// call to recorder. Optional capabilities of the wrapped driver
// (InvocationContractProvider and ExactTokenCounter) remain discoverable by type
// assertion. A nil recorder returns driver unchanged.
func Observe(driver Driver, recorder CallRecorder) Driver {
	if recorder == nil {
		return driver
	}
	base := &observedDriver{Driver: driver, recorder: recorder}
	contracts, hasContracts := driver.(InvocationContractProvider)
	counter, hasCounter := driver.(ExactTokenCounter)
	switch {
	case hasContracts && hasCounter:
		return &observedContractCounterDriver{observedDriver: base, InvocationContractProvider: contracts, ExactTokenCounter: counter}
	case hasContracts:
		return &observedContractDriver{observedDriver: base, InvocationContractProvider: contracts}
	case hasCounter:
		return &observedCounterDriver{observedDriver: base, ExactTokenCounter: counter}
	default:
		return base
	}
}

type observedDriver struct {
	Driver
	recorder CallRecorder
}

type observedContractDriver struct {
	*observedDriver
	InvocationContractProvider
}

type observedCounterDriver struct {
	*observedDriver
	ExactTokenCounter
}

type observedContractCounterDriver struct {
	*observedDriver
	InvocationContractProvider
	ExactTokenCounter
}

func (d *observedDriver) Complete(ctx context.Context, req *CompletionRequest) (*CompletionResponse, error) {
	start := time.Now()
	resp, err := d.Driver.Complete(ctx, req)
	model := ""
	if req != nil {
		model = req.Model
	}
	d.record(OperationComplete, model, start, err)
	return resp, err
}

func (d *observedDriver) Embed(ctx context.Context, req *EmbedRequest) (*EmbedResponse, error) {
	start := time.Now()
	resp, err := d.Driver.Embed(ctx, req)
	model := ""
	if req != nil {
		model = req.Model
	}
	d.record(OperationEmbed, model, start, err)
	return resp, err
}

func (d *observedDriver) record(operation, model string, start time.Time, err error) {
	outcome := CallOutcome{Provider: d.Name(), Operation: operation, Model: model, Duration: time.Since(start)}
	if err != nil {
		outcome.Reason, outcome.ProviderFault = ClassifyCallError(err)
	}
	d.recorder.RecordCall(outcome)
}
