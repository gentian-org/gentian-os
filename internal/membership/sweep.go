/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package membership

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/director/authz"
)

// People is what a sweep asks Keycloak: who a realm holds.
type People interface {
	// AccountIDs lists the id of every account of the realm, enabled or not.
	AccountIDs(ctx context.Context, realm string) ([]string, error)
	// Exists asks for one account by id.
	Exists(ctx context.Context, realm, id string) (bool, error)
}

// Departed is what a sweep removed in one realm: the people it found gone,
// and the memberships it took from them.
type Departed struct {
	Realm   string
	People  []string
	Removed int
}

// Sweep removes the memberships of people who no longer exist in Keycloak.
//
// A deletion statement removes them when it arrives; a sweep is for one that
// did not. It reads every stored membership first and asks Keycloak second,
// so a person created between the two has no membership yet to lose. The
// realm's account list only nominates: each person missing from it is asked
// for by id before anything of theirs is removed, as a list read while
// accounts are added can skip one at a page boundary. A realm Keycloak does
// not answer for is left as it is and named in the error; the others are
// swept.
//
// Removal is the deletion statement's: the person's groups in that realm are
// set to none, here and in the app stores that mirror them.
func (rc *Receiver) Sweep(ctx context.Context, people People) ([]Departed, error) {
	stored, err := rc.proj.store.Read(ctx, authz.Tuple{})
	if err != nil {
		return nil, fmt.Errorf("read memberships: %w", err)
	}
	members := map[string]map[string]bool{} // realm → stored user → true
	for _, t := range stored {
		if t.Relation != "member" || !strings.HasPrefix(t.User, "user:") || !strings.HasPrefix(t.Object, "group:") {
			continue
		}
		realm := rc.proj.scope.realmOf(t.Object)
		if realm == "" {
			continue
		}
		if members[realm] == nil {
			members[realm] = map[string]bool{}
		}
		members[realm][t.User] = true
	}

	realms := make([]string, 0, len(members))
	for realm := range members {
		realms = append(realms, realm)
	}
	sort.Strings(realms)

	var out []Departed
	var errs []error
	for _, realm := range realms {
		d, err := rc.sweepRealm(ctx, people, realm, members[realm])
		if err != nil {
			errs = append(errs, fmt.Errorf("realm %s: %w", realm, err))
		}
		if len(d.People) > 0 {
			out = append(out, d)
		}
	}
	return out, errors.Join(errs...)
}

func (rc *Receiver) sweepRealm(ctx context.Context, people People, realm string, stored map[string]bool) (Departed, error) {
	d := Departed{Realm: realm}
	ids, err := people.AccountIDs(ctx, realm)
	if err != nil {
		return d, err
	}
	present := map[string]bool{}
	for _, id := range ids {
		if user, err := authz.User(id); err == nil {
			present[user] = true
		}
	}
	var gone []string
	for user := range stored {
		if !present[user] {
			gone = append(gone, user)
		}
	}
	sort.Strings(gone)
	for _, user := range gone {
		sub := strings.ReplaceAll(strings.TrimPrefix(user, "user:"), "/", ":")
		exists, err := people.Exists(ctx, realm, sub)
		if err != nil {
			return d, err
		}
		if exists {
			continue
		}
		change, err := rc.proj.SetUserGroups(ctx, realm, sub, nil)
		if err != nil {
			return d, err
		}
		if rc.mirror != nil {
			if err := rc.mirror.SetUserGroups(ctx, realm, sub, nil); err != nil {
				return d, err
			}
		}
		// A statement about this person written before the sweep and
		// delivered after it would give the memberships back.
		rc.newest(realm+"/"+sub, rc.now().UnixMilli())
		d.People = append(d.People, sub)
		d.Removed += len(change.Removed)
	}
	if len(d.People) > 0 {
		rc.log.InfoContext(ctx, "removed the memberships of people no longer in Keycloak",
			"realm", realm, "people", d.People, "memberships", d.Removed)
	}
	return d, nil
}

// realmOf is the realm that speaks for a stored group, or "" for a group
// outside the platform's vocabulary: the realm allows names.
func (s Scope) realmOf(groupObject string) string {
	parts := strings.Split(strings.TrimPrefix(groupObject, "group:"), "/")
	if len(parts) < 3 || parts[0] != "gentian" {
		return ""
	}
	switch parts[1] {
	case "platform":
		return s.PlatformRealm
	case "tenant":
		if len(parts) < 4 || !gentianov1alpha1.ValidTenantName(parts[2]) {
			return ""
		}
		if parts[2] == s.PlatformTenant {
			return s.PlatformRealm
		}
		return parts[2]
	}
	return ""
}
