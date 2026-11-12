// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package traceability

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
)

func TestControllerNativeMutationInventory(test *testing.T) {
	expected := map[string]string{
		"control//events/":                                  "create,patch",
		"control/events.k8s.io/events/":                     "create,patch",
		"control//configmaps/":                              "create,delete,update",
		"control/coordination.k8s.io/leases/":               "create,update",
		"tenant//events/":                                   "create,patch",
		"tenant/events.k8s.io/events/":                      "create,patch",
		"tenant//secrets/":                                  "create,delete,patch,update",
		"tenant//configmaps/":                               "create,delete",
		"tenant//configmaps/kognition-knowledge-gateway-ca": "update",
		"tenant//pods/":                                     "create,delete,patch,update",
		"tenant//pods/attach/":                              "create",
		"tenant//services/":                                 "create,delete,patch,update",
		"tenant/apps/deployments/":                          "create,delete,patch,update",
		"tenant/batch/jobs/":                                "create,delete,patch,update",
		"tenant/networking.k8s.io/networkpolicies/":         "create,delete,patch,update",
		"tenant/coordination.k8s.io/leases/":                "create,update",
	}
	want := map[string]bool{}
	for resource, verbs := range expected {
		for _, verb := range strings.Split(verbs, ",") {
			want[resource+"/"+verb] = true
		}
	}
	got := map[string]bool{}
	for _, document := range renderControllerRBAC(test, "--set", "k8sController.managedNamespaces[0]=tenant") {
		var role rbacv1.Role
		if err := json.Unmarshal(document.Raw, &role); err != nil {
			test.Fatal(err)
		}
		if (role.Kind != "Role" && role.Kind != "ClusterRole") || role.Labels["app.kubernetes.io/component"] != "k8s-controller" {
			continue
		}
		for _, rule := range role.Rules {
			for _, group := range rule.APIGroups {
				if group == "kognition.dev" || group == "knowledge.kognition.dev" {
					continue
				}
				for _, resource := range rule.Resources {
					for _, verb := range rule.Verbs {
						if verb == "get" || verb == "list" || verb == "watch" {
							continue
						}
						got[role.Namespace+"/"+group+"/"+resource+"/"+strings.Join(rule.ResourceNames, ",")+"/"+verb] = true
					}
				}
			}
		}
	}
	if !reflect.DeepEqual(got, want) {
		test.Fatalf("native mutation inventory changed; update independent enforcement evidence: got %v, want %v", got, want)
	}
}
