/*
Copyright 2026 The Gentian Authors.

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

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
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
		fmt.Sprintf("mariadb-delete-%s-", tenantName),
		fmt.Sprintf("s3-delete-%s-", tenantName),
		fmt.Sprintf("nc-group-delete-%s", tenantName),
		fmt.Sprintf("redis-acl-delete-%s-", tenantName),
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
	jobList := &batchv1.JobList{}
	if err := r.List(ctx, jobList, client.InNamespace(kernelNamespace), tenantKernelLabelSelector(tenantName)); err != nil {
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

func (r *TenantReconciler) collectMariaDBAppsForDelete(ctx context.Context, tenant *gentianov1alpha1.Tenant) ([]string, error) {
	var apps []string
	for _, app := range tenant.Spec.Apps {
		profile := &gentianov1alpha1.AppProfile{}
		if err := r.Get(ctx, client.ObjectKey{Name: app.Profile}, profile); err != nil {
			if errors.IsNotFound(err) {
				continue
			}
			return nil, err
		}
		if profile.Spec.KernelRequirements == nil ||
			profile.Spec.KernelRequirements.Database == nil ||
			profile.Spec.KernelRequirements.Database.Engine != gentianov1alpha1.DatabaseEngineMariaDB {
			continue
		}
		apps = appendUniqueStrings(apps, app.Profile)
	}
	fromJobs, err := r.listTenantAppsFromJobPrefix(ctx, tenant.Name, mariadbSetupJobName(tenant.Name, ""))
	if err != nil {
		return nil, err
	}
	return appendUniqueStrings(apps, fromJobs...), nil
}

func (r *TenantReconciler) collectStorageAppsForDelete(ctx context.Context, tenant *gentianov1alpha1.Tenant) ([]string, error) {
	apps, err := r.collectStorageApps(ctx, tenant)
	if err != nil {
		return nil, err
	}
	fromJobs, err := r.listTenantAppsFromJobPrefix(ctx, tenant.Name, s3BucketJobName(tenant.Name, ""))
	if err != nil {
		return nil, err
	}
	return appendUniqueStrings(apps, fromJobs...), nil
}

func (r *TenantReconciler) collectCacheAppsForDelete(ctx context.Context, tenant *gentianov1alpha1.Tenant) (redisApps, memcachedApps []string, err error) {
	redisApps, memcachedApps, err = r.collectCacheApps(ctx, tenant)
	if err != nil {
		return nil, nil, err
	}
	fromJobs, err := r.listTenantAppsFromJobPrefix(ctx, tenant.Name, redisACLJobName(tenant.Name, ""))
	if err != nil {
		return nil, nil, err
	}
	redisApps = appendUniqueStrings(redisApps, fromJobs...)
	return redisApps, memcachedApps, nil
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
	if err := r.List(ctx, dbList, client.InNamespace(kernelNamespace), tenantKernelLabelSelector(tenantName)); err != nil {
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

	if err := r.deleteTenantLabeledDatabaseCRs(ctx, tenant.Name); err != nil {
		return err
	}

	secList := &corev1.SecretList{}
	if err := r.List(ctx, secList, client.InNamespace(kernelNamespace), selector); err != nil {
		return fmt.Errorf("list Secrets for tenant %s: %w", tenant.Name, err)
	}
	for i := range secList.Items {
		if err := r.Delete(ctx, &secList.Items[i]); client.IgnoreNotFound(err) != nil {
			return fmt.Errorf("delete Secret %s: %w", secList.Items[i].Name, err)
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

	jobList := &batchv1.JobList{}
	if err := r.List(ctx, jobList, client.InNamespace(kernelNamespace), selector); err != nil {
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
