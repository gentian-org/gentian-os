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
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// A read-only view of who holds what.
//
// This replaces the idea of exposing OpenFGA's own playground, which is a
// development tool with a write surface: anything a person wants to CHANGE is
// changed on the console's other screens, through the director, and lands in
// git or in Keycloak. There is no second write path, and nothing here has one.
//
// Two halves, and a view with only the first is close to useless. The tuples
// say a group holds "admin" on a cluster. What "admin" lets that group do is
// in the model, several derivations deep -- can_configure is admin,
// can_audit is auditor or security_officer or admin -- and nobody should have
// to read model.fga to find out. So the view resolves both: which groups hold
// which role, and which permissions each role carries.

// Binding is one role on one object, and who holds it.
type Binding struct {
	// Relation is the role as the model names it: admin, auditor,
	// security_officer.
	Relation string `json:"relation"`
	// Groups are the Keycloak groups holding it, in their Keycloak spelling
	// rather than the OpenFGA object id. The ids differ only in the
	// separator, and a person reading this screen knows the Keycloak name.
	Groups []string `json:"groups"`
	// Grants are the permissions this role carries on this type, resolved
	// through the model. The answer to "and what does that let them do".
	Grants []string `json:"grants"`
}

// View is the authorization state of one object.
type View struct {
	// Object is the OpenFGA object this describes, such as cluster:gentian-os.
	Object string `json:"object"`
	// Bindings are the roles that are held. A role nobody holds is included
	// with an empty group list: "nobody holds break_glass" is the single most
	// useful thing this screen can say, and omitting the row would make it
	// indistinguishable from a role that does not exist.
	Bindings []Binding `json:"bindings"`
	// Unheld counts the bindings with no groups, so a console can say it
	// without walking the list.
	Unheld int `json:"unheld"`
}

// modelGraph is the part of an OpenFGA model this needs: for each type, which
// relations exist, what each is derived from, and which of them are roles
// somebody holds rather than edges to another object.
type modelGraph struct {
	// byType[type][relation] = the relations it is computed from, empty for a
	// relation that is assigned directly.
	byType map[string]map[string][]string
	// role[type][relation] is true when the relation is assigned to a group's
	// members or to a user -- that is, when somebody can hold it.
	role map[string]map[string]bool
	// parent[type][relation] are the relations on another object it follows
	// from: "admin from tenant" is {Edge: "tenant", Relation: "admin"}.
	parent map[string]map[string][]parentRef
}

// parentRef is one "relation from edge" a relation is derived from.
type parentRef struct {
	// Edge is the relation that names the other object: tenant, cluster.
	Edge string
	// Relation is what has to be held there.
	Relation string
}

