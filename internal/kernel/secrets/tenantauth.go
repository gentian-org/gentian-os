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

package secrets

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// TenantAuth configures one JWT auth mount per tenant realm.
//
// # Why a mount per tenant rather than one shared mount
//
// A JWT auth mount trusts exactly one issuer. Tenant members authenticate in
// their own Keycloak realm — that is where their account and every per-app OIDC
// client live, so that is where the SSO session has to exist for app launches to
// reuse it — which means their portal token is signed by that realm's key. The
// kernel realm's mount cannot verify it, and the refusal happens at the
// signature, before any claim is looked at.
//
// OpenBao would accept several validation keys on one mount, and that is the
// arrangement to avoid. A tenant administrator holds realm-management/realm-admin
// in their own realm, so they can put themselves in a group named anything at
// all — including the cluster's admin group — and sign a token with it. On a
// mount trusting many keys that token verifies and matches the cluster-admin
// role. Isolation has to come from the mount boundary, because claim *values*
// are exactly what the tenant administrator controls.
//
// So: one mount, one issuer, one tenant. A tenant's token can only be presented
// to that tenant's mount, whose roles grant only that tenant's policy.
type TenantAuth struct {
	c *KVClient
}

// NewTenantAuth borrows the operator's authenticated OpenBao session. It holds
// no credential of its own; the Kubernetes auth login and token refresh are
// KVClient's.
func NewTenantAuth(c *KVClient) *TenantAuth { return &TenantAuth{c: c} }

// MountPath is the auth mount for a tenant realm. One segment, derived from the
// realm, so the operator's policy can be scoped to `sys/auth/oidc-+` rather than
// to every mount OpenBao has.
func MountPath(realm string) string { return "oidc-" + realm }

// RoleConfig is the JWT role bound to a tenant's admins group.
type RoleConfig struct {
	Name           string
	BoundAudiences []string
	GroupsClaim    string
	BoundGroup     string
	TokenPolicies  []string
	TokenTTL       int
	TokenMaxTTL    int

	// ClaimMappings copies verified claims into the token's alias metadata.
	//
	// This is how the tenant reaches anything downstream. The mount proves which
	// realm signed the token, but nothing reads a mount name: the credential
	// manager takes the tenant from metadata, and the policies template their
	// paths from it. Without a mapping the metadata is empty, the caller has no
	// tenant, and everything they create is scoped to the cluster instead.
	ClaimMappings map[string]string
}

func (t *TenantAuth) do(ctx context.Context, method, apiPath string, body any) (int, []byte, error) {
	tok, err := t.c.authToken(ctx)
	if err != nil {
		return 0, nil, fmt.Errorf("openbao login: %w", err)
	}
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, t.c.addr+apiPath, rdr)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("X-Vault-Token", tok)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := t.c.http.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("openbao unreachable: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
	return resp.StatusCode, raw, nil
}

// EnsureMount enables the JWT backend for a realm and points it at that realm's
// discovery document.
//
// Enabling is not idempotent — OpenBao answers 400 "path is already in use" —
// so an existing mount is detected first and only its config is written. That
// is the same shape B-07 uses for the kernel mount, and for the same reason: a
// mount that exists with the wrong issuer is worse than one that is absent,
// because it fails at signature verification and reads as a permissions problem.
func (t *TenantAuth) EnsureMount(ctx context.Context, realm, discoveryURL string) error {
	mount := MountPath(realm)

	// Anything but 200 is treated as absent, rather than 404 specifically.
	// OpenBao answers a read of a mount that does not exist with 400 and "No auth
	// engine at <path>/", not 404, so keying on 404 meant the mount was never
	// created — and the config write below then failed with 404 "route entry not
	// found", which reads as the config path being wrong rather than as the mount
	// being missing.
	status, _, err := t.do(ctx, http.MethodGet, "/v1/sys/auth/"+mount, nil)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		st, body, err := t.do(ctx, http.MethodPost, "/v1/sys/auth/"+mount, map[string]any{
			"type":        "jwt",
			"description": "Portal tokens from the " + realm + " realm",
		})
		if err != nil {
			return err
		}
		// "path is already in use" means the read was wrong about it being
		// absent — a permission gap on the read, or a mount created between the
		// two calls. Either way the mount exists, which is what was wanted.
		alreadyExists := st == http.StatusBadRequest && bytes.Contains(body, []byte("already in use"))
		if (st < 200 || st > 299) && !alreadyExists {
			return fmt.Errorf("enable auth mount %s: HTTP %d: %s", mount, st, truncateBody(body))
		}
	}

	st, body, err := t.do(ctx, http.MethodPost, "/v1/auth/"+mount+"/config", map[string]any{
		"oidc_discovery_url": discoveryURL,
		"default_role":       "",
	})
	if err != nil {
		return err
	}
	if st < 200 || st > 299 {
		return fmt.Errorf("configure auth mount %s: HTTP %d: %s", mount, st, truncateBody(body))
	}
	return nil
}

// EnsureRole writes the JWT role a tenant administrator's token is exchanged
// against. role_type is jwt, not oidc: an oidc role serves the browser flow and
// the plugin refuses a direct token exchange against one.
func (t *TenantAuth) EnsureRole(ctx context.Context, realm string, cfg RoleConfig) error {
	mount := MountPath(realm)
	body := map[string]any{
		"role_type":       "jwt",
		"user_claim":      "preferred_username",
		"bound_audiences": cfg.BoundAudiences,
		"bound_claims": map[string]any{
			cfg.GroupsClaim: cfg.BoundGroup,
		},
		"bound_claims_type": "string",
		"claim_mappings":    cfg.ClaimMappings,
		"token_policies":    cfg.TokenPolicies,
		"token_ttl":         cfg.TokenTTL,
		"token_max_ttl":     cfg.TokenMaxTTL,
	}
	st, raw, err := t.do(ctx, http.MethodPost, "/v1/auth/"+mount+"/role/"+cfg.Name, body)
	if err != nil {
		return err
	}
	if st < 200 || st > 299 {
		return fmt.Errorf("write role %s on %s: HTTP %d: %s", cfg.Name, mount, st, truncateBody(raw))
	}
	return nil
}

// DeleteMount removes a tenant's auth mount and every role on it. Called when
// the Tenant goes: a mount left behind still trusts a realm that no longer
// exists, and a realm name can be reused.
func (t *TenantAuth) DeleteMount(ctx context.Context, realm string) error {
	st, body, err := t.do(ctx, http.MethodDelete, "/v1/sys/auth/"+MountPath(realm), nil)
	if err != nil {
		return err
	}
	if st == http.StatusNotFound || (st >= 200 && st <= 299) {
		return nil
	}
	return fmt.Errorf("delete auth mount %s: HTTP %d: %s", MountPath(realm), st, truncateBody(body))
}

func truncateBody(b []byte) string {
	s := string(bytes.TrimSpace(b))
	if len(s) > 300 {
		return s[:300] + "…"
	}
	return s
}
