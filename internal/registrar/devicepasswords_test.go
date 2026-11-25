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
	"net/http"
	"strings"
	"testing"
	"time"

	dt "github.com/gentian-org/gentian-os/internal/director/directortest"
	"github.com/gentian-org/gentian-os/internal/registrar/identity"
)

// The fake's half of device passwords: kept per realm and person.

func (f *fakeIdentity) DevicePasswords(ctx context.Context, r identity.Realm, id string) ([]identity.DevicePassword, error) {
	f.note(ctx, r)
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]identity.DevicePassword{}, f.devicePasswords[r.Name()+"/"+id]...), nil
}

func (f *fakeIdentity) CreateDevicePassword(ctx context.Context, r identity.Realm, id, name string, ways []string) (identity.NewDevicePassword, error) {
	f.note(ctx, r)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.devicePasswords == nil {
		f.devicePasswords = map[string][]identity.DevicePassword{}
	}
	key := r.Name() + "/" + id
	for _, d := range f.devicePasswords[key] {
		if d.Name == name {
			return identity.NewDevicePassword{}, identity.ErrInvalid
		}
	}
	d := identity.DevicePassword{ID: "dp-" + name, Name: name, Ways: ways, CreatedAt: time.Now()}
	f.devicePasswords[key] = append(f.devicePasswords[key], d)
	return identity.NewDevicePassword{DevicePassword: d, Username: id, Password: "k3m9x-p2hrt-aaaaa-bbbbb-ccccc"}, nil
}

func (f *fakeIdentity) RevokeDevicePassword(ctx context.Context, r identity.Realm, id, passwordID string) error {
	f.note(ctx, r)
	f.mu.Lock()
	defer f.mu.Unlock()
	key := r.Name() + "/" + id
	for i, d := range f.devicePasswords[key] {
		if d.ID == passwordID {
			f.devicePasswords[key] = append(f.devicePasswords[key][:i], f.devicePasswords[key][i+1:]...)
			return nil
		}
	}
	return identity.ErrNotFound
}

const (
	listDevicePasswords  = "/v1/tenants/demo/device-passwords"
	createDevicePassword = "/v1/tenants/demo/actions/create-device-password"
	revokeDevicePassword = "/v1/tenants/demo/actions/revoke-device-password"
)

// signedInAt mints mia's token of demo, signed in at the given time.
func (h *harness) signedInAt(t *testing.T, realm, sub string, at time.Time) string {
	return h.issuer.Token(t, dt.Claims{Realm: realm, Subject: sub, Audience: audience, Name: sub, Email: sub + "@example.com",
		Extra: map[string]any{"auth_time": at.Unix()}})
}

// A person makes a device password for themself after a fresh sign-in, sees
// it once, finds it listed without its secret, and revokes it. Nobody else
// is touched, whatever the request names.
func TestAPersonMakesListsAndRevokesTheirOwnDevicePassword(t *testing.T) {
	f := vouchingFake()
	h := startVouching(t, f, nil)
	mia := h.signedInAt(t, "demo", "mia", time.Now().Add(-time.Minute))

	// A body that names somebody is not read at all.
	if status, _ := h.do(t, http.MethodPost, createDevicePassword, mia, `{"name":"Mia's phone","ways":["calendars"],"person":"tom"}`); status != http.StatusBadRequest {
		t.Fatalf("a body naming somebody else: %d", status)
	}
	status, body := h.do(t, http.MethodPost, createDevicePassword, mia, `{"name":"Mia's phone","ways":["calendars"]}`)
	if status != http.StatusCreated || body["password"] == "" || body["name"] != "Mia's phone" {
		t.Fatalf("making one: %d %v", status, body)
	}
	if len(f.devicePasswords["demo/tom"]) != 0 || len(f.devicePasswords["demo/mia"]) != 1 {
		t.Fatalf("made %v", f.devicePasswords)
	}
	status, body = h.do(t, http.MethodGet, listDevicePasswords, mia, "")
	list, _ := body["devicePasswords"].([]any)
	if status != http.StatusOK || len(list) != 1 {
		t.Fatalf("listing: %d %v", status, body)
	}
	if first, _ := list[0].(map[string]any); first["password"] != nil {
		t.Fatalf("the list carries the secret: %v", first)
	}
	if status, _ := h.do(t, http.MethodPost, revokeDevicePassword, mia, `{"id":"dp-Mia's phone"}`); status != http.StatusNoContent {
		t.Fatalf("revoking: %d", status)
	}
	if status, _ := h.do(t, http.MethodPost, revokeDevicePassword, mia, `{"id":"dp-Mia's phone"}`); status != http.StatusNotFound {
		t.Fatalf("revoking one that is gone: %d", status)
	}
	want := "user:mia can_enter tenant:demo"
	if q := h.asked.questions(); len(q) == 0 || q[0] != want {
		t.Fatalf("the store was asked %v, want %q", q, want)
	}
}

// A sign-in older than a few minutes makes no device password: the answer is
// a step-up challenge, and nothing is made.
func TestADevicePasswordAsksForAFreshSignIn(t *testing.T) {
	f := vouchingFake()
	h := startVouching(t, f, nil)
	for name, token := range map[string]string{
		"an hour ago": h.signedInAt(t, "demo", "mia", time.Now().Add(-time.Hour)),
		"never said":  h.token(t, "demo", "mia"),
	} {
		req, _ := http.NewRequest(http.MethodPost, h.URL+createDevicePassword, strings.NewReader(`{"name":"x","ways":["files"]}`))
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized ||
			!strings.Contains(resp.Header.Get("WWW-Authenticate"), `error="insufficient_user_authentication"`) ||
			!strings.Contains(resp.Header.Get("WWW-Authenticate"), "max_age=600") {
			t.Fatalf("signed in %s: %d %q", name, resp.StatusCode, resp.Header.Get("WWW-Authenticate"))
		}
	}
	if len(f.devicePasswords["demo/mia"]) != 0 {
		t.Fatal("made one without a fresh sign-in")
	}
}

// A device password is kept in the tenant's realm, so it is made and read
// with a token of that realm only.
func TestADevicePasswordIsMadeWithATokenOfTheTenantsRealm(t *testing.T) {
	f := vouchingFake()
	h := startVouching(t, f, nil)
	tina := h.signedInAt(t, "solo", "tina", time.Now())
	for _, call := range []struct{ method, path, body string }{
		{http.MethodGet, listDevicePasswords, ""},
		{http.MethodPost, createDevicePassword, `{"name":"x","ways":["files"]}`},
	} {
		if status, _ := h.do(t, call.method, call.path, tina, call.body); status != http.StatusForbidden {
			t.Fatalf("%s %s with another realm's token: %d", call.method, call.path, status)
		}
	}
}
