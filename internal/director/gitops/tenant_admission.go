/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package gitops

import (
	"context"
	"fmt"
	"path"
	"strings"
)

// TenantAdmitter asks the cluster whether it would take a Tenant's manifest:
// refusal is the cluster's reason where it would not, and err is set where
// the cluster could not be asked.
type TenantAdmitter func(ctx context.Context, tenant string, manifest []byte) (refusal string, err error)

// TenantRefused is a commit that was not made because the cluster would
// refuse a tenant's manifest in it. Committed, Argo CD could apply nothing
// of that tenant any more, neither this change nor any after it.
type TenantRefused struct {
	Tenant string
	// Reason is the cluster's own.
	Reason string
}

func (e *TenantRefused) Error() string {
	return fmt.Sprintf("the cluster would refuse tenant %s as this change leaves it, so nothing was written: %s", e.Tenant, e.Reason)
}

// GuardTenants makes every commit that adds or changes a tenant's manifest
// conditional on the cluster taking that manifest. A cluster that cannot be
// asked holds nothing back: the Tenant webhook still decides what is applied.
func (g *GitOps) GuardTenants(admit TenantAdmitter, unasked func(tenant string, err error)) {
	g.admitTenant, g.tenantUnasked = admit, unasked
}

// checkTenants refuses the staged commit when the cluster would refuse a
// tenant's manifest in it.
func (g *GitOps) checkTenants(ctx context.Context) error {
	if g.admitTenant == nil {
		return nil
	}
	names, cancel := g.gitCmd(ctx, "-C", g.path, "diff", "--cached", "--name-only", "-z", "--no-renames", "--diff-filter=AM")
	defer cancel()
	out, err := names.Output()
	if err != nil {
		return fmt.Errorf("git diff --cached: %w", err)
	}
	prefix := path.Join("clusters", g.cluster, "tenants") + "/"
	for _, rel := range strings.Split(string(out), "\x00") {
		tenant, file, _ := strings.Cut(strings.TrimPrefix(rel, prefix), "/")
		if !strings.HasPrefix(rel, prefix) || file != "tenant.yaml" {
			continue
		}
		manifest, err := g.blob(ctx, ":"+rel)
		if err != nil {
			return err
		}
		refusal, err := g.admitTenant(ctx, tenant, manifest)
		if err != nil {
			if g.tenantUnasked != nil {
				g.tenantUnasked(tenant, err)
			}
			continue
		}
		if refusal != "" {
			return &TenantRefused{Tenant: tenant, Reason: refusal}
		}
	}
	return nil
}
