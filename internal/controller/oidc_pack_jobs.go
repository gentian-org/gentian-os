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

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/meta"
	"github.com/gentian-org/gentian-os/internal/oidc"
)

// oidcAppConfig holds resolved OIDC settings for one tenant app profile.
type oidcAppConfig struct {
	profileName   string
	parentProfile string // set for sidecars (e.g. element-jitsi → parent element)
	clientID      string
	redirectURIs  []string
	pack          *oidc.Pack
	templates     map[string]oidc.MapperTemplate
}

// oidcPacksNeedEntitlementGroups reports whether any OIDC pack maps a group to a client role.
func oidcPacksNeedEntitlementGroups(configs []oidcAppConfig) bool {
	for _, cfg := range configs {
		if cfg.pack != nil && cfg.pack.EntitlementGroup != "" {
			return true
		}
	}
	return false
}

func (r *TenantReconciler) collectOIDCAppConfigs(ctx context.Context, tenant *gentianov1alpha1.Tenant) ([]oidcAppConfig, error) {
	var configs []oidcAppConfig
	seen := make(map[string]struct{})
	for _, app := range tenant.Spec.Apps {
		profile := &gentianov1alpha1.AppProfile{}
		if err := r.Get(ctx, types.NamespacedName{Name: app.Profile}, profile); err != nil {
			if errors.IsNotFound(err) {
				continue
			}
			return nil, fmt.Errorf("get AppProfile %s: %w", app.Profile, err)
		}
		if profile.Spec.KernelRequirements != nil &&
			profile.Spec.KernelRequirements.Identity != nil &&
			profile.Spec.KernelRequirements.Identity.OIDC != nil {
			cfg, err := r.resolveOIDCAppConfig(ctx, tenant, app.Profile)
			if err != nil {
				return nil, err
			}
			configs = append(configs, cfg)
			seen[app.Profile] = struct{}{}
		}
		for _, sidecar := range profile.Spec.Sidecars {
			if sidecar.KernelRequirements == nil ||
				sidecar.KernelRequirements.Identity == nil ||
				sidecar.KernelRequirements.Identity.OIDC == nil {
				continue
			}
			scKey := gentianov1alpha1.SidecarAppName(app.Profile, sidecar.Name)
			if _, ok := seen[scKey]; ok {
				continue
			}
			cfg, err := r.resolveSidecarOIDCAppConfig(ctx, tenant, app.Profile, sidecar)
			if err != nil {
				return nil, err
			}
			configs = append(configs, cfg)
			seen[scKey] = struct{}{}
		}
	}
	return configs, nil
}

// cleanupOrphanedClientJobs deletes Keycloak OIDC/SAML client provisioning Jobs for
// apps no longer listed in tenant.Spec.Apps. Unlike App claims, client Jobs are not
// removed automatically on app uninstall.
func (r *TenantReconciler) cleanupOrphanedClientJobs(ctx context.Context, tenant *gentianov1alpha1.Tenant, oidcConfigs []oidcAppConfig, samlConfigs []samlAppConfig) error {
	desiredApps := make(map[string]struct{}, len(oidcConfigs)+len(samlConfigs))
	for _, cfg := range oidcConfigs {
		desiredApps[cfg.profileName] = struct{}{}
	}
	for _, cfg := range samlConfigs {
		desiredApps[cfg.profileName] = struct{}{}
	}

	prefix := clientJobName(tenant.Name, "")
	jobList := &batchv1.JobList{}
	if err := r.List(ctx, jobList, client.InNamespace(kernelNamespace), tenantKernelLabelSelector(tenant.Name)); err != nil {
		return fmt.Errorf("list OIDC/SAML client Jobs for tenant %s: %w", tenant.Name, err)
	}
	for i := range jobList.Items {
		job := &jobList.Items[i]
		if !strings.HasPrefix(job.Name, prefix) {
			continue
		}
		appName := job.Labels[appLabel]
		if appName == "" {
			appName = strings.TrimPrefix(job.Name, prefix)
		}
		if _, wanted := desiredApps[appName]; wanted {
			continue
		}
		prop := metav1.DeletePropagationBackground
		if err := r.Delete(ctx, job, &client.DeleteOptions{PropagationPolicy: &prop}); client.IgnoreNotFound(err) != nil {
			return fmt.Errorf("delete orphaned OIDC/SAML client Job %s: %w", job.Name, err)
		}
	}
	return nil
}

