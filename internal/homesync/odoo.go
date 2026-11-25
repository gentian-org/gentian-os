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
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Odoo is the driver of Odoo's JSON-2 API (/json/2/<model>/<method>), called
// with the person's token of the realm, which Odoo's gentian_os module takes
// for that person. A record's stamp is its write_date. A person is found by
// their subject (res.users oauth_uid), and stands as "$user" and "$partner"
// in a mapping's filter and defaults.
//
// A recurring meeting is a series in Odoo: one calendar.event per occurrence,
// tied by a calendar.recurrence to the base event that carries the rule. The
// home keeps one entry with a rule. So, for a mapping that keeps the rule
// (rrule), the driver shows only the base event of a series, writes a change
// to every occurrence (recurrence_update all_events, which keeps the base
// event's id), and makes a series whose rule changed or ended anew, since
// Odoo builds the rule of a rewritten series from its own fields and not
// from a new rule. A single occurrence moved or cancelled is not kept equal.
//
// A mapped field the installed Odoo does not have (one an Odoo release
// removed) is left out, with a warning once, rather than failing every
// record of the kind.
type Odoo struct {
	Client *http.Client
	Log    *slog.Logger

	mu    sync.Mutex
	known map[string]map[string]bool // by app address and model
}

// mapped is the mapping with only the fields the app's model has. Where the
// app does not say which it has, the mapping is used as it is.
func (o *Odoo) mapped(ctx context.Context, app *App, m *Mapping, token string) *Mapping {
	key := app.URL + "\n" + m.Model
	o.mu.Lock()
	has, asked := o.known[key]
	o.mu.Unlock()
	if !asked {
		var fields map[string]any
		if err := o.call(ctx, app, token, m.Model, "fields_get", map[string]any{"attributes": []string{"type"}}, &fields); err != nil || len(fields) == 0 {
			return m
		}
		has = make(map[string]bool, len(fields))
		for f := range fields {
			has[f] = true
		}
		var missing []string
		for _, f := range m.Fields {
			if !has[f] {
				missing = append(missing, f)
			}
		}
		if len(missing) > 0 && o.Log != nil {
			sort.Strings(missing)
			o.Log.Warn("fields the app does not have are left out", "app", app.Profile, "model", m.Model, "fields", missing)
		}
		o.mu.Lock()
		if o.known == nil {
			o.known = map[string]map[string]bool{}
		}
		o.known[key] = has
		o.mu.Unlock()
	}
	out := *m
	out.Fields = make(map[Field]string, len(m.Fields))
	for part, f := range m.Fields {
		if has[f] {
			out.Fields[part] = f
		}
	}
	return &out
}

const odooTime = "2006-01-02 15:04:05"

// errGone is an answer about a record that is not there.
var errGone = errors.New("the record is not there")

func (o *Odoo) call(ctx context.Context, app *App, token, model, method string, args any, out any) error {
	body, err := json.Marshal(args)
	if err != nil {
		return err
	}
	url := strings.TrimSuffix(app.URL, "/") + "/json/2/" + model + "/" + method
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "bearer "+token)
	client := o.Client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode == http.StatusNotFound {
		return errGone
	}
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Name    string `json:"name"`
			Message string `json:"message"`
		}
		_ = json.Unmarshal(raw, &e)
		if strings.Contains(e.Name, "MissingError") {
			return errGone
		}
		return fmt.Errorf("%s %s.%s answered %d: %s", app.Profile, model, method, resp.StatusCode, e.Message)
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(raw, out)
}

// Person finds the person by their subject.
func (o *Odoo) Person(ctx context.Context, app *App, token, person string) (Person, error) {
	var users []struct {
		ID      int64 `json:"id"`
		Partner []any `json:"partner_id"`
	}
	err := o.call(ctx, app, token, "res.users", "search_read", map[string]any{
		"domain": []any{[]any{"oauth_uid", "=", person}}, "fields": []string{"id", "partner_id"}, "limit": 1,
	}, &users)
	if err != nil {
		return nil, err
	}
	if len(users) == 0 {
		return nil, fmt.Errorf("%s has no user of the person", app.Profile)
	}
	who := Person{"$user": users[0].ID}
	if len(users[0].Partner) > 0 {
		if id, ok := users[0].Partner[0].(float64); ok {
			who["$partner"] = int64(id)
		}
	}
	return who, nil
}

// placed is a mapping's JSON with the placeholders put in.
func placed(raw json.RawMessage, who Person) (any, error) {
	s := string(raw)
	keys := make([]string, 0, len(who))
	for k := range who {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v, err := json.Marshal(who[k])
		if err != nil {
			return nil, err
		}
		s = strings.ReplaceAll(s, strconv.Quote(k), string(v))
	}
	var out any
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return nil, err
	}
	return out, nil
}

