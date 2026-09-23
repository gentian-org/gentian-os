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

package authn

import (
	"context"
	"fmt"

	"github.com/go-jose/go-jose/v4/jwt"
)

// BackchannelLogoutEvent is the event a logout token carries (OpenID Connect
// Back-Channel Logout 1.0 §2.4).
const BackchannelLogoutEvent = "http://schemas.openid.net/event/backchannel-logout"

// Logout is what a back-channel logout token says: which session of which
// subject the issuer has ended.
type Logout struct {
	Subject   string
	SessionID string
	Realm     string
}

type logoutClaims struct {
	SessionID string         `json:"sid"`
	Events    map[string]any `json:"events"`
	Nonce     string         `json:"nonce"`
}

// VerifyLogout checks a back-channel logout token from one of this
// platform's realms and returns the session it ends.
//
// A logout token is not an access token: it names no audience of ours -- its
// aud is the zone client that Keycloak is telling -- and carries no typ of
// Bearer. What makes it believable is the issuer's signature, the realm
// being one of this platform's, the events claim naming the logout event,
// and a session id to act on; a nonce marks a token that is not a logout
// token at all (§2.6). Any zone client's logout is accepted: the sid it
// names is the Keycloak SSO session, which is the same session at every
// client, and ending it at the edge is what every client's logout means.
func (v *Verifier) VerifyLogout(ctx context.Context, raw string) (*Logout, error) {
	tok, err := jwt.ParseSigned(raw, algorithms)
	if err != nil {
		return nil, fmt.Errorf("%w: parse: %v", ErrUnauthenticated, err)
	}
	if len(tok.Headers) != 1 || tok.Headers[0].KeyID == "" {
		return nil, fmt.Errorf("%w: token names no key", ErrUnauthenticated)
	}
	var unverified jwt.Claims
	if err := tok.UnsafeClaimsWithoutVerification(&unverified); err != nil {
		return nil, fmt.Errorf("%w: claims: %v", ErrUnauthenticated, err)
	}
	realm, err := v.realmOf(unverified.Issuer)
	if err != nil {
		return nil, err
	}
	key, err := v.key(ctx, realm, tok.Headers[0].KeyID)
	if err != nil {
		return nil, err
	}
	var std jwt.Claims
	var lc logoutClaims
	if err := tok.Claims(key, &std, &lc); err != nil {
		return nil, fmt.Errorf("%w: signature: %v", ErrUnauthenticated, err)
	}
	if err := std.ValidateWithLeeway(jwt.Expected{
		Issuer: v.cfg.IssuerBase + "/realms/" + realm,
		Time:   v.cfg.Now(),
	}, leeway); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnauthenticated, err)
	}
	if std.IssuedAt == nil {
		return nil, fmt.Errorf("%w: logout token has no iat", ErrUnauthenticated)
	}
	if _, ok := lc.Events[BackchannelLogoutEvent]; !ok {
		return nil, fmt.Errorf("%w: not a logout token: events names no %s", ErrUnauthenticated, BackchannelLogoutEvent)
	}
	if lc.Nonce != "" {
		return nil, fmt.Errorf("%w: a logout token carries no nonce", ErrUnauthenticated)
	}
	if lc.SessionID == "" || std.Subject == "" {
		return nil, fmt.Errorf("%w: logout token names no session or subject", ErrUnauthenticated)
	}
	return &Logout{Subject: std.Subject, SessionID: lc.SessionID, Realm: realm}, nil
}
