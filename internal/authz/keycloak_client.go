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

package authz

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const keycloakAdminCLI = "admin-cli"
const keycloakAdminPageSize = 200

type keycloakUserRecord struct {
	ID         string              `json:"id"`
	Username   string              `json:"username"`
	Enabled    bool                `json:"enabled"`
	Email      string              `json:"email"`
	Attributes map[string][]string `json:"attributes"`
}

type keycloakGroupRecord struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

func keycloakUserFromRecord(u keycloakUserRecord) (KeycloakUser, bool) {
	if !u.Enabled || u.ID == "" {
		return KeycloakUser{}, false
	}
	return KeycloakUser(u), true
}

func paginatedAdminPath(basePath string, first, max int) string {
	sep := "?"
	if strings.Contains(basePath, "?") {
		sep = "&"
	}
	return fmt.Sprintf("%s%sfirst=%d&max=%d", basePath, sep, first, max)
}

func (c *KeycloakAdminClient) getAdminJSON(ctx context.Context, token, path string, out any) error {
	req, err := c.newAdminRequest(ctx, token, http.MethodGet, path, nil)
	if err != nil {
		return err
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("keycloak GET %s: %s", path, resp.Status)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// KeycloakUser is a minimal Keycloak user representation.
type KeycloakUser struct {
	ID         string
	Username   string
	Enabled    bool
	Email      string
	Attributes map[string][]string
}

// KeycloakAdminClient calls the Keycloak Admin REST API.
type KeycloakAdminClient struct {
	baseURL    string
	username   string
	password   string
	httpClient *http.Client

	mu          sync.Mutex
	token       string
	tokenExpiry time.Time
}

func NewKeycloakAdminClient(baseURL, username, password string) *KeycloakAdminClient {
	return &KeycloakAdminClient{
		baseURL:  strings.TrimRight(baseURL, "/"),
		username: username,
		password: password,
		httpClient: &http.Client{
			Timeout: defaultHTTPTimeout,
		},
	}
}

func (c *KeycloakAdminClient) EnsureRealm(ctx context.Context, realm, displayName string) error {
	token, err := c.adminToken(ctx)
	if err != nil {
		return err
	}
	status, err := c.doAdmin(ctx, token, http.MethodGet, "/admin/realms/"+url.PathEscape(realm), nil)
	if err != nil {
		return err
	}
	if status == http.StatusOK {
		body := map[string]any{
			"enabled":                      true,
			"bruteForceProtected":          true,
			"failureFactor":                30,
			"maxFailureWaitSeconds":        900,
			"minimumQuickLoginWaitSeconds": 60,
			"waitIncrementSeconds":         60,
			"quickLoginCheckMilliSeconds":  1000,
			"maxDeltaTimeSeconds":          86400,
		}
		_, err = c.doAdminExpect(ctx, token, http.MethodPut, "/admin/realms/"+url.PathEscape(realm), body, http.StatusNoContent, http.StatusOK)
		return err
	}
	if status != http.StatusNotFound {
		return fmt.Errorf("keycloak get realm %s: unexpected status %d", realm, status)
	}
	body := map[string]any{
		"realm":                        realm,
		"enabled":                      true,
		"displayName":                  displayName,
		"bruteForceProtected":          true,
		"failureFactor":                30,
		"maxFailureWaitSeconds":        900,
		"minimumQuickLoginWaitSeconds": 60,
		"waitIncrementSeconds":         60,
		"quickLoginCheckMilliSeconds":  1000,
		"maxDeltaTimeSeconds":          86400,
	}
	_, err = c.doAdminExpect(ctx, token, http.MethodPost, "/admin/realms", body, http.StatusCreated)
	return err
}

// DefaultBrowserSecurityHeaders disables X-Frame-Options on realms so OIDC broker
// /endpoint callbacks work inside portal iframes. Ingress sets frame-ancestors.
var DefaultBrowserSecurityHeaders = map[string]any{
	"contentSecurityPolicy":           "",
	"contentSecurityPolicyReportOnly": "",
	"strictTransportSecurity":         "max-age=31536000; includeSubDomains",
	"xContentTypeOptions":             "nosniff",
	"xFrameOptions":                   "",
	"xRobotsTag":                      "none",
	"xXSSProtection":                  "1; mode=block",
	"referrerPolicy":                  "no-referrer",
}

// BrowserSecurityHeadersJSON is the JSON fragment embedded in realm provisioning shell scripts.
func BrowserSecurityHeadersJSON() string {
	b, err := json.Marshal(DefaultBrowserSecurityHeaders)
	if err != nil {
		return `{}`
	}
	return string(b)
}

// GentianLoginTheme is the Keycloak login theme shipped in
// kernel/services/keycloak-idp/theme/. Keycloak falls back to the built-in theme
// if it is absent, so setting this before the theme is deployed degrades to stock
// styling rather than breaking login.
const GentianLoginTheme = "gentian"

// UpdateRealmBrowserSecurityHeaders applies DefaultBrowserSecurityHeaders,
// functional session timeouts (12 hours) and the Gentian login theme to a realm.
func (c *KeycloakAdminClient) UpdateRealmBrowserSecurityHeaders(ctx context.Context, realm string) error {
	if realm == "" {
		return nil
	}
	token, err := c.adminToken(ctx)
	if err != nil {
		return err
	}
	body := map[string]any{
		"browserSecurityHeaders": DefaultBrowserSecurityHeaders,
		"accessTokenLifespan":    43200, // 12 hours
		"ssoSessionIdleTimeout":  43200, // 12 hours
		"ssoSessionMaxLifespan":  43200, // 12 hours
		// Every realm that can render a login screen renders Gentian's, so a user
		// sent to the IdP sees the portal's own card rather than stock Keycloak.
		// Set here rather than per realm-creation path because the kernel realm has
		// no such path — it is bootstrapped once at install.
		"loginTheme": GentianLoginTheme,
	}
	_, err = c.doAdminExpect(ctx, token, http.MethodPut, "/admin/realms/"+url.PathEscape(realm), body, http.StatusNoContent, http.StatusOK)
	return err
}

func (c *KeycloakAdminClient) ListRealmUsers(ctx context.Context, realm string) ([]KeycloakUser, error) {
	token, err := c.adminToken(ctx)
	if err != nil {
		return nil, err
	}
	base := fmt.Sprintf("/admin/realms/%s/users", url.PathEscape(realm))
	var out []KeycloakUser
	for first := 0; ; first += keycloakAdminPageSize {
		var page []keycloakUserRecord
		if err := c.getAdminJSON(ctx, token, paginatedAdminPath(base, first, keycloakAdminPageSize), &page); err != nil {
			return nil, fmt.Errorf("keycloak list users: %w", err)
		}
		for _, u := range page {
			if user, ok := keycloakUserFromRecord(u); ok {
				out = append(out, user)
			}
		}
		if len(page) < keycloakAdminPageSize {
			break
		}
	}
	return out, nil
}

// ListGroupMembers returns enabled users in a Keycloak group by group name.
func (c *KeycloakAdminClient) ListGroupMembers(ctx context.Context, realm, groupName string) ([]KeycloakUser, error) {
	groupID, err := c.findGroupID(ctx, realm, groupName)
	if err != nil {
		return nil, err
	}
	if groupID == "" {
		return nil, nil
	}
	token, err := c.adminToken(ctx)
	if err != nil {
		return nil, err
	}
	base := fmt.Sprintf("/admin/realms/%s/groups/%s/members", url.PathEscape(realm), url.PathEscape(groupID))
	var out []KeycloakUser
	for first := 0; ; first += keycloakAdminPageSize {
		var page []keycloakUserRecord
		if err := c.getAdminJSON(ctx, token, paginatedAdminPath(base, first, keycloakAdminPageSize), &page); err != nil {
			return nil, fmt.Errorf("keycloak list group members: %w", err)
		}
		for _, u := range page {
			if user, ok := keycloakUserFromRecord(u); ok {
				out = append(out, user)
			}
		}
		if len(page) < keycloakAdminPageSize {
			break
		}
	}
	return out, nil
}

func (c *KeycloakAdminClient) findGroupID(ctx context.Context, realm, groupName string) (string, error) {
	token, err := c.adminToken(ctx)
	if err != nil {
		return "", err
	}
	base := fmt.Sprintf("/admin/realms/%s/groups", url.PathEscape(realm))
	for first := 0; ; first += keycloakAdminPageSize {
		var page []keycloakGroupRecord
		if err := c.getAdminJSON(ctx, token, paginatedAdminPath(base, first, keycloakAdminPageSize), &page); err != nil {
			return "", fmt.Errorf("keycloak list groups: %w", err)
		}
		for _, g := range page {
			if g.Name == groupName && g.ID != "" {
				return g.ID, nil
			}
		}
		if len(page) < keycloakAdminPageSize {
			break
		}
	}
	return "", nil
}

func (c *KeycloakAdminClient) adminToken(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token != "" && time.Now().Before(c.tokenExpiry.Add(-30*time.Second)) {
		return c.token, nil
	}
	form := url.Values{}
	form.Set("client_id", keycloakAdminCLI)
	form.Set("username", c.username)
	form.Set("password", c.password)
	form.Set("grant_type", "password")
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+"/realms/master/protocol/openid-connect/token",
		strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("keycloak token: %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	var out struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	if out.AccessToken == "" {
		return "", fmt.Errorf("keycloak token: empty access_token")
	}
	c.token = out.AccessToken
	c.tokenExpiry = time.Now().Add(time.Duration(out.ExpiresIn) * time.Second)
	return c.token, nil
}

func (c *KeycloakAdminClient) doAdmin(ctx context.Context, token, method, path string, body any) (int, error) {
	var payload []byte
	if body != nil {
		var err error
		payload, err = json.Marshal(body)
		if err != nil {
			return 0, err
		}
		fmt.Printf("DEBUG: doAdmin method=%s path=%s payload=%s\n", method, path, string(payload))
	}
	req, err := c.newAdminRequest(ctx, token, method, path, payload)
	if err != nil {
		return 0, err
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	bodyBytes, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return resp.StatusCode, fmt.Errorf("status %d: %s", resp.StatusCode, string(bodyBytes))
	}
	return resp.StatusCode, nil
}

func (c *KeycloakAdminClient) doAdminExpect(ctx context.Context, token, method, path string, body any, ok ...int) (int, error) {
	status, err := c.doAdmin(ctx, token, method, path, body)
	if err != nil {
		return status, err
	}
	for _, code := range ok {
		if status == code {
			return status, nil
		}
	}
	return status, fmt.Errorf("keycloak %s %s: status %d", method, path, status)
}

func (c *KeycloakAdminClient) newAdminRequest(ctx context.Context, token, method, path string, body []byte) (*http.Request, error) {
	var reader io.Reader
	if body != nil {
		reader = strings.NewReader(string(body))
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", "Bearer "+token)
	return req, nil
}

func (c *KeycloakAdminClient) EnsureGroup(ctx context.Context, realm, groupName string, attributes map[string][]string) (string, error) {
	id, err := c.findGroupID(ctx, realm, groupName)
	if err != nil {
		return "", err
	}
	if id != "" {
		if len(attributes) > 0 {
			token, err := c.adminToken(ctx)
			if err != nil {
				return "", err
			}
			body := map[string]any{
				"name":       groupName,
				"attributes": attributes,
			}
			path := fmt.Sprintf("/admin/realms/%s/groups/%s", url.PathEscape(realm), url.PathEscape(id))
			_, err = c.doAdminExpect(ctx, token, http.MethodPut, path, body, http.StatusNoContent, http.StatusOK)
			if err != nil {
				return "", err
			}
		}
		return id, nil
	}

	token, err := c.adminToken(ctx)
	if err != nil {
		return "", err
	}

	body := map[string]any{
		"name": groupName,
	}
	if len(attributes) > 0 {
		body["attributes"] = attributes
	}

	path := fmt.Sprintf("/admin/realms/%s/groups", url.PathEscape(realm))
	status, err := c.doAdminExpect(ctx, token, http.MethodPost, path, body, http.StatusCreated, http.StatusConflict)
	if err != nil {
		return "", err
	}

	if status == http.StatusCreated {
		// Attempt to resolve ID from Location header or find it
		// For simplicity, find it again
		return c.findGroupID(ctx, realm, groupName)
	}

	return c.findGroupID(ctx, realm, groupName)
}

func (c *KeycloakAdminClient) AddUserToGroup(ctx context.Context, realm, userID, groupID string) error {
	token, err := c.adminToken(ctx)
	if err != nil {
		return err
	}
	path := fmt.Sprintf("/admin/realms/%s/users/%s/groups/%s", url.PathEscape(realm), url.PathEscape(userID), url.PathEscape(groupID))
	_, err = c.doAdminExpect(ctx, token, http.MethodPut, path, nil, http.StatusNoContent, http.StatusOK)
	return err
}
