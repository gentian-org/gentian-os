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

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/authz"
	"github.com/gentian-org/gentian-os/internal/locales"
)

func (r *TenantReconciler) ensureRealmBrowserSecurityHeaders(ctx context.Context, realm string, locales []string) error {
	if realm == "" {
		return nil
	}
	kcURL, kcUser, kcPass, err := loadKeycloakAdmin(ctx, r.Client)
	if err != nil {
		return fmt.Errorf("load keycloak-admin for browser security headers: %w", err)
	}
	kc := authz.NewKeycloakAdminClient(kcURL, kcUser, kcPass)
	if err := kc.UpdateRealmBrowserSecurityHeaders(ctx, realm, locales); err != nil {
		return fmt.Errorf("update realm %s browser security headers: %w", realm, err)
	}
	return nil
}

// kernelRealmLocales are the languages the kernel realm offers, which is the
// PLATFORM TENANT's declared state: Tenant/platform adopts the kernel realm
// (AD-10), so what that tenant declares is what that realm is.
//
// Read from the Tenant rather than from this process, because which languages
// a realm offers is an administrator's choice. It reaches the cluster the way
// every other choice does — a director call, a commit, Argo — and the operator
// only applies what git already says. Nothing here decides it.
//
// A cluster with no platform tenant yet gets the platform's own set, which is
// what the realm would have had anyway.
func (r *TenantReconciler) kernelRealmLocales(ctx context.Context, kernelRealm string) []string {
	tenants := &gentianov1alpha1.TenantList{}
	if err := r.List(ctx, tenants); err != nil {
		return locales.Default
	}
	for i := range tenants.Items {
		t := &tenants.Items[i]
		if t.DeletionTimestamp == nil && tenantAdoptsKernelRealm(t, kernelRealm) {
			// Normalised here, never empty: this is the kernel realm's only
			// writer, so "the platform tenant declares nothing" has to come
			// back as the platform's own set rather than as silence. Silence
			// would leave internationalization off and the login page English.
			return locales.Normalise(t.Spec.Locales)
		}
	}
	return locales.Default
}

// ensureKeycloakBrowserSecurityHeaders disables X-Frame-Options on kernel and
// tenant realms so OIDC broker /endpoint callbacks work inside portal iframes.
func (r *TenantReconciler) ensureKeycloakBrowserSecurityHeaders(ctx context.Context, tenant *gentianov1alpha1.Tenant) error {
	kernelRealm := r.KernelRealm
	if kernelRealm == "" {
		kernelRealm = "kernel"
	}
	if err := r.ensureRealmBrowserSecurityHeaders(ctx, kernelRealm, r.kernelRealmLocales(ctx, kernelRealm)); err != nil {
		return err
	}
	// Not the tenant realm. tenant-default composes a Realm that declares the
	// same browser security headers, and the same twelve-hour session and token
	// lifespans and login theme this function used to write alongside them.
	//
	// The kernel realm above stays, and has to: no XTenant exists for it, so no
	// Composition covers it. It is bootstrapped once at install and this is the
	// only thing that maintains it.

	r.deleteRetiredJobs(ctx, kernelBrowserSecurityJobName(), tenantBrowserSecurityJobName(tenant.Name))
	return nil
}
