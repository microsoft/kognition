// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

type failingDispositionWriter struct {
	err error
}

func (writer failingDispositionWriter) Write([]byte) (int, error) {
	return 0, writer.err
}

func TestSQLDisposition(t *testing.T) {
	root := t.TempDir()
	command := exec.Command("helm", "template", "security-test", "../../deploy/helm/kognition", "--namespace", "control", "--output-dir", root)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("render chart: %v\n%s", err, output)
	}
	manifest := filepath.Join(root, filepath.FromSlash(migrationTarget))
	original, err := os.ReadFile(manifest)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	for _, name := range []string{"reviewed SQL", "credential injected", "changed SQL", "changed SQL comment", "renamed migration", "extra key", "missing key", "binary data", "annotation credential", "label credential", "changed label", "extra document", "duplicate key", "renamed configmap", "changed finding", "other target", "additional RBAC finding", "duplicate finding", "ignored finding", "vulnerability", "empty report", "summary write failure", "finding write failure", "stale report", "wrong input hash", "wrong candidate hash", "missing expected candidate", "invalid expected candidate", "uppercase expected candidate", "missing provenance"} {
		t.Run(name, func(t *testing.T) {
			testRoot := t.TempDir()
			renderedRoot := filepath.Join(testRoot, "rendered")
			if err := os.CopyFS(renderedRoot, os.DirFS(root)); err != nil {
				t.Fatal(err)
			}
			manifest := filepath.Join(renderedRoot, filepath.FromSlash(migrationTarget))
			var document map[string]any
			if err := yaml.Unmarshal(original, &document); err != nil {
				t.Fatal(err)
			}
			data := document["data"].(map[string]any)
			entry := result{Target: migrationTarget, Class: "config", Type: "kubernetes", Misconfigurations: []finding{{ID: "KSV-0109", Status: "FAIL", Severity: "HIGH", Message: migrationMessage}}}
			switch name {
			case "credential injected":
				data["006_token_reservations.sql"] = data["006_token_reservations.sql"].(string) + "\npassword = 'negative-control-only'\n"
			case "changed SQL":
				data["038_workflow_executor_approvals.sql"] = data["038_workflow_executor_approvals.sql"].(string) + "\nSELECT 1;\n"
			case "changed SQL comment":
				data["038_workflow_executor_approvals.sql"] = data["038_workflow_executor_approvals.sql"].(string) + "\n-- unreviewed change\n"
			case "renamed migration":
				data["038_unreviewed.sql"] = data["038_workflow_executor_approvals.sql"]
				delete(data, "038_workflow_executor_approvals.sql")
			case "extra key":
				data["password"] = "negative-control-only"
			case "missing key":
				delete(data, "001_initial_schema.sql")
			case "binary data":
				document["binaryData"] = map[string]string{"credential": "bmVnYXRpdmU="}
			case "annotation credential":
				document["metadata"].(map[string]any)["annotations"] = map[string]string{"password": "negative-control-only"}
			case "label credential":
				document["metadata"].(map[string]any)["labels"].(map[string]any)["password"] = "negative-control-only"
			case "changed label":
				document["metadata"].(map[string]any)["labels"].(map[string]any)["app.kubernetes.io/name"] = "negative-control-only"
			case "renamed configmap":
				document["metadata"].(map[string]any)["name"] = "other"
			case "changed finding":
				entry.Misconfigurations[0].Message += " changed"
			case "other target":
				entry.Target = "other/configmap.yaml"
			case "additional RBAC finding", "finding write failure":
				entry.Misconfigurations = append(entry.Misconfigurations, finding{ID: "KSV-0056", Status: "FAIL", Severity: "HIGH"})
			case "duplicate finding":
				entry.Misconfigurations = append(entry.Misconfigurations, entry.Misconfigurations[0])
			case "ignored finding":
				entry.Misconfigurations[0].Status = "EXCEPTION"
			case "vulnerability":
				entry.Vulnerabilities = []json.RawMessage{json.RawMessage(`{"VulnerabilityID":"test"}`)}
			}
			contents, err := yaml.Marshal(document)
			if err != nil {
				t.Fatal(err)
			}
			if name == "extra document" {
				contents = append(contents, []byte("\n---\nkind: Secret\n")...)
			}
			if name == "duplicate key" {
				contents = append(contents, []byte("\ndata: {}\n")...)
			}
			if err := os.WriteFile(manifest, contents, 0600); err != nil {
				t.Fatal(err)
			}
			scan := report{SchemaVersion: 2, Results: []result{entry}}
			if name == "empty report" {
				scan.Results = nil
			}
			reportBytes, err := json.Marshal(scan)
			if err != nil {
				t.Fatal(err)
			}
			reportPath := filepath.Join(testRoot, "report.json")
			if err := os.WriteFile(reportPath, reportBytes, 0600); err != nil {
				t.Fatal(err)
			}
			inputDigest, err := sha256Tree(renderedRoot)
			if err != nil {
				t.Fatal(err)
			}
			reportDigest, err := sha256File(reportPath)
			if err != nil {
				t.Fatal(err)
			}
			candidateDigest := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
			record := provenance{
				SchemaVersion: 1, Scanner: "trivy", ScannerVersion: "0.74.0", DatabaseVersion: 2,
				DatabaseUpdatedAt: now.Add(-time.Hour).Format(time.RFC3339), Command: []string{"trivy", "config", renderedRoot},
				WorkingDirectory: testRoot, CandidateSHA256: candidateDigest, InputKind: "tree", InputSHA256: inputDigest,
				ReportSHA256: reportDigest, StartedAt: now.Add(-time.Minute).Format(time.RFC3339), FinishedAt: now.Format(time.RFC3339), ExitCode: 1,
			}
			if name == "stale report" {
				record.StartedAt = now.Add(-26 * time.Hour).Format(time.RFC3339)
				record.FinishedAt = now.Add(-25 * time.Hour).Format(time.RFC3339)
			}
			if name == "wrong input hash" {
				record.InputSHA256 = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
			}
			if name == "wrong candidate hash" {
				record.CandidateSHA256 = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
			}
			provenanceBytes, err := json.Marshal(record)
			if err != nil {
				t.Fatal(err)
			}
			provenancePath := filepath.Join(testRoot, "provenance.json")
			if name != "missing provenance" {
				if err := os.WriteFile(provenancePath, provenanceBytes, 0600); err != nil {
					t.Fatal(err)
				}
			}
			var output bytes.Buffer
			var destination io.Writer = &output
			writeFailure := errors.New("disposition output unavailable")
			if name == "summary write failure" || name == "finding write failure" {
				destination = failingDispositionWriter{err: writeFailure}
			}
			switch name {
			case "missing expected candidate":
				candidateDigest = ""
			case "invalid expected candidate":
				candidateDigest = "invalid"
			case "uppercase expected candidate":
				candidateDigest = "BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB"
			}
			err = run(reportPath, renderedRoot, provenancePath, candidateDigest, now, destination)
			if name == "changed SQL" || name == "changed SQL comment" || name == "renamed migration" {
				if err == nil || !strings.Contains(err.Error(), "migration content changed; a new review is required") {
					t.Fatalf("unreviewed migration was not rejected by its content pin: %v", err)
				}
			}
			if destination != &output && !errors.Is(err, writeFailure) {
				t.Fatalf("output failure was not propagated: %v", err)
			}
			if (err == nil) != (name == "reviewed SQL") {
				t.Fatalf("unexpected disposition: %v\n%s", err, output.String())
			}
			unchanged, err := os.ReadFile(reportPath)
			if err != nil || !bytes.Equal(unchanged, reportBytes) {
				t.Fatal("raw scanner report was altered")
			}
		})
	}
}

