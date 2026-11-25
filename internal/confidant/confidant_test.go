/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package confidant

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"github.com/gentian-org/gentian-os/internal/componentkeys"
	"github.com/gentian-org/gentian-os/internal/director/authn"
	"github.com/gentian-org/gentian-os/internal/kernel/secrets"
)

// vault is the vault as far as the confidant asks it: paths and what is at
// each.
type vault struct {
	held   map[string]string
	asked  []string
	silent bool
}

func (v *vault) Get(_ context.Context, p string) (map[string]string, error) {
	if v.silent {
		return nil, errors.New("sealed")
	}
	value, ok := v.held[p]
	if !ok {
		return nil, fmt.Errorf("get %s: %w", p, secrets.ErrNotFound)
	}
	return map[string]string{"value": value}, nil
}

func (v *vault) Put(_ context.Context, p string, data map[string]string) error {
	v.held[p] = data["value"]
	return nil
}

func (v *vault) DeleteTree(_ context.Context, p string) error {
	for held := range v.held {
		if held == p || strings.HasPrefix(held, p+"/") {
			delete(v.held, held)
		}
	}
	return nil
}

func (v *vault) ListChildren(_ context.Context, p string) ([]string, error) {
	var out []string
	for held := range v.held {
		if strings.HasPrefix(held, p+"/") {
			out = append(out, strings.TrimPrefix(held, p+"/"))
		}
	}
	sort.Strings(out)
	return out, nil
}

func (v *vault) Request(_ context.Context, method, path string, body any) (int, []byte, error) {
	v.asked = append(v.asked, fmt.Sprintf("%s %s %v", method, path, body))
	return http.StatusNoContent, nil, nil
}

// tokens verifies the tokens a test names.
type tokens map[string]authn.Identity

func (t tokens) VerifyFor(_ context.Context, raw, audience string) (*authn.Identity, error) {
	ident, ok := t[raw+"@"+audience]
	if !ok {
		return nil, errors.New("does not verify")
	}
	return &ident, nil
}

const (
	liaisonKey = "liaison-key"
	otherKey   = "other-key"
	ada        = "0d3b2a4e-ada"
	grace      = "7c1f9b02-grace"
)

