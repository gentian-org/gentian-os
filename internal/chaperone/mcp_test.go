/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package chaperone

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func asAdapterError(err error, target **AdapterError) bool { return errors.As(err, target) }

type seenRequest struct {
	method string
	params map[string]any
	auth   string
}

const listed = `{"tools":[
 {"name":"search-opportunities.v1","description":"Find opportunities","inputSchema":{"type":"object","properties":{"query":{"type":"string"}}},"_meta":{"gentianos.io/class":"read"}},
 {"name":"update-opportunity.v2","_meta":{"gentianos.io/class":"change"}},
 {"name":"send-mail.v1","_meta":{"gentianos.io/class":"irreversible"}},
 {"name":"odd.v1","_meta":{"gentianos.io/class":"harmless"}},
 {"name":"unclassed.v1"},
 {"name":"Not A Tool"}]}`

// adapter is an MCP server that answers tools/list with listed and
// tools/call with result.
func adapter(t *testing.T, seen *[]seenRequest, result string) *McpAdapters {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Method string         `json:"method"`
			Params map[string]any `json:"params"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if seen != nil {
			*seen = append(*seen, seenRequest{body.Method, body.Params, r.Header.Get("Authorization")})
		}
		answer := listed
		if body.Method == "tools/call" {
			answer = result
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":` + answer + `}`))
	}))
	t.Cleanup(srv.Close)
	return NewMcpAdapters([]Installed{{Name: "odoo", URL: srv.URL + "/mcp", App: "odoo-base-ce"}}, nil)
}

func TestTheSettingNamesEachAdaptersAddressAndApp(t *testing.T) {
	got, err := ParseInstalled(`{"odoo": {"url": "http://odoo-adapter:8765/mcp", "app": "odoo-base-ce"}}`)
	if err != nil || len(got) != 1 || got[0] != (Installed{"odoo", "http://odoo-adapter:8765/mcp", "odoo-base-ce"}) {
		t.Fatalf("got %v, %v", got, err)
	}
	if got, err := ParseInstalled("  "); err != nil || len(got) != 0 {
		t.Fatalf("empty: %v, %v", got, err)
	}
	for _, bad := range []string{`[]`, `{"odoo": {"url": "http://x"}}`, `{"odoo": {"app": "x"}}`, `{"odoo": "http://x"}`, `{`} {
		if _, err := ParseInstalled(bad); err == nil {
			t.Errorf("%s: accepted", bad)
		}
	}
}

func TestAToolIsWhatTheAdapterListsAndNoClassIsIrreversible(t *testing.T) {
	a := adapter(t, nil, "")
	classes := map[string]string{}
	for _, tool := range a.All(context.Background()) {
		classes[tool.Key()] = tool.Class
	}
	want := map[string]string{
		"odoo/search-opportunities@1": ClassRead,
		"odoo/update-opportunity@2":   ClassChange,
		"odoo/send-mail@1":            ClassIrreversible,
		"odoo/odd@1":                  ClassIrreversible,
		"odoo/unclassed@1":            ClassIrreversible,
	}
	if len(classes) != len(want) {
		t.Fatalf("tools %v", classes)
	}
	for key, class := range want {
		if classes[key] != class {
			t.Errorf("%s is %q, want %q", key, classes[key], class)
		}
	}
	found, ok := a.Lookup(context.Background(), "odoo/search-opportunities@1")
	if !ok || found.App != "odoo-base-ce" || found.Description != "Find opportunities" {
		t.Fatalf("found %+v", found)
	}
	if _, ok := found.InputSchema["properties"].(map[string]any)["query"]; !ok {
		t.Fatalf("schema %v", found.InputSchema)
	}
}

func TestTheListIsAskedWithoutACredentialAndKeptForAWhile(t *testing.T) {
	var seen []seenRequest
	a := adapter(t, &seen, "")
	if tool, _ := a.Lookup(context.Background(), "odoo/update-opportunity@2"); tool.Class != ClassChange {
		t.Fatalf("tool %+v", tool)
	}
	if _, ok := a.Lookup(context.Background(), "odoo/nothing@1"); ok {
		t.Fatal("found a tool nobody offers")
	}
	if len(seen) != 1 || seen[0].auth != "" {
		t.Fatalf("requests %v", seen)
	}
}

