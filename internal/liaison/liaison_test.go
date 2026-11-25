/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package liaison

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// outside is a service outside the cluster: its authorization server, as far
// as a sign-in asks it.
type outside struct {
	*httptest.Server
	registered []map[string]any
	exchanged  []url.Values
	revoked    []string
	// refuse makes the token endpoint say no.
	refuse bool
	issued int
}

func newOutside(t *testing.T) *outside {
	t.Helper()
	o := &outside{}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/oauth-authorization-server", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{
			"issuer": o.URL, "authorization_endpoint": o.URL + "/authorize", "token_endpoint": o.URL + "/token",
			"registration_endpoint": o.URL + "/register", "revocation_endpoint": o.URL + "/revoke",
		})
	})
	mux.HandleFunc("POST /register", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		o.registered = append(o.registered, body)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]string{"client_id": "client-1"})
	})
	mux.HandleFunc("POST /token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		o.exchanged = append(o.exchanged, r.PostForm)
		if o.refuse {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		o.issued++
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "access-" + string(rune('0'+o.issued)), "refresh_token": "lasting-" + string(rune('0'+o.issued)), "expires_in": 600,
		})
	})
	mux.HandleFunc("POST /revoke", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		o.revoked = append(o.revoked, r.PostForm.Get("token"))
	})
	o.Server = httptest.NewTLSServer(mux)
	t.Cleanup(o.Close)
	return o
}

// confidant is the platform's, as far as the liaison asks it.
type confidant struct {
	held map[string]string
	// with is the person's token each entry was last kept with.
	with map[string]string
}

func (c *confidant) Put(_ context.Context, person, name, value, personToken string) error {
	key := person + "/" + name
	if _, open := c.held[key]; !open && personToken == "" {
		return errors.New("the confidant answered 403")
	}
	c.held[key], c.with[key] = value, personToken
	return nil
}

func (c *confidant) Get(_ context.Context, person, name string) (string, bool, error) {
	value, ok := c.held[person+"/"+name]
	return value, ok, nil
}

func (c *confidant) Delete(_ context.Context, person, name string) error {
	delete(c.held, person+"/"+name)
	return nil
}

type world struct {
	service   *Service
	outside   *outside
	confidant *confidant
	now       time.Time
}

func newWorld(t *testing.T) *world {
	t.Helper()
	w := &world{outside: newOutside(t), confidant: &confidant{held: map[string]string{}, with: map[string]string{}},
		now: time.Unix(1_800_000_000, 0)}
	client := w.outside.Client()
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	w.service = NewService(NewOAuth(client), w.confidant, "Workspace")
	w.service.Now = func() time.Time { return w.now }
	w.service.Tell([]Connection{
		{Name: "recorder", URL: "https://tools.example/mcp", Issuer: w.outside.URL, Scopes: []string{"read"}},
		{Name: "plain", URL: "http://tools.example/mcp", Issuer: w.outside.URL},
	})
	return w
}

const back = "https://liaison.acme.example/connections/callback"

func (w *world) connect(t *testing.T, person string) {
	t.Helper()
	address, err := w.service.Begin(context.Background(), person, "recorder", back, false)
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(address)
	if _, _, err := w.service.Complete(context.Background(), person, person+"-token", u.Query().Get("state"), "code-1"); err != nil {
		t.Fatal(err)
	}
}

