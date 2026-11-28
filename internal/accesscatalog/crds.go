// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package accesscatalog

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"sigs.k8s.io/yaml"
)

// ShippedCRD is the part of a shipped CustomResourceDefinition that access
// control depends on.
type ShippedCRD struct {
	Group     string
	Kind      string
	Resource  string
	Scope     Scope
	HasStatus bool
}

// LoadShippedCRDs reads every CRD manifest in dir, in file name order.
func LoadShippedCRDs(dir string) ([]ShippedCRD, error) {
	paths, err := filepath.Glob(filepath.Join(dir, "*.yaml"))
	if err != nil {
		return nil, fmt.Errorf("list CRD manifests: %w", err)
	}
	if len(paths) == 0 {
		return nil, fmt.Errorf("no CRD manifests found in %s", dir)
	}
	sort.Strings(paths)
	crds := make([]ShippedCRD, 0, len(paths))
	for _, path := range paths {
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", path, err)
		}
		var crd apiextensionsv1.CustomResourceDefinition
		if err := yaml.UnmarshalStrict(raw, &crd); err != nil {
			return nil, fmt.Errorf("parse %s: %w", path, err)
		}
		if crd.Spec.Names.Kind == "" || crd.Spec.Names.Plural == "" || crd.Spec.Group == "" {
			return nil, fmt.Errorf("%s is not a complete CustomResourceDefinition", path)
		}
		hasStatus := false
		for _, version := range crd.Spec.Versions {
			if version.Subresources != nil && version.Subresources.Status != nil {
				hasStatus = true
			}
		}
		crds = append(crds, ShippedCRD{
			Group:     crd.Spec.Group,
			Kind:      crd.Spec.Names.Kind,
			Resource:  crd.Spec.Names.Plural,
			Scope:     Scope(crd.Spec.Scope),
			HasStatus: hasStatus,
		})
	}
	return crds, nil
}

// VerifyShippedCRDs checks that the catalog classifies every shipped CRD exactly
// once, with matching resource name and scope, and names no CRD that is not
// shipped. It reports every problem it finds, not only the first.
func (c Catalog) VerifyShippedCRDs(crds []ShippedCRD) error {
	var problems []string

	classified := map[string]Kind{}
	for _, kind := range c.Kinds {
		if _, duplicate := classified[kind.Name]; duplicate {
			problems = append(problems, fmt.Sprintf("kind %s is classified more than once", kind.Name))
			continue
		}
		classified[kind.Name] = kind
		switch {
		case kind.Class == "":
			problems = append(problems, fmt.Sprintf("kind %s has no access class", kind.Name))
		case !validClass(kind.Class):
			problems = append(problems, fmt.Sprintf("kind %s has unknown access class %q", kind.Name, kind.Class))
		}
	}

	shipped := map[string]bool{}
	for _, crd := range crds {
		shipped[crd.Kind] = true
		if crd.Group != APIGroup {
			problems = append(problems, fmt.Sprintf("CRD %s is in group %q; the chart ships only %s", crd.Kind, crd.Group, APIGroup))
			continue
		}
		kind, ok := classified[crd.Kind]
		if !ok {
			problems = append(problems, fmt.Sprintf("CRD %s (%s) is unclassified; add it to the access catalog", crd.Kind, crd.Resource))
			continue
		}
		if kind.Resource != crd.Resource {
			problems = append(problems, fmt.Sprintf("kind %s resource is %q in the catalog but %q in the CRD", crd.Kind, kind.Resource, crd.Resource))
		}
		if kind.Scope != crd.Scope {
			problems = append(problems, fmt.Sprintf("kind %s scope is %q in the catalog but %q in the CRD", crd.Kind, kind.Scope, crd.Scope))
		}
	}
	for name := range classified {
		if !shipped[name] {
			problems = append(problems, fmt.Sprintf("kind %s is in the catalog but is not shipped as a CRD", name))
		}
	}

	if len(problems) == 0 {
		return nil
	}
	sort.Strings(problems)
	return errors.New("access catalog does not match the shipped CRDs:\n  - " + strings.Join(problems, "\n  - "))
}

func validClass(class Class) bool {
	switch class {
	case ClassConfig, ClassKnowledge, ClassSubmit, ClassRuntime, ClassData, ClassGrant,
		ClassGovernance, ClassDecision, ClassPlatform, ClassClass, ClassSystem:
		return true
	}
	return false
}
