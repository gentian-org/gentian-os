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
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"net/smtp"
	"strings"
	"time"
)

// EndpointValidator probes a credential against its target before it is stored.
//
// This is the service's justification. A form that tests a token against its
// endpoint turns a class of latent failure — "tenant provisioning stalled, the
// path exists, the password was pasted with a trailing newline" — into a red
// field, minutes after the mistake rather than hours.
//
// It covers the same types as the installer's shell validators plus the ones
// shell cannot reach. Where both implement a type they must agree; the shell
// set is deliberately tiny so that overlap stays small.
type EndpointValidator struct {
	HTTP *http.Client

	// Relay resolves the SMTP relay endpoint. Nil means smtp validation cannot
	// run, and says so rather than passing.
	Relay RelayResolver
}

// NewEndpointValidator builds a validator with a bounded timeout, so an
// unreachable endpoint fails the form rather than hanging the request.
func NewEndpointValidator() *EndpointValidator {
	return &EndpointValidator{HTTP: &http.Client{Timeout: 15 * time.Second}}
}

// RelayResolver reports the upstream SMTP relay this cluster sends through.
//
// The smtp-relay requirement carries a username and a password and no endpoint,
// because the endpoint is not a credential: it is on the Cluster claim, next to
// the mail mode that decides whether a relay is used at all. Asking the person
// entering a password to also retype the host would invite them to enter a
// different one from the one Postfix actually dials, and the validation would
// then pass against a server nothing uses.
type RelayResolver func(ctx context.Context) (host, port string, err error)

// Validate dispatches on spec.validate.type.
//
// An unknown type is an ERROR, not a pass. Silently accepting a credential the
// catalogue asked to have checked is the failure this exists to prevent.
func (v *EndpointValidator) Validate(ctx context.Context, kind string, fields map[string]string) error {
	switch kind {
	case "", "noop":
		return nil
	case "oci-registry":
		return v.basicAuthProbe(ctx, fields["host"], "/v2/", fields["username"], fields["password"])
	case "git-https":
		repo := strings.TrimSuffix(fields["url"], ".git")
		return v.basicAuthProbe(ctx, repo, ".git/info/refs?service=git-upload-pack",
			fields["username"], fields["token"])
	case "oidc-discovery":
		return v.bearerProbe(ctx, fields["url"], fields["api-token"])
	case "smtp":
		return v.smtpProbe(ctx, fields)
	default:
		return fmt.Errorf("unknown validator type %q", kind)
	}
}

func (v *EndpointValidator) basicAuthProbe(ctx context.Context, base, suffix, user, pass string) error {
	if base == "" {
		return fmt.Errorf("no endpoint to probe: the requirement declares no host or url")
	}
	url := strings.TrimSuffix(base, "/") + suffix
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	// Only send credentials when there are some: an empty password makes a
	// public endpoint answer 401, reporting a good source as a bad credential.
	if pass != "" {
		req.SetBasicAuth(user, pass)
	}
	return v.classify(url, req)
}

func (v *EndpointValidator) bearerProbe(ctx context.Context, url, token string) error {
	if url == "" {
		return fmt.Errorf("no endpoint to probe: the requirement declares no url")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return v.classify(url, req)
}

// classify separates UNREACHABLE from REJECTED, because they need different
// fixes and an operator shown the wrong one looks in the wrong place.
func (v *EndpointValidator) classify(url string, req *http.Request) error {
	resp, err := v.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("%s is unreachable: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()

	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return nil
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return fmt.Errorf("%s rejected the credential (HTTP %d)", url, resp.StatusCode)
	case resp.StatusCode == http.StatusNotFound:
		return fmt.Errorf("%s returned 404 — a private resource also 404s when the credential cannot see it", url)
	default:
		return fmt.Errorf("%s returned an unexpected HTTP %d", url, resp.StatusCode)
	}
}

// smtpProbe authenticates against the relay the cluster is configured to use.
//
// This used to refuse outright — "not reachable over HTTP", which is true of
// the HTTP client and not of the process. The consequence was that smtp-relay
// could not be stored at all: handleSet validates before writing, so a
// validator that always errors makes the credential unsettable, and the only
// documented way to supply it returned 422 forever. A validator that cannot
// run must not be the reason a credential cannot exist.
//
// Mirrors the shell validator: STARTTLS, then AUTH, and success is the server
// accepting the credentials. Implicit TLS on 465 is handled too, because a
// relay on that port never offers STARTTLS and would otherwise read as one
// that refused to secure the connection.
func (v *EndpointValidator) smtpProbe(ctx context.Context, fields map[string]string) error {
	user := fields["relay_username"]
	pass := fields["relay_password"]
	if user == "" || pass == "" {
		return fmt.Errorf("relay_username and relay_password are both required")
	}
	if v.Relay == nil {
		return fmt.Errorf("cannot check these credentials: this cluster's relay endpoint is unknown")
	}
	host, port, err := v.Relay(ctx)
	if err != nil {
		return fmt.Errorf("cannot check these credentials: %w", err)
	}
	if host == "" {
		return fmt.Errorf("cannot check these credentials: no relay host is set on the Cluster claim " +
			"(spec.mail.host). Set it before supplying the relay's credentials, or they will be " +
			"checked against nothing")
	}
	if port == "" {
		port = "587"
	}
	addr := net.JoinHostPort(host, port)

	dialer := &net.Dialer{Timeout: 10 * time.Second}
	var conn net.Conn
	if port == "465" {
		// Implicit TLS. No STARTTLS is offered on this port.
		conn, err = tls.DialWithDialer(dialer, "tcp", addr, &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12})
	} else {
		conn, err = dialer.DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		return fmt.Errorf("%s is unreachable: %w", addr, err)
	}
	defer func() { _ = conn.Close() }()

	client, err := smtp.NewClient(conn, host)
	if err != nil {
		return fmt.Errorf("%s did not answer as an SMTP server: %w", addr, err)
	}
	defer func() { _ = client.Quit() }()

	if port != "465" {
		if ok, _ := client.Extension("STARTTLS"); !ok {
			// Refused rather than downgraded: AUTH on a cleartext connection
			// puts the password on the wire, and a validator is not the place
			// to decide that is acceptable.
			return fmt.Errorf("%s does not offer STARTTLS; refusing to send credentials in the clear", addr)
		}
		if err := client.StartTLS(&tls.Config{ServerName: host, MinVersion: tls.VersionTLS12}); err != nil {
			return fmt.Errorf("STARTTLS with %s failed: %w", addr, err)
		}
	}

	// PLAIN first, LOGIN second: most relays take PLAIN, and the ones that
	// insist on LOGIN — which the shell validator uses — refuse PLAIN with a
	// 504 rather than treating it as a bad password.
	if err := client.Auth(smtp.PlainAuth("", user, pass, host)); err != nil {
		if lerr := client.Auth(loginAuth{user: user, pass: pass, host: host}); lerr != nil {
			return fmt.Errorf("%s rejected these credentials: %w", addr, err)
		}
	}
	return nil
}

// loginAuth implements the AUTH LOGIN exchange, which Go's net/smtp does not
// ship. Some relays offer only this one.
type loginAuth struct{ user, pass, host string }

func (a loginAuth) Start(server *smtp.ServerInfo) (string, []byte, error) {
	if !server.TLS {
		return "", nil, fmt.Errorf("refusing AUTH LOGIN on an unencrypted connection")
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
	default:
		return nil, fmt.Errorf("unexpected AUTH LOGIN challenge %q", string(fromServer))
	}
}
