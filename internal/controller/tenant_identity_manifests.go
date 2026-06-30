/*
Copyright 2026 The Gentian Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the License.
*/

package controller

import (
	"context"
	"encoding/json"
	"fmt"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/kernel/secrets"
)

const (
	tenantProvisioningJobsConfigType = "tenant-provisioning-jobs"
	tenantProvisioningJobsDataKey    = "jobs.json"
)

func tenantProvisioningConfigMapName(tenantName string) string {
	return "tenant-" + tenantName + "-provisioning-jobs"
}

// ensureTenantProvisioningManifests writes Batch Job and Kubernetes object manifests
// that Crossplane applies via tenant-default. The operator seeds
// credentials and updates this ConfigMap; it does not create those resources directly.
func (r *TenantReconciler) ensureTenantProvisioningManifests(ctx context.Context, tenant *gentianov1alpha1.Tenant) error {
	if err := r.cleanupOrphanedNextcloudGroupJob(ctx, tenant); err != nil {
		return fmt.Errorf("cleanup orphaned Nextcloud group Job: %w", err)
	}
	jobs, err := r.buildTenantProvisioningJobs(ctx, tenant)
	if err != nil {
		return fmt.Errorf("build tenant provisioning jobs: %w", err)
	}
	jobsPayload, err := serializeProvisioningJobs(jobs)
	if err != nil {
		return fmt.Errorf("serialize tenant provisioning jobs: %w", err)
	}

	objects, err := r.buildTenantProvisioningObjects(ctx, tenant)
	if err != nil {
		return fmt.Errorf("build tenant provisioning objects: %w", err)
	}
	objectsPayload, err := serializeProvisioningObjects(objects)
	if err != nil {
		return fmt.Errorf("serialize tenant provisioning objects: %w", err)
	}

	name := tenantProvisioningConfigMapName(tenant.Name)
	desired := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: kernelNamespace,
			Labels: map[string]string{
				tenantLabel:                tenant.Name,
				"gentianos.io/config-type": tenantProvisioningJobsConfigType,
				managedByLabel:             managedByValue,
			},
		},
		Data: map[string]string{
			tenantProvisioningJobsDataKey:    jobsPayload,
			tenantProvisioningObjectsDataKey: objectsPayload,
		},
	}

	existing := &corev1.ConfigMap{}
	err = r.Get(ctx, types.NamespacedName{Name: name, Namespace: kernelNamespace}, existing)
	if errors.IsNotFound(err) {
		return r.Create(ctx, desired)
	}
	if err != nil {
		return err
	}
	if existing.Data != nil &&
		existing.Data[tenantProvisioningJobsDataKey] == jobsPayload &&
		existing.Data[tenantProvisioningObjectsDataKey] == objectsPayload {
		return nil
	}
	patch := client.MergeFrom(existing.DeepCopy())
	if existing.Data == nil {
		existing.Data = map[string]string{}
	}
	existing.Data[tenantProvisioningJobsDataKey] = jobsPayload
	existing.Data[tenantProvisioningObjectsDataKey] = objectsPayload
	existing.Labels = desired.Labels
	return r.Patch(ctx, existing, patch)
}

func (r *TenantReconciler) buildTenantProvisioningObjects(ctx context.Context, tenant *gentianov1alpha1.Tenant) ([]client.Object, error) {
	return r.buildDataPlaneObjects(ctx, tenant)
}

