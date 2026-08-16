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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/catalogue"
	"github.com/gentian-org/gentian-os/internal/kernel/secrets"
	"github.com/gentian-org/gentian-os/internal/keycloak"
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
	stampScriptHashes(jobs)
	// A Job's pod template is immutable, so re-applying a changed script does
	// nothing to a Job that already ran: the old script stays the last thing
	// that touched the cluster, silently. Retire the finished ones whose script
	// has changed and let Crossplane recreate them from the manifests below.
	r.retireStaleProvisioningJobs(ctx, jobs)
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

	samlConfigs, err := r.collectSAMLAppConfigs(ctx, tenant)
	if err != nil {
		return nil, err
	}

	groupsJSON, err := r.collectGentianGroupsJSON(ctx, tenant, oidcConfigs)
	if err != nil {
		return nil, err
	}
	jobs = append(jobs, *makeGentianGroupsJob(tenant, realmName, groupsJSON))

	adminCreds, err := r.seedTenantAdminCreds(ctx, tenant)
	if err != nil {
		return nil, err
	}
	jobs = append(jobs, *makeAdminJob(tenant, realmName, r.tenantAdminEmail(tenant), adminCreds))

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

	for _, cfg := range samlConfigs {
		jobs = append(jobs, *makeSAMLClientJob(tenant, realmName, cfg.profileName, cfg.entityID, cfg.acsURL))
	}

	if r.KernelRealm != "" && r.KernelDomain != "" {
		externalURL := kernelExternalURL(r.KernelDomain)
		jobs = append(jobs, *makeBrokerIdentityProviderJob(tenant.Name, realmName, r.KernelRealm, externalURL))
		jobs = append(jobs, *makeKernelTenantBrokerJob(tenant.Name, realmName, r.KernelRealm, externalURL))
		portalOrigin := fmt.Sprintf("https://%s", kernelPortalHost(r.KernelDomain))
		jobs = append(jobs, *makePortalBFFClientJob(tenant.Name, realmName, portalOrigin))
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
	issuer := fmt.Sprintf("https://id.%s/auth/realms/%s", issuerHost, realmName)
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
			issuer := fmt.Sprintf("https://id.%s/auth/realms/%s", issuerHost, realmName)
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
		issuer := fmt.Sprintf("https://id.%s/auth/realms/%s", issuerHost, realmName)
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
// Sidecars: when the parent profile has compositionRef, Crossplane owns Client MRs
// for sidecar OIDC clients too; the operator still reconciles OIDC pack Jobs when
// a pack catalog is configured (see collectOIDCAppConfigs).
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

func (r *TenantReconciler) collectGentianGroupsJSON(ctx context.Context, tenant *gentianov1alpha1.Tenant, oidcConfigs []oidcAppConfig) (string, error) {
	type groupSpec struct {
		Name       string              `json:"name"`
		Attributes map[string][]string `json:"attributes"`
	}

	var specs []groupSpec
	seen := make(map[string]bool)

	addGroup := func(name string, attrs map[string][]string) {
		if seen[name] {
			return
		}
		seen[name] = true
		if attrs == nil {
			attrs = make(map[string][]string)
		}
		specs = append(specs, groupSpec{
			Name:       name,
			Attributes: attrs,
		})
	}

	// 1. Add tenant default groups
	addGroup(keycloak.TenantMembersGroup(tenant.Name), nil)
	addGroup(keycloak.TenantAdminsGroup(tenant.Name), nil)
	addGroup(keycloak.TenantAppAdminsGroup(tenant.Name), nil)

	// Helper to extract attributes from AppProfile name
	resolveProfileAttrs := func(profileName string) (map[string][]string, error) {
		profile := &gentianov1alpha1.AppProfile{}
		err := r.Get(ctx, types.NamespacedName{Name: profileName}, profile)
		if err != nil {
			if errors.IsNotFound(err) {
				return nil, nil // If profile is not found (e.g. in tests), skip attributes
			}
			return nil, err
		}
		var attrs map[string][]string
		if profile.Annotations != nil {
			if val, ok := profile.Annotations["gentianos.io/keycloak-group-attributes"]; ok {
				var parsed map[string]any
				if err := json.Unmarshal([]byte(val), &parsed); err != nil {
					return nil, fmt.Errorf("failed to unmarshal keycloak-group-attributes for %s: %w", profileName, err)
				}
				attrs = make(map[string][]string)
				for k, v := range parsed {
					if list, ok := v.([]any); ok {
						var strList []string
						for _, item := range list {
							strList = append(strList, fmt.Sprint(item))
						}
						attrs[k] = strList
					} else if valStr, ok := v.(string); ok {
						attrs[k] = []string{valStr}
					} else {
						attrs[k] = []string{fmt.Sprint(v)}
					}
				}
			}
		}
		return attrs, nil
	}

	// 2. Add groups for all tenant apps
	for _, app := range tenant.Spec.Apps {
		profileName, err := catalogue.ResolveTenantAppProfile(ctx, r.Client, app)
		if err != nil {
			return "", err
		}
		attrs, err := resolveProfileAttrs(profileName)
		if err != nil {
			return "", err
		}
		addGroup(keycloak.TenantAppGroup(tenant.Name, profileName), attrs)
	}

	// 3. Add groups for any additional profiles (OIDC configs)
	for _, cfg := range oidcConfigs {
		attrs, err := resolveProfileAttrs(cfg.profileName)
		if err != nil {
			return "", err
		}
		addGroup(keycloak.TenantAppGroup(tenant.Name, cfg.profileName), attrs)
	}

	data, err := json.Marshal(specs)
	if err != nil {
		return "", fmt.Errorf("failed to marshal groups JSON: %w", err)
	}
	return string(data), nil
}

// provisioningScriptHashAnnotation records which script a provisioning Job was
// built from, so a finished Job can be told apart from one whose desired script
// has since changed.
const provisioningScriptHashAnnotation = "gentianos.io/script-hash"

// scriptHash digests what a Job actually executes. Only the image and the
// command line are hashed — env values (a freshly generated ROLE_PW among them)
// change on every reconcile and would make every Job look permanently stale.
func scriptHash(job *batchv1.Job) string {
	h := sha256.New()
	for _, c := range job.Spec.Template.Spec.Containers {
		h.Write([]byte(c.Image))
		for _, part := range append(append([]string{}, c.Command...), c.Args...) {
			h.Write([]byte(part))
			h.Write([]byte{0})
		}
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

func stampScriptHashes(jobs []batchv1.Job) {
	for i := range jobs {
		if jobs[i].Annotations == nil {
			jobs[i].Annotations = map[string]string{}
		}
		jobs[i].Annotations[provisioningScriptHashAnnotation] = scriptHash(&jobs[i])
	}
}

// retireStaleProvisioningJobs deletes finished provisioning Jobs whose script no
// longer matches the desired one, so they are re-created and re-run.
//
// Only finished Jobs are touched: a running one is either about to converge
// anyway or is being watched by a reconcile that expects it to exist. Jobs
// predating this annotation are treated as stale, which re-runs them once —
// safe, because every provisioning script here is written to be re-runnable.
func (r *TenantReconciler) retireStaleProvisioningJobs(ctx context.Context, jobs []batchv1.Job) {
	for i := range jobs {
		desired := jobs[i].Annotations[provisioningScriptHashAnnotation]
		live := &batchv1.Job{}
		if err := r.Get(ctx, types.NamespacedName{
			Name: jobs[i].Name, Namespace: kernelNamespace,
		}, live); err != nil {
			continue
		}
		if live.Annotations[provisioningScriptHashAnnotation] == desired {
			continue
		}
		if live.Status.Succeeded == 0 && live.Status.Failed == 0 {
			continue
		}
		prop := metav1.DeletePropagationBackground
		_ = r.Delete(ctx, live, &client.DeleteOptions{PropagationPolicy: &prop})
	}
}
