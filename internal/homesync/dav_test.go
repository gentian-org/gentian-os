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
	"net/http"
	"net/http/httptest"
	"testing"
)

// nextcloudCalendars answers as Nextcloud does for a person with a calendar
// of birthdays it keeps itself (whose properties alone may be written, and
// whose address sorts first) and their personal calendar, which the
// scheduling inbox names as the default for new events.
func nextcloudCalendars(withDefault bool) http.Handler {
	const head = `<?xml version="1.0"?><d:multistatus xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:caldav">`
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusMultiStatus)
		switch r.URL.Path {
		case "/dav/":
			_, _ = w.Write([]byte(head + `<d:response><d:href>/dav/</d:href><d:propstat><d:prop>` +
				`<d:current-user-principal><d:href>/dav/principals/users/ann/</d:href></d:current-user-principal>` +
				`</d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response></d:multistatus>`))
		case "/dav/principals/users/ann/":
			_, _ = w.Write([]byte(head + `<d:response><d:href>/dav/principals/users/ann/</d:href><d:propstat><d:prop>` +
				`<c:calendar-home-set><d:href>/dav/calendars/ann/</d:href></c:calendar-home-set>` +
				`<c:schedule-inbox-URL><d:href>/dav/calendars/ann/inbox/</d:href></c:schedule-inbox-URL>` +
				`</d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response></d:multistatus>`))
		case "/dav/calendars/ann/inbox/":
			def := ""
			if withDefault {
				def = `<c:schedule-default-calendar-URL><d:href>/dav/calendars/ann/personal/</d:href></c:schedule-default-calendar-URL>`
			}
			_, _ = w.Write([]byte(head + `<d:response><d:href>/dav/calendars/ann/inbox/</d:href><d:propstat><d:prop>` +
				def + `</d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response></d:multistatus>`))
		case "/dav/calendars/ann/":
			calendar := func(href, privileges string) string {
				return `<d:response><d:href>` + href + `</d:href><d:propstat><d:prop>` +
					`<d:resourcetype><d:collection/><c:calendar/></d:resourcetype>` +
					`<c:supported-calendar-component-set><c:comp name="VEVENT"/></c:supported-calendar-component-set>` +
					`<d:current-user-privilege-set>` + privileges + `</d:current-user-privilege-set>` +
					`</d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response>`
			}
			_, _ = w.Write([]byte(head +
				calendar("/dav/calendars/ann/contact_birthdays/",
					`<d:privilege><d:read/></d:privilege><d:privilege><d:write-properties/></d:privilege>`) +
				calendar("/dav/calendars/ann/a-team/",
					`<d:privilege><d:read/></d:privilege><d:privilege><d:write/></d:privilege>`) +
				calendar("/dav/calendars/ann/personal/",
					`<d:privilege><d:read/></d:privilege><d:privilege><d:all/></d:privilege>`) +
				`</d:multistatus>`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
}

// A calendar whose properties alone may be written is not written to, and
// the calendar the home names as the default for new events is chosen over
// the first by address; where it names none, the first writable one is.
func TestTheCalendarWrittenToIsTheHomesDefaultAndWritable(t *testing.T) {
	for _, c := range []struct {
		withDefault bool
		want        string
	}{
		{true, "/dav/calendars/ann/personal/"},
		{false, "/dav/calendars/ann/a-team/"},
	} {
		home := httptest.NewServer(nextcloudCalendars(c.withDefault))
		d := &DAV{Root: home.URL + "/dav/", Kind: Calendar, Token: "t"}
		got, err := d.Collection(context.Background())
		home.Close()
		if err != nil || got != c.want {
			t.Fatalf("with a default %v: %q (%v), want %q", c.withDefault, got, err, c.want)
		}
	}
}

func TestOnlyPrivilegesThatChangeEntriesGrantWrite(t *testing.T) {
	for inner, want := range map[string]bool{
		`<d:write/>`:            true,
		`<d:write-content/>`:    true,
		`<d:bind/>`:             true,
		`<d:all/>`:              true,
		`<d:write-properties/>`: false,
		`<d:write-acl/>`:        false,
		`<d:read/>`:             false,
	} {
		if got := grantsWrite([]byte(inner)); got != want {
			t.Errorf("%s grants write: %v", inner, got)
		}
	}
}

// A mapped field the installed Odoo does not have is left out, once asked,
// and the rest of the mapping kept; an Odoo that does not say which fields
// it has is used with the mapping as it is.
func TestAFieldTheAppDoesNotHaveIsLeftOut(t *testing.T) {
	asked := 0
	odoo := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/json/2/res.partner/fields_get" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		asked++
		_, _ = w.Write([]byte(`{"name":{"type":"char"},"phone":{"type":"char"}}`))
	}))
	defer odoo.Close()
	o := &Odoo{}
	app := &App{Profile: "odoo-base-ce", URL: odoo.URL}
	m := &Mapping{Model: "res.partner", Fields: map[Field]string{"name": "name", "phone": "phone", "mobile": "mobile"}}
	for i := 0; i < 2; i++ {
		got := o.mapped(context.Background(), app, m, "t")
		if len(got.Fields) != 2 || got.Fields["mobile"] != "" || got.Fields["phone"] != "phone" {
			t.Fatalf("fields %v", got.Fields)
		}
	}
	if asked != 1 || len(m.Fields) != 3 {
		t.Fatalf("asked %d times; the mapping itself changed: %v", asked, m.Fields)
	}

	silent := &App{Profile: "odoo-base-ce", URL: odoo.URL + "/elsewhere"}
	if got := o.mapped(context.Background(), silent, m, "t"); len(got.Fields) != 3 {
		t.Fatalf("an app that does not say: %v", got.Fields)
	}
}
