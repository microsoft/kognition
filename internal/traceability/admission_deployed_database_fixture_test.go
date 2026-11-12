// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build integration

package traceability

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
)

func TestDeployedPostgresFixtureRestrictedContract(t *testing.T) {
	pod := deployedPostgresFixture("reviewed-workload", "reviewed-postgres@sha256:fixture")
	if pod.Name != "sg08-postgres" || pod.Namespace != "reviewed-workload" || len(pod.Spec.Containers) != 1 ||
		pod.Spec.Containers[0].Image != "reviewed-postgres@sha256:fixture" || pod.Spec.Containers[0].ImagePullPolicy != corev1.PullNever {
		t.Fatal("fixture changed its reviewed identity or image")
	}
	if pod.Spec.AutomountServiceAccountToken == nil || *pod.Spec.AutomountServiceAccountToken {
		t.Fatal("database fixture can receive a service-account token")
	}
	security := pod.Spec.SecurityContext
	if security == nil || security.RunAsNonRoot == nil || !*security.RunAsNonRoot ||
		security.RunAsUser == nil || *security.RunAsUser != 65534 ||
		security.RunAsGroup == nil || *security.RunAsGroup != 65534 ||
		security.FSGroup == nil || *security.FSGroup != 65534 ||
		security.SeccompProfile == nil || security.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Fatal("database fixture does not match the non-root production database profile")
	}
	container := pod.Spec.Containers[0]
	if container.SecurityContext == nil || container.SecurityContext.AllowPrivilegeEscalation == nil ||
		*container.SecurityContext.AllowPrivilegeEscalation || container.SecurityContext.ReadOnlyRootFilesystem == nil ||
		!*container.SecurityContext.ReadOnlyRootFilesystem || container.SecurityContext.Capabilities == nil ||
		len(container.SecurityContext.Capabilities.Drop) != 1 || container.SecurityContext.Capabilities.Drop[0] != "ALL" {
		t.Fatal("database fixture container is not restricted and read-only")
	}
	expected := map[string]string{
		"data":         "/var/lib/postgresql/data",
		"postgres-run": "/var/run/postgresql",
		"tmp":          "/tmp",
	}
	if len(pod.Spec.Volumes) != len(expected) || len(container.VolumeMounts) != len(expected) {
		t.Fatal("database fixture has unexpected writable volumes")
	}
	for _, volume := range pod.Spec.Volumes {
		if _, ok := expected[volume.Name]; !ok || volume.EmptyDir == nil {
			t.Fatalf("unexpected or non-ephemeral volume %s", volume.Name)
		}
	}
	for _, mount := range container.VolumeMounts {
		if expected[mount.Name] != mount.MountPath || mount.ReadOnly {
			t.Fatalf("incorrect writable database mount %+v", mount)
		}
	}
	if container.ReadinessProbe == nil || container.ReadinessProbe.PeriodSeconds != 1 {
		t.Fatal("database readiness checks were changed")
	}
}