// Stamps lists the person's records by write_date.
func (o *Odoo) Stamps(ctx context.Context, app *App, m *Mapping, token string, who Person) (map[string]string, error) {
	domain := any([]any{})
	if len(m.Filter) > 0 {
		d, err := placed(m.Filter, who)
		if err != nil {
			return nil, err
		}
		domain = d
	}
	fields := []string{"id", "write_date"}
	if keepsSeries(m) {
		fields = append(fields, "recurrence_id")
	}
	var rows []map[string]any
	if err := o.call(ctx, app, token, m.Model, "search_read", map[string]any{
		"domain": domain, "fields": fields,
	}, &rows); err != nil {
		return nil, err
	}
	bases, err := o.bases(ctx, app, token, rows)
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(rows))
	for _, r := range rows {
		id := idOf(r["id"])
		if rec := many2oneID(r["recurrence_id"]); rec != "" && bases[rec] != id {
			continue // an occurrence that is not the series' base
		}
		out[id] = fmt.Sprint(r["write_date"])
	}
	return out, nil
}

// keepsSeries reports whether a mapping keeps the rule of Odoo's series.
func keepsSeries(m *Mapping) bool {
	_, ok := m.Fields[RRule]
	return ok && m.Model == "calendar.event"
}

// bases are the base events of the series the rows belong to, by series.
func (o *Odoo) bases(ctx context.Context, app *App, token string, rows []map[string]any) (map[string]string, error) {
	seen := map[string]bool{}
	var ids []int64
	for _, r := range rows {
		if rec := many2oneID(r["recurrence_id"]); rec != "" && !seen[rec] {
			seen[rec] = true
			n, _ := intID(rec)
			ids = append(ids, n)
		}
	}
	out := map[string]string{}
	if len(ids) == 0 {
		return out, nil
	}
	var recs []map[string]any
	if err := o.call(ctx, app, token, "calendar.recurrence", "read", map[string]any{
		"ids": ids, "fields": []string{"base_event_id"},
	}, &recs); err != nil {
		return nil, err
	}
	for _, r := range recs {
		out[idOf(r["id"])] = many2oneID(r["base_event_id"])
	}
	return out, nil
}

// many2oneID is the id of a many2one value ([id, name]), or "".
func many2oneID(v any) string {
	if x, ok := v.([]any); ok && len(x) == 2 {
		return idOf(x[0])
	}
	return ""
}

func idOf(v any) string {
	if f, ok := v.(float64); ok {
		return strconv.FormatInt(int64(f), 10)
	}
	return fmt.Sprint(v)
}

func intID(id string) (int64, error) { return strconv.ParseInt(id, 10, 64) }

// Read reads one record's mapped fields.
func (o *Odoo) Read(ctx context.Context, app *App, m *Mapping, token, id string) (Values, string, error) {
	m = o.mapped(ctx, app, m, token)
	n, err := intID(id)
	if err != nil {
		return nil, "", err
	}
	fields := []string{"write_date"}
	if keepsSeries(m) {
		fields = append(fields, "recurrence_id")
	}
	for _, f := range m.Fields {
		fields = append(fields, f)
	}
	sort.Strings(fields)
	var rows []map[string]any
	if err := o.call(ctx, app, token, m.Model, "read", map[string]any{"ids": []int64{n}, "fields": fields}, &rows); err != nil {
		return nil, "", err
	}
	if len(rows) == 0 {
		return nil, "", errGone
	}
	r := rows[0]
	v := Values{}
	for part, field := range m.Fields {
		v[part] = fromOdoo(part, r[field])
	}
	if keepsSeries(m) {
		if many2oneID(r["recurrence_id"]) == "" {
			v[RRule] = ""
		} else {
			v[RRule] = ruleOnly(v[RRule])
		}
	}
	return v, fmt.Sprint(r["write_date"]), nil
}

// ruleOnly is the rule of Odoo's serialized rule, which names its start as
// well ("DTSTART:...\nRRULE:FREQ=..."): the part after RRULE:.
func ruleOnly(serialized string) string {
	for _, line := range strings.Split(serialized, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "RRULE:") {
			return strings.TrimPrefix(line, "RRULE:")
		}
	}
	if strings.HasPrefix(serialized, "FREQ=") {
		return serialized
	}
	return ""
}

// fromOdoo is an Odoo value as the text of a part.
func fromOdoo(part Field, value any) string {
	switch x := value.(type) {
	case nil:
		return ""
	case bool:
		if part == AllDay {
			return strconv.FormatBool(x)
		}
		if !x {
			return "" // Odoo's empty field
		}
		return "true"
	case []any: // a many2one: [id, name]
		if len(x) == 2 {
			return fmt.Sprint(x[1])
		}
		return ""
	case string:
		if part == Start || part == End {
			if t, err := time.Parse(odooTime, x); err == nil {
				return t.UTC().Format(timeLayout)
			}
		}
		return x
	default:
		return fmt.Sprint(x)
	}
}

