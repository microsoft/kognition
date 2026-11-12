// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package activation

import (
	"encoding/hex"
	"errors"
	"fmt"
	"reflect"
	"time"
)

const ApprovalVersion = "kognition.dev/activation/v1"
const ApprovalLifetime = 30 * time.Minute

type ActivationTarget struct {
	ClusterUID         string            `json:"clusterUID"`
	Endpoint           string            `json:"endpoint"`
	ServerVersion      string            `json:"serverVersion"`
	Release            string            `json:"release"`
	Namespace          string            `json:"namespace"`
	Namespaces         []string          `json:"workloadNamespaces"`
	Controller         string            `json:"controller"`
	Deployment         string            `json:"controllerDeployment"`
	AuthorityNamespace string            `json:"authorityNamespace"`
	Guard              string            `json:"guard"`
	ManifestSHA256     string            `json:"manifestSHA256"`
	ChartSHA256        string            `json:"chartSHA256"`
	ToolVersion        string            `json:"toolVersion"`
	Replicas           int32             `json:"replicas"`
	Images             map[string]string `json:"images"`
	Objects            []ObjectEvidence  `json:"objects"`
}

type ObjectEvidence struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Namespace  string `json:"namespace,omitempty"`
	Name       string `json:"name"`
	UID        string `json:"uid"`
	Generation int64  `json:"generation"`
	SHA256     string `json:"sha256"`
}

type GateEvidence struct {
	ReportSHA256 string    `json:"reportSHA256"`
	TestVersion  string    `json:"testVersion"`
	AcceptedAt   time.Time `json:"acceptedAt"`
}

type Approval struct {
	Version   string                  `json:"version"`
	Approver  string                  `json:"approver"`
	IssuedAt  time.Time               `json:"issuedAt"`
	ExpiresAt time.Time               `json:"expiresAt"`
	Target    ActivationTarget        `json:"target"`
	Gates     map[string]GateEvidence `json:"gates"`
}

func approvalsEqual(first, second Approval) bool {
	if first.Version != second.Version || first.Approver != second.Approver || !first.IssuedAt.Equal(second.IssuedAt) || !first.ExpiresAt.Equal(second.ExpiresAt) || !reflect.DeepEqual(first.Target, second.Target) || len(first.Gates) != len(second.Gates) {
		return false
	}
	for name, gate := range first.Gates {
		other, exists := second.Gates[name]
		if !exists || gate.ReportSHA256 != other.ReportSHA256 || gate.TestVersion != other.TestVersion || !gate.AcceptedAt.Equal(other.AcceptedAt) {
			return false
		}
	}
	return true
}

func ValidateApproval(approval Approval, target ActivationTarget, now time.Time) error {
	if approval.Version != ApprovalVersion || approval.Approver == "" {
		return errors.New("versioned administrator approval is required")
	}
	if approval.IssuedAt.IsZero() || approval.IssuedAt.After(now) || !approval.ExpiresAt.After(now) || !approval.ExpiresAt.After(approval.IssuedAt) || approval.ExpiresAt.Sub(approval.IssuedAt) > ApprovalLifetime {
		return errors.New("approval is expired, future-dated or exceeds the 30-minute lifetime")
	}
	if err := validateTarget(target); err != nil {
		return err
	}
	if !reflect.DeepEqual(approval.Target, target) {
		return errors.New("approval target does not match current candidate and live state")
	}
	for _, gate := range []string{"SG-03", "SG-07", "SG-08", "SG-09"} {
		evidence, found := approval.Gates[gate]
		if !found || !validSHA256(evidence.ReportSHA256) || evidence.TestVersion == "" || evidence.AcceptedAt.IsZero() || evidence.AcceptedAt.After(approval.IssuedAt) || approval.IssuedAt.Sub(evidence.AcceptedAt) > ApprovalLifetime {
			return fmt.Errorf("missing, invalid or stale acceptance evidence for %s", gate)
		}
	}
	return nil
}

func validateTarget(target ActivationTarget) error {
	if target.Deployment == "" || target.AuthorityNamespace == "" || target.AuthorityNamespace == target.Namespace {
		return errors.New("controller Deployment and separate approval authority are required")
	}
	if target.ClusterUID == "" || target.Endpoint == "" || target.ServerVersion == "" || target.Release == "" || target.Namespace == "" || target.Controller == "" || target.Guard == "" || target.Controller == target.Guard || target.ToolVersion == "" || target.Replicas < 1 || len(target.Namespaces) == 0 || len(target.Images) < 2 || len(target.Objects) == 0 || !validSHA256(target.ManifestSHA256) || !validSHA256(target.ChartSHA256) {
		return errors.New("incomplete activation target")
	}
	seen := map[string]bool{target.Namespace: true, target.AuthorityNamespace: true}
	for _, namespace := range target.Namespaces {
		if namespace == "" || seen[namespace] {
			return errors.New("workload namespace set must be unique and separate")
		}
		seen[namespace] = true
	}
	for _, digest := range target.Images {
		if !validSHA256(digest) {
			return errors.New("activation images must be bound to SHA256 digests")
		}
	}
	seen = map[string]bool{}
	for _, object := range target.Objects {
		key := object.APIVersion + "/" + object.Kind + "/" + object.Namespace + "/" + object.Name
		if object.APIVersion == "" || object.Kind == "" || object.Name == "" || object.UID == "" || object.Generation < 0 || !validSHA256(object.SHA256) || seen[key] {
			return errors.New("invalid or duplicate live object evidence")
		}
		seen[key] = true
	}
	return nil
}

func validSHA256(value string) bool {
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == 32 && len(value) == 64
}
