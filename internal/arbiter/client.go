/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package arbiter

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// PlatformTokenFile is where the operator's token for the platform path is
// projected (charts/gentian-os, the operator's Deployment).
const PlatformTokenFile = "/var/run/secrets/gentian/arbiter/token"

// PlatformClient is the operator's side of the platform path.
type PlatformClient struct {
	// BaseURL is the arbiter's address.
	BaseURL string
	// TokenFile is read on every call: the kubelet replaces the token
	// before it expires.
	TokenFile string
	HTTP      *http.Client
}

// NewPlatformClient is the client of the arbiter of this cluster.
func NewPlatformClient() *PlatformClient {
	return &PlatformClient{
		BaseURL:   fmt.Sprintf("http://%s.%s.svc:%d", ServiceName, Namespace, Port),
		TokenFile: PlatformTokenFile,
		HTTP:      &http.Client{Timeout: 60 * time.Second},
	}
}

// ErrNoStore is an export of a store that was never made.
var ErrNoStore = fmt.Errorf("no store is recorded")

func (c *PlatformClient) call(ctx context.Context, method, path string, body, out any) (int, error) {
	token, err := os.ReadFile(c.TokenFile)
	if err != nil {
		return 0, fmt.Errorf("the platform's token for the arbiter: %w", err)
	}
	var payload io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return 0, err
		}
		payload = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(c.BaseURL, "/")+path, payload)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(token)))
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return 0, fmt.Errorf("arbiter %s: %w", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return resp.StatusCode, fmt.Errorf("arbiter %s: status %d: %s", path, resp.StatusCode, strings.TrimSpace(string(b)))
	}
	if out != nil {
		return resp.StatusCode, json.NewDecoder(resp.Body).Decode(out)
	}
	return resp.StatusCode, nil
}

// SetMemberships states one person's groups in a realm, for the mirroring
// stores of that realm's tenant.
func (c *PlatformClient) SetMemberships(ctx context.Context, req MembershipsRequest) error {
	_, err := c.call(ctx, http.MethodPut, "/v1/platform/memberships", req, nil)
	return err
}

// GroupDeleted says a group of a realm is gone.
func (c *PlatformClient) GroupDeleted(ctx context.Context, req GroupDeletedRequest) error {
	_, err := c.call(ctx, http.MethodPost, "/v1/platform/group-deleted", req, nil)
	return err
}

// Export is a store's model and tuples; ErrNoStore when none was made.
func (c *PlatformClient) Export(ctx context.Context, tenant, component string) (*StoreExport, error) {
	out := &StoreExport{}
	code, err := c.call(ctx, http.MethodGet, "/v1/platform/stores/"+tenant+"/"+component, nil, out)
	if code == http.StatusNotFound {
		return nil, ErrNoStore
	}
	return out, err
}

// Restore puts a store back, fresh, from what a bundle carried.
func (c *PlatformClient) Restore(ctx context.Context, tenant, component string, held *StoreExport) error {
	_, err := c.call(ctx, http.MethodPut, "/v1/platform/stores/"+tenant+"/"+component, held, nil)
	return err
}

// Purge deletes a store.
func (c *PlatformClient) Purge(ctx context.Context, tenant, component string) error {
	_, err := c.call(ctx, http.MethodDelete, "/v1/platform/stores/"+tenant+"/"+component, nil, nil)
	return err
}

// PurgeTenant deletes every store of a tenant.
func (c *PlatformClient) PurgeTenant(ctx context.Context, tenant string) error {
	_, err := c.call(ctx, http.MethodDelete, "/v1/platform/tenants/"+tenant, nil, nil)
	return err
}

// StoreRecordKey is the key a store's ids are recorded under in
// StoresConfigMap, for a reader of the record.
func StoreRecordKey(tenant, component string) string {
	return Entry{Tenant: tenant, Component: component}.recordKey()
}