func (r *TenantReconciler) resolveOIDCAppConfig(ctx context.Context, tenant *gentianov1alpha1.Tenant, profileName string) (oidcAppConfig, error) {
	profile := &gentianov1alpha1.AppProfile{}
	if err := r.Get(ctx, types.NamespacedName{Name: profileName}, profile); err != nil {
		return oidcAppConfig{}, fmt.Errorf("get AppProfile %s: %w", profileName, err)
	}
	oidcSpec := profile.Spec.KernelRequirements.Identity.OIDC
	clientID := oidcSpec.ClientID
	if clientID == "" {
		clientID = oidcClientID(tenant.Name, profileName)
	}
	redirects, err := r.resolveOIDCRedirectURIs(ctx, tenant, profile, profileName, oidcSpec.RedirectURIs)
	if err != nil {
		return oidcAppConfig{}, err
	}

	cfg := oidcAppConfig{
		profileName:  profileName,
		clientID:     clientID,
		redirectURIs: redirects,
	}
	packKey := clientID
	if oidcSpec.OIDCPackRef != "" {
		packKey = oidcSpec.OIDCPackRef
	}
	if pack, templates, ok, err := oidc.ResolvePack(ctx, r.Client, packKey); err != nil {
		return oidcAppConfig{}, err
	} else if ok {
		cfg.pack = &pack
		cfg.templates = templates
	}
	return cfg, nil
}

func (r *TenantReconciler) resolveSidecarOIDCAppConfig(ctx context.Context, tenant *gentianov1alpha1.Tenant, parentProfile string, sidecar gentianov1alpha1.AppSidecarSpec) (oidcAppConfig, error) {
	owner := &gentianov1alpha1.AppProfile{}
	if err := r.Get(ctx, types.NamespacedName{Name: parentProfile}, owner); err != nil {
		return oidcAppConfig{}, fmt.Errorf("get parent AppProfile %s: %w", parentProfile, err)
	}
	profileName := gentianov1alpha1.SidecarAppName(parentProfile, sidecar.Name)
	oidcSpec := sidecar.KernelRequirements.Identity.OIDC
	clientID := oidcSpec.ClientID
	if clientID == "" {
		clientID = oidcClientID(tenant.Name, profileName)
	}
	redirects, err := r.resolveOIDCRedirectURIs(ctx, tenant, owner, profileName, oidcSpec.RedirectURIs)
	if err != nil {
		return oidcAppConfig{}, err
	}

	packKey := clientID
	if oidcSpec.OIDCPackRef != "" {
		packKey = oidcSpec.OIDCPackRef
	}

	cfg := oidcAppConfig{
		profileName:   profileName,
		parentProfile: parentProfile,
		clientID:      clientID,
		redirectURIs:  redirects,
	}
	if pack, templates, ok, err := oidc.ResolvePack(ctx, r.Client, packKey); err != nil {
		return oidcAppConfig{}, err
	} else if ok {
		cfg.pack = &pack
		cfg.templates = templates
	}
	return cfg, nil
}

// getOIDCOwnerProfile returns the AppProfile that owns an OIDC config. Sidecar
// configs (element-jitsi) live on the parent profile (element); there is no
// standalone AppProfile for sidecars; sidecar OIDC configs live on the parent profile.
func (r *TenantReconciler) getOIDCOwnerProfile(ctx context.Context, cfg oidcAppConfig) (*gentianov1alpha1.AppProfile, error) {
	ownerName := cfg.profileName
	if cfg.parentProfile != "" {
		ownerName = cfg.parentProfile
	}
	profile := &gentianov1alpha1.AppProfile{}
	if err := r.Get(ctx, types.NamespacedName{Name: ownerName}, profile); err != nil {
		return nil, fmt.Errorf("get AppProfile %s: %w", ownerName, err)
	}
	return profile, nil
}

