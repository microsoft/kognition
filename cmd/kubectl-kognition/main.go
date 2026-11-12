// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"fmt"
	"io"
	"os"

	"github.com/kognition-ai/kognition/cmd/kognitionctl/cmd"
)

func main() {
	os.Exit(run(cmd.SetPluginMode, cmd.Execute, os.Stderr))
}

func run(setPluginMode func(), execute func() error, stderr io.Writer) int {
	setPluginMode()
	if err := execute(); err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}
