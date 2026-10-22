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
)

// PlatformTenant is the tenant whose realm is the kernel realm (AD-10). Its
// members are the platform's administrators, so it is always operated by the
// cluster: the tuple that says so is written on every start and never
// removed, where every other tenant's is written once at deploy and is the
// tenant's to withdraw.
const PlatformTenant = "platform"

// tenantRoleGroups are the relations on type tenant that a Keycloak group
// holds, and the group's suffix under gentian:tenant:<t>:.
var tenantRoleGroups = []struct{ relation, suffix string }{
	{"admin", "admins"},
	{"member", "members"},
}

// TenantRights is one tenant as the projection is given it: its name, and
// who the Cluster claim names its perimeter approvers (role holders of scope
// tenant:<t>), each as HolderUser.
type TenantRights struct {
	Name               string
	PerimeterApprovers []string
}

// TenantsNamed is tenants nobody is named perimeter approver of.
func TenantsNamed(names ...string) []TenantRights {
	out := make([]TenantRights, 0, len(names))
	for _, name := range names {
		out = append(out, TenantRights{Name: name})
	}
	return out
}

// ReconcileTenants makes each named tenant known to the store: attached to
// its cluster, and its two role relations held by the tenant's groups.
//
// Written by the operator from the Tenant objects of the cluster
// (AuthzProjectionReconciler), which are what git lists under
// clusters/<c>/tenants/, so a cluster rebuilt from git arrives at the same
// place. `cluster` is what audit
// and approval derive through and is written whenever it is missing. The
// role tuples are the same. `operated_by` is consent (model v1): written when
// a tenant is first attached, so a fresh deploy is operated by its cluster,
// and always for the platform tenant; a tenant that later withdraws it is not
// second-guessed here, because the missing tuple is then the tenant's answer.
//
// perimeter_approver on a tenant is the claim's alone: the role holders of
// scope tenant:<t> hold it, and every other entry under it is removed, so an
// entry somebody wrote into the store by hand does not outlive the next
// projection, and a backup does not carry it (StoreOnlyOf). The cluster's
// perimeter approver holds it through the cluster and is not written here.
//
// Membership is not touched: it is a projection of Keycloak.
func (c *OpenFGA) ReconcileTenants(ctx context.Context, cluster string, tenants []TenantRights) error {
	clusterObj := Cluster(cluster)
	var writes, deletes []Tuple
	for _, rights := range tenants {
		tenant := rights.Name
		obj := Tenant(tenant)
		attached, err := c.Read(ctx, Tuple{User: clusterObj, Relation: "cluster", Object: obj})
		if err != nil {
			return fmt.Errorf("read cluster of %s: %w", obj, err)
		}
		if len(attached) == 0 {
			writes = append(writes, Tuple{User: clusterObj, Relation: "cluster", Object: obj})
		}
		if len(attached) == 0 || tenant == PlatformTenant {
			operated, err := c.Read(ctx, Tuple{User: clusterObj, Relation: "operated_by", Object: obj})
			if err != nil {
				return fmt.Errorf("read operated_by of %s: %w", obj, err)
			}
			if len(operated) == 0 {
				writes = append(writes, Tuple{User: clusterObj, Relation: "operated_by", Object: obj})
			}
		}
		// The role tuples, by the list a backup tells the store's own
		// entries from the derived ones by (tenantDefaults).
		standing, _, err := tenantDefaults(cluster, tenant)
		if err != nil {
			return err
		}
		for _, t := range standing[1:] {
			have, err := c.Read(ctx, t)
			if err != nil {
				return fmt.Errorf("read %s on %s: %w", t.Relation, obj, err)
			}
			if len(have) == 0 {
				writes = append(writes, t)
			}
		}
		approvers := map[string]bool{}
		for _, user := range rights.PerimeterApprovers {
			approvers[user] = true
		}
		have, err := c.Read(ctx, Tuple{Relation: "perimeter_approver", Object: obj})
		if err != nil {
			return fmt.Errorf("read perimeter_approver on %s: %w", obj, err)
		}
		for _, t := range have {
			if approvers[t.User] {
				delete(approvers, t.User)
				continue
			}
			deletes = append(deletes, plain(t))
		}
		for user := range approvers {
			writes = append(writes, Tuple{User: user, Relation: "perimeter_approver", Object: obj})
		}
	}
	if len(writes) == 0 && len(deletes) == 0 {
		return nil
	}
	sort.Slice(writes, func(i, j int) bool { return key(writes[i]) < key(writes[j]) })
	sort.Slice(deletes, func(i, j int) bool { return key(deletes[i]) < key(deletes[j]) })
	if err := c.Write(ctx, writes, deletes); err != nil {
		return fmt.Errorf("reconcile tenants: %w", err)
	}
	c.log.InfoContext(ctx, "tenants reconciled", "cluster", cluster, "tenants", len(tenants),
		"written", len(writes), "removed", len(deletes))
	return nil
}
