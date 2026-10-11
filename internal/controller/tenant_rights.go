/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package controller

import (
	"context"
	"fmt"
	"strings"

	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/gentian-org/gentian-os/api/bundle"
	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/backup"
	"github.com/gentian-org/gentian-os/internal/catalogue"
	"github.com/gentian-org/gentian-os/internal/director/authz"
)

// A tenant's rights in a backup.
//
// Which entries of the rights store a bundle carries is the authz package's
// to say (authz.StoreOnlyOf): the ones that follow from nothing else. They
// are read here by the operator, which holds the store's one writing
// client, and travel in the manifest; no Job reads or writes the store, and
// its key is staged nowhere.
//
// A restore puts them back last, after the realm, and only once the
// operator's projection has attached the tenant to its cluster: the
// projection writes the defaults, and what a bundle records as withdrawn is
// a default to take away again.

// RightsStore is the rights store as a backup and a restore use it.
type RightsStore interface {
	Read(ctx context.Context, filter authz.Tuple) ([]authz.Tuple, error)
	Write(ctx context.Context, writes, deletes []authz.Tuple) error
}

// storeOnlyRights reads the tenant's entries that follow from nothing else.
// Nil on a cluster that runs no rights store: there is nothing to carry.
func (r *TenantExportReconciler) storeOnlyRights(ctx context.Context, tenant *gentianov1alpha1.Tenant) (*bundle.ManifestRights, error) {
	tr := r.Reconciler
	if tr == nil || tr.Rights == nil || tr.ClusterID == "" {
		return nil, nil
	}
	var installed []authz.InstalledApp
	for _, app := range tenant.Spec.Apps {
		profile, err := catalogue.ResolveTenantComponentProfile(ctx, r.Client, app)
		if err != nil {
			// As the projection does: an install whose profile cannot be
			// named has no object in the store.
			log.FromContext(ctx).Info("installed app not read from the rights store: its profile cannot be named",
				"tenant", tenant.Name, "error", err.Error())
			continue
		}
		installed = append(installed, authz.InstalledApp{Profile: profile, Addons: app.Addons})
	}
	retained, err := r.retainedApps(ctx, tenant)
	if err != nil {
		return nil, err
	}
	var held []string
	for _, app := range retained {
		held = append(held, app.name)
	}
	only, err := authz.StoreOnlyOf(ctx, tr.Rights, tr.ClusterID, tenant.Name, installed, held)
	if err != nil {
		return nil, fmt.Errorf("read the rights store for tenant %s: %w", tenant.Name, err)
	}
	out := &bundle.ManifestRights{Cluster: tr.ClusterID}
	for _, t := range only.Granted {
		out.Granted = append(out.Granted, bundle.RightsTuple{User: t.User, Relation: t.Relation, Object: t.Object})
	}
	for _, t := range only.Withdrawn {
		out.Withdrawn = append(out.Withdrawn, bundle.RightsTuple{User: t.User, Relation: t.Relation, Object: t.Object})
	}
	return out, nil
}

