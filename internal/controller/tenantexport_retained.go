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
	"slices"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/backup"
)

// The data of apps that were uninstalled.
//
// Uninstalling keeps an app's data, and deleting the tenant destroys it. An
// export used to capture installed apps only, so the one copy of that data
// outside the cluster was a bundle taken before the uninstall. It is
// captured now as an installed app's is: the same stores, found by the rule
// a purge destroys them by, written as the same artefacts, and marked as
// retained in the status and in the manifest. Nothing is paused for it:
// nothing of the app runs.

// platformAppAnnotation marks a profile the platform places itself; what it
// holds is not an uninstalled app's.
const platformAppAnnotation = "gentianos.io/platform-app"

// retainedApp is one uninstalled app that still holds data a bundle
// carries.
type retainedApp struct {
	name string
	// profile is the app's ComponentProfile, nil when the cluster has none.
	profile *gentianov1alpha1.ComponentProfile
	// stores are the stores held for it (backup.HeldStores).
	stores backup.Stores
	// claims are the volume claims that are its (backup.AppVolumes).
	claims []string
}

// heldData reads what the cluster holds for a tenant that carries data: the
// record of what was provisioned, the database records, and the volume
// claims. Each is read or the read fails: an export that could not look
// must not conclude there is nothing.
func (r *TenantExportReconciler) heldData(ctx context.Context, tenant *gentianov1alpha1.Tenant) (backup.Held, error) {
	held := backup.Held{Provisioned: map[string]backup.Provisioned{}, Databases: map[string]bool{}}
	if r.Reconciler != nil {
		recorded, err := r.Reconciler.provisionedStores(ctx, tenant.Name)
		if err != nil {
			return held, err
		}
		held.Provisioned = recorded
	}
	dbs := &unstructured.UnstructuredList{}
	dbs.SetGroupVersionKind(schema.GroupVersionKind{Group: cnpgGroup, Version: cnpgVersion, Kind: cnpgDatabaseKind + "List"})
	err := r.List(ctx, dbs, client.InNamespace(postgresNamespace), tenantKernelLabelSelector(tenant.Name))
	switch {
	case err == nil:
		for i := range dbs.Items {
			if app := dbs.Items[i].GetLabels()[appLabel]; app != "" {
				held.Databases[app] = true
			}
		}
	case apimeta.IsNoMatchError(err) || apierrors.IsNotFound(err):
		// No database operator on this cluster, so no databases of its.
	default:
		return held, fmt.Errorf("list the database records of tenant %s: %w", tenant.Name, err)
	}
	reader := client.Reader(r.Client)
	if r.VolumeReader != nil {
		reader = r.VolumeReader
	}
	pvcs := &corev1.PersistentVolumeClaimList{}
	if err := reader.List(ctx, pvcs, client.InNamespace(backup.TenantNamespace(tenant))); err != nil {
		return held, fmt.Errorf("list claims in %s: %w", backup.TenantNamespace(tenant), err)
	}
	held.Claims = pvcs.Items
	return held, nil
}

// retainedApps are the tenant's uninstalled apps that hold data a bundle
// carries: a database, a bucket, or files. An app that is installed, or
// still being taken down, is none of them.
func (r *TenantExportReconciler) retainedApps(ctx context.Context, tenant *gentianov1alpha1.Tenant) ([]retainedApp, error) {
	held, err := r.heldData(ctx, tenant)
	if err != nil {
		return nil, err
	}
	index, err := loadAppProfileIndex(ctx, r.Client)
	if err != nil {
		return nil, err
	}
	profiles := func(name string) (*gentianov1alpha1.ComponentProfile, error) {
		profile, _ := appProfileFromIndex(index, name)
		return profile, nil
	}
	installed := backup.TenantApps(tenant)
	all, err := r.exportAppSet(ctx, tenant, &gentianov1alpha1.TenantExport{})
	if err != nil {
		return nil, err
	}
	for _, app := range all {
		installed[app] = true
	}
	// What is being taken down still has a Component: its data is not yet
	// what an uninstall left.
	components := &gentianov1alpha1.ComponentList{}
	if err := r.List(ctx, components, client.InNamespace(backup.TenantNamespace(tenant))); err != nil {
		return nil, fmt.Errorf("list components in %s: %w", backup.TenantNamespace(tenant), err)
	}
	for i := range components.Items {
		installed[components.Items[i].Name] = true
		installed[components.Items[i].Spec.ProfileRef.Name] = true
	}

	var out []retainedApp
	for _, name := range backup.DataHolders(tenant, held, profiles) {
		if installed[name] || backup.IsPlatformStore(name) {
			continue
		}
		profile, _ := profiles(name)
		if profile != nil && profile.Annotations[platformAppAnnotation] == "true" {
			continue
		}
		app := retainedApp{name: name, profile: profile, stores: backup.HeldStores(name, held)}
		app.claims, _ = backup.AppVolumes(held.Claims, tenant, name, profile)
		sort.Strings(app.claims)
		if app.stores.Database == "" && !app.stores.S3 && len(app.claims) == 0 {
			continue
		}
		out = append(out, app)
	}
	return out, nil
}

