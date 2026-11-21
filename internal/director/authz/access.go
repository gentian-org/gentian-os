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
	"sort"
	"strings"
)

// What one person, or one group, holds in a tenant, and through what.
//
// A view (view.go) starts at an object and says who holds each role on it.
// This starts at the other end: a person, and every role they hold on the
// tenant and on each of its apps, with the group it is held through. Then the
// store is asked which permissions are in effect for them, because the two do
// not always agree, and an administrator has to be shown where they do not:
//
//   - a permission carried by a role the person holds that is not in effect
//     (an app's use is withheld from the tenant's administrators);
//   - a permission in effect that no role held here carries (it follows from
//     the cluster, which this does not read).
//
// Read-only, and from the store alone: what is shown is what a check would
// answer, not what Keycloak's groups suggest it should.

// Where a permission comes from.
const (
	// FromRole: a role the person holds on this object carries it.
	FromRole = "role"
	// FromTenant: on an app, it follows from what the person holds on the
	// tenant.
	FromTenant = "tenant"
	// FromElsewhere: in effect, and nothing read here accounts for it.
	FromElsewhere = "elsewhere"
)

// Held is one role held on one object.
type Held struct {
	// Role is the role as the model names it: admin, member, entitled.
	Role string `json:"role"`
	// Group is the Keycloak group the role is held through, in Keycloak's
	// spelling. Empty for a role held by name.
	Group string `json:"group,omitempty"`
	// ByName is true for a role the person holds as themselves, through no
	// group.
	ByName bool `json:"byName,omitempty"`
	// Carries are the permissions the role carries on this kind of object.
	Carries []string `json:"carries"`
}

// Permission is one permission on one object, for one person.
type Permission struct {
	Name string `json:"name"`
	// InEffect is the store's own answer for this person.
	InEffect bool `json:"inEffect"`
	// From says where it comes from, one of the From constants. With
	// InEffect false, FromRole marks a permission that a held role carries
	// and the model withholds; empty is one the person simply does not have.
	From string `json:"from,omitempty"`
}

// ObjectAccess is what a person holds on one object.
type ObjectAccess struct {
	// Object is the OpenFGA object: tenant:<t>, or app:<t>/<profile>.
	Object string `json:"object"`
	// App is the profile, for an app.
	App         string       `json:"app,omitempty"`
	Held        []Held       `json:"held"`
	Permissions []Permission `json:"permissions"`
}

// PersonAccess is what one person holds in one tenant.
type PersonAccess struct {
	Tenant  string `json:"tenant"`
	Subject string `json:"subject"`
	// Groups are the groups the store has the person in, in Keycloak's
	// spelling: what their rights are computed from, which follows Keycloak
	// by its events and can differ from it for a moment.
	Groups []string `json:"groups"`
	// OnTenant is the tenant itself; Apps are its apps, every one, also
	// those the person holds nothing on.
	OnTenant ObjectAccess   `json:"onTenant"`
	Apps     []ObjectAccess `json:"apps"`
}

// GroupHold is one role a group holds on one object.
type GroupHold struct {
	Object  string   `json:"object"`
	App     string   `json:"app,omitempty"`
	Role    string   `json:"role"`
	Carries []string `json:"carries"`
}

// GroupAccess is what one group gives its members in one tenant.
type GroupAccess struct {
	Tenant string `json:"tenant"`
	// Group is the group in Keycloak's spelling.
	Group string      `json:"group"`
	Holds []GroupHold `json:"holds"`
}

// AccessReader answers what a person or a group holds in a tenant.
type AccessReader interface {
	// PersonAccess reads what the account sub holds in tenant. cluster is
	// the cluster's id, read only to find roles the person holds by name;
	// empty leaves those unread.
	PersonAccess(ctx context.Context, requestID, cluster, tenant, sub string) (PersonAccess, error)
	// GroupAccess reads what the Keycloak group gives in tenant.
	GroupAccess(ctx context.Context, tenant, group string) (GroupAccess, error)
}

// PersonAccess implements AccessReader.
func (c *OpenFGA) PersonAccess(ctx context.Context, requestID, cluster, tenant, sub string) (PersonAccess, error) {
	ask := func(ctx context.Context, questions []Question) ([]bool, error) {
		return c.BatchCheck(ctx, requestID, questions)
	}
	return personAccess(ctx, c.Read, ask, cluster, tenant, sub)
}

