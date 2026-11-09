/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

// Package mailer sends the platform's own mail: one plain-text message
// through the SMTP settings the cluster already keeps for Keycloak
// (keycloak-smtp-credentials), over STARTTLS or implicit TLS and never in
// the clear. It is what the operator uses to tell the cluster's
// administrators something -- a release that waits for them -- and nothing
// else: no templates, no HTML, no attachments.
package mailer

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"mime"
	"net"
	"net/mail"
	"net/smtp"
	"strings"
	"time"
)

// Settings is how the platform submits mail.
type Settings struct {
	Host, Port         string
	Username, Password string
	// From is the sender, "noreply@<kernelDomain>".
	From string
	// SSL is implicit TLS (port 465); StartTLS upgrades a plain connection.
	// Neither set is refused: a password does not go over the wire in the
	// clear.
	SSL, StartTLS bool
}

// Message is one mail to several people.
type Message struct {
	To      []string
	Subject string
	Text    string
}

// ErrNotConfigured is a cluster whose mail settings are absent or turned off.
var ErrNotConfigured = errors.New("mail is not configured on this cluster")

// SettingsFromSecret reads keycloak-smtp-credentials as the platform writes
// it in both mail modes: smtp_configure gates use, the port defaults to 587,
// the sender to noreply@<kernelDomain>, and ssl/starttls are on only when
// written "true" (how the realm's SMTP Job reads them).
func SettingsFromSecret(data map[string][]byte, kernelDomain string) (Settings, error) {
	get := func(k string) string { return strings.TrimSpace(string(data[k])) }
	if get("smtp_configure") != "true" || get("smtp_host") == "" || get("smtp_user") == "" || get("smtp_password") == "" {
		return Settings{}, ErrNotConfigured
	}
	s := Settings{
		Host: get("smtp_host"), Port: get("smtp_port"),
		Username: get("smtp_user"), Password: get("smtp_password"),
		From:     get("smtp_from"),
		SSL:      get("smtp_ssl") == "true",
		StartTLS: get("smtp_starttls") == "true",
	}
	if s.Port == "" {
		s.Port = "587"
	}
	if s.From == "" {
		if kernelDomain == "" {
			return Settings{}, fmt.Errorf("%w: no sender address and no kernel domain to derive one from", ErrNotConfigured)
		}
		s.From = "noreply@" + kernelDomain
	}
	if !s.SSL && !s.StartTLS {
		// The platform never writes this; a hand-edited Secret could.
		s.StartTLS = true
	}
	return s, nil
}

// Render is the message as it goes on the wire, RFC 5322 with a text/plain
// UTF-8 body. The recipients are in To, so each sees who else was told.
func Render(from string, m Message, now time.Time) ([]byte, error) {
	if len(m.To) == 0 {
		return nil, errors.New("a message needs a recipient")
	}
	if strings.TrimSpace(m.Subject) == "" {
		return nil, errors.New("a message needs a subject")
	}
	// Every address is one bare address, and nothing that goes into a
	// header carries a line break: a recipient or subject that did could
	// add headers or recipients of its own.
	if err := checkAddress(from); err != nil {
		return nil, fmt.Errorf("sender: %w", err)
	}
	for _, to := range m.To {
		if err := checkAddress(to); err != nil {
			return nil, fmt.Errorf("recipient: %w", err)
		}
	}
	if strings.ContainsAny(m.Subject, "\r\n") {
		return nil, errors.New("a subject is one line")
	}
	id := make([]byte, 12)
	if _, err := rand.Read(id); err != nil {
		return nil, err
	}
	domain := from
	if at := strings.LastIndex(from, "@"); at >= 0 {
		domain = from[at+1:]
	}
	var b strings.Builder
	fmt.Fprintf(&b, "From: %s\r\n", from)
	fmt.Fprintf(&b, "To: %s\r\n", strings.Join(m.To, ", "))
	fmt.Fprintf(&b, "Subject: %s\r\n", mime.QEncoding.Encode("utf-8", m.Subject))
	fmt.Fprintf(&b, "Date: %s\r\n", now.UTC().Format(time.RFC1123Z))
	fmt.Fprintf(&b, "Message-ID: <%s@%s>\r\n", hex.EncodeToString(id), domain)
	b.WriteString("MIME-Version: 1.0\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Transfer-Encoding: 8bit\r\nAuto-Submitted: auto-generated\r\n\r\n")
	// Bare line feeds become CRLF, and a line that is one dot would end the
	// message early; net/smtp's writer handles the dot, the CRLF is ours.
	b.WriteString(strings.ReplaceAll(strings.ReplaceAll(m.Text, "\r\n", "\n"), "\n", "\r\n"))
	if !strings.HasSuffix(m.Text, "\n") {
		b.WriteString("\r\n")
	}
	return []byte(b.String()), nil
}

