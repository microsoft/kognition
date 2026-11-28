// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"bytes"
	"errors"
	"fmt"
	"testing"

	"github.com/kognition-ai/kognition/cmd/kognitionctl/cmd"
)

func TestRunStaysSilentForADeniedAccessCheck(t *testing.T) {
	var stderr bytes.Buffer
	if code := run(func() error { return fmt.Errorf("denied: %w", cmd.ErrAccessDenied) }, &stderr); code != 1 || stderr.Len() != 0 {
		t.Fatalf("code = %d, stderr = %q; want exit 1 and no message", code, stderr.String())
	}
}

func TestRun(t *testing.T) {
	var stderr bytes.Buffer
	if code := run(func() error { return nil }, &stderr); code != 0 || stderr.Len() != 0 {
		t.Fatalf("success = code %d, stderr %q", code, stderr.String())
	}
	if code := run(func() error { return errors.New("command failed") }, &stderr); code != 1 || stderr.String() != "command failed\n" {
		t.Fatalf("failure = code %d, stderr %q", code, stderr.String())
	}
}
