/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package registrar_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/gentian-org/gentian-os/internal/director/authz"
	"github.com/gentian-org/gentian-os/internal/registrar"
)

// fakeRights is the operator as the registrar asks it: what each person
// holds in each tenant, the positions a tenant has, and the rule that a
// tenant keeps an administrator of its own.
type fakeRights struct {
	mu sync.Mutex
	// held[tenant][subject] are the assignments, in the order given.
	held map[string]map[string][]authz.Assignment
	// positions are the positions every tenant has here.
	positions map[string]bool
	// asked is every act, as "verb tenant subject what actor".
	asked []string
	fail  error
}

func newFakeRights() *fakeRights {
	return &fakeRights{
		held:      map[string]map[string][]authz.Assignment{},
		positions: map[string]bool{"user": true, "tenant-administrator": true, "app-administrator": true, "sales-manager": true},
	}
}

func administers(a authz.Assignment) bool {
	return a.Position == authz.PositionTenantAdministrator || (a.App == "" && a.Role == authz.RoleAdmin)
}

func (f *fakeRights) administrators(tenant string) []string {
	var out []string
	for subject, held := range f.held[tenant] {
		for _, a := range held {
			if administers(a) {
				out = append(out, subject)
				break
			}
		}
	}
	sort.Strings(out)
	return out
}

func (f *fakeRights) positionsOf(tenant, subject string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, a := range f.held[tenant][subject] {
		if a.Position != "" {
			out = append(out, a.Position)
		}
	}
	return out
}

func (f *fakeRights) Give(_ context.Context, actor, tenant, subject string, a authz.Assignment) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.asked = append(f.asked, fmt.Sprintf("give %s %s %+v as %s", tenant, subject, a, actor))
	switch {
	case f.fail != nil:
		return false, f.fail
	case a.Position != "" && a.Role != "":
		return false, fmt.Errorf("%w: an assignment is a position, or a role", registrar.ErrRefusedAssignment)
	case a.Position != "" && !f.positions[a.Position], a.App == "ghost":
		return false, authz.ErrNoSuchThing
	}
	for _, have := range f.held[tenant][subject] {
		if have == a {
			return false, nil
		}
	}
	if f.held[tenant] == nil {
		f.held[tenant] = map[string][]authz.Assignment{}
	}
	f.held[tenant][subject] = append(f.held[tenant][subject], a)
	return true, nil
}

func (f *fakeRights) Take(_ context.Context, actor, tenant, subject string, a authz.Assignment) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.asked = append(f.asked, fmt.Sprintf("take %s %s %+v as %s", tenant, subject, a, actor))
	if f.fail != nil {
		return false, f.fail
	}
	held := f.held[tenant][subject]
	for i, have := range held {
		if have != a {
			continue
		}
		if administers(a) && len(f.administrators(tenant)) == 1 {
			return false, authz.ErrLastAdministrator
		}
		f.held[tenant][subject] = append(append([]authz.Assignment{}, held[:i]...), held[i+1:]...)
		return true, nil
	}
	return false, nil
}

func (f *fakeRights) RemovePerson(_ context.Context, actor, tenant, subject string) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.asked = append(f.asked, fmt.Sprintf("remove %s %s as %s", tenant, subject, actor))
	if f.fail != nil {
		return 0, f.fail
	}
	if admins := f.administrators(tenant); len(admins) == 1 && admins[0] == subject {
		return 0, authz.ErrLastAdministrator
	}
	n := len(f.held[tenant][subject])
	delete(f.held[tenant], subject)
	return n, nil
}

func (f *fakeRights) Assignments(_ context.Context, tenant, subject string) ([]authz.Assignment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail != nil {
		return nil, f.fail
	}
	return append([]authz.Assignment{}, f.held[tenant][subject]...), nil
}

func (f *fakeRights) Administrators(_ context.Context, tenant string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail != nil {
		return nil, f.fail
	}
	return f.administrators(tenant), nil
}

