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
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Odoo is the driver of Odoo's JSON-2 API (/json/2/<model>/<method>), called
// with the person's token of the realm, which Odoo's gentian_os module takes
// for that person. A record's stamp is its write_date. A person is found by
// their subject (res.users oauth_uid), and stands as "$user" and "$partner"
// in a mapping's filter and defaults.
type Odoo struct {
	Client *http.Client
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
	var rows []map[string]any
	if err := o.call(ctx, app, token, m.Model, "search_read", map[string]any{
		"domain": domain, "fields": []string{"id", "write_date"},
	}, &rows); err != nil {
		return nil, err
	}
	out := make(map[string]string, len(rows))
	for _, r := range rows {
		out[idOf(r["id"])] = fmt.Sprint(r["write_date"])
	}
	return out, nil
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
	n, err := intID(id)
	if err != nil {
		return nil, "", err
	}
	fields := []string{"write_date"}
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
	return v, fmt.Sprint(r["write_date"]), nil
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

// Update writes the parts onto a record.
func (o *Odoo) Update(ctx context.Context, app *App, m *Mapping, token, id string, v Values) (string, error) {
	n, err := intID(id)
	if err != nil {
		return "", err
	}
	record, err := vals(m, v)
	if err != nil {
		return "", err
	}
	if err := o.call(ctx, app, token, m.Model, "write", map[string]any{"ids": []int64{n}, "vals": record}, nil); err != nil {
		return "", err
	}
	_, stamp, err := o.Read(ctx, app, m, token, id)
	return stamp, err
}

// Delete removes a record.
func (o *Odoo) Delete(ctx context.Context, app *App, m *Mapping, token, id string) error {
	n, err := intID(id)
	if err != nil {
		return err
	}
	err = o.call(ctx, app, token, m.Model, "unlink", map[string]any{"ids": []int64{n}}, nil)
	if errors.Is(err, errGone) {
		return nil
	}
	return err
}