func TestAPersonConnectsAndTheLastingCredentialStaysWithTheConfidant(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	if got := w.service.Connections(); len(got) != 1 || got[0].Name != "recorder" {
		t.Fatalf("known connections: %+v, and one is not https", got)
	}
	address, err := w.service.Begin(ctx, "ada", "recorder", back, false)
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(address)
	q := u.Query()
	if u.Path != "/authorize" || q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") == "" ||
		q.Get("redirect_uri") != back || q.Get("resource") != "https://tools.example/mcp" || q.Get("scope") != "read" ||
		q.Get("client_id") != "client-1" {
		t.Fatalf("the person is sent to %s", address)
	}
	if reg := w.outside.registered[0]; reg["token_endpoint_auth_method"] != "none" || reg["client_name"] != "Workspace" {
		t.Fatalf("registered as %+v", reg)
	}
	// Somebody else cannot finish it, and it cannot be finished twice.
	if _, _, err := w.service.Complete(ctx, "grace", "grace-token", q.Get("state"), "code-1"); err == nil {
		t.Fatal("another person finished the sign-in")
	}
	if _, _, err := w.service.Complete(ctx, "ada", "ada-token", q.Get("state"), "code-1"); err == nil {
		t.Fatal("a sign-in somebody else tried to finish is still open")
	}
	w.connect(t, "ada")
	sent := w.outside.exchanged[0]
	if sent.Get("grant_type") != "authorization_code" || sent.Get("code_verifier") == "" ||
		Challenge(sent.Get("code_verifier")) == sent.Get("code_verifier") {
		t.Fatalf("exchanged with %+v", sent)
	}
	if w.confidant.with["ada/recorder"] != "ada-token" {
		t.Fatal("the first entry was not kept with the person's own token")
	}
	credential, expires, err := w.service.Credential(ctx, "ada", "recorder")
	if err != nil || credential != "access-1" || expires != w.now.Unix()+600 {
		t.Fatalf("credential %q until %d: %v", credential, expires, err)
	}
	if strings.Contains(credential, "lasting") {
		t.Fatal("the lasting credential was handed out")
	}
	if _, _, err := w.service.Credential(ctx, "grace", "recorder"); !errors.Is(err, ErrNotConnected) {
		t.Fatalf("a person who never connected: %v", err)
	}
}

func TestACredentialIsRenewedWhileThePersonIsAway(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	w.connect(t, "ada")
	w.now = w.now.Add(9 * time.Minute)
	credential, _, err := w.service.Credential(ctx, "ada", "recorder")
	if err != nil || credential != "access-2" {
		t.Fatalf("renewed credential %q: %v", credential, err)
	}
	renewal := w.outside.exchanged[1]
	if renewal.Get("grant_type") != "refresh_token" || renewal.Get("refresh_token") != "lasting-1" {
		t.Fatalf("renewed with %+v", renewal)
	}
	// The rotated lasting credential is kept, with the liaison's key alone.
	if !strings.Contains(w.confidant.held["ada/recorder"], "lasting-2") || w.confidant.with["ada/recorder"] != "" {
		t.Fatalf("kept %q with %q", w.confidant.held["ada/recorder"], w.confidant.with["ada/recorder"])
	}
	// A service that no longer honors it: the person connects again.
	w.now = w.now.Add(time.Hour)
	w.outside.refuse = true
	if _, _, err := w.service.Credential(ctx, "ada", "recorder"); !errors.Is(err, ErrNotConnected) {
		t.Fatalf("a refused renewal: %v", err)
	}
}

func TestDisconnectingTellsTheServiceAndRemovesWhatWasKept(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	w.connect(t, "ada")
	if err := w.service.Disconnect(ctx, "ada", "recorder"); err != nil {
		t.Fatal(err)
	}
	if len(w.outside.revoked) != 1 || w.outside.revoked[0] != "lasting-1" {
		t.Fatalf("revoked %v", w.outside.revoked)
	}
	if len(w.confidant.held) != 0 {
		t.Fatalf("still kept: %v", w.confidant.held)
	}
}

func TestAnEntryIsNotPresentedAtAnotherAddress(t *testing.T) {
	w := newWorld(t)
	w.connect(t, "ada")
	w.service.Tell([]Connection{{Name: "recorder", URL: "https://elsewhere.example/mcp", Issuer: w.outside.URL}})
	if _, _, err := w.service.Credential(context.Background(), "ada", "recorder"); !errors.Is(err, ErrNotConnected) {
		t.Fatalf("a connection whose address changed: %v", err)
	}
}

type people map[string]string

