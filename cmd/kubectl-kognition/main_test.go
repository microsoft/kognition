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
	pluginCalls := 0
	if code := run(func() { pluginCalls++ }, func() error { return nil }, &stderr); code != 0 || pluginCalls != 1 || stderr.Len() != 0 {
		t.Fatalf("success = code %d, plugin calls %d, stderr %q", code, pluginCalls, stderr.String())
	}
	if code := run(func() { pluginCalls++ }, func() error { return errors.New("command failed") }, &stderr); code != 1 || pluginCalls != 2 || stderr.String() != "command failed\n" {
		t.Fatalf("failure = code %d, plugin calls %d, stderr %q", code, pluginCalls, stderr.String())
	}
}