func TestAnAdapterThatDoesNotAnswerOffersNothing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(srv.Close)
	a := NewMcpAdapters([]Installed{{Name: "odoo", URL: srv.URL, App: "odoo-base-ce"}}, nil)
	if got := a.All(context.Background()); len(got) != 0 {
		t.Fatalf("tools %v", got)
	}
}

func TestACallCarriesTheCredentialAndTheAdaptersOwnNameForTheTool(t *testing.T) {
	var seen []seenRequest
	a := adapter(t, &seen, `{"content":[{"type":"text","text":"{\"id\":7}"}],"structuredContent":{"id":7}}`)
	tool, _ := a.Lookup(context.Background(), "odoo/update-opportunity@2")
	got, err := a.Call(context.Background(), tool, map[string]any{"id": 7, "name": "New"}, "odoo-token")
	if err != nil {
		t.Fatal(err)
	}
	if raw, _ := json.Marshal(got); string(raw) != `{"id":7}` {
		t.Fatalf("result %s", raw)
	}
	last := seen[len(seen)-1]
	arguments, _ := last.params["arguments"].(map[string]any)
	if last.method != "tools/call" || last.auth != "Bearer odoo-token" ||
		last.params["name"] != "update-opportunity.v2" || arguments["name"] != "New" {
		t.Fatalf("request %+v", last)
	}
}

func TestAnAnswerWithoutStructureIsItsContent(t *testing.T) {
	a := adapter(t, nil, `{"content":[{"type":"text","text":"done"}]}`)
	tool, _ := a.Lookup(context.Background(), "odoo/update-opportunity@2")
	got, err := a.Call(context.Background(), tool, nil, "odoo-token")
	if err != nil {
		t.Fatal(err)
	}
	if raw, _ := json.Marshal(got); string(raw) != `[{"type":"text","text":"done"}]` {
		t.Fatalf("result %s", raw)
	}
}

func TestACallWithoutACredentialIsNeverMade(t *testing.T) {
	var seen []seenRequest
	a := adapter(t, &seen, `{}`)
	tool, _ := a.Lookup(context.Background(), "odoo/search-opportunities@1")
	var adapterErr *AdapterError
	if _, err := a.Call(context.Background(), tool, nil, ""); !asAdapterError(err, &adapterErr) {
		t.Fatalf("error %v", err)
	}
	if len(seen) != 1 || seen[0].method != "tools/list" {
		t.Fatalf("requests %v", seen)
	}
}

func TestAToolThatFailedIsAnErrorWithTheAdaptersWords(t *testing.T) {
	a := adapter(t, nil, `{"isError":true,"content":[{"type":"text","text":"Odoo refused:"},{"type":"image"},{"type":"text","text":"access denied"}]}`)
	tool, _ := a.Lookup(context.Background(), "odoo/search-opportunities@1")
	_, err := a.Call(context.Background(), tool, nil, "odoo-token")
	var adapterErr *AdapterError
	if !asAdapterError(err, &adapterErr) || adapterErr.Message != "Odoo refused: access denied" {
		t.Fatalf("error %v", err)
	}
	failed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"error":{"code":-32602,"message":"unknown tool"}}`))
	}))
	t.Cleanup(failed.Close)
	b := NewMcpAdapters([]Installed{{Name: "odoo", URL: failed.URL, App: "odoo-base-ce"}}, nil)
	_, err = b.Call(context.Background(), Tool{Adapter: "odoo", Name: "x", Version: 1}, nil, "odoo-token")
	if !asAdapterError(err, &adapterErr) || adapterErr.Message != "unknown tool" {
		t.Fatalf("error %v", err)
	}
}

func TestAnAnswerSentAsAStreamIsReadToo(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		one := strings.ReplaceAll(`{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"search-opportunities.v1"}]}}`, "\n", "")
		_, _ = w.Write([]byte("event: message\ndata: " + one + "\n\n"))
	}))
	t.Cleanup(srv.Close)
	a := NewMcpAdapters([]Installed{{Name: "odoo", URL: srv.URL, App: "odoo-base-ce"}}, nil)
	got := a.All(context.Background())
	if len(got) != 1 || got[0].Key() != "odoo/search-opportunities@1" {
		t.Fatalf("tools %v", got)
	}
}
