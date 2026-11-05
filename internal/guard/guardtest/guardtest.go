/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

// Package guardtest records what a service's guard does with a request: the
// answer it gives, the questions it puts to the store, what it logs and what
// it hands the route, so that a test can hold all of it to a transcript.
package guardtest

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/gentian-org/gentian-os/internal/director/authn"
)

var update = flag.Bool("update-guard", false, "rewrite the recorded guard transcripts")

// Authn answers FromRequest from the bearer: each token names an identity.
// A request without a bearer, or with a token not listed, is refused the way
// the verifier refuses one.
type Authn map[string]*authn.Identity

// FromRequest implements the services' authenticator.
func (a Authn) FromRequest(r *http.Request) (*authn.Identity, error) {
	h := r.Header.Get("Authorization")
	token, ok := strings.CutPrefix(h, "Bearer ")
	if !ok {
		return nil, fmt.Errorf("%w: no bearer token", authn.ErrUnauthenticated)
	}
	id, ok := a[token]
	if !ok {
		return nil, fmt.Errorf("%w: token not recognized", authn.ErrUnauthenticated)
	}
	cp := *id
	return &cp, nil
}

// Checks is a store that answers from Held and records every question, or
// fails every question with Err.
type Checks struct {
	Held map[string]bool
	Err  error

	mu    sync.Mutex
	calls []string
}

// Check implements authz.Checker.
func (c *Checks) Check(_ context.Context, requestID, user, relation, object string) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, fmt.Sprintf("request_id=%q %s %s %s", requestID, user, relation, object))
	if c.Err != nil {
		return false, c.Err
	}
	return c.Held[user+" "+relation+" "+object], nil
}

// Take returns the questions asked since the last Take.
func (c *Checks) Take() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := c.calls
	c.calls = nil
	return out
}

// Logs is a slog handler that keeps every record, without its time.
type Logs struct {
	mu      sync.Mutex
	records []string
	attrs   []slog.Attr
}

// Logger is a logger writing to l.
func (l *Logs) Logger() *slog.Logger { return slog.New(l) }

func (l *Logs) Enabled(context.Context, slog.Level) bool { return true }

func (l *Logs) Handle(_ context.Context, r slog.Record) error {
	parts := []string{r.Level.String(), fmt.Sprintf("%q", r.Message)}
	for _, a := range l.attrs {
		parts = append(parts, a.String())
	}
	r.Attrs(func(a slog.Attr) bool {
		parts = append(parts, a.Key+"="+fmt.Sprintf("%q", a.Value.String()))
		return true
	})
	l.mu.Lock()
	defer l.mu.Unlock()
	l.records = append(l.records, strings.Join(parts, " "))
	return nil
}

func (l *Logs) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &Logs{attrs: append(append([]slog.Attr(nil), l.attrs...), attrs...)}
}

func (l *Logs) WithGroup(string) slog.Handler { return l }

// Take returns the records since the last Take.
func (l *Logs) Take() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := l.records
	l.records = nil
	return out
}

// Request is one request to put to a handler.
type Request struct {
	Method, Path string
	// Token is sent as a bearer when set; Authorization, when set, is sent
	// as it is instead.
	Token, Authorization string
	// RequestID is sent as X-Request-Id when set.
	RequestID string
	// Reason is sent as X-Gentian-Reason when set.
	Reason string
	Body   string
}

// Exchange is a request and what came of it.
type Exchange struct {
	Request string            `json:"request"`
	Status  int               `json:"status"`
	Header  map[string]string `json:"header"`
	Body    string            `json:"body"`
	Checks  []string          `json:"checks"`
	Logs    []string          `json:"logs"`
	// Handled is what the route was handed, or empty when it was not
	// reached.
	Handled string `json:"handled"`
}

// minted is what a request id the service made itself looks like.
var minted = regexp.MustCompile(`^[0-9a-f]{32}$`)

// Do sends req to h and records the exchange. handled is read after the
// request: what the route recorded of the call it was handed. An id the
// service minted is replaced by "<minted>" wherever it appears, once it is
// seen to have the form of one.
func Do(t *testing.T, h http.Handler, req Request, checks *Checks, logs *Logs, handled *string) Exchange {
	t.Helper()
	*handled = ""
	r := httptest.NewRequest(req.Method, req.Path, strings.NewReader(req.Body))
	switch {
	case req.Authorization != "":
		r.Header.Set("Authorization", req.Authorization)
	case req.Token != "":
		r.Header.Set("Authorization", "Bearer "+req.Token)
	}
	if req.RequestID != "" {
		r.Header.Set("X-Request-Id", req.RequestID)
	}
	if req.Reason != "" {
		r.Header.Set("X-Gentian-Reason", req.Reason)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	x := Exchange{
		Request: fmt.Sprintf("%s %s auth=%q id=%q", req.Method, req.Path, r.Header.Get("Authorization"), req.RequestID),
		Status:  w.Code,
		Header:  map[string]string{},
		Body:    w.Body.String(),
		Checks:  checks.Take(),
		Logs:    logs.Take(),
		Handled: *handled,
	}
	for k := range w.Header() {
		x.Header[k] = strings.Join(w.Header().Values(k), ", ")
	}
	if id := w.Header().Get("X-Request-Id"); id != req.RequestID {
		if !minted.MatchString(id) {
			t.Errorf("%s: minted request id %q is not 32 hex digits", x.Request, id)
		}
		scrub := func(s string) string { return strings.ReplaceAll(s, id, "<minted>") }
		x.Header["X-Request-Id"] = scrub(x.Header["X-Request-Id"])
		x.Body, x.Handled = scrub(x.Body), scrub(x.Handled)
		for i := range x.Checks {
			x.Checks[i] = scrub(x.Checks[i])
		}
		for i := range x.Logs {
			x.Logs[i] = scrub(x.Logs[i])
		}
	}
	return x
}

// Case is a named exchange.
type Case struct {
	Name     string   `json:"name"`
	Exchange Exchange `json:"exchange"`
}

// Golden holds cases to testdata/name.json, or writes it with -update-guard.
func Golden(t *testing.T, name string, cases []Case) {
	t.Helper()
	path := filepath.Join("testdata", name+".json")
	got, err := json.MarshalIndent(cases, "", "  ")
	if err != nil {
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
		t.Fatalf("%v (run with -update-guard to record)", err)
	}
	var want, have []Case
	if err := json.Unmarshal(raw, &want); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(got, &have); err != nil {
		t.Fatal(err)
	}
	if len(want) != len(have) {
		t.Fatalf("%d cases, the transcript has %d", len(have), len(want))
	}
	for i := range want {
		if !reflect.DeepEqual(want[i], have[i]) {
			w, _ := json.MarshalIndent(want[i], "", "  ")
			g, _ := json.MarshalIndent(have[i], "", "  ")
			t.Errorf("case %d differs:\n got %s\nwant %s", i, g, w)
		}
	}
}
