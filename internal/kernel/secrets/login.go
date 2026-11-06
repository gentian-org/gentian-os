/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package secrets

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// Default location of the projected ServiceAccount token inside any pod.
const defaultSATokenPath = "/var/run/secrets/kubernetes.io/serviceaccount/token"

// ErrUnreachable marks a login that never reached the vault: a refused
// connection, a timeout, a certificate that did not verify.
var ErrUnreachable = errors.New("openbao unreachable")

// ErrLoginRefused marks a login the vault answered with anything but a token.
var ErrLoginRefused = errors.New("openbao refused the login")

// Login is a pod's login to the vault by its ServiceAccount, through the
// Kubernetes auth method. The token it gets is kept and renewed within 60s of
// its expiry; nothing in the cluster holds a static long-lived token.
type Login struct {
	// Addr is the vault's address, without a trailing slash.
	Addr string
	// AuthPath is where the Kubernetes auth method is enabled.
	AuthPath string
	// Role is the auth role logged in to.
	Role string
	// SATokenPath is the projected ServiceAccount token presented.
	SATokenPath string
	// HTTP makes the login, and is the client every request to the vault is
	// made with.
	HTTP *http.Client

	mu       sync.Mutex
	token    string
	tokenExp time.Time
	static   bool // the token was set by SetStaticToken and is never dropped
}

// NewLogin is a Login to addr as role with the ServiceAccount token at
// saTokenPath ("" is the projected default), verifying the vault as
// transport does.
func NewLogin(addr, role, saTokenPath string, transport http.RoundTripper) *Login {
	if saTokenPath == "" {
		saTokenPath = defaultSATokenPath
	}
	return &Login{
		Addr:        strings.TrimRight(addr, "/"),
		AuthPath:    "kubernetes",
		Role:        role,
		SATokenPath: saTokenPath,
		HTTP:        &http.Client{Timeout: 10 * time.Second, Transport: transport},
	}
}

// Transport is how a client verifies the vault. With ca non-nil the vault is
// verified against ca alone, and a ca that holds no certificate verifies
// nothing, so every request fails rather than trusting the system roots in
// its place. With ca nil the system roots are used, which is right for a
// vault with a publicly trusted certificate. skipVerify turns verification
// off, for development only.
func Transport(ca []byte, skipVerify bool) http.RoundTripper {
	if !skipVerify && ca == nil {
		return nil
	}
	conf := &tls.Config{MinVersion: tls.VersionTLS12}
	if skipVerify {
		conf.InsecureSkipVerify = true
	} else {
		conf.RootCAs = x509.NewCertPool()
		conf.RootCAs.AppendCertsFromPEM(ca)
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.TLSClientConfig = conf
	return tr
}

// SkipVerifyFromEnv reports BAO_SKIP_VERIFY (or VAULT_SKIP_VERIFY) set to
// "true".
func SkipVerifyFromEnv() bool {
	return os.Getenv("BAO_SKIP_VERIFY") == "true" || os.Getenv("VAULT_SKIP_VERIFY") == "true"
}

// TransportFromEnv is Transport with the vault's CA certificate read from the
// file BAO_CACERT (or VAULT_CACERT) names, and SkipVerifyFromEnv. A named file
// that cannot be read verifies nothing.
func TransportFromEnv() http.RoundTripper {
	path := os.Getenv("BAO_CACERT")
	if path == "" {
		path = os.Getenv("VAULT_CACERT")
	}
	var ca []byte
	if path != "" {
		var err error
		if ca, err = os.ReadFile(path); err != nil {
			ca = []byte{}
		}
	}
	return Transport(ca, SkipVerifyFromEnv())
}

// unreachable is a login that never reached the vault.
type unreachable struct{ error }

func (e unreachable) Is(target error) bool { return target == ErrUnreachable }
func (e unreachable) Unwrap() error        { return e.error }

// refusedLogin is a login the vault answered without a token.
type refusedLogin struct{ error }

func (e refusedLogin) Is(target error) bool { return target == ErrLoginRefused }
func (e refusedLogin) Unwrap() error        { return e.error }

// Token returns a valid token, logging in when none is kept or the one kept
// is within 60s of expiry. A login that never reached the vault is
// ErrUnreachable; one the vault answered without a token is ErrLoginRefused.
func (l *Login) Token(ctx context.Context) (string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.token != "" && time.Until(l.tokenExp) > 60*time.Second {
		return l.token, nil
	}

	jwt, err := os.ReadFile(l.SATokenPath)
	if err != nil {
		return "", fmt.Errorf("read SA token %s: %w", l.SATokenPath, err)
	}
	body, _ := json.Marshal(map[string]string{
		"role": l.Role,
		"jwt":  strings.TrimSpace(string(jwt)),
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		fmt.Sprintf("%s/v1/auth/%s/login", l.Addr, l.AuthPath),
		bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := l.HTTP.Do(req)
	if err != nil {
		return "", fmt.Errorf("openbao kubernetes login: %w", unreachable{err})
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return "", refusedLogin{fmt.Errorf("openbao kubernetes login: HTTP %d: %s",
			resp.StatusCode, strings.TrimSpace(string(raw)))}
	}
	var out struct {
		Auth struct {
			ClientToken   string `json:"client_token"`
			LeaseDuration int    `json:"lease_duration"`
		} `json:"auth"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", refusedLogin{fmt.Errorf("openbao kubernetes login: decode: %w", err)}
	}
	if out.Auth.ClientToken == "" {
		return "", refusedLogin{errors.New("openbao kubernetes login: empty client_token")}
	}
	l.token = out.Auth.ClientToken
	if out.Auth.LeaseDuration > 0 {
		l.tokenExp = time.Now().Add(time.Duration(out.Auth.LeaseDuration) * time.Second)
	} else {
		l.tokenExp = time.Now().Add(time.Hour)
	}
	return l.token, nil
}

// SetStaticToken bypasses the login and uses tok for 24 hours. For tests
// against a stand-in vault.
func (l *Login) SetStaticToken(tok string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.token = tok
	l.tokenExp = time.Now().Add(24 * time.Hour)
	l.static = true
}

// Forget drops tok if it is still the kept token, so the next call logs in
// again, and reports whether a call is worth making again. A static token is
// kept: there is nothing to log in with in its place.
func (l *Login) Forget(tok string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.static {
		return false
	}
	if l.token == tok {
		l.token, l.tokenExp = "", time.Time{}
	}
	return true
}
