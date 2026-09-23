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
	{"perimeter_approver", "perimeter"},
}

// ReconcileTenants makes each named tenant known to the store: attached to
// its cluster, and its three role relations held by the tenant's groups.
//
// Written at start from what git lists under clusters/<c>/tenants/, so a
// cluster rebuilt from git arrives at the same place. `cluster` is what audit
// and approval derive through and is written whenever it is missing. The
// role tuples are the same. `operated_by` is consent (model v1): written when
// a tenant is first attached, so a fresh deploy is operated by its cluster,
// and always for the platform tenant; a tenant that later withdraws it is not
// second-guessed here, because the missing tuple is then the tenant's answer.
//
// Membership is not touched: it is a projection of Keycloak.
func (c *OpenFGA) ReconcileTenants(ctx context.Context, cluster string, tenants []string) error {
	clusterObj := Cluster(cluster)
	var writes []Tuple
	for _, tenant := range tenants {
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
		for _, role := range tenantRoleGroups {
			group, err := Group("gentian:tenant:" + tenant + ":" + role.suffix)
			if err != nil {
				return fmt.Errorf("tenant %s: %w", tenant, err)
			}
			t := Tuple{User: group + "#member", Relation: role.relation, Object: obj}
			have, err := c.Read(ctx, t)
			if err != nil {
				return fmt.Errorf("read %s on %s: %w", role.relation, obj, err)
			}
			if len(have) == 0 {
				writes = append(writes, t)
			}
		}
	}
	if len(writes) == 0 {
		return nil
	}
	sort.Slice(writes, func(i, j int) bool { return key(writes[i]) < key(writes[j]) })
	if err := c.Write(ctx, writes, nil); err != nil {
		return fmt.Errorf("reconcile tenants: %w", err)
	}
	c.log.InfoContext(ctx, "tenants reconciled", "cluster", cluster, "tenants", len(tenants), "written", len(writes))
	return nil
}
