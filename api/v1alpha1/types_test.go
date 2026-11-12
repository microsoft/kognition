// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package v1alpha1

import (
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func TestDurationJSONRoundTrip(t *testing.T) {
	tests := []struct {
		name     string
		duration time.Duration
		wantJSON string
	}{
		{name: "zero", duration: 0, wantJSON: `"0s"`},
		{name: "compound", duration: 2*time.Hour + 30*time.Minute + 15*time.Second, wantJSON: `"2h30m15s"`},
		{name: "negative", duration: -500 * time.Millisecond, wantJSON: `"-500ms"`},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			encoded, err := json.Marshal(Duration{Duration: test.duration})
			if err != nil {
				t.Fatalf("Marshal() error = %v", err)
			}
			if string(encoded) != test.wantJSON {
				t.Fatalf("Marshal() = %s, want %s", encoded, test.wantJSON)
			}

			var decoded Duration
			if err := json.Unmarshal(encoded, &decoded); err != nil {
				t.Fatalf("Unmarshal() error = %v", err)
			}
			if decoded.Duration != test.duration {
				t.Fatalf("Unmarshal() = %s, want %s", decoded.Duration, test.duration)
			}
		})
	}
}

func TestDurationUnmarshalJSONRejectsMalformedDuration(t *testing.T) {
	duration := Duration{Duration: time.Minute}
	if err := json.Unmarshal([]byte(`"eventually"`), &duration); err == nil {
		t.Fatal("Unmarshal() error = nil, want malformed duration error")
	}
	if duration.Duration != time.Minute {
		t.Fatalf("duration changed to %s after failed parse", duration.Duration)
	}
}

func TestDurationYAMLRoundTrip(t *testing.T) {
	duration := Duration{Duration: 90 * time.Minute}
	encoded, err := duration.MarshalYAML()
	if err != nil {
		t.Fatalf("MarshalYAML() error = %v", err)
	}
	if encoded != "1h30m0s" {
		t.Fatalf("MarshalYAML() = %#v, want %q", encoded, "1h30m0s")
	}

	var decoded Duration
	err = decoded.UnmarshalYAML(func(value interface{}) error {
		*(value.(*string)) = encoded.(string)
		return nil
	})
	if err != nil {
		t.Fatalf("UnmarshalYAML() error = %v", err)
	}
	if decoded.Duration != duration.Duration {
		t.Fatalf("UnmarshalYAML() = %s, want %s", decoded.Duration, duration.Duration)
	}
}

func TestDurationUnmarshalYAMLPropagatesErrors(t *testing.T) {
	sentinel := errors.New("decode failed")
	duration := Duration{Duration: time.Minute}
	if err := duration.UnmarshalYAML(func(interface{}) error { return sentinel }); !errors.Is(err, sentinel) {
		t.Fatalf("UnmarshalYAML() error = %v, want %v", err, sentinel)
	}
	if duration.Duration != time.Minute {
		t.Fatalf("duration changed to %s after decoder failure", duration.Duration)
	}

	if err := duration.UnmarshalYAML(func(value interface{}) error {
		*(value.(*string)) = "eventually"
		return nil
	}); err == nil {
		t.Fatal("UnmarshalYAML() error = nil, want malformed duration error")
	}
	if duration.Duration != time.Minute {
		t.Fatalf("duration changed to %s after failed parse", duration.Duration)
	}
}
