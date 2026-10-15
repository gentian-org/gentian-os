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

// A tenant's outside realms, as a commit.
//
// An outside realm -- a customer's cluster -- whose people sign in to this
// tenant, each as an organization of the tenant's realm (Tenant
// spec.brokeredFrom). The director writes the list, as the patch
// brokered-from.yaml beside the tenant; the composition makes the
// organization and the identity provider from it. Nothing here holds a
// Keycloak credential, and a realm rebuilt from scratch comes back with the
// same organizations.

// BrokeredFromFile is the patch a tenant's outside realms are written to.
const BrokeredFromFile = "brokered-from.yaml"

// BrokeredFrom is one outside realm (Tenant spec.brokeredFrom).
type BrokeredFrom struct {
	Name        string `json:"name"`
	DisplayName string `json:"displayName"`
	Issuer      string `json:"issuer"`
	ClientID    string `json:"clientID"`
	Domain      string `json:"domain"`
}

// ErrInvalidBrokeredFrom is an entry that cannot be one.
var ErrInvalidBrokeredFrom = errors.New("invalid outside realm")

// ErrBrokeredFromHeld is an entry whose domain or name as people read it
// another entry of the tenant already has.
var ErrBrokeredFromHeld = errors.New("held by another outside realm of this tenant")

var brokeredClientID = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

// ValidBrokeredFrom checks one entry as the CRD would, and one rule the CRD
// cannot state: the domain is the one the issuer is published under.
//
// An organization's domain decides where Keycloak sends a person who gives
// an address of it. So the domain is not the commissioner's to choose: it is
// read off the issuer, id.<domain>, the layout of a Gentian tenant's realm.
// An outside realm cannot claim the addresses of a domain it does not serve
// its sign-in from.
func ValidBrokeredFrom(b BrokeredFrom) error {
	if !brokeredName.MatchString(b.Name) {
		return fmt.Errorf("%w: name %q is not a DNS label of up to 40 characters", ErrInvalidBrokeredFrom, b.Name)
	}
	if d := strings.TrimSpace(b.DisplayName); d == "" || len(d) > 120 || d != b.DisplayName ||
		strings.ContainsAny(d, "\n\r\t\"\\") {
		return fmt.Errorf("%w: displayName must be 1 to 120 characters on one line, without quotes", ErrInvalidBrokeredFrom)
	}
	if !brokeredClientID.MatchString(b.ClientID) {
		return fmt.Errorf("%w: clientID %q is not a client id", ErrInvalidBrokeredFrom, b.ClientID)
	}
	u, err := url.Parse(b.Issuer)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.Port() != "" || u.User != nil ||
		u.RawQuery != "" || u.Fragment != "" || strings.ContainsAny(b.Issuer, " \t\n\"") ||
		!strings.HasPrefix(u.Path, "/") || strings.HasSuffix(b.Issuer, "/") {
		return fmt.Errorf("%w: issuer %q is not https://<host>/<path to the realm>", ErrInvalidBrokeredFrom, b.Issuer)
	}
	if len(b.Domain) > 253 || !hostnamePattern.MatchString(b.Domain) {
		return fmt.Errorf("%w: domain %q is not a domain", ErrInvalidBrokeredFrom, b.Domain)
	}
	if host := strings.ToLower(u.Hostname()); host != "id."+b.Domain {
		return fmt.Errorf("%w: domain %s is not the issuer's: the issuer's host is %s, and the domain is the one "+
			"it is published under (id.<domain>)", ErrInvalidBrokeredFrom, b.Domain, host)
	}
	return nil
}

