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
	"errors"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/gentian-org/gentian-os/api/bundle"
	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/arbiter"
	"github.com/gentian-org/gentian-os/internal/backup"
	"github.com/gentian-org/gentian-os/internal/layout"
)

// An app's authorization store, in a bundle and out of it (§2.4 of the
// authorization design).
//
// The store is tenant data: what the app wrote, its starting tuples and the
// realm's memberships it mirrors. An export carries every tuple and the model
// they were written against, read through the arbiter's platform path and
// encrypted by the operator itself, as an app's own secrets are: no Job
// reaches the arbiter. A restore puts them back into a fresh store of the
// app in the tenant restored into -- named by that tenant and that app, never
// by the bundle -- which then holds what the bundle holds and nothing else.

// AuthorizationStores is the arbiter's platform path, as an export, a
// restore and a purge use it. *arbiter.PlatformClient is one.
type AuthorizationStores interface {
	Export(ctx context.Context, tenant, component string) (*arbiter.StoreExport, error)
	Restore(ctx context.Context, tenant, component string, held *arbiter.StoreExport) error
	Purge(ctx context.Context, tenant, component string) error
	PurgeTenant(ctx context.Context, tenant string) error
}

// authorizationStoreRecorded reports whether the arbiter recorded a store for
// the tenant's app: the record it keeps of the stores it made, which is what
// finds the store of an app that is no longer installed.
func authorizationStoreRecorded(ctx context.Context, c client.Reader, tenant, app string) (bool, error) {
	cm := &corev1.ConfigMap{}
	err := c.Get(ctx, types.NamespacedName{Name: arbiter.StoresConfigMap, Namespace: arbiter.Namespace}, cm)
	if err != nil {
		return false, client.IgnoreNotFound(err)
	}
	_, ok := cm.Data[arbiter.StoreRecordKey(tenant, app)]
	return ok, nil
}

// withAppAuthorization adds an app's authorization store to what was
// captured for it, when the arbiter made one.
func (r *TenantExportReconciler) withAppAuthorization(
	ctx context.Context,
	export *gentianov1alpha1.TenantExport,
	tenant *gentianov1alpha1.Tenant,
	appName string,
	encryption backup.Encryption,
	entry *gentianov1alpha1.AppExportStatus,
) error {
	if r.Authorization == nil {
		return nil
	}
	recorded, err := authorizationStoreRecorded(ctx, r.Client, tenant.Name, appName)
	if err != nil || !recorded {
		return err
	}
	held, err := r.Authorization.Export(ctx, tenant.Name, appName)
	if errors.Is(err, arbiter.ErrNoStore) {
		return nil
	}
	if err != nil {
		// A bundle that left the store out because the arbiter was away
		// would bring the app back without who may do what in it.
		return fmt.Errorf("%s: read its authorization store: %w", appName, err)
	}
	if r.Bundles == nil || export.Status.Bundle == nil {
		return fmt.Errorf("%s has an authorization store and the operator has no way to write it into the bundle", appName)
	}
	doc := &bundle.AppAuthorization{App: appName, Model: held.Model, Tuples: make([]bundle.RightsTuple, 0, len(held.Tuples))}
	for _, t := range held.Tuples {
		doc.Tuples = append(doc.Tuples, bundle.RightsTuple{User: t.User, Relation: t.Relation, Object: t.Object})
	}
	passphrase := ""
	if encryption.Mode == gentianov1alpha1.ExportEncryptionPassphrase {
		if passphrase, err = r.exportPassphrase(ctx, export); err != nil {
			return err
		}
	}
	cipher, err := backup.SealAppAuthorization(encryption, passphrase, doc)
	if err != nil {
		return fmt.Errorf("%s: %w", appName, err)
	}
	path := backup.AuthorizationArtefact(appName)
	if err := r.Bundles.PutArtefact(ctx, *export.Status.Bundle, path, cipher); err != nil {
		return fmt.Errorf("%s: write its authorization store into the bundle: %w", appName, err)
	}
	entry.Artefacts = append(entry.Artefacts, gentianov1alpha1.BundleArtefact{Kind: bundle.ArtefactAuthorization, Name: appName, Path: path})
	entry.Stores = append(entry.Stores, bundle.ArtefactAuthorization)
	entry.Authorization = &gentianov1alpha1.AppAuthorizationStatus{Tuples: int32(len(doc.Tuples))}
	return nil
}

