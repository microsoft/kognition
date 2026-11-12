// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package traceability

import (
	"encoding/json"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
)

func TestGitOpsRetainsOnlyExplicitlyReviewedMigrationJobs(test *testing.T) {
	for _, scenario := range []struct {
		name, mode string
		retain     bool
	}{
		{"existing-default", "kubernetes", false},
		{"gitops-retention", "kubernetes", true},
		{"standalone-hooks", "standalone", false},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			retain := "false"
			if scenario.retain {
				retain = "true"
			}
			found := 0
			for _, document := range renderControllerChart(test, "--set", "mode="+scenario.mode,
				"--set", "migrations.retainCompletedJobs="+retain) {
				var job batchv1.Job
				if err := json.Unmarshal(document.Raw, &job); err != nil {
					test.Fatal(err)
				}
				if job.Kind != "Job" || job.Labels["app.kubernetes.io/component"] != "migrations" {
					continue
				}
				found++
				if scenario.retain {
					if job.Spec.TTLSecondsAfterFinished != nil {
						test.Fatal("reviewed GitOps completion evidence must not be automatically deleted")
					}
				} else if job.Spec.TTLSecondsAfterFinished == nil || *job.Spec.TTLSecondsAfterFinished != 600 {
					test.Fatal("ordinary installation cleanup must retain its existing ten-minute default")
				}
				if scenario.mode == "kubernetes" && job.Annotations["helm.sh/hook"] != "" {
					test.Fatal("Kubernetes migrations must remain ordinary content-addressed Jobs")
				}
				if scenario.mode == "standalone" && job.Annotations["helm.sh/hook-delete-policy"] != "before-hook-creation,hook-succeeded" {
					test.Fatal("standalone Helm hook cleanup changed")
				}
			}
			if found != 1 {
				test.Fatalf("migration Job count = %d, want 1", found)
			}
		})
	}
}
