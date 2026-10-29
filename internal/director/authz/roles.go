/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package authz

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// ClusterRoles are the cluster's role relations: admin and break_glass, held
// by the Keycloak group the claim names for each, and the ones a role holder
// holds by name. The names are the model's relations on type cluster;
// anything else is refused rather than written, because a tuple naming a
// relation the model does not have is accepted by OpenFGA and then matches
// nothing, which is indistinguishable from a permission that was never granted.
var ClusterRoles = []string{
	"admin",
	"security_officer",
	"auditor",
	"service_admin",
	"shared_apps_admin",
	"perimeter_approver",
	"break_glass",
}

func knownClusterRole(rel string) bool {
	for _, r := range ClusterRoles {
		if r == rel {
			return true
		}
	}
	return false
}

// ClaimRoles maps each field of the Cluster claim's spec.platformRoles, as an
// operator writes it, to the cluster relation it grants: the two roles a
// Keycloak group of the kernel realm holds. The claim's spelling and the
// model's are kept apart on purpose, and this is the one place they meet:
// the operator's projection and the director's reading of the claim both
// translate through it, so the two cannot drift into projecting one set of
// roles and checking another.
var ClaimRoles = map[string]string{
	"admin":      "admin",
	"breakGlass": "break_glass",
}

// RolesFromClaim translates spec.platformRoles of the Cluster claim into the
// relation-keyed map ReconcileClusterRoles takes. A field with an empty group
// grants nothing and is left out.
//
// A field the mapping does not know is returned in unknown, sorted, rather
// than passed through: ReconcileClusterRoles would refuse the whole map for
// it, and the caller decides whether that is an error or something to log
// and project around.
func RolesFromClaim(claim map[string]string) (roles map[string]string, unknown []string) {
	roles = map[string]string{}
	for field, group := range claim {
		rel, ok := ClaimRoles[field]
		if !ok {
			unknown = append(unknown, field)
			continue
		}
		if group != "" {
			roles[rel] = group
		}
	}
	sort.Strings(unknown)
	return roles, unknown
}

// HolderRoles are the roles the Cluster claim gives a person by name
// (spec.platformAccess.roleHolders), keyed by the claim's spelling, each with
// the relation it grants. Never admin or break_glass: those stay with the
// kernel realm's groups. auditor and tenantAuditor grant the relation of one
// name on the cluster and on one tenant.
var HolderRoles = map[string]string{
	"securityOfficer":   "security_officer",
	"auditor":           "auditor",
	"serviceAdmin":      "service_admin",
	"sharedAppsAdmin":   "shared_apps_admin",
	"perimeterApprover": "perimeter_approver",
	"tenantAuditor":     "auditor",
}

// ScopeCluster is a role holder's scope when the role is held over the
// cluster; a perimeter approver or auditor of one tenant has "tenant:<t>"
// instead.
const ScopeCluster = "cluster"

// tenantName is a tenant's name as a scope may name it: a DNS label.
var tenantName = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)

// HolderScope is the scope a role holds for a person of tenant, or ok false
// when that tenant's people may not hold the role:
//
//	securityOfficer, auditor           cluster, for anybody who may be named
//	serviceAdmin, sharedAppsAdmin      cluster, for a person of the platform tenant only
//	perimeterApprover                  cluster for a person of the platform tenant,
//	                                   tenant:<t> for an administrator of tenant t
//	tenantAuditor                      none: its scope is the entry's (EntryScope)
//
// The scope follows from the role and the tenant and is never chosen: the
// claim states it so that a person reading it sees how far the role
// reaches, and an entry stating another is refused.
func HolderScope(role, tenant string) (scope string, ok bool) {
	platform := tenant == PlatformTenant
	switch role {
	case "securityOfficer", "auditor":
		return ScopeCluster, true
	case "serviceAdmin", "sharedAppsAdmin":
		return ScopeCluster, platform
	case "perimeterApprover":
		if platform {
			return ScopeCluster, true
		}
		return "tenant:" + tenant, true
	}
	return "", false
}

// EntryScope is the scope an entry of the claim holds its role over, given
// the scope it states, or ok false when it holds nothing.
//
// For every role but one that is HolderScope's, and an entry may leave it
// out. A tenantAuditor is the exception: a person of the platform tenant
// who reads one other tenant's evidence -- commonly that tenant's
// administrator, given an account of the platform's realm for it -- so the
// tenant is the entry's to name, as tenant:<t>, and there is no default. It
// is never the platform tenant, whose records the cluster's auditor reads.
// The entry is keyed by role, tenant and subject, so one account audits one
// tenant.
func EntryScope(role, tenant, stated string) (scope string, ok bool) {
	if role == "tenantAuditor" {
		t, named := strings.CutPrefix(stated, "tenant:")
		if tenant != PlatformTenant || !named || !tenantName.MatchString(t) || t == PlatformTenant {
			return "", false
		}
		return stated, true
	}
	scope, ok = HolderScope(role, tenant)
	if !ok || (stated != "" && stated != scope) {
		return "", false
	}
	return scope, true
}

// RoleHolder is one account of a realm holding a relation: on the cluster,
// or, with Tenant set, on that tenant (a perimeter approver or auditor of
// scope tenant:<t>).
type RoleHolder struct {
	Relation, Realm, Subject string
	Tenant                   string
}

