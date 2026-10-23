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
	Domain      string `json:"domain,omitempty"`
	// LinkExisting is an entry with no organization and no domain, whose
	// provider links an account that exists and makes none (Tenant
	// spec.brokeredFrom[].linkExisting).
	LinkExisting bool `json:"linkExisting,omitempty"`
	// CreateAccounts, with LinkExisting: the provider makes the account when
	// none has the person's address (Tenant
	// spec.brokeredFrom[].createAccounts).
	CreateAccounts bool `json:"createAccounts,omitempty"`
}

// ErrInvalidBrokeredFrom is an entry that cannot be one.
var ErrInvalidBrokeredFrom = errors.New("invalid outside realm")

// ErrBrokeredFromHeld is an entry whose domain or name as people read it
// another entry of the tenant already has.
var ErrBrokeredFromHeld = errors.New("held by another outside realm of this tenant")

var brokeredClientID = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

// ValidBrokeredFrom checks one entry as the CRD would, and one rule the CRD
// cannot state: the domain is under the cluster the issuer is published by.
//
// An organization's domain decides where Keycloak sends a person who gives
// an address of it. So it is not the commissioner's to choose freely. A
// Gentian cluster publishes every realm at id.<its domain>, and its tenants'
// people have addresses of that domain (a single-tenancy cluster) or of
// <tenant>.<that domain>: the domain must be the issuer's cluster domain or
// one below it. An outside realm cannot claim the addresses of a domain its
// cluster does not serve.
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
	host := strings.ToLower(u.Hostname())
	cluster, ok := strings.CutPrefix(host, "id.")
	if !ok || cluster == "" {
		return fmt.Errorf("%w: issuer %q is not at id.<the cluster's domain>", ErrInvalidBrokeredFrom, b.Issuer)
	}
	if b.CreateAccounts && !b.LinkExisting {
		return fmt.Errorf("%w: createAccounts is for an entry that links existing accounts", ErrInvalidBrokeredFrom)
	}
	if b.LinkExisting {
		// No organization, so no domain whose addresses it would claim.
		if b.Domain != "" {
			return fmt.Errorf("%w: an entry that links existing accounts makes no organization and names no domain", ErrInvalidBrokeredFrom)
		}
		return nil
	}
	if len(b.Domain) > 253 || !hostnamePattern.MatchString(b.Domain) {
		return fmt.Errorf("%w: domain %q is not a domain", ErrInvalidBrokeredFrom, b.Domain)
	}
	if b.Domain != cluster && !strings.HasSuffix(b.Domain, "."+cluster) {
		return fmt.Errorf("%w: domain %s is not the issuer's cluster's: the issuer is published by %s, and the "+
			"domain is that or one below it", ErrInvalidBrokeredFrom, b.Domain, cluster)
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
	if kernel != "" && entry.Domain != "" && (entry.Domain == kernel || strings.HasSuffix(entry.Domain, "."+kernel) ||
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
		if e.Domain != "" && e.Domain == entry.Domain {
			return Result{}, fmt.Errorf("%w: the domain %s is %s's", ErrBrokeredFromHeld, entry.Domain, e.Name)
		}
		if strings.EqualFold(e.DisplayName, entry.DisplayName) {
			return Result{}, fmt.Errorf("%w: the name %q is %s's", ErrBrokeredFromHeld, entry.DisplayName, e.Name)
		}
		out = append(out, e)
	}
	out = append(out, entry)
	return g.writeTenantFileLocked(ctx, tenant, BrokeredFromFile, renderBrokeredFrom(tenant, out), listPatch,
		brokeredFromSubject(tenant, entry), meta)
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
	b.WriteString("# by a JWT this realm signs and gives nobody a group or a role. An entry\n")
	b.WriteString("# with linkExisting is no organization: its provider links an account\n")
	b.WriteString("# that exists here, once, and makes none -- or, with createAccounts, makes\n")
	b.WriteString("# one when none has the person's address.\n")
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
		if e.LinkExisting {
			b.WriteString("      linkExisting: true\n")
			if e.CreateAccounts {
				b.WriteString("      createAccounts: true\n")
			}
		} else {
			b.WriteString("      domain: " + e.Domain + "\n")
		}
	}
	return b.String()
}

func brokeredFromSubject(tenant string, entry BrokeredFrom) string {
	if entry.LinkExisting {
		return fmt.Sprintf("Tenant %s: people of %s sign in to their accounts here", tenant, entry.Name)
	}
	return fmt.Sprintf("Tenant %s: people of %s (%s) sign in as an organization", tenant, entry.Name, entry.Domain)
}
