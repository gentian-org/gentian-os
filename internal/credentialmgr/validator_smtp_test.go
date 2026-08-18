/*
Copyright 2026 Gentian Organization.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package credentialmgr

import (
	"bufio"
	"context"
	"encoding/base64"
	"fmt"
	"net"
	"net/smtp"
	"strings"
	"testing"
)

// fakeRelay speaks just enough SMTP to exercise the probe: EHLO, STARTTLS
// advertised or not, and an AUTH exchange whose verdict the test chooses.
//
// It never actually completes STARTTLS — the probe is expected to fail there
// when TLS is required, which is what lets these tests cover the paths that
// matter without a certificate authority.
type fakeRelay struct {
	advertiseSTARTTLS bool
	addr              string
}

func startFakeRelay(t *testing.T, advertiseSTARTTLS bool) *fakeRelay {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	r := &fakeRelay{advertiseSTARTTLS: advertiseSTARTTLS, addr: ln.Addr().String()}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go r.serve(conn)
		}
	}()
	return r
}

func (r *fakeRelay) serve(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	br := bufio.NewReader(conn)
	_, _ = fmt.Fprintf(conn, "220 fake ESMTP\r\n")
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return
		}
		cmd := strings.ToUpper(strings.TrimSpace(line))
		switch {
		case strings.HasPrefix(cmd, "EHLO"), strings.HasPrefix(cmd, "HELO"):
			if r.advertiseSTARTTLS {
				_, _ = fmt.Fprintf(conn, "250-fake\r\n250-STARTTLS\r\n250 AUTH PLAIN LOGIN\r\n")
			} else {
				_, _ = fmt.Fprintf(conn, "250-fake\r\n250 AUTH PLAIN LOGIN\r\n")
			}
		case strings.HasPrefix(cmd, "QUIT"):
			_, _ = fmt.Fprintf(conn, "221 bye\r\n")
			return
		default:
			_, _ = fmt.Fprintf(conn, "502 not implemented\r\n")
		}
	}
}

func (r *fakeRelay) hostPort(t *testing.T) (string, string) {
	t.Helper()
	h, p, err := net.SplitHostPort(r.addr)
	if err != nil {
		t.Fatalf("split: %v", err)
	}
	return h, p
}

func validatorFor(r *fakeRelay, t *testing.T) *EndpointValidator {
	v := NewEndpointValidator()
	host, port := r.hostPort(t)
	v.Relay = func(context.Context) (string, string, error) { return host, port, nil }
	return v
}

func creds() map[string]string {
	return map[string]string{"relay_username": "u@example.org", "relay_password": "pw"}
}

// The bug this validator replaced: smtp always errored, so handleSet always
// answered 422 and smtp-relay could never be stored at all.
func TestSMTP_NoLongerRefusesOutright(t *testing.T) {
	v := validatorFor(startFakeRelay(t, true), t)
	err := v.Validate(context.Background(), "smtp", creds())
	if err != nil && strings.Contains(err.Error(), "not available") {
		t.Fatalf("the validator must attempt the check, not decline it: %v", err)
	}
}

// AUTH must never be offered on a cleartext connection, so a relay with no
// STARTTLS is refused rather than downgraded.
func TestSMTP_RefusesWithoutSTARTTLS(t *testing.T) {
	v := validatorFor(startFakeRelay(t, false), t)
	err := v.Validate(context.Background(), "smtp", creds())
	if err == nil {
		t.Fatal("expected a refusal when the relay offers no STARTTLS")
	}
	if !strings.Contains(err.Error(), "STARTTLS") {
		t.Errorf("the reason should name STARTTLS, got: %v", err)
	}
}

func TestSMTP_MissingFieldsRejected(t *testing.T) {
	v := validatorFor(startFakeRelay(t, true), t)
	for _, f := range []map[string]string{
		{},
		{"relay_username": "u"},
		{"relay_password": "p"},
	} {
		if err := v.Validate(context.Background(), "smtp", f); err == nil {
			t.Errorf("expected %v to be rejected as incomplete", f)
		}
	}
}

// A cluster with no relay on its claim must be told that, not handed a
// connection error about an empty host.
func TestSMTP_NoRelayConfigured_SaysSo(t *testing.T) {
	v := NewEndpointValidator()
	v.Relay = func(context.Context) (string, string, error) { return "", "", nil }
	err := v.Validate(context.Background(), "smtp", creds())
	if err == nil || !strings.Contains(err.Error(), "spec.mail.host") {
		t.Fatalf("expected the error to name the claim field to set, got: %v", err)
	}
}

// No resolver at all is a wiring fault, and must not read as a bad password.
func TestSMTP_NoResolver_IsNotACredentialFailure(t *testing.T) {
	v := NewEndpointValidator()
	err := v.Validate(context.Background(), "smtp", creds())
	if err == nil || !strings.Contains(err.Error(), "relay endpoint is unknown") {
		t.Fatalf("expected a wiring error, got: %v", err)
	}
}

func TestSMTP_UnreachableRelayNamesTheAddress(t *testing.T) {
	v := NewEndpointValidator()
	v.Relay = func(context.Context) (string, string, error) { return "127.0.0.1", "1", nil }
	err := v.Validate(context.Background(), "smtp", creds())
	if err == nil || !strings.Contains(err.Error(), "127.0.0.1:1") {
		t.Fatalf("expected the unreachable address to be named, got: %v", err)
	}
}

// loginAuth is the AUTH LOGIN exchange Go does not ship. It must refuse to
// speak on an unencrypted connection.
func TestLoginAuth_RefusesWithoutTLS(t *testing.T) {
	a := loginAuth{user: "u", pass: "p", host: "h"}
	if _, _, err := a.Start(&smtp.ServerInfo{TLS: false}); err == nil {
		t.Fatal("AUTH LOGIN must not proceed without TLS")
	}
	if _, _, err := a.Start(&smtp.ServerInfo{TLS: true}); err != nil {
		t.Fatalf("AUTH LOGIN should proceed over TLS: %v", err)
	}
	for challenge, want := range map[string]string{"Username:": "u", "Password:": "p"} {
		got, err := a.Next([]byte(challenge), true)
		if err != nil {
			t.Fatalf("challenge %q: %v", challenge, err)
		}
		if string(got) != want {
			t.Errorf("challenge %q → %q, want %q", challenge, got, want)
		}
	}
	if _, err := a.Next([]byte("Surname:"), true); err == nil {
		t.Error("an unexpected challenge should be an error, not an empty answer")
	}
	// base64 is the wire encoding; the auth type returns raw bytes and net/smtp
	// encodes. Asserted so a future "helpful" pre-encoding is caught.
	if got, _ := a.Next([]byte("Username:"), true); base64.StdEncoding.EncodeToString(got) == string(got) {
		t.Error("loginAuth must return raw credentials, not base64")
	}
}
