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

	runtimeMeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/backup"
	"github.com/gentian-org/gentian-os/internal/kernel/secrets"
)

func tenantKernelLabelSelector(tenantName string) client.MatchingLabels {
	return client.MatchingLabels{
		tenantLabel:    tenantName,
		managedByLabel: managedByValue,
	}
}

func appendUniqueStrings(base []string, extra ...string) []string {
	seen := make(map[string]struct{}, len(base)+len(extra))
	out := make([]string, 0, len(base)+len(extra))
	for _, s := range base {
		if s == "" {
			continue
		}
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	for _, s := range extra {
		if s == "" {
			continue
		}
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}

func isTenantCleanupJobName(tenantName, jobName string) bool {
	cleanupPrefixes := []string{
		fmt.Sprintf("keycloak-realm-delete-%s", tenantName),
		fmt.Sprintf("keycloak-realm-disable-%s", tenantName),
		backup.MariaDBDestroyJobName(tenantName, ""),
		backup.PostgresDestroyJobName(tenantName, ""),
		backup.ObjectStorageDestroyJobName(tenantName, ""),
		backup.CacheDestroyJobName(tenantName, ""),
	}
	for _, prefix := range cleanupPrefixes {
		if strings.HasPrefix(jobName, prefix) {
			return true
		}
	}
	return false
}

// listTenantAppsFromJobPrefix returns app profile names inferred from completed
// or attempted provision Jobs whose names start with namePrefix.
func (r *TenantReconciler) listTenantAppsFromJobPrefix(ctx context.Context, tenantName, namePrefix string) ([]string, error) {
	jobList, err := r.listTenantPlatformJobs(ctx, tenantKernelLabelSelector(tenantName))
	if err != nil {
		return nil, fmt.Errorf("list Jobs for tenant %s: %w", tenantName, err)
	}
	var apps []string
	for _, job := range jobList.Items {
		if !strings.HasPrefix(job.Name, namePrefix) {
			continue
		}
		app := strings.TrimPrefix(job.Name, namePrefix)
		apps = appendUniqueStrings(apps, app)
	}
	return apps, nil
}

// deleteTenantLabeledDatabaseCRs removes all CloudNativePG Database CRs owned by
// the tenant regardless of the current apps list.
func (r *TenantReconciler) deleteTenantLabeledDatabaseCRs(ctx context.Context, tenantName string) error {
	dbList := &unstructured.UnstructuredList{}
	dbList.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   cnpgGroup,
		Version: cnpgVersion,
		Kind:    cnpgDatabaseKind + "List",
	})
	if err := r.List(ctx, dbList, client.InNamespace(postgresNamespace), tenantKernelLabelSelector(tenantName)); err != nil {
		return fmt.Errorf("list Database CRs for tenant %s: %w", tenantName, err)
	}
	for i := range dbList.Items {
		if err := r.Delete(ctx, &dbList.Items[i]); client.IgnoreNotFound(err) != nil {
			return fmt.Errorf("delete Database CR %s: %w", dbList.Items[i].GetName(), err)
		}
	}
	return nil
}

// purgeTenantKernelResources removes orchestrator-owned kernel artifacts that
// may survive app uninstalls or partial deletes. It runs after awaited cleanup
// Jobs have finished; still-active fire-and-forget cleanup Jobs are left to
// finish and expire via their TTL.
func (r *TenantReconciler) purgeTenantKernelResources(ctx context.Context, tenant *gentianov1alpha1.Tenant) error {
	if tenant.Spec.DeletionPolicy != gentianov1alpha1.DeletionPolicyDelete {
		return nil
	}

	selector := tenantKernelLabelSelector(tenant.Name)

	// The tenant's own OpenBao subtree. Only per-app subtrees were purged, so
	// …/tenants/<t>/admin outlived the tenant — and the admin credential is
	// seeded write-once, so a tenant recreated under the same name silently
	// inherited the previous one's login. "Delete it and make it again" did not
	// do what it looks like it does.
	//
	// A failure here fails the pass, and the deletion comes back to it. It
	// used to be logged and passed over, so that an unreachable vault would
	// not hold the finalizer; what that bought was a tenant reported deleted
	// with its credentials still stored. The Tenant stays, Terminating, until
	// the vault answers -- which is the true state. Retain skips this with
	// everything else, which is the policy's meaning: the data stays.
	//
	// An operator with no vault configured has none to purge.
	if r.Seeder != nil && r.Seeder.KV() != nil {
		if err := r.Seeder.KV().DeleteTree(ctx, secrets.TenantPath(tenant.Name)); err != nil {
			return fmt.Errorf("purge the vault paths of tenant %s: %w", tenant.Name, err)
		}
	}

	if err := r.deleteTenantLabeledDatabaseCRs(ctx, tenant.Name); err != nil {
		return err
	}

	secList, err := r.listTenantPlatformSecrets(ctx, selector)
	if err != nil {
		return fmt.Errorf("list Secrets for tenant %s: %w", tenant.Name, err)
	}
	for i := range secList.Items {
		if err := r.Delete(ctx, &secList.Items[i]); client.IgnoreNotFound(err) != nil {
			return fmt.Errorf("delete Secret %s: %w", secList.Items[i].Name, err)
		}
	}

	// The tenant's edge DNSEndpoint, so its records leave the zone with it —
	// external-dns's policy: sync deletes what its source object stops
	// declaring. Listed by the same label pair as everything else here; absent
	// (static-ip, or no external-dns) the list is simply empty.
	depList := &unstructured.UnstructuredList{}
	depList.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   dnsEndpointGVK.Group,
		Version: dnsEndpointGVK.Version,
		Kind:    dnsEndpointGVK.Kind + "List",
	})
	if err := r.List(ctx, depList, selector); err != nil {
		// A cluster without the CRD cannot list it; that is not residue.
		if !runtimeMeta.IsNoMatchError(err) {
			return fmt.Errorf("list DNSEndpoints for tenant %s: %w", tenant.Name, err)
		}
	}
	for i := range depList.Items {
		if err := r.Delete(ctx, &depList.Items[i]); client.IgnoreNotFound(err) != nil {
			return fmt.Errorf("delete DNSEndpoint %s: %w", depList.Items[i].GetName(), err)
		}
	}

	relList := &unstructured.UnstructuredList{}
	relList.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   helmReleaseGVK.Group,
		Version: helmReleaseGVK.Version,
		Kind:    "ReleaseList",
	})
	if err := r.List(ctx, relList, selector); err != nil {
		return fmt.Errorf("list Helm Releases for tenant %s: %w", tenant.Name, err)
	}
	for i := range relList.Items {
		if err := r.Delete(ctx, &relList.Items[i]); client.IgnoreNotFound(err) != nil {
			return fmt.Errorf("delete Helm Release %s: %w", relList.Items[i].GetName(), err)
		}
	}

	jobList, err := r.listTenantPlatformJobs(ctx, selector)
	if err != nil {
		return fmt.Errorf("list Jobs for tenant %s: %w", tenant.Name, err)
	}
	prop := metav1.DeletePropagationBackground
	for i := range jobList.Items {
		job := &jobList.Items[i]
		if isTenantCleanupJobName(tenant.Name, job.Name) && !jobIsComplete(job) && job.DeletionTimestamp == nil {
			continue
		}
		if err := r.Delete(ctx, job, &client.DeleteOptions{PropagationPolicy: &prop}); client.IgnoreNotFound(err) != nil {
			return fmt.Errorf("delete Job %s: %w", job.Name, err)
		}
	}
	return nil
}
