/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
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

	"github.com/gentian-org/gentian-os/internal/locales"
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
	ID         string              `json:"id"`
	Name       string              `json:"name"`
	Attributes map[string][]string `json:"attributes,omitempty"`
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
// UpdateRealmBrowserSecurityHeaders writes the realm settings that have no
// other writer.
//
// offered is the languages the realm offers. It is passed in rather than read
// from this process's environment because it is an administrator's choice, not
// a property of the build: the Tenant declares it, the director is what
// changes it, and this only applies what git already says. Empty means the
// platform's own set.
func (c *KeycloakAdminClient) UpdateRealmBrowserSecurityHeaders(ctx context.Context, realm string, offered []string) error {
	if realm == "" {
		return nil
	}
	token, err := c.adminToken(ctx)
	if err != nil {
		return err
	}
	// The headers and the theme. NOT the session or token lifetimes.
	//
	// This used to carry accessTokenLifespan and both session timeouts, all
	// 12 hours, and it runs on every tenant reconcile — so it was a second
	// writer of them, and the one that ran last. The realm bootstrap set a
	// five-minute access token, reported that it had, and this put twelve
	// hours back within the minute. A twelve-hour access token is also
	// exactly what made a logout need a revocation list: nothing the edge
	// held expired for half a day.
	//
	// Lifetimes belong to the realm bootstrap, which sets a short access
	// token against a workday session. One writer.
	body := map[string]any{
		"browserSecurityHeaders": DefaultBrowserSecurityHeaders,
		// Every realm that can render a login screen renders Gentian's, so a user
		// sent to the IdP sees the portal's own card rather than stock Keycloak.
		// Set here rather than per realm-creation path because the kernel realm has
		// no such path — it is bootstrapped once at install.
		"loginTheme": GentianLoginTheme,
		// And the languages it renders in (AD-15). Keycloak ships these
		// translations; a realm only has to say it wants them, and until it
		// does it serves English to everyone — which, for a German-speaking
		// market, is the product's first screen being in the wrong language.
		//
		// With this on, Keycloak honours the browser's Accept-Language and
		// offers a picker, so a German browser gets German without anybody
		// choosing anything. The default is what answers a browser asking for
		// a language that is not here, and it is English because that is the
		// language the platform's own strings are written in: a fallback
		// should be the author's own words rather than a guess.
		//
		// Here for the same reason as the theme. A tenant realm gets this from
		// its Composition, which declares it alongside the theme; the kernel
		// realm has no Composition and this is the only thing that maintains
		// it.
	}
	// The languages, only where this is the realm's one writer.
	//
	// A tenant realm is composed, and tenant-default declares its languages
	// from the same spec.locales. Writing them here as well would make two
	// writers of one field and the last one would win -- which is exactly the
	// bug recorded above for the session lifetimes. So a caller that passes no
	// locales is saying "not mine to set", and this leaves the realm's own
	// alone rather than putting a default over a declared value.
	if len(offered) > 0 {
		langs := locales.Normalise(offered)
		body["internationalizationEnabled"] = true
		body["supportedLocales"] = langs
		// The FIRST language, not English: the order is the preference, so a
		// tenant listing de then en is saying it is German-speaking and also
		// serves English. Its login page should open in German for a browser
		// that asks for neither.
		body["defaultLocale"] = langs[0]
	}
	_, err = c.doAdminExpect(ctx, token, http.MethodPut, "/admin/realms/"+url.PathEscape(realm), body, http.StatusNoContent, http.StatusOK)
	return err
}

// UpdateRealmMailSender sets the name a realm's mail says it is from, and
// nothing else about its mail.
//
// The realm's mail settings are read back and written whole with only that
// name changed: the password comes back masked, and Keycloak keeps the stored
// one for a masked value, so the credential is never handled here. A realm
// with no mail configured is left alone -- there is no sender to name.
func (c *KeycloakAdminClient) UpdateRealmMailSender(ctx context.Context, realm, name string) error {
	if realm == "" || name == "" {
		return nil
	}
	token, err := c.adminToken(ctx)
	if err != nil {
		return err
	}
	var current struct {
		SMTPServer map[string]any `json:"smtpServer"`
	}
	if err := c.getAdminJSON(ctx, token, "/admin/realms/"+url.PathEscape(realm), &current); err != nil {
		return err
	}
	if len(current.SMTPServer) == 0 || current.SMTPServer["fromDisplayName"] == name {
		return nil
	}
	current.SMTPServer["fromDisplayName"] = name
	_, err = c.doAdminExpect(ctx, token, http.MethodPut, "/admin/realms/"+url.PathEscape(realm),
		map[string]any{"smtpServer": current.SMTPServer}, http.StatusNoContent, http.StatusOK)
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

// CountRealmUsers answers how many enabled accounts a realm holds, as Keycloak
// counts them. A number and nothing about who.
func (c *KeycloakAdminClient) CountRealmUsers(ctx context.Context, realm string) (int, error) {
	token, err := c.adminToken(ctx)
	if err != nil {
		return 0, err
	}
	var n int
	path := fmt.Sprintf("/admin/realms/%s/users/count?enabled=true", url.PathEscape(realm))
	if err := c.getAdminJSON(ctx, token, path, &n); err != nil {
		return 0, fmt.Errorf("keycloak count users: %w", err)
	}
	return n, nil
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

// GroupExists reports whether the realm has a group of this name.
func (c *KeycloakAdminClient) GroupExists(ctx context.Context, realm, groupName string) (bool, error) {
	id, err := c.findGroupID(ctx, realm, groupName)
	return id != "", err
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

// mergeGroupAttributes returns the group's current attributes with updates
// applied over them. Keys not named in updates are carried through unchanged.
func (c *KeycloakAdminClient) mergeGroupAttributes(
	ctx context.Context, token, realm, groupID string, updates map[string][]string,
) (map[string][]string, error) {
	var current keycloakGroupRecord
	path := fmt.Sprintf("/admin/realms/%s/groups/%s", url.PathEscape(realm), url.PathEscape(groupID))
	if err := c.getAdminJSON(ctx, token, path, &current); err != nil {
		return nil, fmt.Errorf("keycloak get group %s: %w", groupID, err)
	}
	merged := make(map[string][]string, len(current.Attributes)+len(updates))
	for k, v := range current.Attributes {
		merged[k] = v
	}
	for k, v := range updates {
		merged[k] = v
	}
	return merged, nil
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
			// Merge, because Keycloak's group update replaces the attribute map
			// wholesale and this is not its only writer. The tenant identity Job
			// writes the keys an AppProfile declares, the App Store writes the
			// default-grant marker, and an administrator sets others by hand in
			// the console. Sending only our own keys deleted everyone else's on
			// every pass.
			merged, err := c.mergeGroupAttributes(ctx, token, realm, id, attributes)
			if err != nil {
				return "", err
			}
			body := map[string]any{
				"name":       groupName,
				"attributes": merged,
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
