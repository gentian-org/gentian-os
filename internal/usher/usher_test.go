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
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gentian-org/gentian-os/internal/director/authn"
	"github.com/gentian-org/gentian-os/internal/director/authz"
	"github.com/gentian-org/gentian-os/internal/tilecatalogue"
)

type fakeAuthn map[string]string

func (f fakeAuthn) FromRequest(r *http.Request) (*authn.Identity, error) {
	sub, ok := f[strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")]
	if !ok {
		return nil, authn.ErrUnauthenticated
	}
	// "vouch:<scope>:<sub>" is a token a component obtained for sub.
	if rest, found := strings.CutPrefix(sub, "vouch:"); found {
		scope, who, _ := strings.Cut(rest, ":")
		return &authn.Identity{Subject: who, Client: "vouch-ops-sync", Scopes: []string{scope}}, nil
	}
	// "<realm>/<sub>" is a token the realm issued.
	if realm, rest, found := strings.Cut(sub, "/"); found {
		return &authn.Identity{Realm: realm, Subject: rest}, nil
	}
	return &authn.Identity{Subject: sub}, nil
}

// fakeStore answers from a set of "user relation object" lines.
type fakeStore struct {
	held map[string]bool
	err  error
}

func (f *fakeStore) Check(_ context.Context, _, user, relation, object string) (bool, error) {
	if f.err != nil {
		return false, f.err
	}
	return f.held[user+" "+relation+" "+object], nil
}

func catalogueFile(t *testing.T) string {
	t.Helper()
	body, err := tilecatalogue.Marshal(tilecatalogue.Catalogue{Tiles: []tilecatalogue.Tile{
		{Name: "argocd", DisplayName: "ArgoCD", URL: "https://argocd.k.example/", Icon: "kube",
			Object: "cluster:demo", AnyOf: []string{"can_configure", "can_audit"}},
		{Name: "acme/admin-console/web", DisplayName: "Administration", URL: "https://admin.acme.k.example/", Icon: "data:x",
			Object: "tenant:acme", AnyOf: []string{"can_administer"}},
		{Name: "acme/notes/web", DisplayName: "Notes", DisplayNames: map[string]string{"de_DE": "Notizen"},
			URL: "https://notes.acme.k.example/", Icon: "data:x", Preopen: true,
			Object: "app:acme/notes", AnyOf: []string{"can_launch"}},
		{Name: "other/admin-console/web", DisplayName: "Administration", URL: "https://admin.other.k.example/", Icon: "data:x",
			Object: "tenant:other", AnyOf: []string{"can_administer"}},
		{Name: "other/notes/web", DisplayName: "Notes", URL: "https://notes.other.k.example/", Icon: "data:x",
			Object: "app:other/notes", AnyOf: []string{"can_launch"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "tiles.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func ask(t *testing.T, s *Server, path, token string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	body := map[string]any{}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	return rec.Code, body
}

func names(body map[string]any) string {
	var out []string
	for _, tile := range body["tiles"].([]any) {
		out = append(out, tile.(map[string]any)["name"].(string))
	}
	return fmt.Sprint(out)
}

// A person is shown the tiles of the tenant they asked about that they may
// open themselves: not the tenant's whole list, and never another tenant's,
// even where they hold the relation there too.
func TestTilesAreTheCallersOwnWithinOneTenant(t *testing.T) {
	store := &fakeStore{held: map[string]bool{
		// The tenant's administrator.
		"user:ada can_enter tenant:acme":      true,
		"user:ada can_administer tenant:acme": true,
		// A member, who may open one app.
		"user:mel can_enter tenant:acme":     true,
		"user:mel can_launch app:acme/notes": true,
		// A platform administrator: everything, everywhere.
		"user:pat can_enter tenant:acme":       true,
		"user:pat can_administer tenant:acme":  true,
		"user:pat can_administer tenant:other": true,
		"user:pat can_launch app:other/notes":  true,
		"user:pat can_audit cluster:demo":      true,
		// Somebody of another tenant.
		"user:olaf can_enter tenant:other": true,
	}}
	s := New(Config{
		Authn: fakeAuthn{"ada": "ada", "mel": "mel", "pat": "pat", "olaf": "olaf"},
		Authz: store, TilesPath: catalogueFile(t),
	})
	for _, c := range []struct {
		token string
		code  int
		tiles string
	}{
		{"ada", http.StatusOK, "[acme/admin-console/web]"},
		{"mel", http.StatusOK, "[acme/notes/web]"},
		{"pat", http.StatusOK, "[argocd acme/admin-console/web]"},
		{"olaf", http.StatusForbidden, ""},
		{"", http.StatusUnauthorized, ""},
		{"nobody", http.StatusUnauthorized, ""},
	} {
		code, body := ask(t, s, "/v1/tenants/acme/tiles", c.token)
		if code != c.code {
			t.Errorf("%q: status %d, want %d", c.token, code, c.code)
			continue
		}
		if code == http.StatusOK && names(body) != c.tiles {
			t.Errorf("%q: tiles %s, want %s", c.token, names(body), c.tiles)
		}
		if code != http.StatusOK && body["tiles"] != nil {
			t.Errorf("%q: a refusal carried tiles", c.token)
		}
	}
}

func TestATileCarriesItsTranslations(t *testing.T) {
	store := &fakeStore{held: map[string]bool{
		"user:mel can_enter tenant:acme": true, "user:mel can_launch app:acme/notes": true,
	}}
	s := New(Config{Authn: fakeAuthn{"mel": "mel"}, Authz: store, TilesPath: catalogueFile(t)})
	_, body := ask(t, s, "/v1/tenants/acme/tiles", "mel")
	tile := body["tiles"].([]any)[0].(map[string]any)
	if tile["displayNames"].(map[string]any)["de_DE"] != "Notizen" {
		t.Errorf("translations lost: %v", tile)
	}
}

// A tile the profile asks to be opened at mount says so to the portal, which
// knows nothing else about the app to decide it by.
func TestATileCarriesItsPreopen(t *testing.T) {
	store := &fakeStore{held: map[string]bool{
		"user:ada can_enter tenant:acme": true, "user:ada can_administer tenant:acme": true,
		"user:ada can_launch app:acme/notes": true,
	}}
	s := New(Config{Authn: fakeAuthn{"ada": "ada"}, Authz: store, TilesPath: catalogueFile(t)})
	_, body := ask(t, s, "/v1/tenants/acme/tiles", "ada")
	for _, raw := range body["tiles"].([]any) {
		tile := raw.(map[string]any)
		want := tile["name"] == "acme/notes/web"
		if got, _ := tile["preopen"].(bool); got != want {
			t.Errorf("%v: preopen %v, want %v", tile["name"], got, want)
		}
	}
}

// What is not a tenant's name never reaches the store as an object id.
func TestANameThatIsNotATenantIsRefused(t *testing.T) {
	s := New(Config{Authn: fakeAuthn{"ada": "ada"}, Authz: &fakeStore{}, TilesPath: catalogueFile(t)})
	for _, name := range []string{"Acme", "a%20b", "acme%23member", "-acme", "a.b"} {
		if code, _ := ask(t, s, "/v1/tenants/"+name+"/tiles", "ada"); code != http.StatusBadRequest {
			t.Errorf("%q: status %d, want 400", name, code)
		}
	}
}

// With the store unreachable nothing is shown: the answer is a failure, not
// an empty desktop that looks like a person with no rights, and not a full one.
func TestAnUnreachableStoreShowsNothing(t *testing.T) {
	s := New(Config{Authn: fakeAuthn{"ada": "ada"}, Authz: &fakeStore{err: errors.New("down")}, TilesPath: catalogueFile(t)})
	code, body := ask(t, s, "/v1/tenants/acme/tiles", "ada")
	if code != http.StatusServiceUnavailable || body["tiles"] != nil {
		t.Errorf("status %d body %v", code, body)
	}
}

// A catalog that is not there is a failure, not an empty desktop: nothing on
// the page would look like a person who holds nothing.
func TestAMissingCatalogueIsAFailure(t *testing.T) {
	store := &fakeStore{held: map[string]bool{"user:ada can_enter tenant:acme": true}}
	s := New(Config{Authn: fakeAuthn{"ada": "ada"}, Authz: store, TilesPath: filepath.Join(t.TempDir(), "absent.yaml")})
	code, body := ask(t, s, "/v1/tenants/acme/tiles", "ada")
	if code != http.StatusServiceUnavailable || body["tiles"] != nil {
		t.Errorf("status %d body %v", code, body)
	}
}

// Only reads are served.
func TestNothingButReadsIsRouted(t *testing.T) {
	s := New(Config{Authn: fakeAuthn{"ada": "ada"}, Authz: &fakeStore{}, TilesPath: catalogueFile(t)})
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
		req := httptest.NewRequest(method, "/v1/tenants/acme/tiles", nil)
		req.Header.Set("Authorization", "Bearer ada")
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, req)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s: status %d, want 405", method, rec.Code)
		}
	}
}

// catalogueWith writes a catalog file with the operator's verdict on the App
// Store beside one tile for the people who may install apps.
func catalogueWith(t *testing.T, offer *tilecatalogue.AppStore) string {
	t.Helper()
	body, err := tilecatalogue.Marshal(tilecatalogue.Catalogue{
		Tiles: []tilecatalogue.Tile{
			{Name: "acme/app-store/web", DisplayName: "App Store", URL: "https://store.acme.k.example/", Icon: "data:x",
				Object: "tenant:acme", AnyOf: []string{"can_install_app"}},
			{Name: "acme/notes/web", DisplayName: "Notes", URL: "https://notes.acme.k.example/", Icon: "data:x",
				Object: "app:acme/notes", AnyOf: []string{"can_launch"}},
		},
		AppStore: offer,
	})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "tiles.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// The App Store is offered only on a cluster that reports what it runs and
// whose claim names a store. The second half is the operator's verdict, read
// from beside the tiles -- the verdict that also places the App Store app --
// and this process's own setting stands in front of it. The tiles answer says
// which this is, and why not, so a page can put the reason into words instead
// of showing a store that is merely missing.
func TestTheAppStoreIsWithheldWhereTheClusterOffersNone(t *testing.T) {
	store := &fakeStore{held: map[string]bool{"user:ada can_enter tenant:acme": true}}
	offered := &tilecatalogue.AppStore{Offered: true}
	noStore := &tilecatalogue.AppStore{Reason: tilecatalogue.AppStoreReasonNoStore}
	noReport := &tilecatalogue.AppStore{Reason: tilecatalogue.AppStoreReasonNoUsageStatistics}
	for name, c := range map[string]struct {
		reporting bool
		offer     *tilecatalogue.AppStore
		want      map[string]any
	}{
		"reporting, a store named":           {true, offered, map[string]any{"available": true}},
		"reporting, no store named":          {true, noStore, map[string]any{"available": false, "reason": "no-store-configured"}},
		"reporting, the store is this host":  {true, &tilecatalogue.AppStore{Reason: tilecatalogue.AppStoreReasonOwnHost}, map[string]any{"available": false, "reason": "store-address-is-own-host"}},
		"reporting, the operator said none":  {true, nil, map[string]any{"available": false, "reason": "no-store-configured"}},
		"the operator says reporting is off": {true, noReport, map[string]any{"available": false, "reason": "usage-statistics-disabled"}},
		"not reporting, a store named":       {false, offered, map[string]any{"available": false, "reason": "usage-statistics-disabled"}},
		"not reporting, no store named":      {false, noStore, map[string]any{"available": false, "reason": "usage-statistics-disabled"}},
	} {
		s := New(Config{
			Authn: fakeAuthn{"ada": "ada"}, Authz: store, TilesPath: catalogueWith(t, c.offer),
			UsageStatisticsEnabled: c.reporting,
		})
		code, body := ask(t, s, "/v1/tenants/acme/tiles", "ada")
		if code != http.StatusOK {
			t.Fatalf("%s: status %d", name, code)
		}
		if got := fmt.Sprint(body["appStore"]); got != fmt.Sprint(c.want) {
			t.Errorf("%s: appStore = %s, want %v", name, got, c.want)
		}
	}
}

// The App Store tile is asked on the tenant with the install right: whoever
// may install apps there is shown it, and a member who may only enter and
// open an app is not.
func TestTheAppStoreTileIsForThePeopleWhoMayInstall(t *testing.T) {
	store := &fakeStore{held: map[string]bool{
		"user:ada can_enter tenant:acme":       true,
		"user:ada can_install_app tenant:acme": true,
		"user:mel can_enter tenant:acme":       true,
		"user:mel can_launch app:acme/notes":   true,
	}}
	s := New(Config{
		Authn: fakeAuthn{"ada": "ada", "mel": "mel"}, Authz: store,
		TilesPath: catalogueWith(t, &tilecatalogue.AppStore{Offered: true}), UsageStatisticsEnabled: true,
	})
	for token, want := range map[string]string{"ada": "[acme/app-store/web]", "mel": "[acme/notes/web]"} {
		code, body := ask(t, s, "/v1/tenants/acme/tiles", token)
		if code != http.StatusOK {
			t.Fatalf("%s: status %d", token, code)
		}
		if got := names(body); got != want {
			t.Errorf("%s sees %s, want %s", token, got, want)
		}
	}
}

// idStore records the request id each decision is asked under.
type idStore struct {
	fakeStore
	ids []string
}

func (s *idStore) Check(ctx context.Context, id, user, relation, object string) (bool, error) {
	s.ids = append(s.ids, id)
	return s.fakeStore.Check(ctx, id, user, relation, object)
}

// Every decision is asked, and so logged, under the request's id: the
// gateway's where it sent one, else one of the usher's own, answered in
// X-Request-Id either way.
func TestEveryDecisionCarriesTheRequestsID(t *testing.T) {
	store := &idStore{fakeStore: fakeStore{held: map[string]bool{
		"user:mel can_enter tenant:acme":     true,
		"user:mel can_launch app:acme/notes": true,
	}}}
	s := New(Config{Authn: fakeAuthn{"mel": "mel"}, Authz: store, TilesPath: catalogueFile(t)})
	for _, sent := range []string{"gateway-id-1234", "", "not an id!"} {
		store.ids = nil
		req := httptest.NewRequest(http.MethodGet, "/v1/tenants/acme/tiles", nil)
		req.Header.Set("Authorization", "Bearer mel")
		if sent != "" {
			req.Header.Set("X-Request-Id", sent)
		}
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, req)
		answered := rec.Header().Get("X-Request-Id")
		if rec.Code != http.StatusOK || answered == "" {
			t.Fatalf("sent %q: status %d, id %q", sent, rec.Code, answered)
		}
		if sent == "gateway-id-1234" && answered != sent {
			t.Errorf("the gateway's id %q was answered as %q", sent, answered)
		}
		if sent == "not an id!" && answered == sent {
			t.Errorf("an id that is not one was kept")
		}
		if len(store.ids) == 0 {
			t.Fatalf("sent %q: no decision was asked", sent)
		}
		for _, id := range store.ids {
			if id != answered {
				t.Errorf("sent %q: a decision was asked under %q, the request is %q", sent, id, answered)
			}
		}
	}
}

// batchStore answers the tile questions as one batch, and only so: a tile
// question asked one at a time, or a batch failing, shows in the answer.
type batchStore struct {
	fakeStore
	batches  int
	batchErr error
}

func (b *batchStore) BatchCheck(ctx context.Context, requestID string, qs []authz.Question) ([]bool, error) {
	b.batches++
	if b.batchErr != nil {
		return nil, b.batchErr
	}
	out := make([]bool, len(qs))
	for i, q := range qs {
		out[i], _ = b.Check(ctx, requestID, q.User, q.Relation, q.Object)
	}
	return out, nil
}

// Where the store can answer in batches, a tile list is one batch, and it
// says exactly what the questions asked one by one would have said.
func TestATileListIsOneBatchWithTheSameAnswer(t *testing.T) {
	held := map[string]bool{
		"user:ada can_enter tenant:acme":      true,
		"user:ada can_administer tenant:acme": true,
		"user:mel can_enter tenant:acme":      true,
		"user:mel can_launch app:acme/notes":  true,
		"user:pat can_enter tenant:acme":      true,
		"user:pat can_administer tenant:acme": true,
		"user:pat can_audit cluster:demo":     true,
	}
	authn := fakeAuthn{"ada": "ada", "mel": "mel", "pat": "pat"}
	one := New(Config{Authn: authn, Authz: &fakeStore{held: held}, TilesPath: catalogueFile(t)})
	store := &batchStore{fakeStore: fakeStore{held: held}}
	batched := New(Config{Authn: authn, Authz: store, TilesPath: catalogueFile(t)})
	for _, token := range []string{"ada", "mel", "pat"} {
		store.batches = 0
		wantCode, want := ask(t, one, "/v1/tenants/acme/tiles", token)
		code, got := ask(t, batched, "/v1/tenants/acme/tiles", token)
		if code != wantCode || names(got) != names(want) {
			t.Errorf("%s: batched %d %s, one by one %d %s", token, code, names(got), wantCode, names(want))
		}
		if store.batches != 1 {
			t.Errorf("%s: %d batches for one list, want 1", token, store.batches)
		}
	}
}

// A batch that fails refuses the whole list, as one failed question does.
func TestAFailedBatchShowsNothing(t *testing.T) {
	store := &batchStore{
		fakeStore: fakeStore{held: map[string]bool{"user:mel can_enter tenant:acme": true}},
		batchErr:  errors.New("openfga unreachable"),
	}
	s := New(Config{Authn: fakeAuthn{"mel": "mel"}, Authz: store, TilesPath: catalogueFile(t)})
	code, body := ask(t, s, "/v1/tenants/acme/tiles", "mel")
	if code != http.StatusServiceUnavailable || body["tiles"] != nil {
		t.Errorf("status %d, tiles %v; want 503 and none", code, body["tiles"])
	}
}

func TestACallerIsToldTheVerbsTheyHoldInOneBatch(t *testing.T) {
	held := map[string]bool{
		"user:pat can_enter tenant:acme":      true,
		"user:pat can_administer tenant:acme": true,
		"user:pat can_audit cluster:demo":     true,
		"user:mel can_enter tenant:acme":      true,
	}
	store := &batchStore{fakeStore: fakeStore{held: held}}
	s := New(Config{Authn: fakeAuthn{"pat": "pat", "mel": "mel"}, Authz: store, TilesPath: catalogueFile(t), Cluster: "demo"})

	code, body := ask(t, s, "/v1/tenants/acme/me", "pat")
	if code != http.StatusOK {
		t.Fatalf("tenant me: %d %v", code, body)
	}
	rel, _ := body["relations"].(map[string]any)
	if len(rel) != len(authz.TenantRelations) {
		t.Errorf("%d relations answered, want %d: %v", len(rel), len(authz.TenantRelations), rel)
	}
	for _, r := range authz.TenantRelations {
		want := r == "can_enter" || r == "can_administer"
		if rel[r] != want {
			t.Errorf("%s = %v, want %v", r, rel[r], want)
		}
	}
	if body["tenant"] != "acme" || body["subject"] != "pat" || body["platformVersionLine"] == "" {
		t.Errorf("tenant me body: %v", body)
	}
	if store.batches != 1 {
		t.Errorf("%d batches, want 1", store.batches)
	}

	// The cluster asks nothing but a sign-in: a person who holds nothing
	// there is told so.
	for token, audit := range map[string]bool{"pat": true, "mel": false} {
		code, body := ask(t, s, "/v1/clusters/demo/me", token)
		rel, _ := body["relations"].(map[string]any)
		if code != http.StatusOK || rel["can_audit"] != audit || len(rel) != len(authz.ClusterRelations) {
			t.Errorf("%s: cluster me %d %v", token, code, body)
		}
	}
	if code, _ := ask(t, s, "/v1/clusters/other/me", "pat"); code != http.StatusNotFound {
		t.Errorf("another cluster: %d, want 404", code)
	}
}

func TestAMeIsForThoseWhoMayEnter(t *testing.T) {
	s := New(Config{Authn: fakeAuthn{"eve": "eve"}, Authz: &fakeStore{held: map[string]bool{}}, TilesPath: catalogueFile(t)})
	if code, _ := ask(t, s, "/v1/tenants/acme/me", "eve"); code != http.StatusForbidden {
		t.Errorf("a person who may not enter: %d, want 403", code)
	}
	if code, _ := ask(t, s, "/v1/clusters/demo/me", "eve"); code != http.StatusNotFound {
		t.Errorf("an usher serving no cluster: %d, want 404", code)
	}
}

func TestAMeWithoutAnAnswerIsRefused(t *testing.T) {
	held := map[string]bool{"user:pat can_enter tenant:acme": true}
	store := &batchStore{fakeStore: fakeStore{held: held}, batchErr: errors.New("store down")}
	s := New(Config{Authn: fakeAuthn{"pat": "pat"}, Authz: store, TilesPath: catalogueFile(t), Cluster: "demo"})
	for _, path := range []string{"/v1/tenants/acme/me", "/v1/clusters/demo/me"} {
		if code, body := ask(t, s, path, "pat"); code != http.StatusServiceUnavailable || body["relations"] != nil {
			t.Errorf("%s: %d %v, want 503 and no answer", path, code, body)
		}
	}
}
