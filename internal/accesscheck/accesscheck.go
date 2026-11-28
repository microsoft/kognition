// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package accesscheck answers what the current Kubernetes principal may do with
// Kognition resources, using SelfSubjectAccessReview and SelfSubjectRulesReview.
// The answers are advisory: the API server and Kognition's admission checks decide
// every real request. Nothing here reads or prints a credential.
package accesscheck

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"

	authorizationv1 "k8s.io/api/authorization/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	authorizationv1client "k8s.io/client-go/kubernetes/typed/authorization/v1"

	"github.com/kognition-ai/kognition/internal/accesscatalog"
)

var verbPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)

// ResolveResource maps a Kognition kind or plural resource name, in any case, to
// the plural resource name and whether the kind is namespaced. It refuses anything
// it cannot name exactly instead of guessing.
func ResolveResource(input string) (resource string, namespaced bool, err error) {
	name := strings.ToLower(strings.TrimSpace(input))
	if name == "" || strings.ContainsAny(name, "/.* \t") {
		return "", false, fmt.Errorf("%q is not a Kognition kind or resource name", input)
	}
	for _, kind := range accesscatalog.Default().Kinds {
		if strings.ToLower(kind.Name) == name || kind.Resource == name {
			return kind.Resource, kind.Scope == accesscatalog.ScopeNamespaced, nil
		}
	}
	return "", false, fmt.Errorf("%q is not a Kognition kind or resource name", input)
}

// Request is one question: may the caller perform Verb on a Kognition resource.
type Request struct {
	Verb        string
	Kind        string
	Name        string
	Subresource string
	Namespace   string
}

// Decision is the cluster's answer.
type Decision struct {
	Allowed bool
	Reason  string
}

// CanI asks the cluster whether the caller may perform the action. An evaluation
// error or a failed call is an error and never an allow.
func CanI(ctx context.Context, authz authorizationv1client.AuthorizationV1Interface, request Request) (Decision, error) {
	if !verbPattern.MatchString(request.Verb) {
		return Decision{}, fmt.Errorf("%q is not a valid verb", request.Verb)
	}
	if request.Subresource != "" && !verbPattern.MatchString(request.Subresource) {
		return Decision{}, fmt.Errorf("%q is not a valid subresource", request.Subresource)
	}
	if strings.ContainsAny(request.Name, "/ \t") {
		return Decision{}, fmt.Errorf("%q is not a valid resource name", request.Name)
	}
	resource, namespaced, err := ResolveResource(request.Kind)
	if err != nil {
		return Decision{}, err
	}
	attributes := &authorizationv1.ResourceAttributes{
		Group: accesscatalog.APIGroup, Resource: resource, Subresource: request.Subresource, Verb: request.Verb, Name: request.Name,
	}
	if namespaced {
		attributes.Namespace = request.Namespace
	}
	review, err := authz.SelfSubjectAccessReviews().Create(ctx, &authorizationv1.SelfSubjectAccessReview{
		Spec: authorizationv1.SelfSubjectAccessReviewSpec{ResourceAttributes: attributes},
	}, metav1.CreateOptions{})
	if err != nil {
		return Decision{}, fmt.Errorf("ask the cluster: %w", err)
	}
	if review.Status.EvaluationError != "" {
		return Decision{}, fmt.Errorf("the cluster could not evaluate the request: %s", review.Status.EvaluationError)
	}
	return Decision{Allowed: review.Status.Allowed && !review.Status.Denied, Reason: review.Status.Reason}, nil
}

// Row is what the caller may do with one resource, with the resourceNames that
// limit it (empty means every object).
type Row struct {
	Resource      string   `json:"resource"`
	Verbs         []string `json:"verbs"`
	ResourceNames []string `json:"resourceNames,omitempty"`
	Wildcard      bool     `json:"wildcard,omitempty"`
}

// RuleSet is the caller's Kognition permissions in one namespace.
type RuleSet struct {
	Namespace       string `json:"namespace"`
	Rows            []Row  `json:"rules"`
	Incomplete      bool   `json:"incomplete,omitempty"`
	EvaluationError string `json:"evaluationError,omitempty"`
}

// Rules returns the caller's rules in a namespace, limited to the kognition.dev
// group (and wildcard rules that cover it). The review does not describe
// cluster-scoped kinds; use CanI for those.
func Rules(ctx context.Context, authz authorizationv1client.AuthorizationV1Interface, namespace string) (RuleSet, error) {
	if namespace == "" {
		return RuleSet{}, errors.New("a namespace is required")
	}
	review, err := authz.SelfSubjectRulesReviews().Create(ctx, &authorizationv1.SelfSubjectRulesReview{
		Spec: authorizationv1.SelfSubjectRulesReviewSpec{Namespace: namespace},
	}, metav1.CreateOptions{})
	if err != nil {
		return RuleSet{}, fmt.Errorf("ask the cluster: %w", err)
	}

	type key struct{ resource, names string }
	type accumulator struct {
		verbs    map[string]struct{}
		names    []string
		wildcard bool
	}
	merged := map[key]*accumulator{}
	for _, rule := range review.Status.ResourceRules {
		covers, groupWildcard := false, false
		for _, group := range rule.APIGroups {
			covers = covers || group == accesscatalog.APIGroup || group == "*"
			groupWildcard = groupWildcard || group == "*"
		}
		if !covers {
			continue
		}
		names := append([]string(nil), rule.ResourceNames...)
		sort.Strings(names)
		for _, resource := range rule.Resources {
			k := key{resource: resource, names: strings.Join(names, ",")}
			entry := merged[k]
			if entry == nil {
				entry = &accumulator{verbs: map[string]struct{}{}, names: names}
				merged[k] = entry
			}
			for _, verb := range rule.Verbs {
				entry.verbs[verb] = struct{}{}
				entry.wildcard = entry.wildcard || verb == "*"
			}
			entry.wildcard = entry.wildcard || resource == "*" || groupWildcard
		}
	}

	set := RuleSet{Namespace: namespace, Incomplete: review.Status.Incomplete, EvaluationError: review.Status.EvaluationError}
	for k, entry := range merged {
		verbs := make([]string, 0, len(entry.verbs))
		for verb := range entry.verbs {
			verbs = append(verbs, verb)
		}
		sort.Strings(verbs)
		set.Rows = append(set.Rows, Row{Resource: k.resource, Verbs: verbs, ResourceNames: entry.names, Wildcard: entry.wildcard})
	}
	sort.Slice(set.Rows, func(i, j int) bool {
		if set.Rows[i].Resource != set.Rows[j].Resource {
			return set.Rows[i].Resource < set.Rows[j].Resource
		}
		return strings.Join(set.Rows[i].ResourceNames, ",") < strings.Join(set.Rows[j].ResourceNames, ",")
	})
	return set, nil
}
