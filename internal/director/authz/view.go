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
// git or in the rights store. There is no second write path, and nothing here
// has one.
//
// Two halves, and a view with only the first is close to useless. The tuples
// say a position holds "admin" on a tenant. What "admin" lets its holders do
// is in the model, several derivations deep -- can_configure is admin,
// can_audit is auditor or security_officer or admin -- and nobody should have
// to read model.fga to find out. So the view resolves both: who holds
// which role, and which permissions each role carries.

// Binding is one role on one object, and who holds it.
type Binding struct {
	// Relation is the role as the model names it: admin, auditor,
	// security_officer.
	Relation string `json:"relation"`
	// Holders are who holds it: positions, people by themselves, and the
	// cluster's groups.
	Holders []Holder `json:"holders"`
	// Grants are the permissions this role carries on this type, resolved
	// through the model. The answer to "and what does that let them do".
	Grants []string `json:"grants"`
	// Declared is true for a role an app's profile declares, as opposed to
	// one the model names.
	Declared bool `json:"declared,omitempty"`
}

// The kinds of holder.
const (
	// HolderPosition: a position of the tenant; Name is the position's.
	HolderPosition = "position"
	// HolderPerson: a person by themselves; Name is the account's subject,
	// qualified by its realm for a role the Cluster claim gives by name.
	HolderPerson = "person"
	// HolderGroup: a group of the sign-in service; Name is its name there.
	HolderGroup = "group"
)

// Holder is one holder of a role.
type Holder struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
}

// View is the authorization state of one object.
type View struct {
	// Object is the OpenFGA object this describes, such as cluster:gentian-os.
	Object string `json:"object"`
	// Bindings are the roles that are held. A role nobody holds is included
	// with an empty list of holders: "nobody holds break_glass" is the single most
	// useful thing this screen can say, and omitting the row would make it
	// indistinguishable from a role that does not exist.
	Bindings []Binding `json:"bindings"`
	// Unheld counts the bindings with no holders, so a console can say it
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
	// role[type][relation] is true when the relation is assigned to a user,
	// to a position's holders or to a group's members -- that is, when
	// somebody can hold it.
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
// a computedUserset and a union of those. A
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
		// user, to a position's holders or to a group's members. A relation whose directly related
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
				// What the operator derives is not a role anybody was given.
				if IsDerived(name) {
					continue
				}
				for _, d := range meta.DirectlyRelatedUserTypes {
					if d.Type == "user" || (d.Type == "group" && d.Relation == "member") ||
						(d.Type == "position" && d.Relation == "holder") {
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
		holders := []Holder{}
		for _, t := range tuples {
			holders = append(holders, holderOf(t.User))
		}
		sort.Slice(holders, func(a, b int) bool {
			if holders[a].Kind != holders[b].Kind {
				return holders[a].Kind < holders[b].Kind
			}
			return holders[a].Name < holders[b].Name
		})
		if len(holders) == 0 {
			view.Unheld++
		}
		view.Bindings = append(view.Bindings, Binding{
			Relation: role,
			Holders:  holders,
			Grants:   g.grantsOf(typ, role),
		})
	}
	return view, nil
}

// appView is the view of one app with the roles its profile declares: who
// was given each. A declared role carries what user carries, since holding
// it is holding user.
func appView(ctx context.Context, read readFunc, tenant, profile string) (View, error) {
	app := App(tenant, profile)
	view, err := viewOf(ctx, read, app)
	if err != nil {
		return View{}, err
	}
	g, err := parseModel(modelV1)
	if err != nil {
		return View{}, err
	}
	declared, err := declaredRolesOf(ctx, read, tenant, app)
	if err != nil {
		return View{}, err
	}
	for _, role := range declared {
		tuples, err := read(ctx, Tuple{Relation: "holder", Object: role})
		if err != nil {
			return View{}, fmt.Errorf("read %s: %w", role, err)
		}
		holders := []Holder{}
		for _, t := range tuples {
			holders = append(holders, holderOf(t.User))
		}
		sort.Slice(holders, func(a, b int) bool {
			if holders[a].Kind != holders[b].Kind {
				return holders[a].Kind < holders[b].Kind
			}
			return holders[a].Name < holders[b].Name
		})
		if len(holders) == 0 {
			view.Unheld++
		}
		view.Bindings = append(view.Bindings, Binding{
			Relation: roleNameOf(role), Holders: holders, Grants: g.grantsOf("app", RoleUser), Declared: true,
		})
	}
	return view, nil
}

// holderOf turns the holder of an entry into what a person reading the
// screen knows it as.
//
// position:demo/sales-manager#holder is the position sales-manager,
// group:gentian/platform/admin#member the sign-in service's group
// gentian:platform:admin (the mapping is reversible because the names it is
// made from contain no '/'), and user:<id> a person by themselves. Anything
// else is left as it is, as a person: it is not something the platform
// writes, and dressing it up would hide exactly that.
func holderOf(user string) Holder {
	object, relation, _ := strings.Cut(user, "#")
	if rest, ok := strings.CutPrefix(object, "position:"); ok && relation == "holder" {
		_, name, _ := strings.Cut(rest, "/")
		return Holder{Kind: HolderPosition, Name: name}
	}
	if rest, ok := strings.CutPrefix(object, "group:"); ok {
		return Holder{Kind: HolderGroup, Name: strings.ReplaceAll(rest, "/", ":")}
	}
	if rest, ok := strings.CutPrefix(object, "user:"); ok && relation == "" {
		return Holder{Kind: HolderPerson, Name: rest}
	}
	return Holder{Kind: HolderPerson, Name: user}
}
