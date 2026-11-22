/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package authz

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// What of a tenant's rights exists only in the store.
//
// The store is a projection in part (AD-12). Which cluster a tenant is
// attached to, which apps it has and the roles they declare, which positions
// it has and the roles each holds follow from the tenant's entry in git, who
// approves its perimeter from the Cluster claim, and the operator
// writes all of that again wherever the tenant is (ReconcileTenants,
// ReconcileApps, ReconcilePositions). A backup that carried those would
// carry a second copy of git, and a restore that wrote them back would
// overrule it.
//
// What a backup has to carry is the rest, and it is of two kinds:
//
//   - an entry on the tenant, on one of its apps, declared roles or
//     positions that no projection would write: who holds a position, and a
//     role somebody was given beside one;
//   - a default the projection writes once and then leaves alone, which the
//     store no longer holds: `operated_by`, the tenant's consent to being
//     administered by its cluster. The missing entry is the tenant's answer,
//     and a set of entries cannot say that one is missing -- so it is
//     recorded as withdrawn.
//
// Nothing here knows how either came about: it is somebody's act, and a
// backup must not lose it.

// Reader reads entries of the store.
type Reader interface {
	Read(ctx context.Context, filter Tuple) ([]Tuple, error)
}

// StoreOnly is what a tenant has in the store that follows from nothing
// else.
type StoreOnly struct {
	// Granted are entries on the tenant, its apps, their declared roles and
	// its positions that no projection writes.
	Granted []Tuple
	// Withdrawn are entries the tenant started with that the store no longer
	// holds.
	Withdrawn []Tuple
}

// ErrNotProjected is a tenant the store does not know yet: the operator's
// projection has not attached it to its cluster. What is missing then is
// not withdrawn, it is not written yet.
var ErrNotProjected = fmt.Errorf("the tenant is not attached to its cluster in the rights store yet")

// tenantDefaults is what ReconcileTenants holds a tenant to: standing, the
// entries it writes whenever one is missing; once, the entry it writes when
// the tenant is first attached and never again.
func tenantDefaults(cluster, tenant string) (standing, once []Tuple) {
	clusterObj, obj := Cluster(cluster), Tenant(tenant)
	standing = append(standing, Tuple{User: clusterObj, Relation: "cluster", Object: obj})
	once = []Tuple{{User: clusterObj, Relation: "operated_by", Object: obj}}
	return standing, once
}

// StoreOnlyOf reads what a tenant has in the store that follows from
// nothing else.
//
// apps are the tenant's installed apps and positions its declared positions
// (the three every tenant has among them), as the projections are given
// them. held are further app names the tenant holds something of -- apps
// that were uninstalled with their data kept -- whose objects are read too:
// a right granted on such an app is still in the store.
//
// The entries that say what an app, a declared role or a position IS are
// the projections' alone: they remove every such entry they would not write
// (ReconcileApps, ReconcilePositions), so one they would not write is not a
// right somebody holds but one that is about to go, and is not carried. The
// same holds for every role a position holds; what is carried of a position
// is who holds it.
func StoreOnlyOf(ctx context.Context, store Reader, cluster, tenant string, apps []InstalledApp, positions []PositionSpec, held []string) (StoreOnly, error) {
	var out StoreOnly
	standing, once := tenantDefaults(cluster, tenant)
	derived := map[string]bool{}
	for _, t := range standing {
		derived[key(t)] = true
	}
	for _, t := range once {
		derived[key(t)] = true
	}
	want, err := appTuples(tenant, apps)
	if err != nil {
		return out, err
	}
	objects := map[string]bool{}
	for k, t := range want {
		derived[k] = true
		objects[t.Object] = true
	}
	if _, err := positionTuples(tenant, positions); err != nil {
		return out, err
	}
	for _, p := range positions {
		objects[Position(tenant, p.Name)] = true
	}
	for _, name := range held {
		if name != "" && !strings.ContainsAny(name, "#/ \t\r\n") {
			objects[App(tenant, name)] = true
		}
	}

	onTenant, err := store.Read(ctx, Tuple{Object: Tenant(tenant)})
	if err != nil {
		return out, fmt.Errorf("read the entries on %s: %w", Tenant(tenant), err)
	}
	have := map[string]bool{}
	for _, t := range onTenant {
		have[key(plain(t))] = true
	}
	if !have[key(standing[0])] {
		return out, fmt.Errorf("%w: %s", ErrNotProjected, tenant)
	}
	for _, t := range onTenant {
		// Who approves the tenant's perimeter and who reads its evidence are
		// the Cluster claim's to say, and the projection removes every entry
		// it does not name. Present or not, it is not a right only the store
		// knows, and it does not travel in a bundle: on the cluster a tenant
		// is imported to, that is its administrator's decision.
		if claimTenantRelation(t.Relation) {
			continue
		}
		if p := plain(t); !derived[key(p)] && !heldByPosition(p, tenant) && !IsDerived(p.Relation) {
			out.Granted = append(out.Granted, p)
		}
	}
	// The platform tenant is always operated by its cluster: the projection
	// writes the entry back whenever it is missing, so it is never withdrawn.
	if tenant != PlatformTenant {
		for _, t := range once {
			if !have[key(t)] {
				out.Withdrawn = append(out.Withdrawn, t)
			}
		}
	}

	// Every app object the store attaches to this tenant, besides the ones
	// named above.
	attached, err := store.Read(ctx, Tuple{User: Tenant(tenant), Relation: "tenant", Object: "app:"})
	if err != nil {
		return out, fmt.Errorf("read the apps of %s: %w", Tenant(tenant), err)
	}
	for _, t := range attached {
		if ofTenant(t.Object, "app:", tenant) {
			objects[t.Object] = true
		}
	}
	// And every position, declared or not: one that is no longer declared
	// loses its holders at the next projection, and until then they are
	// about to go, not held -- so only declared positions are read, above.
	// The declared roles of the apps read: whoever was given one directly.
	for object := range objects {
		if !strings.HasPrefix(object, "app:") {
			continue
		}
		roles, err := store.Read(ctx, Tuple{User: object, Relation: "app", Object: "role:"})
		if err != nil {
			return out, fmt.Errorf("read the roles of %s: %w", object, err)
		}
		for _, t := range roles {
			if ofTenant(t.Object, "role:", tenant) {
				objects[t.Object] = true
			}
		}
	}
	names := make([]string, 0, len(objects))
	for object := range objects {
		names = append(names, object)
	}
	sort.Strings(names)
	for _, object := range names {
		entries, err := store.Read(ctx, Tuple{Object: object})
		if err != nil {
			return out, fmt.Errorf("read the entries on %s: %w", object, err)
		}
		for _, t := range entries {
			p := plain(t)
			// What the operator derives follows from the rest and is
			// derived again after a restore; it is nobody's grant.
			if structural(p) || heldByPosition(p, tenant) || IsDerived(p.Relation) {
				continue
			}
			out.Granted = append(out.Granted, p)
		}
	}
	sort.Slice(out.Granted, func(i, j int) bool { return key(out.Granted[i]) < key(out.Granted[j]) })
	return out, nil
}

