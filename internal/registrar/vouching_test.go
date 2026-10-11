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
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/gentian-org/gentian-os/internal/director/authn"
	dt "github.com/gentian-org/gentian-os/internal/director/directortest"
	"github.com/gentian-org/gentian-os/internal/registrar"
	"github.com/gentian-org/gentian-os/internal/registrar/identity"
)

// The fake's half of the links: a realm has the issuers it was told of, and
// refuses a link to any other, as the real client does.

func (f *fakeIdentity) declare(realm, profile string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.issuers == nil {
		f.issuers = map[string]bool{}
	}
	f.issuers[realm+"/"+profile] = true
}

func (f *fakeIdentity) linkedPeople() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.links))
	for l := range f.links {
		out = append(out, l)
	}
	sort.Strings(out)
	return out
}

func (f *fakeIdentity) VouchingLinked(ctx context.Context, r identity.Realm, id, profile string) (bool, error) {
	f.note(ctx, r)
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.links[r.Name()+"/"+profile+"/"+id], nil
}

func (f *fakeIdentity) LinkVouching(ctx context.Context, r identity.Realm, id, profile string) error {
	f.note(ctx, r)
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.issuers[r.Name()+"/"+profile] {
		return identity.ErrNoSuchIssuer
	}
	if f.links == nil {
		f.links = map[string]bool{}
	}
	f.links[r.Name()+"/"+profile+"/"+id] = true
	return nil
}

func (f *fakeIdentity) UnlinkVouching(ctx context.Context, r identity.Realm, id, profile string) error {
	f.note(ctx, r)
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.links, r.Name()+"/"+profile+"/"+id)
	return nil
}

// vouchingFacts is what model v1 answers for the fixture, for the questions
// these routes ask: tom administers demo, mia is a member of it, tina
// administers solo.
var vouchingFacts = dt.Table{
	"user:tom can_enter tenant:demo":        true,
	"user:tom can_manage_users tenant:demo": true,
	"user:tom can_expose tenant:demo":       true,
	"user:mia can_enter tenant:demo":        true,
	"user:tina can_enter tenant:solo":       true,
}

// The keys the operator gave three components, and the list it wrote of
// them. The list holds the hashes.
const (
	notaryOfDemo = "key-of-the-notary-of-demo"
	scribeOfDemo = "key-of-the-scribe-of-demo"
	notaryOfSolo = "key-of-the-notary-of-solo"
)