// checkAddress accepts one bare address, "user@host", and nothing else.
func checkAddress(addr string) error {
	if strings.ContainsAny(addr, "\r\n<>,") {
		return fmt.Errorf("%q is not a single bare address", addr)
	}
	parsed, err := mail.ParseAddress(addr)
	if err != nil || parsed.Address != addr || parsed.Name != "" {
		return fmt.Errorf("%q is not a single bare address", addr)
	}
	return nil
}

// Send submits the message. The connection is TLS before the password is
// sent: implicit on SSL, STARTTLS otherwise, and a server that offers
// neither is refused. PLAIN first, LOGIN second, as the credential validator
// does.
func Send(ctx context.Context, s Settings, m Message) error {
	body, err := Render(s.From, m, time.Now())
	if err != nil {
		return err
	}
	addr := net.JoinHostPort(s.Host, s.Port)
	dialer := &net.Dialer{Timeout: 15 * time.Second}
	tlsConfig := &tls.Config{ServerName: s.Host, MinVersion: tls.VersionTLS12}
	var conn net.Conn
	if s.SSL {
		conn, err = (&tls.Dialer{NetDialer: dialer, Config: tlsConfig}).DialContext(ctx, "tcp", addr)
	} else {
		conn, err = dialer.DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		return fmt.Errorf("%s is unreachable: %w", addr, err)
	}
	defer func() { _ = conn.Close() }()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	} else {
		_ = conn.SetDeadline(time.Now().Add(60 * time.Second))
	}
	client, err := smtp.NewClient(conn, s.Host)
	if err != nil {
		return fmt.Errorf("%s did not answer as an SMTP server: %w", addr, err)
	}
	defer func() { _ = client.Quit() }()
	if !s.SSL {
		if ok, _ := client.Extension("STARTTLS"); !ok {
			return fmt.Errorf("%s does not offer STARTTLS; refusing to send credentials in the clear", addr)
		}
		if err := client.StartTLS(tlsConfig); err != nil {
			return fmt.Errorf("STARTTLS with %s failed: %w", addr, err)
		}
	}
	if err := client.Auth(smtp.PlainAuth("", s.Username, s.Password, s.Host)); err != nil {
		if lerr := client.Auth(loginAuth{user: s.Username, pass: s.Password}); lerr != nil {
			return fmt.Errorf("%s rejected the platform's mail credentials: %w", addr, err)
		}
	}
	if err := client.Mail(s.From); err != nil {
		return fmt.Errorf("%s refused the sender %s: %w", addr, s.From, err)
	}
	for _, to := range m.To {
		if err := client.Rcpt(to); err != nil {
			return fmt.Errorf("%s refused the recipient %s: %w", addr, to, err)
		}
	}
	w, err := client.Data()
	if err != nil {
		return fmt.Errorf("%s refused the message: %w", addr, err)
	}
	if _, err := w.Write(body); err != nil {
		return fmt.Errorf("sending to %s: %w", addr, err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("%s did not accept the message: %w", addr, err)
	}
	return nil
}

// loginAuth is the AUTH LOGIN exchange, which net/smtp does not ship and
// some relays insist on.
type loginAuth struct{ user, pass string }

func (a loginAuth) Start(server *smtp.ServerInfo) (string, []byte, error) {
	if !server.TLS {
		return "", nil, errors.New("refusing AUTH LOGIN on an unencrypted connection")
	}
	return "LOGIN", nil, nil
}

func (a loginAuth) Next(fromServer []byte, more bool) ([]byte, error) {
	if !more {
		return nil, nil
	}
	switch strings.ToLower(strings.TrimSpace(string(fromServer))) {
	case "username:":
		return []byte(a.user), nil
	case "password:":
		return []byte(a.pass), nil
	}
	return nil, fmt.Errorf("unexpected AUTH LOGIN challenge %q", string(fromServer))
}
