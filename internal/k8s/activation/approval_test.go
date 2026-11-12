// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package activation

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func approvalFixture(now time.Time) Approval {
	digest := strings.Repeat("a", 64)
	return Approval{
		Version: ApprovalVersion, Approver: "installation-security-owner",
		IssuedAt: now.Add(-time.Minute), ExpiresAt: now.Add(time.Minute),
		Target: ActivationTarget{
			ClusterUID: "cluster-uid", Endpoint: "https://127.0.0.1:6443", ServerVersion: "v1.30.3",
			Release: "release", Namespace: "control", Namespaces: []string{"tenant"}, Controller: "controller", Guard: "guard",
			Deployment: "controller-deployment", AuthorityNamespace: "authority",
			ManifestSHA256: digest, ChartSHA256: digest, ToolVersion: "tested-tool-digest", Replicas: 1,
			Images:  map[string]string{"controller": digest, "guard": digest},
			Objects: []ObjectEvidence{{APIVersion: "v1", Kind: "Namespace", Name: "tenant", UID: "namespace-uid", SHA256: digest}},
		},
		Gates: map[string]GateEvidence{
			"SG-03": {ReportSHA256: digest, TestVersion: "deployed-guard", AcceptedAt: now.Add(-2 * time.Minute)},
			"SG-07": {ReportSHA256: digest, TestVersion: "enforcing-CNI-stdio", AcceptedAt: now.Add(-2 * time.Minute)},
			"SG-08": {ReportSHA256: digest, TestVersion: "producer-lifecycle", AcceptedAt: now.Add(-2 * time.Minute)},
			"SG-09": {ReportSHA256: digest, TestVersion: "trust-ADR", AcceptedAt: now.Add(-2 * time.Minute)},
		},
	}
}

func TestValidateApproval(test *testing.T) {
	now := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	for _, scenario := range []struct {
		name   string
		change func(*Approval)
	}{
		{"valid", func(*Approval) {}},
		{"cluster", func(value *Approval) { value.Target.ClusterUID = "other" }},
		{"values", func(value *Approval) { value.Target.ManifestSHA256 = strings.Repeat("b", 64) }},
		{"chart", func(value *Approval) { value.Target.ChartSHA256 = strings.Repeat("b", 64) }},
		{"images", func(value *Approval) { value.Target.Images["guard"] = strings.Repeat("b", 64) }},
		{"namespaces", func(value *Approval) { value.Target.Namespaces = []string{"other"} }},
		{"UID", func(value *Approval) { value.Target.Objects[0].UID = "replacement" }},
		{"generation", func(value *Approval) { value.Target.Objects[0].Generation++ }},
		{"tool", func(value *Approval) { value.Target.ToolVersion = "other" }},
		{"replicas", func(value *Approval) { value.Target.Replicas++ }},
		{"expired", func(value *Approval) { value.ExpiresAt = now }},
		{"future", func(value *Approval) { value.IssuedAt = now.Add(time.Second) }},
		{"excessive-lifetime", func(value *Approval) { value.ExpiresAt = now.Add(time.Hour) }},
		{"missing-runtime-gate", func(value *Approval) { delete(value.Gates, "SG-07") }},
		{"missing-producer-gate", func(value *Approval) { delete(value.Gates, "SG-08") }},
		{"missing-trust", func(value *Approval) { delete(value.Gates, "SG-09") }},
		{"no-approver", func(value *Approval) { value.Approver = "" }},
		{"stale-evidence", func(value *Approval) {
			evidence := value.Gates["SG-03"]
			evidence.AcceptedAt = now.Add(-time.Hour)
			value.Gates["SG-03"] = evidence
		}},
		{"future-evidence", func(value *Approval) {
			evidence := value.Gates["SG-03"]
			evidence.AcceptedAt = now
			value.Gates["SG-03"] = evidence
		}},
		{"boolean-is-not-evidence", func(value *Approval) { value.Gates["SG-07"] = GateEvidence{TestVersion: "passed"} }},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			approval := approvalFixture(now)
			encoded, _ := json.Marshal(approval.Target)
			var target ActivationTarget
			if err := json.Unmarshal(encoded, &target); err != nil {
				test.Fatal(err)
			}
			scenario.change(&approval)
			err := ValidateApproval(approval, target, now)
			if (err == nil) != (scenario.name == "valid") {
				test.Fatalf("unexpected validation result: %v", err)
			}
		})
	}
}
