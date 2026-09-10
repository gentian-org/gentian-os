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
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// Default location of the projected ServiceAccount token inside any pod.
const defaultSATokenPath = "/var/run/secrets/kubernetes.io/serviceaccount/token"

// KVClient is the minimal OpenBao KV v2 client used by the Seeder.
//
// Authentication mirrors the rest of the platform: it logs in via
// auth/kubernetes/login using the projected ServiceAccount token of the pod
// it runs in. The resulting client_token is cached and refreshed before its
// TTL expires. No static long-lived token is needed anywhere in the cluster.
type KVClient struct {
	addr        string
	mount       string
	authPath    string // "kubernetes" by default
	role        string // K8s auth role (e.g. "gentian-os-operator")
	saTokenPath string // path to the projected SA JWT
	http        *http.Client

	mu       sync.Mutex
	token    string
	tokenExp time.Time
}

// NewKVClient constructs a KVClient that authenticates to OpenBao via the
// Kubernetes auth method using role and the projected ServiceAccount token
// at saTokenPath ("" → default /var/run/...).
func NewKVClient(addr, role, saTokenPath string) *KVClient {
	if saTokenPath == "" {
		saTokenPath = defaultSATokenPath
	}
	var tr http.RoundTripper
	if os.Getenv("BAO_SKIP_VERIFY") == "true" || os.Getenv("VAULT_SKIP_VERIFY") == "true" {
		tr = &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		}
	}
	return &KVClient{
		addr:        strings.TrimRight(addr, "/"),
		mount:       Mount,
		authPath:    "kubernetes",
		role:        role,
		saTokenPath: saTokenPath,
		http:        &http.Client{Timeout: 10 * time.Second, Transport: tr},
	}
}

// authToken returns a valid OpenBao token, refreshing via Kubernetes auth
// when no token is cached or the cached one is within 60s of expiry.
func (c *KVClient) authToken(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token != "" && time.Until(c.tokenExp) > 60*time.Second {
		return c.token, nil
	}

	jwt, err := os.ReadFile(c.saTokenPath)
	if err != nil {
		return "", fmt.Errorf("read SA token %s: %w", c.saTokenPath, err)
	}
	body, _ := json.Marshal(map[string]string{
		"role": c.role,
		"jwt":  strings.TrimSpace(string(jwt)),
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		fmt.Sprintf("%s/v1/auth/%s/login", c.addr, c.authPath),
		bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("openbao kubernetes login: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("openbao kubernetes login: HTTP %d: %s", resp.StatusCode, string(raw))
	}
	var out struct {
		Auth struct {
			ClientToken   string `json:"client_token"`
			LeaseDuration int    `json:"lease_duration"`
		} `json:"auth"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("openbao kubernetes login: decode: %w", err)
	}
	if out.Auth.ClientToken == "" {
		return "", fmt.Errorf("openbao kubernetes login: empty client_token")
	}
	c.token = out.Auth.ClientToken
	if out.Auth.LeaseDuration > 0 {
		c.tokenExp = time.Now().Add(time.Duration(out.Auth.LeaseDuration) * time.Second)
	} else {
		c.tokenExp = time.Now().Add(time.Hour)
	}
	return c.token, nil
}

// SetStaticToken bypasses Kubernetes auth and uses tok for every request.
// Intended for unit tests against a fake OpenBao; production code paths
// should always rely on the role-based login flow.
func (c *KVClient) SetStaticToken(tok string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.token = tok
	c.tokenExp = time.Now().Add(24 * time.Hour)
}

// Exists reports whether a secret already exists at the given KV v2 logical
// path (without the "data/" prefix — e.g. "gentian-os/tenants/t/apps/a/oidc").
func (c *KVClient) Exists(ctx context.Context, logicalPath string) (bool, error) {
	tok, err := c.authToken(ctx)
	if err != nil {
		return false, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		fmt.Sprintf("%s/v1/%s/data/%s", c.addr, c.mount, logicalPath), nil)
	if err != nil {
		return false, err
	}
	req.Header.Set("X-Vault-Token", tok)
	resp, err := c.http.Do(req)
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
	tok, err := c.authToken(ctx)
	if err != nil {
		return err
	}
	body := map[string]any{"data": data}
	if withCAS {
		body["options"] = map[string]any{"cas": cas}
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		fmt.Sprintf("%s/v1/%s/data/%s", c.addr, c.mount, logicalPath), bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("X-Vault-Token", tok)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
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

// Get reads the secret data at logicalPath.
func (c *KVClient) Get(ctx context.Context, logicalPath string) (map[string]string, error) {
	tok, err := c.authToken(ctx)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		fmt.Sprintf("%s/v1/%s/data/%s", c.addr, c.mount, logicalPath), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Vault-Token", tok)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("openbao get %s: not found", logicalPath)
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
