/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

// Package modelgateway is the platform's model gateway (LiteLLM) as the
// operator administers it: the key each app of a tenant that declared the
// gateway calls models with, and the tenant's team.
//
// The names are here so that what registers a key and what removes it cannot
// spell it differently. The key itself is not a name: it is generated, held
// in the vault, and known to the gateway by its hash.
package modelgateway

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
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

// Port is the port the gateway's pods listen on, and its Service's. It is
// what an app is handed in its address, what the app's network policy opens
// in the gateway's namespace, and what the gateway's own NetworkPolicy admits
// its clients on.
const Port int32 = 4000

// ServiceName is the gateway's Service.
const ServiceName = "litellm-proxy"

// Namespace is where the gateway runs.
func Namespace() string { return layout.System("llm") }

// DefaultBaseURL is the gateway's address inside the cluster.
func DefaultBaseURL() string {
	return fmt.Sprintf("http://%s.%s.svc.cluster.local:%d", ServiceName, Namespace(), Port)
}

// OpenAIBaseURL is the address an app is handed: the gateway's
// OpenAI-compatible API.
func OpenAIBaseURL(gateway string) string { return gateway + "/v1" }

// KeyPrefix is what a key the gateway accepts begins with.
const KeyPrefix = "sk-"

// HashKey is the form the gateway keeps a key in and lists it by: the
// SHA-256 of the key, in hex. It is how a registered key is compared with
// the one an app holds without the gateway ever giving a key back.
func HashKey(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

// LegacyKey is the key the platform used to register for an app: text made
// from the tenant's and the app's names, which anybody who knew the two could
// write down. Nothing presents it any more. It is spelled here only so that
// one still registered can be recognised and removed.
func LegacyKey(tenant, app string) string { return fmt.Sprintf("sk-gentian-%s-%s", tenant, app) }

// KeyAlias is the name the gateway knows an app's key by. Aliases are unique at
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
	listed, err := c.listKeys(ctx, alias)
	return len(listed) > 0, err
}

// listKeys is what the gateway lists under an alias, undecoded.
func (c *Client) listKeys(ctx context.Context, alias string) ([]json.RawMessage, error) {
	status, raw, err := c.do(ctx, http.MethodGet, "/key/list?key_alias="+url.QueryEscape(alias), nil)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("model gateway /key/list answered %d: %s", status, truncate(raw, 200))
	}
	var listed struct {
		Keys *[]json.RawMessage `json:"keys"`
	}
	if err := json.Unmarshal(raw, &listed); err != nil || listed.Keys == nil {
		// An answer without a key list is not "no such key".
		return nil, fmt.Errorf("model gateway /key/list: the answer has no key list: %s", truncate(raw, 200))
	}
	return *listed.Keys, nil
}

// keyHashes is the keys registered under an alias, each in the hashed form
// the gateway lists it in: a bare string, or an object carrying it as its
// token. An entry that is neither is an error and not a mismatch -- a key
// that cannot be read must not be taken for the wrong key and replaced on
// every pass.
func (c *Client) keyHashes(ctx context.Context, alias string) ([]string, error) {
	listed, err := c.listKeys(ctx, alias)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(listed))
	for _, entry := range listed {
		var hash string
		if err := json.Unmarshal(entry, &hash); err != nil {
			var object struct {
				Token string `json:"token"`
			}
			if err := json.Unmarshal(entry, &object); err != nil {
				return nil, fmt.Errorf("model gateway /key/list: an entry for %s is neither a key hash nor a key object: %s", alias, truncate(entry, 80))
			}
			hash = object.Token
		}
		if !isKeyHash(hash) {
			return nil, fmt.Errorf("model gateway /key/list: an entry for %s carries no key hash, so the registered key cannot be compared", alias)
		}
		out = append(out, hash)
	}
	return out, nil
}

func isKeyHash(s string) bool {
	if len(s) != sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

// KeyIs reports whether key is registered under alias.
func (c *Client) KeyIs(ctx context.Context, alias, key string) (bool, error) {
	registered, err := c.keyHashes(ctx, alias)
	if err != nil {
		return false, err
	}
	return slices.Contains(registered, HashKey(key)), nil
}

// EnsureKey makes key the one key registered under alias, and reports
// whether another was registered there and has been replaced.
//
// An alias names one key at the gateway. When it already names this key,
// nothing is done. When it names another -- the key made from the tenant's
// and the app's names that the platform registered before keys were random
// -- that one is deleted and this one registered in its place: the old key
// stops authenticating in the same pass that the new one starts to.
func (c *Client) EnsureKey(ctx context.Context, alias, key string) (replaced bool, err error) {
	registered, err := c.keyHashes(ctx, alias)
	if err != nil {
		return false, err
	}
	if len(registered) == 1 && registered[0] == HashKey(key) {
		return false, nil
	}
	if len(registered) > 0 {
		if _, err := c.DeleteKey(ctx, alias); err != nil {
			return false, fmt.Errorf("replace the key registered as %s: %w", alias, err)
		}
		replaced = true
	}
	status, raw, err := c.do(ctx, http.MethodPost, "/key/generate", map[string]any{"key": key, "key_alias": alias})
	if err != nil {
		return replaced, err
	}
	if status != http.StatusOK {
		return replaced, fmt.Errorf("model gateway /key/generate answered %d: %s", status, truncate(raw, 200))
	}
	// Registered when the gateway lists it, whatever the call answered.
	now, err := c.keyHashes(ctx, alias)
	if err != nil {
		return replaced, err
	}
	if !slices.Contains(now, HashKey(key)) {
		return replaced, fmt.Errorf("the model gateway does not list the key %s after registering it", alias)
	}
	return replaced, nil
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
