/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package porter

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// standIn is a realm and a file home in one: the realm exchanges the token
// "edge-ada" for "home-ada" under the scope the platform named, and the home
// answers WebDAV for that token as Nextcloud does, the elements under a
// prefix of its own.
type standIn struct {
	*httptest.Server
	exchanges atomic.Int32
}

func newStandIn(t *testing.T) *standIn {
	t.Helper()
	s := &standIn{}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /realms/acme/protocol/openid-connect/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.Form.Get("grant_type") != "urn:ietf:params:oauth:grant-type:token-exchange" ||
			r.Form.Get("client_id") != "vouch-porter" || r.Form.Get("client_secret") != "s3" ||
			r.Form.Get("scope") != "home-nextcloud-base-ce" || r.Form.Get("subject_token") != "edge-ada" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"invalid_grant","error_description":"not exchanged"}`))
			return
		}
		s.exchanges.Add(1)
		_, _ = w.Write([]byte(`{"access_token":"home-ada","expires_in":300}`))
	})
	mux.HandleFunc("/dav/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "PROPFIND" || r.Header.Get("Authorization") != "Bearer home-ada" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		body, _ := io.ReadAll(r.Body)
		multi := func(responses string) {
			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(http.StatusMultiStatus)
			_, _ = w.Write([]byte(`<?xml version="1.0"?><D:multistatus xmlns:D="DAV:" xmlns:oc="http://owncloud.org/ns">` + responses + `</D:multistatus>`))
		}
		ok := func(href, props string) string {
			return `<D:response><D:href>` + href + `</D:href><D:propstat><D:prop>` + props + `</D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response>`
		}
		switch r.URL.Path {
		case "/dav/":
			multi(ok("/dav/", `<D:current-user-principal><D:href>/dav/principals/users/ada%40example/</D:href></D:current-user-principal>`))
		case "/dav/files/ada@example/":
			multi(ok("/dav/files/ada%40example/", `<D:resourcetype><D:collection/></D:resourcetype>`) +
				ok("/dav/files/ada%40example/notes.txt", `<D:resourcetype/><D:getcontentlength>12</D:getcontentlength><D:getcontenttype>text/plain</D:getcontenttype><D:getlastmodified>Fri, 20 Nov 2026 10:00:00 GMT</D:getlastmodified>`) +
				ok("/dav/files/ada%40example/Offers%20%26%20Bids/", `<D:resourcetype><D:collection/></D:resourcetype>`))
		case "/dav/files/ada@example/notes.txt":
			if !bytes.Contains(body, []byte(`xmlns:x="http://owncloud.org/ns"`)) {
				t.Errorf("the id property was not asked for: %s", body)
			}
			multi(ok("/dav/files/ada%40example/notes.txt", `<D:resourcetype/><oc:fileid>4711</oc:fileid>`))
		case "/dav/files/ada@example/Offers & Bids":
			multi(ok("/dav/files/ada%40example/Offers%20%26%20Bids/", `<D:resourcetype><D:collection/></D:resourcetype><oc:fileid>9</oc:fileid>`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	s.Server = httptest.NewServer(mux)
	t.Cleanup(s.Close)
	return s
}

func (s *standIn) vouching(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for name, value := range map[string]string{
		toldClientID: "vouch-porter", toldClientSecret: "s3\n",
		toldTokenURL: s.URL + "/realms/acme/protocol/openid-connect/token",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(value), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func (s *standIn) home() *Home {
	return &Home{
		Profile: "nextcloud-base-ce", URL: s.URL + "/dav/", Root: "/files/{principal}/",
		IDProperty: "{http://owncloud.org/ns}fileid", Open: "https://nextcloud.acme.example/f/{id}",
		Scope: "home-nextcloud-base-ce",
	}
}

// A person's files are listed with a token exchanged from the one the edge
// handed over, below the root the home declares, folders first; a picked
// file is named by the home's id and the address that opens it by that id,
// and nothing of its path.
func TestAPersonsFilesAreListedAndOneIsPicked(t *testing.T) {
	s := newStandIn(t)
	homes := NewHomes(s.vouching(t))
	ctx := context.Background()

	entries, err := homes.List(ctx, s.home(), "edge-ada", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 || !entries[0].Folder || entries[0].Name != "Offers & Bids" ||
		entries[1].Name != "notes.txt" || entries[1].Size != 12 || entries[1].Modified != "2026-11-20T10:00:00Z" {
		t.Fatalf("listed %+v", entries)
	}
	picked, err := homes.Pick(ctx, s.home(), "edge-ada", "notes.txt")
	if err != nil {
		t.Fatal(err)
	}
	if picked.Name != "notes.txt" || picked.ID != "4711" || picked.URL != "https://nextcloud.acme.example/f/4711" {
		t.Fatalf("picked %+v", picked)
	}
	if s.exchanges.Load() != 1 {
		t.Fatalf("the edge's token was exchanged %d times for two calls", s.exchanges.Load())
	}

	var refusal *Refusal
	if _, err := homes.Pick(ctx, s.home(), "edge-ada", "Offers & Bids"); !asRefusal(err, &refusal) || refusal.Status != http.StatusBadRequest {
		t.Fatalf("a folder was picked: %v", err)
	}
	if _, err := homes.List(ctx, s.home(), "edge-ada", "../bob"); !asRefusal(err, &refusal) || refusal.Status != http.StatusNotFound {
		t.Fatalf("a path above the person's files: %v", err)
	}
	for _, odd := range []string{"a\\b", "a\nb", "a\x00"} {
		if _, err := homes.List(ctx, s.home(), "edge-ada", odd); !asRefusal(err, &refusal) || refusal.Status != http.StatusBadRequest {
			t.Fatalf("a path with %q: %v", odd, err)
		}
	}
	if _, err := homes.List(ctx, s.home(), "edge-mallory", ""); !asRefusal(err, &refusal) || refusal.Status != http.StatusForbidden ||
		strings.Contains(refusal.Message, "not exchanged") {
		t.Fatalf("a token the realm does not exchange: %v", err)
	}
	if _, err := homes.List(ctx, nil, "edge-ada", ""); !asRefusal(err, &refusal) || refusal.Status != http.StatusServiceUnavailable {
		t.Fatalf("a tenant with no file home: %v", err)
	}
}

func asRefusal(err error, to **Refusal) bool {
	for err != nil {
		if r, ok := err.(*Refusal); ok {
			*to = r
			return true
		}
		type one interface{ Unwrap() error }
		type many interface{ Unwrap() []error }
		switch u := err.(type) {
		case one:
			err = u.Unwrap()
		case many:
			for _, e := range u.Unwrap() {
				if asRefusal(e, to) {
					return true
				}
			}
			return false
		default:
			return false
		}
	}
	return false
}

// What the dialog's page asks is taken only from the porter's own pages; an
// app is told a file only where the plan lets its origin pick, and the page
// is served in no frame.
func TestOnlyAGrantedAppsPageIsToldAFile(t *testing.T) {
	s := newStandIn(t)
	plan := &Plan{Home: s.home(), Apps: []App{
		{Profile: "odoo-base-ce", Origins: []string{"https://odoo.acme.example"}, Uses: []Use{Pick}},
		{Profile: "wordcraft-ce", Origins: []string{"https://wordcraft.acme.example"}, Uses: []Use{Open, Save}},
	}}
	server := &Server{
		Plan:  func() (*Plan, error) { return plan, nil },
		Homes: NewHomes(s.vouching(t)),
		Log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	h := server.Handler()
	call := func(method, target, site, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, target, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer edge-ada")
		req.Header.Set("X-Gentian-Subject", "ada")
		if site != "" {
			req.Header.Set("Sec-Fetch-Site", site)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	if rec := call("GET", "/dialog", "none", ""); rec.Code != http.StatusOK ||
		!strings.Contains(rec.Header().Get("Content-Security-Policy"), "default-src 'none'") ||
		!strings.Contains(rec.Body.String(), "/dialog.js") {
		t.Fatalf("the dialog's page: %d %v", rec.Code, rec.Header())
	}
	if rec := call("GET", "/dialog.js", "same-origin", ""); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "gentianFiles") {
		t.Fatalf("the dialog's script: %d", rec.Code)
	}
	for _, site := range []string{"", "same-site", "cross-site", "none"} {
		if rec := call("GET", "/api/list?path=", site, ""); rec.Code != http.StatusForbidden {
			t.Fatalf("a request marked %q was taken: %d", site, rec.Code)
		}
	}
	if rec := call("GET", "/api/list?path=", "same-origin", ""); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "notes.txt") {
		t.Fatalf("the listing: %d %s", rec.Code, rec.Body)
	}
	if rec := call("GET", "/api/ask?use=pick&origin=https%3A%2F%2Fodoo.acme.example", "same-origin", ""); rec.Code != http.StatusOK {
		t.Fatalf("an app that may pick was refused: %d %s", rec.Code, rec.Body)
	}
	for _, origin := range []string{"https://wordcraft.acme.example", "https://evil.example", ""} {
		if rec := call("GET", "/api/ask?use=pick&origin="+origin, "same-origin", ""); rec.Code != http.StatusForbidden {
			t.Fatalf("%q may pick: %d", origin, rec.Code)
		}
		if rec := call("POST", "/api/pick", "same-origin", `{"origin":"`+origin+`","path":"notes.txt"}`); rec.Code != http.StatusForbidden {
			t.Fatalf("%q was told a file: %d %s", origin, rec.Code, rec.Body)
		}
	}
	rec := call("POST", "/api/pick", "same-origin", `{"origin":"https://odoo.acme.example","path":"notes.txt"}`)
	var picked Picked
	if err := json.Unmarshal(rec.Body.Bytes(), &picked); err != nil || rec.Code != http.StatusOK || picked.ID != "4711" {
		t.Fatalf("picked: %d %s", rec.Code, rec.Body)
	}
}

// The plan is read as the platform wrote it; one not written yet lets
// nobody ask.
func TestThePlanIsReadAsWritten(t *testing.T) {
	dir := t.TempDir()
	p, err := ReadPlan(filepath.Join(dir, "plan.json"))
	if err != nil || p.Home != nil || p.AppOf("https://odoo.acme.example", Pick) != nil {
		t.Fatalf("no plan: %+v (%v)", p, err)
	}
	write := func(s string) string {
		path := filepath.Join(dir, "plan.json")
		if err := os.WriteFile(path, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	good := `{"home":{"profile":"nextcloud-base-ce","url":"http://nextcloud.tenant-acme.svc.cluster.local:8080/remote.php/dav/","root":"/files/{principal}/","idProperty":"{http://owncloud.org/ns}fileid","open":"https://nextcloud.acme.example/f/{id}","scope":"home-nextcloud-base-ce"},"apps":[{"profile":"odoo-base-ce","origins":["https://odoo.acme.example"],"uses":["pick"]}]}`
	p, err = ReadPlan(write(good))
	if err != nil || p.AppOf("https://odoo.acme.example", Pick) == nil || p.AppOf("https://odoo.acme.example", Save) != nil {
		t.Fatalf("the plan: %+v (%v)", p, err)
	}
	for _, bad := range []string{
		strings.Replace(good, "{http://owncloud.org/ns}fileid", "fileid", 1),
		strings.Replace(good, "/f/{id}", "/f/", 1),
		strings.Replace(good, "https://nextcloud.acme.example/f/{id}", "http://nextcloud.acme.example/f/{id}", 1),
		strings.Replace(good, `"scope":"home-nextcloud-base-ce"`, `"scope":""`, 1),
	} {
		if _, err := ReadPlan(write(bad)); err == nil {
			t.Fatalf("a plan that should not be read was: %s", bad)
		}
	}
}
