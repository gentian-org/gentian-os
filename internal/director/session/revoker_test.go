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

package session

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/gentian-org/gentian-os/internal/director/authn"
)

type fakeStore struct {
	revoked []string
	swept   []time.Time
	fail    error
}

func (f *fakeStore) RevokeSession(_ context.Context, sub, sid string) error {
	if f.fail != nil {
		return f.fail
	}
	f.revoked = append(f.revoked, sub+"/"+sid)
	return nil
}

func (f *fakeStore) SweepRevocations(_ context.Context, olderThan time.Time) (int, error) {
	f.swept = append(f.swept, olderThan)
	return 0, nil
}

type fakeVerifier struct{ ok map[string]*authn.Logout }

func (f fakeVerifier) VerifyLogout(_ context.Context, raw string) (*authn.Logout, error) {
	if l, ok := f.ok[raw]; ok {
		return l, nil
	}
	return nil, authn.ErrUnauthenticated
}

func post(t *testing.T, h http.Handler, form url.Values) *http.Response {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/logout/keycloak", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Result()
}

func TestALogoutTokenEndsTheSessionInTheStore(t *testing.T) {
	store := &fakeStore{}
	rc, err := New(fakeVerifier{ok: map[string]*authn.Logout{"good": {Subject: "u1", SessionID: "s1", Realm: "kernel"}}},
		store, 12*time.Hour, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	resp := post(t, rc, url.Values{"logout_token": {"good"}})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if len(store.revoked) != 1 || store.revoked[0] != "u1/s1" {
		t.Fatalf("revoked = %v", store.revoked)
	}
}

func TestWhatIsNotALogoutTokenIsRefusedAndNothingIsWritten(t *testing.T) {
	store := &fakeStore{}
	rc, _ := New(fakeVerifier{ok: map[string]*authn.Logout{}}, store, time.Hour, slog.New(slog.NewTextHandler(io.Discard, nil)))
	for name, form := range map[string]url.Values{
		"a token the verifier refuses": {"logout_token": {"forged"}},
		"no token at all":              {},
	} {
		t.Run(name, func(t *testing.T) {
			if resp := post(t, rc, form); resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", resp.StatusCode)
			}
		})
	}
	if len(store.revoked) != 0 {
		t.Fatalf("revoked = %v, want nothing", store.revoked)
	}
}

// Keycloak retries a failed delivery; a store that cannot be written is
// reported as such, not swallowed as a success.
func TestAStoreThatCannotBeWrittenIsAskedAgain(t *testing.T) {
	store := &fakeStore{fail: errors.New("openfga down")}
	rc, _ := New(fakeVerifier{ok: map[string]*authn.Logout{"good": {Subject: "u1", SessionID: "s1"}}},
		store, time.Hour, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if resp := post(t, rc, url.Values{"logout_token": {"good"}}); resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
}

func TestTheSweepRetiresWhatIsOlderThanTheTTL(t *testing.T) {
	store := &fakeStore{}
	rc, _ := New(fakeVerifier{}, store, 2*time.Hour, slog.New(slog.NewTextHandler(io.Discard, nil)))
	now := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	rc.now = func() time.Time { return now }
	rc.sweep(context.Background())
	if len(store.swept) != 1 || !store.swept[0].Equal(now.Add(-2*time.Hour)) {
		t.Fatalf("swept = %v", store.swept)
	}
}