func (p people) Verify(_ context.Context, token string) (string, error) {
	if person, ok := p[token]; ok {
		return person, nil
	}
	return "", errors.New("unknown")
}

func TestTheRoutesTakeAPersonOrTheChaperoneAndNeverTheOtherOnes(t *testing.T) {
	w := newWorld(t)
	server := NewServer(w.service, people{"ada-token": "ada"}, "chaperone-key", nil)
	do := func(method, path, bearer, body string, headers ...string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Host = "liaison.acme.example"
		if bearer != "" {
			r.Header.Set("Authorization", "Bearer "+bearer)
		}
		for i := 0; i+1 < len(headers); i += 2 {
			r.Header.Set(headers[i], headers[i+1])
		}
		rec := httptest.NewRecorder()
		server.ServeHTTP(rec, r)
		return rec
	}
	if rec := do("GET", "/api/v1/connections/", "", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("nobody signed in: %d", rec.Code)
	}
	if rec := do("GET", "/api/v1/connections/", "ada-token", ""); rec.Code != http.StatusOK ||
		!strings.Contains(rec.Body.String(), `"name":"recorder"`) || !strings.Contains(rec.Body.String(), `"connected":false`) {
		t.Fatalf("the list: %d %s", rec.Code, rec.Body)
	}
	// A page of another site cannot start or end a connection.
	if rec := do("POST", "/api/v1/connections/recorder/connect", "ada-token", ""); rec.Code != http.StatusForbidden {
		t.Fatalf("a change without the page's header: %d", rec.Code)
	}
	rec := do("POST", "/api/v1/connections/recorder/connect", "ada-token", "", requestHeader, "1")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "liaison.acme.example%2Fconnections%2Fcallback") {
		t.Fatalf("connect: %d %s", rec.Code, rec.Body)
	}
	// The chaperone's routes take its key, and a person's token is not one.
	body := `{"person":"ada","connection":"recorder"}`
	for _, bearer := range []string{"", "ada-token", "wrong"} {
		if rec := do("POST", "/api/v1/internal/credential", bearer, body); rec.Code != http.StatusUnauthorized {
			t.Fatalf("credential with %q: %d", bearer, rec.Code)
		}
	}
	if rec := do("POST", "/api/v1/internal/credential", "chaperone-key", body); rec.Code != http.StatusConflict {
		t.Fatalf("a person who is not connected: %d", rec.Code)
	}
	w.connect(t, "ada")
	if rec := do("POST", "/api/v1/internal/credential", "chaperone-key", body); rec.Code != http.StatusOK ||
		!strings.Contains(rec.Body.String(), `"credential":"access-`) || strings.Contains(rec.Body.String(), "lasting") {
		t.Fatalf("credential: %d %s", rec.Code, rec.Body)
	}
	told := `{"connections":[{"name":"other","url":"https://other.example/mcp","issuer":"https://other.example"}]}`
	if rec := do("PUT", "/api/v1/internal/connections", "chaperone-key", told); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"connections":1`) {
		t.Fatalf("told: %d %s", rec.Code, rec.Body)
	}
}

// The desktop manages a person's connections with the person's own token:
// it lists, starts and ends them, and never opens the page or completes a
// sign-in with it. A sign-in it starts comes back to the liaison's own host,
// whatever host the call reached, and sends the person on to the desktop's
// Connections tab; one started at the page goes back to the page.
func TestTheDesktopManagesConnectionsAndThePersonGoesBackToIt(t *testing.T) {
	w := newWorld(t)
	server := NewServer(w.service, people{"ada-token": "ada"}, "chaperone-key", nil)
	server.Edge = people{"ada-edge": "ada"}
	server.Host, server.DesktopHost = "liaison.acme.example", "desktop.acme.example"
	do := func(method, target, bearer string, headers ...string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, target, nil)
		r.Host = "liaison:8000"
		if bearer != "" {
			r.Header.Set("Authorization", "Bearer "+bearer)
		}
		for i := 0; i+1 < len(headers); i += 2 {
			r.Header.Set(headers[i], headers[i+1])
		}
		rec := httptest.NewRecorder()
		server.ServeHTTP(rec, r)
		return rec
	}
	if rec := do("GET", "/api/v1/connections/", "ada-edge"); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"name":"recorder"`) {
		t.Fatalf("the desktop's list: %d %s", rec.Code, rec.Body)
	}
	if rec := do("GET", "/connections/", "ada-edge"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("the page with the desktop's token: %d", rec.Code)
	}
	if rec := do("POST", "/api/v1/connections/recorder/connect", "ada-edge"); rec.Code != http.StatusForbidden {
		t.Fatalf("a change without the header: %d", rec.Code)
	}
	start := func(bearer string) url.Values {
		t.Helper()
		rec := do("POST", "/api/v1/connections/recorder/connect", bearer, requestHeader, "1")
		var body struct{ URL string }
		if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &body) != nil {
			t.Fatalf("connect: %d %s", rec.Code, rec.Body)
		}
		u, _ := url.Parse(body.URL)
		if got := u.Query().Get("redirect_uri"); got != "https://liaison.acme.example/connections/callback" {
			t.Fatalf("the sign-in comes back to %q", got)
		}
		return u.Query()
	}
	backTo := func(target string) string {
		t.Helper()
		rec := do("GET", target, "ada-token")
		if rec.Code != http.StatusSeeOther {
			t.Fatalf("callback: %d %s", rec.Code, rec.Body)
		}
		return rec.Header().Get("Location")
	}

	q := start("ada-edge")
	if rec := do("GET", "/connections/callback?state="+q.Get("state")+"&code=code-1", "ada-edge"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("a sign-in completed with the desktop's token: %d", rec.Code)
	}
	if got := backTo("/connections/callback?state=" + q.Get("state") + "&code=code-1"); got != "https://desktop.acme.example/?account=connections&connected=recorder" {
		t.Fatalf("a sign-in started in the desktop goes back to %q", got)
	}
	q = start("ada-edge")
	if got := backTo("/connections/callback?state=" + q.Get("state") + "&error=access_denied"); got != "https://desktop.acme.example/?account=connections&failed=declined" {
		t.Fatalf("a declined sign-in started in the desktop goes back to %q", got)
	}
	q = start("ada-token")
	if got := backTo("/connections/callback?state=" + q.Get("state") + "&code=code-1"); got != "/connections/?connected=recorder" {
		t.Fatalf("a sign-in started at the page goes back to %q", got)
	}
	if rec := do("POST", "/api/v1/connections/recorder/disconnect", "ada-edge", requestHeader, "1"); rec.Code != http.StatusNoContent {
		t.Fatalf("the desktop's disconnect: %d %s", rec.Code, rec.Body)
	}

	// Not told where the desktop is, the liaison starts no sign-in for it.
	server.DesktopHost = ""
	if rec := do("POST", "/api/v1/connections/recorder/connect", "ada-edge", requestHeader, "1"); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("connect for a desktop the liaison does not know: %d", rec.Code)
	}
	// Not told to accept the desktop, the liaison refuses its token.
	server.Edge = nil
	if rec := do("GET", "/api/v1/connections/", "ada-edge"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("the desktop's token without Edge: %d", rec.Code)
	}
}

// A person who is not connected is told where to connect: the desktop's
// Connections tab, at its address where the liaison knows it.
func TestNotConnectedNamesTheDesktopsConnectionsTab(t *testing.T) {
	server := NewServer(newWorld(t).service, people{}, "chaperone-key", nil)
	if got := server.notConnected(); !strings.Contains(got, "under Account, Connections") || strings.Contains(got, "https://") {
		t.Fatalf("without the desktop's host: %q", got)
	}
	server.DesktopHost = "desktop.acme.example"
	if got := server.notConnected(); !strings.Contains(got, "https://desktop.acme.example/?account=connections") {
		t.Fatalf("with it: %q", got)
	}
}
