// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package activation

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"
)

type AcceptanceReport struct {
	Version      string    `json:"version"`
	Gate         string    `json:"gate"`
	TargetSHA256 string    `json:"targetSHA256"`
	TestVersion  string    `json:"testVersion"`
	AcceptedAt   time.Time `json:"acceptedAt"`
	Passed       bool      `json:"passed"`
	Findings     []string  `json:"findings"`
	Evidence     []string  `json:"evidence"`
}

func TargetSHA256(target ActivationTarget) (string, error) {
	encoded, err := json.Marshal(target)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", sha256.Sum256(encoded)), nil
}

func validateReports(approval Approval, data map[string]string) error {
	targetDigest, err := TargetSHA256(approval.Target)
	if err != nil {
		return err
	}
	for _, gate := range []string{"SG-03", "SG-07", "SG-08", "SG-09"} {
		raw := data[gate+".json"]
		claim, found := approval.Gates[gate]
		if !found || raw == "" || fmt.Sprintf("%x", sha256.Sum256([]byte(raw))) != claim.ReportSHA256 {
			return fmt.Errorf("missing or changed acceptance report %s", gate)
		}
		var report AcceptanceReport
		if err := decodeStrict([]byte(raw), &report); err != nil {
			return fmt.Errorf("acceptance report %s: %w", gate, err)
		}
		if report.Version != ApprovalVersion || report.Gate != gate || report.TargetSHA256 != targetDigest || report.TestVersion != claim.TestVersion || !report.AcceptedAt.Equal(claim.AcceptedAt) || !report.Passed || len(report.Findings) != 0 || len(report.Evidence) == 0 {
			return fmt.Errorf("acceptance report %s is incomplete, failing or for another target", gate)
		}
		for _, digest := range report.Evidence {
			if !validSHA256(digest) {
				return fmt.Errorf("acceptance report %s needs source artifact SHA256 digests", gate)
			}
		}
	}
	return nil
}

func decodeStrict(content []byte, destination interface{}) error {
	decoder := json.NewDecoder(bytes.NewReader(content))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	if err := decoder.Decode(new(interface{})); err != io.EOF {
		return errors.New("exactly one JSON value is required")
	}
	return nil
}
