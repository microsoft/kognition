// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build integration

package accesscontrol

import (
	"context"
	"strings"
	"testing"

	authorizationv1 "k8s.io/api/authorization/v1"

	"github.com/kognition-ai/kognition/internal/accesscatalog"
	"github.com/kognition-ai/kognition/internal/accesscheck"
)

var standardVerbs = []string{"get", "list", "watch", "create", "update", "patch", "delete"}

// `kognitionctl auth` is advisory, so it must never claim more than the cluster
// allows and, for RBAC, should report everything the cluster allows (TC-0403).
func TestAccessCheckAgreesWithTheClusterForEveryRole(t *testing.T) {
	ctx := context.Background()
	namespaced := map[string]bool{}
	for _, crd := range h.crds {
		namespaced[crd.Resource] = crd.Scope == accesscatalog.ScopeNamespaced
	}

	for _, role := range h.catalog.Roles {
		t.Run(role.Name, func(t *testing.T) {
			user := userFor(role.Name)
			authz := h.users[user].kube.AuthorizationV1()

			for _, kind := range h.catalog.Kinds {
				for _, verb := range standardVerbs {
					attributes := authorizationv1.ResourceAttributes{Group: accesscatalog.APIGroup, Resource: kind.Resource, Verb: verb}
					if kind.Scope == accesscatalog.ScopeNamespaced {
						attributes.Namespace = nsA
					}
					want, err := h.review(ctx, user, attributes)
					if err != nil {
						t.Fatal(err)
					}
					got, err := accesscheck.CanI(ctx, authz, accesscheck.Request{Verb: verb, Kind: kind.Name, Namespace: nsA})
					if err != nil {
						t.Fatal(err)
					}
					if got.Allowed != want {
						t.Errorf("can-i %s %s: %v, the cluster says %v", verb, kind.Name, got.Allowed, want)
					}
				}
			}

			set, err := accesscheck.Rules(ctx, authz, nsA)
			if err != nil {
				t.Fatal(err)
			}
			if set.Incomplete || set.EvaluationError != "" {
				t.Fatalf("RBAC must be able to enumerate its rules: %+v", set)
			}
			listed := map[string]map[string]bool{}
			for _, row := range set.Rows {
				if row.Wildcard {
					t.Errorf("a shipped role produced a wildcard row: %+v", row)
				}
				resource, subresource, _ := strings.Cut(row.Resource, "/")
				for _, verb := range row.Verbs {
					attributes := authorizationv1.ResourceAttributes{Group: accesscatalog.APIGroup, Resource: resource, Subresource: subresource, Verb: verb}
					if namespaced[resource] {
						attributes.Namespace = nsA
					}
					allowed, err := h.review(ctx, user, attributes)
					if err != nil {
						t.Fatal(err)
					}
					if !allowed {
						t.Errorf("auth list claims %s on %s, the cluster denies it", verb, row.Resource)
					}
				}
				if listed[row.Resource] == nil {
					listed[row.Resource] = map[string]bool{}
				}
				for _, verb := range row.Verbs {
					listed[row.Resource][verb] = true
				}
			}
			for _, kind := range h.catalog.Kinds {
				if kind.Scope != accesscatalog.ScopeNamespaced {
					continue
				}
				for _, verb := range standardVerbs {
					if h.catalog.Permits(role.Name, kind.Name, "", verb) && !listed[kind.Resource][verb] {
						t.Errorf("auth list omits %s on %s, which the role allows", verb, kind.Resource)
					}
				}
			}
		})
	}

	t.Run("an unbound principal sees nothing", func(t *testing.T) {
		authz := h.users[unboundUser].kube.AuthorizationV1()
		set, err := accesscheck.Rules(ctx, authz, nsA)
		if err != nil || len(set.Rows) != 0 {
			t.Fatalf("rows = %+v, err = %v", set.Rows, err)
		}
		if decision, err := accesscheck.CanI(ctx, authz, accesscheck.Request{Verb: "get", Kind: "Agent", Namespace: nsA}); err != nil || decision.Allowed {
			t.Fatalf("decision = %+v, err = %v", decision, err)
		}
	})

	t.Run("custom verbs and scoped names are reported", func(t *testing.T) {
		consumer := h.users[userFor("kognition-consumer")].kube.AuthorizationV1()
		for _, request := range []accesscheck.Request{
			{Verb: "use", Kind: "Workflow", Name: "any", Namespace: nsA},
			{Verb: "invoke", Kind: "Agent", Name: "any", Namespace: nsA},
		} {
			decision, err := accesscheck.CanI(ctx, consumer, request)
			if err != nil || !decision.Allowed {
				t.Errorf("%+v: %+v, %v", request, decision, err)
			}
		}
		if decision, err := accesscheck.CanI(ctx, consumer, accesscheck.Request{Verb: "use", Kind: "Workflow", Name: "any", Namespace: nsB}); err != nil || decision.Allowed {
			t.Errorf("a binding in team-a must not reach team-b: %+v, %v", decision, err)
		}
		approver := h.users[userFor("kognition-approver")].kube.AuthorizationV1()
		if decision, err := accesscheck.CanI(ctx, approver, accesscheck.Request{Verb: "approve", Kind: "RuntimeApproval", Subresource: "decision", Name: "x", Namespace: nsA}); err != nil || !decision.Allowed {
			t.Errorf("approve on runtimeapprovals/decision: %+v, %v", decision, err)
		}
	})
}
