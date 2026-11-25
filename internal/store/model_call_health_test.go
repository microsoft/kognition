// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package store

import (
	"fmt"
	"strings"
	"testing"
)

func TestValidateModelCallHealth(t *testing.T) {
	valid := ModelCallHealth{Target: "t", Calls: 2, Faults: 1}
	tests := []struct {
		name     string
		reporter string
		records  []ModelCallHealth
		wantErr  bool
	}{
		{name: "no records", reporter: "r"},
		{name: "valid record", reporter: "r", records: []ModelCallHealth{valid}},
		{name: "missing reporter", records: []ModelCallHealth{valid}, wantErr: true},
		{name: "empty target", reporter: "r", records: []ModelCallHealth{{Calls: 1}}, wantErr: true},
		{name: "oversized target", reporter: "r", records: []ModelCallHealth{{Target: strings.Repeat("t", MaxModelCallHealthTargetLength+1)}}, wantErr: true},
		{name: "target at the limit", reporter: "r", records: []ModelCallHealth{{Target: strings.Repeat("t", MaxModelCallHealthTargetLength)}}},
		{name: "oversized reason", reporter: "r", records: []ModelCallHealth{{Target: "t", Calls: 1, Faults: 1, LastFaultReason: strings.Repeat("r", MaxModelCallHealthReasonLength+1)}}, wantErr: true},
		{name: "faults exceed calls", reporter: "r", records: []ModelCallHealth{{Target: "t", Calls: 1, Faults: 2}}, wantErr: true},
		{name: "negative calls", reporter: "r", records: []ModelCallHealth{{Target: "t", Calls: -1}}, wantErr: true},
		{name: "negative faults", reporter: "r", records: []ModelCallHealth{{Target: "t", Faults: -1}}, wantErr: true},
		{name: "negative fault streak", reporter: "r", records: []ModelCallHealth{{Target: "t", Calls: 1, ConsecutiveFaults: -1}}, wantErr: true},
		{name: "negative success streak", reporter: "r", records: []ModelCallHealth{{Target: "t", Calls: 1, ConsecutiveSuccesses: -1}}, wantErr: true},
		{name: "duplicate target", reporter: "r", records: []ModelCallHealth{valid, valid}, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := ValidateModelCallHealth(test.reporter, test.records); (err != nil) != test.wantErr {
				t.Fatalf("ValidateModelCallHealth() error = %v, want error %t", err, test.wantErr)
			}
		})
	}

	atLimit := make([]ModelCallHealth, MaxModelCallHealthTargets)
	for i := range atLimit {
		atLimit[i] = ModelCallHealth{Target: fmt.Sprintf("target-%d", i)}
	}
	if err := ValidateModelCallHealth("r", atLimit); err != nil {
		t.Errorf("the target limit must be accepted: %v", err)
	}
	over := append(append([]ModelCallHealth(nil), atLimit...), ModelCallHealth{Target: "one-too-many"})
	if err := ValidateModelCallHealth("r", over); err == nil {
		t.Error("more than the target limit must be rejected")
	}
}
