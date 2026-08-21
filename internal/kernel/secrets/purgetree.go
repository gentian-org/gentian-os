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
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// DeleteTree removes a KV v2 subtree, metadata and all versions.
//
// It exists because tenant deletion left the tenant's OpenBao paths behind. Only
// per-app subtrees were ever purged, so …/tenants/<t>/admin survived a delete —
// and because the admin credential is seeded write-once, a tenant recreated
// under the same name silently inherited the old one. "Delete it and make it
// again" did not do what it appears to.
//
// Over the operator's own authenticated session rather than by exec-ing into the
// OpenBao pod. The pod-exec version of this addressed the listener over http,
// which it has never spoken, and every command in it ended in `|| true` — so it
// reported success while deleting nothing.
//
// metadata delete, not data delete: the latter soft-deletes a version and leaves
// the secret readable by version, which is not what a caller asking to purge a
// tenant means.
func (c *KVClient) DeleteTree(ctx context.Context, logicalPath string) error {
	// Whitespace first: " " trimmed of slashes is still " ", which is not empty
	// and would be sent as a path.
	logicalPath = strings.Trim(strings.TrimSpace(logicalPath), "/ ")
	if logicalPath == "" {
		// Refusing this is the whole safety property: an empty path would walk
		// the mount and delete every secret in it.
		return fmt.Errorf("refusing to delete an empty KV path")
	}
	return c.deleteTree(ctx, logicalPath, 0)
}

// maxPurgeDepth bounds the walk. A cycle is not possible in KV, but a bug that
// appends rather than descends would otherwise recurse until the stack goes.
const maxPurgeDepth = 16

func (c *KVClient) deleteTree(ctx context.Context, path string, depth int) error {
	if depth > maxPurgeDepth {
		return fmt.Errorf("KV tree deeper than %d below %q; refusing to continue", maxPurgeDepth, path)
	}
	children, err := c.listChildren(ctx, path)
	if err != nil {
		return err
	}
	for _, child := range children {
		if strings.HasSuffix(child, "/") {
			if err := c.deleteTree(ctx, path+"/"+strings.TrimSuffix(child, "/"), depth+1); err != nil {
				return err
			}
			continue
		}
		if err := c.deleteMetadata(ctx, path+"/"+child); err != nil {
			return err
		}
	}
	// The node itself may hold a secret as well as children.
	return c.deleteMetadata(ctx, path)
}

func (c *KVClient) listChildren(ctx context.Context, path string) ([]string, error) {
	tok, err := c.authToken(ctx)
	if err != nil {
		return nil, err
	}
	url := fmt.Sprintf("%s/v1/%s/metadata/%s?list=true", c.addr, c.mount, path)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Vault-Token", tok)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("openbao unreachable: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotFound {
		return nil, nil // nothing there is a successful purge
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("list %s: HTTP %d: %s", path, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var out struct {
		Data struct {
			Keys []string `json:"keys"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("list %s: %w", path, err)
	}
	return out.Data.Keys, nil
}

func (c *KVClient) deleteMetadata(ctx context.Context, path string) error {
	tok, err := c.authToken(ctx)
	if err != nil {
		return err
	}
	url := fmt.Sprintf("%s/v1/%s/metadata/%s", c.addr, c.mount, path)
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("X-Vault-Token", tok)
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("openbao unreachable: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotFound || (resp.StatusCode >= 200 && resp.StatusCode <= 299) {
		return nil
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	return fmt.Errorf("delete %s: HTTP %d: %s", path, resp.StatusCode, strings.TrimSpace(string(body)))
}
