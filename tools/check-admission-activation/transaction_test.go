// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestFileDigest(test *testing.T) {
	directory := test.TempDir()
	path := filepath.Join(directory, "chart.tgz")
	if err := os.WriteFile(path, []byte("abc"), 0600); err != nil {
		test.Fatal(err)
	}
	if digest, err := fileDigest(path); err != nil || digest != "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad" {
		test.Fatalf("exact byte digest: %s %v", digest, err)
	}
	for _, invalid := range []string{directory, filepath.Join(directory, "missing")} {
		if _, err := fileDigest(invalid); err == nil {
			test.Fatalf("accepted unreadable file %s", invalid)
		}
	}
}

func TestTransactionCommandRequiredInputs(test *testing.T) {
	base := []string{"--manifest", "missing", "--chart", "chart.tgz", "--kubeconfig", "explicit", "--release", "release", "--authority-namespace", "authority", "--replicas", "1", "--writers-frozen"}
	for _, mode := range []string{"prepare", "activate", "recover"} {
		for _, flags := range [][]string{nil, {"--unknown"}, {"--replicas", "0"}, {"--replicas", "2147483648"}} {
			var output bytes.Buffer
			if code := run(append([]string{mode}, flags...), &output, &output); code != 2 {
				test.Fatalf("%s %v returned %d", mode, flags, code)
			}
		}
	}
	for _, scenario := range []struct {
		mode  string
		flags []string
	}{
		{"prepare", []string{"--approval", "approved"}},
		{"activate", nil},
		{"recover", []string{"--approval", "approved"}},
		{"recover", []string{"--approval", "approved", "--executors-stopped", "--dry-run"}},
		{"activate", []string{"--approval", "approved", "--executors-stopped"}},
	} {
		var output bytes.Buffer
		arguments := append(append([]string{scenario.mode}, base...), scenario.flags...)
		if code := run(arguments, &output, &output); code != 2 {
			test.Fatalf("%v returned %d: %s", arguments, code, output.String())
		}
	}
}

func TestTransactionCommandFailureReport(test *testing.T) {
	for _, mode := range []string{"prepare", "activate", "recover"} {
		arguments := []string{mode, "--manifest", filepath.Join(test.TempDir(), "missing"), "--chart", "chart.tgz", "--kubeconfig", "explicit", "--release", "release", "--authority-namespace", "authority", "--replicas", "1", "--writers-frozen"}
		if mode != "prepare" {
			arguments = append(arguments, "--approval", "approved")
		}
		if mode == "recover" {
			arguments = append(arguments, "--executors-stopped")
		}
		var output, diagnostic bytes.Buffer
		code := run(arguments, &output, &diagnostic)
		var report struct {
			Mode   string
			Passed bool
			Error  string
		}
		if err := json.Unmarshal(output.Bytes(), &report); err != nil {
			test.Fatal(err)
		}
		if code != 1 || report.Mode != mode || report.Passed || report.Error == "" || diagnostic.Len() == 0 {
			test.Fatalf("invalid report: %s", output.String())
		}
	}
}
