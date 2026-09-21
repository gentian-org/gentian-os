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

// Package authn establishes who is calling the director.
//
// Identity comes from the issuer and from nowhere else (security principle 1):
// a request carries a Keycloak access token, and the claims in it are believed
// only after its signature has been checked against that realm's published
// keys. No header, no query parameter and no in-cluster source address counts
// as identity.
//
// The platform has one realm per tenant plus the platform realm, so the set of
// acceptable issuers is open-ended. It is bounded instead by shape: the issuer
// must be exactly <base>/realms/<realm> under the one configured Keycloak base
// URL, with a realm name that is a plain label. That is what keeps the key
// fetch from being steered at an arbitrary host by an unverified token.
package authn

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

// ErrUnauthenticated wraps every reason a token is not accepted. Callers answer
// 401 and log the wrapped reason; the reason is never returned to the client.
var ErrUnauthenticated = errors.New("unauthenticated")

// Identity is what a verified token says about its bearer.
type Identity struct {
	// Subject is the token's sub: the stable user id, and the OpenFGA user.
	Subject string
	// Realm is the Keycloak realm that issued the token.
	Realm string
	// Issuer is the full iss claim.
	Issuer string
	// SessionID is the Keycloak sid, the handle revocation is recorded against.
	SessionID string
	// Client is the azp claim: which client the token was issued to.
	Client string
	// Name and Email are display attributes for the commit author. They are
	// never used for a decision.
	Name  string
	Email string
}

// Config configures a Verifier.
type Config struct {
	// IssuerBase is the Keycloak base URL as it appears in tokens, without a
	// trailing slash: tokens must carry iss = IssuerBase + "/realms/<realm>".
	IssuerBase string
	// JWKSBase is where keys are fetched from, when the director reaches
	// Keycloak by a different (in-cluster) URL than the one in tokens. Empty
	// means IssuerBase.
	JWKSBase string
	// Audience must appear in the token's aud claim.
	Audience string
	// HTTPClient fetches key sets. Nil means a client with a short timeout.
	HTTPClient *http.Client
	// Now is the clock; nil means time.Now.
	Now func() time.Time
}

// Verifier verifies Keycloak access tokens for any realm under one base URL.
type Verifier struct {
	cfg  Config
	mu   sync.Mutex
	keys map[string]*realmKeys
}

type realmKeys struct {
	set     jose.JSONWebKeySet
	fetched time.Time
}

const (
	// keyTTL is how long a realm's key set is served from memory.
	keyTTL = 10 * time.Minute
	// refetchFloor bounds how often an unknown kid may trigger a refetch, so a
	// stream of tokens with invented key ids cannot be turned into a stream of
	// requests at Keycloak.
	refetchFloor = 30 * time.Second
	// leeway is the clock skew tolerated on exp, nbf and iat.
	leeway = 30 * time.Second
	// maxJWKSBytes caps a key-set response.
	maxJWKSBytes = 1 << 20
)

