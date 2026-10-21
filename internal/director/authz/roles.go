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
)

// ClusterRoles are the cluster's role relations, each named by the Keycloak
// group that holds it. The names are the model's relations on type cluster;
// anything else is refused rather than written, because a tuple naming a
// relation the model does not have is accepted by OpenFGA and then matches
// nothing, which is indistinguishable from a permission that was never granted.
var ClusterRoles = []string{
	"admin",
	"security_officer",
	"auditor",
	"service_admin",
	"shared_apps_admin",
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
// operator writes it, to the cluster relation it grants. The claim's spelling
// and the model's are kept apart on purpose, and this is the one place they
// meet: the operator's projection and the director's reading of the claim
// both translate through it, so the two cannot drift into projecting one set
// of roles and checking another.
var ClaimRoles = map[string]string{
	"admin":           "admin",
	"securityOfficer": "security_officer",
	"auditor":         "auditor",
	"serviceAdmin":    "service_admin",
	"sharedAppsAdmin": "shared_apps_admin",
	"breakGlass":      "break_glass",
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

// HolderRoles are the cluster roles the Cluster claim gives a user of a
// tenant's realm by name (spec.platformAccess.roleHolders), keyed by the
// claim's spelling. Approving and reading only: a tenant's realm is
// administered by that tenant's administrators, who can take over any
// account in it, so a role held there is in their hands too. Never admin or
// another role that changes the cluster, and never break_glass.
var HolderRoles = map[string]string{
	"securityOfficer": "security_officer",
	"auditor":         "auditor",
}

// RoleHolder is one account of a realm holding a cluster relation.
type RoleHolder struct {
	Relation, Realm, Subject string
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
// Cluster claim assigns: for each relation, the members of one Keycloak group
// hold it, and the role holders the claim names hold theirs.
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
		if !knownClusterRole(rel) {
			return fmt.Errorf("cluster role %q is not a relation of type cluster", rel)
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

func holderRelation(rel string) bool {
	for _, r := range HolderRoles {
		if r == rel {
			return true
		}
	}
	return false
}

func key(t Tuple) string { return t.User + "|" + t.Relation + "|" + t.Object }
