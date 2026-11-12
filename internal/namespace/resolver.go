// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package namespace

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	v1alpha1 "github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/store"
)

// ErrCrossNamespaceDenied is returned when a cross-namespace reference has no matching grant.
var ErrCrossNamespaceDenied = errors.New("cross-namespace reference denied: no matching CrossNamespaceGrant")

// Resolver checks whether resource references are allowed based on
// CrossNamespaceGrant resources in the store.
type Resolver struct {
	store store.Store
}

// NewResolver creates a namespace reference resolver backed by the given store.
func NewResolver(s store.Store) *Resolver {
	return &Resolver{store: s}
}

// CheckReference validates that a resource in sourceNamespace is allowed to
// reference a resource identified by ref. If the reference is within the same
// namespace, it is always allowed. Cross-namespace references require a
// matching CrossNamespaceGrant.
func (r *Resolver) CheckReference(ctx context.Context, sourceNamespace string, ref v1alpha1.ResourceReference) error {
	targetNamespace := ref.Namespace
	if targetNamespace == "" || targetNamespace == sourceNamespace {
		// Same-namespace reference: always allowed.
		return nil
	}

	return r.checkGrant(ctx, targetNamespace, sourceNamespace, ref.Kind, ref.Name)
}

// CheckReferences validates multiple references at once. Returns the first denial error encountered.
func (r *Resolver) CheckReferences(ctx context.Context, sourceNamespace string, refs []v1alpha1.ResourceReference) error {
	for _, ref := range refs {
		if err := r.CheckReference(ctx, sourceNamespace, ref); err != nil {
			return err
		}
	}
	return nil
}

// checkGrant searches for a CrossNamespaceGrant in fromNamespace that allows
// toNamespace to reference the specified resource.
func (r *Resolver) checkGrant(ctx context.Context, fromNamespace, toNamespace, kind, name string) error {
	// List all CrossNamespaceGrant resources in the source namespace.
	grants, err := r.store.List(ctx, "CrossNamespaceGrant", store.ListOptions{
		Namespace: fromNamespace,
	})
	if err != nil {
		return fmt.Errorf("checking cross-namespace grants: %w", err)
	}

	for _, data := range grants {
		var grant v1alpha1.CrossNamespaceGrant
		if err := json.Unmarshal(data, &grant); err != nil {
			continue
		}
		if matchesGrant(grant.Spec, fromNamespace, toNamespace, kind, name) {
			return nil
		}
	}

	return fmt.Errorf("%w: %s/%s in namespace %q cannot be referenced from namespace %q",
		ErrCrossNamespaceDenied, kind, name, fromNamespace, toNamespace)
}

// matchesGrant checks whether a grant spec permits the given cross-namespace reference.
func matchesGrant(spec v1alpha1.CrossNamespaceGrantSpec, fromNamespace, toNamespace, kind, name string) bool {
	// Check namespace match.
	if spec.From != fromNamespace {
		return false
	}
	if spec.To != "*" && spec.To != toNamespace {
		return false
	}

	// Check resource match.
	for _, res := range spec.Resources {
		if res.Kind != kind {
			continue
		}
		if res.Name == "*" || res.Name == name {
			return true
		}
	}

	return false
}
