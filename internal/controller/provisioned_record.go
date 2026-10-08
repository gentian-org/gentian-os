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

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/backup"
)

// The tenant's record of the stores provisioning made (backup.Provisioned):
// written here, before the Jobs that make the stores are handed over, and
// read here when the tenant is deleted.

// recordProvisionedStores writes down, for every app of the tenant, the
// stores its profile declares and the names they are made under. It only
// adds: an app that has left spec.apps keeps its entry, which is the point --
// its stores are still there, and the entry is how they are found.
//
// It runs before the provisioning Jobs are written for Crossplane to apply,
// and a failure here fails the pass, so no store is made that is not on
// record first.
func (r *TenantReconciler) recordProvisionedStores(ctx context.Context, tenant *gentianov1alpha1.Tenant) error {
	profiles, err := loadAppProfileIndex(ctx, r.Client)
	if err != nil {
		return err
	}
	// A key at the model gateway is made for an app that declares it
	// (requires.services.llm), and only on a cluster that serves models:
	// elsewhere the requirement waits and nothing is made to record.
	models := clusterLLMEnabled(ctx, r.Client)
	made := map[string]backup.Provisioned{}
	for _, app := range tenant.Spec.Apps {
		profile, ok := appProfileFromIndex(profiles, app.Profile)
		if !ok {
			continue
		}
		p := backup.ProvisionedOf(backup.InventoryOf(tenant, app.Profile, profile))
		if !models {
			p.ModelKey = ""
		}
		if !p.Empty() {
			made[app.Profile] = p
		}
	}
	if len(made) == 0 {
		return nil
	}

	key := backup.ProvisionedRecordKey(tenant.Name)
	record := &corev1.ConfigMap{}
	err = r.Get(ctx, key, record)
	create := errors.IsNotFound(err)
	if create {
		record = backup.NewProvisionedRecord(tenant.Name)
	} else if err != nil {
		return fmt.Errorf("read the record of what was provisioned for %s: %w", tenant.Name, err)
	}
	original := record.DeepCopy()
	changed := false
	for app, p := range made {
		c, err := backup.RecordProvisioned(record, app, p)
		if err != nil {
			return err
		}
		changed = changed || c
	}
	switch {
	case create:
		if err := r.Create(ctx, record); err != nil {
			return fmt.Errorf("write the record of what was provisioned for %s: %w", tenant.Name, err)
		}
	case changed:
		// An optimistic patch: a purge takes entries out of the same object,
		// and overwriting what it wrote would put a destroyed store back on
		// record.
		if err := r.Patch(ctx, record, client.MergeFromWithOptions(original, client.MergeFromWithOptimisticLock{})); err != nil {
			return fmt.Errorf("update the record of what was provisioned for %s: %w", tenant.Name, err)
		}
	}
	return nil
}

// provisionedStores reads the tenant's record. A tenant without one has had
// nothing recorded. It reads the API server when it can: this is what a
// deletion decides what to destroy by, and a cache that has not seen the
// record yet would read as "nothing was provisioned".
func (r *TenantReconciler) provisionedStores(ctx context.Context, tenantName string) (map[string]backup.Provisioned, error) {
	reader := client.Reader(r.Client)
	if r.APIReader != nil {
		reader = r.APIReader
	}
	record := &corev1.ConfigMap{}
	if err := reader.Get(ctx, backup.ProvisionedRecordKey(tenantName), record); err != nil {
		if errors.IsNotFound(err) {
			return map[string]backup.Provisioned{}, nil
		}
		return nil, fmt.Errorf("read the record of what was provisioned for %s: %w", tenantName, err)
	}
	return backup.ReadProvisioned(record)
}

// recordedApps are the apps the record names a store for, as has picks them.
func (r *TenantReconciler) recordedApps(ctx context.Context, tenantName string, has func(backup.Provisioned) bool) ([]string, error) {
	recorded, err := r.provisionedStores(ctx, tenantName)
	if err != nil {
		return nil, err
	}
	return backup.ProvisionedApps(recorded, has), nil
}

// deleteProvisionedRecord removes the record once the tenant's stores are
// destroyed. With deletionPolicy Retain the stores stay, and so does the
// record of them: a tenant made again under the name finds what it left.
func (r *TenantReconciler) deleteProvisionedRecord(ctx context.Context, tenant *gentianov1alpha1.Tenant) error {
	if tenant.Spec.DeletionPolicy != gentianov1alpha1.DeletionPolicyDelete {
		return nil
	}
	record := backup.NewProvisionedRecord(tenant.Name)
	if err := r.Delete(ctx, record); client.IgnoreNotFound(err) != nil {
		return fmt.Errorf("delete the record of what was provisioned for %s: %w", tenant.Name, err)
	}
	return nil
}
