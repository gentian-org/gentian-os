/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package chaperone

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

type fakeRights struct {
	use  map[[2]string]bool
	down bool
}

func (f *fakeRights) MayUse(_ context.Context, person, app string) (bool, error) {
	if f.down {
		return false, errors.New("connection refused")
	}
	return f.use[[2]string{person, app}], nil
}

type fakeTools struct{ tools []Tool }

func newFakeTools() *fakeTools {
	return &fakeTools{tools: []Tool{
		{Adapter: "pa", Name: "list-tasks", Version: 1, App: "projects", Class: ClassRead, Description: "List the tasks of a project"},
		{Adapter: "pa", Name: "create-task", Version: 1, App: "projects", Class: ClassChange},
		// A tool with no class counts as irreversible: the adapters' list says so.
		{Adapter: "pa", Name: "unclassed", Version: 1, App: "projects", Class: ClassIrreversible},
		{Adapter: "pa", Name: "send-mail", Version: 1, App: "projects", Class: ClassIrreversible},
	}}
}

func (f *fakeTools) Lookup(_ context.Context, key string) (Tool, bool) {
	for _, t := range f.tools {
		if t.Key() == key {
			return t, true
		}
	}
	return Tool{}, false
}

func (f *fakeTools) All(context.Context) []Tool { return f.tools }

// fakePersons takes tokens of the form "<person>:<client>".
type fakePersons struct{}

func (fakePersons) Verify(_ context.Context, token string, _ int64) (Present, error) {
	person, client, ok := strings.Cut(token, ":")
	if !ok || person == "" || client == "" || strings.Contains(token, ".") {
		return Present{}, errors.New("not a token of this realm")
	}
	return Present{Person: person, Client: client, Token: token}, nil
}

type fakeCredentials struct{ fail bool }

func (f *fakeCredentials) ForApp(_ context.Context, token, app string) (string, error) {
	if f.fail {
		return "", adapterError("the realm answered invalid_scope")
	}
	person, _, _ := strings.Cut(token, ":")
	return app + "-token-of-" + person, nil
}

type madeCall struct {
	key        string
	arguments  map[string]any
	credential string
}

type fakeAdapters struct {
	calls []madeCall
	fail  bool
}

func (f *fakeAdapters) Call(_ context.Context, tool Tool, arguments map[string]any, credential string) (any, error) {
	if f.fail {
		return nil, adapterError("the app answered 500")
	}
	f.calls = append(f.calls, madeCall{tool.Key(), arguments, credential})
	return map[string]any{"tasks": []string{"a", "b"}}, nil
}

type world struct {
	t           *testing.T
	rights      *fakeRights
	adapters    *fakeAdapters
	credentials *fakeCredentials
	journal     *MemoryJournal
	server      *Server
	handler     http.Handler
	key         *ecdsa.PrivateKey
}

func newWorld(t *testing.T) *world {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	issuer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{
			{Key: &key.PublicKey, KeyID: "k1", Algorithm: "ES256", Use: "sig"},
		}})
	}))
	t.Cleanup(issuer.Close)
	w := &world{
		t:           t,
		rights:      &fakeRights{use: map[[2]string]bool{{"anna", "projects"}: true}},
		adapters:    &fakeAdapters{},
		credentials: &fakeCredentials{},
		journal:     &MemoryJournal{},
		key:         key,
	}
	w.server = &Server{
		Service: &Service{
			Rights: w.rights, Tools: newFakeTools(), Adapters: w.adapters,
			Credentials: w.credentials, Journal: w.journal, Tenant: "acme",
		},
		Passes:  NewIssuerPasses(issuer.URL, "issuer", "chaperone", "acme", nil),
		Persons: fakePersons{},
	}
	w.handler = w.server.Handler()
	return w
}

// pass is a pass good here, with claims changed by over (nil removes one).
func (w *world) pass(signer *ecdsa.PrivateKey, kid string, over map[string]any) string {
	w.t.Helper()
	if signer == nil {
		signer = w.key
	}
	now := time.Now().Unix()
	claims := map[string]any{
		"iss": "issuer", "aud": "chaperone", "iat": now, "exp": now + 60, "sub": "anna",
		"tenant": "acme", "engine": "flow", "run": "run_1", "mandate": "man_1", "fingerprint": "f1",
		"stage": nil, "tools": []string{"pa/list-tasks@1"}, "run_ends": now + 3600,
	}
	for k, v := range over {
		claims[k] = v
	}
	sig, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.ES256, Key: signer},
		(&jose.SignerOptions{}).WithHeader("kid", kid).WithType("JWT"))
	if err != nil {
		w.t.Fatal(err)
	}
	token, err := jwt.Signed(sig).Claims(claims).Serialize()
	if err != nil {
		w.t.Fatal(err)
	}
	return token
}

