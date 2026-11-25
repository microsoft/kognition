// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build integration

package traceability

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	admissionv1 "k8s.io/api/admissionregistration/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func controllerAdmissionTypecheckPolicyInventory() map[string]bool {
	return map[string]bool{
		"security-test-kognition-control-leases":            false,
		"security-test-kognition-control-gateway-leases":    false,
		"security-test-kognition-control-services":          false,
		"security-test-kognition-control-network":           false,
		"security-test-kognition-control-state":             false,
		"security-test-kognition-control-credentials":       false,
		"security-test-kognition-control-workload":          false,
		"security-test-kognition-control-model-dispatch":    false,
		"security-test-kognition-control-runtime-recovery":  false,
		"security-test-kognition-control-model-call-health": false,
	}
}

func TestControllerAdmissionTypecheckInventoryMatchesChart(test *testing.T) {
	expected := controllerAdmissionTypecheckPolicyInventory()
	for _, document := range renderControllerRBAC(test, "--set", "k8sController.managedNamespaces[0]=tenant") {
		var policy admissionv1.ValidatingAdmissionPolicy
		if err := json.Unmarshal(document.Raw, &policy); err != nil {
			test.Fatal(err)
		}
		if policy.Kind != "ValidatingAdmissionPolicy" {
			continue
		}
		if seen, exists := expected[policy.Name]; !exists || seen {
			test.Fatal("unexpected/duplicate typecheck policy", policy.Name)
		}
		expected[policy.Name] = true
	}
	for name, seen := range expected {
		if !seen {
			test.Fatal("missing typecheck policy", name)
		}
	}
}

func TestControllerAdmissionPolicyTypeChecking(test *testing.T) {
	kubeconfig := os.Getenv("KOGNITION_ADMISSION_KUBECONFIG")
	if kubeconfig == "" {
		test.Skip("KOGNITION_ADMISSION_KUBECONFIG must point to an isolated local control plane with kube-controller-manager")
	}
	config, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		test.Fatal(err)
	}
	config.Timeout = 10 * time.Second
	if !strings.HasPrefix(config.Host, "https://127.0.0.1:") || os.Getenv("KOGNITION_ADMISSION_ISOLATED") != "true" {
		test.Fatal("typechecking requires explicit isolated=true and a loopback control plane")
	}
	admin, err := client.New(config, client.Options{Scheme: scheme.Scheme})
	if err != nil {
		test.Fatal(err)
	}
	cluster, err := kubernetes.NewForConfig(config)
	if err != nil {
		test.Fatal(err)
	}
	version, err := cluster.Discovery().ServerVersion()
	if err != nil {
		test.Fatal(err)
	}
	expectedVersion := os.Getenv("KOGNITION_ADMISSION_EXPECTED_VERSION")
	if expectedVersion == "" || version.GitVersion != expectedVersion {
		test.Fatalf("explicit KOGNITION_ADMISSION_EXPECTED_VERSION must match server: expected=%q actual=%q", expectedVersion, version.GitVersion)
	}
	test.Logf("Actual controller-manager typechecking: server=%s target=%s", version.GitVersion, config.Host)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	policies := cluster.AdmissionregistrationV1().ValidatingAdmissionPolicies()
	expected := controllerAdmissionTypecheckPolicyInventory()
	for _, document := range renderControllerRBAC(test, "--set", "k8sController.managedNamespaces[0]=tenant") {
		var metadata metav1.TypeMeta
		if err := json.Unmarshal(document.Raw, &metadata); err != nil {
			test.Fatal(err)
		}
		if metadata.Kind != "ValidatingAdmissionPolicy" {
			continue
		}
		var policy admissionv1.ValidatingAdmissionPolicy
		if err := json.Unmarshal(document.Raw, &policy); err != nil {
			test.Fatal(err)
		}
		if seen, exists := expected[policy.Name]; !exists || seen {
			test.Fatalf("unexpected or duplicate admission policy: %s", policy.Name)
		}
		expected[policy.Name] = true
		test.Run(policy.Name, func(test *testing.T) {
			policy.Name += "-" + uuid.NewString()[:8]
			var owned []client.Object
			test.Cleanup(func() {
				for _, object := range owned {
					created := object.(*admissionv1.ValidatingAdmissionPolicy)
					cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
					defer cancel()
					if err := policies.Delete(cleanup, created.Name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &created.UID}}); err != nil && !apierrors.IsNotFound(err) {
						test.Error(err)
						continue
					}
					if err := wait.PollUntilContextCancel(cleanup, 100*time.Millisecond, true, func(ctx context.Context) (bool, error) {
						current, err := policies.Get(ctx, created.Name, metav1.GetOptions{})
						if apierrors.IsNotFound(err) {
							return true, nil
						}
						if err == nil && current.UID != created.UID {
							return false, fmt.Errorf("replacement UID for %s", created.Name)
						}
						return false, err
					}); err != nil {
						test.Error(err)
					} else {
						test.Logf("Cleanup confirmed policy %s UID=%s", created.Name, created.UID)
					}
				}
			})
			if err := createAdmissionFixture(ctx, admin, &policy, &owned); err != nil {
				test.Fatal(err)
			}
			created := &policy
			if err := wait.PollUntilContextCancel(ctx, 200*time.Millisecond, true, func(ctx context.Context) (bool, error) {
				current, err := policies.Get(ctx, created.Name, metav1.GetOptions{})
				if err != nil {
					return false, err
				}
				if current.UID != created.UID || current.Generation != created.Generation || current.DeletionTimestamp != nil {
					test.Fatal("policy identity or generation changed during typechecking")
				}
				if current.Status.ObservedGeneration != current.Generation || current.Status.TypeChecking == nil {
					return false, nil
				}
				if len(current.Status.TypeChecking.ExpressionWarnings) != 0 {
					test.Fatalf("policy expression warnings: %+v", current.Status.TypeChecking.ExpressionWarnings)
				}
				test.Logf("Verified policy %s UID=%s generation=%d observed=%d warnings=0", current.Name, current.UID, current.Generation, current.Status.ObservedGeneration)
				return true, nil
			}); err != nil {
				test.Fatalf("policy typechecking did not complete: %v", err)
			}
		})
	}
	for name, seen := range expected {
		if !seen {
			test.Errorf("required admission policy missing: %s", name)
		}
	}
}
