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
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"sigs.k8s.io/yaml"
)

// A tenant's partners, as a commit.
//
// A partner is a realm of another cluster this tenant's realm links its
// people's accounts with (Tenant spec.partners, AD-17). The director writes
// the list, as the patch partners.yaml beside the tenant; the composition
// makes the clients, scope and identity provider from it. Nothing here holds
// a Keycloak credential, and a realm rebuilt from scratch comes back with the
// same partners.

// PartnersFile is the patch a tenant's partners are written to.
const PartnersFile = "partners.yaml"

// Partner is one entry of Tenant spec.partners.
type Partner struct {
	Name   string         `json:"name"`
	Issuer string         `json:"issuer"`
	As     string         `json:"as,omitempty"`
	Issue  bool           `json:"issue,omitempty"`
	Accept *PartnerAccept `json:"accept,omitempty"`
}

// PartnerAccept is the receiving side of a partner.
type PartnerAccept struct {
	// Accounts is link, create or organization.
	Accounts    string `json:"accounts"`
	Domain      string `json:"domain,omitempty"`
	DisplayName string `json:"displayName,omitempty"`
}

// ErrInvalidPartner is an entry that cannot be one.
var ErrInvalidPartner = errors.New("invalid partner")

// ErrPartnerNotAllowed is a partner whose issuer the Cluster claim does not
// allow (spec.partners.allowedIssuers).
var ErrPartnerNotAllowed = errors.New("partner not allowed on this cluster")

// ErrPartnerHeld is an entry a component may not change: one that issues, or
// one of another kind than it may write.
var ErrPartnerHeld = errors.New("held by the tenant's administrator")

var partnerName = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,38}[a-z0-9])?$`)

// ValidPartner checks one entry as the CRD would.
func ValidPartner(p Partner) error {
	if !partnerName.MatchString(p.Name) {
		return fmt.Errorf("%w: name %q is not a DNS label of up to 40 characters", ErrInvalidPartner, p.Name)
	}
	if p.As != "" && !partnerName.MatchString(p.As) {
		return fmt.Errorf("%w: as %q is not a DNS label of up to 40 characters", ErrInvalidPartner, p.As)
	}
	u, err := url.Parse(p.Issuer)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" ||
		strings.ContainsAny(p.Issuer, " \t\n\"\\") || len(u.Path) < 2 || strings.HasSuffix(p.Issuer, "/") ||
		len(p.Issuer) > 500 {
		return fmt.Errorf("%w: issuer %q is not https://<host>/<path to the realm>", ErrInvalidPartner, p.Issuer)
	}
	if !p.Issue && p.Accept == nil {
		return fmt.Errorf("%w: a partner issues, accepts, or both", ErrInvalidPartner)
	}
	if a := p.Accept; a != nil {
		switch a.Accounts {
		case "link", "create":
			if a.Domain != "" || a.DisplayName != "" {
				return fmt.Errorf("%w: domain and displayName are for an organization", ErrInvalidPartner)
			}
		case "organization":
			if len(a.Domain) > 253 || !hostnamePattern.MatchString(a.Domain) {
				return fmt.Errorf("%w: an organization names its domain; %q is not one", ErrInvalidPartner, a.Domain)
			}
			if d := a.DisplayName; len(d) > 120 || strings.ContainsAny(d, "\n\r\t\"\\") {
				return fmt.Errorf("%w: displayName must be up to 120 characters on one line, without quotes", ErrInvalidPartner)
			}
		default:
			return fmt.Errorf("%w: accept.accounts is link, create or organization", ErrInvalidPartner)
		}
	}
	return nil
}

// IssuerAllowed reports whether an issuer matches one of the patterns, "*"
// matching any run of characters (the operator's IssuerAllowed: the two
// must agree).
func IssuerAllowed(patterns []string, issuer string) bool {
	for _, p := range patterns {
		parts := strings.Split(p, "*")
		for i := range parts {
			parts[i] = regexp.QuoteMeta(parts[i])
		}
		if regexp.MustCompile("^" + strings.Join(parts, ".*") + "$").MatchString(issuer) {
			return true
		}
	}
	return false
}

// partnerBounds reads the Cluster claim's spec.partners: the issuers a
// tenant may name, and the defaults' issuers, which are allowed by being
// declared.
func (g *GitOps) partnerBounds(ctx context.Context) ([]string, error) {
	var claim struct {
		Spec struct {
			Partners struct {
				AllowedIssuers []string `json:"allowedIssuers"`
				Defaults       []struct {
					Issuer string `json:"issuer"`
				} `json:"defaults"`
			} `json:"partners"`
		} `json:"spec"`
	}
	if err := g.readClusterClaim(ctx, &claim); err != nil {
		if errors.Is(err, ErrNoClusterClaim) {
			return nil, nil
		}
		return nil, err
	}
	out := append([]string(nil), claim.Spec.Partners.AllowedIssuers...)
	for _, d := range claim.Spec.Partners.Defaults {
		out = append(out, strings.ReplaceAll(d.Issuer, "*", ""))
	}
	return out, nil
}

// TenantPartners reads a tenant's partners back.
func (g *GitOps) TenantPartners(ctx context.Context, tenant string) ([]Partner, error) {
	if !ValidName(tenant) {
		return nil, fmt.Errorf("%w: tenant %q", ErrInvalidName, tenant)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.partnersLocked(ctx, tenant)
}

func (g *GitOps) partnersLocked(ctx context.Context, tenant string) ([]Partner, error) {
	manifest, err := g.tenantFileRead(ctx, tenant)
	if err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(filepath.Join(filepath.Dir(manifest), PartnersFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var doc struct {
		Spec struct {
			Partners []Partner `json:"partners"`
		} `json:"spec"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("read %s: %w", PartnersFile, err)
	}
	return doc.Spec.Partners, nil
}