func (w *world) good(over map[string]any) string { return w.pass(nil, "k1", over) }

func (w *world) do(method, path, token string, body any) (int, map[string]any, []any) {
	w.t.Helper()
	var reader *bytes.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		reader = bytes.NewReader(raw)
	} else {
		reader = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, reader)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	w.handler.ServeHTTP(rec, req)
	var object map[string]any
	var list []any
	if json.Unmarshal(rec.Body.Bytes(), &object) != nil {
		_ = json.Unmarshal(rec.Body.Bytes(), &list)
	}
	return rec.Code, object, list
}

const (
	callPath    = "/api/v1/tools/call"
	presentPath = "/api/v1/present/tools/call"
	anna        = "anna:claude-code"
)

var listTasks = map[string]any{"tool": "pa/list-tasks@1", "arguments": map[string]any{"project": "Accounting"}}

func call(tool string) map[string]any {
	return map[string]any{"tool": tool, "arguments": map[string]any{}}
}

func (w *world) refused(path, token string, body any) string {
	w.t.Helper()
	status, object, _ := w.do(http.MethodPost, path, token, body)
	if status != http.StatusForbidden {
		w.t.Fatalf("status %d, want 403: %v", status, object)
	}
	if len(w.adapters.calls) != 0 {
		w.t.Fatalf("a refused call reached an adapter: %v", w.adapters.calls)
	}
	reason, _ := object["detail"].(map[string]any)["reason"].(string)
	return reason
}

func contains(t *testing.T, got, want string) {
	t.Helper()
	if !strings.Contains(got, want) {
		t.Fatalf("%q does not say %q", got, want)
	}
}

func names(list []any) []string {
	out := []string{}
	for _, entry := range list {
		out = append(out, entry.(map[string]any)["tool"].(string))
	}
	return out
}

func TestADeclaredReadingToolIsCarriedAndJournalled(t *testing.T) {
	w := newWorld(t)
	status, object, _ := w.do(http.MethodPost, callPath, w.good(nil), listTasks)
	if status != http.StatusOK {
		t.Fatalf("status %d: %v", status, object)
	}
	if got, _ := json.Marshal(object); string(got) != `{"result":{"tasks":["a","b"]}}` {
		t.Fatalf("answer %s", got)
	}
	if len(w.adapters.calls) != 1 || w.adapters.calls[0].key != "pa/list-tasks@1" ||
		w.adapters.calls[0].arguments["project"] != "Accounting" {
		t.Fatalf("calls %v", w.adapters.calls)
	}
	lines := w.journal.Lines()
	if len(lines) != 1 {
		t.Fatalf("%d journal lines", len(lines))
	}
	l := lines[0]
	if l.Decision != "allowed" || l.Outcome != "ok" || l.Person != "anna" || l.Engine != "flow" ||
		l.Run != "run_1" || l.Mandate != "man_1" || l.Fingerprint != "f1" || l.Class != ClassRead || l.Tenant != "acme" {
		t.Fatalf("line %+v", l)
	}
	if strings.Contains(l.ArgumentsDigest, "Accounting") || len(l.ArgumentsDigest) != 64 {
		t.Fatalf("digest %q", l.ArgumentsDigest)
	}
}

func TestAnUndeclaredToolIsRefused(t *testing.T) {
	w := newWorld(t)
	contains(t, w.refused(callPath, w.good(nil), call("pa/create-task@1")), "did not declare")
	if w.journal.Lines()[0].Decision != "refused" {
		t.Fatal("the refusal was not journalled as one")
	}
}

func TestNothingThatChangesDataPassesWhateverThePassSays(t *testing.T) {
	w := newWorld(t)
	declared := map[string]any{"tools": []string{"pa/create-task@1", "pa/unclassed@1"}}
	contains(t, w.refused(callPath, w.good(declared), call("pa/create-task@1")), "reading tools")
	contains(t, w.refused(callPath, w.good(declared), call("pa/unclassed@1")), "reading tools")
	gone := map[string]any{"tools": []string{"pa/gone@1"}}
	contains(t, w.refused(callPath, w.good(gone), call("pa/gone@1")), "no installed adapter")
}

func TestThePersonsOwnRightsAreAskedOnEveryCall(t *testing.T) {
	w := newWorld(t)
	w.rights.use = nil
	contains(t, w.refused(callPath, w.good(nil), listTasks), "may not use projects")
}

