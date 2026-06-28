// Copyright 2026 The Gentian Authors. Licensed under Apache 2.0.

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

// KeycloakUser is a minimal Keycloak user representation.
type KeycloakUser struct {
	ID       string
	Username string
	Enabled  bool
}

// KeycloakAdminClient calls the Keycloak Admin REST API.
type KeycloakAdminClient struct {
	baseURL   string
	username  string
	password  string
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
		body := map[string]any{"enabled": true}
		_, err = c.doAdminExpect(ctx, token, http.MethodPut, "/admin/realms/"+url.PathEscape(realm), body, http.StatusNoContent, http.StatusOK)
		return err
	}
	if status != http.StatusNotFound {
		return fmt.Errorf("keycloak get realm %s: unexpected status %d", realm, status)
	}
	body := map[string]any{
		"realm":       realm,
		"enabled":     true,
		"displayName": displayName,
	}
	_, err = c.doAdminExpect(ctx, token, http.MethodPost, "/admin/realms", body, http.StatusCreated)
	return err
}

func (c *KeycloakAdminClient) ListRealmUsers(ctx context.Context, realm string) ([]KeycloakUser, error) {
	token, err := c.adminToken(ctx)
	if err != nil {
		return nil, err
	}
	path := fmt.Sprintf("/admin/realms/%s/users?max=1000", url.PathEscape(realm))
	req, err := c.newAdminRequest(ctx, token, http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("keycloak list users: %s", resp.Status)
	}
	var raw []struct {
		ID       string `json:"id"`
		Username string `json:"username"`
		Enabled  bool   `json:"enabled"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, err
	}
	out := make([]KeycloakUser, 0, len(raw))
	for _, u := range raw {
		if !u.Enabled || u.ID == "" {
			continue
		}
		out = append(out, KeycloakUser{ID: u.ID, Username: u.Username, Enabled: u.Enabled})
	}
	return out, nil
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
	_, _ = io.Copy(io.Discard, resp.Body)
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
