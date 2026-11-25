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
	"sort"
	"strings"

	"sigs.k8s.io/yaml"
)

// TenantTrustedDomains is a tenant's spec.trustedDomains: the domains
// outside the cluster trusted for this tenant alone, and whether the tenant
// may keep such a list.
type TenantTrustedDomains struct {
	// Delegated says the tenant's perimeter approver may add and remove
	// domains. While it is off the list answers nothing.
	Delegated bool     `json:"delegated"`
	Domains   []string `json:"domains"`
}

// MaxTenantTrustedDomains is how many domains one tenant's list holds, as
// the Tenant's schema has it.
const MaxTenantTrustedDomains = 64

// Errors of changing a tenant's trusted domains.
var (
	// ErrTrustedDomainInvalid is a name that is not a domain.
	ErrTrustedDomainInvalid = errors.New("not a domain")
	// ErrTrustedDomainsNotDelegated is a tenant that may not keep a list.
	ErrTrustedDomainsNotDelegated = errors.New("trusted domains are not delegated to this tenant")
	// ErrTrustedDomainNotFound is a domain that is not on the list.
	ErrTrustedDomainNotFound = errors.New("no such trusted domain")
	// ErrTrustedDomainsFull is a list that holds as many as it may.
	ErrTrustedDomainsFull = errors.New("the tenant's list of trusted domains is full")
)

// trustedDomain is a domain as the Tenant's schema takes one: lower-case
// labels, at least two, the last one not a number.
var trustedDomain = regexp.MustCompile(`^([a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?\.)+[a-z]([-a-z0-9]{0,61}[a-z0-9])?$`)

// ValidTrustedDomain says whether domain is one a list of trusted domains
// holds.
func ValidTrustedDomain(domain string) bool {
	return len(domain) <= 253 && trustedDomain.MatchString(domain)
}

// parseTenantTrustedDomains reads spec.trustedDomains from a tenant
// manifest's text, keeping only what is a domain, once.
func parseTenantTrustedDomains(text string) (TenantTrustedDomains, error) {
	var doc struct {
		Spec struct {
			TrustedDomains *TenantTrustedDomains `json:"trustedDomains"`
		} `json:"spec"`
	}
	if err := yaml.Unmarshal([]byte(text), &doc); err != nil {
		return TenantTrustedDomains{}, fmt.Errorf("parse tenant manifest: %w", err)
	}
	out := TenantTrustedDomains{}
	if doc.Spec.TrustedDomains == nil {
		return out, nil
	}
	out.Delegated = doc.Spec.TrustedDomains.Delegated
	seen := map[string]bool{}
	for _, d := range doc.Spec.TrustedDomains.Domains {
		if !ValidTrustedDomain(d) || seen[d] {
			continue
		}
		seen[d] = true
		out.Domains = append(out.Domains, d)
	}
	return out, nil
}

// TenantTrustedDomains reads one tenant's own trusted domains and whether
// it may keep them.
func (g *GitOps) TenantTrustedDomains(ctx context.Context, tenant string) (TenantTrustedDomains, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	file, err := g.tenantFileRead(ctx, tenant)
	if err != nil {
		return TenantTrustedDomains{}, err
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		return TenantTrustedDomains{}, err
	}
	return parseTenantTrustedDomains(string(raw))
}

// AddTenantTrustedDomain trusts one domain for a tenant: an egress request
// of one of its apps whose hosts are all in trusted domains is granted
// without a further answer. Refused unless the cluster's administrator
// delegated the list to the tenant.
func (g *GitOps) AddTenantTrustedDomain(ctx context.Context, tenant, domain string, meta Meta) (Result, error) {
	if !ValidTrustedDomain(domain) {
		return Result{}, fmt.Errorf("%w: %q", ErrTrustedDomainInvalid, domain)
	}
	msg := fmt.Sprintf("feat(%s): trust the domain %s (via %s)", tenant, domain, meta.actor())
	return g.apply(ctx, tenant, msg, meta, func(text string) (string, string, bool, error) {
		have, err := parseTenantTrustedDomains(text)
		if err != nil {
			return text, "", false, err
		}
		if !have.Delegated {
			return text, "", false, ErrTrustedDomainsNotDelegated
		}
		for _, d := range have.Domains {
			if d == domain {
				return text, "unchanged", false, nil
			}
		}
		if len(have.Domains) >= MaxTenantTrustedDomains {
			return text, "", false, ErrTrustedDomainsFull
		}
		have.Domains = append(have.Domains, domain)
		out, err := setTenantTrustedDomains(text, have)
		if err != nil {
			return text, "", false, err
		}
		return out, "added", true, nil
	})
}