func TestAPassThatIsNotGenuineOrNotForHereIsNoPass(t *testing.T) {
	w := newWorld(t)
	other, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tokens := map[string]string{
		"none":           "",
		"another signer": w.pass(other, "k1", nil),
		"unknown key":    w.pass(nil, "other", nil),
		"audience":       w.good(map[string]any{"aud": "projects"}),
		"issuer":         w.good(map[string]any{"iss": "somebody"}),
		"expired":        w.good(map[string]any{"exp": 1}),
		"tenant":         w.good(map[string]any{"tenant": "other"}),
		"run ended":      w.good(map[string]any{"run_ends": 1}),
		"no engine":      w.good(map[string]any{"engine": ""}),
		"no tools":       w.good(map[string]any{"tools": []string{}}),
		"a person":       anna,
	}
	for name, token := range tokens {
		if status, _, _ := w.do(http.MethodPost, callPath, token, listTasks); status != http.StatusUnauthorized {
			t.Errorf("%s: status %d, want 401", name, status)
		}
	}
	if len(w.journal.Lines()) != 0 || len(w.adapters.calls) != 0 {
		t.Fatal("a call without a pass left a trace")
	}
}

func TestAFailingAdapterIsAnErrorNotARefusal(t *testing.T) {
	w := newWorld(t)
	w.adapters.fail = true
	if status, _, _ := w.do(http.MethodPost, callPath, w.good(nil), listTasks); status != http.StatusBadGateway {
		t.Fatalf("status %d, want 502", status)
	}
	if l := w.journal.Lines()[0]; l.Decision != "allowed" || l.Outcome != "error" {
		t.Fatalf("line %+v", l)
	}
}

func TestTheToolListShowsOnlyWhatTheRunMayCall(t *testing.T) {
	w := newWorld(t)
	token := w.good(map[string]any{"tools": []string{"pa/list-tasks@1", "pa/create-task@1", "pa/gone@1"}})
	_, _, list := w.do(http.MethodGet, "/api/v1/tools", token, nil)
	if got := names(list); len(got) != 1 || got[0] != "pa/list-tasks@1" {
		t.Fatalf("listed %v", got)
	}
}

func TestNothingBoundMeansNothingPasses(t *testing.T) {
	w := newWorld(t)
	token := w.good(nil)
	w.server.Passes = nil
	if status, _, _ := w.do(http.MethodPost, callPath, token, listTasks); status != http.StatusServiceUnavailable {
		t.Fatalf("no issuer: status %d", status)
	}
	w.server.Service = nil
	for _, path := range []string{callPath, presentPath} {
		if status, _, _ := w.do(http.MethodPost, path, anna, listTasks); status != http.StatusServiceUnavailable {
			t.Fatalf("%s unbound: status %d", path, status)
		}
	}
}

func TestAPlatformThatCannotBeAskedIsNeverAYes(t *testing.T) {
	w := newWorld(t)
	w.rights.down = true
	for path, token := range map[string]string{callPath: w.good(nil), presentPath: anna} {
		if status, _, _ := w.do(http.MethodPost, path, token, listTasks); status != http.StatusServiceUnavailable {
			t.Fatalf("%s: status %d, want 503", path, status)
		}
	}
	if status, _, _ := w.do(http.MethodGet, "/api/v1/present/tools", anna, nil); status != http.StatusServiceUnavailable {
		t.Fatalf("list: status %d, want 503", status)
	}
	if len(w.adapters.calls) != 0 {
		t.Fatal("an adapter was called")
	}
	for _, l := range w.journal.Lines() {
		if l.Decision != "refused" {
			t.Fatalf("line %+v", l)
		}
	}
}

func TestAPresentPersonsReadingToolIsCarriedWithACredentialForThatAppAlone(t *testing.T) {
	w := newWorld(t)
	status, object, _ := w.do(http.MethodPost, presentPath, anna, listTasks)
	if status != http.StatusOK {
		t.Fatalf("status %d: %v", status, object)
	}
	// The adapter is handed the exchanged token, never the person's own.
	if len(w.adapters.calls) != 1 || w.adapters.calls[0].credential != "projects-token-of-anna" {
		t.Fatalf("calls %v", w.adapters.calls)
	}
	// The program acted, for the person; no run, no mandate, no graph.
	l := w.journal.Lines()[0]
	if l.Person != "anna" || l.Engine != "claude-code" || l.Decision != "allowed" || l.Outcome != "ok" ||
		l.Class != ClassRead || l.Run != "" || l.Mandate != "" || l.Fingerprint != "" {
		t.Fatalf("line %+v", l)
	}
}

func TestAPresentPersonsChangingToolPassesAndIsJournalled(t *testing.T) {
	w := newWorld(t)
	if status, _, _ := w.do(http.MethodPost, presentPath, anna, call("pa/create-task@1")); status != http.StatusOK {
		t.Fatalf("status %d", status)
	}
	if l := w.journal.Lines()[0]; l.Class != ClassChange || l.Decision != "allowed" {
		t.Fatalf("line %+v", l)
	}
}

