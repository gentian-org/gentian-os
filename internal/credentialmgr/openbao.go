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

// Package credentialmgr serves the Credential Manager: a view over the
// CredentialRequirement catalogue and ESO's satisfaction status, plus a write
// path that writes as the requesting user.
//
// Two constraints shape every type in this package, and both are structural
// rather than conventional — they are enforced by what the code can express,
// not by reviewers remembering them.
//
// # The service holds no OpenBao token of its own
//
// It exchanges the caller's Keycloak OIDC token for a short-lived OpenBao token
// through the JWT auth backend, and the *user's* identity performs the write.
// The alternative — the service holding broad write credentials and doing
// authorisation itself — creates one component able to write every secret in
// the cluster, and records the service rather than the human in the audit
// device. That weakens the audit guarantee instead of strengthening it.
//
// Enforced by [Writer] having no field that could hold a service token: every
// write takes the caller's token as an argument.
//
// # Write-only, no read-back
//
// Displaying a credential creates an exfiltration surface, needs a different
// threat model, and hands an attacker with a stolen session everything at once.
// The API returns metadata only: whether a value exists, who set it and when,
// and the last validation result. Lost credentials are rotated, not recovered.
//
// Enforced by [Status] having no field capable of carrying a value, and by a
// test that enumerates every route and asserts none returns one.
package credentialmgr

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	ctrl "sigs.k8s.io/controller-runtime"
)

// OpenBao is a minimal client. It deliberately implements only the four calls
// this service needs; a fuller client would make it easy to add a read path
// that the design forbids.
type OpenBao struct {
	Addr    string
	KVMount string
	// AuthMount is the path the JWT/OIDC auth backend is enabled at for the
	// KERNEL realm. It is not "jwt": the backend is enabled with -path=oidc, so
	// the login endpoint is auth/oidc/login. Hardcoding the plugin's default
	// name here meant every exchange hit a mount that does not exist.
	AuthMount string

	// KernelRealm names the realm AuthMount trusts. A token from any other
	// realm is routed to that realm's own mount instead — see mountForToken.
	KernelRealm string
	// OIDCRoles are the auth backend roles a caller's token is offered to, in
	// order, until one accepts it. The roles, not this service, decide who a
	// caller is: each binds a different group claim, and the policies on
	// whichever token comes back are what the viewer is derived from. A single
	// role would mean only that role's group could ever use this service.
	OIDCRoles []string

	HTTP *http.Client
}

// ErrUpstream marks a failure to REACH OpenBao, as opposed to OpenBao
// declining the caller.
//
// The distinction is the whole reason this exists. Every transport failure used
// to arrive at the handler as an ordinary error and leave as 401, so the portal
// told an administrator "OpenBao refused the token — check that you are in the
// cluster-admin group" when the truth was that no request had reached OpenBao
// at all. The advice was correct, actionable, and about the wrong thing, which
// is worse than no advice: it sends someone to audit group membership that is
// already right.
var ErrUpstream = errors.New("openbao unreachable")

// NewOpenBao builds a client with a bounded HTTP timeout, so a hung OpenBao
// surfaces as a failed request rather than a wedged handler.
//
// caCert, when non-empty, is the PEM OpenBao's certificate is verified against.
// It is not optional in practice: OpenBao serves a self-signed certificate on
// this platform, so the default transport — which verifies against the system
// roots — fails every exchange. ESO reaches the same endpoint by loading the
// same CA out of the openbao-tls Secret, and this is that pattern in Go.
//
// An empty caCert keeps the system roots, which is right for a cluster that
// gave OpenBao a publicly trusted certificate.
func NewOpenBao(addr, kvMount, authMount, kernelRealm string, oidcRoles []string, caCert []byte, skipVerify bool) *OpenBao {
	tlsConf := &tls.Config{MinVersion: tls.VersionTLS12}
	if skipVerify {
		// An escape hatch, not a mode. Named so it appears in the Deployment
		// for anyone wondering why verification is not happening.
		tlsConf.InsecureSkipVerify = true
	} else if len(caCert) > 0 {
		pool := x509.NewCertPool()
		if pool.AppendCertsFromPEM(caCert) {
			tlsConf.RootCAs = pool
		} else {
			ctrl.Log.WithName("credentialmgr").Info(
				"the configured OpenBao CA is not valid PEM; falling back to the system roots")
		}
	}
	return &OpenBao{
		Addr:        strings.TrimSuffix(addr, "/"),
		KVMount:     kvMount,
		AuthMount:   authMount,
		KernelRealm: kernelRealm,
		OIDCRoles:   oidcRoles,
		HTTP: &http.Client{
			Timeout:   15 * time.Second,
			Transport: &http.Transport{TLSClientConfig: tlsConf},
		},
	}
}