// parseModel reads the embedded model into a graph of derivations.
//
// Only the shapes this model actually uses are handled: a direct assignment,
// a computedUserset, a union of those, and a difference, whose base is what
// the relation is computed from ("entitled but not ..." is carried by
// entitled; whether it is in effect for one person is the store's to say). A
// tupleToUserset -- "can_audit from cluster" -- crosses to another object and
// is deliberately NOT followed into what a role carries: a view describes one
// object, and a permission somebody holds because of their relation to the
// cluster is the cluster's row to show, not the tenant's. Saying so is better
// than implying the tenant granted it. Such a derivation is kept apart
// (parent), for the reader that can look at the other object too.
func parseModel(raw []byte) (*modelGraph, error) {
	var m struct {
		TypeDefinitions []struct {
			Type     string `json:"type"`
			Metadata *struct {
				Relations map[string]struct {
					DirectlyRelatedUserTypes []struct {
						Type     string `json:"type"`
						Relation string `json:"relation"`
					} `json:"directly_related_user_types"`
				} `json:"relations"`
			} `json:"metadata"`
			Relations map[string]struct {
				ComputedUserset *struct {
					Relation string `json:"relation"`
				} `json:"computedUserset"`
				TupleToUserset *modelTupleToUserset `json:"tupleToUserset"`
				Union          *struct {
					Child []struct {
						ComputedUserset *struct {
							Relation string `json:"relation"`
						} `json:"computedUserset"`
						TupleToUserset *modelTupleToUserset `json:"tupleToUserset"`
					} `json:"child"`
				} `json:"union"`
				Difference *struct {
					Base struct {
						ComputedUserset *struct {
							Relation string `json:"relation"`
						} `json:"computedUserset"`
					} `json:"base"`
				} `json:"difference"`
			} `json:"relations"`
		} `json:"type_definitions"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("parse authorization model: %w", err)
	}
	g := &modelGraph{
		byType: map[string]map[string][]string{},
		role:   map[string]map[string]bool{},
		parent: map[string]map[string][]parentRef{},
	}
	for _, td := range m.TypeDefinitions {
		rels := map[string][]string{}
		roles := map[string]bool{}
		// A role is a relation somebody can HOLD: the model assigns it to a
		// group's members, or to a user. A relation whose directly related
		// type is another object -- which cluster operates a tenant, which
		// app provides a contract -- is an edge in the graph and not a
		// binding, and showing one as a binding would read as a permission
		// somebody was given.
		//
		// Derived from the model rather than listed here. The first draft
		// carried a hand-written list of structural names and got `member`
		// wrong: it is the group's membership edge on type group and a real
		// role on type tenant, so no list keyed on the name alone can be
		// right.
		if td.Metadata != nil {
			for name, meta := range td.Metadata.Relations {
				for _, d := range meta.DirectlyRelatedUserTypes {
					if d.Type == "user" || (d.Type == "group" && d.Relation == "member") {
						roles[name] = true
						break
					}
				}
			}
		}
		parents := map[string][]parentRef{}
		for name, def := range td.Relations {
			var from []string
			if def.ComputedUserset != nil && def.ComputedUserset.Relation != "" {
				from = append(from, def.ComputedUserset.Relation)
			}
			if ref, ok := def.TupleToUserset.ref(); ok {
				parents[name] = append(parents[name], ref)
			}
			if def.Union != nil {
				for _, c := range def.Union.Child {
					if c.ComputedUserset != nil && c.ComputedUserset.Relation != "" {
						from = append(from, c.ComputedUserset.Relation)
					}
					if ref, ok := c.TupleToUserset.ref(); ok {
						parents[name] = append(parents[name], ref)
					}
				}
			}
			if def.Difference != nil && def.Difference.Base.ComputedUserset != nil &&
				def.Difference.Base.ComputedUserset.Relation != "" {
				from = append(from, def.Difference.Base.ComputedUserset.Relation)
			}
			rels[name] = from
		}
		g.byType[td.Type] = rels
		g.role[td.Type] = roles
		g.parent[td.Type] = parents
	}
	return g, nil
}

// modelTupleToUserset is "relation from edge" as the model's JSON has it.
type modelTupleToUserset struct {
	Tupleset struct {
		Relation string `json:"relation"`
	} `json:"tupleset"`
	ComputedUserset struct {
		Relation string `json:"relation"`
	} `json:"computedUserset"`
}

func (t *modelTupleToUserset) ref() (parentRef, bool) {
	if t == nil || t.Tupleset.Relation == "" || t.ComputedUserset.Relation == "" {
		return parentRef{}, false
	}
	return parentRef{Edge: t.Tupleset.Relation, Relation: t.ComputedUserset.Relation}, true
}

// permissions lists the can_* relations of a type. Sorted.
func (g *modelGraph) permissions(typ string) []string {
	var out []string
	for name := range g.byType[typ] {
		if strings.HasPrefix(name, "can_") {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// grantsOf resolves which can_* permissions a role carries on a type.
//
// Walks forward: a permission lists the roles it is computed from, so this
// inverts that and follows the chain, since a permission may be computed from
// another permission rather than straight from a role.
func (g *modelGraph) grantsOf(typ, role string) []string {
	var out []string
	for name := range g.reach(typ, role) {
		if strings.HasPrefix(name, "can_") {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// reach is every relation of a type that holding one of them implies, the
// relation itself included. Nil for a type the model does not have.
func (g *modelGraph) reach(typ, relation string) map[string]bool {
	rels, ok := g.byType[typ]
	if !ok {
		return nil
	}
	// reach[r] means "holding relation implies r".
	reach := map[string]bool{relation: true}
	// The model is small and shallow; iterate to a fixed point rather than
	// recursing, which also makes a cycle terminate instead of overflowing.
	for changed := true; changed; {
		changed = false
		for name, from := range rels {
			if reach[name] {
				continue
			}
			for _, f := range from {
				if reach[f] {
					reach[name] = true
					changed = true
					break
				}
			}
		}
	}
	return reach
}

// roles lists the relations of a type that somebody can hold. Sorted, so the
// view is stable across reads.
func (g *modelGraph) roles(typ string) []string {
	var out []string
	for name := range g.role[typ] {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// ViewOf reads every role binding on one object.
//
// One Read per role rather than one unfiltered read: OpenFGA's read API wants
// a filter, and asking per relation keeps the result to what this object
// actually has instead of paging the whole graph to sort it here.
func (c *OpenFGA) ViewOf(ctx context.Context, object string) (View, error) {
	return viewOf(ctx, c.Read, object)
}

// viewOf is ViewOf with the read separated out, so the resolution can be
// tested without an OpenFGA to read from.
func viewOf(ctx context.Context, read func(context.Context, Tuple) ([]Tuple, error), object string) (View, error) {
	typ, _, found := strings.Cut(object, ":")
	if !found || typ == "" {
		return View{}, fmt.Errorf("not an object id: %q", object)
	}
	g, err := parseModel(modelV1)
	if err != nil {
		return View{}, err
	}
	view := View{Object: object, Bindings: []Binding{}}
	for _, role := range g.roles(typ) {
		tuples, err := read(ctx, Tuple{Relation: role, Object: object})
		if err != nil {
			return View{}, fmt.Errorf("read %s on %s: %w", role, object, err)
		}
		groups := []string{}
		for _, t := range tuples {
			groups = append(groups, displayGroup(t.User))
		}
		sort.Strings(groups)
		if len(groups) == 0 {
			view.Unheld++
		}
		view.Bindings = append(view.Bindings, Binding{
			Relation: role,
			Groups:   groups,
			Grants:   g.grantsOf(typ, role),
		})
	}
	return view, nil
}

// displayGroup turns an OpenFGA user id back into the name a person knows.
//
// group:gentian/tenant/demo/admins#member is the Keycloak group
// gentian:tenant:demo:admins. The mapping is reversible because tenant and
// profile names are DNS labels and contain no '/', which is the reason the
// encoding chose that separator. Anything that is not a group is left as it
// is: a tuple naming a user directly is not something this platform writes,
// and disguising it as a group would hide exactly that.
func displayGroup(user string) string {
	rest, ok := strings.CutPrefix(user, "group:")
	if !ok {
		return user
	}
	rest, _, _ = strings.Cut(rest, "#")
	return strings.ReplaceAll(rest, "/", ":")
}
