// Copyright 2026 The Gentian Authors. Licensed under Apache 2.0.

package oidc

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"sigs.k8s.io/controller-runtime/pkg/client"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

const managedByAttributePrefix = "managed-by-attribute-"

// NormalizeMBAGroupName returns the short managed-by-attribute group suffix.
func NormalizeMBAGroupName(ldapGroup string) string {
	trimmed := strings.TrimSpace(ldapGroup)
	if trimmed == "" {
		return ""
	}
	if strings.HasPrefix(trimmed, managedByAttributePrefix) {
		return strings.TrimPrefix(trimmed, managedByAttributePrefix)
	}
	return trimmed
}

// ManagedByAttributeGroupNames returns sorted unique MBA group suffixes from all
// cluster OIDCPackCatalog CRs (pack ldapGroup values plus extraManagedByAttributeGroups).
func ManagedByAttributeGroupNames(ctx context.Context, c client.Reader) ([]string, error) {
	if c == nil {
		return nil, fmt.Errorf("kubernetes client is required")
	}
	list := &gentianov1alpha1.OIDCPackCatalogList{}
	if err := c.List(ctx, list); err != nil {
		return nil, fmt.Errorf("list OIDCPackCatalog: %w", err)
	}
	seen := make(map[string]struct{})
	var names []string
	add := func(raw string) {
		name := NormalizeMBAGroupName(raw)
		if name == "" {
			return
		}
		if _, ok := seen[name]; ok {
			return
		}
		seen[name] = struct{}{}
		names = append(names, name)
	}
	for i := range list.Items {
		catalog := &list.Items[i]
		for _, pack := range catalog.Spec.Packs {
			add(pack.LDAPGroup)
		}
		for _, group := range catalog.Spec.ExtraManagedByAttributeGroups {
			add(group)
		}
	}
	sort.Strings(names)
	return names, nil
}