// restoreAppAuthorization puts an app's authorization store back from the
// bundle, once. ready says the restore of the app may go on; failure, when
// not empty, is why it cannot.
func (r *TenantRestoreReconciler) restoreAppAuthorization(
	ctx context.Context,
	restore *gentianov1alpha1.TenantRestore,
	tenant *gentianov1alpha1.Tenant,
	appName string,
) (ready bool, failure string, err error) {
	entry := appStatus(&restore.Status.Apps, appName)
	artefact := artefactOf(entry.Artefacts, bundle.ArtefactAuthorization)
	if artefact == nil || (entry.Authorization != nil && entry.Authorization.RestoredAt != nil) {
		return true, "", nil
	}
	if r.Authorization == nil {
		return false, "the bundle holds the app's authorization store and this operator has no arbiter to put it back with", nil
	}
	// The store goes back into this cluster's app instance, which runs
	// while a component declares a store. A bundle of an app that is not
	// installed here is put back once one is, or the engine is always on.
	set := &corev1.ConfigMap{}
	if err := r.Get(ctx, types.NamespacedName{Name: systemEnginesConfigMap, Namespace: r.engineNamespace()}, set); client.IgnoreNotFound(err) != nil {
		return false, "", err
	}
	if set.Data[EngineAuthorization] != engineWanted {
		return false, "the bundle holds the app's authorization store and this cluster runs no app authorization: " +
			"install a component that declares one, or name authorization under the claim's systemServices.always", nil
	}
	if restore.Status.Bundle == nil {
		return false, "the restore records no bundle to read the app's authorization store from", nil
	}
	key, err := r.bundleKey(ctx, restore)
	if err != nil {
		return false, fmt.Sprintf("the key to open the app's authorization store with: %v", err), nil
	}
	held, err := r.Bundles.AppAuthorization(ctx, *restore.Status.Bundle, artefact.Path, key)
	if err != nil {
		return false, fmt.Sprintf("its authorization store: %v", err), nil
	}
	put := &arbiter.StoreExport{Model: held.Model, Tuples: make([]arbiter.Tuple, 0, len(held.Tuples))}
	for _, t := range held.Tuples {
		put.Tuples = append(put.Tuples, arbiter.Tuple{User: t.User, Relation: t.Relation, Object: t.Object})
	}
	// The arbiter may not answer yet: the engine was composed for this
	// restore a moment ago. Asked again on the next pass.
	if err := r.Authorization.Restore(ctx, tenant.Name, appName, put); err != nil {
		return false, "", fmt.Errorf("%s: put its authorization store back: %w", appName, err)
	}
	now := metav1.Now()
	entry.Authorization = &gentianov1alpha1.AppAuthorizationStatus{Tuples: int32(len(put.Tuples)), RestoredAt: &now}
	if err := r.persist(ctx, restore); err != nil {
		return false, "", err
	}
	return true, "", nil
}

// engineNamespace is where the engine set is: the control namespace.
func (r *TenantRestoreReconciler) engineNamespace() string {
	return envOrDefault("POD_NAMESPACE", layout.Namespace(layout.Control))
}

// deleteAuthorizationStores deletes every authorization store of a tenant
// deleted with deletionPolicy: Delete, through the arbiter. A cluster with no
// arbiter recorded none; with one that does not answer, the deletion waits.
func (r *TenantReconciler) deleteAuthorizationStores(ctx context.Context, tenant *gentianov1alpha1.Tenant) error {
	if tenant.Spec.DeletionPolicy != gentianov1alpha1.DeletionPolicyDelete || r.Authorization == nil {
		return nil
	}
	cm := &corev1.ConfigMap{}
	if err := r.Get(ctx, types.NamespacedName{Name: arbiter.StoresConfigMap, Namespace: arbiter.Namespace}, cm); err != nil {
		return client.IgnoreNotFound(err)
	}
	held := false
	for key := range cm.Data {
		if t, _, ok := strings.Cut(key, "."); ok && t == tenant.Name {
			held = true
		}
	}
	if !held {
		return nil
	}
	if err := r.Authorization.PurgeTenant(ctx, tenant.Name); err != nil {
		return fmt.Errorf("delete the authorization stores of tenant %s: %w", tenant.Name, err)
	}
	return nil
}
