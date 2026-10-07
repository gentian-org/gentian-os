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
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/backup"
)

// backupTenantComponent labels work that belongs to the tenant rather than to
// any one app — the realm, the shell database, the manifest.
const backupTenantComponent = "gentian-tenant"

// scheduleLabel ties an export back to the schedule that created it, which is
// how retention finds its own exports without touching a hand-made one.
const scheduleLabel = "gentianos.io/tenant-export-schedule"

// tenantNameFromNamespace recovers the tenant from its namespace, and returns
// "" for a namespace that is not a tenant's.
func tenantNameFromNamespace(namespace string) string {
	const prefix = "tenant-"
	if !strings.HasPrefix(namespace, prefix) || namespace == prefix {
		return ""
	}
	return strings.TrimPrefix(namespace, prefix)
}

// tenantNamespaceByName is the namespace of the tenant called name, by the one
// rule there is (backup.TenantNamespace). The helpers that pause, resume and
// exec into an app are handed a tenant's name, sometimes after the Tenant is
// gone -- resuming what an export paused must still work then -- so a tenant
// that is not found is given the namespace the rule gives a tenant that sets
// nothing. Any other error is returned: not knowing where a tenant's
// workloads are is not a reason to look somewhere else.
func tenantNamespaceByName(ctx context.Context, c client.Reader, name string) (string, error) {
	tenant := &gentianov1alpha1.Tenant{}
	err := c.Get(ctx, types.NamespacedName{Name: name}, tenant)
	if apierrors.IsNotFound(err) {
		tenant = &gentianov1alpha1.Tenant{ObjectMeta: metav1.ObjectMeta{Name: name}}
	} else if err != nil {
		return "", fmt.Errorf("read tenant %s to find its namespace: %w", name, err)
	}
	return backup.TenantNamespace(tenant), nil
}

// exportJobName builds a Job name that is unique per export, app and unit, and
// short enough to survive Kubernetes' 63-character limit on the pod labels
// derived from it. Long profile names are truncated from the middle of the
// composite rather than the end, so the unit suffix always survives.
func exportJobName(exportName, appName, unit string) string {
	name := fmt.Sprintf("tx-%s-%s-%s", exportName, appName, unit)
	const max = 52
	if len(name) <= max {
		return name
	}
	keep := max - len(unit) - 1
	if keep < 1 {
		keep = 1
	}
	return name[:keep] + "-" + unit
}

// appStatus returns this app's status entry, creating it on first use so the
// caller can mutate it in place.
// The four helpers below take the slices rather than the CR.
//
// TenantExport and TenantRestore carry the same two status fields — Apps and
// Quiesced — and each had its own copy of every one of these, differing only in
// the receiver type: appStatus/restoreAppStatus, nextPendingApp/
// nextPendingRestoreApp, markQuiesced/markRestoreQuiesced, and unmark likewise.
// Eight functions for four behaviours, on the two paths where getting it wrong
// means a backup that silently skips an app or a restore that leaves one
// quiesced. Taking the slice makes them one set that both callers share.

// appStatus returns the entry for appName, appending a Pending one if absent.
func appStatus(apps *[]gentianov1alpha1.AppExportStatus, appName string) *gentianov1alpha1.AppExportStatus {
	for i := range *apps {
		if (*apps)[i].Name == appName {
			return &(*apps)[i]
		}
	}
	*apps = append(*apps, gentianov1alpha1.AppExportStatus{
		Name:  appName,
		Phase: gentianov1alpha1.TenantExportPhasePending,
	})
	return &(*apps)[len(*apps)-1]
}

// nextPendingApp returns the first app still to process, or "" when all are done.
// Sequential by design: see the reconciler's type comment.
func nextPendingApp(apps []gentianov1alpha1.AppExportStatus, want []string) string {
	for _, name := range want {
		done := false
		for i := range apps {
			if apps[i].Name == name && apps[i].Phase == gentianov1alpha1.TenantExportPhaseReady {
				done = true
				break
			}
		}
		if !done {
			return name
		}
	}
	return ""
}

func markQuiesced(quiesced *[]string, appName string) {
	for _, existing := range *quiesced {
		if existing == appName {
			return
		}
	}
	*quiesced = append(*quiesced, appName)
}

func unmarkQuiesced(quiesced *[]string, appName string) {
	out := (*quiesced)[:0]
	for _, existing := range *quiesced {
		if existing != appName {
			out = append(out, existing)
		}
	}
	*quiesced = out
}

