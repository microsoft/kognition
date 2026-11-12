// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"testing"

	corev1 "k8s.io/api/core/v1"

	k8s "github.com/kognition-ai/kognition/api/k8s/v1alpha1"
	pgvectorprovider "github.com/kognition-ai/kognition/providers/pgvector"
)

// TestSetupSchemeRegistersRequiredTypes pins the scheme returned to
// newManager: both the kognition CRD group (KnowledgeBase et al.) and the
// core client-go types (Secrets, needed by LoadConnectionMaterial) must be
// registered so the controller-runtime manager can watch and read them.
func TestSetupSchemeRegistersRequiredTypes(t *testing.T) {
	scheme := setupScheme()
	if scheme == nil {
		t.Fatal("setupScheme returned nil")
	}
	if !scheme.Recognizes(k8s.GroupVersion.WithKind("KnowledgeBase")) {
		t.Fatal("kognition v1alpha1 KnowledgeBase not registered in scheme")
	}
	if !scheme.Recognizes(corev1.SchemeGroupVersion.WithKind("Secret")) {
		t.Fatal("core/v1 Secret not registered in scheme")
	}
}

// TestPgvectorConfigurationKindMatchesProvider guards against silent drift
// between the adapter's advertised GVK and the provider package's own
// constants. Any drift here would break scope authorization and the pluginsdk
// handshake at runtime.
func TestPgvectorConfigurationKindMatchesProvider(t *testing.T) {
	gvk := pgvectorConfigurationKind()
	if gvk.Group != pgvectorprovider.APIGroup {
		t.Fatalf("group = %q, want %q", gvk.Group, pgvectorprovider.APIGroup)
	}
	if gvk.Version != pgvectorprovider.APIVersion {
		t.Fatalf("version = %q, want %q", gvk.Version, pgvectorprovider.APIVersion)
	}
	if gvk.Kind != pgvectorprovider.Kind {
		t.Fatalf("kind = %q, want %q", gvk.Kind, pgvectorprovider.Kind)
	}
}