// realmName is deliberately narrower than what Keycloak allows: it is
// interpolated into a URL before anything about the token has been verified.
var realmName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,62}$`)

// accepted signature algorithms. Symmetric algorithms and "none" are excluded
// by construction: go-jose refuses anything not listed here at parse time.
var algorithms = []jose.SignatureAlgorithm{jose.RS256, jose.RS384, jose.RS512, jose.ES256, jose.ES384, jose.PS256}

// NewVerifier returns a Verifier, or an error if the configuration could not
// authenticate anyone safely.
func NewVerifier(cfg Config) (*Verifier, error) {
	cfg.IssuerBase = strings.TrimRight(cfg.IssuerBase, "/")
	cfg.JWKSBase = strings.TrimRight(cfg.JWKSBase, "/")
	if cfg.IssuerBase == "" {
		return nil, errors.New("authn: issuer base URL is required")
	}
	if cfg.Audience == "" {
		return nil, errors.New("authn: audience is required: a token minted for another service must not be accepted here")
	}
	if cfg.JWKSBase == "" {
		cfg.JWKSBase = cfg.IssuerBase
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: 10 * time.Second}
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Verifier{cfg: cfg, keys: map[string]*realmKeys{}}, nil
}

// FromRequest verifies the bearer token of an HTTP request.
func (v *Verifier) FromRequest(r *http.Request) (*Identity, error) {
	h := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if len(h) <= len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
		return nil, fmt.Errorf("%w: no bearer token", ErrUnauthenticated)
	}
	return v.Verify(r.Context(), strings.TrimSpace(h[len(prefix):]))
}

type keycloakClaims struct {
	SessionID string `json:"sid"`
	Client    string `json:"azp"`
	Name      string `json:"name"`
	Username  string `json:"preferred_username"`
	Email     string `json:"email"`
	Type      string `json:"typ"`
}

// Verify checks a raw token and returns the identity it carries.
func (v *Verifier) Verify(ctx context.Context, raw string) (*Identity, error) {
	tok, err := jwt.ParseSigned(raw, algorithms)
	if err != nil {
		return nil, fmt.Errorf("%w: parse: %v", ErrUnauthenticated, err)
	}
	if len(tok.Headers) != 1 || tok.Headers[0].KeyID == "" {
		return nil, fmt.Errorf("%w: token names no key", ErrUnauthenticated)
	}

	// The issuer is read before the signature is checked, because it says
	// which keys to check against. Nothing else is taken from these claims.
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
	var kc keycloakClaims
	if err := tok.Claims(key, &std, &kc); err != nil {
		return nil, fmt.Errorf("%w: signature: %v", ErrUnauthenticated, err)
	}
	if std.Expiry == nil {
		return nil, fmt.Errorf("%w: token has no expiry", ErrUnauthenticated)
	}
	if err := std.ValidateWithLeeway(jwt.Expected{
		Issuer:      v.cfg.IssuerBase + "/realms/" + realm,
		AnyAudience: jwt.Audience{v.cfg.Audience},
		Time:        v.cfg.Now(),
	}, leeway); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnauthenticated, err)
	}
	if std.Subject == "" {
		return nil, fmt.Errorf("%w: token has no subject", ErrUnauthenticated)
	}
	// Keycloak marks access tokens "Bearer". An ID or refresh token is signed
	// by the same keys and must not pass for one.
	if kc.Type != "" && !strings.EqualFold(kc.Type, "Bearer") {
		return nil, fmt.Errorf("%w: token type %q is not an access token", ErrUnauthenticated, kc.Type)
	}

	name := kc.Name
	if name == "" {
		name = kc.Username
	}
	return &Identity{
		Subject:   std.Subject,
		Realm:     realm,
		Issuer:    std.Issuer,
		SessionID: kc.SessionID,
		Client:    kc.Client,
		Name:      name,
		Email:     kc.Email,
	}, nil
}

func (v *Verifier) realmOf(issuer string) (string, error) {
	prefix := v.cfg.IssuerBase + "/realms/"
	if !strings.HasPrefix(issuer, prefix) {
		return "", fmt.Errorf("%w: issuer %q is not this platform's", ErrUnauthenticated, issuer)
	}
	realm := issuer[len(prefix):]
	if !realmName.MatchString(realm) {
		return "", fmt.Errorf("%w: issuer names no valid realm", ErrUnauthenticated)
	}
	return realm, nil
}

// key returns the realm's key with the given id, fetching the realm's key set
// when it is not cached, has aged out, or does not contain the id (a rotation).
func (v *Verifier) key(ctx context.Context, realm, kid string) (*jose.JSONWebKey, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	now := v.cfg.Now()
	rk := v.keys[realm]
	if rk != nil && now.Sub(rk.fetched) < keyTTL {
		if k := find(rk.set, kid); k != nil {
			return k, nil
		}
		if now.Sub(rk.fetched) < refetchFloor {
			return nil, fmt.Errorf("%w: unknown key id", ErrUnauthenticated)
		}
	}
	set, err := v.fetch(ctx, realm)
	if err != nil {
		// A realm that does not exist, or a Keycloak that is down, cannot vouch
		// for anyone. Both are a 401 to the caller; the log has the difference.
		return nil, fmt.Errorf("%w: keys for realm %q: %v", ErrUnauthenticated, realm, err)
	}
	v.keys[realm] = &realmKeys{set: *set, fetched: now}
	if k := find(*set, kid); k != nil {
		return k, nil
	}
	return nil, fmt.Errorf("%w: unknown key id", ErrUnauthenticated)
}

func find(set jose.JSONWebKeySet, kid string) *jose.JSONWebKey {
	for _, k := range set.Key(kid) {
		if k.Use == "" || k.Use == "sig" {
			k := k
			return &k
		}
	}
	return nil
}

func (v *Verifier) fetch(ctx context.Context, realm string) (*jose.JSONWebKeySet, error) {
	url := v.cfg.JWKSBase + "/realms/" + realm + "/protocol/openid-connect/certs"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := v.cfg.HTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}
	var set jose.JSONWebKeySet
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxJWKSBytes)).Decode(&set); err != nil {
		return nil, err
	}
	return &set, nil
}
