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
// A tenant's team names the models its apps may use, by the access groups
// the gateway's configuration puts every model in
// (kernel/services/llm/manifests/templates/gateway-config.yaml), and carries
// the tenant's budget. Every key of the tenant belongs to the team and names
// the same models, so a request -- and every fallback it names -- is held to
// them by the key and by the team.
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
	"sort"
	"strings"
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

// KeyAlias is the name the gateway knows an app's key by. Aliases are unique at
// the gateway, which is what lets a key be found and removed by name.
func KeyAlias(tenant, app string) string { return fmt.Sprintf("%s-%s", tenant, app) }

// TeamAlias is the name of a tenant's team.
func TeamAlias(tenant string) string { return tenant }

// EveryTenantGroup is the access group of the models every tenant may use:
// the ones the cluster serves itself. A provider's models are in its own
// group, which a team names only for a tenant that opted in to it.
const EveryTenantGroup = "gentian:every-tenant"

// ProviderGroup is the access group of an external provider's models.
func ProviderGroup(provider string) string { return "gentian:provider:" + provider }

// NoModels is what a team or key names when it may use no model: the
// gateway reads an empty list as every model, and this name as none.
const NoModels = "no-default-models"

// AppRoutes are the routes of the gateway an app's key may call: the
// OpenAI API's chat completions, embeddings and model list, with and
// without the /v1 prefix. No passthrough to a provider, no files, batches
// or assistants, and none of the gateway's administration.
var AppRoutes = []string{
	"/v1/chat/completions", "/chat/completions",
	"/v1/embeddings", "/embeddings",
	"/v1/models", "/models",
}

// ProviderKeyProperty is the property of the shared provider credential a
// provider's token is read from; the gateway's chart offers no model of a
// provider that names another.
func ProviderKeyProperty(provider string) string {
	return strings.ReplaceAll(provider, "-", "_") + "_api_key"
}

// ClaimModels is what the Cluster claim's spec.llm says about the models
// the gateway's configuration file lists.
type ClaimModels struct {
	GPUAcceleration bool `json:"gpuAcceleration"`
	Instances       []struct {
		Name string `json:"name"`
	} `json:"instances"`
	Providers []struct {
		Name           string `json:"name"`
		APIKeyProperty string `json:"apiKeyProperty"`
		Models         []struct {
			Name string `json:"name"`
		} `json:"models"`
	} `json:"providers"`
}

// ServedGroups are the access groups that hold at least one model, read the
// way the gateway's chart writes its configuration from the claim: an
// instance only with gpuAcceleration, a provider only with models and its
// own token property.
func (c ClaimModels) ServedGroups() map[string]bool {
	out := map[string]bool{}
	if c.GPUAcceleration && len(c.Instances) > 0 {
		out[EveryTenantGroup] = true
	}
	for _, p := range c.Providers {
		if len(p.Models) == 0 || p.APIKeyProperty != ProviderKeyProperty(p.Name) {
			continue
		}
		out[ProviderGroup(p.Name)] = true
	}
	return out
}

// TeamModels is what a tenant's team names: the models every tenant may use
// and those of the providers the tenant opted in to, as access groups. With
// served given, a group that holds no model is left out, so that the
// gateway does not list its name as a model; NoModels when nothing is left.
func TeamModels(providers []string, served map[string]bool) []string {
	want := []string{EveryTenantGroup}
	named := append([]string(nil), providers...)
	sort.Strings(named)
	for _, p := range named {
		if g := ProviderGroup(p); !slices.Contains(want, g) {
			want = append(want, g)
		}
	}
	out := []string{}
	for _, g := range want {
		if served == nil || served[g] {
			out = append(out, g)
		}
	}
	if len(out) == 0 {
		return []string{NoModels}
	}
	return out
}

// Team is what a tenant's team at the gateway carries: the models its apps
// may use and its limits. A nil limit is none.
type Team struct {
	Models         []string
	MaxBudget      *int64
	BudgetDuration string
	RPMLimit       *int64
	TPMLimit       *int64
}

func (t Team) body() map[string]any {
	models := t.Models
	if len(models) == 0 {
		// Never an empty list: the gateway reads it as every model.
		models = []string{NoModels}
	}
	body := map[string]any{"models": models, "max_budget": nil, "budget_duration": nil, "rpm_limit": nil, "tpm_limit": nil}
	if t.MaxBudget != nil {
		body["max_budget"] = *t.MaxBudget
	}
	if t.BudgetDuration != "" {
		body["budget_duration"] = t.BudgetDuration
	}
	if t.RPMLimit != nil {
		body["rpm_limit"] = *t.RPMLimit
	}
	if t.TPMLimit != nil {
		body["tpm_limit"] = *t.TPMLimit
	}
	return body
}

