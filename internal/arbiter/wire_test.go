/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package arbiter

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gentian-org/gentian-os/internal/openfga"
	"github.com/gentian-org/gentian-os/internal/openfga/openfgatest"
)

// The requests every operation of the arbiter's client sends, and what each
// returns, held to testdata/arbiter-wire.json.
func TestTheArbitersRequestsOnTheWire(t *testing.T) {
	ctx := context.Background()
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	// errOf is an error as the arbiter's callers see it: its text, the
	// statuses it is checked for, and what answerError makes of it.
	errOf := func(err error) any {
		if err == nil {
			return nil
		}
		w := httptest.NewRecorder()
		(&Server{log: quiet}).answerError(w, httptest.NewRequest(http.MethodPost, "/v1/x", nil), Entry{Tenant: "t", Component: "c"}, err)
		return map[string]any{
			"error": err.Error(), "is400": openfga.IsStatus(err, 400), "is404": openfga.IsStatus(err, 404),
			"answered": fmt.Sprintf("%d %s", w.Code, w.Body.String()),
		}
	}
	tuples := func(n int) []Tuple {
		out := make([]Tuple, n)
		for i := range out {
			out[i] = Tuple{User: fmt.Sprintf("user:u%d", i), Relation: "member", Object: "group:g"}
		}
		return out
	}
	page := `{"tuples":[{"key":{"user":"user:a","relation":"r","object":"o:1"}}],"continuation_token":"t+/=&x"}`
	last := `{"tuples":[{"key":{"user":"user:b","relation":"r","object":"o:2"}}],"continuation_token":""}`

	cases := []struct {
		name    string
		replies []openfgatest.Reply
		run     func(c *openFGA) any
	}{
		{"create a store", []openfgatest.Reply{openfgatest.OK(`{"id":"S1","name":"t/c"}`)}, func(c *openFGA) any {
			id, err := c.createStore(ctx, "t/c")
			return []any{id, errOf(err)}
		}},
		{"create a store answered without an id", []openfgatest.Reply{openfgatest.OK(`{}`)}, func(c *openFGA) any {
			id, err := c.createStore(ctx, "t/c")
			return []any{id, errOf(err)}
		}},
		{"create a store refused", []openfgatest.Reply{{Status: 400, Body: ` {"code":"validation_error","message":"name too long"} `}}, func(c *openFGA) any {
			id, err := c.createStore(ctx, "t/c")
			return []any{id, errOf(err)}
		}},
		{"create a store fails", []openfgatest.Reply{{Status: 500, Body: `{"message":"internal"}`}}, func(c *openFGA) any {
			id, err := c.createStore(ctx, "t/c")
			return []any{id, errOf(err)}
		}},
		{"delete a store", []openfgatest.Reply{{Status: 204}}, func(c *openFGA) any {
			return errOf(c.deleteStore(ctx, "S1"))
		}},
		{"delete a store that is gone", []openfgatest.Reply{{Status: 404, Body: "{}"}}, func(c *openFGA) any {
			return errOf(c.deleteStore(ctx, "S1"))
		}},
		{"delete a store fails", []openfgatest.Reply{{Status: 503, Body: "busy"}}, func(c *openFGA) any {
			return errOf(c.deleteStore(ctx, "S1"))
		}},
		{"write a model", []openfgatest.Reply{openfgatest.OK(`{"authorization_model_id":"M1"}`)}, func(c *openFGA) any {
			id, err := c.writeModel(ctx, "S1", json.RawMessage("{ \"schema_version\": \"1.1\",\n \"type_definitions\": [] }"))
			return []any{id, errOf(err)}
		}},
		{"write a model answered without an id", []openfgatest.Reply{openfgatest.OK(`{}`)}, func(c *openFGA) any {
			id, err := c.writeModel(ctx, "S1", json.RawMessage(`{}`))
			return []any{id, errOf(err)}
		}},
		{"write a model refused", []openfgatest.Reply{{Status: 400, Body: `not json`}}, func(c *openFGA) any {
			id, err := c.writeModel(ctx, "S1", json.RawMessage(`{}`))
			return []any{id, errOf(err)}
		}},
		{"write tuples in chunks under a model", []openfgatest.Reply{openfgatest.OK(`{}`), openfgatest.OK(`{}`), openfgatest.OK(`{}`)}, func(c *openFGA) any {
			return errOf(c.writeTuples(ctx, "S1", "M1", tuples(30), tuples(150)))
		}},
		{"write tuples under no model", []openfgatest.Reply{openfgatest.OK(`{}`)}, func(c *openFGA) any {
			return errOf(c.writeTuples(ctx, "S1", "", tuples(1), nil))
		}},
		{"write tuples with empty fields", []openfgatest.Reply{openfgatest.OK(`{}`)}, func(c *openFGA) any {
			return errOf(c.writeTuples(ctx, "S1", "M1", nil, []Tuple{{Object: "group:g"}}))
		}},
		{"write no tuples", nil, func(c *openFGA) any {
			return errOf(c.writeTuples(ctx, "S1", "M1", nil, nil))
		}},
		{"write tuples refused", []openfgatest.Reply{{Status: 400, Body: `{"message":"type 'x' not found"}`}}, func(c *openFGA) any {
			return errOf(c.writeTuples(ctx, "S1", "M1", tuples(1), nil))
		}},
		{"read everything, two pages", []openfgatest.Reply{openfgatest.OK(page), openfgatest.OK(last)}, func(c *openFGA) any {
			got, err := c.readAll(ctx, "S1", fgaTuple{})
			return []any{got, errOf(err)}
		}},
		{"read by relation and type", []openfgatest.Reply{openfgatest.OK(last)}, func(c *openFGA) any {
			got, err := c.readAll(ctx, "S1", fgaTuple{Relation: "member", Object: "group:"})
			return []any{got, errOf(err)}
		}},
		{"read by user and type", []openfgatest.Reply{openfgatest.OK(last)}, func(c *openFGA) any {
			got, err := c.readAll(ctx, "S1", fgaTuple{User: "user:a", Object: "group:"})
			return []any{got, errOf(err)}
		}},
		{"read past an empty page", []openfgatest.Reply{openfgatest.OK(`{"tuples":[],"continuation_token":"more"}`), openfgatest.OK(last)}, func(c *openFGA) any {
			got, err := c.readAll(ctx, "S1", fgaTuple{Object: "group:g"})
			return []any{got, errOf(err)}
		}},
		{"read fails on the second page", []openfgatest.Reply{openfgatest.OK(page), {Status: 500, Body: "boom"}}, func(c *openFGA) any {
			got, err := c.readAll(ctx, "S1", fgaTuple{Object: "group:g"})
			return []any{got, errOf(err)}
		}},
		{"read answered garbage", []openfgatest.Reply{openfgatest.OK(`[`)}, func(c *openFGA) any {
			got, err := c.readAll(ctx, "S1", fgaTuple{Object: "group:g"})
			return []any{got, errOf(err)}
		}},
		{"read a model", []openfgatest.Reply{openfgatest.OK(`{"authorization_model":{"id":"M1","type_definitions":[]}}`)}, func(c *openFGA) any {
			got, err := c.readModel(ctx, "S1", "M1")
			return []any{string(got), errOf(err)}
		}},
		{"read a model that is gone", []openfgatest.Reply{{Status: 404, Body: `{"code":"authorization_model_not_found","message":"gone"}`}}, func(c *openFGA) any {
			got, err := c.readModel(ctx, "S1", "M1")
			return []any{string(got), errOf(err)}
		}},
		{"ask directly with a body", []openfgatest.Reply{openfgatest.OK(`{"allowed":true}`)}, func(c *openFGA) any {
			var out map[string]any
			err := c.Do(ctx, http.MethodPost, storePath("S1", "/check"), map[string]any{"tuple_key": fgaTuple{User: "user:a", Relation: "viewer", Object: "doc:1"}, "authorization_model_id": "M1"}, &out)
			return []any{out, errOf(err)}
		}},
		{"ask directly without a body or an answer", []openfgatest.Reply{openfgatest.OK(`{"ignored":true}`)}, func(c *openFGA) any {
			return errOf(c.Do(ctx, http.MethodGet, storePath("S1", ""), nil, nil))
		}},
		{"ask directly and be refused", []openfgatest.Reply{{Status: 400, Body: `{"code":"validation_error","message":"type 'undefined' not found"}`}}, func(c *openFGA) any {
			return errOf(c.Do(ctx, http.MethodPost, storePath("S1", "/list-objects"), map[string]any{"type": "undefined"}, nil))
		}},
		{"ask directly and fail", []openfgatest.Reply{{Status: 502, Body: `{"message":"upstream"}`}}, func(c *openFGA) any {
			return errOf(c.Do(ctx, http.MethodPost, storePath("S1", "/list-users"), map[string]any{}, nil))
		}},
	}
	var got []openfgatest.Case
	for _, tc := range cases {
		rec := openfgatest.New(t, tc.replies...)
		result := tc.run(NewOpenFGA(rec.URL+"/", "tok"))
		got = append(got, openfgatest.Case{Name: tc.name, Requests: rec.Requests(), Result: result})
	}
	// An unreachable instance, and one without a key.
	unreachable := NewOpenFGA("http://127.0.0.1:1", "tok")
	_, err := unreachable.createStore(ctx, "t/c")
	got = append(got, openfgatest.Case{Name: "unreachable", Result: errOf(err)})
	rec := openfgatest.New(t, openfgatest.OK(`{"id":"S1"}`))
	_, err = NewOpenFGA(rec.URL, "").createStore(ctx, "t/c")
	got = append(got, openfgatest.Case{Name: "no key", Requests: rec.Requests(), Result: errOf(err)})
	openfgatest.Compare(t, "arbiter-wire", got)
}

// A page without tuples is not the end of a read while it carries a
// continuation token: the tuples after it are read too.
func TestAReadFollowsTheTokenPastAnEmptyPage(t *testing.T) {
	rec := openfgatest.New(t,
		openfgatest.OK(`{"tuples":[],"continuation_token":"more"}`),
		openfgatest.OK(`{"tuples":[{"key":{"user":"user:a","relation":"member","object":"group:g"}}],"continuation_token":""}`),
	)
	got, err := NewOpenFGA(rec.URL, "tok").readAll(context.Background(), "S1", fgaTuple{Object: "group:g"})
	if err != nil {
		t.Fatal(err)
	}
	want := []Tuple{{User: "user:a", Relation: "member", Object: "group:g"}}
	if len(got) != 1 || got[0] != want[0] {
		t.Fatalf("read %+v, want %+v", got, want)
	}
	if reqs := rec.Requests(); len(reqs) != 2 {
		t.Fatalf("%d requests, want 2", len(reqs))
	}
}
