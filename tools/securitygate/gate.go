// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const migrationTarget = "kognition/templates/migrations-configmap.yaml"
const migrationDigest = "b690c091cfa7d24d24ef8b1029c6aff1a2c73e295772afd6706f071189b4348f"
const migrationMessage = `ConfigMap 'security-test-kognition-migrations' in 'control' namespace stores secrets in key(s) or value(s) '{"        (status <> 'Prepared' AND preparation_token ", "006_token_reservations.sql", "007_token_reservation_quota_periods.sql"}'`

type finding struct {
	ID       string
	Status   string
	Severity string
	Message  string
}

type result struct {
	Target            string
	Class             string
	Type              string
	Misconfigurations []finding
	Vulnerabilities   []json.RawMessage
	Secrets           []json.RawMessage
	Licenses          []json.RawMessage
}

type report struct {
	SchemaVersion int
	Results       []result
}

type provenance struct {
	SchemaVersion     int      `json:"schemaVersion"`
	Scanner           string   `json:"scanner"`
	ScannerVersion    string   `json:"scannerVersion"`
	DatabaseVersion   int      `json:"databaseVersion"`
	DatabaseUpdatedAt string   `json:"databaseUpdatedAt"`
	Command           []string `json:"command"`
	WorkingDirectory  string   `json:"workingDirectory"`
	CandidateSHA256   string   `json:"candidateSHA256"`
	InputKind         string   `json:"inputKind"`
	InputSHA256       string   `json:"inputSHA256"`
	ReportSHA256      string   `json:"reportSHA256"`
	StartedAt         string   `json:"startedAt"`
	FinishedAt        string   `json:"finishedAt"`
	ExitCode          int      `json:"exitCode"`
}

func sha256File(path string) (string, error) {
	contents, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(contents)
	return hex.EncodeToString(digest[:]), nil
}

func sha256Tree(root string) (string, error) {
	var paths []string
	if err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("input tree contains symlink %q", path)
		}
		if !entry.IsDir() {
			if !entry.Type().IsRegular() {
				return fmt.Errorf("input tree contains non-regular file %q", path)
			}
			paths = append(paths, path)
		}
		return nil
	}); err != nil {
		return "", err
	}
	sort.Slice(paths, func(i, j int) bool {
		left, _ := filepath.Rel(root, paths[i])
		right, _ := filepath.Rel(root, paths[j])
		return filepath.ToSlash(left) < filepath.ToSlash(right)
	})
	digest := sha256.New()
	for _, path := range paths {
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return "", err
		}
		contents, err := os.ReadFile(path)
		if err != nil {
			return "", err
		}
		fileDigest := sha256.Sum256(contents)
		_, _ = io.WriteString(digest, filepath.ToSlash(relative)+"\x00"+hex.EncodeToString(fileDigest[:])+"\x00")
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}

func validSHA256(value string) bool {
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size && value == strings.ToLower(value)
}

func verifyProvenance(provenancePath, reportPath, inputPath, expectedCandidateSHA256 string, now time.Time) error {
	if !validSHA256(expectedCandidateSHA256) {
		return errors.New("expected candidate SHA256 must be a lowercase SHA256")
	}
	contents, err := os.ReadFile(provenancePath)
	if err != nil {
		return fmt.Errorf("read scan provenance: %w", err)
	}
	var record provenance
	decoder := json.NewDecoder(bytes.NewReader(contents))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&record); err != nil {
		return fmt.Errorf("decode scan provenance: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("scan provenance must contain exactly one JSON value")
	}
	if record.SchemaVersion != 1 || record.Scanner != "trivy" || record.ScannerVersion == "" || record.DatabaseVersion <= 0 || len(record.Command) == 0 || record.WorkingDirectory == "" {
		return errors.New("scan provenance metadata is incomplete")
	}
	if !validSHA256(record.CandidateSHA256) || !validSHA256(record.InputSHA256) || !validSHA256(record.ReportSHA256) {
		return errors.New("scan provenance contains an invalid SHA256")
	}
	if record.CandidateSHA256 != expectedCandidateSHA256 {
		return errors.New("scan provenance candidate hash does not match expected candidate")
	}
	databaseUpdatedAt, err := time.Parse(time.RFC3339Nano, record.DatabaseUpdatedAt)
	if err != nil || databaseUpdatedAt.After(now) {
		return errors.New("scan provenance database timestamp is invalid")
	}
	startedAt, startErr := time.Parse(time.RFC3339Nano, record.StartedAt)
	finishedAt, finishErr := time.Parse(time.RFC3339Nano, record.FinishedAt)
	if startErr != nil || finishErr != nil || finishedAt.Before(startedAt) || finishedAt.After(now.Add(5*time.Minute)) || now.Sub(finishedAt) > 24*time.Hour {
		return errors.New("scan provenance is stale or has invalid timestamps")
	}
	if record.ExitCode < 0 || record.ExitCode > 1 {
		return errors.New("scan provenance exit code is invalid")
	}
	reportDigest, err := sha256File(reportPath)
	if err != nil {
		return fmt.Errorf("hash raw scan report: %w", err)
	}
	if reportDigest != record.ReportSHA256 {
		return errors.New("raw scan report hash does not match provenance")
	}
	var inputDigest string
	switch record.InputKind {
	case "file":
		inputDigest, err = sha256File(inputPath)
	case "tree":
		inputDigest, err = sha256Tree(inputPath)
	default:
		return fmt.Errorf("unsupported provenance input kind %q", record.InputKind)
	}
	if err != nil {
		return fmt.Errorf("hash scan input: %w", err)
	}
	if inputDigest != record.InputSHA256 {
		return errors.New("scan input hash does not match provenance")
	}
	return nil
}

