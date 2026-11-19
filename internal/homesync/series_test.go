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
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// seriesOdoo keeps calendar events as Odoo does: a rule makes a series, a base
// event and its occurrences, tied by a recurrence.
type seriesOdoo struct {
	mu          sync.Mutex
	events      map[int64]map[string]any
	recurrences map[int64]map[string]any // id -> base, rule
	next, clock int64
}

func (o *seriesOdoo) touch(rec map[string]any) {
	o.clock++
	rec["write_date"] = fmt.Sprintf("2026-11-19 11:00:%02d", o.clock)
}

// series makes a series of base and two more occurrences.
func (o *seriesOdoo) series(base int64, rule string) {
	o.next++
	rid := o.next + 1000
	o.recurrences[rid] = map[string]any{"base": base, "rule": rule}
	b := o.events[base]
	b["recurrence_id"] = []any{rid, "series"}
	b["recurrency"] = true
	o.touch(b)
	for i := 0; i < 2; i++ {
		o.next++
		occ := map[string]any{"recurrence_id": []any{rid, "series"}, "recurrency": true}
		for k, v := range b {
			if k != "recurrence_id" {
				occ[k] = v
			}
		}
		o.touch(occ)
		o.events[o.next] = occ
	}
}

func (o *seriesOdoo) rruleOf(rec map[string]any) any {
	if r, ok := rec["recurrence_id"].([]any); ok {
		rid := int64(r[0].(int64))
		return "DTSTART:20261120T090000\nRRULE:" + o.recurrences[rid]["rule"].(string)
	}
	return false
}

