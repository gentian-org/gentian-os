// Copyright 2026 The Gentian Authors. Licensed under Apache 2.0.

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
	"github.com/gentian-org/gentian-os/internal/oidc"
)

// oidcAppConfig holds resolved OIDC settings for one tenant app profile.
type oidcAppConfig struct {
	profileName  string
	clientID     string
	redirectURIs []string
	pack         *oidc.Pack
	templates    map[string]oidc.MapperTemplate
}

// oidcPacksNeedLDAPGroups reports whether any resolved OIDC config uses an
// OpenDesk pack that maps a managed-by-attribute-* LDAP group to a client role.
func oidcPacksNeedLDAPGroups(configs []oidcAppConfig) bool {
	for _, cfg := range configs {
		if cfg.pack != nil && cfg.pack.LDAPGroup != "" {
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

// cleanupOrphanedOIDCClientJobs deletes Keycloak OIDC client provisioning Jobs for
// apps no longer listed in tenant.Spec.Apps. Unlike App claims, client Jobs are not
// removed automatically on app uninstall.
func (r *TenantReconciler) cleanupOrphanedOIDCClientJobs(ctx context.Context, tenant *gentianov1alpha1.Tenant, desired []oidcAppConfig) error {
	desiredApps := make(map[string]struct{}, len(desired))
	for _, cfg := range desired {
		desiredApps[cfg.profileName] = struct{}{}
	}

	prefix := clientJobName(tenant.Name, "")
	jobList := &batchv1.JobList{}
	if err := r.List(ctx, jobList, client.InNamespace(kernelNamespace), tenantKernelLabelSelector(tenant.Name)); err != nil {
		return fmt.Errorf("list OIDC client Jobs for tenant %s: %w", tenant.Name, err)
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
			return fmt.Errorf("delete orphaned OIDC client Job %s: %w", job.Name, err)
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
	redirects := resolveOIDCRedirectURIs(tenant, profileName, oidcSpec.RedirectURIs, r.KernelDomain, r.TenancyMode)

	cfg := oidcAppConfig{
		profileName:  profileName,
		clientID:     clientID,
		redirectURIs: redirects,
	}
	if pack, templates, ok, err := oidc.PackForClient(clientID); err != nil {
		return oidcAppConfig{}, err
	} else if ok {
		cfg.pack = &pack
		cfg.templates = templates
	}
	return cfg, nil
}

func (r *TenantReconciler) resolveSidecarOIDCAppConfig(_ context.Context, tenant *gentianov1alpha1.Tenant, parentProfile string, sidecar gentianov1alpha1.AppSidecarSpec) (oidcAppConfig, error) {
	profileName := gentianov1alpha1.SidecarAppName(parentProfile, sidecar.Name)
	oidcSpec := sidecar.KernelRequirements.Identity.OIDC
	clientID := oidcSpec.ClientID
	if clientID == "" {
		clientID = oidcClientID(tenant.Name, profileName)
	}
	redirects := resolveOIDCRedirectURIs(tenant, profileName, oidcSpec.RedirectURIs, r.KernelDomain, r.TenancyMode)

	cfg := oidcAppConfig{
		profileName:  profileName,
		clientID:     clientID,
		redirectURIs: redirects,
	}
	if pack, templates, ok, err := oidc.PackForClient(clientID); err != nil {
		return oidcAppConfig{}, err
	} else if ok {
		cfg.pack = &pack
		cfg.templates = templates
	}
	return cfg, nil
}

func resolveOIDCRedirectURIs(tenant *gentianov1alpha1.Tenant, profileName string, uris []string, kernelDomain, tenancyMode string) []string {
	if len(uris) > 0 {
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
	// Legacy fallback when AppProfile omits redirectUris.
	host := tenant.EffectiveDomain(kernelDomain, tenancyMode)
	if host == "" {
		host = tenant.Spec.Domain
	}
	if profileName == "element" {
		return []string{fmt.Sprintf("https://matrix.%s/_synapse/client/oidc/callback", host)}
	}
	return []string{fmt.Sprintf("https://%s/%s/*", host, profileName)}
}

func (r *TenantReconciler) ensureOIDCBrowserFlowJob(ctx context.Context, tenant *gentianov1alpha1.Tenant, realmName string) (bool, error) {
	jobName := oidcBrowserFlowJobName(tenant.Name)
	job := &batchv1.Job{}
	err := r.Get(ctx, types.NamespacedName{Name: jobName, Namespace: kernelNamespace}, job)
	if errors.IsNotFound(err) {
		return false, r.Create(ctx, makeOIDCBrowserFlowJob(tenant, realmName))
	}
	if err != nil {
		return false, err
	}
	if jobIsFailed(job) {
		prop := metav1.DeletePropagationBackground
		_ = r.Delete(ctx, job, &client.DeleteOptions{PropagationPolicy: &prop})
		return false, nil
	}
	return jobIsComplete(job), nil
}

func (r *TenantReconciler) ensureOIDCClientJob(ctx context.Context, tenant *gentianov1alpha1.Tenant, realmName string, cfg oidcAppConfig) (bool, error) {
	if cfg.pack != nil {
		return r.ensureOIDCPackJob(ctx, tenant, realmName, cfg)
	}
	return r.ensureClientJob(ctx, tenant, realmName, cfg.profileName, cfg.clientID, cfg.redirectURIs)
}

func (r *TenantReconciler) ensureOIDCPackJob(ctx context.Context, tenant *gentianov1alpha1.Tenant, realmName string, cfg oidcAppConfig) (bool, error) {
	jobName := clientJobName(tenant.Name, cfg.profileName)
	job := &batchv1.Job{}
	err := r.Get(ctx, types.NamespacedName{Name: jobName, Namespace: kernelNamespace}, job)
	if errors.IsNotFound(err) {
		clientSecret := ""
		if r.Seeder != nil && !cfg.pack.PublicClient {
			issuerHost := tenant.Spec.Domain
			if r.KernelDomain != "" {
				issuerHost = r.KernelDomain
			}
			issuer := fmt.Sprintf("https://id.%s/realms/%s", issuerHost, realmName)
			creds, seedErr := r.Seeder.SeedOIDC(ctx, tenant.Name, cfg.profileName, issuer, cfg.clientID)
			if seedErr != nil {
				return false, fmt.Errorf("seed oidc: %w", seedErr)
			}
			clientSecret = creds.ClientSecret
		}
		return false, r.Create(ctx, makeOIDCPackJob(tenant, realmName, cfg, clientSecret))
	}
	if err != nil {
		return false, err
	}
	if jobIsFailed(job) {
		prop := metav1.DeletePropagationBackground
		_ = r.Delete(ctx, job, &client.DeleteOptions{PropagationPolicy: &prop})
		return false, nil
	}
	return jobIsComplete(job), nil
}

func makeOIDCPackJob(tenant *gentianov1alpha1.Tenant, realmName string, cfg oidcAppConfig, clientSecret string) *batchv1.Job {
	ttl := int32(3600)
	container := keycloakContainer("provision-oidc-pack",
		buildOIDCPackScript(realmName, cfg.clientID, *cfg.pack, cfg.templates, cfg.redirectURIs, clientSecret))
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
	ttl := int32(3600)
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
