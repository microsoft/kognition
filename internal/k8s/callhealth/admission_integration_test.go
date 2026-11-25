// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build integration

package callhealth

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kognition-ai/kognition/internal/store"
)

// These tests run the real ConfigMapStore against a cluster that has the chart's
// ValidatingAdmissionPolicies installed, impersonating the workload identities.
// They are skipped unless KOGNITION_ADMISSION_CONTEXT names a kubeconfig context.
//
// Prepare a cluster with:
//
//	helm template kognition deploy/helm/kognition --namespace kognition-system \
//	  --set mode=kubernetes --set modelGateway.enabled=true --kube-version 1.31.0
//
// then apply the namespace, the ServiceAccounts, the Roles (plus a binding that
// grants the controller its Role) and every ValidatingAdmissionPolicy and binding.
//
// KOGNITION_ADMISSION_RELEASE (default "kognition") is the chart's full name and
// KOGNITION_ADMISSION_NAMESPACE (default "kognition-system") the release namespace.
func admissionClient(t *testing.T, serviceAccount string) client.Client {
	t.Helper()
	kubeContext := os.Getenv("KOGNITION_ADMISSION_CONTEXT")
	if kubeContext == "" {
		t.Skip("KOGNITION_ADMISSION_CONTEXT is not set")
	}
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, &clientcmd.ConfigOverrides{CurrentContext: kubeContext}).ClientConfig()
	if err != nil {
		t.Fatalf("load kubeconfig context %q: %v", kubeContext, err)
	}
	cfg = rest.CopyConfig(cfg)
	cfg.Impersonate = rest.ImpersonationConfig{UserName: fmt.Sprintf("system:serviceaccount:%s:%s-%s", admissionNamespace(), admissionRelease(), serviceAccount)}
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatalf("build client: %v", err)
	}
	return c
}

func admissionNamespace() string {
	if v := os.Getenv("KOGNITION_ADMISSION_NAMESPACE"); v != "" {
		return v
	}
	return "kognition-system"
}

func admissionRelease() string {
	if v := os.Getenv("KOGNITION_ADMISSION_RELEASE"); v != "" {
		return v
	}
	return "kognition"
}

func fullRecords(n int) []store.ModelCallHealth {
	now := time.Now().UTC()
	records := make([]store.ModelCallHealth, 0, n)
	for i := 0; i < n; i++ {
		records = append(records, store.ModelCallHealth{
			Target:               fmt.Sprintf("%0*d", store.MaxModelCallHealthTargetLength, i),
			Calls:                32,
			Faults:               32,
			ConsecutiveFaults:    32,
			ConsecutiveSuccesses: 32,
			LastFaultReason:      strings.Repeat("r", store.MaxModelCallHealthReasonLength),
			LastFaultAt:          now,
			LastSuccessAt:        now,
		})
	}
	return records
}

func TestAdmissionAllowsStoreWrites(t *testing.T) {
	ctx := context.Background()
	namespace := admissionNamespace()
	for _, tc := range []struct {
		serviceAccount string
		component      string
		options        []Option
	}{
		{"controller", "controller", []Option{WithGarbageCollection()}},
		{"controller", "model-gateway", nil},
		{"model-gateway", "model-gateway", nil},
	} {
		t.Run(tc.serviceAccount+" writes "+tc.component, func(t *testing.T) {
			c := admissionClient(t, tc.serviceAccount)
			s, err := NewConfigMapStore(c, namespace, tc.component, tc.options...)
			if err != nil {
				t.Fatal(err)
			}
			reporter := fmt.Sprintf("admission-test-%s-%s-%d", tc.serviceAccount, tc.component, time.Now().UnixNano())
			t.Cleanup(func() {
				// Only the controller may delete; use its identity for cleanup.
				cleaner := admissionClient(t, "controller")
				var configMaps corev1.ConfigMapList
				_ = cleaner.List(ctx, &configMaps, client.InNamespace(namespace), client.MatchingLabels{HealthLabel: tc.component})
				for i := range configMaps.Items {
					if strings.Contains(configMaps.Items[i].Data[DataKey], reporter) {
						_ = cleaner.Delete(ctx, &configMaps.Items[i])
					}
				}
			})

			small := []store.ModelCallHealth{{Target: "t1", Calls: 1, LastSuccessAt: time.Now().UTC()}}
			if err := s.ReplaceModelCallHealth(ctx, reporter, small); err != nil {
				t.Fatalf("create denied: %v", err)
			}
			// The second publication takes the update path, at the largest size
			// the store accepts.
			if err := s.ReplaceModelCallHealth(ctx, reporter, fullRecords(store.MaxModelCallHealthTargets)); err != nil {
				t.Fatalf("update of a maximum-size publication denied: %v", err)
			}
			records, err := s.ListModelCallHealth(ctx, time.Minute)
			if err != nil {
				t.Fatalf("list: %v", err)
			}
			if len(records) < store.MaxModelCallHealthTargets {
				t.Fatalf("listed %d records, want at least %d", len(records), store.MaxModelCallHealthTargets)
			}
		})
	}
}

func TestAdmissionDeniesForeignStoreWrites(t *testing.T) {
	ctx := context.Background()
	namespace := admissionNamespace()
	records := []store.ModelCallHealth{{Target: "t1", Calls: 1, LastSuccessAt: time.Now().UTC()}}
	for _, tc := range []struct {
		name           string
		serviceAccount string
		component      string
	}{
		{"model gateway writing the controller's objects", "model-gateway", "controller"},
		{"tool gateway writing model gateway objects", "tool-gateway", "model-gateway"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, err := NewConfigMapStore(admissionClient(t, tc.serviceAccount), namespace, tc.component)
			if err != nil {
				t.Fatal(err)
			}
			err = s.ReplaceModelCallHealth(ctx, fmt.Sprintf("denied-%d", time.Now().UnixNano()), records)
			if err == nil {
				t.Fatal("write was admitted, want it denied")
			}
			if !apierrors.IsForbidden(err) {
				t.Fatalf("error = %v, want Forbidden", err)
			}
		})
	}
}
