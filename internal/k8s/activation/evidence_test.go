// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package activation

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

func acceptanceFixture(test *testing.T, approval *Approval) map[string]string {
	test.Helper()
	digest, err := TargetSHA256(approval.Target)
	if err != nil {
		test.Fatal(err)
	}
	data := map[string]string{}
	for gate, claim := range approval.Gates {
		report := AcceptanceReport{Version: ApprovalVersion, Gate: gate, TargetSHA256: digest, TestVersion: claim.TestVersion, AcceptedAt: claim.AcceptedAt, Passed: true, Evidence: []string{strings.Repeat("b", 64)}}
		encoded, err := json.Marshal(report)
		if err != nil {
			test.Fatal(err)
		}
		data[gate+".json"] = string(encoded)
		claim.ReportSHA256 = fmt.Sprintf("%x", sha256.Sum256(encoded))
		approval.Gates[gate] = claim
	}
	return data
}

func TestAcceptanceReports(test *testing.T) {
	for _, scenario := range []string{"valid", "missing-runtime", "changed-bytes", "wrong-target", "failed", "open-finding", "no-source", "unknown-field"} {
		test.Run(scenario, func(test *testing.T) {
			approval := approvalFixture(time.Now().UTC())
			data := acceptanceFixture(test, &approval)
			var report AcceptanceReport
			if err := json.Unmarshal([]byte(data["SG-07.json"]), &report); err != nil {
				test.Fatal(err)
			}
			switch scenario {
			case "wrong-target":
				report.TargetSHA256 = strings.Repeat("c", 64)
			case "failed":
				report.Passed = false
			case "open-finding":
				report.Findings = []string{"pending"}
			case "no-source":
				report.Evidence = nil
			}
			encoded, err := json.Marshal(report)
			if err != nil {
				test.Fatal(err)
			}
			data["SG-07.json"] = string(encoded)
			if scenario == "unknown-field" {
				data["SG-07.json"] = strings.TrimSuffix(string(encoded), "}") + `,"bypass":true}`
			}
			claim := approval.Gates["SG-07"]
			claim.ReportSHA256 = fmt.Sprintf("%x", sha256.Sum256([]byte(data["SG-07.json"])))
			approval.Gates["SG-07"] = claim
			if scenario == "missing-runtime" {
				delete(data, "SG-07.json")
			}
			if scenario == "changed-bytes" {
				data["SG-07.json"] += " "
			}
			err = validateReports(approval, data)
			if (err == nil) != (scenario == "valid") {
				test.Fatalf("unexpected report result: %v", err)
			}
		})
	}
}
