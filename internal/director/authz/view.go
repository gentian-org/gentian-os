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
}

// parseModel reads the embedded model into a graph of derivations.
//
// Only the shapes this model actually uses are handled: a direct assignment,
// a computedUserset, and a union of those. A tupleToUserset -- "can_audit
// from cluster" -- crosses to another object and is deliberately NOT followed:
// this view describes one object, and a permission somebody holds because of
// their relation to the cluster is the cluster's row to show, not the
// tenant's. Saying so is better than implying the tenant granted it.
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
				Union *struct {
					Child []struct {
						ComputedUserset *struct {
							Relation string `json:"relation"`
						} `json:"computedUserset"`
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
		for name, def := range td.Relations {
			var from []string
			if def.ComputedUserset != nil && def.ComputedUserset.Relation != "" {
				from = append(from, def.ComputedUserset.Relation)
			}
			if def.Union != nil {
				for _, c := range def.Union.Child {
					if c.ComputedUserset != nil && c.ComputedUserset.Relation != "" {
						from = append(from, c.ComputedUserset.Relation)
					}
				}
			}
			rels[name] = from
		}
		g.byType[td.Type] = rels
		g.role[td.Type] = roles
	}
	return g, nil
}

// grantsOf resolves which can_* permissions a role carries on a type.
//
// Walks forward: a permission lists the roles it is computed from, so this
// inverts that and follows the chain, since a permission may be computed from
// another permission rather than straight from a role.
func (g *modelGraph) grantsOf(typ, role string) []string {
	rels, ok := g.byType[typ]
	if !ok {
		return nil
	}
	// reach[r] means "holding role implies r".
	reach := map[string]bool{role: true}
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
	var out []string
	for name := range reach {
		if strings.HasPrefix(name, "can_") {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
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
