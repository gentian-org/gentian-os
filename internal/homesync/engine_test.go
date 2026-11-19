/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package homesync

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// fakeHome is a DAV home of one person with a calendar that may be written
// to and one that may not.
type fakeHome struct {
	mu      sync.Mutex
	objects map[string]string // href -> body
	etags   map[string]int
	n       int
}

func (h *fakeHome) put(href, body string) {
	h.n++
	h.objects[href] = body
	h.etags[href] = h.n
}

func (h *fakeHome) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer home-token" {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	ms := func(body string) {
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusMultiStatus)
		_, _ = io.WriteString(w, `<?xml version="1.0"?><d:multistatus xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:caldav">`+body+`</d:multistatus>`)
	}
	ok := func(href, props string) string {
		return `<d:response><d:href>` + href + `</d:href><d:propstat><d:prop>` + props + `</d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response>`
	}
	p := r.URL.Path
	etag := func(href string) string { return `"` + strconv.Itoa(h.etags[href]) + `"` }
	switch r.Method {
	case "PROPFIND":
		switch p {
		case "/dav/":
			ms(ok(p, `<d:current-user-principal><d:href>/dav/principals/p/</d:href></d:current-user-principal>`))
		case "/dav/principals/p/":
			ms(ok(p, `<c:calendar-home-set><d:href>/dav/cal/p/</d:href></c:calendar-home-set>`))
		case "/dav/cal/p/":
			ms(ok(p, `<d:resourcetype><d:collection/></d:resourcetype>`) +
				ok("/dav/cal/p/birthdays/", `<d:resourcetype><d:collection/><c:calendar/></d:resourcetype><c:supported-calendar-component-set><c:comp name="VEVENT"/></c:supported-calendar-component-set><d:current-user-privilege-set><d:privilege><d:read/></d:privilege></d:current-user-privilege-set>`) +
				ok("/dav/cal/p/personal/", `<d:resourcetype><d:collection/><c:calendar/></d:resourcetype><c:supported-calendar-component-set><c:comp name="VEVENT"/></c:supported-calendar-component-set><d:current-user-privilege-set><d:privilege><d:read/></d:privilege><d:privilege><d:write/></d:privilege></d:current-user-privilege-set>`))
		case "/dav/cal/p/personal/":
			body := ok(p, `<d:resourcetype><d:collection/></d:resourcetype>`)
			for href := range h.objects {
				body += ok(href, `<d:getetag>`+etag(href)+`</d:getetag>`)
			}
			ms(body)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	case http.MethodGet:
		body, found := h.objects[p]
		if !found {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("ETag", etag(p))
		_, _ = io.WriteString(w, body)
	case http.MethodPut:
		_, exists := h.objects[p]
		if (r.Header.Get("If-None-Match") == "*" && exists) || (r.Header.Get("If-Match") != "" && (!exists || r.Header.Get("If-Match") != etag(p))) {
			w.WriteHeader(http.StatusPreconditionFailed)
			return
		}
		b, _ := io.ReadAll(r.Body)
		h.put(p, string(b))
		w.Header().Set("ETag", etag(p))
		w.WriteHeader(http.StatusCreated)
	case http.MethodDelete:
		if _, exists := h.objects[p]; exists && r.Header.Get("If-Match") != "" && r.Header.Get("If-Match") != etag(p) {
			w.WriteHeader(http.StatusPreconditionFailed)
			return
		}
		delete(h.objects, p)
		delete(h.etags, p)
		w.WriteHeader(http.StatusNoContent)
	}
}

// fakeOdoo is Odoo's JSON-2 API with one user and its events.
type fakeOdoo struct {
	mu      sync.Mutex
	events  map[int64]map[string]any
	next    int64
	clock   int
	domains []string
}

func (o *fakeOdoo) touch(rec map[string]any) {
	o.clock++
	rec["write_date"] = fmt.Sprintf("2026-11-19 10:00:%02d", o.clock)
}

func (o *fakeOdoo) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if r.Header.Get("Authorization") != "bearer app-token" {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/json/2/"), "/")
	var args map[string]json.RawMessage
	_ = json.NewDecoder(r.Body).Decode(&args)
	reply := func(v any) { _ = json.NewEncoder(w).Encode(v) }
	model, method := parts[0], parts[1]
	if model == "res.users" {
		reply([]any{map[string]any{"id": 7, "partner_id": []any{70, "Person"}}})
		return
	}
	var ids []int64
	_ = json.Unmarshal(args["ids"], &ids)
	switch method {
	case "search_read":
		o.domains = append(o.domains, string(args["domain"]))
		out := []any{}
		for id, rec := range o.events {
			out = append(out, map[string]any{"id": id, "write_date": rec["write_date"]})
		}
		reply(out)
	case "read":
		out := []any{}
		for _, id := range ids {
			if rec, ok := o.events[id]; ok {
				row := map[string]any{"id": id}
				for k, v := range rec {
					row[k] = v
				}
				out = append(out, row)
			}
		}
		reply(out)
	case "create":
		var vals []map[string]any
		_ = json.Unmarshal(args["vals_list"], &vals)
		o.next++
		o.touch(vals[0])
		o.events[o.next] = vals[0]
		reply([]int64{o.next})
	case "write":
		var vals map[string]any
		_ = json.Unmarshal(args["vals"], &vals)
		rec, ok := o.events[ids[0]]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		for k, v := range vals {
			rec[k] = v
		}
		o.touch(rec)
		reply(true)
	case "unlink":
		delete(o.events, ids[0])
		reply(true)
	}
}

type fakeTokens struct{}

func (fakeTokens) For(_ context.Context, _, scope string) (string, error) {
	if strings.HasPrefix(scope, "home-") {
		return "home-token", nil
	}
	return "app-token", nil
}

func event(uid, summary, start string) string {
	return "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//test//EN\r\nBEGIN:VEVENT\r\nUID:" + uid + "\r\nDTSTAMP:20261119T090000Z\r\nSUMMARY:" + summary +
		"\r\nDTSTART:" + start + "\r\nDTEND:" + start[:9] + "235900Z\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
}

type world struct {
	home   *fakeHome
	odoo   *fakeOdoo
	engine *Engine
	store  *Memory
}

func newWorld(t *testing.T) *world {
	t.Helper()
	w := &world{home: &fakeHome{objects: map[string]string{}, etags: map[string]int{}}, odoo: &fakeOdoo{events: map[int64]map[string]any{}}, store: &Memory{}}
	homeSrv, odooSrv := httptest.NewServer(w.home), httptest.NewServer(w.odoo)
	t.Cleanup(homeSrv.Close)
	t.Cleanup(odooSrv.Close)
	plan := &Plan{
		Home: Home{Calendar: &HomeOf{Profile: "nextcloud-base-ce", URL: homeSrv.URL + "/dav/"}},
		Apps: []App{{Profile: "odoo-base-ce", URL: odooSrv.URL, Kinds: map[Kind]Mapping{Calendar: {
			Driver: "odoo", Model: "calendar.event",
			Fields:   map[Field]string{Summary: "name", Start: "start", End: "stop", AllDay: "allday", StartDate: "start_date", EndDate: "stop_date"},
			Filter:   json.RawMessage(`[["partner_ids","in",["$partner"]]]`),
			Defaults: map[string]json.RawMessage{"user_id": json.RawMessage(`"$user"`), "partner_ids": json.RawMessage(`[[4,"$partner"]]`)},
		}}}},
	}
	w.engine = &Engine{
		Plan:    func() (*Plan, error) { return plan, plan.validate() },
		People:  func(context.Context) ([]string, error) { return []string{"sub-1"}, nil },
		Tokens:  fakeTokens{},
		Store:   w.store,
		Drivers: Drivers(nil),
		Log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	return w
}

func (w *world) round(t *testing.T) {
	t.Helper()
	if err := w.engine.Round(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func (w *world) homeSummaries() []string {
	var out []string
	for _, body := range w.home.objects {
		v, _, _ := Read(Calendar, []byte(body))
		out = append(out, v[Summary])
	}
	sort.Strings(out)
	return out
}

func (w *world) appSummaries() []string {
	var out []string
	for _, rec := range w.odoo.events {
		out = append(out, fmt.Sprint(rec["name"]))
	}
	sort.Strings(out)
	return out
}

func equal(a, b []string) bool { return strings.Join(a, "|") == strings.Join(b, "|") }

// New on either side is made on the other; a change goes the way it was made;
// a change on both sides is the home's; a deletion goes both ways, except
// that an entry changed at the home is made again in the app that deleted it.
func TestTheRulesOfASync(t *testing.T) {
	w := newWorld(t)
	w.home.put("/dav/cal/p/personal/a.ics", event("a", "From the home", "20261120T090000Z"))
	w.odoo.events[100] = map[string]any{"name": "From the app", "start": "2026-11-21 09:00:00", "stop": "2026-11-21 10:00:00", "allday": false}
	w.odoo.touch(w.odoo.events[100])
	w.odoo.next = 100
	w.round(t)
	if want := []string{"From the app", "From the home"}; !equal(w.homeSummaries(), want) || !equal(w.appSummaries(), want) {
		t.Fatalf("after the first round: home %v, app %v", w.homeSummaries(), w.appSummaries())
	}
	if d := w.odoo.domains[0]; d != `[["partner_ids","in",[70]]]` {
		t.Fatalf("the person's records were asked for with %s", d)
	}
	var made map[string]any
	for _, rec := range w.odoo.events {
		if rec["name"] == "From the home" {
			made = rec
		}
	}
	if fmt.Sprint(made["user_id"]) != "7" || fmt.Sprint(made["start"]) != "2026-11-20 09:00:00" {
		t.Fatalf("the record made from the home: %v", made)
	}

	// A second round with nothing changed changes nothing.
	before := fmt.Sprint(w.home.etags)
	w.round(t)
	if fmt.Sprint(w.home.etags) != before {
		t.Fatal("a round with nothing changed wrote to the home")
	}

	// Changed in the app: the home takes it.
	w.odoo.events[100]["name"] = "Renamed in the app"
	w.odoo.touch(w.odoo.events[100])
	w.round(t)
	if want := []string{"From the home", "Renamed in the app"}; !equal(w.homeSummaries(), want) {
		t.Fatalf("home after a change in the app: %v", w.homeSummaries())
	}

	// Changed on both sides: the home's change wins.
	w.home.put("/dav/cal/p/personal/a.ics", event("a", "Changed at the home", "20261120T090000Z"))
	for _, rec := range w.odoo.events {
		if rec["name"] == "From the home" {
			rec["name"] = "Changed in the app too"
			w.odoo.touch(rec)
		}
	}
	w.round(t)
	if want := []string{"Changed at the home", "Renamed in the app"}; !equal(w.appSummaries(), want) || !equal(w.homeSummaries(), want) {
		t.Fatalf("after a change on both sides: home %v, app %v", w.homeSummaries(), w.appSummaries())
	}

	// Deleted at the home: deleted in the app.
	delete(w.home.objects, "/dav/cal/p/personal/a.ics")
	w.round(t)
	if want := []string{"Renamed in the app"}; !equal(w.appSummaries(), want) {
		t.Fatalf("app after a deletion at the home: %v", w.appSummaries())
	}

	// Deleted in the app: deleted at the home.
	delete(w.odoo.events, 100)
	w.round(t)
	if len(w.home.objects) != 0 || len(w.odoo.events) != 0 {
		t.Fatalf("after a deletion in the app: home %v, app %v", w.homeSummaries(), w.appSummaries())
	}
}

// An entry changed at the home while its record was deleted in the app is the
// home's, and is made again in the app.
func TestAChangeAtTheHomeOutlivesADeletionInTheApp(t *testing.T) {
	w := newWorld(t)
	w.home.put("/dav/cal/p/personal/b.ics", event("b", "Kept", "20261122T090000Z"))
	w.round(t)
	for id := range w.odoo.events {
		delete(w.odoo.events, id)
	}
	w.home.put("/dav/cal/p/personal/b.ics", event("b", "Kept and changed", "20261122T090000Z"))
	w.round(t)
	if want := []string{"Kept and changed"}; !equal(w.appSummaries(), want) || !equal(w.homeSummaries(), want) {
		t.Fatalf("home %v, app %v", w.homeSummaries(), w.appSummaries())
	}
}

// A person the realm gives no token is passed over, and nobody else is.
func TestAPersonWithoutATokenIsPassedOver(t *testing.T) {
	w := newWorld(t)
	w.engine.People = func(context.Context) ([]string, error) { return []string{"unlinked", "sub-1"}, nil }
	w.engine.Tokens = tokensExcept{"unlinked"}
	w.home.put("/dav/cal/p/personal/c.ics", event("c", "For sub-1", "20261123T090000Z"))
	w.round(t)
	if want := []string{"For sub-1"}; !equal(w.appSummaries(), want) {
		t.Fatalf("app %v", w.appSummaries())
	}
}

type tokensExcept struct{ person string }

func (t tokensExcept) For(ctx context.Context, person, scope string) (string, error) {
	if person == t.person {
		return "", ErrRefused
	}
	return fakeTokens{}.For(ctx, person, scope)
}
