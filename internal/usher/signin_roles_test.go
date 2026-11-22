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
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gentian-org/gentian-os/internal/director/authz"
	"github.com/gentian-org/gentian-os/internal/membership"
)

type fakeSignInRoles struct {
	asked []string
	err   error
}

func (f *fakeSignInRoles) SignInRoles(_ context.Context, _, tenant, profile, sub string) ([]authz.AppRoleSet, error) {
	f.asked = append(f.asked, tenant+"/"+profile+"/"+sub)
	if f.err != nil {
		return nil, f.err
	}
	return []authz.AppRoleSet{{App: profile, Roles: []string{"user"}}}, nil
}

func signed(t *testing.T, key ed25519.PrivateKey, keyID string, at time.Time, body string) *http.Request {
	t.Helper()
	ts := fmt.Sprint(at.Unix())
	sig := base64.StdEncoding.EncodeToString(ed25519.Sign(key, []byte(ts+"."+body)))
	req := httptest.NewRequest(http.MethodPost, "/v1/sign-in-roles", strings.NewReader(body))
	req.Header.Set(membership.SignatureHeader, "keyid="+keyID+",t="+ts+",sig="+sig)
	return req
}

// The sign-in question is answered to the identity provider alone: a request
// one of its listed keys signed, recently.
func TestTheSignInQuestionIsAnsweredToASignedRequestOnly(t *testing.T) {
	pub, key, _ := ed25519.GenerateKey(nil)
	_, other, _ := ed25519.GenerateKey(nil)
	now := time.Unix(1_800_000_000, 0)
	roles := &fakeSignInRoles{}
	h, err := SignInRolesHandler(SignInRolesConfig{
		Keys: map[string]ed25519.PublicKey{"listener": pub}, Roles: roles, Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	body := `{"tenant":"acme","app":"odoo-base-ce","person":"p-1"}`
	serve := func(req *http.Request) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	rec := serve(signed(t, key, "listener", now, body))
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != `{"apps":[{"app":"odoo-base-ce","roles":["user"]}]}` {
		t.Fatalf("answer = %d %s", rec.Code, rec.Body.String())
	}
	if len(roles.asked) != 1 || roles.asked[0] != "acme/odoo-base-ce/p-1" {
		t.Errorf("asked = %v", roles.asked)
	}

	refused := map[string]*http.Request{
		"no signature":       httptest.NewRequest(http.MethodPost, "/v1/sign-in-roles", strings.NewReader(body)),
		"another key":        signed(t, other, "listener", now, body),
		"an unlisted key id": signed(t, key, "someone", now, body),
		"an old signature":   signed(t, key, "listener", now.Add(-10*time.Minute), body),
	}
	tampered := signed(t, key, "listener", now, body)
	tampered.Body = httptest.NewRequest(http.MethodPost, "/", strings.NewReader(strings.Replace(body, "p-1", "p-2", 1))).Body
	refused["a body the signature is not over"] = tampered
	for name, req := range refused {
		if rec := serve(req); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s: %d, want 401", name, rec.Code)
		}
	}
	if len(roles.asked) != 1 {
		t.Errorf("the store was asked for a refused request: %v", roles.asked)
	}

	for name, bad := range map[string]string{
		"no person":           `{"tenant":"acme","app":"odoo-base-ce"}`,
		"an app of another":   `{"tenant":"acme","app":"other/odoo","person":"p-1"}`,
		"a tenant not a name": `{"tenant":"Acme Inc","app":"odoo","person":"p-1"}`,
		"not JSON":            `tenant=acme`,
	} {
		if rec := serve(signed(t, key, "listener", now, bad)); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: %d, want 400", name, rec.Code)
		}
	}
}

// A store that cannot be asked is not "no roles": the asker is told, and
// fails the sign-in.
func TestAnUnansweredSignInQuestionIsNotAnEmptyAnswer(t *testing.T) {
	pub, key, _ := ed25519.GenerateKey(nil)
	now := time.Unix(1_800_000_000, 0)
	h, err := SignInRolesHandler(SignInRolesConfig{
		Keys: map[string]ed25519.PublicKey{"listener": pub}, Roles: &fakeSignInRoles{err: errors.New("store away")},
		Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, signed(t, key, "listener", now, `{"tenant":"acme","app":"odoo","person":"p-1"}`))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("answer = %d, want 503", rec.Code)
	}
	if _, err := SignInRolesHandler(SignInRolesConfig{Roles: &fakeSignInRoles{}}); err == nil {
		t.Error("a handler with no key would answer anybody")
	}
}
