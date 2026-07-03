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

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/kernel/secrets"
)

func (r *TenantReconciler) buildDataPlaneJobs(ctx context.Context, tenant *gentianov1alpha1.Tenant) ([]batchv1.Job, error) {
	var jobs []batchv1.Job
	nsName := tenantNamespaceName(tenant)

	if err := r.appendPortalShellRoleJob(ctx, tenant, nsName, &jobs); err != nil {
		return nil, err
	}

	pgApps, err := r.collectPostgresApps(ctx, tenant)
	if err != nil {
		return nil, err
	}
	for _, appName := range pgApps {
		profile := &gentianov1alpha1.AppProfile{}
		if err := r.Get(ctx, client.ObjectKey{Name: appName}, profile); err != nil {
			return nil, fmt.Errorf("get AppProfile %s: %w", appName, err)
		}
		if appUsesCrossplaneDBInit(profile) {
			continue
		}
		dbName := databaseName(tenant, appName)
		rolePassword := ""
		if r.Seeder != nil {
			creds, seedErr := r.Seeder.SeedDatabase(ctx, tenant.Name, appName, secrets.DatabaseCreds{
				Host: fmt.Sprintf("%s-rw.%s.svc.cluster.local", cnpgClusterName, kernelNamespace),
				Port: "5432",
				Name: dbName,
				User: roleUserName(tenant.Name, appName),
			})
			if seedErr != nil {
				return nil, fmt.Errorf("seed database for %s: %w", appName, seedErr)
			}
			rolePassword = creds.Password
		}
		jobs = append(jobs, *makeRoleJob(tenant, nsName, dbName, appName, rolePassword))
	}

	mariaApps, err := r.collectMariaDBApps(ctx, tenant, CollectForProvision)
	if err != nil {
		return nil, err
	}
	for _, appName := range mariaApps {
		profile := &gentianov1alpha1.AppProfile{}
		if err := r.Get(ctx, client.ObjectKey{Name: appName}, profile); err != nil {
			return nil, fmt.Errorf("get AppProfile %s: %w", appName, err)
		}
		allowDynamic := false
		if profile.Spec.KernelRequirements != nil && profile.Spec.KernelRequirements.Database != nil {
			allowDynamic = profile.Spec.KernelRequirements.Database.AllowDynamicDatabaseCreation
		}
		dbPassword := ""
		if r.Seeder != nil {
			creds, seedErr := r.Seeder.SeedMariaDB(ctx, tenant.Name, appName, secrets.DatabaseCreds{
				Host: fmt.Sprintf("%s.%s.svc.cluster.local", "mariadb", kernelNamespace),
				Port: "3306",
				Name: databaseName(tenant, appName),
				User: mariadbUserName(tenant.Name, appName),
			})
			if seedErr != nil {
				return nil, fmt.Errorf("seed mariadb for %s: %w", appName, seedErr)
			}
			dbPassword = creds.Password
		}
		jobs = append(jobs, *makeMariaDBSetupJob(tenant, appName, dbPassword, allowDynamic))
	}

	s3Apps, err := r.collectStorageApps(ctx, tenant, CollectForProvision)
	if err != nil {
		return nil, err
	}
	for _, appName := range s3Apps {
		profile := &gentianov1alpha1.AppProfile{}
		if err := r.Get(ctx, client.ObjectKey{Name: appName}, profile); err != nil {
			return nil, fmt.Errorf("get AppProfile %s: %w", appName, err)
		}
		if appUsesCrossplaneS3Init(profile) {
			continue
		}
		if r.Seeder != nil {
			if _, seedErr := r.Seeder.SeedS3(ctx, tenant.Name, appName, secrets.S3Creds{
				Bucket: s3BucketName(tenant, appName),
			}); seedErr != nil {
				return nil, fmt.Errorf("seed s3 for %s: %w", appName, seedErr)
			}
		}
		jobs = append(jobs, *makeS3BucketJob(tenant, appName))
	}

	redisApps, memcachedApps, err := r.collectCacheApps(ctx, tenant, CollectForProvision)
	if err != nil {
		return nil, err
	}
	for _, appName := range redisApps {
		userPassword := ""
		if r.Seeder != nil {
			redisHost, redisPort, hostErr := r.redisCacheEndpoint(ctx)
			if hostErr != nil {
				return nil, hostErr
			}
			creds, seedErr := r.Seeder.SeedCache(ctx, tenant.Name, appName, secrets.CacheCreds{
				Host: redisHost,
				Port: redisPort,
			})
			if seedErr != nil {
				return nil, fmt.Errorf("seed redis for %s: %w", appName, seedErr)
			}
			userPassword = creds.Password
		}
		jobs = append(jobs, *makeRedisACLJob(tenant, appName, userPassword))
	}
	_ = memcachedApps // memcached workload is emitted via buildDataPlaneObjects

	return jobs, nil
}