func setExportCondition(
	export *gentianov1alpha1.TenantExport,
	condType string,
	status metav1.ConditionStatus,
	reason, message string,
) {
	now := metav1.Now()
	for i := range export.Status.Conditions {
		c := &export.Status.Conditions[i]
		if c.Type != condType {
			continue
		}
		if c.Status != status {
			c.LastTransitionTime = now
		}
		c.Status, c.Reason, c.Message = status, reason, message
		c.ObservedGeneration = export.Generation
		return
	}
	export.Status.Conditions = append(export.Status.Conditions, metav1.Condition{
		Type:               condType,
		Status:             status,
		Reason:             reason,
		Message:            message,
		LastTransitionTime: now,
		ObservedGeneration: export.Generation,
	})
}

// profileBackupSpec returns the profile's backup contract, or nil when it
// declares none — which the accessors read as the platform default.
func profileBackupSpec(profile *gentianov1alpha1.ComponentProfile) *gentianov1alpha1.BackupSpec {
	if profile == nil {
		return nil
	}
	return profile.Spec.Backup
}

func profileChartVersion(profile *gentianov1alpha1.ComponentProfile) string {
	// Chart() is nil for an entry that is not delivered as one -- an API
	// entry, an addon. Neither has a chart version, and an export that
	// recorded one would be recording a fiction.
	chart := profile.Chart()
	if chart == nil {
		return ""
	}
	return chart.Version
}

func unitKinds(units []captureUnit) []string {
	seen := map[string]struct{}{}
	var kinds []string
	for _, unit := range units {
		if _, ok := seen[unit.Kind]; ok {
			continue
		}
		seen[unit.Kind] = struct{}{}
		kinds = append(kinds, unit.Kind)
	}
	return kinds
}

// unitArtefacts is what a set of capture units produced, as the status and
// the manifest record it.
func unitArtefacts(units []captureUnit) []gentianov1alpha1.BundleArtefact {
	out := make([]gentianov1alpha1.BundleArtefact, 0, len(units))
	for _, unit := range units {
		out = append(out, gentianov1alpha1.BundleArtefact{
			Kind: unit.Kind, Name: unit.Name, Path: unit.Path, Release: unit.Release,
		})
	}
	return out
}

// manifestStores is one entry per artefact captured for the app: its kind,
// what it was captured from and where in the bundle it is.
func manifestStores(app gentianov1alpha1.AppExportStatus) []backup.ManifestStore {
	stores := make([]backup.ManifestStore, 0, len(app.Artefacts))
	for _, a := range app.Artefacts {
		stores = append(stores, backup.ManifestStore{Kind: a.Kind, Name: a.Name, Path: a.Path, Release: a.Release})
	}
	return stores
}

// tenantAppDigest is the digest the tenant pins the app's build to, "" when
// it pins none.
func tenantAppDigest(tenant *gentianov1alpha1.Tenant, appName string) string {
	for _, app := range tenant.Spec.Apps {
		if app.Profile == appName {
			return app.Digest
		}
	}
	return ""
}

func ptrNow() *metav1.Time {
	now := metav1.Now()
	return &now
}

func timeOrNow(t *metav1.Time) string {
	if t == nil {
		return metav1.Now().UTC().Format(time.RFC3339)
	}
	return t.UTC().Format(time.RFC3339)
}

func timeOrEmpty(t *metav1.Time) string {
	if t == nil {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// resolveProfile looks up the AppProfile backing an installed app, so every
// capture decision is driven by the catalogue rather than by the app's name.
//
// A missing profile is an error rather than a shrug: without it there is no
// kernelRequirements to enumerate, and an export that quietly captured nothing
// for an app would be worse than one that refuses.
func resolveProfile(
	ctx context.Context,
	c client.Client,
	tenant *gentianov1alpha1.Tenant,
	appName string,
) (*gentianov1alpha1.ComponentProfile, error) {
	installed := false
	for _, app := range tenant.Spec.Apps {
		if app.Profile == appName {
			installed = true
			break
		}
	}
	if !installed {
		return nil, fmt.Errorf("app %q is not installed for tenant %q", appName, tenant.Name)
	}

	index, err := loadAppProfileIndex(ctx, c)
	if err != nil {
		return nil, err
	}
	profile, ok := appProfileFromIndex(index, appName)
	if !ok {
		return nil, fmt.Errorf("ComponentProfile %q not found in the catalogue", appName)
	}
	return profile, nil
}
