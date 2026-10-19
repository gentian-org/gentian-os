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
	stderrors "errors"
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

// oidcClientRefusal is why one app's OIDC client cannot be made as its
// profile declares it. It is that app's alone: the app is left out of what is
// provisioned and named on the tenant's IdentityReady condition, and every
// other app of the tenant is provisioned as before. Any other error -- the API
// server not answering -- still ends the reconcile, as it is no app's.
type oidcClientRefusal struct{ msg string }

func (e *oidcClientRefusal) Error() string { return e.msg }

func refuseOIDCClient(format string, args ...any) error {
	return &oidcClientRefusal{msg: fmt.Sprintf(format, args...)}
}

// collectOIDCAppConfigs resolves the OIDC clients of the tenant's apps. refused
// are the apps whose client cannot be made as declared (oidcClientRefusal),
// one message each; they are not among configs.
func (r *TenantReconciler) collectOIDCAppConfigs(ctx context.Context, tenant *gentianov1alpha1.Tenant) (configs []oidcAppConfig, refused []string, err error) {
	seen := make(map[string]struct{})
	add := func(key string, cfg oidcAppConfig, err error) error {
		var refusal *oidcClientRefusal
		if stderrors.As(err, &refusal) {
			refused = append(refused, refusal.msg)
			return nil
		}
		if err != nil {
			return err
		}
		configs = append(configs, cfg)
		seen[key] = struct{}{}
		return nil
	}
	for _, app := range tenant.Spec.Apps {
		profile, err := getProfileFor(ctx, r.Client, app.Profile, app.Digest)
		if err != nil {
			if errors.IsNotFound(err) {
				continue
			}
			return nil, nil, fmt.Errorf("get ComponentProfile %s: %w", app.Profile, err)
		}
		if profile.Services() != nil &&
			profile.Services().Identity != nil &&
			profile.Services().Identity.OIDC != nil {
			cfg, err := r.resolveOIDCAppConfig(ctx, tenant, app.Profile)
			if err := add(app.Profile, cfg, err); err != nil {
				return nil, nil, err
			}
		}
		for _, sidecar := range profile.Spec.Extensions {
			if sidecar.ServiceRequirements == nil ||
				sidecar.ServiceRequirements.Identity == nil ||
				sidecar.ServiceRequirements.Identity.OIDC == nil {
				continue
			}
			scKey := gentianov1alpha1.SidecarAppName(app.Profile, sidecar.Name)
			if _, ok := seen[scKey]; ok {
				continue
			}
			cfg, err := r.resolveSidecarOIDCAppConfig(ctx, tenant, app.Profile, sidecar)
			if err := add(scKey, cfg, err); err != nil {
				return nil, nil, err
			}
		}
	}
	return configs, refused, nil
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
	if err := r.List(ctx, jobList, client.InNamespace(identityNamespace), tenantKernelLabelSelector(tenant.Name)); err != nil {
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
	profile := &gentianov1alpha1.ComponentProfile{}
	if err := r.Get(ctx, types.NamespacedName{Name: profileName}, profile); err != nil {
		return oidcAppConfig{}, fmt.Errorf("get ComponentProfile %s: %w", profileName, err)
	}
	oidcSpec := profile.Services().Identity.OIDC
	clientID := oidcSpec.ClientID
	if clientID == "" {
		clientID = oidcClientID(tenant.Name, profileName)
	}
	// A request-bound client is the app Composition's to compose, with the
	// rule that narrows its https://* (OIDCRequestParameterSpec). A profile
	// that has no Composition has its client made by a Job, which holds no
	// such rule and would register a client the parameter binds nothing on.
	if oidcSpec.RequestParameter != nil && profile.IsAPI() {
		return oidcAppConfig{}, refuseOIDCClient(
			"ComponentProfile %s: requires.services.identity.oidc.requestParameter needs the app Composition, which an API entry does not have", profileName)
	}
	redirects, err := r.resolveOIDCRedirectURIs(ctx, tenant, profile, profileName, oidcSpec.RedirectURIs, oidcSpec.RequestParameter != nil)
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
	if err := r.resolveOIDCPack(ctx, &cfg, profileName, packKey); err != nil {
		return oidcAppConfig{}, err
	}
	return cfg, nil
}

// resolveOIDCPack puts the pack the profile's own catalogue holds under
// packKey on cfg, where the app Composition looks for it (oidc.ResolvePack).
// A profile two catalogues are labelled for is refused: which one the
// Composition reads is not defined, so neither writer can be told the client
// is its own.
func (r *TenantReconciler) resolveOIDCPack(ctx context.Context, cfg *oidcAppConfig, profileName, packKey string) error {
	pack, templates, ok, err := oidc.ResolvePack(ctx, r.Client, profileName, packKey)
	if stderrors.Is(err, oidc.ErrCatalogAmbiguous) {
		return refuseOIDCClient("ComponentProfile %s: %v", profileName, err)
	}
	if err != nil {
		return err
	}
	if ok {
		cfg.pack = &pack
		cfg.templates = templates
	}
	return nil
}

func (r *TenantReconciler) resolveSidecarOIDCAppConfig(ctx context.Context, tenant *gentianov1alpha1.Tenant, parentProfile string, sidecar gentianov1alpha1.AppSidecarSpec) (oidcAppConfig, error) {
	owner := &gentianov1alpha1.ComponentProfile{}
	if err := r.Get(ctx, types.NamespacedName{Name: parentProfile}, owner); err != nil {
		return oidcAppConfig{}, fmt.Errorf("get parent ComponentProfile %s: %w", parentProfile, err)
	}
	profileName := gentianov1alpha1.SidecarAppName(parentProfile, sidecar.Name)
	oidcSpec := sidecar.ServiceRequirements.Identity.OIDC
	clientID := oidcSpec.ClientID
	if clientID == "" {
		clientID = oidcClientID(tenant.Name, profileName)
	}
	// The app Composition holds the rule that narrows a request-bound
	// client's https://* for the app's own client alone; an extension's it
	// composes without one, so nothing would hold its request parameter
	// (see resolveOIDCAppConfig).
	if oidcSpec.RequestParameter != nil {
		return oidcAppConfig{}, refuseOIDCClient(
			"ComponentProfile %s: extension %s declares requestParameter, which only the app's own client can: the app Composition binds no request parameter on an extension's client", parentProfile, sidecar.Name)
	}
	redirects, err := r.resolveOIDCRedirectURIs(ctx, tenant, owner, profileName, oidcSpec.RedirectURIs, false)
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
	// The parent's catalogue: the extension's client is declared on it, and
	// the app Composition looks for its pack there.
	if err := r.resolveOIDCPack(ctx, &cfg, parentProfile, packKey); err != nil {
		return oidcAppConfig{}, err
	}
	return cfg, nil
}

// getOIDCOwnerProfile returns the ComponentProfile that owns an OIDC config. Sidecar
// configs (element-jitsi) live on the parent profile (element); there is no
// standalone ComponentProfile for sidecars; sidecar OIDC configs live on the parent profile.
func (r *TenantReconciler) getOIDCOwnerProfile(ctx context.Context, cfg oidcAppConfig) (*gentianov1alpha1.ComponentProfile, error) {
	ownerName := cfg.profileName
	if cfg.parentProfile != "" {
		ownerName = cfg.parentProfile
	}
	profile := &gentianov1alpha1.ComponentProfile{}
	if err := r.Get(ctx, types.NamespacedName{Name: ownerName}, profile); err != nil {
		return nil, fmt.Errorf("get ComponentProfile %s: %w", ownerName, err)
	}
	return profile, nil
}

// resolveOIDCRedirectURIs is where the client's codes may be delivered: the
// declared redirectUris, else the profile's default annotation. A
// request-bound client needs neither -- its address is computed from each
// request, and the Composition registers https://* for it -- so for one
// nothing declared is no address, not a refusal.
func (r *TenantReconciler) resolveOIDCRedirectURIs(
	_ context.Context,
	tenant *gentianov1alpha1.Tenant,
	profile *gentianov1alpha1.ComponentProfile,
	profileName string,
	uris []string,
	requestBound bool,
) ([]string, error) {
	if len(uris) > 0 {
		return substituteTenantDomainInURIs(tenant, uris, r.KernelDomain, r.TenancyMode), nil
	}
	if profile != nil {
		defaults, err := gentianov1alpha1.ProfileOIDCDefaultRedirectURIs(profile)
		if err != nil {
			return nil, refuseOIDCClient("parse %s on ComponentProfile %s: %v", gentianov1alpha1.AnnotationProfileOIDCDefaultRedirectURIs, profileName, err)
		}
		if len(defaults) > 0 {
			return substituteTenantDomainInURIs(tenant, defaults, r.KernelDomain, r.TenancyMode), nil
		}
	}
	if requestBound {
		return nil, nil
	}
	// Sidecar OIDC redirect URIs must be declared on the ComponentProfile or in the pack spec.
	return nil, refuseOIDCClient("ComponentProfile %s: no OIDC redirect URIs in pack spec and no %s annotation",
		profileName, gentianov1alpha1.AnnotationProfileOIDCDefaultRedirectURIs)
}

func substituteTenantDomainInURIs(tenant *gentianov1alpha1.Tenant, uris []string, kernelDomain, tenancyMode string) []string {
	host := tenant.EffectiveDomain(kernelDomain, tenancyMode)
	out := make([]string, 0, len(uris))
	for _, u := range uris {
		out = append(out, strings.ReplaceAll(u, "${TENANT_DOMAIN}", host))
	}
	return out
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

// makeOIDCPackJob creates a Job that makes or updates an app's OIDC client,
// with its pack's scope, mappers and role, in the tenant's realm.
//
// When withSecret is set (Seeder enabled, confidential client) the Job gives
// the client the secret the vault holds for the app. The pod reads it from
// the Secret of the Job's name, which the Job owns (jobSecret); it is in no
// Job spec, no ConfigMap and no composed Object.
func makeOIDCPackJob(tenant *gentianov1alpha1.Tenant, realmName string, cfg oidcAppConfig, withSecret bool, entitlementGroup string) *batchv1.Job {
	ttl := meta.ProvisioningJobTTLSeconds
	deadline := meta.ProvisioningJobActiveDeadlineSeconds
	jobName := clientJobName(tenant.Name, cfg.profileName)
	container := keycloakContainer("provision-oidc-pack",
		buildOIDCPackScript(realmName, cfg.clientID, *cfg.pack, cfg.templates, cfg.redirectURIs, withSecret, entitlementGroup))
	if withSecret {
		container.Env = append(container.Env, meta.SecretEnv("OIDC_CLIENT_SECRET", jobName, jobSecretClientSecretKey))
	}
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      jobName,
			Namespace: identityNamespace,
			Labels: map[string]string{
				tenantLabel:    tenant.Name,
				managedByLabel: managedByValue,
				appLabel:       cfg.profileName,
			},
		},
		Spec: batchv1.JobSpec{
			TTLSecondsAfterFinished: &ttl,
			ActiveDeadlineSeconds:   &deadline,
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyOnFailure,
					Containers:    []corev1.Container{container},
				},
			},
		},
	}
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
		profile, err := getProfileFor(ctx, r.Client, app.Profile, app.Digest)
		if err != nil {
			if errors.IsNotFound(err) {
				continue
			}
			return nil, fmt.Errorf("get ComponentProfile %s: %w", app.Profile, err)
		}
		if profile.Services() != nil &&
			profile.Services().Identity != nil &&
			profile.Services().Identity.SAML != nil {
			samlSpec := profile.Services().Identity.SAML
			host := tenant.EffectiveDomain(r.KernelDomain, r.TenancyMode)
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
