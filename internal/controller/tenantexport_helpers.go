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

package controller

import (
	"context"
	"fmt"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/backup"
)

// backupTenantComponent labels work that belongs to the tenant rather than to
// any one app — the realm, the shell database, the manifest.
const backupTenantComponent = "gentian-tenant"

// tenantNameFromNamespace recovers the tenant from its namespace, and returns
// "" for a namespace that is not a tenant's.
func tenantNameFromNamespace(namespace string) string {
	const prefix = "tenant-"
	if !strings.HasPrefix(namespace, prefix) || namespace == prefix {
		return ""
	}
	return strings.TrimPrefix(namespace, prefix)
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
func appStatus(export *gentianov1alpha1.TenantExport, appName string) *gentianov1alpha1.AppExportStatus {
	for i := range export.Status.Apps {
		if export.Status.Apps[i].Name == appName {
			return &export.Status.Apps[i]
		}
	}
	export.Status.Apps = append(export.Status.Apps, gentianov1alpha1.AppExportStatus{
		Name:  appName,
		Phase: gentianov1alpha1.TenantExportPhasePending,
	})
	return &export.Status.Apps[len(export.Status.Apps)-1]
}

// nextPendingApp returns the first app still to capture, or "" when all are done.
// Sequential by design: see the reconciler's type comment.
func nextPendingApp(export *gentianov1alpha1.TenantExport, apps []string) string {
	for _, name := range apps {
		done := false
		for i := range export.Status.Apps {
			if export.Status.Apps[i].Name == name &&
				export.Status.Apps[i].Phase == gentianov1alpha1.TenantExportPhaseReady {
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

func markQuiesced(export *gentianov1alpha1.TenantExport, appName string) {
	for _, existing := range export.Status.Quiesced {
		if existing == appName {
			return
		}
	}
	export.Status.Quiesced = append(export.Status.Quiesced, appName)
}

func unmarkQuiesced(export *gentianov1alpha1.TenantExport, appName string) {
	out := export.Status.Quiesced[:0]
	for _, existing := range export.Status.Quiesced {
		if existing != appName {
			out = append(out, existing)
		}
	}
	export.Status.Quiesced = out
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
func profileBackupSpec(profile *gentianov1alpha1.AppProfile) *gentianov1alpha1.BackupSpec {
	if profile == nil {
		return nil
	}
	return profile.Spec.Backup
}

func profileChartVersion(profile *gentianov1alpha1.AppProfile) string {
	if profile == nil {
		return ""
	}
	return profile.Spec.Chart.Version
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

func manifestStores(app gentianov1alpha1.AppExportStatus) []backup.ManifestStore {
	stores := make([]backup.ManifestStore, 0, len(app.Stores))
	for _, kind := range app.Stores {
		stores = append(stores, backup.ManifestStore{Kind: kind, Name: app.Name})
	}
	return stores
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
) (*gentianov1alpha1.AppProfile, error) {
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
		return nil, fmt.Errorf("AppProfile %q not found in the catalogue", appName)
	}
	return profile, nil
}