// Identity is OpenBao's verdict on a caller's token.
//
// Every field here was decided by OpenBao after it verified the JWT's
// signature, issuer and audience and applied the role's bound claims. None of
// it is asserted by the caller, which is the point: the alternative is this
// service parsing the JWT itself, which would make it a second identity
// authority that can disagree with the one enforcing the write.
type Identity struct {
	// Token is the short-lived OpenBao token the write is performed with.
	Token string
	// Policies are the policies OpenBao attached, from the role the token
	// matched. This is what "is this caller a cluster admin" is read from.
	Policies []string
	// Metadata carries the role's claim mappings — the tenant among them.
	Metadata map[string]string
}

// ExchangeToken trades the caller's OIDC token for a short-lived OpenBao token
// and the identity that came with it.
//
// This is the whole of the service's authorisation model: it does not decide
// what the caller may write. OpenBao's policy engine does, based on the claims
// in the presented token, and the resulting token is what performs the write —
// so the audit device records the human.

// refusalReason turns OpenBao's rejection into the name of the check that
// failed.
//
// This exists because the absence of it cost three separate debugging rounds.
// The service reported "OpenBao refused the token" and the console added "check
// that you are in the cluster-admin group" — while the actual causes were, in
// order, a login against a mount that does not exist, a role whose type forbids
// direct token exchange, and a token with no audience the role accepts. Group
// membership was correct every time, and it was the one thing the message named.
//
// The returned string is safe to show a caller: it names a category, never a
// policy, path or role. The detail goes to the log instead.
func refusalReason(status int, body string) string {
	b := strings.ToLower(body)
	switch {
	case strings.Contains(b, "audience"):
		return "the token's audience is not one this cluster's roles accept"
	case strings.Contains(b, "bound claim"), strings.Contains(b, "claim"):
		return "the token's claims do not match any role — typically the group claim"
	case strings.Contains(b, "role_type"), strings.Contains(b, "not allowed"):
		return "the auth backend role does not permit a direct token exchange"
	case strings.Contains(b, "could not be found"), strings.Contains(b, "unknown role"):
		return "the auth backend role does not exist on this cluster"
	case status == http.StatusNotFound:
		return "the auth backend is not mounted where this service expects it"
	case strings.Contains(b, "signature"), strings.Contains(b, "expired"), strings.Contains(b, "validating token"):
		return "the token did not validate — signature, issuer or expiry"
	default:
		return "OpenBao refused it and the reason is in the credential manager's log"
	}
}

// mountForToken picks the auth mount from the token's issuer.
//
// One JWT mount trusts one issuer. Tenant members authenticate in their own
// Keycloak realm — that is where their apps' OIDC clients live, so that is where
// the SSO session must exist — which means their token is signed by that realm
// and the kernel realm's mount cannot verify it. Each tenant realm therefore has
// its own mount, and this decides which one a token goes to.
//
// The issuer is read WITHOUT verifying the signature, and that is safe because
// it is used only to route. OpenBao then verifies against the chosen mount's
// JWKS, so a forged issuer merely picks a mount that refuses the token; it can
// never make one mount accept another realm's key. Nothing here is an
// authorisation decision.
func (b *OpenBao) mountForToken(oidcToken string) string {
	realm := realmFromUnverifiedToken(oidcToken)
	if realm == "" || realm == b.KernelRealm {
		return b.AuthMount
	}
	return "oidc-" + realm
}

