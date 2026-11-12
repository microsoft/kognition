// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package traceability

import (
	"os"
	"strings"
	"testing"
)

func TestStandaloneDeploymentPreservesRuntimeAuthSecretAcrossHelmUpgrades(t *testing.T) {
	scriptBytes, err := os.ReadFile("../../make.ps1")
	if err != nil {
		t.Fatalf("read make.ps1: %v", err)
	}
	_, script, found := strings.Cut(string(scriptBytes), "function Target-DeployStandalone {")
	if !found {
		t.Fatal("standalone deployment target is missing")
	}
	script, _, found = strings.Cut(script, "function Target-UndeployStandalone {")
	if !found {
		t.Fatal("standalone deployment target boundary is missing")
	}

	if count := strings.Count(script, "$runtimeAuthSecret = Ensure-LocalRuntimeAuthSecret"); count != 1 {
		t.Fatalf("standalone deployment initializes stable runtime auth Secret %d times; want 1", count)
	}
	if count := strings.Count(script, "'--set', \"runtimeAuth.existingSecret=$runtimeAuthSecret\""); count != 1 {
		t.Fatalf("standalone deployment configures existing runtime auth Secret %d times; want 1", count)
	}
	if count := strings.Count(script, "$runtimeAuthBackup = Backup-LocalRuntimeAuthSecret $runtimeAuthSecret"); count != 1 {
		t.Fatalf("standalone deployment backs up runtime auth Secret %d times; want 1", count)
	}
	if count := strings.Count(script, "Restore-LocalRuntimeAuthSecret $runtimeAuthBackup"); count != 1 {
		t.Fatalf("standalone deployment restores runtime auth Secret %d times; want 1", count)
	}
	if strings.Contains(script, "'--set', 'runtimeAuth.create=true'") {
		t.Fatal("standalone deployment must not ask Helm to regenerate runtime auth credentials")
	}
}
