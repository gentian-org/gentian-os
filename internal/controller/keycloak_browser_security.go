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

	batchv1 "k8s.io/api/batch/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/log"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/authz"
)

func kernelBrowserSecurityJobName() string {
	return "keycloak-browser-security-kernel"
}

func tenantBrowserSecurityJobName(tenantName string) string {
	return fmt.Sprintf("keycloak-browser-security-%s", tenantName)
}

func (r *TenantReconciler) deleteLegacyBrowserSecurityJobs(ctx context.Context, names ...string) {
	logger := log.FromContext(ctx)
	for _, name := range names {
		job := &batchv1.Job{}
		job.Name = name
		job.Namespace = kernelNamespace
		if err := r.Delete(ctx, job); err != nil && !errors.IsNotFound(err) {
			logger.Error(err, "delete legacy browser security job", "job", name)
		}
	}
}

func (r *TenantReconciler) ensureRealmBrowserSecurityHeaders(ctx context.Context, realm string) error {
	if realm == "" {
		return nil
	}
	kcURL, kcUser, kcPass, err := loadKeycloakAdmin(ctx, r.Client)
	if err != nil {
		return fmt.Errorf("load keycloak-admin for browser security headers: %w", err)
	}
	kc := authz.NewKeycloakAdminClient(kcURL, kcUser, kcPass)
	if err := kc.UpdateRealmBrowserSecurityHeaders(ctx, realm); err != nil {
		return fmt.Errorf("update realm %s browser security headers: %w", realm, err)
	}
	return nil
}

// ensureKeycloakBrowserSecurityHeaders disables X-Frame-Options on kernel and
// tenant realms so OIDC broker /endpoint callbacks work inside portal iframes.
func (r *TenantReconciler) ensureKeycloakBrowserSecurityHeaders(ctx context.Context, tenant *gentianov1alpha1.Tenant) error {
	kernelRealm := r.KernelRealm
	if kernelRealm == "" {
		kernelRealm = "kernel"
	}
	if err := r.ensureRealmBrowserSecurityHeaders(ctx, kernelRealm); err != nil {
		return err
	}
	// Not the tenant realm. tenant-default composes a Realm that declares the
	// same browser security headers, and the same twelve-hour session and token
	// lifespans and login theme this function used to write alongside them.
	//
	// The kernel realm above stays, and has to: no XTenant exists for it, so no
	// Composition covers it. It is bootstrapped once at install and this is the
	// only thing that maintains it.

	r.deleteLegacyBrowserSecurityJobs(ctx, kernelBrowserSecurityJobName(), tenantBrowserSecurityJobName(tenant.Name))
	return nil
}