func verifyMigration(root string) error {
	contents, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(migrationTarget)))
	if err != nil {
		return err
	}
	decoder := yaml.NewDecoder(bytes.NewReader(contents))
	var document map[string]any
	if err := decoder.Decode(&document); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("migration manifest must contain exactly one document")
	}
	for key := range document {
		if key != "apiVersion" && key != "kind" && key != "metadata" && key != "data" {
			return fmt.Errorf("unexpected migration field %q", key)
		}
	}
	metadata, ok := document["metadata"].(map[string]any)
	if !ok || document["apiVersion"] != "v1" || document["kind"] != "ConfigMap" || metadata["name"] != "security-test-kognition-migrations" || metadata["namespace"] != "control" {
		return errors.New("migration identity mismatch")
	}
	for key := range metadata {
		if key != "name" && key != "namespace" && key != "labels" {
			return fmt.Errorf("unexpected migration metadata %q", key)
		}
	}
	labels := map[string]any{
		"helm.sh/chart":                "kognition-0.1.0",
		"app.kubernetes.io/name":       "kognition",
		"app.kubernetes.io/version":    "0.1.0",
		"app.kubernetes.io/managed-by": "Helm",
		"app.kubernetes.io/part-of":    "kognition",
		"app.kubernetes.io/component":  "migrations",
	}
	if !reflect.DeepEqual(metadata["labels"], labels) {
		return errors.New("migration labels changed; a new review is required")
	}
	data, ok := document["data"].(map[string]any)
	if !ok || len(data) != 37 {
		return errors.New("migration key set changed")
	}
	keys := make([]string, 0, len(data))
	for key := range data {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	digest := sha256.New()
	for _, key := range keys {
		value, ok := data[key].(string)
		if !ok {
			return errors.New("migration data must be strings")
		}
		_, _ = io.WriteString(digest, key+"\x00"+strings.ReplaceAll(value, "\r\n", "\n")+"\x00")
	}
	if observed := hex.EncodeToString(digest.Sum(nil)); observed != migrationDigest {
		return fmt.Errorf("migration content changed; a new review is required (observed SHA256 %s)", observed)
	}
	return nil
}

func run(reportPath, renderedRoot, provenancePath, expectedCandidateSHA256 string, now time.Time, output io.Writer) error {
	if err := verifyProvenance(provenancePath, reportPath, renderedRoot, expectedCandidateSHA256, now); err != nil {
		return err
	}
	contents, err := os.ReadFile(reportPath)
	if err != nil {
		return err
	}
	var scan report
	if err := json.Unmarshal(contents, &scan); err != nil {
		return err
	}
	if scan.SchemaVersion != 2 || len(scan.Results) == 0 {
		return errors.New("expected a nonempty Trivy v2 Kubernetes config report")
	}
	verified, blocked := 0, 0
	for _, entry := range scan.Results {
		if entry.Class != "config" || entry.Type != "kubernetes" || len(entry.Vulnerabilities)+len(entry.Secrets)+len(entry.Licenses) != 0 {
			return errors.New("only Kubernetes configuration reports are supported; other scanners remain independent gates")
		}
		for _, item := range entry.Misconfigurations {
			if item.Status == "PASS" {
				continue
			}
			if entry.Target == migrationTarget && item.ID == "KSV-0109" && item.Status == "FAIL" && item.Severity == "HIGH" && item.Message == migrationMessage {
				if err := verifyMigration(renderedRoot); err != nil {
					return fmt.Errorf("SQL false-positive verification failed: %w", err)
				}
				verified++
				continue
			}
			blocked++
			if _, err := fmt.Fprintf(output, "BLOCKED %s %s %s\n", entry.Target, item.ID, item.Severity); err != nil {
				return fmt.Errorf("write blocking finding: %w", err)
			}
		}
	}
	if verified != 1 {
		return fmt.Errorf("expected exactly one verified SQL finding, got %d", verified)
	}
	if _, err := fmt.Fprintf(output, "Verified SQL false positive: %s; payload SHA256 %s; remaining findings: %d\n", migrationTarget, migrationDigest, blocked); err != nil {
		return fmt.Errorf("write disposition summary: %w", err)
	}
	if blocked != 0 {
		return fmt.Errorf("%d configuration findings remain blocking", blocked)
	}
	return nil
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "plan" {
		os.Exit(runPlan(os.Args[2:], os.Stdout, os.Stderr))
	}
	if len(os.Args) > 1 && os.Args[1] == "identity-fixture" {
		os.Exit(runIdentityFixture(os.Args[2:], os.Stdout, os.Stderr))
	}
	reportPath := flag.String("report", "", "raw Trivy v2 Kubernetes configuration JSON report")
	renderedRoot := flag.String("rendered-root", "", "the exact helm --output-dir tree scanned by Trivy")
	provenancePath := flag.String("provenance", "", "provenance JSON binding the raw report to the exact scan input")
	expectedCandidateSHA256 := flag.String("candidate-sha256", "", "expected candidate SHA256 from the independently frozen candidate manifest")
	flag.Parse()
	if *reportPath == "" || *renderedRoot == "" || *provenancePath == "" || *expectedCandidateSHA256 == "" || flag.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "required: --report PATH --rendered-root PATH --provenance PATH --candidate-sha256 SHA256")
		os.Exit(2)
	}
	if err := run(*reportPath, *renderedRoot, *provenancePath, *expectedCandidateSHA256, time.Now(), os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
