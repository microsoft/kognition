// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package store

import (
	"context"
	"fmt"
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
	// StreakStartedAt is when that run began, zero when unknown. Readers use it to
	// count only the part of a run that no other reporter's opposite outcome
	// interrupted.
	StreakStartedAt time.Time
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

// ValidateModelCallHealth checks a reporter's publication against the limits every
// ModelCallHealthStore enforces, so that implementations accept the same input.
func ValidateModelCallHealth(reporter string, records []ModelCallHealth) error {
	if reporter == "" {
		return fmt.Errorf("model call health requires a reporter")
	}
	if len(records) > MaxModelCallHealthTargets {
		return fmt.Errorf("model call health accepts at most %d targets per reporter", MaxModelCallHealthTargets)
	}
	seen := make(map[string]struct{}, len(records))
	for _, record := range records {
		if err := ValidateModelCallHealthRecord(record); err != nil {
			return err
		}
		if _, duplicate := seen[record.Target]; duplicate {
			return fmt.Errorf("model call health target %q is published twice", record.Target)
		}
		seen[record.Target] = struct{}{}
	}
	return nil
}

// ValidateModelCallHealthRecord checks one record.
func ValidateModelCallHealthRecord(record ModelCallHealth) error {
	switch {
	case record.Target == "" || len(record.Target) > MaxModelCallHealthTargetLength:
		return fmt.Errorf("model call health target must be 1 to %d characters", MaxModelCallHealthTargetLength)
	case len(record.LastFaultReason) > MaxModelCallHealthReasonLength:
		return fmt.Errorf("model call health reason must be at most %d characters", MaxModelCallHealthReasonLength)
	case record.Calls < 0 || record.Faults < 0 || record.Faults > record.Calls:
		return fmt.Errorf("model call health requires 0 <= faults <= calls")
	case record.ConsecutiveFaults < 0 || record.ConsecutiveSuccesses < 0:
		return fmt.Errorf("model call health streaks must not be negative")
	}
	return nil
}