// realmFromUnverifiedToken reads the realm out of a JWT's iss claim without
// checking the signature. Returns "" when the token is not a JWT, the claim is
// absent, or the issuer is not a Keycloak realm URL — every one of which falls
// back to the kernel mount rather than inventing a mount name from attacker
// input.
func realmFromUnverifiedToken(token string) string {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return ""
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var claims struct {
		Iss string `json:"iss"`
	}
	if err := json.Unmarshal(raw, &claims); err != nil {
		return ""
	}
	i := strings.LastIndex(claims.Iss, "/realms/")
	if i < 0 {
		return ""
	}
	realm := strings.Trim(claims.Iss[i+len("/realms/"):], "/")
	// A mount path is one segment. Anything else is not a realm name, and
	// concatenating it would address a different mount entirely.
	if realm == "" || strings.ContainsAny(realm, "/?#%") {
		return ""
	}
	return realm
}

func (b *OpenBao) ExchangeToken(ctx context.Context, oidcToken string) (Identity, error) {
	if oidcToken == "" {
		return Identity{}, fmt.Errorf("no OIDC token presented")
	}
	roles := b.OIDCRoles
	if len(roles) == 0 {
		return Identity{}, fmt.Errorf("no auth backend roles configured")
	}
	// Offered to each role in turn. A role whose bound claims do not match
	// refuses the token, which is a 400 rather than a fact about the caller —
	// so a refusal only rules out that role, not the request.
	log := ctrl.LoggerFrom(ctx)
	mount := b.mountForToken(oidcToken)
	var lastStatus int
	var lastReason string
	for _, role := range roles {
		id, status, body, err := b.exchangeWithRole(ctx, oidcToken, role, mount)
		if err != nil {
			return Identity{}, err
		}
		if status == http.StatusOK {
			return id, nil
		}
		lastStatus = status
		lastReason = refusalReason(status, body)
		// The log gets OpenBao's own words. Without this every refusal was a
		// dead end: the response cannot carry them, so nothing anywhere did,
		// and each cause had to be found by reading code instead.
		log.Info("OpenBao refused a token exchange",
			"role", role, "mount", mount, "status", status,
			"reason", lastReason, "openbao", truncate(body, 300))
	}
	// The category, not OpenBao's body: that can name policies and paths the
	// caller has no business learning from a failed login. Naming the failed
	// check is not the same as naming what it protects.
	return Identity{}, fmt.Errorf("token exchange rejected by every configured role: %s (last HTTP %d)",
		lastReason, lastStatus)
}

