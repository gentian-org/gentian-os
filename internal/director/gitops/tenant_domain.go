/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package gitops

import (
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"

	"sigs.k8s.io/yaml"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// ErrInvalidDomain is a custom domain that cannot be one: not a hostname, on
// the kernel domain, or already another tenant's.
var ErrInvalidDomain = errors.New("invalid custom domain")

// ErrPlatformTenantDomain is a domain asked for the platform tenant.
//
// That tenant's addresses are the cluster's own: its desktop is
// platform.<kernel>, its components answer below that, and the kernel's
// routes, the administrators' sign-in and the front page on the bare domain
// are all written for those names. Bound to another domain it would be
// declared somewhere none of them look. The message is what a person reads:
// the API hands it on unchanged.
var ErrPlatformTenantDomain = errors.New(
	"this is the platform tenant: it is where the cluster's administrators sign in, at platform.<the cluster's domain>, " +
		"and it stays on the cluster's own addresses. A domain is bound to a tenant for users. Nothing was changed")

// errSingleTenancyDomain is a domain asked for a tenant of a single-tenancy
// cluster, wrapped in ErrSingleTenancy.
func errSingleTenancyDomain() error {
	return fmt.Errorf("%w: its one tenant for users is on the cluster's own addresses, and bound to a domain of its own "+
		"it would leave them and give up the cluster's main address. Binding a domain is for a cluster with many tenants "+
		"(tenancyMode: multi). Nothing was changed", ErrSingleTenancy)
}

// refuseDomainBinding is why no domain may be bound to this tenant, or nil.
//
// Two tenants take none. The platform tenant, under either mode. And any
// tenant of a single-tenancy cluster: the user tenant's domain there is the
// cluster's, which is what puts its apps at <label>.<kernel> and lets it
// hold the main address. Asked before the domain is looked at, since no
// domain would do.
func (g *GitOps) refuseDomainBinding(ctx context.Context, tenant string) error {
	platform, err := g.IsPlatformTenant(ctx, tenant)
	if err != nil {
		return err
	}
	if platform {
		return ErrPlatformTenantDomain
	}
	settings, err := g.ClusterSettingValues(ctx)
	if err != nil && !errors.Is(err, ErrNoClusterClaim) {
		return err
	}
	if gentianov1alpha1.NormalizeTenancyMode(settings["tenancyMode"]) == gentianov1alpha1.TenancyModeSingle {
		return errSingleTenancyDomain()
	}
	return nil
}

var hostnamePattern = regexp.MustCompile(`^([a-z0-9]([-a-z0-9]*[a-z0-9])?\.)+[a-z]{2,}$`)

// SetTenantDomain binds a tenant to a custom domain, or unbinds it when
// domain is empty: one commit of the TenantDomain beside the tenant's
// manifest and its kustomization entry. The operator moves the tenant's
// hosts, mail and logins to the domain from there.
//
// Refused before anything is written: a bind for the platform tenant
// (ErrPlatformTenantDomain) or on a single-tenancy cluster
// (ErrSingleTenancy); a name that is not a hostname, one on the kernel
// domain -- the operator would refuse it, and the tenant is there already --
// and one another tenant holds, since two tenants on one domain would claim
// the same hosts. Unbinding is refused to nobody, so a tenant that was bound
// where it should not have been can be put back.
func (g *GitOps) SetTenantDomain(ctx context.Context, tenant, domain string, meta Meta) (Result, error) {
	if !ValidName(tenant) {
		return Result{}, fmt.Errorf("%w: tenant %q", ErrInvalidName, tenant)
	}
	domain = strings.ToLower(strings.TrimSpace(domain))
	if domain == "" {
		return g.writeTenantFile(ctx, tenant, TenantDomainFile, "", listResource,
			fmt.Sprintf("Tenant %s: no custom domain", tenant), meta)
	}
	if err := g.refuseDomainBinding(ctx, tenant); err != nil {
		return Result{}, err
	}
	if len(domain) > 253 || !hostnamePattern.MatchString(domain) {
		return Result{}, fmt.Errorf("%w: %q is not a hostname", ErrInvalidDomain, domain)
	}
	kernel, err := g.KernelDomain(ctx)
	if err != nil {
		return Result{}, err
	}
	if domain == kernel || strings.HasSuffix(domain, "."+kernel) {
		return Result{}, fmt.Errorf("%w: %s is on the kernel domain, where every tenant already is", ErrInvalidDomain, domain)
	}
	names, err := g.Tenants(ctx)
	if err != nil {
		return Result{}, err
	}
	for _, other := range names {
		if other == tenant {
			continue
		}
		file, err := g.TenantFile(ctx, other)
		if err != nil {
			continue
		}
		if held, err := tenantCustomDomain(file); err == nil && held == domain {
			return Result{}, fmt.Errorf("%w: %s is tenant %s's", ErrInvalidDomain, domain, other)
		}
	}
	return g.writeTenantFile(ctx, tenant, TenantDomainFile, renderTenantDomain(tenant, domain), listResource,
		fmt.Sprintf("Tenant %s: custom domain %s", tenant, domain), meta)
}

// renderTenantDomain is the object a reviewer reads in the commit.
func renderTenantDomain(tenant, domain string) string {
	return "# Managed by the director: where this tenant is served instead of\n" +
		"# <tenant>.<kernel>. Its hosts, mail and logins follow. The domain's DNS\n" +
		"# has to point at this cluster, and the certificate issuer has to be able\n" +
		"# to answer for it.\n" +
		"apiVersion: gentianos.io/v1alpha1\n" +
		"kind: TenantDomain\n" +
		"metadata:\n" +
		"  name: " + tenant + "\n" +
		"spec:\n" +
		"  domain: " + domain + "\n"
}

// TenantPlacement is what git says about where a tenant's hosts are: the
// realm its people are in, which is what makes it the platform tenant, and
// the domain a TenantDomain binds it to.
type TenantPlacement struct {
	// Realm is spec.isolation.keycloakRealm; empty for a tenant that names
	// none, whose realm is its own name.
	Realm string
	// CustomDomain is the bound domain, or "" for a tenant at its default.
	// One on the kernel domain is "" as well: the operator refuses it and
	// leaves the tenant where it was (resolveTenantDomain).
	CustomDomain string
}

// TenantPlacement reads a tenant's placement from its manifest and the
// TenantDomain beside it. It is the repository's word: the operator moves a
// tenant to a newly bound domain a moment after the commit.
func (g *GitOps) TenantPlacement(ctx context.Context, tenant string) (TenantPlacement, error) {
	if !ValidName(tenant) {
		return TenantPlacement{}, fmt.Errorf("%w: tenant %q", ErrInvalidName, tenant)
	}
	file, err := g.TenantFile(ctx, tenant)
	if err != nil {
		return TenantPlacement{}, err
	}
	b, err := os.ReadFile(file)
	if err != nil {
		return TenantPlacement{}, err
	}
	var doc struct {
		Spec struct {
			Isolation struct {
				KeycloakRealm string `json:"keycloakRealm"`
			} `json:"isolation"`
		} `json:"spec"`
	}
	if err := yaml.Unmarshal(b, &doc); err != nil {
		return TenantPlacement{}, fmt.Errorf("read %s: %w", file, err)
	}
	out := TenantPlacement{Realm: doc.Spec.Isolation.KeycloakRealm}
	custom, err := tenantCustomDomain(file)
	if err != nil {
		return TenantPlacement{}, err
	}
	if custom == "" {
		return out, nil
	}
	kernel, err := g.KernelDomain(ctx)
	if err != nil && !errors.Is(err, ErrNoClusterClaim) {
		return TenantPlacement{}, err
	}
	if kernel == "" || (custom != kernel && !strings.HasSuffix(custom, "."+kernel)) {
		out.CustomDomain = custom
	}
	return out, nil
}
