/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package homesync

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"sort"
	"strings"
)

// The home's side: CalDAV and CardDAV, spoken as the person, with the few
// requests a sync needs. The service never assumes the home's paths: it asks
// the root who the person is (current-user-principal), the principal where
// their collections are (the home sets), and the home which collection may be
// written to. Every write names the version it replaces (If-Match) or says it
// makes something new (If-None-Match), so a change made at the home meanwhile
// is never written over.

// ErrChanged is the home's answer to a write whose version is not the one
// named: the object changed meanwhile. The next round reads it again.
var ErrChanged = errors.New("the object changed at the home meanwhile")

// DAV is the home's side for one person and one kind.
type DAV struct {
	Root   string // the contract's address, the DAV root
	Kind   Kind
	Token  string
	Client *http.Client
}

const (
	nsDAV     = "DAV:"
	nsCalDAV  = "urn:ietf:params:xml:ns:caldav"
	nsCardDAV = "urn:ietf:params:xml:ns:carddav"
)

func (d *DAV) client() *http.Client {
	if d.Client != nil {
		return d.Client
	}
	return http.DefaultClient
}

// resolve is a path the home named, as an address.
func (d *DAV) resolve(href string) (string, error) {
	base, err := url.Parse(d.Root)
	if err != nil {
		return "", err
	}
	ref, err := url.Parse(href)
	if err != nil {
		return "", err
	}
	return base.ResolveReference(ref).String(), nil
}

func (d *DAV) do(ctx context.Context, method, href string, header http.Header, body []byte) (*http.Response, error) {
	target, err := d.resolve(href)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, method, target, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	for k, v := range header {
		req.Header[k] = v
	}
	req.Header.Set("Authorization", "Bearer "+d.Token)
	return d.client().Do(req)
}

// multistatus is the answer of a PROPFIND, as far as it is read here.
type multistatus struct {
	Responses []struct {
		Href     string `xml:"href"`
		Propstat []struct {
			Status string `xml:"status"`
			Prop   struct {
				Principal struct {
					Href string `xml:"href"`
				} `xml:"current-user-principal"`
				CalendarHome struct {
					Href string `xml:"href"`
				} `xml:"urn:ietf:params:xml:ns:caldav calendar-home-set"`
				AddressbookHome struct {
					Href string `xml:"href"`
				} `xml:"urn:ietf:params:xml:ns:carddav addressbook-home-set"`
				ScheduleInbox struct {
					Href string `xml:"href"`
				} `xml:"urn:ietf:params:xml:ns:caldav schedule-inbox-URL"`
				ScheduleDefault struct {
					Href string `xml:"href"`
				} `xml:"urn:ietf:params:xml:ns:caldav schedule-default-calendar-URL"`
				ResourceType struct {
					Calendar    *struct{} `xml:"urn:ietf:params:xml:ns:caldav calendar"`
					Addressbook *struct{} `xml:"urn:ietf:params:xml:ns:carddav addressbook"`
				} `xml:"resourcetype"`
				Components struct {
					Comp []struct {
						Name string `xml:"name,attr"`
					} `xml:"comp"`
				} `xml:"urn:ietf:params:xml:ns:caldav supported-calendar-component-set"`
				Privileges struct {
					Privilege []struct {
						Inner []byte `xml:",innerxml"`
					} `xml:"privilege"`
				} `xml:"current-user-privilege-set"`
				ETag string `xml:"getetag"`
			} `xml:"prop"`
		} `xml:"propstat"`
	} `xml:"response"`
}

func (d *DAV) propfind(ctx context.Context, href, depth, props string) (*multistatus, error) {
	body := `<?xml version="1.0" encoding="utf-8"?><d:propfind xmlns:d="DAV:" xmlns:c="` + nsCalDAV + `" xmlns:a="` + nsCardDAV + `"><d:prop>` + props + `</d:prop></d:propfind>`
	resp, err := d.do(ctx, "PROPFIND", href, http.Header{"Depth": {depth}, "Content-Type": {"application/xml; charset=utf-8"}}, []byte(body))
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusMultiStatus {
		return nil, fmt.Errorf("PROPFIND %s answered %d", href, resp.StatusCode)
	}
	ms := &multistatus{}
	if err := xml.NewDecoder(io.LimitReader(resp.Body, 32<<20)).Decode(ms); err != nil {
		return nil, fmt.Errorf("PROPFIND %s: %w", href, err)
	}
	return ms, nil
}

