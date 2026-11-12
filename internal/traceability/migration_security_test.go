// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package traceability

import (
	"encoding/json"
	"slices"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
)

func TestMigrationJobUsesRestrictedSecurityContexts(test *testing.T) {
	for _, mode := range []string{"standalone", "kubernetes"} {
		test.Run(mode, func(test *testing.T) {
			found := false
			for _, document := range renderControllerRBAC(test, "--set", "mode="+mode) {
				var job batchv1.Job
				if err := json.Unmarshal(document.Raw, &job); err != nil {
					test.Fatal(err)
				}
				if job.Kind != "Job" || job.Labels["app.kubernetes.io/component"] != "migrations" {
					continue
				}
				found = true
				pod := job.Spec.Template.Spec
				security := pod.SecurityContext
				if security == nil || security.RunAsNonRoot == nil || !*security.RunAsNonRoot || security.RunAsUser == nil || *security.RunAsUser <= 0 || security.SeccompProfile == nil || security.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
					test.Fatal("migration Job must run as an explicit non-root user with runtime-default seccomp")
				}
				for _, container := range append(pod.InitContainers, pod.Containers...) {
					security := container.SecurityContext
					if security == nil || security.AllowPrivilegeEscalation == nil || *security.AllowPrivilegeEscalation || security.ReadOnlyRootFilesystem == nil || !*security.ReadOnlyRootFilesystem || security.Capabilities == nil || !slices.Contains(security.Capabilities.Drop, corev1.Capability("ALL")) {
						test.Fatalf("%s must use a read-only filesystem, drop capabilities and prohibit escalation", container.Name)
					}
				}
			}
			if !found {
				test.Fatal("migration Job missing")
			}
		})
	}
}
