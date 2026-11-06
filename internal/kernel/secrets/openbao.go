/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package secrets

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// KVClient is the minimal OpenBao KV v2 client used by the Seeder.
//
// It logs in as the pod it runs in (Login); no static long-lived token is
// needed anywhere in the cluster.
type KVClient struct {
	*Login
	mount string
}

// NewKVClient constructs a KVClient that logs in to OpenBao at addr as role
// with the projected ServiceAccount token at saTokenPath ("" → default
// /var/run/...), verifying the vault as TransportFromEnv does.
func NewKVClient(addr, role, saTokenPath string) *KVClient {
	return &KVClient{Login: NewLogin(addr, role, saTokenPath, TransportFromEnv()), mount: Mount}
}

// refused reports an answer that refuses the token itself. OpenBao answers
// 403 for a token it does not know as well as for a path the policy denies,
// so both are taken as possibly the token's.
func refused(status int) bool {
	return status == http.StatusUnauthorized || status == http.StatusForbidden
}

// send makes one request to url with the client's token. The vault refuses a
// token it issued once it has revoked it or restarted, and a cached token
// would go on being refused until it expired: a refused token is dropped and
// the request made once more with a fresh login. The caller closes the
// response's body.
func (c *KVClient) send(ctx context.Context, method, url string, body []byte, contentType string) (*http.Response, error) {
	for retried := false; ; retried = true {
		tok, err := c.Token(ctx)
		if err != nil {
			return nil, err
		}
		var reader io.Reader
		if body != nil {
			reader = bytes.NewReader(body)
		}
		req, err := http.NewRequestWithContext(ctx, method, url, reader)
		if err != nil {
			return nil, err
		}
		req.Header.Set("X-Vault-Token", tok)
		if contentType != "" {
			req.Header.Set("Content-Type", contentType)
		}
		resp, err := c.HTTP.Do(req)
		if err != nil {
			return nil, err
		}
		if retried || !refused(resp.StatusCode) || !c.Forget(tok) {
			return resp, nil
		}
		_ = resp.Body.Close()
	}
}

// Exists reports whether a secret already exists at the given KV v2 logical
// path (without the "data/" prefix — e.g. "gentian-os/tenants/t/apps/a/oidc").
func (c *KVClient) Exists(ctx context.Context, logicalPath string) (bool, error) {
	resp, err := c.send(ctx, http.MethodGet, fmt.Sprintf("%s/v1/%s/data/%s", c.Addr, c.mount, logicalPath), nil, "")
	if err != nil {
		return false, err
	}
	defer func() { _ = resp.Body.Close() }()
	switch resp.StatusCode {
	case http.StatusOK:
		return true, nil
	case http.StatusNotFound:
		return false, nil
	default:
		body, _ := io.ReadAll(resp.Body)
		return false, fmt.Errorf("openbao exists %s: HTTP %d: %s", logicalPath, resp.StatusCode, string(body))
	}
}

// PutOnce writes data to logicalPath only if no version exists yet (KV v2
// check-and-set with cas=0). A subsequent rotation cannot silently overwrite
// live credentials.
func (c *KVClient) PutOnce(ctx context.Context, logicalPath string, data map[string]string) error {
	return c.put(ctx, logicalPath, data, 0, true)
}

// Put writes data unconditionally. Intended for explicit rotation paths.
func (c *KVClient) Put(ctx context.Context, logicalPath string, data map[string]string) error {
	return c.put(ctx, logicalPath, data, -1, false)
}

func (c *KVClient) put(ctx context.Context, logicalPath string, data map[string]string, cas int, withCAS bool) error {
	body := map[string]any{"data": data}
	if withCAS {
		body["options"] = map[string]any{"cas": cas}
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	resp, err := c.send(ctx, http.MethodPost, fmt.Sprintf("%s/v1/%s/data/%s", c.Addr, c.mount, logicalPath), raw, "application/json")
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusNoContent {
		return nil
	}
	// cas=0 against an existing secret returns 400 with check-and-set message
	// — treat that as success for PutOnce semantics.
	payload, _ := io.ReadAll(resp.Body)
	if withCAS && resp.StatusCode == http.StatusBadRequest &&
		strings.Contains(string(payload), "check-and-set") {
		return nil
	}
	return fmt.Errorf("openbao put %s: HTTP %d: %s", logicalPath, resp.StatusCode, string(payload))
}

// Request sends one authenticated request to path under /v1/ and answers the
// status and the body, for what the KV methods do not cover. A body is sent
// as JSON, or as a JSON merge patch for PATCH.
func (c *KVClient) Request(ctx context.Context, method, path string, body any) (int, []byte, error) {
	var raw []byte
	if body != nil {
		var err error
		if raw, err = json.Marshal(body); err != nil {
			return 0, nil, err
		}
	}
	contentType := "application/json"
	if method == http.MethodPatch {
		contentType = "application/merge-patch+json"
	}
	resp, err := c.send(ctx, method, fmt.Sprintf("%s/v1/%s", c.Addr, path), raw, contentType)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	answer, err := io.ReadAll(resp.Body)
	return resp.StatusCode, answer, err
}

// RecordMetadata is what the vault says of a KV v2 record without its
// values: when the record was made (new when it is deleted and written
// again) and its current version (one more on every write).
type RecordMetadata struct {
	CreatedTime    string
	CurrentVersion int
}

// Metadata reads the KV v2 metadata of the record at logicalPath. A path that
// holds nothing is ErrNotFound.
func (c *KVClient) Metadata(ctx context.Context, logicalPath string) (RecordMetadata, error) {
	status, body, err := c.Request(ctx, http.MethodGet, c.mount+"/metadata/"+logicalPath, nil)
	if err != nil {
		return RecordMetadata{}, err
	}
	switch status {
	case http.StatusOK:
	case http.StatusNotFound:
		return RecordMetadata{}, fmt.Errorf("openbao metadata %s: %w", logicalPath, ErrNotFound)
	default:
		return RecordMetadata{}, fmt.Errorf("openbao metadata %s: HTTP %d: %s", logicalPath, status, string(body))
	}
	var out struct {
		Data struct {
			CreatedTime    string `json:"created_time"`
			CurrentVersion int    `json:"current_version"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return RecordMetadata{}, fmt.Errorf("openbao metadata %s: decode: %w", logicalPath, err)
	}
	if out.Data.CreatedTime == "" || out.Data.CurrentVersion == 0 {
		return RecordMetadata{}, fmt.Errorf("openbao metadata %s: no created_time or current_version", logicalPath)
	}
	return RecordMetadata{CreatedTime: out.Data.CreatedTime, CurrentVersion: out.Data.CurrentVersion}, nil
}

// ErrNotFound is a path that holds nothing.
var ErrNotFound = errors.New("not found")

// Get reads the secret data at logicalPath. A path that holds nothing is
// ErrNotFound.
func (c *KVClient) Get(ctx context.Context, logicalPath string) (map[string]string, error) {
	resp, err := c.send(ctx, http.MethodGet, fmt.Sprintf("%s/v1/%s/data/%s", c.Addr, c.mount, logicalPath), nil, "")
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("openbao get %s: %w", logicalPath, ErrNotFound)
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("openbao get %s: HTTP %d: %s", logicalPath, resp.StatusCode, string(body))
	}
	var out struct {
		Data struct {
			Data map[string]string `json:"data"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("openbao get %s: decode: %w", logicalPath, err)
	}
	return out.Data.Data, nil
}
