// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

const identityFixtureTarget = "e2e/fixtures/workload-identity-platform.yaml"
const identityFixtureDigest = "1ddc8083300394454e7eb503593435194496dfb42d03a30f13339eeb4415ecb3"
const identityIgnoreTarget = ".trivyignore.yaml"
const identityIgnoreDigest = "9bf362f3ef6f900ca799e2d6138f9cf1704a10751fe117b438288b4a873f5368"

func verifyIdentityFixture(root string, output io.Writer) error {
	for _, input := range []struct {
		path   string
		digest string
	}{
		{identityFixtureTarget, identityFixtureDigest},
		{identityIgnoreTarget, identityIgnoreDigest},
	} {
		path := filepath.Join(root, filepath.FromSlash(input.path))
		info, err := os.Lstat(path)
		if err != nil {
			return fmt.Errorf("inspect reviewed identity input %s: %w", input.path, err)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("reviewed identity input %s must be a regular file", input.path)
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read reviewed identity input %s: %w", input.path, err)
		}
		digest := fmt.Sprintf("%x", sha256.Sum256(bytes.ReplaceAll(content, []byte("\r\n"), []byte("\n"))))
		if digest != input.digest {
			return fmt.Errorf("identity input %s changed; a new privilege/disposition review is required", input.path)
		}
	}
	if _, err := fmt.Fprintln(output, "Verified exact SPIRE acceptance fixture and scoped scanner dispositions; deployed acceptance remains separate."); err != nil {
		return fmt.Errorf("write identity fixture verification: %w", err)
	}
	return nil
}

func runIdentityFixture(args []string, output, errorOutput io.Writer) int {
	flags := flag.NewFlagSet("identity-fixture", flag.ContinueOnError)
	flags.SetOutput(errorOutput)
	root := flags.String("root", ".", "repository root containing the reviewed fixture and scanner policy")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if flags.NArg() != 0 {
		if _, err := fmt.Fprintln(errorOutput, "identity-fixture accepts only --root PATH"); err != nil {
			fmt.Fprintln(os.Stderr, "write identity-fixture diagnostic:", err)
			return 1
		}
		return 2
	}
	if err := verifyIdentityFixture(*root, output); err != nil {
		if _, writeErr := fmt.Fprintln(errorOutput, err); writeErr != nil {
			fmt.Fprintln(os.Stderr, "write identity-fixture diagnostic:", writeErr)
		}
		return 1
	}
	return 0
}
