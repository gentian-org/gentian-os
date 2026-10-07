/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

// Package modelgateway is the platform's model gateway (LiteLLM) as the
// operator administers it: the key each app of a tenant calls models with,
// and the tenant's team.
//
// Provisioning registers both, and until now nothing removed either: an app
// purged, or a tenant deleted, left a key that still authenticated and a team
// that still existed. The names are here so that what registers a key and
// what removes it cannot spell it differently.
package modelgateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/gentian-org/gentian-os/internal/layout"
)

const (
	// MasterKeySecret holds the gateway's admin key, in its namespace.
	MasterKeySecret = "llm-sensitive-values"
	// MasterKeyField is the key of that Secret the admin key is under.
	MasterKeyField = "litellm_master_key" //nolint:gosec // Secret key name, not a credential.
)

// Namespace is where the gateway runs.
func Namespace() string { return layout.System("llm") }

// DefaultBaseURL is the gateway's address inside the cluster.
func DefaultBaseURL() string {
	return fmt.Sprintf("http://litellm-proxy.%s.svc.cluster.local:4000", Namespace())
}

// VirtualKey is the key an app of a tenant presents to the gateway.
func VirtualKey(tenant, app string) string { return fmt.Sprintf("sk-gentian-%s-%s", tenant, app) }

// KeyAlias is the name the gateway knows that key by. Aliases are unique at
// the gateway, which is what lets a key be found and removed by name.
func KeyAlias(tenant, app string) string { return fmt.Sprintf("%s-%s", tenant, app) }

// TeamAlias is the name of a tenant's team.
func TeamAlias(tenant string) string { return tenant }

// Client talks to the gateway's admin API.
type Client struct {
	BaseURL   string
	MasterKey string
	HTTP      *http.Client
}

// FromCluster builds a client from the admin key the cluster holds. present
// is false, with no error, when the cluster holds none: it runs no gateway.
func FromCluster(ctx context.Context, c client.Reader) (gateway *Client, present bool, err error) {
	secret := &corev1.Secret{}
	if err := c.Get(ctx, types.NamespacedName{Name: MasterKeySecret, Namespace: Namespace()}, secret); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("read the model gateway's admin key (%s/%s): %w", Namespace(), MasterKeySecret, err)
	}
	key := string(secret.Data[MasterKeyField])
	if key == "" {
		return nil, false, fmt.Errorf("secret %s/%s has no %q key", Namespace(), MasterKeySecret, MasterKeyField)
	}
	return &Client{BaseURL: DefaultBaseURL(), MasterKey: key}, true, nil
}

func (c *Client) do(ctx context.Context, method, path string, body any) (int, []byte, error) {
	var payload io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		payload = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, payload)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.MasterKey)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	httpClient := c.HTTP
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 10 * time.Second}
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("model gateway %s: %w", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return resp.StatusCode, nil, fmt.Errorf("model gateway %s: %w", path, err)
	}
	return resp.StatusCode, raw, nil
}

// KeyExists reports whether the gateway has a key of this alias.
//
// It asks for the keys of the alias (/key/list) and not for the key
// (/key/info): the gateway goes on answering /key/info for a key that has
// been deleted, from the record it keeps of deleted keys, so a key removed at
// a purge would read as registered when the app is installed again and never
// be registered a second time -- the app would hold a key that authenticates
// against nothing.
func (c *Client) KeyExists(ctx context.Context, alias string) (bool, error) {
	status, raw, err := c.do(ctx, http.MethodGet, "/key/list?key_alias="+url.QueryEscape(alias), nil)
	if err != nil {
		return false, err
	}
	if status != http.StatusOK {
		return false, fmt.Errorf("model gateway /key/list answered %d: %s", status, truncate(raw, 200))
	}
	var listed struct {
		Keys *[]json.RawMessage `json:"keys"`
	}
	if err := json.Unmarshal(raw, &listed); err != nil || listed.Keys == nil {
		// An answer without a key list is not "no such key".
		return false, fmt.Errorf("model gateway /key/list: the answer has no key list: %s", truncate(raw, 200))
	}
	return len(*listed.Keys) > 0, nil
}

// DeleteKey removes the key of this alias, and reports whether there was one.
// It looks first and looks again afterwards: the key is gone when the gateway
// no longer lists it, whatever the delete answered.
func (c *Client) DeleteKey(ctx context.Context, alias string) (existed bool, err error) {
	exists, err := c.KeyExists(ctx, alias)
	if err != nil || !exists {
		return false, err
	}
	status, raw, err := c.do(ctx, http.MethodPost, "/key/delete", map[string]any{"key_aliases": []string{alias}})
	if err != nil {
		return true, err
	}
	if status != http.StatusOK && status != http.StatusNotFound {
		return true, fmt.Errorf("model gateway /key/delete answered %d: %s", status, truncate(raw, 200))
	}
	left, err := c.KeyExists(ctx, alias)
	if err != nil {
		return true, err
	}
	if left {
		return true, fmt.Errorf("the model gateway still lists the key %s after deleting it", alias)
	}
	return true, nil
}

// TeamID finds a team by its alias. found is false, with no error, when the
// gateway has no team of that alias.
func (c *Client) TeamID(ctx context.Context, alias string) (id string, found bool, err error) {
	status, raw, err := c.do(ctx, http.MethodGet, "/team/list", nil)
	if err != nil {
		return "", false, err
	}
	if status != http.StatusOK {
		return "", false, fmt.Errorf("model gateway /team/list answered %d: %s", status, truncate(raw, 200))
	}
	// A list of team objects; the gateway has changed the envelope between
	// versions, so decode loosely -- but an object has to carry a teams
	// array. Decoding into a struct would accept any object, an error body
	// included, and report a team that exists as absent.
	var teams []map[string]any
	if err := json.Unmarshal(raw, &teams); err != nil {
		var wrapped map[string]json.RawMessage
		if err2 := json.Unmarshal(raw, &wrapped); err2 != nil {
			return "", false, fmt.Errorf("model gateway /team/list: unrecognised response: %w", err)
		}
		inner, ok := wrapped["teams"]
		if !ok {
			return "", false, fmt.Errorf("model gateway /team/list: response has no team list: %s", truncate(raw, 200))
		}
		if err2 := json.Unmarshal(inner, &teams); err2 != nil {
			return "", false, fmt.Errorf("model gateway /team/list: teams is not a list: %w", err2)
		}
	}
	for _, t := range teams {
		if a, _ := t["team_alias"].(string); a == alias {
			id, _ := t["team_id"].(string)
			return id, true, nil
		}
	}
	return "", false, nil
}

// DeleteTeam removes the team of this alias, and reports whether there was
// one. Like DeleteKey it looks before and after.
func (c *Client) DeleteTeam(ctx context.Context, alias string) (existed bool, err error) {
	id, found, err := c.TeamID(ctx, alias)
	if err != nil || !found {
		return false, err
	}
	if id == "" {
		return true, fmt.Errorf("the model gateway lists the team %s without an id; it cannot be deleted by name", alias)
	}
	status, raw, err := c.do(ctx, http.MethodPost, "/team/delete", map[string]any{"team_ids": []string{id}})
	if err != nil {
		return true, err
	}
	if status != http.StatusOK && status != http.StatusNotFound {
		return true, fmt.Errorf("model gateway /team/delete answered %d: %s", status, truncate(raw, 200))
	}
	if _, left, err := c.TeamID(ctx, alias); err != nil {
		return true, err
	} else if left {
		return true, fmt.Errorf("the model gateway still lists the team %s after deleting it", alias)
	}
	return true, nil
}

func truncate(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "…"
}