func (r *TenantReconciler) resolveOIDCRedirectURIs(
	_ context.Context,
	tenant *gentianov1alpha1.Tenant,
	profile *gentianov1alpha1.AppProfile,
	profileName string,
	uris []string,
) ([]string, error) {
	if len(uris) > 0 {
		return substituteTenantDomainInURIs(tenant, uris, r.KernelDomain, r.TenancyMode), nil
	}
	if profile != nil {
		defaults, err := gentianov1alpha1.ProfileOIDCDefaultRedirectURIs(profile)
		if err != nil {
			return nil, fmt.Errorf("parse %s on AppProfile %s: %w", gentianov1alpha1.AnnotationProfileOIDCDefaultRedirectURIs, profileName, err)
		}
		if len(defaults) > 0 {
			return substituteTenantDomainInURIs(tenant, defaults, r.KernelDomain, r.TenancyMode), nil
		}
	}
	// Sidecar OIDC redirect URIs must be declared on the AppProfile or in the pack spec.
	return nil, fmt.Errorf("AppProfile %s: no OIDC redirect URIs in pack spec and no %s annotation",
		profileName, gentianov1alpha1.AnnotationProfileOIDCDefaultRedirectURIs)
}

func substituteTenantDomainInURIs(tenant *gentianov1alpha1.Tenant, uris []string, kernelDomain, tenancyMode string) []string {
	host := tenant.EffectiveDomain(kernelDomain, tenancyMode)
	if host == "" {
		host = tenant.Spec.Domain
	}
	out := make([]string, 0, len(uris))
	for _, u := range uris {
		out = append(out, strings.ReplaceAll(u, "${TENANT_DOMAIN}", host))
	}
	return out
}

func (r *TenantReconciler) ensureBrokerFirstLoginFlowJob(ctx context.Context, tenant *gentianov1alpha1.Tenant, realmName string) (bool, error) {
	jobName := brokerFirstLoginFlowJobName(tenant.Name)
	job := &batchv1.Job{}
	err := r.Get(ctx, types.NamespacedName{Name: jobName, Namespace: kernelNamespace}, job)
	if err == nil && !brokerFirstLoginFlowJobCurrent(job) {
		prop := metav1.DeletePropagationBackground
		if delErr := r.Delete(ctx, job, &client.DeleteOptions{PropagationPolicy: &prop}); delErr != nil && !errors.IsNotFound(delErr) {
			return false, fmt.Errorf("delete stale broker first-login flow job %s: %w", jobName, delErr)
		}
	}
	return r.waitForProvisioningJob(ctx, tenant.Name, jobName)
}

func brokerFirstLoginFlowJobCurrent(job *batchv1.Job) bool {
	return job != nil && job.Labels["gentianos.io/keycloak-broker-first-login"] == brokerFirstLoginFlowJobVersion
}

func makeBrokerFirstLoginFlowJob(tenant *gentianov1alpha1.Tenant, realmName string) *batchv1.Job {
	ttl := meta.ProvisioningJobTTLSeconds
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      brokerFirstLoginFlowJobName(tenant.Name),
			Namespace: kernelNamespace,
			Labels: map[string]string{
				tenantLabel:    tenant.Name,
				managedByLabel: managedByValue,
				"gentianos.io/keycloak-broker-first-login": brokerFirstLoginFlowJobVersion,
			},
		},
		Spec: batchv1.JobSpec{
			TTLSecondsAfterFinished: &ttl,
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyOnFailure,
					Containers: []corev1.Container{
						keycloakContainer("provision-broker-first-login-flow", buildFirstBrokerLoginFlowScript(realmName)),
					},
				},
			},
		},
	}
}

func brokerFirstLoginFlowJobName(tenantName string) string {
	return fmt.Sprintf("keycloak-broker-first-login-%s", tenantName)
}

func (r *TenantReconciler) ensureOIDCBrowserFlowJob(ctx context.Context, tenant *gentianov1alpha1.Tenant, realmName string) (bool, error) {
	return r.waitForProvisioningJob(ctx, tenant.Name, oidcBrowserFlowJobName(tenant.Name))
}

