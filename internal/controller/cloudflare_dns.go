// Copyright 2026 The Gentian Authors. Licensed under Apache 2.0.

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

// CloudflareDNSClient is a minimal client for managing Cloudflare DNS records.
// It only supports CNAME create/ensure/delete operations, which is all the
// operator needs for per-tenant app-hostname records.
type CloudflareDNSClient struct {
	token       string
	zoneID      string
	tunnelCNAME string // e.g. <uuid>.cfargotunnel.com
	http        *http.Client
}

// NewCloudflareDNSClient creates a new CloudflareDNSClient with the given credentials.
func NewCloudflareDNSClient(token, zoneID, tunnelCNAME string) *CloudflareDNSClient {
	return &CloudflareDNSClient{
		token:       token,
		zoneID:      zoneID,
		tunnelCNAME: tunnelCNAME,
		http:        &http.Client{},
	}
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

type cfError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// ensureCNAME creates a proxied CNAME record pointing hostname → target if one
// does not already exist. Idempotent: if a record with the exact same content
// already exists, it is left unchanged.
func (c *CloudflareDNSClient) ensureCNAME(ctx context.Context, hostname, target string) error {
	existing, err := c.listRecords(ctx, hostname)
	if err != nil {
		return err
	}
	for _, r := range existing {
		if r.Type == "CNAME" && r.Content == target && r.Proxied {
			return nil // already correct
		}
	}
	return c.createRecord(ctx, cfDNSRecord{
		Type:    "CNAME",
		Name:    hostname,
		Content: target,
		Proxied: true,
	})
}

// deleteCNAME deletes all CNAME records for hostname. Silently returns nil if
// no records exist.
func (c *CloudflareDNSClient) deleteCNAME(ctx context.Context, hostname string) error {
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

func (c *CloudflareDNSClient) listRecords(ctx context.Context, name string) ([]cfDNSRecord, error) {
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

func (c *CloudflareDNSClient) createRecord(ctx context.Context, rec cfDNSRecord) error {
	payload, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		fmt.Sprintf("%s/zones/%s/dns_records", cloudflareAPIBase, c.zoneID),
		bytes.NewReader(payload))
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
		return fmt.Errorf("parse Cloudflare create response: %w", err)
	}
	if !result.Success {
		return fmt.Errorf("cloudflare create DNS record: %v", result.Errors)
	}
	return nil
}

func (c *CloudflareDNSClient) setHeaders(req *http.Request) {
	req.Header.Set("Authorization", "Bearer "+c.token)
}
