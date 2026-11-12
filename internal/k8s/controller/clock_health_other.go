// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build !linux

package controller

import (
	"fmt"
	"time"
)

// NewHostClockHealthCheck fails closed on unsupported production platforms.
func NewHostClockHealthCheck(_ time.Duration) func() error {
	return func() error {
		return fmt.Errorf("kernel clock health is unsupported on this platform")
	}
}