// rightsPlan is what a restore does with the entries a bundle holds.
//
// Every entry is first put under the names of the tenant restored into
// (authz.Rebase): the tenant, its apps, its groups and the cluster.
//
// What the bundle records as withdrawn is withdrawn: taking a right away
// widens nothing.
//
// What it records as granted is granted only where the bundle is restored
// into the tenant it was taken of. Into a tenant made new -- an import,
// under any name and on any cluster -- none of it is: a right somebody
// granted in one tenant is not carried into another by a file, and whoever
// may grant it there grants it again. Each such entry is named.
//
// An entry that is not held on the tenant or one of its apps, that is not
// an id of the store, or that names a person by an identifier of the realm
// the bundle was taken of -- which a restored person does not keep -- is
// never written, wherever the bundle is restored.
func rightsPlan(m *backup.Manifest, tenant *gentianov1alpha1.Tenant, cluster string, intoNewTenant, haveStore bool) *gentianov1alpha1.RestoreRights {
	if m.Rights == nil || (len(m.Rights.Granted) == 0 && len(m.Rights.Withdrawn) == 0) {
		return nil
	}
	fresh := intoNewTenant || m.Tenant != tenant.Name || (m.Rights.Cluster != "" && m.Rights.Cluster != cluster)
	out := &gentianov1alpha1.RestoreRights{}
	usable := func(raw bundle.RightsTuple) (authz.Tuple, string) {
		t := authz.Rebase(authz.Tuple{User: raw.User, Relation: raw.Relation, Object: raw.Object}, m.Tenant, tenant.Name, m.Rights.Cluster, cluster)
		switch {
		case !haveStore:
			return t, "this cluster's operator has no rights store to write it to"
		case authz.WellFormed(t) != nil:
			return t, authz.WellFormed(t).Error()
		case !authz.OnTenant(t, tenant.Name):
			return t, "it is held on something that is neither this tenant nor one of its apps"
		case strings.HasPrefix(t.User, "user:"):
			return t, "it names a person by the identifier they had in the realm the bundle was taken of, which a restore does not bring back"
		}
		return t, ""
	}
	for _, raw := range m.Rights.Withdrawn {
		t, why := usable(raw)
		if why != "" {
			out.NotBrought = append(out.NotBrought, rightsText(t)+" (withdrawn in the bundle): "+why)
			continue
		}
		out.Withdraw = append(out.Withdraw, rightsText(t))
	}
	for _, raw := range m.Rights.Granted {
		t, why := usable(raw)
		if why == "" && fresh {
			why = "it was granted in the tenant the bundle was taken of, and a grant is not carried into a tenant made new; whoever may grant it here grants it again"
		}
		if why != "" {
			out.NotBrought = append(out.NotBrought, rightsText(t)+": "+why)
			continue
		}
		out.Grant = append(out.Grant, rightsText(t))
	}
	return out
}

func rightsText(t authz.Tuple) string { return t.User + " " + t.Relation + " " + t.Object }

func rightsTuple(text string) (authz.Tuple, error) {
	parts := strings.Split(text, " ")
	if len(parts) != 3 {
		return authz.Tuple{}, fmt.Errorf("%q is not an entry of the rights store", text)
	}
	t := authz.Tuple{User: parts[0], Relation: parts[1], Object: parts[2]}
	return t, authz.WellFormed(t)
}

// applyRights writes what the plan decided. done is false while the
// operator's projection has not attached the tenant yet.
//
// Each entry is read before it is written: the store refuses to write what
// it holds and to remove what it does not, and a restore that is run again
// finds its own work.
func applyRights(ctx context.Context, store RightsStore, cluster, tenant string, plan *gentianov1alpha1.RestoreRights) (done bool, err error) {
	if plan == nil || plan.Applied || (len(plan.Grant) == 0 && len(plan.Withdraw) == 0) {
		return true, nil
	}
	if store == nil {
		return false, fmt.Errorf("the restore was planned with a rights store and this operator has none")
	}
	attached, err := store.Read(ctx, authz.Tuple{User: authz.Cluster(cluster), Relation: "cluster", Object: authz.Tenant(tenant)})
	if err != nil {
		return false, fmt.Errorf("read the rights store: %w", err)
	}
	if len(attached) == 0 {
		return false, nil
	}
	var writes, deletes []authz.Tuple
	for _, text := range plan.Grant {
		t, err := rightsTuple(text)
		if err != nil {
			return false, err
		}
		if !authz.OnTenant(t, tenant) {
			return false, fmt.Errorf("refused: %s is not held on tenant %s or one of its apps", text, tenant)
		}
		have, err := store.Read(ctx, t)
		if err != nil {
			return false, fmt.Errorf("read %s: %w", text, err)
		}
		if len(have) == 0 {
			writes = append(writes, t)
		}
	}
	for _, text := range plan.Withdraw {
		t, err := rightsTuple(text)
		if err != nil {
			return false, err
		}
		if !authz.OnTenant(t, tenant) {
			return false, fmt.Errorf("refused: %s is not held on tenant %s or one of its apps", text, tenant)
		}
		have, err := store.Read(ctx, t)
		if err != nil {
			return false, fmt.Errorf("read %s: %w", text, err)
		}
		if len(have) > 0 {
			deletes = append(deletes, t)
		}
	}
	if len(writes) > 0 || len(deletes) > 0 {
		if err := store.Write(ctx, writes, deletes); err != nil {
			return false, fmt.Errorf("write the rights store: %w", err)
		}
	}
	plan.Applied = true
	return true, nil
}