// A position, or a role, is given to one of the tenant's people by whoever
// manages them; the operator is asked, as the person who asked.
func TestAPositionIsGivenAndTakenThroughTheOperator(t *testing.T) {
	f := newFakeIdentity("demo")
	h := startWithIdentity(t, f)
	tom := h.token(t, "tenant-demo", "tom")
	give, take := "/v1/tenants/demo/actions/give", "/v1/tenants/demo/actions/take"

	status, body := h.do(t, http.MethodPost, give, tom, `{"person":"u1","position":"sales-manager"}`)
	if status != http.StatusOK || body["changed"] != true || body["position"] != "sales-manager" || body["holds"] != true {
		t.Fatalf("given: %d %v", status, body)
	}
	if body["totpRequired"] != nil {
		t.Errorf("a position that administers nothing asked for a second factor: %v", body)
	}
	if status, body := h.do(t, http.MethodPost, give, tom, `{"person":"u1","position":"sales-manager"}`); status != http.StatusOK || body["changed"] != false {
		t.Errorf("given again: %d %v", status, body)
	}
	status, body = h.do(t, http.MethodPost, give, tom, `{"person":"u1","app":"wiki","role":"user"}`)
	if status != http.StatusOK || body["role"] != "user" || body["app"] != "wiki" {
		t.Errorf("a role on an app: %d %v", status, body)
	}
	if last := h.rights.asked[0]; !strings.HasSuffix(last, " as tom") || !strings.HasPrefix(last, "give demo u1 ") {
		t.Errorf("the operator was asked %q", last)
	}
	status, body = h.do(t, http.MethodGet, "/v1/tenants/demo/people/u1/assignments", tom, "")
	if list, _ := body["assignments"].([]any); status != http.StatusOK || len(list) != 2 {
		t.Errorf("assignments: %d %v", status, body)
	}
	if status, body := h.do(t, http.MethodPost, take, tom, `{"person":"u1","position":"sales-manager"}`); status != http.StatusOK || body["changed"] != true || body["holds"] != false {
		t.Errorf("taken: %d %v", status, body)
	}

	// Refused, each for what it is, and the operator is not asked about a
	// person who is nobody's here.
	before := len(h.rights.asked)
	if status, _ := h.do(t, http.MethodPost, give, tom, `{"person":"nobody","position":"user"}`); status != http.StatusNotFound || len(h.rights.asked) != before {
		t.Errorf("a person who is not the tenant's: %d", status)
	}
	for name, c := range map[string]struct {
		body string
		want int
	}{
		"a position the tenant has not": {`{"person":"u1","position":"ghost"}`, http.StatusNotFound},
		"an app the tenant has not":     {`{"person":"u1","app":"ghost","role":"user"}`, http.StatusNotFound},
		"what is no assignment":         {`{"person":"u1","position":"user","role":"admin"}`, http.StatusBadRequest},
	} {
		if status, body := h.do(t, http.MethodPost, give, tom, c.body); status != c.want {
			t.Errorf("%s: %d %v, want %d", name, status, body, c.want)
		}
	}
	// Only whoever manages the tenant's people: not a user of it, and not
	// another tenant's administrator.
	for who, token := range map[string]string{"a user": h.token(t, "tenant-demo", "mia"), "another tenant's": h.token(t, "tenant-solo", "tina")} {
		if status, _ := h.do(t, http.MethodPost, give, token, `{"person":"u1","position":"user"}`); status != http.StatusForbidden {
			t.Errorf("%s administrator giving a position: %d, want 403", who, status)
		}
		if status, _ := h.do(t, http.MethodGet, "/v1/tenants/demo/people/u1/assignments", token, ""); status != http.StatusForbidden {
			t.Errorf("%s reading assignments: %d, want 403", who, status)
		}
	}

	// An operator that does not answer changes nothing and says so, without
	// its error.
	h.rights.fail = errors.New("dial tcp 10.0.0.9:8082: connection refused")
	status, body = h.do(t, http.MethodPost, give, tom, `{"person":"u1","position":"user"}`)
	if msg, _ := body["error"].(string); status != http.StatusBadGateway || strings.Contains(msg, "10.0.0.9") {
		t.Errorf("an operator that does not answer: %d %v", status, body)
	}
}

// Whoever comes to administer a tenant is asked for a second factor; and
// the tenant's last administrator keeps the role and is not removed.
func TestAnAdministratorIsAskedForASecondFactorAndTheLastOneStays(t *testing.T) {
	f := newFakeIdentity("demo")
	h := startWithIdentity(t, f)
	tom := h.token(t, "tenant-demo", "tom")
	give, take := "/v1/tenants/demo/actions/give", "/v1/tenants/demo/actions/take"
	admin := `{"person":"u1","position":"tenant-administrator"}`

	status, body := h.do(t, http.MethodPost, give, tom, admin)
	if status != http.StatusOK || body["totpRequired"] != true {
		t.Fatalf("an administrator made: %d %v", status, body)
	}
	if status, body := h.do(t, http.MethodPost, take, tom, admin); status != http.StatusConflict {
		t.Errorf("the last administrator's position taken: %d %v", status, body)
	}
	status, body = h.do(t, http.MethodPost, "/v1/tenants/demo/actions/remove-person", tom, `{"person":"u1"}`)
	if status != http.StatusConflict || len(f.removed) != 0 {
		t.Fatalf("the last administrator removed: %d %v, removed at the sign-in service: %v", status, body, f.removed)
	}
	// With another administrator the first is removed, and what they held
	// goes with them.
	if _, err := h.rights.Give(context.Background(), "test", "demo", "u2", authz.Assignment{Position: authz.PositionTenantAdministrator}); err != nil {
		t.Fatal(err)
	}
	status, body = h.do(t, http.MethodPost, "/v1/tenants/demo/actions/remove-person", tom, `{"person":"u1"}`)
	if status != http.StatusOK || len(f.removed) != 1 || body["warning"] != nil {
		t.Fatalf("removed: %d %v", status, body)
	}
	if left := h.rights.positionsOf("demo", "u1"); len(left) != 0 {
		t.Errorf("left of a removed person: %v", left)
	}
}
