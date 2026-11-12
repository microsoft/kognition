// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build linux

package controller

import (
	"fmt"
	"time"

	"golang.org/x/sys/unix"
)

const linuxClockUnsynchronized = 0x0040

// NewHostClockHealthCheck uses the kernel time-discipline state and estimated
// maximum error to verify the host clock remains within the configured bound.
func NewHostClockHealthCheck(maximumClockSkew time.Duration) func() error {
	return func() error {
		var state unix.Timex
		clockState, err := unix.Adjtimex(&state)
		if err != nil {
			return fmt.Errorf("read kernel clock state: %w", err)
		}
		if clockState == unix.TIME_ERROR {
			return fmt.Errorf("kernel clock discipline reports TIME_ERROR")
		}
		if state.Status&linuxClockUnsynchronized != 0 {
			return fmt.Errorf("kernel clock is unsynchronized")
		}
		maximumError := time.Duration(state.Maxerror) * time.Microsecond
		if maximumError > maximumClockSkew {
			return fmt.Errorf("kernel clock maximum error %s exceeds %s", maximumError, maximumClockSkew)
		}
		return nil
	}
}
