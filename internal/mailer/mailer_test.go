/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package mailer

import (
	"bufio"
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"
)

func data(kv ...string) map[string][]byte {
	out := map[string][]byte{}
	for i := 0; i+1 < len(kv); i += 2 {
		out[kv[i]] = []byte(kv[i+1])
	}
	return out
}

func TestSettingsAreReadAsThePlatformWritesThem(t *testing.T) {
	s, err := SettingsFromSecret(data("smtp_configure", "true", "smtp_host", "mail.example.org", "smtp_user", "gentian-system@example.org", "smtp_password", "pw", "smtp_starttls", "true"), "example.org")
	if err != nil {
		t.Fatal(err)
	}
	if s.Port != "587" || s.From != "noreply@example.org" || !s.StartTLS || s.SSL {
		t.Fatalf("settings = %+v", s)
	}
	s, err = SettingsFromSecret(data("smtp_configure", "true", "smtp_host", "relay", "smtp_port", "465", "smtp_user", "u", "smtp_password", "p", "smtp_from", "ops@corp.example", "smtp_ssl", "true"), "")
	if err != nil || !s.SSL || s.StartTLS || s.From != "ops@corp.example" {
		t.Fatalf("relay settings = %+v %v", s, err)
	}
	for _, bad := range []map[string][]byte{
		data("smtp_host", "mail", "smtp_user", "u", "smtp_password", "p"),
		data("smtp_configure", "true", "smtp_host", "", "smtp_user", "u", "smtp_password", "p"),
		data("smtp_configure", "true", "smtp_host", "mail", "smtp_user", "u"),
	} {
		if _, err := SettingsFromSecret(bad, "example.org"); !errors.Is(err, ErrNotConfigured) {
			t.Errorf("SettingsFromSecret(%v) = %v", bad, err)
		}
	}
}

func TestTheMessageIsRFC5322(t *testing.T) {
	at := time.Date(2026, 11, 13, 8, 0, 0, 0, time.UTC)
	body, err := Render("noreply@example.org", Message{To: []string{"a@example.org", "b@example.org"}, Subject: "Release v0.5.1 ist da", Text: "Line one\nLine two\n"}, at)
	if err != nil {
		t.Fatal(err)
	}
	s := string(body)
	for _, want := range []string{
		"From: noreply@example.org\r\n",
		"To: a@example.org, b@example.org\r\n",
		"Subject: Release v0.5.1 ist da\r\n",
		"Date: Fri, 13 Nov 2026 08:00:00 +0000\r\n",
		"@example.org>\r\n",
		"Content-Type: text/plain; charset=utf-8\r\n",
		"\r\n\r\nLine one\r\nLine two\r\n",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("message lacks %q:\n%s", want, s)
		}
	}
	if _, err := Render("noreply@example.org", Message{Subject: "x"}, at); err == nil {
		t.Error("no recipient must be refused")
	}
	if _, err := Render("noreply@example.org", Message{To: []string{"a@example.org"}}, at); err == nil {
		t.Error("no subject must be refused")
	}
	for _, bad := range []Message{
		{To: []string{"a@example.org\r\nBcc: x@evil.example"}, Subject: "s"},
		{To: []string{"Eve <a@example.org>"}, Subject: "s"},
		{To: []string{"a@example.org, b@example.org"}, Subject: "s"},
		{To: []string{"a@example.org"}, Subject: "s\r\nBcc: x@evil.example"},
	} {
		if _, err := Render("noreply@example.org", bad, at); err == nil {
			t.Errorf("Render(%+v) must be refused", bad)
		}
	}
	if _, err := Render("noreply@example.org\r\nX: y", Message{To: []string{"a@example.org"}, Subject: "s"}, at); err == nil {
		t.Error("a sender with a line break must be refused")
	}
}

// A server that offers no STARTTLS gets no password.
func TestSendRefusesACleartextServer(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	seen := make(chan string, 8)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		r := bufio.NewReader(conn)
		_, _ = conn.Write([]byte("220 plain.example ESMTP\r\n"))
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			seen <- strings.TrimSpace(line)
			switch {
			case strings.HasPrefix(strings.ToUpper(line), "EHLO"):
				_, _ = conn.Write([]byte("250-plain.example\r\n250 AUTH PLAIN LOGIN\r\n"))
			case strings.HasPrefix(strings.ToUpper(line), "QUIT"):
				_, _ = conn.Write([]byte("221 bye\r\n"))
				return
			default:
				_, _ = conn.Write([]byte("250 ok\r\n"))
			}
		}
	}()
	host, port, _ := net.SplitHostPort(ln.Addr().String())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err = Send(ctx, Settings{Host: host, Port: port, Username: "u", Password: "secret", From: "noreply@example.org", StartTLS: true},
		Message{To: []string{"a@example.org"}, Subject: "s", Text: "t"})
	if err == nil || !strings.Contains(err.Error(), "STARTTLS") {
		t.Fatalf("Send = %v, want a STARTTLS refusal", err)
	}
	close(seen)
	for line := range seen {
		if strings.Contains(line, "secret") || strings.HasPrefix(strings.ToUpper(line), "AUTH") {
			t.Fatalf("the password or AUTH went over the wire in the clear: %q", line)
		}
	}
}
