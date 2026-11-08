/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package lifecycle

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The operator answers 401 only to a token it does not accept. That is a fault
// between the director and the operator, and it must not reach a browser as a
// 401, which a console reads as an expired session and answers by signing in
// again -- in a loop, several times a second.
func TestAnOperatorRefusingTheTokenIsABadGatewayNotAnExpiredSession(t *testing.T) {
	op := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"detail":"this API is the director's; present its token"}`))
	}))
	defer op.Close()
	c := New(op.URL, func() string { return "wrong" })

	if status, body, err := c.Get(context.Background(), "/v1/tenants/demo/backups", nil); err != nil || status != http.StatusBadGateway || !strings.Contains(string(body), "refused the director's token") {
		t.Fatalf("Get = %d %s %v", status, body, err)
	}
	if status, _, err := c.Do(context.Background(), "/v1/tenants/demo/actions/backup", "ada", map[string]string{}); err != nil || status != http.StatusBadGateway {
		t.Fatalf("Do = %d %v", status, err)
	}
	if status, _, err := c.Upload(context.Background(), "/v1/bundles", "application/x-tar", strings.NewReader("x")); err != nil || status != http.StatusBadGateway {
		t.Fatalf("Upload = %d %v", status, err)
	}
	resp, err := c.Stream(context.Background(), "/v1/tenants/demo/backups/x/download")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusBadGateway || resp.Header.Get("Content-Disposition") != "" || !strings.Contains(string(body), "refused") {
		t.Fatalf("Stream = %d %q %s", resp.StatusCode, resp.Header.Get("Content-Disposition"), body)
	}
}

// A projected ServiceAccount token is replaced in its file every few minutes.
// The client presents what the file holds at the time of each request, and
// presents nothing when there is no file.
func TestTheTokenIsReadFromItsFileOnEveryRequest(t *testing.T) {
	var presented []string
	op := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		presented = append(presented, r.Header.Get("Authorization"))
		_, _ = w.Write([]byte(`{}`))
	}))
	defer op.Close()
	file := filepath.Join(t.TempDir(), "token")
	c := New(op.URL, TokenFile(file))

	ask := func() {
		t.Helper()
		if _, _, err := c.Get(context.Background(), "/v1/tenants/demo/backups", nil); err != nil {
			t.Fatal(err)
		}
	}
	ask()
	if err := os.WriteFile(file, []byte("first\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ask()
	if err := os.WriteFile(file, []byte("second\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ask()
	want := []string{"", "Bearer first", "Bearer second"}
	if len(presented) != len(want) || presented[0] != want[0] || presented[1] != want[1] || presented[2] != want[2] {
		t.Fatalf("presented %q, want %q", presented, want)
	}
}

// A purge is answered when it is over, minutes after it was asked for. The
// ordinary timeout of this client would cut it off while the operator was
// still destroying things, so that one action is relayed under a deadline of
// its own -- and every other request keeps the short one.
func TestAPatientRequestOutlastsTheOrdinaryTimeout(t *testing.T) {
	op := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(150 * time.Millisecond)
		_, _ = w.Write([]byte(`{"status":"purged"}`))
	}))
	defer op.Close()
	c := New(op.URL, func() string { return "t" })
	c.http.Timeout = 30 * time.Millisecond

	if _, _, err := c.Do(context.Background(), "/v1/tenants/demo/actions/provision-app", "ada", map[string]string{}); err == nil {
		t.Fatal("an ordinary request outlasted the ordinary timeout")
	}
	ctx, cancel := Patient(context.Background(), 5*time.Second)
	defer cancel()
	status, body, err := c.Do(ctx, "/v1/tenants/demo/actions/purge-app", "ada", map[string]string{"profile": "wiki"})
	if err != nil || status != http.StatusOK || !strings.Contains(string(body), "purged") {
		t.Fatalf("a patient request: %d %s %v", status, body, err)
	}
	// Patient is still bounded: by the deadline it was given.
	short, cancelShort := Patient(context.Background(), 30*time.Millisecond)
	defer cancelShort()
	if _, _, err := c.Do(short, "/v1/tenants/demo/actions/purge-app", "ada", map[string]string{}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want the deadline", err)
	}
}

// Telling the operator of a push is one request that names nothing but who
// pushed: the operator decides what reads the repository.
func TestTellingTheOperatorOfAPushNamesNothingButTheActor(t *testing.T) {
	type seen struct{ method, path, actor, auth, body string }
	got := make(chan seen, 4)
	status := http.StatusOK
	op := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		got <- seen{r.Method, r.URL.Path, r.Header.Get("X-Gentian-Actor"), r.Header.Get("Authorization"), string(body)}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"detail":"no"}`))
	}))
	defer op.Close()
	c := New(op.URL, func() string { return "sa-token" })

	if err := c.RefreshDeployments(context.Background(), "ada@example.com"); err != nil {
		t.Fatal(err)
	}
	want := seen{http.MethodPost, RefreshDeploymentsPath, "ada@example.com", "Bearer sa-token", "{}"}
	if s := <-got; s != want {
		t.Fatalf("the operator was sent %+v, want %+v", s, want)
	}

	// Done shortly is as good as done; anything else is said.
	status = http.StatusAccepted
	if err := c.RefreshDeployments(context.Background(), "ada"); err != nil {
		t.Fatalf("a deferred refresh: %v", err)
	}
	<-got
	status = http.StatusServiceUnavailable
	var upstream *UpstreamError
	if err := c.RefreshDeployments(context.Background(), "ada"); !errors.As(err, &upstream) || upstream.Status != http.StatusServiceUnavailable {
		t.Fatalf("a refusing operator: %v", err)
	}
	<-got

	// After a push: once, beside the caller, and a failure goes to whoever
	// was named for it and nowhere else.
	failed, done := make(chan error, 1), make(chan error, 1)
	c.AfterPush(func(err error) { failed <- err }, func(err error) { done <- err })("ada")
	if err := <-done; err == nil {
		t.Fatal("a refusal was reported as told")
	}
	if err := <-failed; !errors.As(err, &upstream) {
		t.Fatalf("failed was given %v", err)
	}
	<-got
	select {
	case s := <-got:
		t.Fatalf("one push asked the operator twice: %+v", s)
	case <-time.After(100 * time.Millisecond):
	}
}