// retainedSet is the retained apps this export captures, by name: every one
// the tenant holds, or with spec.apps the ones named there -- and any the
// export has begun, so that a store destroyed while the export runs fails
// its capture instead of dropping out of the bundle without a word.
func (r *TenantExportReconciler) retainedSet(
	ctx context.Context,
	tenant *gentianov1alpha1.Tenant,
	export *gentianov1alpha1.TenantExport,
) (map[string]retainedApp, []string, error) {
	found, err := r.retainedApps(ctx, tenant)
	if err != nil {
		return nil, nil, err
	}
	set := map[string]retainedApp{}
	for _, app := range found {
		if len(export.Spec.Apps) > 0 && !slices.Contains(export.Spec.Apps, app.name) {
			continue
		}
		set[app.name] = app
	}
	for _, entry := range export.Status.Apps {
		if _, ok := set[entry.Name]; entry.Retained && !ok {
			set[entry.Name] = retainedApp{name: entry.Name}
		}
	}
	names := make([]string, 0, len(set))
	for name := range set {
		names = append(names, name)
	}
	sort.Strings(names)
	return set, names, nil
}

// captureRetained captures one uninstalled app's data. No pause and no
// resume: the app is not running.
func (r *TenantExportReconciler) captureRetained(
	ctx context.Context,
	export *gentianov1alpha1.TenantExport,
	tenant *gentianov1alpha1.Tenant,
	app retainedApp,
	encryption backup.Encryption,
) (ctrl.Result, error) {
	entry := appStatus(&export.Status.Apps, app.name)
	entry.Retained = true
	failed := func(message string) (ctrl.Result, error) {
		entry.Phase = gentianov1alpha1.TenantExportPhaseFailed
		entry.Message = message
		return r.fail(ctx, export, "CaptureFailed", fmt.Sprintf("%s (uninstalled, data retained): %s", app.name, message))
	}
	if app.stores == (backup.Stores{}) && len(app.claims) == 0 {
		return failed("the data this export began to capture is no longer there: it was purged, or the app was installed again, while the export ran")
	}
	units, err := r.unitsFor(ctx, tenant, app.name, app.profile, app.stores, app.claims, true, export, encryption)
	if err != nil {
		return failed(fmt.Sprintf("enumerate what to capture: %v", err))
	}
	if err := r.stageFor(ctx, export, units, encryption); err != nil {
		return failed(err.Error())
	}
	entry.Phase = gentianov1alpha1.TenantExportPhaseRunning
	var pending []string
	for _, unit := range units {
		done, err := r.ensureCaptureJob(ctx, export, unit)
		if err != nil {
			return ctrl.Result{}, err
		}
		if !done {
			pending = append(pending, unit.JobName)
		}
	}
	if len(pending) > 0 {
		if entry.Attempts > exportMaxAttempts {
			why := fmt.Sprintf("capture did not succeed after %d attempts (last waiting on %s)", entry.Attempts, strings.Join(pending, ", "))
			if entry.LastFailure != "" {
				why = fmt.Sprintf("%s — %s", why, entry.LastFailure)
			}
			return failed(why)
		}
		entry.Message = fmt.Sprintf("capturing retained data; waiting on %s", strings.Join(pending, ", "))
		return r.requeueExport(ctx, export, tenant)
	}
	entry.Phase = gentianov1alpha1.TenantExportPhaseReady
	entry.Message = ""
	entry.Stores = unitKinds(units)
	entry.Artefacts = unitArtefacts(units)
	if err := r.persist(ctx, export); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{Requeue: true}, nil
}

// claimRecord is a volume claim as a bundle records it: what making it
// again takes, with the labels and annotations that say whose it is.
func claimRecord(pvc *corev1.PersistentVolumeClaim) *gentianov1alpha1.BundleClaim {
	out := &gentianov1alpha1.BundleClaim{Labels: map[string]string{}, Annotations: map[string]string{}}
	if size, ok := pvc.Spec.Resources.Requests[corev1.ResourceStorage]; ok {
		out.Size = size.String()
	}
	for _, mode := range pvc.Spec.AccessModes {
		out.AccessModes = append(out.AccessModes, string(mode))
	}
	if pvc.Spec.StorageClassName != nil {
		out.StorageClass = *pvc.Spec.StorageClassName
	}
	for key, value := range pvc.Labels {
		if claimIdentityKey(key) {
			out.Labels[key] = value
		}
	}
	for key, value := range pvc.Annotations {
		if claimIdentityKey(key) {
			out.Annotations[key] = value
		}
	}
	return out
}

// claimIdentityKey reports whether a label or annotation of a claim says
// whose the claim is: the app's and Helm's. What a volume plugin, a
// scheduler or kubectl wrote on it describes the old volume, not the claim.
func claimIdentityKey(key string) bool {
	return strings.HasPrefix(key, "app.kubernetes.io/") || strings.HasPrefix(key, "meta.helm.sh/") ||
		strings.HasPrefix(key, "helm.sh/") || strings.HasPrefix(key, "gentianos.io/")
}
