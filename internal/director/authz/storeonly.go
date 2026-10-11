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
// The store is a projection, mostly (AD-12). Who is whose member follows
// from the identity provider; which cluster a tenant is attached to, which
// groups hold its three roles, which apps it has and which groups are
// entitled to them follow from the tenant's entry in git, and the operator
// writes all of that again wherever the tenant is (ReconcileTenants,
// ReconcileApps). A backup that carried those would carry a second copy of
// git, and a restore that wrote them back would overrule it.
//
// What a backup has to carry is the rest, and it is of two kinds:
//
//   - an entry on the tenant or on one of its apps that no projection would
//     write: a right somebody granted there beyond the defaults;
//   - a default the projection writes once and then leaves alone, which the
//     store no longer holds: `operated_by`, the tenant's consent to being
//     administered by its cluster. The missing entry is the tenant's answer,
//     and a set of entries cannot say that one is missing -- so it is
//     recorded as withdrawn.
//
// Nothing here knows how either came about. The operator has no act that
// writes them today; a store that holds one was written by a person with
// the store's key, and it is that person's act a backup must not lose.

// Reader reads entries of the store.
type Reader interface {
	Read(ctx context.Context, filter Tuple) ([]Tuple, error)
}

// StoreOnly is what a tenant has in the store that follows from nothing
// else.
type StoreOnly struct {
	// Granted are entries on the tenant and its apps that no projection
	// writes.
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
func tenantDefaults(cluster, tenant string) (standing, once []Tuple, err error) {
	clusterObj, obj := Cluster(cluster), Tenant(tenant)
	standing = append(standing, Tuple{User: clusterObj, Relation: "cluster", Object: obj})
	for _, role := range tenantRoleGroups {
		group, err := Group("gentian:tenant:" + tenant + ":" + role.suffix)
		if err != nil {
			return nil, nil, fmt.Errorf("tenant %s: %w", tenant, err)
		}
		standing = append(standing, Tuple{User: group + "#member", Relation: role.relation, Object: obj})
	}
	once = []Tuple{{User: clusterObj, Relation: "operated_by", Object: obj}}
	return standing, once, nil
}

// StoreOnlyOf reads what a tenant has in the store that follows from
// nothing else.
//
// apps are the tenant's installed apps, as the projection is given them.
// held are further app names the tenant holds something of -- apps that
// were uninstalled with their data kept -- whose objects are read too: a
// right granted on such an app is still in the store.
//
// On an app's object the relations `tenant` and `entitled` are the
// projection's alone: it removes every such entry it would not write
// (ReconcileApps), so one it would not write is not a right somebody holds
// but one that is about to go, and is not carried.
func StoreOnlyOf(ctx context.Context, store Reader, cluster, tenant string, apps []InstalledApp, held []string) (StoreOnly, error) {
	var out StoreOnly
	standing, once, err := tenantDefaults(cluster, tenant)
	if err != nil {
		return out, err
	}
	derived := map[string]bool{}
	for _, t := range standing {
		derived[key(t)] = true
	}
	for _, t := range once {
		derived[key(t)] = true
	}
	// Whether the tenant's administrators approve its public addresses is
	// the manifest's to say, and the projection removes the entry where the
	// manifest does not. Present or not, it is not a right only the store
	// knows, and it does not travel in a bundle: on the cluster a tenant is
	// imported to, that is its administrator's decision.
	approve, err := adminsApprove(tenant)
	if err != nil {
		return out, err
	}
	derived[key(approve)] = true
	want, err := appTuples(tenant, apps)
	if err != nil {
		return out, err
	}
	objects := map[string]bool{}
	for k, t := range want {
		derived[k] = true
		objects[t.Object] = true
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
		if p := plain(t); !derived[key(p)] {
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
		if strings.HasPrefix(t.Object, "app:"+tenant+"/") {
			objects[t.Object] = true
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
			if t.Relation == "tenant" || t.Relation == "entitled" {
				continue
			}
			out.Granted = append(out.Granted, plain(t))
		}
	}
	sort.Slice(out.Granted, func(i, j int) bool { return key(out.Granted[i]) < key(out.Granted[j]) })
	return out, nil
}

func plain(t Tuple) Tuple { return Tuple{User: t.User, Relation: t.Relation, Object: t.Object} }

// Rebase is an entry recorded for one tenant on one cluster, under the
// names of another: what an entry of a bundle is in the tenant the bundle
// is restored into.
//
// The tenant, its apps and its groups are named for the tenant, and the
// cluster for the cluster: tenant:<t>, app:<t>/<profile>,
// group:gentian/tenant/<t>/..., cluster:<c>. Each is rewritten wherever it
// stands, as who holds the relation and as what it is held on. Everything
// else is left as it is.
func Rebase(t Tuple, fromTenant, toTenant, fromCluster, toCluster string) Tuple {
	rename := func(id string) string {
		id, relation, _ := strings.Cut(id, "#")
		switch {
		case id == Tenant(fromTenant):
			id = Tenant(toTenant)
		case strings.HasPrefix(id, "app:"+fromTenant+"/"):
			id = "app:" + toTenant + "/" + strings.TrimPrefix(id, "app:"+fromTenant+"/")
		case strings.HasPrefix(id, "group:gentian/tenant/"+fromTenant+"/"):
			id = "group:gentian/tenant/" + toTenant + "/" + strings.TrimPrefix(id, "group:gentian/tenant/"+fromTenant+"/")
		case fromCluster != "" && id == Cluster(fromCluster):
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
// of its apps: the only objects a tenant's backup may write to.
func OnTenant(t Tuple, tenant string) bool {
	return t.Object == Tenant(tenant) || (strings.HasPrefix(t.Object, "app:"+tenant+"/") && len(t.Object) > len("app:"+tenant+"/"))
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
