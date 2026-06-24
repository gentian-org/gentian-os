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
	"github.com/gentian-org/gentian-os/internal/oidc"
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

	if r.LDAPBase != "" {
		ldapJobs, err := r.buildLDAPProvisioningJobs(ctx, tenant)
		if err != nil {
			return nil, err
		}
		jobs = append(jobs, ldapJobs...)
	}

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

func (r *TenantReconciler) buildLDAPProvisioningJobs(ctx context.Context, tenant *gentianov1alpha1.Tenant) ([]batchv1.Job, error) {
	ouDN := tenantOUDN(tenant)
	mbaGroups, err := oidc.ManagedByAttributeGroupNames(ctx, r.Client)
	if err != nil {
		return nil, fmt.Errorf("resolve managed-by-attribute groups: %w", err)
	}
	if len(mbaGroups) == 0 {
		return nil, fmt.Errorf("no managed-by-attribute groups found in OIDCPackCatalog")
	}

	var jobs []batchv1.Job

	jobs = append(jobs, *makeOUJob(tenant, ouDN, mbaGroups))
	jobs = append(jobs, *makeMBAGroupsJob(tenant, ouDN, mbaGroups))

	mailDomain := tenantUserMailDomain(tenant, r.KernelDomain, r.TenancyMode)
	if mailDomain != "" {
		jobs = append(jobs, *makeAppUserTemplateJob(tenant, ouDN, mailDomain))
	}
	jobs = append(jobs, *makeAppUserCapabilitiesJob(tenant, ouDN))

	adminCreds, err := r.seedTenantAdminCreds(ctx, tenant)
	if err != nil {
		return nil, err
	}
	jobs = append(jobs, *makeAdminUserJob(tenant, ouDN, adminCreds))
	jobs = append(jobs, *makeAdminPolicyJob(tenant, ouDN))

	ldapApps, err := r.collectLDAPApps(ctx, tenant)
	if err != nil {
		return nil, err
	}
	ldapApps = append(ldapApps, "keycloak")
	ldapApps = dedupeStrings(ldapApps)

	for _, appName := range ldapApps {
		bindPassword := ""
		if r.Seeder != nil {
			creds, seedErr := r.Seeder.SeedLDAP(ctx, tenant.Name, appName, secrets.LDAPCreds{
				BindDN: fmt.Sprintf("uid=app-%s-%s,%s", appName, tenant.Name, ouDN),
				BaseDN: ouDN,
			})
			if seedErr != nil {
				return nil, fmt.Errorf("seed ldap bind for %s: %w", appName, seedErr)
			}
			bindPassword = creds.BindPassword
		}
		jobs = append(jobs, *makeBindAccountJob(tenant, ouDN, appName, bindPassword))
	}

	portalApps, err := r.collectDedicatedPortalApps(ctx, tenant)
	if err != nil {
		return nil, fmt.Errorf("collect dedicated portal apps: %w", err)
	}
	tenantDomain := r.tenantEffectiveDomain(tenant)
	for _, pa := range portalApps {
		jobs = append(jobs, *makePortalEntryJob(tenant, ouDN, pa, tenantDomain))
	}

	meetURL, chatURL := r.portalRealtimeLinkTargets(tenant)
	if meetURL != "" || chatURL != "" {
		includeLegacy := gentianov1alpha1.NormalizeTenancyMode(r.TenancyMode) == gentianov1alpha1.TenancyModeSingle
		jobs = append(jobs, *makePortalRealtimeLinksJob(tenant, ouDN, meetURL, chatURL, includeLegacy))
	}

	return jobs, nil
}

