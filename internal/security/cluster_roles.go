/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package security

import (
	"time"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// The cluster roles a profile may ask for.
//
// A profile asks for a role by name and never states its rules: a catalogue
// entry that could write the rules of a ClusterRole would decide what it may
// do on every node and in every tenant. The set is the platform's, kept in
// this repository in two places that a test holds together: the names here,
// and the ClusterRole objects the operator's chart ships
// (charts/gentian-os/templates/profile-cluster-roles.yaml).
//
// A role belongs here only if it is narrow and reads: no Secrets, no write
// verb, no wildcard, nothing of RBAC or admission, no nodes/proxy, no
// impersonation.

// ClusterRoleSet is the platform's roles: name, and what the role allows in
// words an approver reads.
type ClusterRoleSet map[string]string

// PlatformClusterRoles is the set this build of the platform ships. Empty: no
// profile of the catalogue asks for a cluster role, so there is none to
// define yet. docs/design/security.md says how one is added.
var PlatformClusterRoles = ClusterRoleSet{}

// ClusterRoleObjectPrefix starts the name of every ClusterRole of the set, so
// that the operator's permission to bind can be held to these names.
const ClusterRoleObjectPrefix = "gentian-profile-role-"

// ClusterRoleNameLabel is on every ClusterRole of the set, with the name a
// profile asks for.
const ClusterRoleNameLabel = "gentianos.io/profile-cluster-role"

// ClusterRoleObjectName is the ClusterRole a name of the set stands for.
func ClusterRoleObjectName(name string) string { return ClusterRoleObjectPrefix + name }

// Why a requested cluster role is not bound. The order is the order they are
// asked in: what the request is, what the platform has, what the cluster
// permits, what was granted for this install.
const (
	ClusterRoleBound            = "Bound"
	ClusterRoleFreeFormRules    = "FreeFormRules"
	ClusterRoleUnknown          = "UnknownRole"
	ClusterRoleNoServiceAccount = "NoServiceAccount"
	ClusterRoleNotAllowed       = "NotAllowed"
	ClusterRoleNotGranted       = "NotGranted"
)

// ClusterRoleVerdict is the answer to one request of a profile.
type ClusterRoleVerdict struct {
	// Name is the request's name.
	Name string
	// ServiceAccount is the account the role is bound to when Bind is true.
	ServiceAccount string
	// Bind is true only when every condition holds.
	Bind bool
	// Reason is one of the constants above.
	Reason string
	// Message says it in words, for the Component's status.
	Message string
}

// ResolveClusterRoles answers every cluster-role request of a profile.
//
// A role is bound only when all of these hold, and each is necessary:
//
//   - the request states no rules of its own and names a ServiceAccount other
//     than "default";
//   - its name is in the platform's set;
//   - the cluster's allowlist permits that role for this profile;
//   - a live grant for clusterRoles/<name> is on the install.
//
// It reads no cluster, so the reconciler and a test reach the same verdict.
func ResolveClusterRoles(
	profile *gentianov1alpha1.ComponentProfile,
	set ClusterRoleSet,
	allowed []gentianov1alpha1.AllowedClusterRole,
	grants []gentianov1alpha1.PrivilegeGrant,
	now time.Time,
) []ClusterRoleVerdict {
	if profile == nil || profile.Privileges() == nil {
		return nil
	}
	granted := GrantedSet(grants, now)
	permitted := map[string]struct{}{}
	for _, a := range allowed {
		if a.Profile == profile.Name {
			permitted[a.Role] = struct{}{}
		}
	}
	requests := profile.Privileges().ClusterRoles
	out := make([]ClusterRoleVerdict, 0, len(requests))
	for i := range requests {
		req := &requests[i]
		v := ClusterRoleVerdict{Name: req.Name, ServiceAccount: req.ServiceAccount}
		_, inSet := set[req.Name]
		_, isPermitted := permitted[req.Name]
		_, isGranted := granted[PrivilegeRef(PrivilegeClusterRoles, req.Name)]
		switch {
		case len(req.Rules) > 0:
			v.Reason = ClusterRoleFreeFormRules
			v.Message = req.Name + ": the profile states rules of its own, and no role is made from a profile's rules"
		case !inSet:
			v.Reason = ClusterRoleUnknown
			v.Message = req.Name + ": not one of the cluster roles the platform defines"
		case req.ServiceAccount == "" || req.ServiceAccount == "default":
			v.Reason = ClusterRoleNoServiceAccount
			v.Message = req.Name + ": the profile names no ServiceAccount of the component's own to bind it to"
		case !isPermitted:
			v.Reason = ClusterRoleNotAllowed
			v.Message = req.Name + ": this cluster's PlatformSecurityPolicy does not permit it for " + profile.Name
		case !isGranted:
			v.Reason = ClusterRoleNotGranted
			v.Message = req.Name + ": not granted on this install, or the grant has expired"
		default:
			v.Bind = true
			v.Reason = ClusterRoleBound
			v.Message = req.Name + ": bound to ServiceAccount " + req.ServiceAccount
		}
		out = append(out, v)
	}
	return out
}