func plain(t Tuple) Tuple { return Tuple{User: t.User, Relation: t.Relation, Object: t.Object} }

// structural reports whether an entry says what an app, a declared role or a
// position is, as opposed to who holds something on it.
func structural(t Tuple) bool {
	switch {
	case strings.HasPrefix(t.Object, "app:"):
		return appRelations[t.Relation]
	case strings.HasPrefix(t.Object, "role:"):
		return roleRelations[t.Relation]
	case strings.HasPrefix(t.Object, "position:"):
		return t.Relation != "holder"
	}
	return false
}

// heldByPosition reports whether an entry is a role one of the tenant's
// positions holds: declared, and so the projection's (ReconcilePositions).
func heldByPosition(t Tuple, tenant string) bool {
	object, relation, ok := strings.Cut(t.User, "#")
	return ok && relation == "holder" && ofTenant(object, "position:", tenant)
}

// Rebase is an entry recorded on one cluster under the names of another:
// what an entry of a bundle is on the cluster the bundle is restored on. A
// bundle is restored only into the tenant of its own name, so the tenant,
// its apps and its groups keep theirs; the cluster, cluster:<c>, is
// rewritten wherever it stands, as who holds the relation and as what it is
// held on. Everything else is left as it is.
func Rebase(t Tuple, fromCluster, toCluster string) Tuple {
	rename := func(id string) string {
		id, relation, _ := strings.Cut(id, "#")
		if fromCluster != "" && id == Cluster(fromCluster) {
			id = Cluster(toCluster)
		}
		if relation != "" {
			id += "#" + relation
		}
		return id
	}
	return Tuple{User: rename(t.User), Relation: t.Relation, Object: rename(t.Object)}
}

// OnTenant reports whether an entry is held on the tenant itself or on one
// of its apps, their declared roles or its positions: the only objects a
// tenant's backup may write to.
func OnTenant(t Tuple, tenant string) bool {
	for _, kind := range []string{"tenant:", "app:", "role:", "position:"} {
		if ofTenant(t.Object, kind, tenant) {
			return true
		}
	}
	return false
}

// WellFormed reports why an entry cannot be one of the store's, or nil: an
// entry read from a bundle is written with the store's key, and a bundle
// can come from anywhere.
func WellFormed(t Tuple) error {
	part := func(what, id string, userset bool) error {
		if id == "" || strings.ContainsAny(id, " \t\r\n*") {
			return fmt.Errorf("%s %q is not an id of the rights store", what, id)
		}
		object, relation, has := strings.Cut(id, "#")
		if has && (!userset || relation == "" || strings.Contains(relation, "#")) {
			return fmt.Errorf("%s %q is not an id of the rights store", what, id)
		}
		kind, name, ok := strings.Cut(object, ":")
		if !ok || kind == "" || name == "" || strings.Contains(name, ":") {
			return fmt.Errorf("%s %q is not an id of the rights store", what, id)
		}
		return nil
	}
	if err := part("the holder", t.User, true); err != nil {
		return err
	}
	if err := part("the object", t.Object, false); err != nil {
		return err
	}
	if t.Relation == "" || strings.ContainsAny(t.Relation, " \t\r\n#:*") {
		return fmt.Errorf("the relation %q is not one of the rights store's", t.Relation)
	}
	return nil
}