func TestWhatCannotBeUndoneIsRefused(t *testing.T) {
	w := newWorld(t)
	contains(t, w.refused(presentPath, anna, call("pa/send-mail@1")), "cannot be undone")
	contains(t, w.refused(presentPath, anna, call("pa/unclassed@1")), "cannot be undone")
	contains(t, w.refused(presentPath, anna, call("pa/gone@1")), "no installed adapter")
	for _, l := range w.journal.Lines() {
		if l.Decision != "refused" {
			t.Fatalf("line %+v", l)
		}
	}
}

func TestAPresentPersonsOwnRightsAreAskedOnEveryCall(t *testing.T) {
	w := newWorld(t)
	contains(t, w.refused(presentPath, "ben:claude-code", call("pa/list-tasks@1")), "may not use projects")
	w.rights.use = nil
	contains(t, w.refused(presentPath, anna, call("pa/list-tasks@1")), "may not use projects")
}

func TestTheListIsWhatThisPersonMayHaveCalled(t *testing.T) {
	w := newWorld(t)
	_, _, list := w.do(http.MethodGet, "/api/v1/present/tools", anna, nil)
	if got := names(list); len(got) != 2 || got[0] != "pa/list-tasks@1" || got[1] != "pa/create-task@1" {
		t.Fatalf("listed %v", got)
	}
	if _, ok := list[0].(map[string]any)["inputSchema"]; !ok {
		t.Fatal("the list does not say what a tool takes")
	}
	status, _, list := w.do(http.MethodGet, "/api/v1/present/tools", "ben:claude-code", nil)
	if status != http.StatusOK || len(list) != 0 {
		t.Fatalf("ben: status %d, listed %v", status, list)
	}
}

func TestNoTokenOrOneTheRealmDidNotIssueIsNobody(t *testing.T) {
	w := newWorld(t)
	// A pass is not a person's token either.
	for _, token := range []string{"", "junk", w.good(nil)} {
		if status, _, _ := w.do(http.MethodPost, presentPath, token, listTasks); status != http.StatusUnauthorized {
			t.Fatalf("token %q: status %d, want 401", token, status)
		}
	}
	if len(w.journal.Lines()) != 0 || len(w.adapters.calls) != 0 {
		t.Fatal("a call by nobody left a trace")
	}
}

func TestWithoutTheRealmNothingPasses(t *testing.T) {
	w := newWorld(t)
	w.server.Service.Credentials = nil
	if status, _, _ := w.do(http.MethodPost, presentPath, anna, listTasks); status != http.StatusServiceUnavailable {
		t.Fatalf("no credentials: status %d", status)
	}
	w.server.Service.Credentials = w.credentials
	w.server.Persons = nil
	if status, _, _ := w.do(http.MethodPost, presentPath, anna, listTasks); status != http.StatusServiceUnavailable {
		t.Fatalf("no persons: status %d", status)
	}
	if len(w.adapters.calls) != 0 {
		t.Fatal("an adapter was called")
	}
}

func TestARealmThatGivesNoCredentialIsAnErrorAndNothingIsCalled(t *testing.T) {
	w := newWorld(t)
	w.credentials.fail = true
	if status, _, _ := w.do(http.MethodPost, presentPath, anna, listTasks); status != http.StatusBadGateway {
		t.Fatalf("status %d, want 502", status)
	}
	if len(w.adapters.calls) != 0 {
		t.Fatal("an adapter was called")
	}
	if l := w.journal.Lines()[0]; l.Decision != "allowed" || l.Outcome != "error" {
		t.Fatalf("line %+v", l)
	}
}

type brokenJournal struct{}

func (brokenJournal) Add(context.Context, Line) error { return errors.New("disk full") }

func TestACallThatCouldNotBeJournalledIsNotAnsweredAsASuccess(t *testing.T) {
	w := newWorld(t)
	w.server.Service.Journal = brokenJournal{}
	if status, _, _ := w.do(http.MethodPost, presentPath, anna, listTasks); status != http.StatusBadGateway {
		t.Fatalf("status %d, want 502", status)
	}
}

func TestACallWithoutAToolIsNotACall(t *testing.T) {
	w := newWorld(t)
	if status, _, _ := w.do(http.MethodPost, presentPath, anna, map[string]any{"arguments": map[string]any{}}); status != http.StatusUnprocessableEntity {
		t.Fatalf("status %d, want 422", status)
	}
}

func TestTheDigestIsOfTheArgumentsInOneWriting(t *testing.T) {
	// The SHA-256 of {"a":"<b>","z":1}: keys sorted, no spaces, nothing escaped.
	const want = "2ab12692fbaadc701c798f656620fa819bd57f8819d76d5e671263fe4ed13aa5"
	if got := Digest(map[string]any{"z": json.Number("1"), "a": "<b>"}); got != want {
		t.Fatalf("digest %q, want %q", got, want)
	}
	if Digest(nil) != Digest(map[string]any{}) {
		t.Fatal("no arguments and empty arguments differ")
	}
}