// exchangeWithRole performs one login attempt. A non-200 is returned as a
// status rather than an error, because the caller has another role to try; a
// transport failure is an error, because it says nothing about the token.
func (b *OpenBao) exchangeWithRole(ctx context.Context, oidcToken, role, mount string) (Identity, int, string, error) {
	body, _ := json.Marshal(map[string]string{
		"role": role,
		"jwt":  oidcToken,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		fmt.Sprintf("%s/v1/auth/%s/login", b.Addr, mount), bytes.NewReader(body))
	if err != nil {
		return Identity{}, 0, "", err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := b.HTTP.Do(req)
	if err != nil {
		// ErrUpstream, not a bare error: http.go distinguishes "could not reach
		// OpenBao" from "OpenBao said no", and the two must not read alike to a
		// caller — one is an outage, the other is an answer.
		return Identity{}, 0, "", fmt.Errorf("%w: %w", ErrUpstream, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		// Read it here or lose it: the caller cannot, once the body is closed,
		// and this is the only place OpenBao ever says why.
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return Identity{}, resp.StatusCode, string(raw), nil
	}
	var out struct {
		Auth struct {
			ClientToken   string            `json:"client_token"`
			TokenPolicies []string          `json:"token_policies"`
			Policies      []string          `json:"policies"`
			Metadata      map[string]string `json:"metadata"`
		} `json:"auth"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return Identity{}, 0, "", fmt.Errorf("malformed token exchange response: %w", err)
	}
	if out.Auth.ClientToken == "" {
		return Identity{}, 0, "", fmt.Errorf("token exchange returned no token")
	}
	policies := out.Auth.TokenPolicies
	if len(policies) == 0 {
		// Older OpenBao releases report the same list under "policies".
		policies = out.Auth.Policies
	}
	return Identity{
		Token:    out.Auth.ClientToken,
		Policies: policies,
		Metadata: out.Auth.Metadata,
	}, http.StatusOK, "", nil
}

// truncate bounds what reaches the log. OpenBao's errors are short; a
// pathological body should not become a megabyte of log line.
func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// PathMetadata is what the API is allowed to say about a stored credential.
// There is no field here that can carry a value, and that is the point.
type PathMetadata struct {
	Exists    bool      `json:"exists"`
	SetBy     string    `json:"setBy,omitempty"`
	UpdatedAt time.Time `json:"updatedAt,omitempty"`
	Version   int       `json:"version,omitempty"`
}

// Metadata reads a path's metadata. It calls the KV *metadata* endpoint, never
// the data endpoint, so no value is retrievable through this client at all.
func (b *OpenBao) Metadata(ctx context.Context, token, path string) (PathMetadata, error) {
	url := fmt.Sprintf("%s/v1/%s/metadata/%s", b.Addr, b.KVMount, strings.TrimPrefix(path, "/"))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return PathMetadata{}, err
	}
	req.Header.Set("X-Vault-Token", token)

	resp, err := b.HTTP.Do(req)
	if err != nil {
		return PathMetadata{}, fmt.Errorf("openbao unreachable: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	switch resp.StatusCode {
	case http.StatusNotFound:
		return PathMetadata{Exists: false}, nil
	case http.StatusOK:
	default:
		return PathMetadata{}, fmt.Errorf("metadata read failed (HTTP %d)", resp.StatusCode)
	}

	var out struct {
		Data struct {
			CurrentVersion int    `json:"current_version"`
			UpdatedTime    string `json:"updated_time"`
			CustomMetadata struct {
				SetBy string `json:"set_by"`
			} `json:"custom_metadata"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return PathMetadata{}, err
	}
	md := PathMetadata{
		Exists:  true,
		Version: out.Data.CurrentVersion,
		SetBy:   out.Data.CustomMetadata.SetBy,
	}
	if t, err := time.Parse(time.RFC3339, out.Data.UpdatedTime); err == nil {
		md.UpdatedAt = t
	}
	return md, nil
}

// Write stores a credential using the CALLER's token.
//
// The token is a parameter rather than client state precisely so this cannot be
// called on the service's own authority — there is no service authority to call
// it with.
//
// set_by is recorded as custom metadata so the "who set this" the API reports
// survives independently of the audit device, which an operator reading the UI
// may not have access to.
func (b *OpenBao) Write(ctx context.Context, token, path string, fields map[string]string, setBy string) error {
	if token == "" {
		return fmt.Errorf("no caller token: the credential manager cannot write on its own authority")
	}
	// No options block, and specifically no check-and-set.
	//
	// This sent {"options":{"cas":null}}. cas is KV v2's check-and-set: a
	// value of 0 means "write only if this path does not exist", and a null
	// is not a way of saying "no opinion" — it is a cas the server has to
	// parse, which it either rejects or reads as zero. Either way the write
	// fails the moment the path already has a version.
	//
	// Every path here has one. The installer seeds this mount at bootstrap,
	// so supplying a credential through the credential manager is always an
	// UPDATE of a path that exists, never a create — which made the one
	// operation this service exists to perform the one it could never do.
	//
	// Omitting options entirely is an unconditional write, which is what
	// "supply this credential" means. The mount does not set cas_required,
	// so nothing is being bypassed.
	// PATCH, not POST: a KV v2 write REPLACES the document, and several paths
	// hold more than the one credential a form supplies.
	//
	// gentian-os/kernel/dns/cloudflare is the case that found this. The
	// installer seeds api-token, zone-id and tunnel-cname there as one
	// document; the catalogue declares only api-token as an operator-supplied
	// field, because the other two are derived from the token and the running
	// cloudflared rather than typed by anyone. Supplying a rotated token
	// through the console therefore wrote a document containing api-token
	// alone, and the other two keys ceased to exist.
	//
	// Nothing said so. The write succeeded and the console reported success;
	// the ExternalSecret that reads all three failed two components away with
	// `cannot find secret data for key: "zone-id"`, kept serving the Secret's
	// last good content — the superseded token — and the operator went on
	// authenticating with a credential that had already been revoked.
	//
	// A merge patch is a server-side merge: keys present in the request are
	// written, keys absent are left alone, which is what "supply this
	// credential" has always meant. Doing it in this client instead would mean
	// reading the document first, and this client deliberately cannot read
	// values at all — see Metadata.
	payload := map[string]any{
		"data": fields,
	}
	body, _ := json.Marshal(payload)
	url := fmt.Sprintf("%s/v1/%s/data/%s", b.Addr, b.KVMount, strings.TrimPrefix(path, "/"))
	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("X-Vault-Token", token)
	req.Header.Set("Content-Type", "application/merge-patch+json")

	resp, err := b.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrUpstream, err)
	}
	defer func() { _ = resp.Body.Close() }()

	// A patch needs something to merge onto. 404 is a path with no version
	// yet — a credential nothing has seeded — where a full write is both
	// correct and the only option, and destroys nothing because there is
	// nothing there.
	if resp.StatusCode == http.StatusNotFound {
		return b.createAndRecord(ctx, token, path, url, body, setBy)
	}

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		// OpenBao's own words, not a guess at them. "the caller's policy may
		// not permit this" was a plausible reading of every non-2xx, and it
		// sent an operator to audit a policy that already granted the path
		// while the actual answer was in a response body nobody read.
		//
		// Safe to relay: reaching here means the caller already authenticated
		// and holds the cluster-admin policy, so the path and the reason are
		// things they may see. The value is not in the response.
		detail := strings.TrimSpace(readCapped(resp.Body, 2048))
		if detail == "" {
			detail = "no detail returned"
		}
		return fmt.Errorf("openbao rejected the write to %s (HTTP %d): %s", path, resp.StatusCode, detail)
	}
	return b.setCustomMetadata(ctx, token, path, setBy)
}

