/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package controller

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"regexp"
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// Partners (Tenant spec.partners, docs/guides/identity-brokering.md,
// AD-17): realms of other clusters a tenant's realm links its people's
// accounts with. The operator decides which entries reach the composite --
// the tenant's own and the Cluster claim's defaults, within the claim's
// allowedIssuers -- and fills in what the composition must not guess: the
// name the partner gives this realm (as, default the tenant's uid) and the
// salt of the pairwise subject.
//
// The names below are what the tenant's Composition and the component have
// to agree on, and are derived in one place.

// PartnerClientID is a partner app's client in its tenant's realm: the one it
// exchanges a person's token at (issuing) and presents assertions with
// (receiving), authenticated by its service account token.
func PartnerClientID(profile string) string { return "partner-" + profile }

// PartnerSecretName is the Secret of the tenant's namespace a partner app is
// told its client, token endpoint and token audience in.
func PartnerSecretName(profile string) string { return "partners-" + profile }

// partnerProfilesKey is the zone projection's line per partner app
// (zone_hosts_projection.go): "<profile> <service account> <call> <accept>",
// call a comma-separated list or "-", accept "1" or "0".
const partnerProfilesKey = "partnerProfiles"

// partnerServiceAccountTokenPath is where the operator reads the issuer of
// the cluster's service account tokens from: its own token names it.
var partnerServiceAccountTokenPath = "/var/run/secrets/kubernetes.io/serviceaccount/token"

