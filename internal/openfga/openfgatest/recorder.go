/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

// Package openfgatest is a recording OpenFGA for tests: it answers from a
// script and keeps every request it was sent, so a test can hold a client to
// the requests it makes on the wire.
package openfgatest

import (
	"bytes"
	"encoding/json"
	"flag"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
)

// update rewrites the transcripts a test compares against.
var update = flag.Bool("update-wire", false, "rewrite the recorded OpenFGA wire transcripts")

// Reply is one scripted answer.
type Reply struct {
	Status int
	Body   string
}

// OK is a 200 with body.
func OK(body string) Reply { return Reply{Status: http.StatusOK, Body: body} }

// Request is one request as it arrived.
type Request struct {
	Method        string `json:"method"`
	URI           string `json:"uri"`
	Authorization string `json:"authorization,omitempty"`
	ContentType   string `json:"contentType,omitempty"`
	ContentLength int64  `json:"contentLength"`
	Body          string `json:"body,omitempty"`
}

// Recorder answers each request with the next scripted reply, and a 599 once
// the script is spent.
type Recorder struct {
	URL string

	mu       sync.Mutex
	replies  []Reply
	requests []Request
}

// New starts a recorder that answers with replies, in order.
func New(t testing.TB, replies ...Reply) *Recorder {
	t.Helper()
	r := &Recorder{replies: replies}
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	r.URL = srv.URL
	return r
}

func (r *Recorder) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	body, _ := io.ReadAll(req.Body)
	r.mu.Lock()
	r.requests = append(r.requests, Request{
		Method:        req.Method,
		URI:           req.RequestURI,
		Authorization: req.Header.Get("Authorization"),
		ContentType:   req.Header.Get("Content-Type"),
		ContentLength: req.ContentLength,
		Body:          string(body),
	})
	reply := Reply{Status: 599, Body: "no reply scripted"}
	if len(r.replies) > 0 {
		reply, r.replies = r.replies[0], r.replies[1:]
	}
	r.mu.Unlock()
	w.WriteHeader(reply.Status)
	_, _ = io.WriteString(w, reply.Body)
}

// Requests is every request received so far.
func (r *Recorder) Requests() []Request {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Request(nil), r.requests...)
}

// Case is one operation: the requests it sent and what it returned.
type Case struct {
	Name     string    `json:"name"`
	Requests []Request `json:"requests"`
	Result   any       `json:"result"`
}

// Compare holds cases to the transcript in testdata/name.json, or writes it
// with -update-wire. Bodies are compared as JSON values: the order of an
// object's keys is not part of a request's meaning, a key that is absent and
// one that is empty are.
func Compare(t *testing.T, name string, cases []Case) {
	t.Helper()
	path := filepath.Join("testdata", name+".json")
	got, err := json.MarshalIndent(cases, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	// Round-trip so that results compare as JSON, whatever Go type made them.
	var gotCases []Case
	if err := json.Unmarshal(got, &gotCases); err != nil {
		t.Fatal(err)
	}
	if *update {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, append(got, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run with -update-wire to record)", err)
	}
	var want []Case
	if err := json.Unmarshal(raw, &want); err != nil {
		t.Fatal(err)
	}
	if len(want) != len(gotCases) {
		t.Fatalf("%d cases, the transcript has %d", len(gotCases), len(want))
	}
	for i := range want {
		w, g := want[i], gotCases[i]
		if w.Name != g.Name {
			t.Fatalf("case %d is %q, the transcript has %q", i, g.Name, w.Name)
		}
		if len(w.Requests) != len(g.Requests) {
			t.Errorf("%s: %d requests, the transcript has %d:\n got %+v\nwant %+v", g.Name, len(g.Requests), len(w.Requests), g.Requests, w.Requests)
			continue
		}
		for j := range w.Requests {
			wr, gr := w.Requests[j], g.Requests[j]
			wb, gb := canonical(t, wr.Body), canonical(t, gr.Body)
			wr.Body, gr.Body = "", ""
			if wr != gr || !bytes.Equal(wb, gb) {
				t.Errorf("%s: request %d differs:\n got %+v %s\nwant %+v %s", g.Name, j, gr, gb, wr, wb)
			}
		}
		if !reflect.DeepEqual(w.Result, g.Result) {
			t.Errorf("%s: result differs:\n got %#v\nwant %#v", g.Name, g.Result, w.Result)
		}
	}
}

// canonical is a body re-encoded with its keys sorted; a body that is not
// JSON is kept as it is.
func canonical(t *testing.T, body string) []byte {
	t.Helper()
	if body == "" {
		return nil
	}
	var v any
	if err := json.Unmarshal([]byte(body), &v); err != nil {
		return []byte(body)
	}
	out, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return out
}