// Collection finds the person's collection of this kind that may be written
// to: of the collections in their home set that hold entries of the kind and
// grant the person write, the calendar the home names as the default for new
// events (RFC 6638 schedule-default-calendar-URL), or else the first by
// address. A collection whose properties alone may be written (a calendar of
// birthdays the home keeps itself) is not one.
func (d *DAV) Collection(ctx context.Context) (string, error) {
	ms, err := d.propfind(ctx, d.Root, "0", `<d:current-user-principal/>`)
	if err != nil {
		return "", err
	}
	principal := ""
	for _, r := range ms.Responses {
		for _, ps := range r.Propstat {
			if h := ps.Prop.Principal.Href; h != "" {
				principal = h
			}
		}
	}
	if principal == "" {
		return "", errors.New("the home names no principal for the person")
	}
	homeProp := `<c:calendar-home-set/><c:schedule-inbox-URL/>`
	if d.Kind == Contacts {
		homeProp = `<a:addressbook-home-set/>`
	}
	ms, err = d.propfind(ctx, principal, "0", homeProp)
	if err != nil {
		return "", err
	}
	homeSet, inbox := "", ""
	for _, r := range ms.Responses {
		for _, ps := range r.Propstat {
			if ps.Prop.ScheduleInbox.Href != "" {
				inbox = ps.Prop.ScheduleInbox.Href
			}
			if d.Kind == Contacts && ps.Prop.AddressbookHome.Href != "" {
				homeSet = ps.Prop.AddressbookHome.Href
			}
			if d.Kind == Calendar && ps.Prop.CalendarHome.Href != "" {
				homeSet = ps.Prop.CalendarHome.Href
			}
		}
	}
	if homeSet == "" {
		return "", fmt.Errorf("the home names no %s home set for the person", d.Kind)
	}
	ms, err = d.propfind(ctx, homeSet, "1", `<d:resourcetype/><c:supported-calendar-component-set/><d:current-user-privilege-set/>`)
	if err != nil {
		return "", err
	}
	var found []string
	for _, r := range ms.Responses {
		ok, writable := false, false
		for _, ps := range r.Propstat {
			if !strings.Contains(ps.Status, " 200") {
				continue
			}
			p := ps.Prop
			if d.Kind == Contacts && p.ResourceType.Addressbook != nil {
				ok = true
			}
			if d.Kind == Calendar && p.ResourceType.Calendar != nil {
				ok = len(p.Components.Comp) == 0
				for _, c := range p.Components.Comp {
					if c.Name == "VEVENT" {
						ok = true
					}
				}
			}
			for _, priv := range p.Privileges.Privilege {
				if grantsWrite(priv.Inner) {
					writable = true
				}
			}
		}
		if ok && writable {
			found = append(found, r.Href)
		}
	}
	if len(found) == 0 {
		return "", fmt.Errorf("the person has no %s at the home that may be written to", d.Kind)
	}
	sort.Strings(found)
	if d.Kind == Calendar && inbox != "" {
		if def := d.defaultCalendar(ctx, inbox); def != "" {
			for _, f := range found {
				if strings.TrimSuffix(f, "/") == strings.TrimSuffix(def, "/") {
					return f, nil
				}
			}
		}
	}
	return found[0], nil
}

// defaultCalendar is the calendar the person's scheduling inbox names as the
// default for new events; nothing where it names none.
func (d *DAV) defaultCalendar(ctx context.Context, inbox string) string {
	ms, err := d.propfind(ctx, inbox, "0", `<c:schedule-default-calendar-URL/>`)
	if err != nil {
		return ""
	}
	for _, r := range ms.Responses {
		for _, ps := range r.Propstat {
			if h := ps.Prop.ScheduleDefault.Href; h != "" {
				return h
			}
		}
	}
	return ""
}

// grantsWrite reports whether a privilege of current-user-privilege-set lets
// entries be made and changed: write, write-content, bind or all, and not
// write-properties or write-acl, which leave the entries as they are.
func grantsWrite(inner []byte) bool {
	dec := xml.NewDecoder(bytes.NewReader(inner))
	for {
		tok, err := dec.Token()
		if err != nil {
			return false
		}
		if el, ok := tok.(xml.StartElement); ok {
			switch el.Name.Local {
			case "write", "write-content", "bind", "all":
				return true
			}
		}
	}
}

// List is the collection's entries: each one's address and version.
func (d *DAV) List(ctx context.Context, collection string) (map[string]string, error) {
	ms, err := d.propfind(ctx, collection, "1", `<d:getetag/><d:resourcetype/>`)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	self := strings.TrimSuffix(collection, "/")
	for _, r := range ms.Responses {
		if strings.TrimSuffix(r.Href, "/") == self {
			continue
		}
		for _, ps := range r.Propstat {
			if strings.Contains(ps.Status, " 200") && ps.Prop.ETag != "" {
				out[r.Href] = ps.Prop.ETag
			}
		}
	}
	return out, nil
}

// Get is one entry and its version.
func (d *DAV) Get(ctx context.Context, href string) ([]byte, string, error) {
	resp, err := d.do(ctx, http.MethodGet, href, nil, nil)
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("GET %s answered %d", href, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	return body, resp.Header.Get("ETag"), err
}

// Put writes an entry: over the version named, or as a new one where none
// is. It returns the new version, or "" where the home names none (the next
// listing has it).
func (d *DAV) Put(ctx context.Context, href, etag string, body []byte) (string, error) {
	h := http.Header{"Content-Type": {d.contentType()}}
	if etag == "" {
		h.Set("If-None-Match", "*")
	} else {
		h.Set("If-Match", etag)
	}
	resp, err := d.do(ctx, http.MethodPut, href, h, body)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	switch resp.StatusCode {
	case http.StatusOK, http.StatusCreated, http.StatusNoContent:
		return resp.Header.Get("ETag"), nil
	case http.StatusPreconditionFailed:
		return "", ErrChanged
	}
	return "", fmt.Errorf("PUT %s answered %d", href, resp.StatusCode)
}

// Delete removes an entry of the version named. One already gone is gone.
func (d *DAV) Delete(ctx context.Context, href, etag string) error {
	h := http.Header{}
	if etag != "" {
		h.Set("If-Match", etag)
	}
	resp, err := d.do(ctx, http.MethodDelete, href, h, nil)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	switch resp.StatusCode {
	case http.StatusOK, http.StatusNoContent, http.StatusNotFound, http.StatusGone:
		return nil
	case http.StatusPreconditionFailed:
		return ErrChanged
	}
	return fmt.Errorf("DELETE %s answered %d", href, resp.StatusCode)
}

func (d *DAV) contentType() string {
	if d.Kind == Contacts {
		return "text/vcard; charset=utf-8"
	}
	return "text/calendar; charset=utf-8"
}

// NewHref is the address of a new entry in the collection.
func (d *DAV) NewHref(collection, uid string) string {
	ext := ".ics"
	if d.Kind == Contacts {
		ext = ".vcf"
	}
	return path.Join(collection, url.PathEscape(uid)+ext)
}