// RemoveTenantTrustedDomain stops trusting one domain for a tenant. A
// request it answered waits for a person again. Refused unless the list is
// delegated to the tenant.
func (g *GitOps) RemoveTenantTrustedDomain(ctx context.Context, tenant, domain string, meta Meta) (Result, error) {
	if !ValidTrustedDomain(domain) {
		return Result{}, fmt.Errorf("%w: %q", ErrTrustedDomainInvalid, domain)
	}
	msg := fmt.Sprintf("feat(%s): stop trusting the domain %s (via %s)", tenant, domain, meta.actor())
	return g.apply(ctx, tenant, msg, meta, func(text string) (string, string, bool, error) {
		have, err := parseTenantTrustedDomains(text)
		if err != nil {
			return text, "", false, err
		}
		if !have.Delegated {
			return text, "", false, ErrTrustedDomainsNotDelegated
		}
		kept := make([]string, 0, len(have.Domains))
		for _, d := range have.Domains {
			if d != domain {
				kept = append(kept, d)
			}
		}
		if len(kept) == len(have.Domains) {
			return text, "", false, fmt.Errorf("%w: %s", ErrTrustedDomainNotFound, domain)
		}
		have.Domains = kept
		out, err := setTenantTrustedDomains(text, have)
		if err != nil {
			return text, "", false, err
		}
		return out, "removed", true, nil
	})
}

// SetTenantTrustedDomainsDelegation turns on or off whether a tenant may
// keep its own trusted domains. Turning it off removes nothing: the list
// stays in the manifest and answers nothing until it is delegated again.
func (g *GitOps) SetTenantTrustedDomainsDelegation(ctx context.Context, tenant string, delegated bool, meta Meta) (Result, error) {
	verb := "delegate trusted domains to"
	if !delegated {
		verb = "stop delegating trusted domains to"
	}
	msg := fmt.Sprintf("feat(%s): %s the tenant (via %s)", tenant, verb, meta.actor())
	return g.apply(ctx, tenant, msg, meta, func(text string) (string, string, bool, error) {
		have, err := parseTenantTrustedDomains(text)
		if err != nil {
			return text, "", false, err
		}
		if have.Delegated == delegated {
			return text, "unchanged", false, nil
		}
		have.Delegated = delegated
		out, err := setTenantTrustedDomains(text, have)
		if err != nil {
			return text, "", false, err
		}
		return out, "updated", true, nil
	})
}

// setTenantTrustedDomains writes spec.trustedDomains into a tenant
// manifest's text, sorted, or removes it when there is nothing to say, and
// checks that what it wrote reads back as what was meant.
func setTenantTrustedDomains(text string, t TenantTrustedDomains) (string, error) {
	domains := append([]string(nil), t.Domains...)
	sort.Strings(domains)
	var block []string
	if t.Delegated || len(domains) > 0 {
		block = []string{"  trustedDomains:"}
		if t.Delegated {
			block = append(block, "    delegated: true")
		}
		if len(domains) > 0 {
			block = append(block, "    domains:")
			for _, d := range domains {
				block = append(block, "      - "+quoteScalar(d))
			}
		}
	}
	out, err := setSpecBlock(text, []string{"trustedDomains"}, block)
	if err != nil {
		return "", err
	}
	back, err := parseTenantTrustedDomains(out)
	if err != nil {
		return "", fmt.Errorf("the edited manifest is not valid YAML: %w", err)
	}
	if back.Delegated != t.Delegated || strings.Join(back.Domains, " ") != strings.Join(domains, " ") {
		return "", errors.New("the edited manifest does not read back as written")
	}
	return out, nil
}
