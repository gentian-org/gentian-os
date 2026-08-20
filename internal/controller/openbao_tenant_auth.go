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

	"sigs.k8s.io/controller-runtime/pkg/log"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/kernel/secrets"
	"github.com/gentian-org/gentian-os/internal/keycloak"
)

// tenantAuthRoleName is the JWT role on a tenant's own auth mount. It does not
// need the tenant in its name: the mount already scopes it to one realm, and the
// credential manager finds it by mount rather than by role.
const tenantAuthRoleName = "tenant-admin"

// tenantClaimName is the claim the tenant realm stamps and the role maps into
// metadata. It matches CREDENTIAL_TENANT_CLAIM, which the credential manager
// reads the metadata under — one name, so the two cannot disagree.
const tenantClaimName = "tenant"

// ensureTenantOpenBaoAuth gives a tenant's realm its own JWT auth mount in
// OpenBao, so a tenant administrator's portal token can be exchanged at all.
//
// Without it the exchange fails at signature verification: the kernel realm's
// mount trusts one issuer, and a tenant member authenticates in their own realm
// because that is where their apps' OIDC clients live. Both roles on the kernel
// mount were therefore unreachable by every tenant administrator — the failure
// looked like a permissions problem and was not one.
//
// Reconciled in-process rather than through a Job. The operator already holds an
// OpenBao session through Kubernetes auth, so a Job would add an image, a script
// and a second identity to do what one HTTP call does — and unlike a Job this
// re-asserts the configuration on every reconcile, which is what makes a changed
// discovery URL or a hand-edited role converge back.
func (r *TenantReconciler) ensureTenantOpenBaoAuth(ctx context.Context, tenant *gentianov1alpha1.Tenant) error {
	if r.Seeder == nil || r.Seeder.KV() == nil || r.KernelDomain == "" {
		// No OpenBao session, or no kernel domain to build an issuer from. Both
		// are cluster-level configuration; a tenant cannot supply them.
		return nil
	}
	realm := tenantRealm(tenant)
	if realm == "" {
		return nil
	}

	auth := secrets.NewTenantAuth(r.Seeder.KV())
	// The same base the broker IdP uses for this cluster's Keycloak, with the
	// tenant's realm instead of the kernel's. Deriving it here rather than
	// taking a second setting keeps one answer to "where is Keycloak": two
	// could disagree, and the one that is wrong fails at signature
	// verification, which reads as a permissions problem.
	discovery := fmt.Sprintf("%s/realms/%s", strings.TrimRight(kernelExternalURL(r.KernelDomain), "/"), realm)

	if err := auth.EnsureMount(ctx, realm, discovery); err != nil {
		return fmt.Errorf("tenant auth mount: %w", err)
	}
	if err := auth.EnsureRole(ctx, realm, secrets.RoleConfig{
		Name: tenantAuthRoleName,
		// The audience the portal's own client puts in the token. Without the
		// audience mapper on that client this list matches nothing, which is a
		// refusal that names the audience rather than the group.
		BoundAudiences: []string{"openbao"},
		GroupsClaim:    "groups",
		// The group identity_reconciler actually puts a tenant administrator in,
		// from the same helper that names it — so the two cannot drift apart.
		//
		// No leading slash: the tenant realm's groups mapper is created with
		// full.path=false, so the claim carries the bare group name. The kernel
		// realm's is created with full.path=true and its roles bind /-prefixed
		// values, which is why copying one into the other silently matches
		// nothing. This value must follow the mapper in THIS realm.
		BoundGroup: keycloak.TenantAdminsGroup(tenant.Name),
		// The tenant, carried from a verified claim into token metadata. The
		// credential manager reads it to decide scope, and without it a tenant
		// admin's writes land at cluster paths — which is what happened.
		//
		// The claim is stamped on the tenant realm's portal client with the
		// tenant NAME, not the realm: the two are the same by default but a
		// claim may set isolation.keycloakRealm to something else, and every
		// path downstream is keyed by the tenant.
		ClaimMappings: map[string]string{tenantClaimName: tenantClaimName},
		// The policy the tenant Composition already emits, scoped to this
		// tenant's paths. Nothing here grants a path; it names one that exists.
		TokenPolicies: []string{"tenant-" + tenant.Name},
		TokenTTL:      1800,
		TokenMaxTTL:   3600,
	}); err != nil {
		return fmt.Errorf("tenant auth role: %w", err)
	}

	log.FromContext(ctx).Info("tenant OpenBao auth mount reconciled",
		"tenant", tenant.Name, "mount", secrets.MountPath(realm), "discovery", discovery)
	return nil
}

// removeTenantOpenBaoAuth deletes the mount when the Tenant goes. A mount left
// behind trusts a realm that no longer exists, and realm names are reusable — so
// the next tenant of the same name would inherit the previous one's roles.
func (r *TenantReconciler) removeTenantOpenBaoAuth(ctx context.Context, tenant *gentianov1alpha1.Tenant) error {
	if r.Seeder == nil || r.Seeder.KV() == nil {
		return nil
	}
	realm := tenantRealm(tenant)
	if realm == "" {
		return nil
	}
	return secrets.NewTenantAuth(r.Seeder.KV()).DeleteMount(ctx, realm)
}

// tenantRealm is the realm a tenant's members authenticate in, defaulting to the
// tenant name the way every other consumer of this field does.
func tenantRealm(tenant *gentianov1alpha1.Tenant) string {
	if tenant.Spec.Isolation.KeycloakRealm != "" {
		return tenant.Spec.Isolation.KeycloakRealm
	}
	return tenant.Name
}