func (r *TenantReconciler) buildDataPlaneObjects(ctx context.Context, tenant *gentianov1alpha1.Tenant) ([]client.Object, error) {
	var objects []client.Object
	nsName := tenantNamespaceName(tenant)

	objects = append(objects, buildDatabaseCR(
		tenant,
		nsName,
		databaseName(tenant, portalShellAppName),
		portalShellAppName,
	))

	pgApps, err := r.collectPostgresApps(ctx, tenant)
	if err != nil {
		return nil, err
	}
	for _, appName := range pgApps {
		profile := &gentianov1alpha1.AppProfile{}
		if err := r.Get(ctx, client.ObjectKey{Name: appName}, profile); err != nil {
			return nil, fmt.Errorf("get AppProfile %s: %w", appName, err)
		}
		if appUsesCrossplaneDBInit(profile) {
			continue
		}
		dbName := databaseName(tenant, appName)
		objects = append(objects, buildDatabaseCR(tenant, nsName, dbName, appName))
	}

	_, memcachedApps, err := r.collectCacheApps(ctx, tenant, CollectForProvision)
	if err != nil {
		return nil, err
	}
	if len(memcachedApps) > 0 {
		if r.Seeder != nil {
			for _, appName := range memcachedApps {
				if _, err := r.Seeder.SeedCache(ctx, tenant.Name, appName, secrets.CacheCreds{
					Host: memcachedServiceName,
					Port: fmt.Sprintf("%d", memcachedPort),
				}); err != nil {
					return nil, fmt.Errorf("seed memcached for %s: %w", appName, err)
				}
			}
		}
		dep := makeMemcachedDeployment(tenant)
		dep.SetGroupVersionKind(appsv1.SchemeGroupVersion.WithKind("Deployment"))
		svc := makeMemcachedService(tenant)
		svc.SetGroupVersionKind(corev1.SchemeGroupVersion.WithKind("Service"))
		objects = append(objects, dep, svc)
	}

	bindings, err := r.collectDesiredIntegrationBindings(ctx, tenant)
	if err != nil {
		return nil, err
	}
	for _, ib := range bindings {
		objects = append(objects, ib)
	}

	edgeObjects, err := r.buildTenantEdgeObjects(ctx, tenant)
	if err != nil {
		return nil, err
	}
	objects = append(objects, edgeObjects...)

	return objects, nil
}

func (r *TenantReconciler) appendPortalShellRoleJob(
	ctx context.Context,
	tenant *gentianov1alpha1.Tenant,
	nsName string,
	jobs *[]batchv1.Job,
) error {
	appName := portalShellAppName
	dbName := databaseName(tenant, appName)
	rolePassword := ""
	if r.Seeder != nil {
		creds, err := r.Seeder.SeedDatabase(ctx, tenant.Name, appName, secrets.DatabaseCreds{
			Host: fmt.Sprintf("%s-rw.%s.svc.cluster.local", cnpgClusterName, kernelNamespace),
			Port: "5432",
			Name: dbName,
			User: roleUserName(tenant.Name, appName),
		})
		if err != nil {
			return fmt.Errorf("seed portal shell database: %w", err)
		}
		rolePassword = creds.Password
	}
	*jobs = append(*jobs, *makeRoleJob(tenant, nsName, dbName, appName, rolePassword))
	return nil
}

// collectDesiredIntegrationBindings returns IntegrationBinding CRs that should
// exist for the tenant.
func (r *TenantReconciler) collectDesiredIntegrationBindings(ctx context.Context, tenant *gentianov1alpha1.Tenant) ([]*gentianov1alpha1.IntegrationBinding, error) {
	nsName := tenantNamespaceName(tenant)
	presentApps := make(map[string]struct{}, len(tenant.Spec.Apps))
	for _, app := range tenant.Spec.Apps {
		presentApps[app.Profile] = struct{}{}
	}

	var out []*gentianov1alpha1.IntegrationBinding
	for _, app := range tenant.Spec.Apps {
		profile := &gentianov1alpha1.AppProfile{}
		if err := r.Get(ctx, client.ObjectKey{Name: app.Profile}, profile); err != nil {
			if errors.IsNotFound(err) {
				continue
			}
			return nil, fmt.Errorf("get AppProfile %s: %w", app.Profile, err)
		}
		for _, integration := range profile.Spec.OptionalIntegrations {
			providerApp := ""
			if integration.Provider != "" {
				if _, ok := presentApps[integration.Provider]; ok {
					providerApp = integration.Provider
				}
			} else {
				providerApp = r.findProviderInTenant(ctx, tenant, integration.Contract, presentApps, app.Profile)
			}
			if providerApp == "" {
				continue
			}
			name := integrationBindingName(tenant.Name, app.Profile, integration.Contract)
			ib := buildIntegrationBinding(name, nsName, tenant.Name, app.Profile, providerApp, integration)
			ib.SetGroupVersionKind(gentianov1alpha1.GroupVersion.WithKind("IntegrationBinding"))
			out = append(out, ib)
		}
	}
	return out, nil
}