func (o *seriesOdoo) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	o.mu.Lock()
	defer o.mu.Unlock()
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/json/2/"), "/")
	var args map[string]json.RawMessage
	_ = json.NewDecoder(r.Body).Decode(&args)
	reply := func(v any) { _ = json.NewEncoder(w).Encode(v) }
	var ids []int64
	_ = json.Unmarshal(args["ids"], &ids)
	switch parts[0] + "/" + parts[1] {
	case "res.users/search_read":
		reply([]any{map[string]any{"id": 7, "partner_id": []any{70, "Person"}}})
	case "calendar.recurrence/read":
		out := []any{}
		for _, id := range ids {
			out = append(out, map[string]any{"id": id, "base_event_id": []any{o.recurrences[id]["base"], "base"}})
		}
		reply(out)
	case "calendar.event/search_read":
		out := []any{}
		for id, rec := range o.events {
			row := map[string]any{"id": id, "write_date": rec["write_date"], "recurrence_id": false}
			if rid, ok := rec["recurrence_id"]; ok {
				row["recurrence_id"] = rid
			}
			out = append(out, row)
		}
		reply(out)
	case "calendar.event/read":
		out := []any{}
		for _, id := range ids {
			rec, ok := o.events[id]
			if !ok {
				continue
			}
			row := map[string]any{"id": id, "recurrence_id": false}
			for k, v := range rec {
				row[k] = v
			}
			row["rrule"] = o.rruleOf(rec)
			out = append(out, row)
		}
		reply(out)
	case "calendar.event/create":
		var vals []map[string]any
		_ = json.Unmarshal(args["vals_list"], &vals)
		o.next++
		id := o.next
		v := vals[0]
		rule, _ := v["rrule"].(string)
		delete(v, "rrule")
		delete(v, "recurrency")
		o.touch(v)
		o.events[id] = v
		if rule != "" {
			o.series(id, rule)
		}
		reply([]int64{id})
	case "calendar.event/write":
		var vals map[string]any
		_ = json.Unmarshal(args["vals"], &vals)
		rec := o.events[ids[0]]
		rule, _ := vals["rrule"].(string)
		all := vals["recurrence_update"] == "all_events"
		delete(vals, "rrule")
		delete(vals, "recurrency")
		delete(vals, "recurrence_update")
		targets := []map[string]any{rec}
		if all {
			rid := rec["recurrence_id"].([]any)[0]
			targets = nil
			for _, e := range o.events {
				if r, ok := e["recurrence_id"].([]any); ok && r[0] == rid {
					targets = append(targets, e)
				}
			}
		}
		for _, t := range targets {
			for k, v := range vals {
				t[k] = v
			}
			o.touch(t)
		}
		if rule != "" && rec["recurrence_id"] == nil {
			o.series(ids[0], rule)
		}
		reply(true)
	case "calendar.event/action_mass_deletion":
		rid := o.events[ids[0]]["recurrence_id"].([]any)[0]
		for id, e := range o.events {
			if r, ok := e["recurrence_id"].([]any); ok && r[0] == rid {
				delete(o.events, id)
			}
		}
		delete(o.recurrences, rid.(int64))
		reply(true)
	case "calendar.event/unlink":
		delete(o.events, ids[0])
		reply(true)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func seriesWorld(t *testing.T) (*fakeHome, *seriesOdoo, *Engine) {
	t.Helper()
	home := &fakeHome{objects: map[string]string{}, etags: map[string]int{}}
	odoo := &seriesOdoo{events: map[int64]map[string]any{}, recurrences: map[int64]map[string]any{}}
	homeSrv, odooSrv := httptest.NewServer(home), httptest.NewServer(odoo)
	t.Cleanup(homeSrv.Close)
	t.Cleanup(odooSrv.Close)
	plan := &Plan{
		Home: Home{Calendar: &HomeOf{Profile: "nextcloud-base-ce", URL: homeSrv.URL + "/dav/"}},
		Apps: []App{{Profile: "odoo-base-ce", URL: odooSrv.URL, Kinds: map[Kind]Mapping{Calendar: {
			Driver: "odoo", Model: "calendar.event",
			Fields: map[Field]string{Summary: "name", Start: "start", End: "stop", AllDay: "allday", RRule: "rrule"},
		}}}},
	}
	return home, odoo, &Engine{
		Plan:    func() (*Plan, error) { return plan, plan.validate() },
		People:  func(context.Context) ([]string, error) { return []string{"sub-1"}, nil },
		Tokens:  fakeTokens{},
		Store:   &Memory{},
		Drivers: Drivers(nil),
		Log:     slog.New(slog.NewTextHandler(testLog{t}, nil)),
	}
}

type testLog struct{ t *testing.T }

func (l testLog) Write(p []byte) (int, error) {
	l.t.Log(strings.TrimSpace(string(p)))
	return len(p), nil
}

func weekly(uid, summary, rule string) string {
	return "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//test//EN\r\nBEGIN:VEVENT\r\nUID:" + uid + "\r\nDTSTAMP:20261119T090000Z\r\nSUMMARY:" + summary +
		"\r\nDTSTART:20261120T090000Z\r\nDTEND:20261120T100000Z\r\nRRULE:" + rule + "\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
}

// A recurring entry is one series in Odoo and one entry at the home, both
// ways: the occurrences Odoo makes are not entries of their own. A change is
// written to the whole series; a changed rule makes the series anew; a
// deletion takes the whole series.
func TestARecurringEntryIsOneSeries(t *testing.T) {
	home, odoo, engine := seriesWorld(t)
	round := func() {
		t.Helper()
		if err := engine.Round(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	home.put("/dav/cal/p/personal/w.ics", weekly("w", "Standup", "FREQ=WEEKLY;COUNT=3"))
	round()
	round()
	if len(odoo.events) != 3 || len(odoo.recurrences) != 1 {
		t.Fatalf("Odoo holds %d events in %d series, want one series of 3", len(odoo.events), len(odoo.recurrences))
	}
	if len(home.objects) != 1 {
		t.Fatalf("the home holds %d entries, want the one: the occurrences came back as entries", len(home.objects))
	}

	// Renamed at the home: every occurrence takes it, the series stays.
	home.put("/dav/cal/p/personal/w.ics", weekly("w", "Daily standup", "FREQ=WEEKLY;COUNT=3"))
	round()
	for _, e := range odoo.events {
		if e["name"] != "Daily standup" {
			t.Fatalf("an occurrence is named %v", e["name"])
		}
	}
	if len(odoo.recurrences) != 1 || len(home.objects) != 1 {
		t.Fatal("a rename made a new series or a new entry")
	}

	// The rule changes at the home: the series is made anew, still one.
	home.put("/dav/cal/p/personal/w.ics", weekly("w", "Daily standup", "FREQ=DAILY;COUNT=5"))
	round()
	round()
	if len(odoo.recurrences) != 1 || len(home.objects) != 1 {
		t.Fatalf("after a new rule: %d series, %d entries", len(odoo.recurrences), len(home.objects))
	}
	for _, r := range odoo.recurrences {
		if r["rule"] != "FREQ=DAILY;COUNT=5" {
			t.Fatalf("the series' rule is %v", r["rule"])
		}
	}

	// A series made in Odoo is one entry at the home, with its rule.
	odoo.next++
	odoo.events[odoo.next] = map[string]any{"name": "Review", "start": "2026-11-21 09:00:00", "stop": "2026-11-21 10:00:00", "allday": false}
	odoo.touch(odoo.events[odoo.next])
	odoo.series(odoo.next, "FREQ=MONTHLY;COUNT=2")
	round()
	if len(home.objects) != 2 {
		t.Fatalf("the home holds %d entries, want 2", len(home.objects))
	}
	found := false
	for _, body := range home.objects {
		if strings.Contains(body, "SUMMARY:Review") {
			found = strings.Contains(body, "RRULE:FREQ=MONTHLY;COUNT=2")
		}
	}
	if !found {
		t.Fatal("the series made in Odoo reached the home without its rule")
	}

	// Deleted at the home: the whole series goes.
	delete(home.objects, "/dav/cal/p/personal/w.ics")
	round()
	if len(odoo.recurrences) != 1 || len(odoo.events) != 3 {
		t.Fatalf("after deleting one series: %d series, %d events", len(odoo.recurrences), len(odoo.events))
	}
}