// KeySpec is what an app's key carries besides the key: the team it belongs
// to, the same models as the team, the routes it may call and whose it is.
type KeySpec struct {
	TeamID   string
	Models   []string
	Routes   []string
	Metadata map[string]string
}

func (k KeySpec) body() map[string]any {
	models := k.Models
	if len(models) == 0 {
		models = []string{NoModels}
	}
	return map[string]any{"team_id": k.TeamID, "models": models, "allowed_routes": k.Routes, "metadata": k.Metadata}
}

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

// registeredKey is a key the gateway lists under an alias: its hash, and
// what it carries when the gateway lists the whole key.
type registeredKey struct {
	Token         string         `json:"token"`
	TeamID        *string        `json:"team_id"`
	Models        []string       `json:"models"`
	AllowedRoutes []string       `json:"allowed_routes"`
	Metadata      map[string]any `json:"metadata"`
	whole         bool
}

// carries reports whether the key carries what spec says. A key listed by
// its hash alone does not say, and is taken to carry nothing.
func (k registeredKey) carries(spec KeySpec) bool {
	if !k.whole || k.TeamID == nil || *k.TeamID != spec.TeamID {
		return false
	}
	want := spec.body()
	if !sameSet(k.Models, want["models"].([]string)) || !sameSet(k.AllowedRoutes, spec.Routes) {
		return false
	}
	for name, value := range spec.Metadata {
		if have, _ := k.Metadata[name].(string); have != value {
			return false
		}
	}
	return true
}

func sameSet(a, b []string) bool {
	x, y := slices.Clone(a), slices.Clone(b)
	sort.Strings(x)
	sort.Strings(y)
	return slices.Equal(slices.Compact(x), slices.Compact(y))
}

