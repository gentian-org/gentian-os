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
*/package authz

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"reflect"
	"sort"
	"strings"
	"time"

	_ "embed"
)

// modelV1 is authz/model/v1/model.json, the model this director checks
// against. It is embedded rather than fetched so that the code and the model
// it was tested with cannot be separated.
//
//go:embed model.json
var modelV1 []byte

// StoreName is the OpenFGA store the platform uses. One per cluster.
const StoreName = "gentian"

// Bootstrap finds or creates the store and makes sure the embedded model is
// the one its decisions are made against. It returns the store and model ids
// to pin.
//
// This is the director's own work, not an installer step: the ids exist only
// after OpenFGA is answering, and a cluster rebuilt from git must arrive at
// the same place without anyone remembering to run something.
func Bootstrap(ctx context.Context, o Options) (storeID, modelID string, err error) {
	c, err := newClient(o)
	if err != nil {
		return "", "", err
	}
	storeID, err = c.ensureStore(ctx, StoreName)
	if err != nil {
		return "", "", err
	}
	modelID, err = c.ensureModel(ctx, storeID)
	if err != nil {
		return "", "", err
	}
	c.log.InfoContext(ctx, "authorization store ready", "store", storeID, "model", modelID)
	return storeID, modelID, nil
}

// newClient is a client for the server rather than for a store: bootstrap is
// what produces the store and model ids that NewOpenFGA insists on.
func newClient(o Options) (*OpenFGA, error) {
	if o.BaseURL == "" {
		return nil, errors.New("authz: OpenFGA URL is required")
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.Client == nil {
		o.Client = &http.Client{Timeout: 15 * time.Second}
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return &OpenFGA{
		baseURL: strings.TrimRight(o.BaseURL, "/"), token: o.APIToken,
		http: o.Client, log: o.Logger, now: o.Now,
	}, nil
}

type storeRow struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
}

// ensureStore returns the id of the store called name, creating it if there is
// none. Where several share the name — two replicas that started together —
// the oldest wins, so every replica converges on one answer rather than on
// whichever it created.
func (c *OpenFGA) ensureStore(ctx context.Context, name string) (string, error) {
	found, err := c.storesNamed(ctx, name)
	if err != nil {
		return "", err
	}
	if len(found) == 0 {
		var created storeRow
		if err := c.post(ctx, "", "/stores", map[string]string{"name": name}, &created); err != nil {
			return "", fmt.Errorf("create store %q: %w", name, err)
		}
		// Look again: another replica may have created one in the meantime,
		// and the oldest is the one everybody must use.
		found, err = c.storesNamed(ctx, name)
		if err != nil || len(found) == 0 {
			// The store this replica created is an answer; another listing
			// error must not undo a successful create.
			return created.ID, nil
		}
	}
	sort.Slice(found, func(i, j int) bool { return found[i].CreatedAt.Before(found[j].CreatedAt) })
	if len(found) > 1 {
		c.log.WarnContext(ctx, "several authorization stores carry this name; using the oldest",
			"name", name, "count", len(found), "store", found[0].ID)
	}
	return found[0].ID, nil
}

func (c *OpenFGA) storesNamed(ctx context.Context, name string) ([]storeRow, error) {
	var out []storeRow
	token := ""
	for {
		var page struct {
			Stores            []storeRow `json:"stores"`
			ContinuationToken string     `json:"continuation_token"`
		}
		path := "/stores?page_size=100"
		if token != "" {
			path += "&continuation_token=" + token
		}
		if err := c.get(ctx, path, &page); err != nil {
			return nil, fmt.Errorf("list stores: %w", err)
		}
		for _, s := range page.Stores {
			if s.Name == name {
				out = append(out, s)
			}
		}
		if token = page.ContinuationToken; token == "" {
			return out, nil
		}
	}
}

// ensureModel returns the id of the store's newest model when that model is
// already the embedded one, and writes it otherwise. Writing is idempotent in
// effect but not in fact — every write is a new version — so the comparison is
// what keeps a restart from filling the store with identical models.
func (c *OpenFGA) ensureModel(ctx context.Context, storeID string) (string, error) {
	var want map[string]any
	if err := json.Unmarshal(modelV1, &want); err != nil {
		return "", fmt.Errorf("embedded model: %w", err)
	}
	var latest struct {
		Models []map[string]any `json:"authorization_models"`
	}
	if err := c.get(ctx, "/stores/"+storeID+"/authorization-models?page_size=1", &latest); err != nil {
		return "", fmt.Errorf("read models: %w", err)
	}
	if len(latest.Models) == 1 && sameModel(latest.Models[0], want) {
		id, _ := latest.Models[0]["id"].(string)
		return id, nil
	}
	var written struct {
		ID string `json:"authorization_model_id"`
	}
	if err := c.post(ctx, storeID, "/authorization-models", want, &written); err != nil {
		return "", fmt.Errorf("write model: %w", err)
	}
	c.log.InfoContext(ctx, "authorization model written", "store", storeID, "model", written.ID)
	return written.ID, nil
}

// sameModel compares what the model says, not what the server added. OpenFGA
// echoes a model with its empty defaults filled in — "relations": {},
// "metadata": null, "generic_types": [] — and an id of its own, so a literal
// comparison calls every stored model different and every restart writes
// another version of the same thing.
func sameModel(got, want map[string]any) bool {
	for _, field := range []string{"type_definitions", "conditions"} {
		if !reflect.DeepEqual(normalise(got[field]), normalise(want[field])) {
			return false
		}
	}
	return true
}

// normalise drops what carries no meaning — null, {}, [] and "" — at every
// level, so a model and the server's echo of it compare alike. The server
// fills in its zero values: a relation with no condition comes back with
// "condition": "", a type with no module with "module": "". None of those is
// a value a model can carry deliberately: a condition or module named by the
// empty string does not exist.
func normalise(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			if n := normalise(val); n != nil {
				out[k] = n
			}
		}
		if len(out) == 0 {
			return nil
		}
		return out
	case []any:
		out := make([]any, 0, len(t))
		for _, val := range t {
			out = append(out, normalise(val))
		}
		if len(out) == 0 {
			return nil
		}
		return out
	case string:
		if t == "" {
			return nil
		}
		return t
	default:
		return v
	}
}
