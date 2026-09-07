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

package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
)

const cloudflareAPIBase = "https://api.cloudflare.com/client/v4"

// CloudflareDNSWriter implements EdgeDNSWriter against Cloudflare's DNS record
// API. It knows about zones and records and nothing about tunnels: hand it a
// target and it writes it, whether that target came from a Cloudflare tunnel,
// a LoadBalancer address, or an exit node this platform has never heard of.
//
// Its credential is the DNS credential -- Zone -> Zone -> Read to find the
// zone and Zone -> DNS -> Edit to write in it. That is a different grant from
// the tunnel's, which is why it is a different field here rather than one
// token threaded through both halves. The same token may hold both, and on the
// common single-token deployment it does; that is a property of the deployment
// and not something this type assumes.
type CloudflareDNSWriter struct {
	token  string
	zoneID string
	http   *http.Client
}

// NewCloudflareDNSWriter builds the DNS half of a Cloudflare edge.
func NewCloudflareDNSWriter(token, zoneID string) *CloudflareDNSWriter {
	return &CloudflareDNSWriter{token: token, zoneID: zoneID, http: &http.Client{}}
}

// EnsureRecord implements EdgeDNSWriter.
func (c *CloudflareDNSWriter) EnsureRecord(ctx context.Context, hostname string, target EdgeTarget) error {
	if hostname == "" || target.IsZero() {
		return nil
	}
	existing, err := c.listRecords(ctx, hostname)
	if err != nil {
		return err
	}
	desired := cfDNSRecord{
		Type:    target.Type,
		Name:    hostname,
		Content: target.Value,
		Proxied: target.Proxied,
	}
	for _, r := range existing {
		if r.Type != target.Type {
			continue
		}
		if r.Content == target.Value && r.Proxied == target.Proxied {
			return nil // already correct
		}
		return c.updateRecord(ctx, r.ID, desired)
	}
	return c.createRecord(ctx, desired)
}

// DeleteRecord implements EdgeDNSWriter.
func (c *CloudflareDNSWriter) DeleteRecord(ctx context.Context, hostname string) error {
	return c.deleteRecords(ctx, hostname)
}

type cfDNSRecord struct {
	ID      string `json:"id,omitempty"`
	Type    string `json:"type"`
	Name    string `json:"name"`
	Content string `json:"content"`
	Proxied bool   `json:"proxied"`
}

type cfListResponse struct {
	Success bool          `json:"success"`
	Result  []cfDNSRecord `json:"result"`
	Errors  []cfError     `json:"errors"`
}

type cfCreateResponse struct {
	Success bool        `json:"success"`
	Result  cfDNSRecord `json:"result"`
	Errors  []cfError   `json:"errors"`
}

// deleteRecords deletes all records for hostname for hostname. Silently returns nil if
// no records exist.
func (c *CloudflareDNSWriter) deleteRecords(ctx context.Context, hostname string) error {
	records, err := c.listRecords(ctx, hostname)
	if err != nil {
		return err
	}
	for _, r := range records {
		if r.Type != "CNAME" {
			continue
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodDelete,
			fmt.Sprintf("%s/zones/%s/dns_records/%s", cloudflareAPIBase, c.zoneID, r.ID), nil)
		if err != nil {
			return err
		}
		c.setHeaders(req)
		resp, err := c.http.Do(req)
		if err != nil {
			return err
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
			return fmt.Errorf("delete DNS record %s: HTTP %d", r.ID, resp.StatusCode)
		}
	}
	return nil
}

func (c *CloudflareDNSWriter) listRecords(ctx context.Context, name string) ([]cfDNSRecord, error) {
	u := fmt.Sprintf("%s/zones/%s/dns_records?%s",
		cloudflareAPIBase, c.zoneID,
		url.Values{"name": {name}}.Encode())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	c.setHeaders(req)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	var result cfListResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("parse Cloudflare list response: %w", err)
	}
	if !result.Success {
		return nil, fmt.Errorf("cloudflare list DNS records: %v", result.Errors)
	}
	return result.Result, nil
}

// writeRecord is create and update: same body, same headers, same response
// shape, differing only in method and URL. They were two functions whose bodies
// matched line for line apart from the word "create"/"update" in one error
// string, which is how a fix to one of them misses the other.
func (c *CloudflareDNSWriter) writeRecord(ctx context.Context, method, url, verb string, rec cfDNSRecord) error {
	payload, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	c.setHeaders(req)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	var result cfCreateResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return fmt.Errorf("parse Cloudflare %s response: %w", verb, err)
	}
	if !result.Success {
		return fmt.Errorf("cloudflare %s DNS record: %v", verb, result.Errors)
	}
	return nil
}

func (c *CloudflareDNSWriter) createRecord(ctx context.Context, rec cfDNSRecord) error {
	return c.writeRecord(ctx, http.MethodPost,
		fmt.Sprintf("%s/zones/%s/dns_records", cloudflareAPIBase, c.zoneID), "create", rec)
}

func (c *CloudflareDNSWriter) updateRecord(ctx context.Context, id string, rec cfDNSRecord) error {
	return c.writeRecord(ctx, http.MethodPut,
		fmt.Sprintf("%s/zones/%s/dns_records/%s", cloudflareAPIBase, c.zoneID, id), "update", rec)
}

func (c *CloudflareDNSWriter) setHeaders(req *http.Request) {
	req.Header.Set("Authorization", "Bearer "+c.token)
}

type cfError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func formatCloudflareErrors(errors []cfError) error {
	if len(errors) == 0 {
		return fmt.Errorf("unknown error")
	}
	// 10000 is Cloudflare's generic authentication error; 1001 is what the
	// tunnel endpoints return for a token that authenticated but carries no
	// tunnel permission. Both mean the same thing to an operator, and 1001 is
	// the one a DNS-scoped token actually produces — it went without the hint
	// until a tenant deploy spent half an hour retrying "Not authorized" with
	// nothing to act on.
	//
	// The permission is named as the dashboard names it today. Cloudflare
	// folded tunnels into Cloudflare One and renamed it, so "Cloudflare Tunnel"
	// — what this said, and what the API still implies — appears nowhere in the
	// permission list an operator is reading. Sending someone to look for a
	// setting under a name it no longer has is the same defect as saying
	// nothing, so both names are given.
	if errors[0].Code == 10000 || errors[0].Code == 1001 {
		return fmt.Errorf("%v (grant Account → Cloudflare One Connector: cloudflared → Edit"+
			" — older accounts call it Cloudflare Tunnel — or set CLOUDFLARE_TUNNEL_API_TOKEN)", errors)
	}
	return fmt.Errorf("%v", errors)
}