type fixture struct {
	*Server
	vault *vault
	log   *bytes.Buffer
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	keys := &Keys{}
	keys.Set([]componentkeys.PersonSecrets{
		{Key: componentkeys.Key{Tenant: "acme", Component: "liaison", KeyHash: componentkeys.Hash(liaisonKey)}, Profile: "liaison-ee", Realm: "acme"},
		{Key: componentkeys.Key{Tenant: "acme", Component: "other", KeyHash: componentkeys.Hash(otherKey)}, Profile: "other-ee", Realm: "acme"},
	})
	v := &vault{held: map[string]string{}}
	log := &bytes.Buffer{}
	srv, err := New(Config{
		Keys:  keys,
		Vault: v,
		Log:   slog.New(slog.NewJSONHandler(log, nil)),
		Tokens: tokens{
			"ada-at-liaison@liaison-ee":   {Subject: ada, Realm: "acme", Client: issuedTo},
			"ada-direct@liaison-ee":       {Subject: ada, Realm: "acme", Client: "some-app"},
			"ada-elsewhere@liaison-ee":    {Subject: ada, Realm: "globex", Client: issuedTo},
			"grace-at-liaison@liaison-ee": {Subject: grace, Realm: "acme", Client: issuedTo},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return &fixture{Server: srv, vault: v, log: log}
}

func (f *fixture) do(method, path, key, personToken, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if key != "" {
		r.Header.Set("Authorization", "Bearer "+key)
	}
	if personToken != "" {
		r.Header.Set(PersonTokenHeader, personToken)
	}
	w := httptest.NewRecorder()
	f.ServeHTTP(w, r)
	return w
}

func entry(person, name string) string { return "/v1/people/" + person + "/secrets/" + name }

func TestAComponentKeepsAndReadsAPersonsSecret(t *testing.T) {
	f := newFixture(t)
	if w := f.do("PUT", entry(ada, "plaud"), liaisonKey, "ada-at-liaison", `{"value":"s3cr3t-one"}`); w.Code != http.StatusNoContent {
		t.Fatalf("first write: %d %s", w.Code, w.Body)
	}
	at := "gentian-os/tenants/acme/people/" + ada + "/liaison/plaud"
	if f.vault.held[at] != "s3cr3t-one" {
		t.Fatalf("kept at %v, want %s", f.vault.held, at)
	}
	if len(f.vault.asked) != 1 || !strings.Contains(f.vault.asked[0], "secret/metadata/"+at) || !strings.Contains(f.vault.asked[0], "max_versions:1") {
		t.Fatalf("one version is asked for: %v", f.vault.asked)
	}
	w := f.do("GET", entry(ada, "plaud"), liaisonKey, "", "")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"value":"s3cr3t-one"`) {
		t.Fatalf("read: %d %s", w.Code, w.Body)
	}
	// A renewal while the person is away: the key alone.
	if w := f.do("PUT", entry(ada, "plaud"), liaisonKey, "", `{"value":"s3cr3t-two"}`); w.Code != http.StatusNoContent {
		t.Fatalf("renewal: %d %s", w.Code, w.Body)
	}
	if f.vault.held[at] != "s3cr3t-two" {
		t.Fatal("the renewal did not replace the entry")
	}
	// Nothing of a value is ever logged.
	if log := f.log.String(); strings.Contains(log, "s3cr3t") {
		t.Fatalf("a value reached the log: %s", log)
	}
	for _, want := range []string{`"action":"write"`, `"action":"read"`, `"outcome":"done"`, `"person":"` + ada + `"`, `"component":"liaison"`, `"name":"plaud"`} {
		if !strings.Contains(f.log.String(), want) {
			t.Errorf("the record lacks %s: %s", want, f.log)
		}
	}
}

func TestTheFirstEntryForAPersonNeedsTheirToken(t *testing.T) {
	f := newFixture(t)
	for name, token := range map[string]string{
		"no token":                     "",
		"a token of another person":    "grace-at-liaison",
		"a token issued elsewhere":     "ada-direct",
		"a token of another realm":     "ada-elsewhere",
		"a token that does not verify": "forged",
	} {
		if w := f.do("PUT", entry(ada, "plaud"), liaisonKey, token, `{"value":"x"}`); w.Code != http.StatusForbidden {
			t.Errorf("%s: %d, want 403", name, w.Code)
		}
	}
	if len(f.vault.held) != 0 {
		t.Fatalf("something was kept: %v", f.vault.held)
	}
	if !strings.Contains(f.log.String(), `"outcome":"refused"`) {
		t.Fatal("a refusal is recorded")
	}
	// A token that is presented decides, also once the space is open.
	f.do("PUT", entry(ada, "plaud"), liaisonKey, "ada-at-liaison", `{"value":"x"}`)
	if w := f.do("PUT", entry(ada, "plaud"), liaisonKey, "grace-at-liaison", `{"value":"y"}`); w.Code != http.StatusForbidden {
		t.Fatalf("a wrong token beside an open space: %d, want 403", w.Code)
	}
}

func TestAComponentReachesItsOwnEntriesOnly(t *testing.T) {
	f := newFixture(t)
	f.do("PUT", entry(ada, "plaud"), liaisonKey, "ada-at-liaison", `{"value":"x"}`)
	if w := f.do("GET", entry(ada, "plaud"), otherKey, "", ""); w.Code != http.StatusNotFound {
		t.Fatalf("another component reads it: %d", w.Code)
	}
	// The other component's space for the person is not open because this
	// one's is.
	if w := f.do("PUT", entry(ada, "plaud"), otherKey, "", `{"value":"y"}`); w.Code != http.StatusForbidden {
		t.Fatalf("another component writes beside it: %d", w.Code)
	}
	for _, key := range []string{"", "nobody's"} {
		if w := f.do("GET", entry(ada, "plaud"), key, "", ""); w.Code != http.StatusUnauthorized {
			t.Fatalf("key %q: %d, want 401", key, w.Code)
		}
	}
}

func TestRemovingEveryEntryClosesTheSpace(t *testing.T) {
	f := newFixture(t)
	f.do("PUT", entry(ada, "plaud"), liaisonKey, "ada-at-liaison", `{"value":"x"}`)
	f.do("PUT", entry(ada, "other"), liaisonKey, "", `{"value":"y"}`)
	if w := f.do("DELETE", entry(ada, "other"), liaisonKey, "", ""); w.Code != http.StatusNoContent {
		t.Fatalf("remove one: %d", w.Code)
	}
	if len(f.vault.held) != 1 {
		t.Fatalf("held %v", f.vault.held)
	}
	if w := f.do("DELETE", "/v1/people/"+ada, liaisonKey, "", ""); w.Code != http.StatusNoContent {
		t.Fatalf("remove all: %d", w.Code)
	}
	if len(f.vault.held) != 0 {
		t.Fatalf("held %v", f.vault.held)
	}
	if w := f.do("PUT", entry(ada, "plaud"), liaisonKey, "", `{"value":"z"}`); w.Code != http.StatusForbidden {
		t.Fatalf("a write after everything was removed: %d, want 403", w.Code)
	}
}

func TestWhatIsNotAnEntryIsRefused(t *testing.T) {
	f := newFixture(t)
	for _, path := range []string{entry(ada, "Plaud"), entry(ada, "a_b"), entry("a.b", "plaud"), entry("a%2Fb", "plaud")} {
		if w := f.do("GET", path, liaisonKey, "", ""); w.Code != http.StatusBadRequest {
			t.Errorf("%s: %d, want 400", path, w.Code)
		}
	}
	big := `{"value":"` + strings.Repeat("a", MaxValueBytes+1) + `"}`
	for _, body := range []string{``, `{}`, `{"value":""}`, big} {
		if w := f.do("PUT", entry(ada, "plaud"), liaisonKey, "ada-at-liaison", body); w.Code != http.StatusBadRequest {
			t.Errorf("body of %d bytes: %d, want 400", len(body), w.Code)
		}
	}
}

func TestAVaultThatDoesNotAnswerIsSaid(t *testing.T) {
	f := newFixture(t)
	f.vault.silent = true
	if w := f.do("GET", entry(ada, "plaud"), liaisonKey, "", ""); w.Code != http.StatusBadGateway {
		t.Fatalf("%d, want 502", w.Code)
	}
}
