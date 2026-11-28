// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Command access-catalog renders the human-role Helm template from the access
// catalog and verifies that the committed template has not drifted.
//
//	go run ./tools/access-catalog          # write the template
//	go run ./tools/access-catalog --check  # fail if the template is out of date
package main

import (
	"bytes"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/kognition-ai/kognition/internal/accesscatalog"
)

const defaultOutput = "deploy/helm/kognition/templates/human-roles.yaml"

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("access-catalog", flag.ContinueOnError)
	flags.SetOutput(stderr)
	out := flags.String("out", defaultOutput, "path of the generated Helm template")
	check := flags.Bool("check", false, "fail if the template differs from the catalog instead of writing it")
	if err := flags.Parse(args); err != nil {
		return 2
	}

	want, err := accesscatalog.Default().RenderChartTemplate()
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "access-catalog: render: %v\n", err)
		return 1
	}

	if *check {
		have, err := os.ReadFile(*out)
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "access-catalog: read %s: %v\n", *out, err)
			return 1
		}
		if !bytes.Equal(normalizeLineEndings(have), want) {
			_, _ = fmt.Fprintf(stderr, "access-catalog: %s is out of date; run `go run ./tools/access-catalog` and commit the result\n", *out)
			return 1
		}
		_, _ = fmt.Fprintf(stdout, "access-catalog: %s is up to date\n", *out)
		return 0
	}

	if err := os.WriteFile(*out, want, 0o644); err != nil {
		_, _ = fmt.Fprintf(stderr, "access-catalog: write %s: %v\n", *out, err)
		return 1
	}
	_, _ = fmt.Fprintf(stdout, "access-catalog: wrote %s\n", *out)
	return 0
}

// normalizeLineEndings makes the drift check independent of checkout settings:
// git on Windows can check out text files with CRLF line endings.
func normalizeLineEndings(content []byte) []byte {
	return bytes.ReplaceAll(content, []byte("\r\n"), []byte("\n"))
}
