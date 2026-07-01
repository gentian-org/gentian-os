// Copyright 2026 The Gentian Authors. Licensed under Apache 2.0.

package nextcloud

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const ocsAPIRequestHeader = "OCS-APIRequest"

// Client calls the Nextcloud OCS user/group API with admin credentials.
type Client struct {
	BaseURL  string
	Username string
	Password string
	HTTP     *http.Client
}

func (c *Client) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: 30 * time.Second}
}

type ocsResponse struct {
	Meta struct {
		Status     string `xml:"status"`
		StatusCode int    `xml:"statuscode"`
		Message    string `xml:"message"`
	} `xml:"meta"`
	Data struct {
		Users struct {
			Elements []string `xml:"element"`
		} `xml:"users"`
	} `xml:"data"`
}

func (c *Client) ListGroupUsers(ctx context.Context, groupID string) ([]string, error) {
	endpoint := fmt.Sprintf("%s/ocs/v1.php/cloud/groups/%s",
		strings.TrimRight(c.BaseURL, "/"), url.PathEscape(groupID))
	body, err := c.ocsRequest(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	var parsed ocsResponse
	if err := xml.Unmarshal(body, &parsed); err != nil {
		return nil, err
	}
	if parsed.Meta.Status != "ok" && parsed.Meta.StatusCode != 100 {
		return nil, fmt.Errorf("nextcloud list group %s: %s", groupID, parsed.Meta.Message)
	}
	return parsed.Data.Users.Elements, nil
}

func (c *Client) UserExists(ctx context.Context, userID string) (bool, error) {
	endpoint := fmt.Sprintf("%s/ocs/v1.php/cloud/users/%s",
		strings.TrimRight(c.BaseURL, "/"), url.PathEscape(userID))
	_, err := c.ocsRequest(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		if strings.Contains(err.Error(), "not found") || strings.Contains(strings.ToLower(err.Error()), "does not exist") {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func (c *Client) EnsureUser(ctx context.Context, userID, displayName string) error {
	exists, err := c.UserExists(ctx, userID)
	if err != nil {
		return err
	}
	if exists {
		return nil
	}
	createEndpoint := strings.TrimRight(c.BaseURL, "/") + "/ocs/v1.php/cloud/users"
	form := url.Values{}
	form.Set("userid", userID)
	form.Set("password", randomPassword())
	if displayName != "" {
		form.Set("displayName", displayName)
	}
	_, err = c.ocsRequest(ctx, http.MethodPost, createEndpoint, form)
	return err
}

func (c *Client) AddUserToGroup(ctx context.Context, userID, groupID string) error {
	endpoint := fmt.Sprintf("%s/ocs/v1.php/cloud/users/%s/groups",
		strings.TrimRight(c.BaseURL, "/"), url.PathEscape(userID))
	form := url.Values{}
	form.Set("groupid", groupID)
	_, err := c.ocsRequest(ctx, http.MethodPost, endpoint, form)
	return err
}

func (c *Client) RemoveUserFromGroup(ctx context.Context, userID, groupID string) error {
	endpoint := fmt.Sprintf("%s/ocs/v1.php/cloud/users/%s/groups/%s",
		strings.TrimRight(c.BaseURL, "/"), url.PathEscape(userID), url.PathEscape(groupID))
	_, err := c.ocsRequest(ctx, http.MethodDelete, endpoint, nil)
	return err
}

func (c *Client) ocsRequest(ctx context.Context, method, endpoint string, form url.Values) ([]byte, error) {
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set(ocsAPIRequestHeader, "true")
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	req.SetBasicAuth(c.Username, c.Password)
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	payload, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("nextcloud: not found")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("nextcloud %s %s: http %d", method, endpoint, resp.StatusCode)
	}
	var parsed ocsResponse
	if err := xml.Unmarshal(payload, &parsed); err != nil {
		return payload, nil
	}
	if parsed.Meta.Status == "failure" || (parsed.Meta.StatusCode != 0 && parsed.Meta.StatusCode != 100) {
		return nil, fmt.Errorf("nextcloud ocs: %s", strings.TrimSpace(parsed.Meta.Message))
	}
	return payload, nil
}

func randomPassword() string {
	// OCS user create requires a password even for OIDC-only accounts.
	return fmt.Sprintf("Gtn-%d", time.Now().UnixNano())
}
