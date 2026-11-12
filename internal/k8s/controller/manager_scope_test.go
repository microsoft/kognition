// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"testing"

	ctrl "sigs.k8s.io/controller-runtime"
)

func TestManagedNamespacesFailClosed(test *testing.T) {
	for _, namespaces := range [][]string{nil, {""}, {"*"}, {"control", ""}, {"Uppercase"}, {"control,tenant"}} {
		if err := configureManagedNamespaces(&ctrl.Options{}, namespaces); err == nil {
			test.Errorf("accepted invalid namespaces: %q", namespaces)
		}
	}
	options := &ctrl.Options{LeaderElectionNamespace: "control"}
	configureConfigMapCache(options)
	if err := configureManagedNamespaces(options, []string{"control", "tenant", "control"}); err != nil {
		test.Fatal(err)
	}
	if len(options.Cache.DefaultNamespaces) != 2 {
		test.Fatalf("cache namespaces: %v", options.Cache.DefaultNamespaces)
	}
	for _, namespace := range []string{"control", "tenant"} {
		if _, allowed := options.Cache.DefaultNamespaces[namespace]; !allowed {
			test.Errorf("cache does not watch %s", namespace)
		}
	}
	if len(options.Cache.ByObject) != 1 {
		test.Fatal("namespace configuration discarded the ConfigMap cache policy")
	}
	options.LeaderElectionNamespace = "unrelated"
	if err := configureManagedNamespaces(options, []string{"control", "tenant"}); err == nil {
		test.Fatal("accepted leader election outside the managed namespaces")
	}
}
