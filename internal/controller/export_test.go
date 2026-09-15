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

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// DovecotDeployedForTest exposes the Dovecot gate to the external test package.
// The predicate decides whether an entire provisioning path runs, and it fails
// silently when wrong, so it is worth asserting directly.
func (r *TenantReconciler) DovecotDeployedForTest(ctx context.Context) bool {
	return r.dovecotDeployed(ctx)
}

// DefaultTenantMailModeForTest exposes the mail-mode default to the external
// test package. Like the Dovecot gate it decides a whole provisioning path from
// cluster state a tenant never states, and getting it wrong is silent.
func (r *TenantReconciler) DefaultTenantMailModeForTest(ctx context.Context) gentianov1alpha1.MailMode {
	return r.defaultTenantMailMode(ctx)
}

// SyncTenantMailDNSForTest exposes the tenant mail-DNS writer. It publishes into
// a public zone and, on a tunnel cluster, its records collide with the tenant's
// own web CNAME -- so whether it runs at all is worth asserting directly rather
// than inferring from a reconcile.
func (r *TenantReconciler) SyncTenantMailDNSForTest(ctx context.Context, tenant *gentianov1alpha1.Tenant) error {
	return r.syncTenantMailDNS(ctx, tenant)
}
