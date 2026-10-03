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

package gitops

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// ErrInvalidDomain is a custom domain that cannot be one: not a hostname, on
// the kernel domain, or already another tenant's.
var ErrInvalidDomain = errors.New("invalid custom domain")

var hostnamePattern = regexp.MustCompile(`^([a-z0-9]([-a-z0-9]*[a-z0-9])?\.)+[a-z]{2,}$`)

// SetTenantDomain binds a tenant to a custom domain, or unbinds it when
// domain is empty: one commit of the TenantDomain beside the tenant's
// manifest and its kustomization entry. The operator moves the tenant's
// hosts, mail and logins to the domain from there.
//
// Refused before anything is written: a name that is not a hostname, one on
// the kernel domain -- the operator would refuse it, and the tenant is there
// already -- and one another tenant holds, since two tenants on one domain
// would claim the same hosts.
func (g *GitOps) SetTenantDomain(ctx context.Context, tenant, domain string, meta Meta) (Result, error) {
	if !ValidName(tenant) {
		return Result{}, fmt.Errorf("%w: tenant %q", ErrInvalidName, tenant)
	}
	domain = strings.ToLower(strings.TrimSpace(domain))
	if domain == "" {
		return g.writeTenantFile(ctx, tenant, TenantDomainFile, "", listResource,
			fmt.Sprintf("Tenant %s: no custom domain", tenant), meta)
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
