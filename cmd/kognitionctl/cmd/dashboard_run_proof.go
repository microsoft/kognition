// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cmd

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
)

const maxRunProofReportBytes = 1 << 20

type dashboardRunProof struct {
	SchemaVersion   int                         `json:"schemaVersion"`
	RunID           string                      `json:"runId"`
	Scenario        string                      `json:"scenario"`
	ProtectedAction string                      `json:"protectedAction"`
	Mode            string                      `json:"mode"`
	Passed          bool                        `json:"passed"`
	Assertions      dashboardRunProofAssertions `json:"assertions"`
	ReportRef       string                      `json:"reportRef,omitempty"`
}

type dashboardRunProofAssertions struct {
	Total  int `json:"total"`
	Passed int `json:"passed"`
	Failed int `json:"failed"`
}

type dashboardRunProofReport struct {
	DemoRunID  string `json:"demoRunId"`
	Passed     bool   `json:"passed"`
	Assertions []struct {
		Passed bool `json:"passed"`
	} `json:"assertions"`
	RunProof struct {
		SchemaVersion   int    `json:"schemaVersion"`
		Scenario        string `json:"scenario"`
		ProtectedAction string `json:"protectedAction"`
		Mode            string `json:"mode"`
		ReportRef       string `json:"reportRef"`
	} `json:"runProof"`
}

func handleDashRunProof(w http.ResponseWriter, r *http.Request) {
	runID := strings.TrimSpace(r.URL.Query().Get("runId"))
	if dashboardRunProofPath == "" || runID == "" {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "run proof is not available"})
		return
	}

	report, err := readDashboardRunProof(dashboardRunProofPath, runID)
	if err != nil {
		status := http.StatusUnprocessableEntity
		if errors.Is(err, os.ErrNotExist) {
			status = http.StatusNotFound
		}
		writeJSON(w, status, map[string]string{"error": "run proof is not available"})
		return
	}
	writeJSON(w, http.StatusOK, report)
}

func readDashboardRunProof(path, runID string) (dashboardRunProof, error) {
	file, err := os.Open(path)
	if err != nil {
		return dashboardRunProof{}, err
	}
	defer func() { _ = file.Close() }()

	data, err := io.ReadAll(io.LimitReader(file, maxRunProofReportBytes+1))
	if err != nil || len(data) > maxRunProofReportBytes {
		return dashboardRunProof{}, errors.New("invalid run proof report")
	}
	var source dashboardRunProofReport
	if err := json.Unmarshal(data, &source); err != nil {
		return dashboardRunProof{}, err
	}
	scenario := strings.TrimSpace(source.RunProof.Scenario)
	protectedAction := strings.TrimSpace(source.RunProof.ProtectedAction)
	mode := strings.TrimSpace(source.RunProof.Mode)
	if source.DemoRunID != runID || len(source.DemoRunID) > 128 || source.RunProof.SchemaVersion != 1 ||
		scenario == "" || len(scenario) > 256 || protectedAction == "" || len(protectedAction) > 256 ||
		mode == "" || len(mode) > 64 {
		return dashboardRunProof{}, errors.New("invalid run proof report")
	}

	result := dashboardRunProof{
		SchemaVersion:   1,
		RunID:           source.DemoRunID,
		Scenario:        scenario,
		ProtectedAction: protectedAction,
		Mode:            mode,
		Passed:          source.Passed,
		Assertions:      dashboardRunProofAssertions{Total: len(source.Assertions)},
		ReportRef:       dashboardSafeReportRef(source.RunProof.ReportRef),
	}
	for _, assertion := range source.Assertions {
		if assertion.Passed {
			result.Assertions.Passed++
		} else {
			result.Assertions.Failed++
		}
	}
	return result, nil
}

func dashboardSafeReportRef(raw string) string {
	ref := strings.TrimSpace(raw)
	if ref == "" || len(ref) > 256 || strings.ContainsAny(ref, `/\:?#`) || ref == "." || ref == ".." {
		return ""
	}
	return ref
}
