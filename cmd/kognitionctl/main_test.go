// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"bytes"
	"errors"
	"testing"
)

func TestRun(t *testing.T) {
	var stderr bytes.Buffer
	if code := run(func() error { return nil }, &stderr); code != 0 || stderr.Len() != 0 {
		t.Fatalf("success = code %d, stderr %q", code, stderr.String())
	}
	if code := run(func() error { return errors.New("command failed") }, &stderr); code != 1 || stderr.String() != "command failed\n" {
		t.Fatalf("failure = code %d, stderr %q", code, stderr.String())
	}
}
