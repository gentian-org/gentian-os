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
	"sort"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/backup"
	"github.com/gentian-org/gentian-os/internal/controller/provisioner"
	"github.com/gentian-org/gentian-os/internal/kernel/netpolicy"
	"github.com/gentian-org/gentian-os/internal/kernel/secrets"
)

// buildDataPlaneJobs builds the Jobs that provision a tenant's stores, and
// the passwords and key pairs those Jobs are to be handed, by Job
// (ensureProvisioningJobSecrets): those are not in the Jobs.
func (r *TenantReconciler) buildDataPlaneJobs(ctx context.Context, tenant *gentianov1alpha1.Tenant) ([]batchv1.Job, provisioningJobSecrets, error) {
	var jobs []batchv1.Job
	keys := provisioningJobSecrets{}
	nsName := tenantNamespaceName(tenant)

	pgApps, err := r.collectPostgresApps(ctx, tenant)
	if err != nil {
		return nil, nil, err
	}
	pgProfileIndex, err := loadComponentProfileIndex(ctx, r.Client)
	if err != nil {
		return nil, nil, err
	}
	for _, appName := range pgApps {
		profile, _ := appProfileFromIndex(pgProfileIndex, appName)
		dbName := databaseName(tenant, appName)
		rolePassword := ""
		if r.Seeder != nil {
			creds, seedErr := r.Seeder.SeedDatabase(ctx, tenant.Name, appName, secrets.DatabaseCreds{
				Host: fmt.Sprintf("%s-rw.%s.svc.cluster.local", cnpgClusterName, postgresNamespace),
				Port: fmt.Sprint(provisioner.PostgresPort),
				Name: dbName,
				User: roleUserName(tenant.Name, appName),
			})
			if seedErr != nil {
				return nil, nil, fmt.Errorf("seed database for %s: %w", appName, seedErr)
			}
			rolePassword = creds.Password
		}
		if rolePassword != "" {
			keys[types.NamespacedName{Namespace: postgresNamespace, Name: roleJobName(tenant.Name, appName)}] = passwordSecretData(rolePassword)
		}
		jobs = append(jobs, *makeRoleJob(tenant, nsName, dbName, appName, rolePassword != "",
			schemaPreferenceFor(profile), allowsDynamicDatabaseCreation(profile)))
	}

	mariaApps, err := r.collectMariaDBApps(ctx, tenant, CollectForProvision)
	if err != nil {
		return nil, nil, err
	}
	for _, appName := range mariaApps {
		profile := &gentianov1alpha1.ComponentProfile{}
		if err := r.Get(ctx, client.ObjectKey{Name: appName}, profile); err != nil {
			return nil, nil, fmt.Errorf("get ComponentProfile %s: %w", appName, err)
		}
		allowDynamic := false
		if profile.Services() != nil && profile.Services().Database != nil {
			allowDynamic = profile.Services().Database.AllowDynamicDatabaseCreation
		}
		dbPassword := ""
		if r.Seeder != nil {
			creds, seedErr := r.Seeder.SeedMariaDB(ctx, tenant.Name, appName, secrets.DatabaseCreds{
				Host: fmt.Sprintf("%s.%s.svc.cluster.local", "mariadb", mariadbNamespace),
				Port: fmt.Sprint(provisioner.MariaDBPort),
				Name: databaseName(tenant, appName),
				User: mariadbUserName(tenant.Name, appName),
			})
			if seedErr != nil {
				return nil, nil, fmt.Errorf("seed mariadb for %s: %w", appName, seedErr)
			}
			dbPassword = creds.Password
		}
		if dbPassword != "" {
			keys[types.NamespacedName{Namespace: mariadbNamespace, Name: mariadbSetupJobName(tenant.Name, appName)}] = passwordSecretData(dbPassword)
		}
		jobs = append(jobs, *makeMariaDBSetupJob(tenant, appName, dbPassword != "", allowDynamic))
	}

	s3Apps, err := r.collectStorageApps(ctx, tenant, CollectForProvision)
	if err != nil {
		return nil, nil, err
	}
	for _, appName := range s3Apps {
		accessKey, secretKey, seedErr := r.seedObjectStorage(ctx, tenant, appName)
		if seedErr != nil {
			return nil, nil, seedErr
		}
		withKey := accessKey != "" && secretKey != ""
		if withKey {
			keys[types.NamespacedName{Namespace: s3Namespace, Name: s3BucketJobName(tenant.Name, appName)}] = map[string][]byte{
				backup.ObjectStorageKeyAccessKey: []byte(accessKey),
				backup.ObjectStorageKeySecretKey: []byte(secretKey),
			}
		}
		jobs = append(jobs, *makeS3BucketJob(tenant, appName, withKey))
	}

	redisApps, memcachedApps, err := r.collectCacheApps(ctx, tenant, CollectForProvision)
	if err != nil {
		return nil, nil, err
	}
	for _, appName := range redisApps {
		userPassword := ""
		if r.Seeder != nil {
			redisHost, redisPort, hostErr := r.redisCacheEndpoint(ctx)
			if hostErr != nil {
				return nil, nil, hostErr
			}
			creds, seedErr := r.Seeder.SeedCache(ctx, tenant.Name, appName, secrets.CacheCreds{
				Host: redisHost,
				Port: redisPort,
				User: redisACLUsername(tenant.Name, appName),
			})
			if seedErr != nil {
				return nil, nil, fmt.Errorf("seed redis for %s: %w", appName, seedErr)
			}
			userPassword = creds.Password
		}
		if userPassword != "" {
			keys[types.NamespacedName{Namespace: cacheNamespace, Name: redisACLJobName(tenant.Name, appName)}] = passwordSecretData(userPassword)
		}
		jobs = append(jobs, *makeRedisACLJob(tenant, appName, userPassword != ""))
	}
	_ = memcachedApps // memcached workload is emitted via buildDataPlaneObjects

	return jobs, keys, nil
}