// ServiceAccountIssuer is the iss of the operator's own service account
// token, or empty when there is none to read (outside a cluster). The realm
// trusts that issuer for its partner apps' clients.
func ServiceAccountIssuer() string {
	raw, err := os.ReadFile(partnerServiceAccountTokenPath)
	if err != nil {
		return ""
	}
	parts := strings.Split(strings.TrimSpace(string(raw)), ".")
	if len(parts) != 3 {
		return ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var claims struct {
		Iss string `json:"iss"`
	}
	if json.Unmarshal(payload, &claims) != nil || !strings.HasPrefix(claims.Iss, "https://") {
		return ""
	}
	return claims.Iss
}

// wantsPartners reports whether a profile calls partners or accepts their
// assertions: a component that needs the client partner-<profile>.
func wantsPartners(profile *gentianov1alpha1.ComponentProfile) bool {
	services := profile.Services()
	return services != nil && services.Partners != nil &&
		(len(services.Partners.Call) > 0 || services.Partners.Accept)
}

// wantsPartnerCommission reports whether a profile commissions partners of
// the tenants that name it.
func wantsPartnerCommission(profile *gentianov1alpha1.ComponentProfile) bool {
	services := profile.Services()
	return services != nil && services.Partners != nil && services.Partners.Commission
}

// partnerLine is a partner app's entry in the zone projection; only called
// for a profile wantsPartners says yes to.
func partnerLine(profile *gentianov1alpha1.ComponentProfile) string {
	p := profile.Services().Partners
	sa := p.ServiceAccount
	if sa == "" {
		sa = profile.Name
	}
	call := "-"
	if len(p.Call) > 0 {
		names := append([]string(nil), p.Call...)
		sort.Strings(names)
		call = strings.Join(names, ",")
	}
	accept := "0"
	if p.Accept {
		accept = "1"
	}
	return strings.Join([]string{profile.Name, sa, call, accept}, " ")
}

// clusterPartners is the Cluster claim's spec.partners.
type clusterPartners struct {
	AllowedIssuers []string `json:"allowedIssuers,omitempty"`
	Defaults       []struct {
		Name   string `json:"name"`
		Issuer string `json:"issuer"`
	} `json:"defaults,omitempty"`
}

// readClusterPartners reads spec.partners of the cluster's claim. No claim,
// or one without the block: no issuer allowed and no default.
func readClusterPartners(ctx context.Context, c client.Reader) (clusterPartners, error) {
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(schema.GroupVersionKind{
		Group: clusterClaimGVK.Group, Version: clusterClaimGVK.Version, Kind: clusterClaimGVK.Kind + "List",
	})
	if err := c.List(ctx, list, client.InNamespace(clusterConfigNamespace)); err != nil {
		if meta.IsNoMatchError(err) {
			// The claim's kind is not installed: no claim, nothing allowed.
			return clusterPartners{}, nil
		}
		return clusterPartners{}, err
	}
	sort.Slice(list.Items, func(a, b int) bool { return list.Items[a].GetName() < list.Items[b].GetName() })
	for i := range list.Items {
		return claimPartners(&list.Items[i]), nil
	}
	return clusterPartners{}, nil
}

func claimPartners(u *unstructured.Unstructured) clusterPartners {
	var out clusterPartners
	raw, found, _ := unstructured.NestedMap(u.Object, "spec", "partners")
	if !found {
		return out
	}
	b, err := json.Marshal(raw)
	if err == nil {
		_ = json.Unmarshal(b, &out)
	}
	return out
}

// IssuerAllowed reports whether an issuer matches one of the patterns, where
// "*" matches any run of characters and everything else itself.
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

// partnerSalt is the salt of a partner's pairwise subject: fixed for the
// tenant and the partner, so the subject a partner stored at linking and the
// one in every later assertion agree, and different for every tenant and
// partner. Keycloak would make up one of its own when none is given, and
// replace it whenever the mapper is written again.
func partnerSalt(tenant *gentianov1alpha1.Tenant, name string) string {
	sum := sha256.Sum256([]byte(string(tenant.UID) + "/" + name))
	return hex.EncodeToString(sum[:16])
}

// tenantPartnerProjection is the composite's spec.partners: the claim's
// defaults, replaced by the tenant's own entries of the same name, those
// whose issuer the claim allows, with as and salt filled in. A default is
// allowed by being declared. Nothing while the tenant has no uid yet: the
// default as and the salt are derived from it.
func tenantPartnerProjection(tenant *gentianov1alpha1.Tenant, claim clusterPartners) []interface{} {
	if tenant.UID == "" {
		return nil
	}
	entries := map[string]gentianov1alpha1.TenantPartner{}
	allowed := map[string]bool{}
	for _, d := range claim.Defaults {
		if d.Name == "" || !strings.HasPrefix(d.Issuer, "https://") {
			continue
		}
		entries[d.Name] = gentianov1alpha1.TenantPartner{Name: d.Name, Issuer: d.Issuer, Issue: true}
		allowed[d.Name] = true
	}
	for _, p := range tenant.Spec.Partners {
		entries[p.Name] = p
		allowed[p.Name] = IssuerAllowed(claim.AllowedIssuers, strings.TrimSuffix(p.Issuer, "/")) ||
			isDefaultIssuer(claim, p.Issuer)
	}
	names := make([]string, 0, len(entries))
	for n := range entries {
		names = append(names, n)
	}
	sort.Strings(names)
	var out []interface{}
	for _, n := range names {
		p := entries[n]
		if !allowed[n] || (!p.Issue && p.Accept == nil) {
			continue
		}
		as := p.As
		if as == "" {
			as = string(tenant.UID)
		}
		entry := map[string]interface{}{
			"name":   p.Name,
			"issuer": strings.TrimSuffix(p.Issuer, "/"),
			"as":     as,
			"salt":   partnerSalt(tenant, p.Name),
		}
		if p.Issue {
			entry["issue"] = true
		}
		if a := p.Accept; a != nil {
			accept := map[string]interface{}{"accounts": string(a.Accounts)}
			if a.Accounts == gentianov1alpha1.PartnerAccountsOrganization {
				display := a.DisplayName
				if display == "" {
					display = p.Name
				}
				accept["domain"] = a.Domain
				accept["displayName"] = display
			}
			entry["accept"] = accept
		}
		out = append(out, entry)
	}
	return out
}

func isDefaultIssuer(claim clusterPartners, issuer string) bool {
	for _, d := range claim.Defaults {
		if strings.TrimSuffix(d.Issuer, "/") == strings.TrimSuffix(issuer, "/") {
			return true
		}
	}
	return false
}

// claimTenantFactsChanged lets a claim event through only when it can change
// what a tenant's composite says: the store's address, or the partners'
// bounds and defaults.
func claimTenantFactsChanged() predicate.Predicate {
	facts := func(obj client.Object) string {
		u, ok := obj.(*unstructured.Unstructured)
		if !ok {
			return ""
		}
		raw, _, _ := unstructured.NestedString(u.Object, "spec", "catalogue", "storeUrl")
		b, _ := json.Marshal(claimPartners(u))
		return normaliseStoreAddress(raw) + "\n" + string(b)
	}
	return predicate.Funcs{
		CreateFunc:  func(event.CreateEvent) bool { return true },
		DeleteFunc:  func(event.DeleteEvent) bool { return true },
		GenericFunc: func(event.GenericEvent) bool { return false },
		UpdateFunc:  func(e event.UpdateEvent) bool { return facts(e.ObjectOld) != facts(e.ObjectNew) },
	}
}