func (r *TenantReconciler) buildIdentityProvisioningJobs(ctx context.Context, tenant *gentianov1alpha1.Tenant, realmName string) ([]batchv1.Job, error) {
	var jobs []batchv1.Job

	ldap, err := r.buildRealmLDAPParams(ctx, tenant)
	if err != nil {
		return nil, err
	}
	var broker *realmBrokerParams
	if r.KernelRealm != "" && r.KernelDomain != "" {
		broker = &realmBrokerParams{
			kernelRealm:       r.KernelRealm,
			kernelExternalURL: fmt.Sprintf("https://id.%s", r.KernelDomain),
		}
	}
	jobs = append(jobs, *makeRealmJob(tenant, realmName, r.KernelDomain, ldap, broker))

	adminCreds, err := r.seedTenantAdminCreds(ctx, tenant)
	if err != nil {
		return nil, err
	}
	jobs = append(jobs, *makeAdminJob(tenant, realmName, adminCreds))

	oidcConfigs, err := r.collectOIDCAppConfigs(ctx, tenant)
	if err != nil {
		return nil, err
	}

	if len(oidcConfigs) > 0 {
		jobs = append(jobs, *makeOIDCBrowserFlowJob(tenant, realmName))
		jobs = append(jobs, *makeBrokerFirstLoginFlowJob(tenant, realmName))
	}

	if len(oidcConfigs) > 0 && r.LDAPBase != "" && oidcPacksNeedLDAPGroups(oidcConfigs) {
		jobs = append(jobs, *makeKCLDAPGroupSyncJob(tenant, realmName))
	}

	for _, cfg := range oidcConfigs {
		profile, err := r.getOIDCOwnerProfile(ctx, cfg)
		if err != nil {
			return nil, err
		}
		if crossplaneOwnsOIDCClient(profile, cfg) {
			continue
		}
		job, err := r.buildOIDCClientProvisioningJob(ctx, tenant, realmName, cfg)
		if err != nil {
			return nil, err
		}
		jobs = append(jobs, *job)
	}

	if r.KernelRealm != "" {
		adminEmail := tenant.Spec.AdminEmail
		if adminEmail == "" {
			adminEmail = fmt.Sprintf("admin-%s@gentian.org", tenant.Name)
		}
		jobs = append(jobs, *makeOpendeskAdminEnableJob(tenant, adminEmail, r.KernelRealm))
		jobs = append(jobs, *makeKernelLDAPSyncJob(tenant, r.KernelRealm))
		jobs = append(jobs, *makeKCLDAPSyncJob(tenant, realmName))
		jobs = append(jobs, *makeKCLDAPOpenDeskMappersJob(tenant, realmName))
	}

	if r.KernelRealm != "" && r.KernelDomain != "" {
		kernelExternalURL := fmt.Sprintf("https://id.%s", r.KernelDomain)
		jobs = append(jobs, *makeBrokerIdentityProviderJob(tenant.Name, realmName, r.KernelRealm, kernelExternalURL))
	}

	return jobs, nil
}

func (r *TenantReconciler) buildRealmLDAPParams(ctx context.Context, tenant *gentianov1alpha1.Tenant) (*realmLDAPParams, error) {
	if r.LDAPBase == "" || r.LDAPServer == "" || r.Seeder == nil {
		return nil, nil
	}
	ouDN := tenantConcreteOUDN(tenant, r.LDAPBase)
	bindDN := fmt.Sprintf("uid=app-keycloak-%s,%s", tenant.Name, ouDN)
	creds, err := r.Seeder.SeedLDAP(ctx, tenant.Name, "keycloak", secrets.LDAPCreds{
		BindDN: bindDN,
		BaseDN: ouDN,
	})
	if err != nil {
		return nil, fmt.Errorf("seed keycloak ldap: %w", err)
	}
	return &realmLDAPParams{
		server:   r.LDAPServer,
		bindDN:   bindDN,
		bindPW:   creds.BindPassword,
		usersDN:  "ou=users," + ouDN,
		groupsDN: ouDN,
	}, nil
}

func (r *TenantReconciler) seedTenantAdminCreds(ctx context.Context, tenant *gentianov1alpha1.Tenant) (secrets.TenantAdminCreds, error) {
	if r.Seeder != nil {
		return r.Seeder.SeedTenantAdmin(ctx, tenant.Name)
	}
	return secrets.TenantAdminCreds{Username: "admin-" + tenant.Name, Password: "placeholder"}, nil
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
		return makeOIDCPackJob(tenant, realmName, cfg, clientSecret), nil
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

func dedupeStrings(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}
