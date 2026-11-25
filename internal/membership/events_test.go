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
	"crypto/ed25519"
	"encoding/base64"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

type told struct {
	entered    [][4]string
	registered [][2]string
}

func (t *told) PersonEntered(_ context.Context, realm, sub, provider, providerUser string) error {
	t.entered = append(t.entered, [4]string{realm, sub, provider, providerUser})
	return nil
}

func (t *told) PersonRegistered(_ context.Context, realm, sub string) error {
	t.registered = append(t.registered, [2]string{realm, sub})
	return nil
}

// enteredOnly is told of arrivals and nothing else.
type enteredOnly struct{ n int }

func (e *enteredOnly) PersonEntered(context.Context, string, string, string, string) error {
	e.n++
	return nil
}

func post(t *testing.T, rc *Receiver, priv ed25519.PrivateKey, body string) int {
	t.Helper()
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	sig := base64.StdEncoding.EncodeToString(ed25519.Sign(priv, []byte(ts+"."+body)))
	req := httptest.NewRequest(http.MethodPost, "/v1/events/keycloak", strings.NewReader(body))
	req.Header.Set(SignatureHeader, "keyid=k1,t="+ts+",sig="+sig)
	rec := httptest.NewRecorder()
	rc.ServeHTTP(rec, req)
	return rec.Code
}

func receiver(t *testing.T, arrivals Arrivals) (*Receiver, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	rc, err := NewReceiver(map[string]ed25519.PublicKey{"k1": pub}, arrivals, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return rc, priv
}

// The statements as the listener inside Keycloak writes them
// (kernel/extensions/keycloak-event-listener, Payload.java: the two must
// agree): an arrival with and without who the person is at the provider,
// and an account a person made themself.
func TestReceiverTellsArrivalsAndRegistrations(t *testing.T) {
	got := &told{}
	rc, priv := receiver(t, got)

	for _, c := range []struct {
		body string
		want int
	}{
		{`{"id":"00000000-0000-0000-0000-000000000001","time":1,"realm":"shop","type":"user.entered","user":"u-1","provider":"tenant-team","providerUser":"t-1"}`, http.StatusNoContent},
		{`{"id":"00000000-0000-0000-0000-000000000002","time":1,"realm":"shop","type":"user.entered","user":"u-2","provider":"kernel"}`, http.StatusNoContent},
		{`{"id":"00000000-0000-0000-0000-000000000003","time":1,"realm":"shop","type":"user.registered","user":"u-3"}`, http.StatusNoContent},
		// Who the person is at the provider is an id, not text.
		{`{"id":"00000000-0000-0000-0000-000000000004","time":1,"realm":"shop","type":"user.entered","user":"u-4","provider":"tenant-team","providerUser":"a b"}`, http.StatusBadRequest},
		{`{"id":"00000000-0000-0000-0000-000000000005","time":1,"realm":"shop","type":"user.registered"}`, http.StatusBadRequest},
	} {
		if code := post(t, rc, priv, c.body); code != c.want {
			t.Errorf("%s answered %d, want %d", c.body, code, c.want)
		}
	}
	want := [][4]string{{"shop", "u-1", "tenant-team", "t-1"}, {"shop", "u-2", "kernel", ""}}
	if len(got.entered) != len(want) || got.entered[0] != want[0] || got.entered[1] != want[1] {
		t.Errorf("entered = %v, want %v", got.entered, want)
	}
	if len(got.registered) != 1 || got.registered[0] != [2]string{"shop", "u-3"} {
		t.Errorf("registered = %v", got.registered)
	}
}

// An Arrivals that is not told of registrations accepts them all the same.
func TestARegistrationNobodyIsToldOfIsAccepted(t *testing.T) {
	e := &enteredOnly{}
	rc, priv := receiver(t, e)
	body := `{"id":"00000000-0000-0000-0000-000000000006","time":1,"realm":"shop","type":"user.registered","user":"u-1"}`
	if code := post(t, rc, priv, body); code != http.StatusNoContent {
		t.Fatalf("answered %d", code)
	}
	if e.n != 0 {
		t.Fatal("a registration was told as an arrival")
	}
}
