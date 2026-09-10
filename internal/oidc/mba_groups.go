/*
Copyright 2026 Gentian Organization.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

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
func NormalizeMBAGroupName(groupName string) string {
	trimmed := strings.TrimSpace(groupName)
	if trimmed == "" {
		return ""
	}
	if strings.HasPrefix(trimmed, managedByAttributePrefix) {
		return strings.TrimPrefix(trimmed, managedByAttributePrefix)
	}
	return trimmed
}

// ManagedByAttributeGroupNames returns sorted unique MBA group suffixes from all
// cluster OIDCPackCatalog CRs (pack entitlementGroup values plus extraManagedByAttributeGroups).
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
			add(pack.EntitlementGroup)
		}
		for _, group := range catalog.Spec.ExtraManagedByAttributeGroups {
			add(group)
		}
	}
	sort.Strings(names)
	return names, nil
}
