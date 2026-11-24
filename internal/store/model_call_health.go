// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package store

import (
	"context"
	"time"
)

// ModelCallHealth is one reporter's summary of its recent real model calls to a
// single target. A target is an opaque hash of the provider, endpoint and model
// ID, so the record never contains endpoint URLs.
type ModelCallHealth struct {
	// Target identifies the provider, endpoint and model the calls were made to.
	Target string
	// Calls and Faults count the reporter's recent calls and, of those, the
	// failures that reflect the provider's health.
	Calls  int32
	Faults int32
	// ConsecutiveFaults and ConsecutiveSuccesses are the length of the
	// reporter's current run of provider failures or successes.
	ConsecutiveFaults    int32
	ConsecutiveSuccesses int32
	// LastFaultReason is the failure reason of the most recent provider failure.
	LastFaultReason string
	// LastFaultAt and LastSuccessAt are the reporter's observation times, zero
	// when there has been no such call.
	LastFaultAt   time.Time
	LastSuccessAt time.Time
}

// ModelCallHealthStore shares passive model health between processes. The
// processes that make model calls publish summaries; controllers read them.
// Stores that cannot share state do not implement it.
type ModelCallHealthStore interface {
	// ReplaceModelCallHealth atomically replaces everything the reporter has
	// published with records, which may be empty. Records of reporters that have
	// stopped publishing expire on their own.
	ReplaceModelCallHealth(ctx context.Context, reporter string, records []ModelCallHealth) error

	// ListModelCallHealth returns the records, from all reporters, that were
	// published within maxAge. Reporters publishing the same target yield one
	// record each.
	ListModelCallHealth(ctx context.Context, maxAge time.Duration) ([]ModelCallHealth, error)
}

const (
	// MaxModelCallHealthTargets bounds the records one reporter may publish.
	MaxModelCallHealthTargets = 1024
	// MaxModelCallHealthReasonLength bounds ModelCallHealth.LastFaultReason.
	MaxModelCallHealthReasonLength = 64
	// MaxModelCallHealthTargetLength bounds ModelCallHealth.Target.
	MaxModelCallHealthTargetLength = 128
)
