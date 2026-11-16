/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package bouncer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gentian-org/gentian-os/internal/director/authn"
)

// lateVerifier refuses "stale-token" as expired nine seconds ago, as the real
// verifier does, and knows the rest as fakeVerifier does.
type lateVerifier struct{ fakeVerifier }

func (v lateVerifier) Verify(ctx context.Context, raw string) (*authn.Identity, error) {
	if raw == "stale-token" {
		return nil, &authn.Refusal{Cause: authn.CauseExpired, Late: 9 * time.Second,
			Err: errors.New("unauthenticated: go-jose/go-jose/jwt: validation failed, token is expired (exp)")}
	}
	return v.fakeVerifier.Verify(ctx, raw)
}

// The audit line of a session that was not accepted says why, as a word, and
// for an expired token by how much; the verifier's text stays out of it.
func TestARefusedSessionNamesItsCause(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	d := restartDecider(&fakeStore{allow: map[string]bool{}})
	d.verify = lateVerifier{d.verify.(fakeVerifier)}
	d.log = slog.New(slog.NewJSONHandler(&buf, nil))

	fetch := withAuth(page("argocd.k.example", "/api/v1/applications", ""), "Bearer stale-token")
	fetch.FetchMode = "cors"
	for _, r := range []Request{
		fetch, // an expired token on a fetch: the 401
		withAuth(page("argocd.k.example", "/", endedSession), "Bearer stale-token"), // on a page: the restart
		page("argocd.k.example", "/", ""),                                           // no token at all
		withAuth(page("argocd.k.example", "/", ""), "Bearer no-session-token"),
		withAuth(page("argocd.k.example", "/", ""), "Bearer forged"), // a refusal that carries no cause
	} {
		d.Decide(context.Background(), r)
	}
	want := []struct {
		reason, cause string
		lateMS        float64
	}{
		{"unauthenticated", authn.CauseExpired, 9000},
		{"session-ended", authn.CauseExpired, 9000},
		{"unauthenticated", authn.CauseNoToken, 0},
		{"unauthenticated", causeNoSession, 0},
		{"unauthenticated", "other", 0},
	}
	var got []map[string]any
	for _, l := range bytes.Split(bytes.TrimSpace(buf.Bytes()), []byte("\n")) {
		if bytes.Contains(l, []byte("token is expired")) {
			t.Fatalf("the verifier's text reached the log: %s", l)
		}
		var m map[string]any
		if err := json.Unmarshal(l, &m); err != nil {
			t.Fatal(err)
		}
		if m["audit"] == true {
			got = append(got, m["detail"].(map[string]any))
		}
	}
	if len(got) != len(want) {
		t.Fatalf("%d audit lines, want %d: %v", len(got), len(want), got)
	}
	for i, w := range want {
		late, _ := got[i]["expired_by_ms"].(float64)
		if got[i]["reason"] != w.reason || got[i]["cause"] != w.cause || late != w.lateMS {
			t.Errorf("line %d = %v, want reason %s, cause %s, expired_by_ms %v", i, got[i], w.reason, w.cause, w.lateMS)
		}
	}
}

// An allowed request, or one refused for its access, has no cause: the cause
// is about a session that was not accepted, and nothing else.
func TestOnlyARefusedSessionHasACause(t *testing.T) {
	t.Parallel()
	lines := auditLines(t, &fakeStore{allow: map[string]bool{"user:root|can_configure|cluster:c1": true}},
		withAuth(page("argocd.k.example", "/", ""), "Bearer root-token"),
		withAuth(page("argocd.k.example", "/", ""), "Bearer mia-token"))
	for _, l := range lines {
		if detail := l["detail"].(map[string]any); detail["cause"] != nil || detail["expired_by_ms"] != nil {
			t.Fatalf("detail = %v", detail)
		}
	}
}

// The page for a session that could not be renewed reloads the page; it never
// signs the person out, which would end the realm's session as well.
func TestASessionNotRenewedIsReloadedNotSignedOut(t *testing.T) {
	t.Parallel()
	page := deniedPage("", http.StatusUnauthorized)
	if !strings.Contains(page, `<a href="">Reload</a>`) || strings.Contains(page, LogoutPath) {
		t.Fatalf("page = %s", page)
	}
}

// keylessVerifier is a Keycloak that does not answer for its keys.
type keylessVerifier struct{ fakeVerifier }

func (keylessVerifier) Verify(context.Context, string) (*authn.Identity, error) {
	return nil, &authn.Refusal{Cause: authn.CauseKeysUnavailable, Err: errors.New("unauthenticated: keys for realm \"kernel\": connection refused")}
}

// A token whose keys cannot be fetched is not judged: the answer is 503,
// which a page and a program retry, and not the 401 that ends a session or
// restarts its sign-in. Nothing reaches a backend.
func TestKeysThatCannotBeFetchedAreUnavailableNotUnauthenticated(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	d := restartDecider(&fakeStore{allow: map[string]bool{}})
	d.verify = keylessVerifier{d.verify.(fakeVerifier)}
	d.log = slog.New(slog.NewJSONHandler(&buf, nil))
	fetch := withAuth(page("argocd.k.example", "/api/v1/applications", endedSession), "Bearer root-token")
	fetch.FetchMode = "cors"
	for name, req := range map[string]Request{
		"a page":  withAuth(page("argocd.k.example", "/", endedSession), "Bearer root-token"),
		"a fetch": fetch,
	} {
		dec := d.Decide(context.Background(), req)
		if dec.Allow || dec.Status != http.StatusServiceUnavailable || dec.Redirect != "" || len(dec.SetCookies) != 0 || dec.Browser {
			t.Fatalf("%s: %+v", name, dec)
		}
	}
	if !bytes.Contains(buf.Bytes(), []byte(`"reason":"unavailable"`)) || !bytes.Contains(buf.Bytes(), []byte(`"cause":"keys-unavailable"`)) {
		t.Fatalf("audit = %s", buf.String())
	}
}