// createAndRecord writes the whole document, for a path that does not exist yet.
//
// Only reachable from Write's 404 branch. It is the pre-patch behaviour, kept
// for the one case where replacing the document cannot lose anything: there is
// no document.
func (b *OpenBao) createAndRecord(ctx context.Context, token, path, url string, body []byte, setBy string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("X-Vault-Token", token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := b.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrUpstream, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		detail := strings.TrimSpace(readCapped(resp.Body, 2048))
		if detail == "" {
			detail = "no detail returned"
		}
		return fmt.Errorf("openbao rejected the write to %s (HTTP %d): %s", path, resp.StatusCode, detail)
	}
	return b.setCustomMetadata(ctx, token, path, setBy)
}

func (b *OpenBao) setCustomMetadata(ctx context.Context, token, path, setBy string) error {
	if setBy == "" {
		return nil
	}
	body, _ := json.Marshal(map[string]any{
		"custom_metadata": map[string]string{"set_by": setBy},
	})
	url := fmt.Sprintf("%s/v1/%s/metadata/%s", b.Addr, b.KVMount, strings.TrimPrefix(path, "/"))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("X-Vault-Token", token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := b.HTTP.Do(req)
	if err != nil {
		// The value is already stored; failing the whole request here would
		// tell the operator the write failed when it did not.
		return nil
	}
	defer func() { _ = resp.Body.Close() }()
	return nil
}

// readCapped reads at most n bytes, so a large or hostile error body cannot
// become the log line.
func readCapped(r io.Reader, n int64) string {
	b, err := io.ReadAll(io.LimitReader(r, n))
	if err != nil {
		return ""
	}
	return string(b)
}