func TestArchiveProvenanceRejectsChangedArchive(t *testing.T) {
	now := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	root := t.TempDir()
	archivePath := filepath.Join(root, "candidate.tgz")
	reportPath := filepath.Join(root, "report.json")
	provenancePath := filepath.Join(root, "provenance.json")
	if err := os.WriteFile(archivePath, []byte("reviewed archive"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(reportPath, []byte(`{"SchemaVersion":2,"Results":[]}`), 0600); err != nil {
		t.Fatal(err)
	}
	archiveDigest, err := sha256File(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	reportDigest, err := sha256File(reportPath)
	if err != nil {
		t.Fatal(err)
	}
	record := provenance{
		SchemaVersion: 1, Scanner: "trivy", ScannerVersion: "0.74.0", DatabaseVersion: 2,
		DatabaseUpdatedAt: now.Add(-time.Hour).Format(time.RFC3339), Command: []string{"trivy", "fs", archivePath},
		WorkingDirectory: root, CandidateSHA256: archiveDigest, InputKind: "file", InputSHA256: archiveDigest,
		ReportSHA256: reportDigest, StartedAt: now.Add(-time.Minute).Format(time.RFC3339), FinishedAt: now.Format(time.RFC3339), ExitCode: 0,
	}
	contents, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(provenancePath, contents, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(archivePath, []byte("changed archive"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := verifyProvenance(provenancePath, reportPath, archivePath, archiveDigest, now); err == nil {
		t.Fatal("changed archive passed provenance verification")
	}
}

func TestCLIRequiresCandidateHash(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "securitygate.exe")
	if output, err := exec.Command("go", "build", "-o", binary, ".").CombinedOutput(); err != nil {
		t.Fatalf("build verifier: %v\n%s", err, output)
	}
	for _, testCase := range []struct {
		name     string
		flags    []string
		exitCode int
		message  string
	}{
		{name: "missing", exitCode: 2, message: "--candidate-sha256 SHA256"},
		{name: "malformed", flags: []string{"--candidate-sha256", "invalid"}, exitCode: 1, message: "expected candidate SHA256 must be a lowercase SHA256"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			arguments := append([]string{"--report", "unused", "--rendered-root", "unused", "--provenance", "unused"}, testCase.flags...)
			output, err := exec.Command(binary, arguments...).CombinedOutput()
			var exitError *exec.ExitError
			if !errors.As(err, &exitError) || exitError.ExitCode() != testCase.exitCode || !bytes.Contains(output, []byte(testCase.message)) {
				t.Fatalf("unexpected CLI result: %v\n%s", err, output)
			}
		})
	}
	t.Run("plan-is-separate", func(t *testing.T) {
		root, base := planRepository(t)
		output, err := exec.Command(binary, "plan", "--repo", root, "--base", base).CombinedOutput()
		if err != nil || !bytes.Contains(output, []byte(`"purpose":"validation-plan-not-clearance"`)) {
			t.Fatalf("separate plan command failed: %v\n%s", err, output)
		}
	})
}
