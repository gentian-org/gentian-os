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
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// OpenBao is a minimal client. It deliberately implements only the four calls
// this service needs; a fuller client would make it easy to add a read path
// that the design forbids.
type OpenBao struct {
	Addr    string
	KVMount string
	// OIDCRole is the JWT auth backend role the caller's token is exchanged
	// against. The role, not this service, decides which policies the resulting
	// token carries.
	OIDCRole string

	HTTP *http.Client
}

// NewOpenBao builds a client with a bounded HTTP timeout, so a hung OpenBao
// surfaces as a failed request rather than a wedged handler.
func NewOpenBao(addr, kvMount, oidcRole string) *OpenBao {
	return &OpenBao{
		Addr:     strings.TrimSuffix(addr, "/"),
		KVMount:  kvMount,
		OIDCRole: oidcRole,
		HTTP:     &http.Client{Timeout: 15 * time.Second},
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
func (b *OpenBao) ExchangeToken(ctx context.Context, oidcToken string) (Identity, error) {
	if oidcToken == "" {
		return Identity{}, fmt.Errorf("no OIDC token presented")
	}
	body, _ := json.Marshal(map[string]string{
		"role": b.OIDCRole,
		"jwt":  oidcToken,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		b.Addr+"/v1/auth/jwt/login", bytes.NewReader(body))
	if err != nil {
		return Identity{}, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := b.HTTP.Do(req)
	if err != nil {
		return Identity{}, fmt.Errorf("openbao unreachable: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		// Deliberately not echoing OpenBao's body: it can name policies and
		// paths the caller has no business learning about from a failed login.
		return Identity{}, fmt.Errorf("token exchange rejected (HTTP %d)", resp.StatusCode)
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
		return Identity{}, fmt.Errorf("malformed token exchange response: %w", err)
	}
	if out.Auth.ClientToken == "" {
		return Identity{}, fmt.Errorf("token exchange returned no token")
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
	}, nil
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
	payload := map[string]any{
		"data": fields,
		"options": map[string]any{
			"cas": nil,
		},
	}
	body, _ := json.Marshal(payload)
	url := fmt.Sprintf("%s/v1/%s/data/%s", b.Addr, b.KVMount, strings.TrimPrefix(path, "/"))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("X-Vault-Token", token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := b.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("openbao unreachable: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("write rejected (HTTP %d) — the caller's policy may not permit %s",
			resp.StatusCode, path)
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
