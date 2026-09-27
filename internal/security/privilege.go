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

package security

import (
	"sort"
	"strings"
	"time"

	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// A privilege is a request until a person answers it (AD-5). The profile says
// what it would need; the Component carries the grants somebody gave; and the
// difference is what holds the install. This file is that arithmetic, and
// nothing else: it reads no cluster and decides no policy, so the reconciler,
// the director and a test all reach the same verdict from the same two objects.

// The three kinds, spelled as PrivilegeGrant.Privilege's pattern requires.
// Who approves follows from the kind: pod security and cluster roles weaken
// something that protects the node, so they are the platform operator's;
// egress leaves the tenant's own namespace and is the tenant's.
const (
	PrivilegePodSecurity  = "podSecurity"
	PrivilegeEgress       = "egress"
	PrivilegeClusterRoles = "clusterRoles"
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
// the platform operator, "tenant" for the tenant's own approver. It is
// derived from the kind and never read from a field -- a profile that could
// name its approver would name the cheaper one.
func PrivilegeScope(kind string) string {
	if kind == PrivilegeEgress {
		return "tenant"
	}
	return "cluster"
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
	out := map[string]struct{}{}
	for i := range comp.Spec.Privileges {
		g := &comp.Spec.Privileges[i]
		if g.ExpiresAt != nil && !now.Before(g.ExpiresAt.Time) {
			continue
		}
		out[g.Privilege] = struct{}{}
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
	requested := RequestedPrivileges(profile)
	if len(requested) == 0 {
		return nil
	}
	granted := GrantedPrivileges(comp, now)
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
	granted := GrantedPrivileges(comp, now)
	var out []networkingv1.NetworkPolicyEgressRule
	for i := range profile.Spec.Requires.Privileges.Egress {
		e := &profile.Spec.Requires.Privileges.Egress[i]
		if _, ok := granted[PrivilegeRef(PrivilegeEgress, e.Name)]; ok {
			out = append(out, e.Rule)
		}
	}
	return out
}

// GrantedPodSecurityWaivers are the waivers a person granted, in the
// profile's order.
func GrantedPodSecurityWaivers(profile *gentianov1alpha1.ComponentProfile, comp *gentianov1alpha1.Component, now time.Time) []gentianov1alpha1.PodSecurityWaiver {
	if profile == nil || profile.Spec.Requires == nil || profile.Spec.Requires.Privileges == nil {
		return nil
	}
	granted := GrantedPrivileges(comp, now)
	var out []gentianov1alpha1.PodSecurityWaiver
	for i := range profile.Spec.Requires.Privileges.PodSecurity {
		w := &profile.Spec.Requires.Privileges.PodSecurity[i]
		if _, ok := granted[PrivilegeRef(PrivilegePodSecurity, w.Name)]; ok {
			out = append(out, *w)
		}
	}
	return out
}

// GrantedClusterRoleRules are the API-access rules a person granted, in the
// profile's order.
func GrantedClusterRoleRules(profile *gentianov1alpha1.ComponentProfile, comp *gentianov1alpha1.Component, now time.Time) []rbacv1.PolicyRule {
	if profile == nil || profile.Spec.Requires == nil || profile.Spec.Requires.Privileges == nil {
		return nil
	}
	granted := GrantedPrivileges(comp, now)
	var out []rbacv1.PolicyRule
	for i := range profile.Spec.Requires.Privileges.ClusterRoles {
		c := &profile.Spec.Requires.Privileges.ClusterRoles[i]
		if _, ok := granted[PrivilegeRef(PrivilegeClusterRoles, c.Name)]; ok {
			out = append(out, c.Rules...)
		}
	}
	return out
}

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
	}
	return ""
}