// registeredKeys is the keys registered under an alias, each with the hash
// the gateway lists it by: a bare string, or the whole key with its hash as
// its token. An entry that is neither is an error and not a mismatch -- a
// key that cannot be read must not be taken for the wrong key and replaced
// on every pass.
func (c *Client) registeredKeys(ctx context.Context, alias string) ([]registeredKey, error) {
	status, raw, err := c.do(ctx, http.MethodGet, "/key/list?return_full_object=true&key_alias="+url.QueryEscape(alias), nil)
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
		return nil, fmt.Errorf("model gateway /key/list: the answer has no key list: %s", truncate(raw, 200))
	}
	out := make([]registeredKey, 0, len(*listed.Keys))
	for _, entry := range *listed.Keys {
		var key registeredKey
		if err := json.Unmarshal(entry, &key.Token); err != nil {
			if err := json.Unmarshal(entry, &key); err != nil {
				return nil, fmt.Errorf("model gateway /key/list: an entry for %s is neither a key hash nor a key object: %s", alias, truncate(entry, 80))
			}
			key.whole = true
		}
		if !isKeyHash(key.Token) {
			return nil, fmt.Errorf("model gateway /key/list: an entry for %s carries no key hash, so the registered key cannot be compared", alias)
		}
		out = append(out, key)
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

// EnsureKey makes key the one key registered under alias, carrying what spec
// says, and reports whether another was registered there and has been
// replaced.
//
// An alias names one key at the gateway. When it already names this key,
// what the key carries is brought to spec in place (/key/update), and
// nothing is done when it carries it already. When it names another, that
// one is deleted and this one registered in its place: the old key stops
// authenticating in the same pass that the new one starts to.
func (c *Client) EnsureKey(ctx context.Context, alias, key string, spec KeySpec) (replaced bool, err error) {
	registered, err := c.registeredKeys(ctx, alias)
	if err != nil {
		return false, err
	}
	if len(registered) == 1 && registered[0].Token == HashKey(key) {
		if registered[0].carries(spec) {
			return false, nil
		}
		body := spec.body()
		body["key"] = key
		status, raw, err := c.do(ctx, http.MethodPost, "/key/update", body)
		if err != nil {
			return false, err
		}
		if status != http.StatusOK {
			return false, fmt.Errorf("model gateway /key/update answered %d: %s", status, truncate(raw, 200))
		}
		return false, c.keyCarries(ctx, alias, key, spec)
	}
	if len(registered) > 0 {
		if _, err := c.DeleteKey(ctx, alias); err != nil {
			return false, fmt.Errorf("replace the key registered as %s: %w", alias, err)
		}
		replaced = true
	}
	body := spec.body()
	body["key"], body["key_alias"] = key, alias
	status, raw, err := c.do(ctx, http.MethodPost, "/key/generate", body)
	if err != nil {
		return replaced, err
	}
	if status != http.StatusOK {
		return replaced, fmt.Errorf("model gateway /key/generate answered %d: %s", status, truncate(raw, 200))
	}
	return replaced, c.keyCarries(ctx, alias, key, spec)
}

// keyCarries checks, after a write, that the gateway lists the key under the
// alias carrying what spec says, whatever the call answered.
func (c *Client) keyCarries(ctx context.Context, alias, key string, spec KeySpec) error {
	now, err := c.registeredKeys(ctx, alias)
	if err != nil {
		return err
	}
	for _, k := range now {
		if k.Token != HashKey(key) {
			continue
		}
		if k.whole && !k.carries(spec) {
			return fmt.Errorf("the model gateway lists the key %s without the team, models, routes or owner it was given", alias)
		}
		return nil
	}
	return fmt.Errorf("the model gateway does not list the key %s after registering it", alias)
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
	team, err := c.team(ctx, alias)
	if err != nil || team == nil {
		return "", false, err
	}
	id, _ = team["team_id"].(string)
	return id, true, nil
}

// EnsureTeam makes the team of this alias carry what want says, creating it
// when the gateway has none, and answers its id. A team that carries it
// already is not written.
func (c *Client) EnsureTeam(ctx context.Context, alias string, want Team) (id string, err error) {
	team, err := c.team(ctx, alias)
	if err != nil {
		return "", err
	}
	body := want.body()
	path := "/team/new"
	if team != nil {
		id, _ = team["team_id"].(string)
		if id == "" {
			return "", fmt.Errorf("the model gateway lists the team %s without an id", alias)
		}
		if teamCarries(team, body) {
			return id, nil
		}
		path = "/team/update"
		body["team_id"] = id
	} else {
		body["team_alias"] = alias
	}
	status, raw, err := c.do(ctx, http.MethodPost, path, body)
	if err != nil {
		return "", err
	}
	if status != http.StatusOK {
		return "", fmt.Errorf("model gateway %s answered %d: %s", path, status, truncate(raw, 200))
	}
	// Written when the gateway lists it so, whatever the call answered.
	team, err = c.team(ctx, alias)
	if err != nil {
		return "", err
	}
	if team == nil {
		return "", fmt.Errorf("the model gateway does not list the team %s after %s", alias, path)
	}
	if !teamCarries(team, want.body()) {
		return "", fmt.Errorf("the model gateway lists the team %s without the models or limits it was given", alias)
	}
	id, _ = team["team_id"].(string)
	if id == "" {
		return "", fmt.Errorf("the model gateway lists the team %s without an id", alias)
	}
	return id, nil
}

// teamCarries compares a listed team with the fields of a write.
func teamCarries(team, body map[string]any) bool {
	var models []string
	if listed, ok := team["models"].([]any); ok {
		for _, m := range listed {
			if name, ok := m.(string); ok {
				models = append(models, name)
			}
		}
	}
	if !sameSet(models, body["models"].([]string)) {
		return false
	}
	number := func(v any) (float64, bool) {
		switch n := v.(type) {
		case float64:
			return n, true
		case int64:
			return float64(n), true
		}
		return 0, false
	}
	for _, field := range []string{"max_budget", "rpm_limit", "tpm_limit"} {
		have, hasHave := number(team[field])
		want, hasWant := number(body[field])
		if hasHave != hasWant || have != want {
			return false
		}
	}
	have, _ := team["budget_duration"].(string)
	want, _ := body["budget_duration"].(string)
	return have == want
}

// team is the team of this alias as the gateway lists it, or nil.
func (c *Client) team(ctx context.Context, alias string) (map[string]any, error) {
	status, raw, err := c.do(ctx, http.MethodGet, "/team/list", nil)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("model gateway /team/list answered %d: %s", status, truncate(raw, 200))
	}
	// A list of team objects; the gateway has changed the envelope between
	// versions, so decode loosely -- but an object has to carry a teams
	// array. Decoding into a struct would accept any object, an error body
	// included, and report a team that exists as absent.
	var teams []map[string]any
	if err := json.Unmarshal(raw, &teams); err != nil {
		var wrapped map[string]json.RawMessage
		if err2 := json.Unmarshal(raw, &wrapped); err2 != nil {
			return nil, fmt.Errorf("model gateway /team/list: unrecognized response: %w", err)
		}
		inner, ok := wrapped["teams"]
		if !ok {
			return nil, fmt.Errorf("model gateway /team/list: response has no team list: %s", truncate(raw, 200))
		}
		if err2 := json.Unmarshal(inner, &teams); err2 != nil {
			return nil, fmt.Errorf("model gateway /team/list: teams is not a list: %w", err2)
		}
	}
	for _, t := range teams {
		if a, _ := t["team_alias"].(string); a == alias {
			return t, nil
		}
	}
	return nil, nil
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
