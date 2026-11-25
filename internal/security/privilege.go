/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package security

import (
	"sort"
	"strings"
	"time"

	networkingv1 "k8s.io/api/networking/v1"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// A privilege is a request until a person answers it (AD-5). The profile says
// what it would need; the Component carries the grants somebody gave; and the
// difference is what holds the install. This file is that arithmetic, and
// nothing else: it reads no cluster and decides no policy, so the reconciler,
// the director and a test all reach the same verdict from the same two objects.

// The four kinds, spelled as PrivilegeGrant.Privilege's pattern requires.
// Who approves follows from the kind: pod security and cluster roles weaken
// something that protects the node, so they are the platform operator's;
// egress leaves the tenant's own namespace and is the tenant's; an API
// extension changes the cluster's API for every tenant, and is break-glass's.
const (
	PrivilegePodSecurity   = "podSecurity"
	PrivilegeEgress        = "egress"
	PrivilegeClusterRoles  = "clusterRoles"
	PrivilegeAPIExtensions = "apiExtensions"
)

// PrivilegeRef is the <kind>/<name> a grant names. Built here rather than
// spelled at each call site, because the pattern on PrivilegeGrant.Privilege
// rejects anything else and a mismatch would be a grant that can never apply.
func PrivilegeRef(kind, name string) string { return kind + "/" + name }

// SplitPrivilegeRef undoes PrivilegeRef. A ref that is not <kind>/<name>
// yields empty strings, which every caller treats as "matches nothing".
func SplitPrivilegeRef(ref string) (kind, name string) {
	i := strings.IndexByte(ref, '/')
	if i <= 0 || i == len(ref)-1 {
		return "", ""
	}
	return ref[:i], ref[i+1:]
}

// PrivilegeScope says who may answer a request of this kind: "cluster" for
// the platform operator, "tenant" for the tenant's perimeter approver. It is
// derived from the kind and never read from a field -- a profile that could
// name its approver would name the cheaper one.
func PrivilegeScope(kind string) string {
	if kind == PrivilegeEgress {
		return "tenant"
	}
	return "cluster"
}

// Who answers a request, as a console names the person: the tenant's
// perimeter approver (tenant#can_approve_privilege), the cluster's security
// officer (cluster#can_approve), or break-glass (cluster#can_set_admission).
const (
	PrivilegeApproverPerimeter       = "perimeter-approver"
	PrivilegeApproverSecurityOfficer = "security-officer"
	PrivilegeApproverBreakGlass      = "break-glass"
)

// PrivilegeApprover says who answers the request ref (<kind>/<name>), in the
// words a console shows, so that a console asks which relation a request
// needs instead of deciding it again. The director's authorization of an
// approval (mayApprove) reads the same function, so what a console offers and
// what the director allows cannot drift apart.
//
// It follows PrivilegeScope, with one step up at cluster scope: what lets a
// component write through the Kubernetes API -- a type of its own, or a role
// of the set that writes -- is break-glass's, as the cluster's admission is.
// A ref that is not one yields the security officer, the strictest of the
// ordinary answers.
func PrivilegeApprover(ref string) string {
	kind, name := SplitPrivilegeRef(ref)
	switch {
	case PrivilegeScope(kind) == "tenant":
		return PrivilegeApproverPerimeter
	case kind == PrivilegeAPIExtensions:
		return PrivilegeApproverBreakGlass
	case kind == PrivilegeClusterRoles && PlatformClusterRoles[name].Writes:
		return PrivilegeApproverBreakGlass
	}
	return PrivilegeApproverSecurityOfficer
}

// RequestedPrivileges lists every privilege a profile asks for, as
// <kind>/<name>, sorted so that a status field and a console list do not
// reorder between reconciles for no reason.
func RequestedPrivileges(profile *gentianov1alpha1.ComponentProfile) []string {
	if profile == nil || profile.Spec.Requires == nil || profile.Spec.Requires.Privileges == nil {
		return nil
	}
	p := profile.Spec.Requires.Privileges
	var out []string
	for i := range p.PodSecurity {
		out = append(out, PrivilegeRef(PrivilegePodSecurity, p.PodSecurity[i].Name))
	}
	for i := range p.Egress {
		out = append(out, PrivilegeRef(PrivilegeEgress, p.Egress[i].Name))
	}
	for i := range p.ClusterRoles {
		out = append(out, PrivilegeRef(PrivilegeClusterRoles, p.ClusterRoles[i].Name))
	}
	for i := range p.APIExtensions {
		out = append(out, PrivilegeRef(PrivilegeAPIExtensions, p.APIExtensions[i].Name))
	}
	sort.Strings(out)
	return out
}

// GrantedPrivileges is the set of refs the Component carries a live grant
// for. A grant whose ExpiresAt has passed is not a grant: the whole point of
// bounding one is that it stops applying by itself, so an expiry turns the
// request back into a pending one and the install waits again rather than
// carrying on with a privilege nobody has renewed.
func GrantedPrivileges(comp *gentianov1alpha1.Component, now time.Time) map[string]struct{} {
	if comp == nil {
		return nil
	}
	return GrantedSet(comp.Spec.Privileges, now)
}

// GrantedSet is the same arithmetic over a bare list of grants, for a caller
// that has the grants without a Component to hang them on.
//
// The tenant's own netpolicy is built from Tenant.spec.apps and the profiles
// they name, before any Component exists for them, and it must not open
// outbound network for a privilege nobody answered. So it reads the grants
// the Tenant carries and asks this the same question the reconciler does.
func GrantedSet(grants []gentianov1alpha1.PrivilegeGrant, now time.Time) map[string]struct{} {
	out := map[string]struct{}{}
	for i := range grants {
		g := &grants[i]
		if g.ExpiresAt != nil && !now.Before(g.ExpiresAt.Time) {
			continue
		}
		out[g.Privilege] = struct{}{}
	}
	return out
}

// TenantGrants are the grants a Tenant carries for one install.
func TenantGrants(tenant *gentianov1alpha1.Tenant, install string) []gentianov1alpha1.PrivilegeGrant {
	if tenant == nil {
		return nil
	}
	var out []gentianov1alpha1.PrivilegeGrant
	for i := range tenant.Spec.Privileges {
		if tenant.Spec.Privileges[i].Install == install {
			out = append(out, tenant.Spec.Privileges[i].Grant())
		}
	}
	return out
}

// PendingPrivileges is what the profile asks for and no live grant answers,
// in RequestedPrivileges' order. While this is non-empty the install waits.
//
// A grant for something the profile no longer requests is simply not counted
// here and not applied anywhere: an uninstalled request cannot be dangerous,
// and deleting the grant would destroy the record that somebody once said yes.
func PendingPrivileges(profile *gentianov1alpha1.ComponentProfile, comp *gentianov1alpha1.Component, now time.Time) []string {
	return PendingPrivilegesUnder(profile, comp, now, nil)
}

// PendingPrivilegesUnder is PendingPrivileges with the requests the cluster's
// policy answers (trusted domains, a profile the platform ships) counted
// as granted.
func PendingPrivilegesUnder(profile *gentianov1alpha1.ComponentProfile, comp *gentianov1alpha1.Component, now time.Time, policy map[string]struct{}) []string {
	requested := RequestedPrivileges(profile)
	if len(requested) == 0 {
		return nil
	}
	var grants []gentianov1alpha1.PrivilegeGrant
	if comp != nil {
		grants = comp.Spec.Privileges
	}
	granted := LiveGrants(profile, grants, now)
	for ref := range policy {
		granted[ref] = struct{}{}
	}
	var out []string
	for _, ref := range requested {
		if _, ok := granted[ref]; !ok {
			out = append(out, ref)
		}
	}
	return out
}

// GrantedEgressRules are the egress rules of the privileges a person actually
// granted, in the profile's own order. This is the function that makes a
// grant mean something: until it was written, requires.privileges.egress had
// no reader at all and an app's declared outbound access was either applied
// to everyone or to nobody.
func GrantedEgressRules(profile *gentianov1alpha1.ComponentProfile, comp *gentianov1alpha1.Component, now time.Time) []networkingv1.NetworkPolicyEgressRule {
	if profile == nil || profile.Spec.Requires == nil || profile.Spec.Requires.Privileges == nil {
		return nil
	}
	return GrantedEgressRulesUnder(profile, comp, now, nil)
}

// GrantedEgressRulesUnder is GrantedEgressRules with the requests the
// cluster's policy answers (trusted domains, a profile the platform ships)
// counted as granted: GrantedEgress's rules.
func GrantedEgressRulesUnder(profile *gentianov1alpha1.ComponentProfile, comp *gentianov1alpha1.Component, now time.Time, policy map[string]struct{}) []networkingv1.NetworkPolicyEgressRule {
	return GrantedEgress(profile, componentGrants(comp), now, policy).Rules
}

// GrantedEgressHostsUnder are the hosts outside the cluster a component was
// granted: those of its egress requests by host that a person answered or
// the cluster's policy answers, and, for a request whose hosts are named at
// approval, those its live grant names. GrantedEgress's hosts.
func GrantedEgressHostsUnder(profile *gentianov1alpha1.ComponentProfile, comp *gentianov1alpha1.Component, now time.Time, policy map[string]struct{}) []string {
	return GrantedEgress(profile, componentGrants(comp), now, policy).Hosts
}

// componentGrants are the grants comp carries, none for no component.
func componentGrants(comp *gentianov1alpha1.Component) []gentianov1alpha1.PrivilegeGrant {
	if comp == nil {
		return nil
	}
	return comp.Spec.Privileges
}

// TrustedEgress are the egress requests of profile that trusted domains
// answer: each request by host whose every host is in one of domains, or
// under one. A request with one host outside them is answered by a person,
// who then sees all of its hosts. A request by address is never answered
// here: an address is in no domain.
func TrustedEgress(profile *gentianov1alpha1.ComponentProfile, domains []string) map[string]struct{} {
	if profile == nil || len(domains) == 0 || profile.Privileges() == nil {
		return nil
	}
	var out map[string]struct{}
	for i := range profile.Privileges().Egress {
		e := &profile.Privileges().Egress[i]
		if len(e.Hosts) == 0 {
			continue
		}
		all := true
		for _, h := range e.Hosts {
			if !InTrustedDomain(h, domains) {
				all = false
				break
			}
		}
		if all {
			if out == nil {
				out = map[string]struct{}{}
			}
			out[PrivilegeRef(PrivilegeEgress, e.Name)] = struct{}{}
		}
	}
	return out
}

// InTrustedDomain says whether host, as an egress request names it (a name
// with an optional port), is one of domains or a name under one. A domain
// covers whole labels: "example.org" covers "api.example.org" and not
// "notexample.org".
func InTrustedDomain(host string, domains []string) bool {
	name := host
	if i := strings.LastIndexByte(name, ':'); i >= 0 {
		name = name[:i]
	}
	for _, d := range domains {
		if d != "" && (name == d || strings.HasSuffix(name, "."+d)) {
			return true
		}
	}
	return false
}

// TenantTrustedDomains are the domains trusted for a tenant alone: its own
// list while the cluster's administrator delegated one, none otherwise.
func TenantTrustedDomains(tenant *gentianov1alpha1.Tenant) []string {
	if tenant == nil || tenant.Spec.TrustedDomains == nil || !tenant.Spec.TrustedDomains.Delegated {
		return nil
	}
	return tenant.Spec.TrustedDomains.Domains
}

// OriginAnnotation is where a materialized profile records the catalog it
// was fetched from (profilebundle.OriginAnnotation, which the director
// writes and a catalog source never states).
const OriginAnnotation = "gentianos.io/catalogue-origin"

// PrivilegeReason is what the profile said it needs the privilege for, which
// is what an approver reads before deciding. Empty when the ref is not one of
// the profile's requests.
func PrivilegeReason(profile *gentianov1alpha1.ComponentProfile, ref string) string {
	if profile == nil || profile.Spec.Requires == nil || profile.Spec.Requires.Privileges == nil {
		return ""
	}
	kind, name := SplitPrivilegeRef(ref)
	p := profile.Spec.Requires.Privileges
	switch kind {
	case PrivilegePodSecurity:
		for i := range p.PodSecurity {
			if p.PodSecurity[i].Name == name {
				return p.PodSecurity[i].Reason
			}
		}
	case PrivilegeEgress:
		for i := range p.Egress {
			if p.Egress[i].Name == name {
				return p.Egress[i].Reason
			}
		}
	case PrivilegeClusterRoles:
		for i := range p.ClusterRoles {
			if p.ClusterRoles[i].Name == name {
				return p.ClusterRoles[i].Reason
			}
		}
	case PrivilegeAPIExtensions:
		for i := range p.APIExtensions {
			if p.APIExtensions[i].Name == name {
				return p.APIExtensions[i].Reason
			}
		}
	}
	return ""
}

// WaiverRequests are a profile's pod-security privileges in the shape the
// PlatformSecurityPolicy allowlist is written in.
//
// The allowlist is a cluster-wide statement about what may EVER be waived
// here, keyed on policy and scope; a privilege adds a name and a reason for
// the person who approves one install. Both questions are asked, and this is
// what lets the coarse one keep its own vocabulary.
func WaiverRequests(privileges *gentianov1alpha1.PrivilegeRequest) []gentianov1alpha1.MacWaiverRequest {
	if privileges == nil {
		return nil
	}
	out := make([]gentianov1alpha1.MacWaiverRequest, 0, len(privileges.PodSecurity))
	for i := range privileges.PodSecurity {
		out = append(out, gentianov1alpha1.MacWaiverRequest{
			Policy: privileges.PodSecurity[i].Policy,
			Scope:  privileges.PodSecurity[i].Scope,
		})
	}
	return out
}