func hashOf(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

func operatorsList() []registrar.VouchingKey {
	return []registrar.VouchingKey{
		{Tenant: "demo", Component: "notary", KeyHash: hashOf(notaryOfDemo)},
		{Tenant: "demo", Component: "scribe", KeyHash: hashOf(scribeOfDemo)},
		{Tenant: "solo", Component: "notary", KeyHash: hashOf(notaryOfSolo)},
	}
}

// startVouching is the registrar on a cluster whose tenants each have a
// realm of their own name, so that which realm issued a token can be told
// from the tenant it is used at. keys is the operator's list, or nil for a
// registrar that was given none.
func startVouching(t *testing.T, f *fakeIdentity, keys []registrar.VouchingKey) *harness {
	t.Helper()
	is := dt.NewIssuer(t, "gentian", "demo", "solo")
	v, err := authn.NewVerifier(authn.Config{IssuerBase: is.URL, Audience: audience})
	if err != nil {
		t.Fatal(err)
	}
	var held *registrar.VouchingKeys
	if keys != nil {
		held = &registrar.VouchingKeys{}
		held.Set(keys)
	}
	decisions := &asked{Checker: dt.Checker(t, vouchingFacts)}
	srv, err := registrar.New(registrar.Config{
		Authn: v, Authz: decisions, Cluster: dt.Cluster,
		Tenants:      fakeTenants{"demo": ownRealm("demo"), "solo": ownRealm("solo")},
		Identity:     f,
		Log:          slog.New(slog.NewTextHandler(io.Discard, nil)),
		VouchingKeys: held,
	})
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{Server: httptest.NewServer(srv), issuer: is, asked: decisions}
	t.Cleanup(h.Close)
	return h
}

func vouchingFake() *fakeIdentity {
	f := newFakeIdentity("demo", "solo")
	f.declare("demo", "notary")
	f.declare("demo", "scribe")
	f.declare("solo", "notary")
	return f
}

const (
	linkNotary  = "/v1/tenants/demo/vouching/notary/link"
	miaAtNotary = "/v1/tenants/demo/vouching/notary/people/mia"
	tomAtNotary = "/v1/tenants/demo/vouching/notary/people/tom"
)

// A person links themself with their own token, and linking twice is the
// same as linking once. The store is asked whether they may enter the
// tenant, and nothing wider.
func TestAPersonLinksThemself(t *testing.T) {
	f := vouchingFake()
	h := startVouching(t, f, operatorsList())
	mia := h.token(t, "demo", "mia")
	for range 2 {
		if status, body := h.do(t, http.MethodPost, linkNotary, mia, ""); status != http.StatusNoContent {
			t.Fatalf("mia linking herself: %d %v", status, body)
		}
	}
	if got := f.linkedPeople(); strings.Join(got, ",") != "demo/notary/mia" {
		t.Fatalf("linked = %v, want mia at the notary of demo alone", got)
	}
	want := "user:mia can_enter tenant:demo"
	if q := h.asked.questions(); len(q) == 0 || q[0] != want {
		t.Fatalf("the store was asked %v, want %q first", q, want)
	}
	if status, body := h.do(t, http.MethodGet, miaAtNotary, mia, ""); status != http.StatusOK || body["linked"] != true {
		t.Fatalf("mia asking about herself: %d %v", status, body)
	}
}

// Nothing in the request names the person to link: a body that tries is not
// read, and the administrator who sends it links themself.
func TestNobodyLinksSomebodyElse(t *testing.T) {
	f := vouchingFake()
	h := startVouching(t, f, operatorsList())
	tom := h.token(t, "demo", "tom")
	for _, path := range []string{linkNotary, linkNotary + "?person=mia&id=mia"} {
		if status, _ := h.do(t, http.MethodPost, path, tom, `{"person":"mia","id":"mia","userId":"mia"}`); status != http.StatusNoContent {
			t.Fatalf("POST %s: %d", path, status)
		}
	}
	if got := f.linkedPeople(); strings.Join(got, ",") != "demo/notary/tom" {
		t.Fatalf("linked = %v: the administrator linked somebody other than himself", got)
	}
	// A component's key links nobody either: it is not a person's token.
	if status, _ := h.do(t, http.MethodPost, linkNotary, notaryOfDemo, ""); status != http.StatusUnauthorized {
		t.Fatalf("a component linking with its key: %d, want 401", status)
	}
	// Nor is there a way to link by the address the links are read at.
	if status, _ := h.do(t, http.MethodPost, miaAtNotary, tom, ""); status != http.StatusMethodNotAllowed {
		t.Fatalf("POST to a person's link: %d, want 405", status)
	}
	if got := f.linkedPeople(); len(got) != 1 {
		t.Fatalf("linked = %v", got)
	}
}

// A token another realm issued does not make its subject a person of this
// tenant, whatever the store says about that subject.
func TestATokenOfAnotherRealmLinksNobody(t *testing.T) {
	f := vouchingFake()
	h := startVouching(t, f, operatorsList())
	for _, realm := range []string{"solo", "gentian"} {
		foreign := h.token(t, realm, "mia")
		if status, _ := h.do(t, http.MethodPost, linkNotary, foreign, ""); status != http.StatusForbidden {
			t.Errorf("linking with a token of %s: %d, want 403", realm, status)
		}
		if status, _ := h.do(t, http.MethodDelete, miaAtNotary, foreign, ""); status != http.StatusForbidden {
			t.Errorf("unlinking oneself with a token of %s: %d, want 403", realm, status)
		}
		if status, _ := h.do(t, http.MethodGet, miaAtNotary, foreign, ""); status != http.StatusForbidden {
			t.Errorf("asking about oneself with a token of %s: %d, want 403", realm, status)
		}
	}
	if got := f.linkedPeople(); len(got) != 0 {
		t.Fatalf("linked = %v", got)
	}
	// Somebody who may not enter the tenant is refused before the realm is
	// looked at.
	if status, _ := h.do(t, http.MethodPost, linkNotary, h.token(t, "demo", "tina"), ""); status != http.StatusForbidden {
		t.Fatalf("tina linking herself in demo: %d, want 403", status)
	}
}

// A link is made only to what was declared.
func TestALinkToSomethingNobodyDeclaredIsRefused(t *testing.T) {
	f := vouchingFake()
	h := startVouching(t, f, operatorsList())
	mia := h.token(t, "demo", "mia")
	status, body := h.do(t, http.MethodPost, "/v1/tenants/demo/vouching/nowhere/link", mia, "")
	if status != http.StatusNotFound || !strings.Contains(body["error"].(string), "requires.services.vouching") {
		t.Fatalf("linking to an alias that is not there: %d %v", status, body)
	}
	if status, _ := h.do(t, http.MethodPost, "/v1/tenants/demo/vouching/No_Such/link", mia, ""); status != http.StatusBadRequest {
		t.Fatalf("a name that is not a profile's: %d, want 400", status)
	}
	if got := f.linkedPeople(); len(got) != 0 {
		t.Fatalf("linked = %v", got)
	}
}

// The three who may take a link away each can, and taking away what is not
// there is the same answer.
func TestTheThreeWhoMayUnlink(t *testing.T) {
	for name, bearer := range map[string]func(h *harness) string{
		"the person":       func(h *harness) string { return h.token(t, "demo", "mia") },
		"an administrator": func(h *harness) string { return h.token(t, "demo", "tom") },
		"the component":    func(*harness) string { return notaryOfDemo },
	} {
		f := vouchingFake()
		h := startVouching(t, f, operatorsList())
		if status, _ := h.do(t, http.MethodPost, linkNotary, h.token(t, "demo", "mia"), ""); status != http.StatusNoContent {
			t.Fatalf("%s: linking mia: %d", name, status)
		}
		who := bearer(h)
		if status, body := h.do(t, http.MethodGet, miaAtNotary, who, ""); status != http.StatusOK || body["linked"] != true {
			t.Errorf("%s asking: %d %v", name, status, body)
		}
		for range 2 {
			if status, body := h.do(t, http.MethodDelete, miaAtNotary, who, ""); status != http.StatusNoContent {
				t.Errorf("%s unlinking: %d %v", name, status, body)
			}
		}
		if got := f.linkedPeople(); len(got) != 0 {
			t.Errorf("%s: still linked: %v", name, got)
		}
		if status, body := h.do(t, http.MethodGet, miaAtNotary, who, ""); status != http.StatusOK || body["linked"] != false {
			t.Errorf("%s asking afterwards: %d %v", name, status, body)
		}
	}
}

// A component's key reaches the links to that component, in its own tenant:
// not another tenant's, and not another component's of its own.
func TestAKeyOpensItsOwnLinksOnly(t *testing.T) {
	f := vouchingFake()
	h := startVouching(t, f, operatorsList())
	if status, _ := h.do(t, http.MethodPost, linkNotary, h.token(t, "demo", "mia"), ""); status != http.StatusNoContent {
		t.Fatal("linking mia")
	}
	before := len(f.realmsSeen())
	for name, key := range map[string]string{
		"another tenant's component":      notaryOfSolo,
		"another component of the tenant": scribeOfDemo,
	} {
		for _, method := range []string{http.MethodGet, http.MethodDelete} {
			if status, _ := h.do(t, method, miaAtNotary, key, ""); status != http.StatusForbidden {
				t.Errorf("%s: %s answered %d, want 403", name, method, status)
			}
		}
	}
	// A key nobody was given is not a caller at all.
	if status, _ := h.do(t, http.MethodDelete, miaAtNotary, "key-nobody-was-given", ""); status != http.StatusUnauthorized {
		t.Errorf("an unknown key: %d, want 401", status)
	}
	// And the hash of a key is not the key.
	if status, _ := h.do(t, http.MethodDelete, miaAtNotary, hashOf(notaryOfDemo), ""); status != http.StatusUnauthorized {
		t.Errorf("the hash of a key: %d, want 401", status)
	}
	if got := f.linkedPeople(); len(got) != 1 {
		t.Fatalf("a refused key unlinked somebody: %v", got)
	}
	if seen := f.realmsSeen(); len(seen) != before {
		t.Fatalf("a refused key reached %v", seen[before:])
	}
	if q := h.asked.questions(); len(q) != 2 {
		// The two questions of mia's own link, and none for a key: the
		// store knows people, and a component is not one.
		t.Fatalf("the store was asked %v", q)
	}
}

// A member takes away their own link and nobody else's.
func TestAMemberUnlinksOnlyThemself(t *testing.T) {
	f := vouchingFake()
	h := startVouching(t, f, operatorsList())
	if status, _ := h.do(t, http.MethodPost, linkNotary, h.token(t, "demo", "tom"), ""); status != http.StatusNoContent {
		t.Fatal("linking tom")
	}
	mia := h.token(t, "demo", "mia")
	before := len(h.asked.questions())
	for _, method := range []string{http.MethodGet, http.MethodDelete} {
		if status, _ := h.do(t, method, tomAtNotary, mia, ""); status != http.StatusForbidden {
			t.Errorf("mia, %s on tom's link: %d, want 403", method, status)
		}
	}
	for _, q := range h.asked.questions()[before:] {
		if q != "user:mia can_manage_users tenant:demo" {
			t.Errorf("for somebody else's link the store was asked %q", q)
		}
	}
	if got := f.linkedPeople(); strings.Join(got, ",") != "demo/notary/tom" {
		t.Fatalf("linked = %v", got)
	}
	// Tom administers demo and not solo.
	if status, _ := h.do(t, http.MethodDelete, "/v1/tenants/solo/vouching/notary/people/tina", h.token(t, "demo", "tom"), ""); status != http.StatusForbidden {
		t.Fatalf("tom at solo: %d, want 403", status)
	}
}

// A key is good for the two routes about its own links and is nobody
// anywhere else.
func TestAKeyDoesNothingElse(t *testing.T) {
	f := vouchingFake()
	h := startVouching(t, f, operatorsList())
	others := append(routes(), route{method: "POST", path: linkNotary})
	for _, rt := range others {
		if status, _ := h.do(t, rt.method, rt.path, notaryOfDemo, rt.body); status != http.StatusUnauthorized {
			t.Errorf("%s %s with a component's key: %d, want 401", rt.method, rt.path, status)
		}
	}
	if seen := f.realmsSeen(); len(seen) != 0 {
		t.Fatalf("a key reached %v", seen)
	}
	if q := h.asked.questions(); len(q) != 0 {
		t.Fatalf("the store was asked about a key: %v", q)
	}
}

// With no list from the operator no key is recognised, and the people who
// may link and unlink still can.
func TestWithoutAListNoKeyIsRecognised(t *testing.T) {
	f := vouchingFake()
	h := startVouching(t, f, nil)
	mia := h.token(t, "demo", "mia")
	if status, _ := h.do(t, http.MethodPost, linkNotary, mia, ""); status != http.StatusNoContent {
		t.Fatalf("mia linking herself: %d", status)
	}
	if status, _ := h.do(t, http.MethodDelete, miaAtNotary, notaryOfDemo, ""); status != http.StatusUnauthorized {
		t.Fatalf("a key with no list: %d, want 401", status)
	}
	if status, _ := h.do(t, http.MethodDelete, miaAtNotary, "", ""); status != http.StatusUnauthorized {
		t.Fatalf("nobody at all: %d, want 401", status)
	}
	if status, _ := h.do(t, http.MethodDelete, miaAtNotary, mia, ""); status != http.StatusNoContent {
		t.Fatalf("mia unlinking herself: %d", status)
	}
}

// The list is refused whole when an entry could not be decided by.
func TestTheListIsReadStrictly(t *testing.T) {
	good := `{"keys":[{"tenant":"demo","component":"notary","keyHash":"` + hashOf(notaryOfDemo) + `"}]}`
	if keys, err := registrar.ParseVouchingKeys([]byte(good)); err != nil || len(keys) != 1 {
		t.Fatalf("the operator's list: %v, %v", keys, err)
	}
	if keys, err := registrar.ParseVouchingKeys([]byte(`{"keys":[]}`)); err != nil || len(keys) != 0 {
		t.Fatalf("an empty list: %v, %v", keys, err)
	}
	for name, bad := range map[string]string{
		"not a list":   `nonsense`,
		"no tenant":    `{"keys":[{"component":"notary","keyHash":"` + hashOf("a") + `"}]}`,
		"no component": `{"keys":[{"tenant":"demo","keyHash":"` + hashOf("a") + `"}]}`,
		"a key":        `{"keys":[{"tenant":"demo","component":"notary","keyHash":"` + notaryOfDemo + `"}]}`,
		"a shared key": `{"keys":[{"tenant":"demo","component":"notary","keyHash":"` + hashOf("a") + `"},{"tenant":"solo","component":"notary","keyHash":"` + hashOf("a") + `"}]}`,
	} {
		if _, err := registrar.ParseVouchingKeys([]byte(bad)); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

// The list follows the file: an entry the operator drops stops counting, a
// file that cannot be read keeps the list before it, and a file that is gone
// is no list.
func TestTheListFollowsTheFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keys.json")
	write := func(content string) {
		t.Helper()
		tmp := path + ".next"
		if err := os.WriteFile(tmp, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		// A modification time of its own for each content, whatever the
		// clock's grain.
		when := time.Now().Add(time.Duration(len(content)) * time.Second)
		if err := os.Chtimes(tmp, when, when); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(tmp, path); err != nil {
			t.Fatal(err)
		}
	}
	held := &registrar.VouchingKeys{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go held.Follow(ctx, path, 5*time.Millisecond, slog.New(slog.NewTextHandler(io.Discard, nil)))
	becomes := func(what string, n int) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for held.Len() != n {
			if time.Now().After(deadline) {
				t.Fatalf("%s: the list holds %d, want %d", what, held.Len(), n)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	becomes("with no file", 0)
	one := `{"keys":[{"tenant":"demo","component":"notary","keyHash":"` + hashOf(notaryOfDemo) + `"}]}`
	two := `{"keys":[{"tenant":"demo","component":"notary","keyHash":"` + hashOf(notaryOfDemo) +
		`"},{"tenant":"demo","component":"scribe","keyHash":"` + hashOf(scribeOfDemo) + `"}]}`
	write(two)
	becomes("after the operator wrote two", 2)
	write(`{"keys": this is not a list}`)
	time.Sleep(50 * time.Millisecond)
	becomes("after a file that cannot be read", 2)
	write(one)
	becomes("after the operator dropped one", 1)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	becomes("after the file went", 0)
}