// vals are the parts as Odoo's values. An all-day entry writes its dates and
// no times, one that is not its times and no dates, so that Odoo computes
// the rest itself.
func vals(m *Mapping, v Values) (map[string]any, error) {
	out := map[string]any{}
	allDay := v[AllDay] == "true"
	for part, field := range m.Fields {
		s, ok := v[part]
		if !ok {
			continue
		}
		switch part {
		case AllDay:
			out[field] = allDay
		case Start, End:
			if allDay {
				continue
			}
			t, err := time.Parse(timeLayout, s)
			if err != nil {
				return nil, fmt.Errorf("%s is no time: %q", part, s)
			}
			out[field] = t.UTC().Format(odooTime)
		case StartDate, EndDate:
			if !allDay {
				continue
			}
			out[field] = s
		case RRule:
			// Written as the series it is: a rule makes one, and only
			// Create and Update say what happens to an existing one.
			if s != "" && m.Model == "calendar.event" {
				out["recurrency"] = true
				out[field] = s
			}
		default:
			if s == "" {
				out[field] = false
			} else {
				out[field] = s
			}
		}
	}
	return out, nil
}

// Create makes a record with the mapping's defaults.
func (o *Odoo) Create(ctx context.Context, app *App, m *Mapping, token string, who Person, v Values) (string, string, error) {
	m = o.mapped(ctx, app, m, token)
	record, err := vals(m, v)
	if err != nil {
		return "", "", err
	}
	for k, raw := range m.Defaults {
		d, err := placed(raw, who)
		if err != nil {
			return "", "", err
		}
		record[k] = d
	}
	var ids []float64
	if err := o.call(ctx, app, token, m.Model, "create", map[string]any{"vals_list": []any{record}}, &ids); err != nil {
		return "", "", err
	}
	if len(ids) != 1 {
		return "", "", fmt.Errorf("%s made %d records of one", app.Profile, len(ids))
	}
	id := strconv.FormatInt(int64(ids[0]), 10)
	_, stamp, err := o.Read(ctx, app, m, token, id)
	return id, stamp, err
}

// Update writes the parts onto a record. A series is written as a whole; a
// series whose rule changed or ended is made anew, and its new id returned.
func (o *Odoo) Update(ctx context.Context, app *App, m *Mapping, token string, who Person, id string, v Values) (string, string, error) {
	m = o.mapped(ctx, app, m, token)
	n, err := intID(id)
	if err != nil {
		return "", "", err
	}
	record, err := vals(m, v)
	if err != nil {
		return "", "", err
	}
	if keepsSeries(m) {
		if rule, given := v[RRule]; given {
			current, _, err := o.Read(ctx, app, m, token, id)
			if err != nil {
				return "", "", err
			}
			inSeries := current[RRule] != ""
			switch {
			case inSeries && rule != current[RRule]:
				// Changed or ended: Odoo would keep its own rule on a
				// rewrite, so the series is made again.
				if err := o.Delete(ctx, app, m, token, id); err != nil {
					return "", "", err
				}
				return o.Create(ctx, app, m, token, who, v)
			case inSeries:
				delete(record, "recurrency")
				delete(record, m.Fields[RRule])
				record["recurrence_update"] = "all_events"
			}
			// Not a series yet: a rule given makes one of this event,
			// which keeps its id; none leaves it as it is.
		}
	}
	if err := o.call(ctx, app, token, m.Model, "write", map[string]any{"ids": []int64{n}, "vals": record}, nil); err != nil {
		return "", "", err
	}
	_, stamp, err := o.Read(ctx, app, m, token, id)
	return id, stamp, err
}

// Delete removes a record; a series' base event removes the series.
func (o *Odoo) Delete(ctx context.Context, app *App, m *Mapping, token, id string) error {
	n, err := intID(id)
	if err != nil {
		return err
	}
	if keepsSeries(m) {
		var rows []map[string]any
		err := o.call(ctx, app, token, m.Model, "read", map[string]any{"ids": []int64{n}, "fields": []string{"recurrence_id"}}, &rows)
		if errors.Is(err, errGone) || (err == nil && len(rows) == 0) {
			return nil
		}
		if err != nil {
			return err
		}
		if many2oneID(rows[0]["recurrence_id"]) != "" {
			return o.call(ctx, app, token, m.Model, "action_mass_deletion", map[string]any{
				"ids": []int64{n}, "recurrence_update_setting": "all_events",
			}, nil)
		}
	}
	err = o.call(ctx, app, token, m.Model, "unlink", map[string]any{"ids": []int64{n}}, nil)
	if errors.Is(err, errGone) {
		return nil
	}
	return err
}
