//go:build windows

// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package studiotutorial

import (
	"os"

	"golang.org/x/sys/windows"
)

func lockProgress(file *os.File) (func(), error) {
	var overlapped windows.Overlapped
	handle := windows.Handle(file.Fd())
	err := windows.LockFileEx(handle, windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &overlapped)
	return func() { _ = windows.UnlockFileEx(handle, 0, 1, 0, &overlapped) }, err
}
