/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package usher

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/gentian-org/gentian-os/internal/director/authz"
)

// fakeAccess answers what was asked, and records it.
type fakeAccess struct {
	asked []string
	err   error
}

func (f *fakeAccess) PersonAccess(_ context.Context, _, cluster, tenant, sub string) (authz.PersonAccess, error) {
	f.asked = append(f.asked, fmt.Sprintf("person %s %s %s", cluster, tenant, sub))
	if f.err != nil {
		return authz.PersonAccess{}, f.err
	}
	if strings.Contains(sub, "bad") {
		return authz.PersonAccess{}, fmt.Errorf("%w: %q", authz.ErrInvalidID, sub)
	}
	return authz.PersonAccess{Tenant: tenant, Subject: sub}, nil
}

func (f *fakeAccess) PositionAccess(_ context.Context, tenant, position string) (authz.PositionAccess, error) {
	f.asked = append(f.asked, fmt.Sprintf("position %s %s", tenant, position))
	if f.err != nil {
		return authz.PositionAccess{}, f.err
	}
	if !authz.ValidName(position) {
		return authz.PositionAccess{}, fmt.Errorf("%w: %q", authz.ErrInvalidID, position)
	}
	return authz.PositionAccess{Tenant: tenant, Position: position}, nil
}

func (f *fakeAccess) PositionsHeld(_ context.Context, tenant, sub string) ([]string, error) {
	f.asked = append(f.asked, fmt.Sprintf("positions %s %s", tenant, sub))
	if f.err != nil {
		return nil, f.err
	}
	return []string{"user"}, nil
}

func (f *fakeAccess) AppAccess(_ context.Context, tenant, profile string) (authz.View, error) {
	f.asked = append(f.asked, fmt.Sprintf("app %s %s", tenant, profile))
	if f.err != nil {
		return authz.View{}, f.err
	}
	if !authz.ValidName(profile) {
		return authz.View{}, fmt.Errorf("%w: %q", authz.ErrInvalidID, profile)
	}
	return authz.View{Object: authz.App(tenant, profile)}, nil
}

func (f *fakeAccess) AppRoles(context.Context, string, string, string, string) ([]string, error) {
	return nil, f.err
}

func TestAccessIsReadByWhoeverManagesTheTenantsPeople(t *testing.T) {
	held := map[string]bool{
		"user:ada can_manage_users tenant:acme": true,
		// Reading the tenant, or managing another tenant's people, opens
		// nothing here.
		"user:mel can_view tenant:acme":          true,
		"user:mel can_manage_users tenant:other": true,
	}
	access, viewer := &fakeAccess{}, &fakeViewer{}
	s := New(Config{Authn: fakeAuthn{"ada": "ada", "mel": "mel"}, Authz: &fakeStore{held: held},
		Viewer: viewer, Access: access, TilesPath: catalogueFile(t), Cluster: "demo"})
	for _, c := range []struct {
		path, token string
		code        int
	}{
		{"/v1/tenants/acme/access/people/u1", "ada", http.StatusOK},
		{"/v1/tenants/acme/access/people/u1", "mel", http.StatusForbidden},
		{"/v1/tenants/acme/access/people/u1", "", http.StatusUnauthorized},
		{"/v1/tenants/acme/access/people/bad", "ada", http.StatusNotFound},
		{"/v1/tenants/acme/access/positions/sales-manager", "ada", http.StatusOK},
		{"/v1/tenants/acme/access/positions/sales-manager", "mel", http.StatusForbidden},
		// A name that is none, and no name at all. Another tenant's position
		// cannot be asked for: a position is named within the path's tenant.
		{"/v1/tenants/acme/access/positions/Sales_Manager", "ada", http.StatusNotFound},
		{"/v1/tenants/acme/access/positions/", "ada", http.StatusNotFound},
		{"/v1/tenants/acme/access/apps/notes", "ada", http.StatusOK},
		{"/v1/tenants/acme/access/apps/notes", "mel", http.StatusForbidden},
		{"/v1/tenants/acme/access/apps/No_App", "ada", http.StatusNotFound},
	} {
		if code, _ := ask(t, s, c.path, c.token); code != c.code {
			t.Errorf("%s as %q: %d, want %d", c.path, c.token, code, c.code)
		}
	}
	want := "person demo acme u1|person demo acme bad|position acme sales-manager|position acme Sales_Manager|app acme notes|app acme No_App"
	if got := strings.Join(access.asked, "|"); got != want {
		t.Errorf("the store was read for\n %s\nwant\n %s", got, want)
	}
	if len(viewer.asked) != 0 {
		t.Errorf("the view was read for %v: an app's access is the reader's to answer", viewer.asked)
	}

	access.err, viewer.err = errors.New("store down"), errors.New("store down")
	for _, path := range []string{
		"/v1/tenants/acme/access/people/u1",
		"/v1/tenants/acme/access/positions/sales-manager",
		"/v1/tenants/acme/access/apps/notes",
	} {
		if code, _ := ask(t, s, path, "ada"); code != http.StatusBadGateway {
			t.Errorf("%s with an unreadable store: %d, want 502", path, code)
		}
	}
}

// Without a reader the routes do not exist, as the authorization view's do
// not without a viewer.
func TestAccessRoutesNeedAReader(t *testing.T) {
	held := map[string]bool{"user:ada can_manage_users tenant:acme": true}
	s := New(Config{Authn: fakeAuthn{"ada": "ada"}, Authz: &fakeStore{held: held},
		Viewer: &fakeViewer{}, TilesPath: catalogueFile(t), Cluster: "demo"})
	if code, _ := ask(t, s, "/v1/tenants/acme/access/people/u1", "ada"); code != http.StatusNotFound {
		t.Errorf("%d, want 404", code)
	}
}
