// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/kognition-ai/kognition/cmd/kognitionctl/cmd"
)

func main() {
	os.Exit(run(cmd.Execute, os.Stderr))
}

func run(execute func() error, stderr io.Writer) int {
	if err := execute(); err != nil {
		// A denied `auth can-i` has already printed its answer.
		if !errors.Is(err, cmd.ErrAccessDenied) {
			_, _ = fmt.Fprintln(stderr, err)
		}
		return 1
	}
	return 0
}
