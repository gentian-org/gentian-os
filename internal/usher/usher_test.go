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
	"github.com/gentian-org/gentian-os/internal/tilecatalogue"
)

type fakeAuthn map[string]string

func (f fakeAuthn) FromRequest(r *http.Request) (*authn.Identity, error) {
	sub, ok := f[strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")]
	if !ok {
		return nil, authn.ErrUnauthenticated
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
			URL: "https://notes.acme.k.example/", Icon: "data:x",
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

// Before the operator has projected anything there are no tiles, and that is
// an answer rather than an error.
func TestNoCatalogueYetIsAnEmptyDesktop(t *testing.T) {
	store := &fakeStore{held: map[string]bool{"user:ada can_enter tenant:acme": true}}
	s := New(Config{Authn: fakeAuthn{"ada": "ada"}, Authz: store, TilesPath: filepath.Join(t.TempDir(), "absent.yaml")})
	code, body := ask(t, s, "/v1/tenants/acme/tiles", "ada")
	if code != http.StatusOK || names(body) != "[]" {
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