func (r *TenantReconciler) ensureOIDCClientJob(ctx context.Context, tenant *gentianov1alpha1.Tenant, realmName string, cfg oidcAppConfig) (bool, error) {
	if cfg.pack != nil {
		return r.ensureOIDCPackJob(ctx, tenant, realmName, cfg)
	}
	return r.ensureClientJob(ctx, tenant, realmName, cfg.profileName, cfg.clientID, cfg.redirectURIs)
}

func (r *TenantReconciler) ensureOIDCPackJob(ctx context.Context, tenant *gentianov1alpha1.Tenant, realmName string, cfg oidcAppConfig) (bool, error) {
	return r.waitForProvisioningJob(ctx, tenant.Name, clientJobName(tenant.Name, cfg.profileName))
}

func makeOIDCPackJob(tenant *gentianov1alpha1.Tenant, realmName string, cfg oidcAppConfig, clientSecret, entitlementGroup string) *batchv1.Job {
	ttl := meta.ProvisioningJobTTLSeconds
	container := keycloakContainer("provision-oidc-pack",
		buildOIDCPackScript(realmName, cfg.clientID, *cfg.pack, cfg.templates, cfg.redirectURIs, clientSecret, entitlementGroup))
	if clientSecret != "" {
		container.Env = append(container.Env, corev1.EnvVar{
			Name:  "OIDC_CLIENT_SECRET",
			Value: clientSecret,
		})
	}
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      clientJobName(tenant.Name, cfg.profileName),
			Namespace: kernelNamespace,
			Labels: map[string]string{
				tenantLabel:    tenant.Name,
				managedByLabel: managedByValue,
				appLabel:       cfg.profileName,
			},
		},
		Spec: batchv1.JobSpec{
			TTLSecondsAfterFinished: &ttl,
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyOnFailure,
					Containers:    []corev1.Container{container},
				},
			},
		},
	}
}

func makeOIDCBrowserFlowJob(tenant *gentianov1alpha1.Tenant, realmName string) *batchv1.Job {
	ttl := meta.ProvisioningJobTTLSeconds
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      oidcBrowserFlowJobName(tenant.Name),
			Namespace: kernelNamespace,
			Labels: map[string]string{
				tenantLabel:    tenant.Name,
				managedByLabel: managedByValue,
			},
		},
		Spec: batchv1.JobSpec{
			TTLSecondsAfterFinished: &ttl,
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyOnFailure,
					Containers: []corev1.Container{
						keycloakContainer("provision-oidc-browser-flow", buildOIDCBrowserFlowScript(realmName)),
					},
				},
			},
		},
	}
}

func oidcBrowserFlowJobName(tenantName string) string {
	return fmt.Sprintf("keycloak-oidc-browser-%s", tenantName)
}

// samlAppConfig holds resolved SAML settings for one tenant app profile.
type samlAppConfig struct {
	profileName string
	entityID    string
	acsURL      string
}

func (r *TenantReconciler) collectSAMLAppConfigs(ctx context.Context, tenant *gentianov1alpha1.Tenant) ([]samlAppConfig, error) {
	var configs []samlAppConfig
	for _, app := range tenant.Spec.Apps {
		profile := &gentianov1alpha1.AppProfile{}
		if err := r.Get(ctx, types.NamespacedName{Name: app.Profile}, profile); err != nil {
			if errors.IsNotFound(err) {
				continue
			}
			return nil, fmt.Errorf("get AppProfile %s: %w", app.Profile, err)
		}
		if profile.Spec.KernelRequirements != nil &&
			profile.Spec.KernelRequirements.Identity != nil &&
			profile.Spec.KernelRequirements.Identity.SAML != nil {
			samlSpec := profile.Spec.KernelRequirements.Identity.SAML
			host := tenant.EffectiveDomain(r.KernelDomain, r.TenancyMode)
			if host == "" {
				host = tenant.Spec.Domain
			}
			acsURL := strings.ReplaceAll(samlSpec.ACSURL, "${TENANT_DOMAIN}", host)

			configs = append(configs, samlAppConfig{
				profileName: app.Profile,
				entityID:    samlSpec.EntityID,
				acsURL:      acsURL,
			})
		}
	}
	return configs, nil
}