func serializeProvisioningJobs(jobs []batchv1.Job) (string, error) {
	clean := make([]batchv1.Job, 0, len(jobs))
	for i := range jobs {
		clean = append(clean, sanitizeJobForJSON(&jobs[i]))
	}
	raw, err := json.Marshal(clean)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

func sanitizeJobForJSON(job *batchv1.Job) batchv1.Job {
	out := job.DeepCopy()
	out.TypeMeta = metav1.TypeMeta{APIVersion: batchv1.SchemeGroupVersion.String(), Kind: "Job"}
	out.ResourceVersion = ""
	out.UID = ""
	out.Generation = 0
	out.CreationTimestamp = metav1.Time{}
	out.ManagedFields = nil
	out.Status = batchv1.JobStatus{}
	out.Spec.Selector = nil
	out.Spec.Template.ResourceVersion = ""
	return *out
}

func (r *TenantReconciler) buildTenantProvisioningJobs(ctx context.Context, tenant *gentianov1alpha1.Tenant) ([]batchv1.Job, error) {
	var jobs []batchv1.Job
	realmName := keycloakRealmName(tenant)

	identityJobs, err := r.buildIdentityProvisioningJobs(ctx, tenant, realmName)
	if err != nil {
		return nil, err
	}
	jobs = append(jobs, identityJobs...)

	dataJobs, err := r.buildDataPlaneJobs(ctx, tenant)
	if err != nil {
		return nil, err
	}
	jobs = append(jobs, dataJobs...)
	return jobs, nil
}

func (r *TenantReconciler) buildIdentityProvisioningJobs(ctx context.Context, tenant *gentianov1alpha1.Tenant, realmName string) ([]batchv1.Job, error) {
	var jobs []batchv1.Job

	var broker *realmBrokerParams
	if r.KernelRealm != "" && r.KernelDomain != "" {
		broker = &realmBrokerParams{
			kernelRealm:       r.KernelRealm,
			kernelExternalURL: kernelExternalURL(r.KernelDomain),
		}
	}
	jobs = append(jobs, *makeRealmJob(tenant, realmName, r.KernelDomain, broker))

	oidcConfigs, err := r.collectOIDCAppConfigs(ctx, tenant)
	if err != nil {
		return nil, err
	}

	groupNames := collectGentianTenantGroupNames(tenant, oidcConfigs)
	jobs = append(jobs, *makeGentianGroupsJob(tenant, realmName, groupNames))

	adminCreds, err := r.seedTenantAdminCreds(ctx, tenant)
	if err != nil {
		return nil, err
	}
	jobs = append(jobs, *makeAdminJob(tenant, realmName, adminCreds))

	if len(oidcConfigs) > 0 {
		jobs = append(jobs, *makeOIDCBrowserFlowJob(tenant, realmName))
		jobs = append(jobs, *makeBrokerFirstLoginFlowJob(tenant, realmName))
	}

	for _, cfg := range oidcConfigs {
		profile, err := r.getOIDCOwnerProfile(ctx, cfg)
		if err != nil {
			return nil, err
		}
		if crossplaneOwnsOIDCClient(profile, cfg) {
			if err := r.seedOIDCSecrets(ctx, tenant, realmName, cfg); err != nil {
				return nil, err
			}
			continue
		}
		job, err := r.buildOIDCClientProvisioningJob(ctx, tenant, realmName, cfg)
		if err != nil {
			return nil, err
		}
		jobs = append(jobs, *job)
	}

	if r.KernelRealm != "" && r.KernelDomain != "" {
		externalURL := kernelExternalURL(r.KernelDomain)
		jobs = append(jobs, *makeBrokerIdentityProviderJob(tenant.Name, realmName, r.KernelRealm, externalURL))
		jobs = append(jobs, *makeKernelTenantBrokerJob(tenant.Name, realmName, r.KernelRealm, externalURL))
		jobs = append(jobs, *makePortalBFFClientJob(tenant.Name, realmName))
		portalOrigin := fmt.Sprintf("https://%s", kernelPortalHost(r.KernelDomain))
		jobs = append(jobs, *makePortalPublicClientJob(tenant.Name, realmName, portalOrigin))
		if r.clusterKeycloakSMTPCredentialsAvailable(ctx) {
			jobs = append(jobs, *makeTenantSMTPJob(tenant.Name, realmName))
		}
	}

	return jobs, nil
}

func (r *TenantReconciler) seedTenantAdminCreds(ctx context.Context, tenant *gentianov1alpha1.Tenant) (secrets.TenantAdminCreds, error) {
	if r.Seeder != nil {
		return r.Seeder.SeedTenantAdmin(ctx, tenant.Name)
	}
	return secrets.TenantAdminCreds{Username: "admin-" + tenant.Name, Password: "placeholder"}, nil
}

// seedOIDCSecrets writes issuer/client-id/client-secret to OpenBao for apps whose
// Keycloak Client MR is owned by Crossplane (operator OIDC Jobs are skipped).
func (r *TenantReconciler) seedOIDCSecrets(ctx context.Context, tenant *gentianov1alpha1.Tenant, realmName string, cfg oidcAppConfig) error {
	if r.Seeder == nil || cfg.clientID == "" {
		return nil
	}
	issuerHost := tenant.Spec.Domain
	if r.KernelDomain != "" {
		issuerHost = r.KernelDomain
	}
	issuer := fmt.Sprintf("https://id.%s/realms/%s", issuerHost, realmName)
	if _, err := r.Seeder.SeedOIDC(ctx, tenant.Name, cfg.profileName, issuer, cfg.clientID); err != nil {
		return fmt.Errorf("seed oidc for %s: %w", cfg.profileName, err)
	}
	return nil
}

func (r *TenantReconciler) buildOIDCClientProvisioningJob(ctx context.Context, tenant *gentianov1alpha1.Tenant, realmName string, cfg oidcAppConfig) (*batchv1.Job, error) {
	if cfg.pack != nil {
		clientSecret := ""
		if r.Seeder != nil && !cfg.pack.PublicClient {
			issuerHost := tenant.Spec.Domain
			if r.KernelDomain != "" {
				issuerHost = r.KernelDomain
			}
			issuer := fmt.Sprintf("https://id.%s/realms/%s", issuerHost, realmName)
			creds, seedErr := r.Seeder.SeedOIDC(ctx, tenant.Name, cfg.profileName, issuer, cfg.clientID)
			if seedErr != nil {
				return nil, fmt.Errorf("seed oidc pack for %s: %w", cfg.profileName, seedErr)
			}
			clientSecret = creds.ClientSecret
		}
		return makeOIDCPackJob(tenant, realmName, cfg, clientSecret, gentianTenantAppGroup(tenant.Name, cfg.profileName)), nil
	}

	clientSecret := ""
	if r.Seeder != nil {
		issuerHost := tenant.Spec.Domain
		if r.KernelDomain != "" {
			issuerHost = r.KernelDomain
		}
		issuer := fmt.Sprintf("https://id.%s/realms/%s", issuerHost, realmName)
		creds, seedErr := r.Seeder.SeedOIDC(ctx, tenant.Name, cfg.profileName, issuer, cfg.clientID)
		if seedErr != nil {
			return nil, fmt.Errorf("seed oidc for %s: %w", cfg.profileName, seedErr)
		}
		clientSecret = creds.ClientSecret
	}
	return makeClientJob(tenant, realmName, cfg.profileName, cfg.clientID, cfg.redirectURIs, clientSecret), nil
}

// crossplaneOwnsOIDCClient reports whether the app Composition already emits a
// provider-keycloak Client MR, so the operator pack/client Job can be skipped.
// Sidecar OIDC (element-jitsi) is owned by the parent profile's composition.
func crossplaneOwnsOIDCClient(profile *gentianov1alpha1.AppProfile, cfg oidcAppConfig) bool {
	if cfg.pack != nil {
		return false
	}
	if profile.Spec.DeploymentMethod != "" && profile.Spec.DeploymentMethod != gentianov1alpha1.DeploymentMethodCrossplane {
		return false
	}
	if cfg.parentProfile != "" {
		return profile.Spec.CompositionRef != ""
	}
	kr := profile.Spec.KernelRequirements
	if kr == nil || kr.Identity == nil || kr.Identity.OIDC == nil {
		return false
	}
	return true
}
