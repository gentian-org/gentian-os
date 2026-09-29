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
// Package membership keeps OpenFGA's group memberships equal to Keycloak's.
//
// Keycloak is the only place a membership can be changed. What OpenFGA holds is
// a projection of it, fed by signed events from a listener inside Keycloak and
// never edited in place. This package is the receiving end: it verifies that an
// event came from the listener, decides which of the groups it names this realm
// is entitled to speak about, and makes the stored tuples match.
//
// Events carry state, not deltas: "this user's groups are now exactly these".
// Applying one is a comparison with what is stored, so an event delivered twice
// changes nothing, and an event that was lost is repaired by the next one for
// the same user instead of leaving the projection wrong until a sweep.
package membership

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"

	"github.com/gentian-org/gentian-os/internal/director/authz"
	"github.com/gentian-org/gentian-os/internal/director/gitops"
)

// Store is the part of OpenFGA the projection needs.
type Store interface {
	Read(ctx context.Context, filter authz.Tuple) ([]authz.Tuple, error)
	Write(ctx context.Context, writes, deletes []authz.Tuple) error
}

// Scope says which groups a realm may speak about. It is the boundary that
// keeps a tenant's realm — where the tenant's own administrators can create
// any group they like — from naming a group that carries someone else's role.
type Scope struct {
	// PlatformRealm is the realm of the platform's own roles.
	PlatformRealm string
	// PlatformTenant is the tenant whose realm is the platform realm.
	PlatformTenant string
}

const prefix = "gentian:"

// allows reports whether realm may assert membership of group. Every other
// realm is a tenant's, named after the tenant, and speaks for that tenant only.
func (s Scope) allows(realm, group string) bool {
	parts := strings.Split(group, ":")
	if len(parts) < 3 || parts[0] != "gentian" {
		return false
	}
	switch parts[1] {
	case "platform":
		return realm == s.PlatformRealm
	case "tenant":
		if len(parts) < 4 || !gitops.ValidName(parts[2]) {
			return false
		}
		if realm == s.PlatformRealm {
			return parts[2] == s.PlatformTenant
		}
		return parts[2] == realm
	}
	return false
}

// owns reports whether a stored group id belongs to realm's side of the
// boundary — the inverse of allows, over ids as OpenFGA holds them. A realm's
// event must never remove a membership another realm asserted.
func (s Scope) owns(realm, groupObject string) bool {
	name := strings.ReplaceAll(strings.TrimPrefix(groupObject, "group:"), "/", ":")
	return s.allows(realm, name)
}

// Projector applies membership state to the store.
type Projector struct {
	store Store
	scope Scope
	log   *slog.Logger
	// mu serialises applications. Each is a read, a comparison and a write;
	// two for one user interleaved would each undo part of the other.
	mu sync.Mutex
}

// NewProjector returns a Projector.
func NewProjector(store Store, scope Scope, log *slog.Logger) (*Projector, error) {
	if store == nil || scope.PlatformRealm == "" || scope.PlatformTenant == "" {
		return nil, errors.New("membership: store, platform realm and platform tenant are required")
	}
	if log == nil {
		log = slog.Default()
	}
	return &Projector{store: store, scope: scope, log: log}, nil
}

// Change is what applying an event did.
type Change struct {
	Added, Removed, Refused []string
}

// SetUserGroups makes user's stored memberships, within what realm may speak
// about, equal to groups. Groups outside the platform's vocabulary are ignored;
// groups inside it that this realm may not assert are refused and reported,
// because somebody created them on purpose.
func (p *Projector) SetUserGroups(ctx context.Context, realm, sub string, groups []string) (Change, error) {
	user, err := authz.User(sub)
	if err != nil {
		return Change{}, err
	}
	var change Change
	want := map[string]bool{}
	for _, g := range groups {
		g = strings.TrimPrefix(g, "/")
		if !strings.HasPrefix(g, prefix) {
			continue
		}
		obj, err := authz.Group(g)
		if err != nil || !p.scope.allows(realm, g) {
			change.Refused = append(change.Refused, g)
			continue
		}
		want[obj] = true
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	stored, err := p.store.Read(ctx, authz.Tuple{User: user, Object: "group:"})
	if err != nil {
		return Change{}, fmt.Errorf("read memberships of %s: %w", user, err)
	}
	var writes, deletes []authz.Tuple
	have := map[string]bool{}
	for _, t := range stored {
		if t.Relation != "member" || !p.scope.owns(realm, t.Object) {
			continue
		}
		have[t.Object] = true
		if !want[t.Object] {
			deletes = append(deletes, t)
			change.Removed = append(change.Removed, t.Object)
		}
	}
	for obj := range want {
		if !have[obj] {
			writes = append(writes, authz.Tuple{User: user, Relation: "member", Object: obj})
			change.Added = append(change.Added, obj)
		}
	}
	sort.Strings(change.Added)
	sort.Strings(change.Removed)
	if len(writes)+len(deletes) > 0 {
		if err := p.store.Write(ctx, writes, deletes); err != nil {
			return Change{}, fmt.Errorf("write memberships of %s: %w", user, err)
		}
	}
	if len(change.Refused) > 0 {
		p.log.WarnContext(ctx, "realm asserted groups outside its scope", "realm", realm, "user", user, "groups", change.Refused)
	}
	return change, nil
}

// RemoveGroup deletes every membership of a group that has been deleted.
func (p *Projector) RemoveGroup(ctx context.Context, realm, group string) (Change, error) {
	group = strings.TrimPrefix(group, "/")
	if !strings.HasPrefix(group, prefix) {
		return Change{}, nil
	}
	obj, err := authz.Group(group)
	if err != nil || !p.scope.allows(realm, group) {
		p.log.WarnContext(ctx, "realm reported deletion of a group outside its scope", "realm", realm, "group", group)
		return Change{Refused: []string{group}}, nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	stored, err := p.store.Read(ctx, authz.Tuple{Relation: "member", Object: obj})
	if err != nil {
		return Change{}, fmt.Errorf("read members of %s: %w", obj, err)
	}
	if len(stored) == 0 {
		return Change{}, nil
	}
	if err := p.store.Write(ctx, nil, stored); err != nil {
		return Change{}, fmt.Errorf("remove members of %s: %w", obj, err)
	}
	var change Change
	for _, t := range stored {
		change.Removed = append(change.Removed, t.User)
	}
	return change, nil
}