// GroupAccess implements AccessReader.
func (c *OpenFGA) GroupAccess(ctx context.Context, tenant, group string) (GroupAccess, error) {
	return groupAccess(ctx, c.Read, tenant, group)
}

type (
	readFunc func(context.Context, Tuple) ([]Tuple, error)
	askFunc  func(context.Context, []Question) ([]bool, error)
)

// appsOf lists a tenant's app objects, sorted.
func appsOf(ctx context.Context, read readFunc, tenant string) ([]string, error) {
	tuples, err := read(ctx, Tuple{User: Tenant(tenant), Relation: "tenant", Object: "app:"})
	if err != nil {
		return nil, fmt.Errorf("read the apps of %s: %w", tenant, err)
	}
	apps := make([]string, 0, len(tuples))
	for _, t := range tuples {
		// The store is the platform's alone, but an app object named for
		// another tenant would be shown as this one's. Keep to the name.
		if strings.HasPrefix(t.Object, "app:"+tenant+"/") {
			apps = append(apps, t.Object)
		}
	}
	sort.Strings(apps)
	return apps, nil
}

// profileOf is the profile an app object is named for.
func profileOf(object, tenant string) string {
	return strings.TrimPrefix(object, "app:"+tenant+"/")
}

func personAccess(ctx context.Context, read readFunc, ask askFunc, cluster, tenant, sub string) (PersonAccess, error) {
	user, err := User(sub)
	if err != nil {
		return PersonAccess{}, err
	}
	g, err := parseModel(modelV1)
	if err != nil {
		return PersonAccess{}, err
	}
	out := PersonAccess{Tenant: tenant, Subject: sub, Groups: []string{}, Apps: []ObjectAccess{}}

	// The groups the store has the person in.
	memberships, err := read(ctx, Tuple{User: user, Relation: "member", Object: "group:"})
	if err != nil {
		return PersonAccess{}, fmt.Errorf("read the groups of %s: %w", user, err)
	}
	through := map[string]string{} // group:<g>#member -> the Keycloak name
	for _, t := range memberships {
		name := displayGroup(t.Object)
		through[t.Object+"#member"] = name
		out.Groups = append(out.Groups, name)
	}
	sort.Strings(out.Groups)

	apps, err := appsOf(ctx, read, tenant)
	if err != nil {
		return PersonAccess{}, err
	}
	objects := append([]string{Tenant(tenant)}, apps...)

	// A role held by name is stored for user:<realm>/<sub>, and only a check
	// asked as that user sees it. The realm is not known here, and need not
	// be: the tuples say which such users end in this subject.
	byName := func(u string) bool {
		rest, ok := strings.CutPrefix(u, "user:")
		return ok && strings.HasSuffix(rest, "/"+strings.TrimPrefix(user, "user:"))
	}
	identities := map[string]bool{user: true}
	held := map[string][]Held{}
	scan := func(object string, keep bool) error {
		typ, _, _ := strings.Cut(object, ":")
		tuples, err := read(ctx, Tuple{Object: object})
		if err != nil {
			return fmt.Errorf("read %s: %w", object, err)
		}
		for _, t := range tuples {
			if !g.role[typ][t.Relation] {
				continue
			}
			h := Held{Role: t.Relation, Carries: g.grantsOf(typ, t.Relation)}
			if h.Carries == nil {
				h.Carries = []string{}
			}
			switch name, ok := through[t.User]; {
			case ok:
				h.Group = name
			case t.User == user:
				h.ByName = true
			case byName(t.User):
				h.ByName = true
				identities[t.User] = true
			default:
				continue
			}
			if keep {
				held[object] = append(held[object], h)
			}
		}
		return nil
	}
	for _, object := range objects {
		if err := scan(object, true); err != nil {
			return PersonAccess{}, err
		}
	}
	if cluster != "" {
		// Read for the names alone: what the person holds on the cluster is
		// the cluster's to show.
		if err := scan(Cluster(cluster), false); err != nil {
			return PersonAccess{}, err
		}
	}
	users := make([]string, 0, len(identities))
	for u := range identities {
		users = append(users, u)
	}
	sort.Strings(users)

	// What is in effect: every permission of each object, and on the tenant
	// its roles too, since an app's permissions follow from them.
	asked := func(object string) []string {
		typ, _, _ := strings.Cut(object, ":")
		relations := g.permissions(typ)
		if typ == "tenant" {
			relations = append(relations, g.roles(typ)...)
		}
		return relations
	}
	var questions []Question
	for _, object := range objects {
		for _, relation := range asked(object) {
			for _, u := range users {
				questions = append(questions, Question{User: u, Relation: relation, Object: object})
			}
		}
	}
	answers, err := ask(ctx, questions)
	if err != nil {
		return PersonAccess{}, err
	}
	if len(answers) != len(questions) {
		return PersonAccess{}, fmt.Errorf("asked %d questions, answered %d", len(questions), len(answers))
	}
	inEffect := map[string]map[string]bool{}
	for i, q := range questions {
		if inEffect[q.Object] == nil {
			inEffect[q.Object] = map[string]bool{}
		}
		if answers[i] {
			inEffect[q.Object][q.Relation] = true
		}
	}

	onTenant := inEffect[Tenant(tenant)]
	for _, object := range objects {
		typ, _, _ := strings.Cut(object, ":")
		access := ObjectAccess{Object: object, Held: held[object], Permissions: []Permission{}}
		if access.Held == nil {
			access.Held = []Held{}
		}
		sort.SliceStable(access.Held, func(a, b int) bool {
			if access.Held[a].Role != access.Held[b].Role {
				return access.Held[a].Role < access.Held[b].Role
			}
			return access.Held[a].Group < access.Held[b].Group
		})
		carried := map[string]bool{}
		for _, h := range access.Held {
			for _, p := range h.Carries {
				carried[p] = true
			}
		}
		for _, name := range g.permissions(typ) {
			p := Permission{Name: name, InEffect: inEffect[object][name]}
			switch {
			case carried[name]:
				p.From = FromRole
			case !p.InEffect:
			case typ == "app" && g.followsFrom(typ, name, "tenant", onTenant):
				p.From = FromTenant
			default:
				p.From = FromElsewhere
			}
			access.Permissions = append(access.Permissions, p)
		}
		if typ == "app" {
			access.App = profileOf(object, tenant)
			out.Apps = append(out.Apps, access)
		} else {
			out.OnTenant = access
		}
	}
	return out, nil
}