// SetTenantPartner adds a partner to a tenant, or replaces the one of that
// name: one commit of the patch. A commissioning component (commissioned)
// writes receiving entries only, and never replaces one that issues.
func (g *GitOps) SetTenantPartner(ctx context.Context, tenant string, entry Partner, commissioned bool, meta Meta) (Result, error) {
	if !ValidName(tenant) {
		return Result{}, fmt.Errorf("%w: tenant %q", ErrInvalidName, tenant)
	}
	if entry.Accept != nil {
		entry.Accept.Domain = strings.ToLower(strings.TrimSpace(entry.Accept.Domain))
	}
	if err := ValidPartner(entry); err != nil {
		return Result{}, err
	}
	if commissioned && (entry.Issue || entry.Accept == nil) {
		return Result{}, fmt.Errorf("%w: a component commissions receiving partners only (accept, no issue)", ErrPartnerHeld)
	}
	bounds, err := g.partnerBounds(ctx)
	if err != nil {
		return Result{}, err
	}
	if !IssuerAllowed(bounds, entry.Issuer) {
		return Result{}, fmt.Errorf("%w: %s is not in the Cluster claim's spec.partners.allowedIssuers", ErrPartnerNotAllowed, entry.Issuer)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	list, err := g.partnersLocked(ctx, tenant)
	if err != nil {
		return Result{}, err
	}
	out := make([]Partner, 0, len(list)+1)
	for _, e := range list {
		if e.Name == entry.Name {
			if commissioned && e.Issue {
				return Result{}, fmt.Errorf("%w: %s issues", ErrPartnerHeld, e.Name)
			}
			continue
		}
		out = append(out, e)
	}
	out = append(out, entry)
	return g.writeTenantFileLocked(ctx, tenant, PartnersFile, renderPartners(tenant, out), listPatch,
		fmt.Sprintf("Tenant %s: partner %s (%s)", tenant, entry.Name, partnerRoles(entry)), meta)
}

// RemoveTenantPartner takes a partner away: its client and scope, or its
// identity provider and with it every link to it. Removing one that is not
// there is unchanged; a component may not remove one that issues.
func (g *GitOps) RemoveTenantPartner(ctx context.Context, tenant, name string, commissioned bool, meta Meta) (Result, error) {
	if !ValidName(tenant) {
		return Result{}, fmt.Errorf("%w: tenant %q", ErrInvalidName, tenant)
	}
	if !partnerName.MatchString(name) {
		return Result{}, fmt.Errorf("%w: name %q is not a DNS label of up to 40 characters", ErrInvalidPartner, name)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	list, err := g.partnersLocked(ctx, tenant)
	if err != nil {
		return Result{}, err
	}
	out := make([]Partner, 0, len(list))
	for _, e := range list {
		if e.Name != name {
			out = append(out, e)
			continue
		}
		if commissioned && e.Issue {
			return Result{}, fmt.Errorf("%w: %s issues", ErrPartnerHeld, e.Name)
		}
	}
	if len(out) == len(list) {
		return Result{Status: "unchanged"}, nil
	}
	body := ""
	if len(out) > 0 {
		body = renderPartners(tenant, out)
	}
	return g.writeTenantFileLocked(ctx, tenant, PartnersFile, body, listPatch,
		fmt.Sprintf("Tenant %s: partner %s removed", tenant, name), meta)
}

func partnerRoles(p Partner) string {
	var roles []string
	if p.Issue {
		roles = append(roles, "issues")
	}
	if p.Accept != nil {
		roles = append(roles, "accepts: "+p.Accept.Accounts)
	}
	return strings.Join(roles, ", ")
}

// renderPartners writes the patch a reviewer reads in the commit, the
// entries in the order of their names.
func renderPartners(tenant string, list []Partner) string {
	sorted := append([]Partner(nil), list...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })
	var b strings.Builder
	b.WriteString("# Managed by the director: realms of other clusters this tenant's realm\n")
	b.WriteString("# links its people's accounts with (AD-17). issue: our people sign in\n")
	b.WriteString("# there and our apps ask assertions towards it, with a pairwise subject\n")
	b.WriteString("# only. accept: their people arrive here through the identity provider\n")
	b.WriteString("# partner-<name>. No secret crosses, no group or role.\n")
	b.WriteString("apiVersion: gentianos.io/v1alpha1\n")
	b.WriteString("kind: Tenant\n")
	b.WriteString("metadata:\n")
	b.WriteString("  name: " + tenant + "\n")
	b.WriteString("spec:\n")
	b.WriteString("  partners:\n")
	for _, e := range sorted {
		b.WriteString("    - name: " + e.Name + "\n")
		b.WriteString("      issuer: " + strconv.Quote(e.Issuer) + "\n")
		if e.As != "" {
			b.WriteString("      as: " + e.As + "\n")
		}
		if e.Issue {
			b.WriteString("      issue: true\n")
		}
		if a := e.Accept; a != nil {
			b.WriteString("      accept:\n")
			b.WriteString("        accounts: " + a.Accounts + "\n")
			if a.Domain != "" {
				b.WriteString("        domain: " + a.Domain + "\n")
			}
			if a.DisplayName != "" {
				b.WriteString("        displayName: " + strconv.Quote(a.DisplayName) + "\n")
			}
		}
	}
	return b.String()
}
