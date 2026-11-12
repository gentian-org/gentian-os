/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package main

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// callback is the component a run calls back to.
type callback struct {
	inputs   string
	outputs  string
	exitCode string
	attempt  string
	tokens   []string
	refuse   int
}

func (c *callback) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c.tokens = append(c.tokens, r.Header.Get("Authorization"))
	if c.refuse != 0 {
		w.WriteHeader(c.refuse)
		return
	}
	switch r.Method + " " + r.URL.Path {
	case "GET /cb/runs/r1/inputs":
		_, _ = io.WriteString(w, c.inputs)
	case "POST /cb/runs/r1/outputs":
		raw, _ := io.ReadAll(r.Body)
		c.outputs, c.exitCode, c.attempt = string(raw), r.Header.Get("X-Run-Exit-Code"), r.Header.Get("X-Run-Attempt")
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func environment(url string) func(string) string {
	return func(name string) string {
		return map[string]string{"RUN_CALLBACK_URL": url + "/cb/", "RUN_TOKEN": "secret", "RUN_ID": "r1", "RUN_ATTEMPT": "1"}[name]
	}
}

// The command reads the run's inputs on standard input and what it writes
// to standard output is delivered as the outputs, with how it ended. Its
// exit code is the pod's.
func TestTheCommandIsGivenTheInputsAndItsOutputsAreDelivered(t *testing.T) {
	cb := &callback{inputs: `{"n": 2}`}
	srv := httptest.NewServer(cb)
	defer srv.Close()
	var stderr bytes.Buffer

	code := run([]string{"--", "sh", "-c", `tr a-z A-Z; echo note >&2`}, environment(srv.URL), &stderr)
	if code != 0 || cb.outputs != `{"N": 2}` || cb.exitCode != "0" || cb.attempt != "1" {
		t.Fatalf("code %d, outputs %q, exit %q, attempt %q, stderr %s", code, cb.outputs, cb.exitCode, cb.attempt, stderr.String())
	}
	if !strings.Contains(stderr.String(), "note") {
		t.Fatalf("the command's own log is lost: %q", stderr.String())
	}
	for _, token := range cb.tokens {
		if token != "Bearer secret" {
			t.Fatalf("a call without the run's token: %q", token)
		}
	}

	code = run([]string{"--", "sh", "-c", `echo partial; exit 3`}, environment(srv.URL), &stderr)
	if code != 3 || cb.exitCode != "3" || cb.outputs != "partial\n" {
		t.Fatalf("a failing command: code %d, exit %q, outputs %q", code, cb.exitCode, cb.outputs)
	}
}

// A component that refuses the run's token, a command that is not there and
// an environment without the callback each end the run with an error of the
// program's own, and nothing is run on inputs that were never fetched.
func TestWhatEndsARunBeforeItsCommand(t *testing.T) {
	cb := &callback{refuse: http.StatusForbidden}
	srv := httptest.NewServer(cb)
	defer srv.Close()
	var stderr bytes.Buffer
	marker := filepath.Join(t.TempDir(), "ran")
	if code := run([]string{"--", "sh", "-c", "touch " + marker}, environment(srv.URL), &stderr); code != 70 {
		t.Fatalf("code = %d", code)
	}
	if _, err := os.Stat(marker); err == nil || len(cb.tokens) != 1 {
		t.Fatalf("the command ran, or a refusal was asked again: %d calls", len(cb.tokens))
	}
	cb.refuse = 0
	if code := run([]string{"--", "/no/such/command"}, environment(srv.URL), &stderr); code != 70 {
		t.Fatalf("a command that is not there: %d", code)
	}
	if code := run([]string{"--", "true"}, func(string) string { return "" }, &stderr); code != 70 {
		t.Fatalf("no environment: %d", code)
	}
	if code := run([]string{"true"}, environment(srv.URL), &stderr); code != 70 {
		t.Fatalf("no separator: %d", code)
	}
}

// --install leaves the program where the run's container finds it.
func TestInstallCopiesTheProgram(t *testing.T) {
	target := filepath.Join(t.TempDir(), "run")
	if code := run([]string{"--install", target}, os.Getenv, io.Discard); code != 0 {
		t.Fatalf("code = %d", code)
	}
	info, err := os.Stat(target)
	if err != nil || info.Size() == 0 || info.Mode().Perm()&0o111 == 0 {
		t.Fatalf("installed: %v %v", info, err)
	}
}