func (r *TenantReconciler) buildDataPlaneObjects(ctx context.Context, tenant *gentianov1alpha1.Tenant) ([]client.Object, error) {
	var objects []client.Object
	nsName := tenantNamespaceName(tenant)

	pgApps, err := r.collectPostgresApps(ctx, tenant)
	if err != nil {
		return nil, err
	}
	for _, appName := range pgApps {
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
		if r.Seeder != nil {
			if _, err := r.Seeder.SeedContract(ctx, tenant.Name, ib.Spec.Contract); err != nil {
				return nil, fmt.Errorf("seed contract %s: %w", ib.Spec.Contract, err)
			}
		}
		objects = append(objects, ib)
	}
	if r.Seeder != nil {
		// A component that maps a contract's credentials has them whether
		// or not the other side is installed: a provider holds the password
		// its consumers present before any of them is, and a consumer is
		// told an empty endpoint while no provider is.
		mapped, err := r.mappedContracts(ctx, tenant)
		if err != nil {
			return nil, err
		}
		for _, contract := range mapped {
			if _, err := r.Seeder.SeedContract(ctx, tenant.Name, contract); err != nil {
				return nil, fmt.Errorf("seed contract %s: %w", contract, err)
			}
		}
		if err := r.clearUngrantedContractEndpoints(ctx, tenant, bindings, mapped); err != nil {
			return nil, err
		}
	}

	edgeObjects, err := r.buildTenantEdgeObjects(ctx, tenant)
	if err != nil {
		return nil, err
	}
	objects = append(objects, edgeObjects...)

	return objects, nil
}

// clearUngrantedContractEndpoints empties the endpoint of every contract the
// tenant seeds that no granted binding answers: its provider was removed, so
// the binding is no longer desired, or the consumer's grant was withdrawn.
// The binding's reconciler writes the endpoint, and has nothing left to
// reconcile once its binding is gone; this is read from what is desired now,
// so an operator restarted in between clears it on its next pass all the
// same.
func (r *TenantReconciler) clearUngrantedContractEndpoints(
	ctx context.Context,
	tenant *gentianov1alpha1.Tenant,
	bindings []*gentianov1alpha1.IntegrationBinding,
	mapped []string,
) error {
	var grants gentianov1alpha1.AppGrantList
	if err := r.List(ctx, &grants, client.InNamespace(tenantNamespaceName(tenant))); err != nil {
		return fmt.Errorf("list AppGrants for tenant %s: %w", tenant.Name, err)
	}
	grantOf := make(map[string]*gentianov1alpha1.AppGrant, len(grants.Items))
	for i := range grants.Items {
		grantOf[grants.Items[i].Spec.App] = &grants.Items[i]
	}
	seeded := map[string]struct{}{}
	granted := map[string]struct{}{}
	for _, ib := range bindings {
		seeded[ib.Spec.Contract] = struct{}{}
		if len(netpolicy.EffectiveContractCapabilities(ib, grantOf[ib.Spec.Consumer.App])) > 0 {
			granted[ib.Spec.Contract] = struct{}{}
		}
	}
	for _, contract := range mapped {
		seeded[contract] = struct{}{}
	}
	for contract := range seeded {
		if _, ok := granted[contract]; ok {
			continue
		}
		if err := r.Seeder.ClearContractEndpoint(ctx, tenant.Name, contract); err != nil {
			return err
		}
	}
	return nil
}

// mappedContracts are the contracts the tenant's installed components map
// under package.valueMapping.integrations, sorted.
func (r *TenantReconciler) mappedContracts(ctx context.Context, tenant *gentianov1alpha1.Tenant) ([]string, error) {
	seen := map[string]struct{}{}
	for _, app := range tenant.Spec.Apps {
		profile, err := getProfileFor(ctx, r.Client, app.Profile, app.Digest)
		if err != nil {
			if errors.IsNotFound(err) {
				continue
			}
			return nil, fmt.Errorf("get ComponentProfile %s: %w", app.Profile, err)
		}
		if m := profile.Spec.Package.ValueMapping; m != nil {
			for contract := range m.Integrations {
				seen[contract] = struct{}{}
			}
		}
	}
	out := make([]string, 0, len(seen))
	for contract := range seen {
		out = append(out, contract)
	}
	sort.Strings(out)
	return out, nil
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
		profile, err := getProfileFor(ctx, r.Client, app.Profile, app.Digest)
		if err != nil {
			if errors.IsNotFound(err) {
				continue
			}
			return nil, fmt.Errorf("get ComponentProfile %s: %w", app.Profile, err)
		}
		for _, integration := range profile.Spec.Integrations {
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
			endpoint, err := r.providedEndpoint(ctx, providerApp, integration.Contract)
			if err != nil {
				return nil, err
			}
			name := integrationBindingName(tenant.Name, app.Profile, integration.Contract)
			ib := buildIntegrationBinding(name, nsName, tenant.Name, app.Profile, providerApp, integration, endpoint)
			ib.SetGroupVersionKind(gentianov1alpha1.GroupVersion.WithKind("IntegrationBinding"))
			out = append(out, ib)
		}
	}
	return out, nil
}