// TenantBrokeredFrom reads a tenant's outside realms back.
func (g *GitOps) TenantBrokeredFrom(ctx context.Context, tenant string) ([]BrokeredFrom, error) {
	if !ValidName(tenant) {
		return nil, fmt.Errorf("%w: tenant %q", ErrInvalidName, tenant)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.brokeredFromLocked(ctx, tenant)
}

func (g *GitOps) brokeredFromLocked(ctx context.Context, tenant string) ([]BrokeredFrom, error) {
	manifest, err := g.tenantFileRead(ctx, tenant)
	if err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(filepath.Join(filepath.Dir(manifest), BrokeredFromFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var doc struct {
		Spec struct {
			BrokeredFrom []BrokeredFrom `json:"brokeredFrom"`
		} `json:"spec"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("read %s: %w", BrokeredFromFile, err)
	}
	return doc.Spec.BrokeredFrom, nil
}

// SetTenantBrokeredFrom adds an outside realm to a tenant, or replaces the
// one of that name: one commit of the patch.
//
// Refused: an entry ValidBrokeredFrom refuses; a domain on the kernel domain,
// whose addresses are the cluster's own people's; and a domain or a name as
// people read it that another entry of the tenant has -- Keycloak holds both
// unique in a realm, and would refuse the second organization after the
// commit, where nobody asked it.
func (g *GitOps) SetTenantBrokeredFrom(ctx context.Context, tenant string, entry BrokeredFrom, meta Meta) (Result, error) {
	if !ValidName(tenant) {
		return Result{}, fmt.Errorf("%w: tenant %q", ErrInvalidName, tenant)
	}
	entry.Domain = strings.ToLower(strings.TrimSpace(entry.Domain))
	if err := ValidBrokeredFrom(entry); err != nil {
		return Result{}, err
	}
	kernel, err := g.KernelDomain(ctx)
	if err != nil && !errors.Is(err, ErrNoClusterClaim) {
		return Result{}, err
	}
	if kernel != "" && (entry.Domain == kernel || strings.HasSuffix(entry.Domain, "."+kernel) ||
		strings.HasSuffix(kernel, "."+entry.Domain)) {
		return Result{}, fmt.Errorf("%w: %s is this cluster's domain or shares it, and its addresses are this cluster's people's",
			ErrInvalidBrokeredFrom, entry.Domain)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	list, err := g.brokeredFromLocked(ctx, tenant)
	if err != nil {
		return Result{}, err
	}
	out := make([]BrokeredFrom, 0, len(list)+1)
	for _, e := range list {
		if e.Name == entry.Name {
			continue
		}
		if e.Domain == entry.Domain {
			return Result{}, fmt.Errorf("%w: the domain %s is %s's", ErrBrokeredFromHeld, entry.Domain, e.Name)
		}
		if strings.EqualFold(e.DisplayName, entry.DisplayName) {
			return Result{}, fmt.Errorf("%w: the name %q is %s's", ErrBrokeredFromHeld, entry.DisplayName, e.Name)
		}
		out = append(out, e)
	}
	out = append(out, entry)
	return g.writeTenantFileLocked(ctx, tenant, BrokeredFromFile, renderBrokeredFrom(tenant, out), listPatch,
		fmt.Sprintf("Tenant %s: people of %s (%s) sign in as an organization", tenant, entry.Name, entry.Domain), meta)
}

// RemoveTenantBrokeredFrom takes an outside realm away: its people stop
// signing in here, and the organization goes with the identity provider.
// Removing one that is not there is unchanged.
func (g *GitOps) RemoveTenantBrokeredFrom(ctx context.Context, tenant, name string, meta Meta) (Result, error) {
	if !ValidName(tenant) {
		return Result{}, fmt.Errorf("%w: tenant %q", ErrInvalidName, tenant)
	}
	if !brokeredName.MatchString(name) {
		return Result{}, fmt.Errorf("%w: name %q is not a DNS label of up to 40 characters", ErrInvalidBrokeredFrom, name)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	list, err := g.brokeredFromLocked(ctx, tenant)
	if err != nil {
		return Result{}, err
	}
	out := make([]BrokeredFrom, 0, len(list))
	for _, e := range list {
		if e.Name != name {
			out = append(out, e)
		}
	}
	if len(out) == len(list) {
		return Result{Status: "unchanged"}, nil
	}
	body := ""
	if len(out) > 0 {
		body = renderBrokeredFrom(tenant, out)
	}
	return g.writeTenantFileLocked(ctx, tenant, BrokeredFromFile, body, listPatch,
		fmt.Sprintf("Tenant %s: people of %s no longer sign in", tenant, name), meta)
}

// renderBrokeredFrom writes the patch a reviewer reads in the commit, the
// entries in the order of their names.
func renderBrokeredFrom(tenant string, list []BrokeredFrom) string {
	sorted := append([]BrokeredFrom(nil), list...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })
	var b strings.Builder
	b.WriteString("# Managed by the director: realms outside this tenant whose people sign in\n")
	b.WriteString("# here, each as an organization of the tenant's realm. The composition makes\n")
	b.WriteString("# the organization and an identity provider, org-<name>, that authenticates\n")
	b.WriteString("# by a JWT this realm signs and gives nobody a group or a role.\n")
	b.WriteString("apiVersion: gentianos.io/v1alpha1\n")
	b.WriteString("kind: Tenant\n")
	b.WriteString("metadata:\n")
	b.WriteString("  name: " + tenant + "\n")
	b.WriteString("spec:\n")
	b.WriteString("  brokeredFrom:\n")
	for _, e := range sorted {
		b.WriteString("    - name: " + e.Name + "\n")
		b.WriteString("      displayName: " + strconv.Quote(e.DisplayName) + "\n")
		b.WriteString("      issuer: " + strconv.Quote(e.Issuer) + "\n")
		b.WriteString("      clientID: " + strconv.Quote(e.ClientID) + "\n")
		b.WriteString("      domain: " + e.Domain + "\n")
	}
	return b.String()
}
