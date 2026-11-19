/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

// Package homesync keeps the calendars and contacts an app keeps of its own
// equal to the tenant's home for them (gentian-os#1156, homes #1149).
//
// One instance runs per tenant. It has two sides: the tenant's home, over
// CalDAV and CardDAV, and every app whose profile declares homeSync, over that
// app's own API through a driver for its framework. It acts as the person on
// both, with a token of that person for each, obtained by the statements it
// signs as a vouching component. The rules are the same for every app: the
// home wins a conflict, deletions go both ways, and the state of every pair of
// records is kept, so that a change is told from a deletion.
package homesync

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
)

// Kind is a kind of data a home keeps.
type Kind string

const (
	Calendar Kind = "calendar"
	Contacts Kind = "contacts"
)

// Plan is what the service syncs: the tenant's home and the apps that declare
// homeSync. The platform writes it as JSON (the ConfigMap
// home-sync-<component>), and it is read again on every round.
type Plan struct {
	// Home is the tenant's home: its profile and, per kind, the address its
	// contract is answered at (the DAV root).
	Home Home `json:"home"`
	// Apps are the apps that keep a copy, in the order of their profiles.
	Apps []App `json:"apps"`
	// ContactsBook is whose address book an app's contacts go to: "person"
	// (each person's own) or "shared" (one for the tenant). Empty is
	// "person".
	ContactsBook string `json:"contactsBook,omitempty"`
}

// Home is the tenant's home.
type Home struct {
	Profile string `json:"profile"`
	// Calendar and Contacts are the addresses of calendar-store and
	// contacts-store; empty where the tenant has no home for that kind.
	Calendar string `json:"calendar,omitempty"`
	Contacts string `json:"contacts,omitempty"`
}

// App is one app that keeps a copy, as its profile declares it.
type App struct {
	Profile string `json:"profile"`
	// URL is where the app's API answers inside the tenant.
	URL string `json:"url"`
	// Kinds are what it keeps, by kind.
	Kinds map[Kind]Mapping `json:"kinds"`
}

// Mapping is how an app keeps one kind: the driver of its framework, the
// record type, which of its fields hold which part of an entry, which of its
// records are a person's, and what a record made from the home is given.
type Mapping struct {
	Driver string `json:"driver"`
	Model  string `json:"model"`
	// Fields maps a part of an entry (Field) to the app's field.
	Fields map[Field]string `json:"fields"`
	// Filter selects a person's records, in the driver's form; "$user" and
	// "$partner" stand for the person in the app.
	Filter json.RawMessage `json:"filter,omitempty"`
	// Defaults are written onto a record made from the home, in the
	// driver's form, with the same placeholders.
	Defaults map[string]json.RawMessage `json:"defaults,omitempty"`
}

// Field is one part of an entry, named the same for every app.
type Field string

// The parts of a calendar entry.
const (
	Summary     Field = "summary"
	Description Field = "description"
	Location    Field = "location"
	Start       Field = "start"     // a time, for an entry that is not all day
	End         Field = "end"       // a time, for an entry that is not all day
	StartDate   Field = "startDate" // a date, for an all-day entry
	EndDate     Field = "endDate"   // the last day, for an all-day entry
	AllDay      Field = "allDay"
	RRule       Field = "rrule"
)

// The parts of a contact.
const (
	Name         Field = "name"
	Email        Field = "email"
	Phone        Field = "phone"
	Mobile       Field = "mobile"
	Organization Field = "organization"
	Title        Field = "title"
	Note         Field = "note"
	Street       Field = "street"
	City         Field = "city"
	Zip          Field = "zip"
	Country      Field = "country"
	Website      Field = "website"
)

// Fields of each kind, in a fixed order.
var kindFields = map[Kind][]Field{
	Calendar: {Summary, Description, Location, Start, End, StartDate, EndDate, AllDay, RRule},
	Contacts: {Name, Email, Phone, Mobile, Organization, Title, Note, Street, City, Zip, Country, Website},
}

// ReadPlan reads the plan the platform wrote.
func ReadPlan(path string) (*Plan, error) {
	raw, err := os.ReadFile(path) //nolint:gosec // The path is the service's own setting.
	if err != nil {
		return nil, err
	}
	p := &Plan{}
	if err := json.Unmarshal(raw, p); err != nil {
		return nil, fmt.Errorf("the plan at %s is not JSON of a plan: %w", path, err)
	}
	return p, p.validate()
}

func (p *Plan) validate() error {
	switch p.ContactsBook {
	case "", "person", "shared":
	default:
		return fmt.Errorf("contactsBook is person or shared, not %q", p.ContactsBook)
	}
	sort.Slice(p.Apps, func(i, j int) bool { return p.Apps[i].Profile < p.Apps[j].Profile })
	for _, a := range p.Apps {
		if a.Profile == "" || a.URL == "" {
			return fmt.Errorf("an app of the plan names no profile or no address")
		}
		for kind, m := range a.Kinds {
			known := kindFields[kind]
			if known == nil {
				return fmt.Errorf("%s: %q is no kind a home keeps", a.Profile, kind)
			}
			if m.Driver == "" || m.Model == "" {
				return fmt.Errorf("%s: %s names no driver or no record type", a.Profile, kind)
			}
			for f := range m.Fields {
				if !hasField(known, f) {
					return fmt.Errorf("%s: %s has no part %q", a.Profile, kind, f)
				}
			}
		}
	}
	return nil
}

func hasField(fields []Field, f Field) bool {
	for _, x := range fields {
		if x == f {
			return true
		}
	}
	return false
}
