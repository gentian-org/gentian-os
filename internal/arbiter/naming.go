/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package arbiter

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/gentian-org/gentian-os/internal/director/authz"
)

// How people and groups are named in an app's store (app-authorization.md
// §2.2): by their realm and their name there, through the one mapping every
// identifier crosses into OpenFGA by (authz.User, authz.Group), so that the
// platform's tuples and an app's agree on who someone is.
//
//	a person    user:<realm>/<subject>             user:acme/0b6d...
//	a group     group:<realm>/<group name>         group:acme/gentian/tenant/acme/members
//	its members group:<realm>/<group name>#member
//
// ':' in a name becomes '/', as in the kernel's store; a name OpenFGA could
// not hold ('/', '#', whitespace) is refused, never repaired, because two
// names must never become one.

// UserObject is a person of realm, by the subject of their tokens.
func UserObject(realm, subject string) (string, error) {
	if !dnsLabel.MatchString(realm) {
		return "", fmt.Errorf("%w: realm %q", authz.ErrInvalidID, realm)
	}
	return authz.User(realm + ":" + subject)
}

// GroupObject is a group of realm, by its name or by its top-level path
// ("/staff"). A nested group ("/staff/berlin") is refused, as the kernel's
// store refuses one (authz.GroupFromPath): the platform's groups are flat,
// and a member of a subgroup is not mirrored as a member of anything.
func GroupObject(realm, name string) (string, error) {
	if !dnsLabel.MatchString(realm) {
		return "", fmt.Errorf("%w: realm %q", authz.ErrInvalidID, realm)
	}
	name = strings.TrimPrefix(name, "/")
	if name == "" || strings.Contains(name, "/") {
		return "", fmt.Errorf("%w: group %q is not a top-level group", authz.ErrInvalidID, name)
	}
	return authz.Group(realm + ":" + name)
}

// groupType is the type a mirrored group is of, and the one no component's
// key may write a tuple of.
const groupType = "group"

// isGroupTyped reports whether a tuple's user or object is of type group:
// a group, a group's members (group:x#member), or every group (group:*).
func isGroupTyped(t Tuple) bool {
	return strings.HasPrefix(t.User, groupType+":") || strings.HasPrefix(t.Object, groupType+":")
}

// MaxStartingTuples bounds a profile's starting tuples.
const MaxStartingTuples = 100

// modelTypes is a compiled model as much as checking a tuple against it
// needs: per type, per relation, the kinds of user it may be written with.
type modelTypes map[string]map[string][]relatedType

type relatedType struct {
	Type     string    `json:"type"`
	Relation string    `json:"relation,omitempty"`
	Wildcard *struct{} `json:"wildcard,omitempty"`
}

func readModel(compiled json.RawMessage) (modelTypes, error) {
	var m struct {
		TypeDefinitions []struct {
			Type      string                     `json:"type"`
			Relations map[string]json.RawMessage `json:"relations"`
			Metadata  *struct {
				Relations map[string]struct {
					DirectlyRelatedUserTypes []relatedType `json:"directly_related_user_types"`
				} `json:"relations"`
			} `json:"metadata"`
		} `json:"type_definitions"`
	}
	if err := json.Unmarshal(compiled, &m); err != nil {
		return nil, err
	}
	out := modelTypes{}
	for _, td := range m.TypeDefinitions {
		rels := map[string][]relatedType{}
		for name := range td.Relations {
			rels[name] = nil
			if td.Metadata != nil {
				rels[name] = td.Metadata.Relations[name].DirectlyRelatedUserTypes
			}
		}
		out[td.Type] = rels
	}
	return out, nil
}

// admits reports whether the model lets user be written with relation on an
// object of objType, and why not.
func (m modelTypes) admits(user, relation, objType string) error {
	rels, ok := m[objType]
	if !ok {
		return fmt.Errorf("the model has no type %q", objType)
	}
	allowed, ok := rels[relation]
	if !ok {
		return fmt.Errorf("type %q has no relation %q", objType, relation)
	}
	userType, rest, ok := strings.Cut(user, ":")
	if !ok || userType == "" || rest == "" {
		return fmt.Errorf("user %q is not <type>:<id>", user)
	}
	id, userRelation, _ := strings.Cut(rest, "#")
	for _, a := range allowed {
		switch {
		case a.Type != userType:
		case a.Wildcard != nil:
			if id == "*" && userRelation == "" {
				return nil
			}
		case a.Relation != "":
			if userRelation == a.Relation && id != "*" {
				return nil
			}
		case userRelation == "" && id != "*":
			return nil
		}
	}
	return fmt.Errorf("%s#%s cannot be written with the user %q", objType, relation, user)
}

// ValidateStartingTuples holds a profile's starting tuples, and its wish to
// mirror the realm's groups, to the compiled model. It is asked when the
// profile is admitted, so that a tuple the store would refuse is a failed
// install rather than a store that never reaches what its profile says.
//
// A store that mirrors groups has to take them: a type group with a relation
// member that users are written with. Its starting tuples may then name no
// group as their object, because which people are in which group is the
// realm's to say.
func ValidateStartingTuples(compiled json.RawMessage, tuples []Tuple, mirror bool) error {
	m, err := readModel(compiled)
	if err != nil {
		return fmt.Errorf("the compiled model: %w", err)
	}
	if mirror {
		if err := m.admits("user:someone", "member", groupType); err != nil {
			return fmt.Errorf("groups: mirror needs a type group with a relation member that users are written with: %w", err)
		}
	}
	if len(tuples) > MaxStartingTuples {
		return fmt.Errorf("at most %d starting tuples", MaxStartingTuples)
	}
	seen := map[Tuple]bool{}
	for i, t := range tuples {
		if err := t.valid(fmt.Sprintf("tuples[%d]", i)); err != nil {
			return err
		}
		objType, objID, ok := strings.Cut(t.Object, ":")
		if !ok || objType == "" || objID == "" || strings.Contains(objID, "#") {
			return fmt.Errorf("tuples[%d]: object %q is not <type>:<id>", i, t.Object)
		}
		if mirror && objType == groupType {
			return fmt.Errorf("tuples[%d]: who is in a group is the realm's to say, and this store mirrors the realm's groups", i)
		}
		if err := m.admits(t.User, t.Relation, objType); err != nil {
			return fmt.Errorf("tuples[%d]: %w", i, err)
		}
		if seen[t] {
			return fmt.Errorf("tuples[%d]: named twice", i)
		}
		seen[t] = true
	}
	return nil
}
