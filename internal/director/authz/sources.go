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

package authz

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// CatalogueSource returns the object for a catalogue's slug -- the first half
// of a coordinate, so "main/nextcloud-base-ce" has source catalogue_source:main.
func CatalogueSource(name string) (string, error) {
	if name == "" || strings.ContainsAny(name, ":#/ \t\r\n") {
		return "", fmt.Errorf("%w: catalogue source %q", ErrInvalidID, name)
	}
	return "catalogue_source:" + name, nil
}

// ReconcileCatalogueSources makes catalogue_source#open equal to what the
// Cluster claim opens (AD-14): sources maps a catalogue's slug to the tenants
// it is open to, and tenants is every tenant this cluster has.
//
// Declarative, like the cluster's roles. A tenant the claim stops naming
// loses the tuple on the next pass, so closing a catalogue to a tenant is an
// edit to the claim -- reviewable, attributable, and with a date -- rather
// than something somebody has to remember to undo. That is the whole reason
// these live on the claim instead of in the director's environment: an
// environment variable changes when a Deployment rolls and leaves no record
// of who decided it.
//
// An open source is the one path by which software installs into a tenant
// without a signed statement from the store, so it is worth being plain about
// what it is: the platform administrator saying "this repository is mine and
// this tenant may install from it". Nothing is open by default.
//
// What is stored is read per tenant rather than per source, which is what
// makes a source DELETED from the claim lose its tuples as well -- reading
// only the sources the claim still names would never look at it.
func (c *OpenFGA) ReconcileCatalogueSources(ctx context.Context, tenants []string, sources map[string][]string) error {
	want := map[string]Tuple{}
	for name, open := range sources {
		object, err := CatalogueSource(name)
		if err != nil {
			return err
		}
		for _, tenant := range open {
			if tenant == "" {
				continue
			}
			t := Tuple{User: Tenant(tenant), Relation: "open", Object: object}
			want[key(t)] = t
		}
	}

	var writes, deletes []Tuple
	for _, tenant := range tenants {
		have, err := c.Read(ctx, Tuple{User: Tenant(tenant), Object: "catalogue_source:"})
		if err != nil {
			return fmt.Errorf("read open catalogue sources of tenant %s: %w", tenant, err)
		}
		for _, t := range have {
			if t.Relation != "open" {
				continue
			}
			if _, keep := want[key(t)]; keep {
				delete(want, key(t))
				continue
			}
			deletes = append(deletes, Tuple{User: t.User, Relation: t.Relation, Object: t.Object})
		}
	}
	// Anything left in want is for a tenant this cluster does not have, which
	// is a name in the claim that matches nothing. Writing it would be a
	// tuple nobody can use and nothing would ever remove; saying so is more
	// use than storing it.
	known := map[string]bool{}
	for _, t := range tenants {
		known[Tenant(t)] = true
	}
	for k, t := range want {
		if !known[t.User] {
			c.log.WarnContext(ctx, "the Cluster claim opens a catalogue to a tenant this cluster does not have",
				"source", t.Object, "tenant", t.User)
			delete(want, k)
			continue
		}
		writes = append(writes, t)
	}
	if len(writes) == 0 && len(deletes) == 0 {
		return nil
	}
	sort.Slice(writes, func(i, j int) bool { return key(writes[i]) < key(writes[j]) })
	sort.Slice(deletes, func(i, j int) bool { return key(deletes[i]) < key(deletes[j]) })

	if err := c.Write(ctx, writes, deletes); err != nil {
		return fmt.Errorf("reconcile catalogue sources: %w", err)
	}
	c.log.InfoContext(ctx, "catalogue sources reconciled",
		"sources", len(sources), "opened", len(writes), "closed", len(deletes))
	return nil
}

// BindEntryToSource records which catalogue serves an entry, which is what
// makes catalogue_entry#can_install's "open from source" leg reachable.
//
// It is a fact about the coordinate's own spelling -- "in-house/timesheets"
// is served by "in-house" and by nothing else -- so it decides nothing by
// itself: the tuple grants an install only where the source is also open to
// that tenant, and an open source is written from the Cluster claim.
//
// Written when an entry is first installed rather than for the whole
// catalogue up front, because a cluster holds no catalogue to enumerate
// (AD-3): an entry exists here from the moment somebody asks for it.
func (c *OpenFGA) BindEntryToSource(ctx context.Context, coordinate string) error {
	entry, err := CatalogueEntry(coordinate)
	if err != nil {
		return err
	}
	cat, _, _ := strings.Cut(coordinate, "/")
	source, err := CatalogueSource(cat)
	if err != nil {
		return err
	}
	t := Tuple{User: source, Relation: "source", Object: entry}
	have, err := c.Read(ctx, t)
	if err != nil {
		return fmt.Errorf("read source of %s: %w", entry, err)
	}
	if len(have) > 0 {
		return nil
	}
	if err := c.Write(ctx, []Tuple{t}, nil); err != nil {
		return fmt.Errorf("bind %s to %s: %w", entry, source, err)
	}
	return nil
}
