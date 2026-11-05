/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package authz

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/gentian-org/gentian-os/internal/openfga/openfgatest"
)

// The requests every operation of the kernel client sends, and what each
// returns, held to testdata/kernel-wire.json: what goes to OpenFGA is part
// of how a decision is made, and it changes only on purpose.
func TestTheKernelClientsRequestsOnTheWire(t *testing.T) {
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx := context.Background()
	fixed := func() time.Time { return time.Unix(0, 0) }
	opts := func(url string) Options {
		return Options{BaseURL: url + "/", APIToken: "tok", StoreID: "S1", ModelID: "M1", Logger: quiet, Now: fixed}
	}
	client := func(t *testing.T, o Options) *OpenFGA {
		c, err := NewOpenFGA(o)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	errOf := func(err error) any {
		if err == nil {
			return nil
		}
		return map[string]any{"error": err.Error(), "notFound": isNotFound(err)}
	}
	var withID map[string]any
	if err := json.Unmarshal(modelV1, &withID); err != nil {
		t.Fatal(err)
	}
	withID["id"] = "M9"
	model, _ := json.Marshal(withID)
	modelsPage := func(token string, models ...string) string {
		return fmt.Sprintf(`{"authorization_models":[%s],"continuation_token":%q}`, strings.Join(models, ","), token)
	}
	tuple := func(i int) Tuple {
		return Tuple{User: fmt.Sprintf("user:u%d", i), Relation: "member", Object: "group:g"}
	}
	tuples := func(n int) []Tuple {
		out := make([]Tuple, n)
		for i := range out {
			out[i] = tuple(i)
		}
		return out
	}
	page := `{"tuples":[{"key":{"user":"user:a","relation":"r","object":"o:1"}}],"continuation_token":"t+/=&x"}`
	last := `{"tuples":[{"key":{"user":"user:b","relation":"r","object":"o:2"}}],"continuation_token":""}`

	type run func(t *testing.T, url string) any
	cases := []struct {
		name    string
		replies []openfgatest.Reply
		run     run
	}{
		{"check allowed", []openfgatest.Reply{openfgatest.OK(`{"allowed":true}`)}, func(t *testing.T, url string) any {
			ok, err := client(t, opts(url)).Check(ctx, "req-1", "user:a", "viewer", "tenant:t")
			return []any{ok, errOf(err)}
		}},
		{"check denied", []openfgatest.Reply{openfgatest.OK(`{"allowed":false}`)}, func(t *testing.T, url string) any {
			ok, err := client(t, opts(url)).Check(ctx, "req-1", "user:a", "viewer", "tenant:t")
			return []any{ok, errOf(err)}
		}},
		{"check refused", []openfgatest.Reply{{Status: 403, Body: ` {"code":"x","message":"m"} `}}, func(t *testing.T, url string) any {
			ok, err := client(t, opts(url)).Check(ctx, "req-1", "user:a", "viewer", "tenant:t")
			return []any{ok, errOf(err)}
		}},
		{"check answered 204", []openfgatest.Reply{{Status: 204}}, func(t *testing.T, url string) any {
			ok, err := client(t, opts(url)).Check(ctx, "req-1", "user:a", "viewer", "tenant:t")
			return []any{ok, errOf(err)}
		}},
		{"check answered garbage", []openfgatest.Reply{openfgatest.OK(`nope`)}, func(t *testing.T, url string) any {
			ok, err := client(t, opts(url)).Check(ctx, "req-1", "user:a", "viewer", "tenant:t")
			return []any{ok, errOf(err)}
		}},
		{"check without a token", []openfgatest.Reply{openfgatest.OK(`{"allowed":true}`)}, func(t *testing.T, url string) any {
			o := opts(url)
			o.APIToken = ""
			ok, err := client(t, o).Check(ctx, "req-1", "", "", "")
			return []any{ok, errOf(err)}
		}},
		{"check unreachable", nil, func(t *testing.T, _ string) any {
			ok, err := client(t, opts("http://127.0.0.1:1")).Check(ctx, "req-1", "user:a", "viewer", "tenant:t")
			return []any{ok, errOf(err)}
		}},
		{"read everything, two pages", []openfgatest.Reply{openfgatest.OK(page), openfgatest.OK(last)}, func(t *testing.T, url string) any {
			got, err := client(t, opts(url)).Read(ctx, Tuple{})
			return []any{got, errOf(err)}
		}},
		{"read by user and type", []openfgatest.Reply{openfgatest.OK(last)}, func(t *testing.T, url string) any {
			got, err := client(t, opts(url)).Read(ctx, Tuple{User: "user:a", Object: "group:"})
			return []any{got, errOf(err)}
		}},
		{"read by relation and object", []openfgatest.Reply{openfgatest.OK(last)}, func(t *testing.T, url string) any {
			got, err := client(t, opts(url)).Read(ctx, Tuple{Relation: "member", Object: "group:g"})
			return []any{got, errOf(err)}
		}},
		{"read by all three", []openfgatest.Reply{openfgatest.OK(last)}, func(t *testing.T, url string) any {
			got, err := client(t, opts(url)).Read(ctx, Tuple{User: "user:a", Relation: "member", Object: "group:g"})
			return []any{got, errOf(err)}
		}},
		{"read past an empty page", []openfgatest.Reply{openfgatest.OK(`{"tuples":[],"continuation_token":"more"}`), openfgatest.OK(last)}, func(t *testing.T, url string) any {
			got, err := client(t, opts(url)).Read(ctx, Tuple{Object: "group:g"})
			return []any{got, errOf(err)}
		}},
		{"read fails on the second page", []openfgatest.Reply{openfgatest.OK(page), {Status: 500, Body: "boom"}}, func(t *testing.T, url string) any {
			got, err := client(t, opts(url)).Read(ctx, Tuple{Object: "group:g"})
			return []any{got, errOf(err)}
		}},
		{"read unreachable", nil, func(t *testing.T, _ string) any {
			got, err := client(t, opts("http://127.0.0.1:1")).Read(ctx, Tuple{Object: "group:g"})
			return []any{got, errOf(err)}
		}},
		{"read answered garbage", []openfgatest.Reply{openfgatest.OK(`[`)}, func(t *testing.T, url string) any {
			got, err := client(t, opts(url)).Read(ctx, Tuple{Object: "group:g"})
			return []any{got, errOf(err)}
		}},
		{"write in chunks, deletes first", []openfgatest.Reply{openfgatest.OK(`{}`), openfgatest.OK(`{}`), openfgatest.OK(`{}`), openfgatest.OK(`{}`)}, func(t *testing.T, url string) any {
			return errOf(client(t, opts(url)).Write(ctx, tuples(120), tuples(150)))
		}},
		{"write only writes", []openfgatest.Reply{openfgatest.OK(`{}`)}, func(t *testing.T, url string) any {
			return errOf(client(t, opts(url)).Write(ctx, tuples(2), nil))
		}},
		{"write nothing", nil, func(t *testing.T, url string) any {
			return errOf(client(t, opts(url)).Write(ctx, nil, nil))
		}},
		{"write refused", []openfgatest.Reply{{Status: 400, Body: `{"code":"validation_error","message":"bad"}`}}, func(t *testing.T, url string) any {
			return errOf(client(t, opts(url)).Write(ctx, tuples(1), tuples(1)))
		}},
		{"changes of a type from a token", []openfgatest.Reply{openfgatest.OK(`{"changes":[
			{"tuple_key":{"user":"user:a","relation":"member","object":"group:g"},"operation":"TUPLE_OPERATION_WRITE","timestamp":"2025-01-01T00:00:00Z"},
			{"tuple_key":{"user":"user:b","relation":"member","object":"group:g"},"operation":"TUPLE_OPERATION_DELETE","timestamp":"2025-01-02T00:00:00Z"}
		],"continuation_token":"next"}`)}, func(t *testing.T, url string) any {
			got, next, err := client(t, opts(url)).Changes(ctx, "group", "a+b/=&c")
			return []any{got, next, errOf(err)}
		}},
		{"changes of everything from the start", []openfgatest.Reply{openfgatest.OK(`{"changes":[]}`)}, func(t *testing.T, url string) any {
			got, next, err := client(t, opts(url)).Changes(ctx, "", "")
			return []any{got, next, errOf(err)}
		}},
		{"changes refused", []openfgatest.Reply{{Status: 404, Body: "gone"}}, func(t *testing.T, url string) any {
			got, next, err := client(t, opts(url)).Changes(ctx, "group", "")
			return []any{got, next, errOf(err)}
		}},
		{"bootstrap keeps the recorded store and its model", []openfgatest.Reply{
			openfgatest.OK(`{"id":"S1","name":"gentian"}`), openfgatest.OK(modelsPage("", string(model))),
		}, func(t *testing.T, url string) any {
			s, m, err := Bootstrap(ctx, Options{BaseURL: url, APIToken: "tok", StoreID: "S1", Logger: quiet})
			return []any{s, m, errOf(err)}
		}},
		{"bootstrap replaces a recorded store that is gone", []openfgatest.Reply{
			{Status: 404, Body: "{}"}, openfgatest.OK(`{"id":"S2"}`), openfgatest.OK(modelsPage("")), openfgatest.OK(`{"authorization_model_id":"M2"}`),
		}, func(t *testing.T, url string) any {
			s, m, err := Bootstrap(ctx, Options{BaseURL: url, APIToken: "tok", StoreID: "S1", Logger: quiet})
			return []any{s, m, errOf(err)}
		}},
		{"bootstrap replaces a recorded store answered under another id", []openfgatest.Reply{
			openfgatest.OK(`{"id":"other"}`), openfgatest.OK(`{"id":"S2"}`), {Status: 500, Body: "models down"},
		}, func(t *testing.T, url string) any {
			s, m, err := Bootstrap(ctx, Options{BaseURL: url, StoreID: "S1", Logger: quiet})
			return []any{s, m, errOf(err)}
		}},
		{"bootstrap fails reading the recorded store", []openfgatest.Reply{{Status: 500, Body: "down"}}, func(t *testing.T, url string) any {
			s, m, err := Bootstrap(ctx, Options{BaseURL: url, APIToken: "tok", StoreID: "S 1", Logger: quiet})
			return []any{s, m, errOf(err)}
		}},
		{"bootstrap fails creating a store", []openfgatest.Reply{{Status: 404}, {Status: 409, Body: "exists"}}, func(t *testing.T, url string) any {
			s, m, err := Bootstrap(ctx, Options{BaseURL: url, APIToken: "tok", StoreID: "S1", Logger: quiet})
			return []any{s, m, errOf(err)}
		}},
		{"bootstrap without a record creates the store and adopts the oldest", []openfgatest.Reply{
			openfgatest.OK(`{"stores":[{"id":"x","name":"other"}],"continuation_token":"p+2/="}`),
			openfgatest.OK(`{"stores":[],"continuation_token":""}`),
			openfgatest.OK(`{"id":"mine","name":"gentian"}`),
			openfgatest.OK(`{"stores":[{"id":"mine","name":"gentian","created_at":"2025-01-02T00:00:00Z"},{"id":"theirs","name":"gentian","created_at":"2025-01-01T00:00:00Z"}]}`),
			openfgatest.OK(modelsPage("", `{"id":"old","type_definitions":[]}`)),
			openfgatest.OK(`{"authorization_model_id":"M3"}`),
		}, func(t *testing.T, url string) any {
			s, m, err := Bootstrap(ctx, Options{BaseURL: url, APIToken: "tok", Logger: quiet})
			return []any{s, m, errOf(err)}
		}},
		{"bootstrap without a record keeps its own store when the second listing fails", []openfgatest.Reply{
			openfgatest.OK(`{"stores":[]}`), openfgatest.OK(`{"id":"mine"}`), {Status: 503, Body: "busy"}, openfgatest.OK(modelsPage("", string(model))),
		}, func(t *testing.T, url string) any {
			s, m, err := Bootstrap(ctx, Options{BaseURL: url, APIToken: "tok", Logger: quiet})
			return []any{s, m, errOf(err)}
		}},
		{"bootstrap without a record fails listing", []openfgatest.Reply{{Status: 401, Body: "who"}}, func(t *testing.T, url string) any {
			s, m, err := Bootstrap(ctx, Options{BaseURL: url, APIToken: "tok", Logger: quiet})
			return []any{s, m, errOf(err)}
		}},
		{"bootstrap fails writing the model", []openfgatest.Reply{
			openfgatest.OK(`{"id":"S1"}`), openfgatest.OK(modelsPage("")), {Status: 400, Body: `{"message":"invalid"}`},
		}, func(t *testing.T, url string) any {
			s, m, err := Bootstrap(ctx, Options{BaseURL: url, APIToken: "tok", StoreID: "S1", Logger: quiet})
			return []any{s, m, errOf(err)}
		}},
		{"pinned takes the recorded model", []openfgatest.Reply{openfgatest.OK(`{"authorization_model":` + string(model) + `}`)}, func(t *testing.T, url string) any {
			s, m, err := Pinned(ctx, opts(url))
			return []any{s, m, errOf(err)}
		}},
		{"pinned searches the store when the record is not this binary's", []openfgatest.Reply{
			openfgatest.OK(`{"authorization_model":{"id":"M1","type_definitions":[]}}`),
			openfgatest.OK(modelsPage("q+1/=&x", `{"id":"a","type_definitions":[]}`)),
			openfgatest.OK(modelsPage("", string(model))),
		}, func(t *testing.T, url string) any {
			o := opts(url)
			o.ModelID = "M 1/?x"
			s, m, err := Pinned(ctx, o)
			return []any{s, m, errOf(err)}
		}},
		{"pinned looks once: the record is gone and so is the model", []openfgatest.Reply{
			{Status: 404}, openfgatest.OK(modelsPage("")),
		}, func(t *testing.T, url string) any {
			c, err := newClient(opts(url))
			if err != nil {
				t.Fatal(err)
			}
			m, err := c.pinnedOnce(ctx, opts(url), withID)
			return []any{m, errOf(err)}
		}},
		{"pinned looks once: the record is not a model", []openfgatest.Reply{
			openfgatest.OK(`{"authorization_model":"x"}`), openfgatest.OK(modelsPage("")),
		}, func(t *testing.T, url string) any {
			c, err := newClient(opts(url))
			if err != nil {
				t.Fatal(err)
			}
			m, err := c.pinnedOnce(ctx, opts(url), withID)
			return []any{m, errOf(err)}
		}},
		{"pinned looks once: reading the record fails", []openfgatest.Reply{{Status: 500, Body: "down"}}, func(t *testing.T, url string) any {
			c, err := newClient(opts(url))
			if err != nil {
				t.Fatal(err)
			}
			m, err := c.pinnedOnce(ctx, opts(url), withID)
			return []any{m, errOf(err)}
		}},
		{"pinned looks once: listing fails", []openfgatest.Reply{{Status: 404}, {Status: 500, Body: "down"}}, func(t *testing.T, url string) any {
			c, err := newClient(opts(url))
			if err != nil {
				t.Fatal(err)
			}
			m, err := c.pinnedOnce(ctx, opts(url), withID)
			return []any{m, errOf(err)}
		}},
		{"pinned looks once: five pages and no further", []openfgatest.Reply{
			openfgatest.OK(modelsPage("1")), openfgatest.OK(modelsPage("2")), openfgatest.OK(modelsPage("3")),
			openfgatest.OK(modelsPage("4")), openfgatest.OK(modelsPage("5")), openfgatest.OK(modelsPage("6")),
		}, func(t *testing.T, url string) any {
			c, err := newClient(opts(url))
			if err != nil {
				t.Fatal(err)
			}
			o := opts(url)
			o.ModelID = "M1"
			// The recorded read takes the first reply: a list page, which is
			// no model.
			m, err := c.pinnedOnce(ctx, o, withID)
			return []any{m, errOf(err)}
		}},
	}
	var got []openfgatest.Case
	for _, tc := range cases {
		rec := openfgatest.New(t, tc.replies...)
		result := tc.run(t, rec.URL)
		got = append(got, openfgatest.Case{Name: tc.name, Requests: rec.Requests(), Result: result})
	}
	openfgatest.Compare(t, "kernel-wire", got)
}
