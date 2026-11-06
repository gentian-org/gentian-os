/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package custodian

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

var updateLogin = flag.Bool("update-login", false, "rewrite testdata/login.txt")

// TestLoginRecord records how the custodian logs in to the vault: how the
// client verifies the vault, its timeout, and what each kind of login answer
// becomes. testdata/login.txt is the record; -update-login rewrites it.
func TestLoginRecord(t *testing.T) {
	var lines []string
	add := func(format string, args ...any) { lines = append(lines, fmt.Sprintf(format, args...)) }

	ok := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"auth":{"client_token":"s.tok","lease_duration":3600}}`))
	}))
	defer ok.Close()
	refused := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"errors":["role not found"]}`))
	}))
	defer refused.Close()
	empty := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"auth":{}}`))
	}))
	defer empty.Close()

	for _, c := range []struct {
		name string
		srv  *httptest.Server
		ca   []byte
		skip bool
	}{
		{"no CA, self-signed", ok, nil, false},
		{"its CA", ok, serverCAPEM(t, ok), false},
		{"CA not PEM", ok, []byte("not a certificate"), false},
		{"CA empty", ok, []byte{}, false},
		{"skip verify", ok, nil, true},
		{"login refused", refused, serverCAPEM(t, refused), false},
		{"no token in the answer", empty, serverCAPEM(t, empty), false},
	} {
		b := asCustodian(t, NewOpenBao(c.srv.URL, "secret", "gentian-os-custodian", c.ca, c.skip))
		tok, err := b.Token(context.Background())
		add("%s: %s; token=%q %s", c.name, describeClient(b.HTTP), tok, describeLoginErr(err))
	}

	unreachable := asCustodian(t, NewOpenBao("https://127.0.0.1:1", "secret", "gentian-os-custodian", nil, false))
	_, err := unreachable.Token(context.Background())
	add("unreachable: %s", describeLoginErr(err))

	noJWT := NewOpenBao(ok.URL, "secret", "gentian-os-custodian", serverCAPEM(t, ok), false)
	noJWT.ServiceAccountTokenPath = filepath.Join(t.TempDir(), "absent")
	_, err = noJWT.Token(context.Background())
	add("no ServiceAccount token: %s", describeLoginErr(err))

	static := NewOpenBao(ok.URL, "secret", "gentian-os-custodian", nil, false)
	static.SetStaticToken("static")
	tok, err := static.Token(context.Background())
	add("static token: token=%q err=%v forgotten=%v", tok, err, static.forget("static"))

	got := strings.Join(lines, "\n") + "\n"
	path := filepath.Join("testdata", "login.txt")
	if *updateLogin {
		if err := os.WriteFile(path, []byte(got), 0o600); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run with -update-login to record it)", err)
	}
	if got != string(want) {
		t.Errorf("the login record changed:\n--- got\n%s--- want\n%s", got, want)
	}
}

func describeClient(c *http.Client) string {
	roots := "default transport"
	if tr, ok := c.Transport.(*http.Transport); ok && tr != nil {
		roots = "system roots"
		if conf := tr.TLSClientConfig; conf != nil {
			switch {
			case conf.InsecureSkipVerify:
				roots = "no verification"
			case conf.RootCAs != nil:
				//nolint:staticcheck // Subjects is enough to count a pool built from PEM.
				roots = fmt.Sprintf("configured roots (%d)", len(conf.RootCAs.Subjects()))
			}
			if conf.MinVersion != tls.VersionTLS12 {
				roots += fmt.Sprintf(", min TLS %#x", conf.MinVersion)
			}
		}
	}
	return fmt.Sprintf("timeout=%s verify=%s", c.Timeout.Round(time.Second), roots)
}

var vaultURL = regexp.MustCompile(`https://127\.0\.0\.1:\d+`)

func describeLoginErr(err error) string {
	if err == nil {
		return "ok"
	}
	msg := vaultURL.ReplaceAllString(err.Error(), "https://<vault>")
	if i := strings.Index(msg, "tls: "); i >= 0 {
		msg = msg[:i] + "<tls failure>"
	}
	if i := strings.Index(msg, "dial tcp"); i >= 0 {
		msg = msg[:i] + "<dial failure>"
	}
	if i := strings.Index(msg, "open "); i >= 0 {
		msg = msg[:i] + "<no such file>"
	}
	return fmt.Sprintf("upstream=%v login=%v err=%q", errors.Is(err, ErrUpstream), errors.Is(err, ErrVaultLogin), msg)
}
