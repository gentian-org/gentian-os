/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package homesync

import (
	"strings"
	"testing"
)

const eventInBerlin = "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//test//EN\r\n" +
	"BEGIN:VTIMEZONE\r\nTZID:Europe/Berlin\r\nBEGIN:STANDARD\r\nDTSTART:19701025T030000\r\nTZOFFSETFROM:+0200\r\nTZOFFSETTO:+0100\r\nEND:STANDARD\r\nEND:VTIMEZONE\r\n" +
	"BEGIN:VEVENT\r\nUID:meeting-1\r\nDTSTAMP:20261119T090000Z\r\nSUMMARY:Planning\r\nLOCATION:Room 2\r\n" +
	"DTSTART;TZID=Europe/Berlin:20261120T100000\r\nDTEND;TZID=Europe/Berlin:20261120T110000\r\n" +
	"RRULE:FREQ=WEEKLY;COUNT=4\r\n" +
	"BEGIN:VALARM\r\nACTION:DISPLAY\r\nTRIGGER:-PT15M\r\nDESCRIPTION:Reminder\r\nEND:VALARM\r\n" +
	"END:VEVENT\r\nEND:VCALENDAR\r\n"

// A timed entry is read as UTC whatever zone the home wrote it in, with its
// rule; writing parts onto it keeps what the parts do not name, an alarm.
func TestAnEventIsReadInUTCAndKeepsWhatIsNotMapped(t *testing.T) {
	v, uid, err := Read(Calendar, []byte(eventInBerlin))
	if err != nil {
		t.Fatal(err)
	}
	if uid != "meeting-1" || v[Summary] != "Planning" || v[Location] != "Room 2" || v[AllDay] != "false" {
		t.Fatalf("read %v %q", v, uid)
	}
	if v[Start] != "2026-11-20T09:00:00Z" || v[End] != "2026-11-20T10:00:00Z" {
		t.Fatalf("times %s %s, want 09:00 and 10:00 UTC", v[Start], v[End])
	}
	if v[RRule] != "FREQ=WEEKLY;COUNT=4" {
		t.Fatalf("rule %q", v[RRule])
	}
	out, err := Write(Calendar, []byte(eventInBerlin), "", Values{Summary: "Planning, moved", Start: "2026-11-21T09:00:00Z", End: "2026-11-21T10:30:00Z", AllDay: "false"})
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	for _, want := range []string{`SUMMARY:Planning\, moved`, "DTSTART:20261121T090000Z", "DTEND:20261121T103000Z", "BEGIN:VALARM", "LOCATION:Room 2", "UID:meeting-1"} {
		if !strings.Contains(s, want) {
			t.Errorf("written entry lacks %q:\n%s", want, s)
		}
	}
}

// An all-day entry is its first and its last day; the home's DTEND is the day
// after the last, both ways.
func TestAnAllDayEntryIsItsFirstAndLastDay(t *testing.T) {
	out, err := Write(Calendar, nil, "trip", Values{Summary: "Trip", AllDay: "true", StartDate: "2026-12-01", EndDate: "2026-12-03"})
	if err != nil {
		t.Fatal(err)
	}
	if s := string(out); !strings.Contains(s, "DTSTART;VALUE=DATE:20261201") || !strings.Contains(s, "DTEND;VALUE=DATE:20261204") {
		t.Fatalf("written:\n%s", s)
	}
	v, uid, err := Read(Calendar, out)
	if err != nil {
		t.Fatal(err)
	}
	if uid != "trip" || v[AllDay] != "true" || v[StartDate] != "2026-12-01" || v[EndDate] != "2026-12-03" {
		t.Fatalf("read back %v", v)
	}
}

// A contact's parts both ways: a mobile number apart from the other, the
// address, and a part emptied in the app is emptied at the home.
func TestAContactBothWays(t *testing.T) {
	out, err := Write(Contacts, nil, "c-1", Values{Name: "Ada Lovelace", Email: "ada@example.org", Phone: "+41 44 000 00 00", Mobile: "+41 79 000 00 00", Organization: "Analytical", City: "London", Street: "Main 1"})
	if err != nil {
		t.Fatal(err)
	}
	v, uid, err := Read(Contacts, out)
	if err != nil {
		t.Fatal(err)
	}
	if uid != "c-1" || v[Name] != "Ada Lovelace" || v[Email] != "ada@example.org" || v[Phone] != "+41 44 000 00 00" ||
		v[Mobile] != "+41 79 000 00 00" || v[Organization] != "Analytical" || v[City] != "London" || v[Street] != "Main 1" {
		t.Fatalf("read back %v", v)
	}
	out, err = Write(Contacts, out, "", Values{Email: "", Mobile: "+41 79 111 11 11"})
	if err != nil {
		t.Fatal(err)
	}
	v, _, _ = Read(Contacts, out)
	if v[Email] != "" || v[Mobile] != "+41 79 111 11 11" || v[Phone] != "+41 44 000 00 00" || v[Name] != "Ada Lovelace" {
		t.Fatalf("after the change %v", v)
	}
}