// holderRealm is a realm name as the director's verifier accepts it.
var holderRealm = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,62}$`)

// HolderUser is the OpenFGA user a role holder's tuple names:
// user:<realm>/<sub>. A subject never contains '/' (User refuses it), so no
// token's plain user:<sub> can be mistaken for it, and the realm in it is the
// one a token must come from to be asked as it (CheckAs).
func HolderUser(realm, sub string) (string, error) {
	if !holderRealm.MatchString(realm) {
		return "", fmt.Errorf("%w: realm %q", ErrInvalidID, realm)
	}
	id, err := encode(sub)
	if err != nil {
		return "", err
	}
	return "user:" + realm + "/" + id, nil
}

// TenantAdministrator says whether the account sub is a member of tenant's
// admins group, as the store holds it from Keycloak's events: whether a
// person of that tenant may hold a role by name (HolderScope's rule for a
// tenant other than the platform's). A tenant or subject that cannot be
// named is not one.
func TenantAdministrator(ctx context.Context, c Checker, requestID, tenant, sub string) (bool, error) {
	user, err := User(sub)
	if err != nil {
		return false, nil
	}
	group, err := Group("gentian:tenant:" + tenant + ":admins")
	if err != nil || strings.ContainsAny(tenant, ":/") {
		return false, nil
	}
	return c.Check(ctx, requestID, user, "member", group)
}

// CheckAs asks for a verified caller: as user, and, when that is refused and
// holder is set, as holder -- HolderUser of the realm that issued the token.
// A role holder's tuple therefore answers only a token of the realm the
// claim names for it.
func CheckAs(ctx context.Context, c Checker, requestID, user, holder, relation, object string) (bool, error) {
	ok, err := c.Check(ctx, requestID, user, relation, object)
	if err != nil || ok || holder == "" {
		return ok, err
	}
	return c.Check(ctx, requestID, holder, relation, object)
}

// ReconcileClusterRoles makes the cluster's role tuples equal to what the
// Cluster claim assigns: admin and break_glass held by the members of one
// Keycloak group each, and the role holders of scope cluster holding theirs.
// A holder with Tenant set is not the cluster's and is left to
// ReconcileTenants.
//
// Declarative, not additive. A role whose group the claim drops loses its
// tuple, so authority over a cluster can be taken away by editing the claim
// rather than by remembering to delete something. That is also why this reads
// what is stored first: the claim is the whole truth about these relations,
// and anything else found under them was either a previous claim's answer or
// something nobody declared.
//
// Group MEMBERSHIP is not touched here. It is a projection of Keycloak, fed by
// the event listener, and the two must not both write it.
func (c *OpenFGA) ReconcileClusterRoles(ctx context.Context, cluster string, roles map[string]string, holders []RoleHolder) error {
	object := Cluster(cluster)

	// A key the model has no relation for is a mistake in the claim, and a
	// silent one: OpenFGA accepts the tuple and it then matches nothing.
	for rel := range roles {
		if !groupRelation(rel) {
			return fmt.Errorf("cluster role %q is not one a group holds", rel)
		}
	}

	want := map[string]Tuple{}
	for _, rel := range ClusterRoles {
		group, ok := roles[rel]
		if !ok || group == "" {
			continue
		}
		obj, err := Group(group)
		if err != nil {
			return fmt.Errorf("role %s names group %q: %w", rel, group, err)
		}
		t := Tuple{User: obj + "#member", Relation: rel, Object: object}
		want[key(t)] = t
	}
	for _, h := range holders {
		if h.Tenant != "" {
			continue
		}
		if !holderRelation(h.Relation) {
			return fmt.Errorf("cluster role %q is not one a role holder may hold", h.Relation)
		}
		user, err := HolderUser(h.Realm, h.Subject)
		if err != nil {
			return fmt.Errorf("role holder %s/%s: %w", h.Realm, h.Subject, err)
		}
		t := Tuple{User: user, Relation: h.Relation, Object: object}
		want[key(t)] = t
	}

	var writes, deletes []Tuple
	for _, rel := range ClusterRoles {
		have, err := c.Read(ctx, Tuple{Relation: rel, Object: object})
		if err != nil {
			return fmt.Errorf("read %s on %s: %w", rel, object, err)
		}
		for _, t := range have {
			if _, keep := want[key(t)]; keep {
				delete(want, key(t))
				continue
			}
			deletes = append(deletes, Tuple{User: t.User, Relation: t.Relation, Object: t.Object})
		}
	}
	for _, t := range want {
		writes = append(writes, t)
	}
	if len(writes) == 0 && len(deletes) == 0 {
		return nil
	}
	// Sorted so a log line or a test reads the same way twice.
	sort.Slice(writes, func(i, j int) bool { return key(writes[i]) < key(writes[j]) })
	sort.Slice(deletes, func(i, j int) bool { return key(deletes[i]) < key(deletes[j]) })

	if err := c.Write(ctx, writes, deletes); err != nil {
		return fmt.Errorf("reconcile cluster roles: %w", err)
	}
	c.log.InfoContext(ctx, "cluster roles reconciled",
		"cluster", cluster, "granted", len(writes), "revoked", len(deletes))
	return nil
}

func groupRelation(rel string) bool {
	for _, r := range ClaimRoles {
		if r == rel {
			return true
		}
	}
	return false
}

func holderRelation(rel string) bool {
	for _, r := range HolderRoles {
		if r == rel {
			return true
		}
	}
	return false
}

func key(t Tuple) string { return t.User + "|" + t.Relation + "|" + t.Object }