// followsFrom reports whether a permission of a type follows from something
// held on the object its edge names: "can_write_credential: admin from
// tenant" follows once admin is in effect on the tenant.
func (g *modelGraph) followsFrom(typ, permission, edge string, there map[string]bool) bool {
	for relation, refs := range g.parent[typ] {
		if !g.reach(typ, relation)[permission] {
			continue
		}
		for _, ref := range refs {
			if ref.Edge == edge && there[ref.Relation] {
				return true
			}
		}
	}
	return false
}

func groupAccess(ctx context.Context, read readFunc, tenant, group string) (GroupAccess, error) {
	object, err := Group(group)
	if err != nil {
		return GroupAccess{}, err
	}
	g, err := parseModel(modelV1)
	if err != nil {
		return GroupAccess{}, err
	}
	out := GroupAccess{Tenant: tenant, Group: group, Holds: []GroupHold{}}
	for _, typ := range []string{"tenant", "app"} {
		tuples, err := read(ctx, Tuple{User: object + "#member", Object: typ + ":"})
		if err != nil {
			return GroupAccess{}, fmt.Errorf("read what %s holds: %w", object, err)
		}
		for _, t := range tuples {
			if !g.role[typ][t.Relation] {
				continue
			}
			hold := GroupHold{Object: t.Object, Role: t.Relation, Carries: g.grantsOf(typ, t.Relation)}
			if hold.Carries == nil {
				hold.Carries = []string{}
			}
			switch {
			case t.Object == Tenant(tenant):
			case strings.HasPrefix(t.Object, "app:"+tenant+"/"):
				hold.App = profileOf(t.Object, tenant)
			default:
				// Another tenant's object is that tenant's to show.
				continue
			}
			out.Holds = append(out.Holds, hold)
		}
	}
	sort.SliceStable(out.Holds, func(a, b int) bool {
		if out.Holds[a].Object != out.Holds[b].Object {
			return out.Holds[a].Object < out.Holds[b].Object
		}
		return out.Holds[a].Role < out.Holds[b].Role
	})
	return out, nil
}
