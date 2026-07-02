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
	"strings"
)

const cloudflareAPIBase = "https://api.cloudflare.com/client/v4"

// CloudflareDNSClient is an optional edge-DNS adapter for Cloudflare. It manages
// proxied CNAME records for *.<effectiveDomain> and, in gateway+tunnel mode,
// public hostname → origin mappings on the remotely-managed Cloudflare tunnel.
type CloudflareDNSClient struct {
	token       string
	tunnelToken string // optional; falls back to token for tunnel configuration API
	zoneID      string
	tunnelCNAME string // e.g. <uuid>.cfargotunnel.com
	accountID   string // lazily resolved from zone metadata
	http        *http.Client
}

// NewCloudflareDNSClient creates a CloudflareDNSClient. tunnelToken may be empty;
// when set it is used for Cloudflare Tunnel configuration API calls (requires
// Account → Cloudflare Tunnel → Edit), while token is used for DNS record API.
func NewCloudflareDNSClient(token, zoneID, tunnelCNAME, tunnelToken string) *CloudflareDNSClient {
	return &CloudflareDNSClient{
		token:       token,
		tunnelToken: tunnelToken,
		zoneID:      zoneID,
		tunnelCNAME: tunnelCNAME,
		http:        &http.Client{},
	}
}

func (c *CloudflareDNSClient) tunnelAPIToken() string {
	if c.tunnelToken != "" {
		return c.tunnelToken
	}
	return c.token
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

type cfZoneResponse struct {
	Success bool `json:"success"`
	Result  struct {
		Account struct {
			ID string `json:"id"`
		} `json:"account"`
	} `json:"result"`
	Errors []cfError `json:"errors"`
}

type cfTunnelConfigResponse struct {
	Success bool `json:"success"`
	Result  struct {
		Config cfTunnelConfig `json:"config"`
	} `json:"result"`
	Errors []cfError `json:"errors"`
}

type cfTunnelConfig struct {
	Ingress []cfTunnelIngressRule `json:"ingress"`
}

type cfTunnelOriginRequest struct {
	MatchSNItoHost bool `json:"matchSNItoHost,omitempty"`
	NoTLSVerify    bool `json:"noTLSVerify,omitempty"`
}

type cfTunnelIngressRule struct {
	Hostname      string                 `json:"hostname,omitempty"`
	Service       string                 `json:"service"`
	OriginRequest *cfTunnelOriginRequest `json:"originRequest,omitempty"`
}

func tunnelIngressRuleForService(hostname, service string) cfTunnelIngressRule {
	rule := cfTunnelIngressRule{Hostname: hostname, Service: service}
	if strings.HasPrefix(strings.ToLower(service), "https://") {
		rule.OriginRequest = &cfTunnelOriginRequest{
			MatchSNItoHost: true,
			NoTLSVerify:    true, // cluster origin certs may be staging or not chain to public roots from cloudflared
		}
	}
	return rule
}

func tunnelIngressRulesEqual(a, b cfTunnelIngressRule) bool {
	if a.Hostname != b.Hostname || a.Service != b.Service {
		return false
	}
	return tunnelOriginRequestEqual(a.OriginRequest, b.OriginRequest)
}

func tunnelOriginRequestEqual(a, b *cfTunnelOriginRequest) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	return a.MatchSNItoHost == b.MatchSNItoHost && a.NoTLSVerify == b.NoTLSVerify
}

func parseTunnelID(tunnelCNAME string) string {
	host := strings.TrimSpace(tunnelCNAME)
	if i := strings.Index(host, "."); i > 0 {
		return host[:i]
	}
	return host
}

// ensureTunnelIngress adds or updates a public hostname → service mapping on the
// remotely-managed Cloudflare tunnel. Existing ingress rules are preserved.
func (c *CloudflareDNSClient) ensureTunnelIngress(ctx context.Context, hostname, service string) error {
	if hostname == "" || service == "" {
		return nil
	}
	accountID, err := c.accountIDForZone(ctx)
	if err != nil {
		return err
	}
	tunnelID := parseTunnelID(c.tunnelCNAME)
	if tunnelID == "" {
		return fmt.Errorf("invalid tunnel CNAME %q", c.tunnelCNAME)
	}

	config, err := c.getTunnelConfig(ctx, accountID, tunnelID)
	if err != nil {
		return err
	}
	desired := tunnelIngressRuleForService(hostname, service)
	if idx := ingressRuleIndex(config.Ingress, hostname); idx >= 0 {
		if tunnelIngressRulesEqual(config.Ingress[idx], desired) {
			return nil
		}
		config.Ingress[idx] = desired
		return c.putTunnelConfig(ctx, accountID, tunnelID, config)
	}
	config.Ingress = upsertTunnelIngress(config.Ingress, desired)
	return c.putTunnelConfig(ctx, accountID, tunnelID, config)
}

// deleteTunnelIngress removes a hostname from the tunnel ingress configuration.
func (c *CloudflareDNSClient) deleteTunnelIngress(ctx context.Context, hostname string) error {
	if hostname == "" {
		return nil
	}
	accountID, err := c.accountIDForZone(ctx)
	if err != nil {
		return err
	}
	tunnelID := parseTunnelID(c.tunnelCNAME)
	config, err := c.getTunnelConfig(ctx, accountID, tunnelID)
	if err != nil {
		return err
	}
	idx := ingressRuleIndex(config.Ingress, hostname)
	if idx < 0 {
		return nil
	}
	config.Ingress = append(config.Ingress[:idx], config.Ingress[idx+1:]...)
	if len(config.Ingress) == 0 {
		config.Ingress = []cfTunnelIngressRule{{Service: "http_status:404"}}
	}
	return c.putTunnelConfig(ctx, accountID, tunnelID, config)
}

func (c *CloudflareDNSClient) accountIDForZone(ctx context.Context) (string, error) {
	if c.accountID != "" {
		return c.accountID, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		fmt.Sprintf("%s/zones/%s", cloudflareAPIBase, c.zoneID), nil)
	if err != nil {
		return "", err
	}
	c.setHeaders(req)
	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	var result cfZoneResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return "", fmt.Errorf("parse Cloudflare zone response: %w", err)
	}
	if !result.Success || result.Result.Account.ID == "" {
		return "", fmt.Errorf("cloudflare zone lookup failed: %v", result.Errors)
	}
	c.accountID = result.Result.Account.ID
	return c.accountID, nil
}

func (c *CloudflareDNSClient) getTunnelConfig(ctx context.Context, accountID, tunnelID string) (cfTunnelConfig, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		fmt.Sprintf("%s/accounts/%s/cfd_tunnel/%s/configurations", cloudflareAPIBase, accountID, tunnelID), nil)
	if err != nil {
		return cfTunnelConfig{}, err
	}
	c.setTunnelHeaders(req)
	resp, err := c.http.Do(req)
	if err != nil {
		return cfTunnelConfig{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	var result cfTunnelConfigResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return cfTunnelConfig{}, fmt.Errorf("parse Cloudflare tunnel config response: %w", err)
	}
	if !result.Success {
		return cfTunnelConfig{}, fmt.Errorf("cloudflare get tunnel config: %w", formatCloudflareErrors(result.Errors))
	}
	if len(result.Result.Config.Ingress) == 0 {
		result.Result.Config.Ingress = []cfTunnelIngressRule{{Service: "http_status:404"}}
	}
	return result.Result.Config, nil
}

func (c *CloudflareDNSClient) putTunnelConfig(ctx context.Context, accountID, tunnelID string, config cfTunnelConfig) error {
	payload, err := json.Marshal(map[string]interface{}{"config": config})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut,
		fmt.Sprintf("%s/accounts/%s/cfd_tunnel/%s/configurations", cloudflareAPIBase, accountID, tunnelID),
		bytes.NewReader(payload))
	if err != nil {
		return err
	}
	c.setTunnelHeaders(req)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	var result cfTunnelConfigResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return fmt.Errorf("parse Cloudflare tunnel config update response: %w", err)
	}
	if !result.Success {
		return fmt.Errorf("cloudflare put tunnel config: %w", formatCloudflareErrors(result.Errors))
	}
	return nil
}

func ingressRuleIndex(rules []cfTunnelIngressRule, hostname string) int {
	for i := range rules {
		if rules[i].Hostname == hostname {
			return i
		}
	}
	return -1
}

func upsertTunnelIngress(rules []cfTunnelIngressRule, rule cfTunnelIngressRule) []cfTunnelIngressRule {
	catchAllIdx := -1
	for i := range rules {
		if rules[i].Hostname == "" {
			catchAllIdx = i
			break
		}
	}
	if idx := ingressRuleIndex(rules, rule.Hostname); idx >= 0 {
		rules[idx] = rule
		return rules
	}
	if catchAllIdx >= 0 {
		out := make([]cfTunnelIngressRule, 0, len(rules)+1)
		out = append(out, rules[:catchAllIdx]...)
		out = append(out, rule)
		out = append(out, rules[catchAllIdx:]...)
		return out
	}
	return append(rules, rule)
}

// ensureCNAME creates or updates a proxied CNAME record pointing hostname → target.
// Idempotent: if a record with the exact same content already exists, it is left unchanged.
func (c *CloudflareDNSClient) ensureCNAME(ctx context.Context, hostname, target string) error {
	existing, err := c.listRecords(ctx, hostname)
	if err != nil {
		return err
	}
	for _, r := range existing {
		if r.Type != "CNAME" {
			continue
		}
		if r.Content == target && r.Proxied {
			return nil // already correct
		}
		return c.updateRecord(ctx, r.ID, cfDNSRecord{
			Type:    "CNAME",
			Name:    hostname,
			Content: target,
			Proxied: true,
		})
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

func (c *CloudflareDNSClient) updateRecord(ctx context.Context, id string, rec cfDNSRecord) error {
	payload, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut,
		fmt.Sprintf("%s/zones/%s/dns_records/%s", cloudflareAPIBase, c.zoneID, id),
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
		return fmt.Errorf("parse Cloudflare update response: %w", err)
	}
	if !result.Success {
		return fmt.Errorf("cloudflare update DNS record: %v", result.Errors)
	}
	return nil
}

func (c *CloudflareDNSClient) setHeaders(req *http.Request) {
	req.Header.Set("Authorization", "Bearer "+c.token)
}

func (c *CloudflareDNSClient) setTunnelHeaders(req *http.Request) {
	req.Header.Set("Authorization", "Bearer "+c.tunnelAPIToken())
}

func formatCloudflareErrors(errors []cfError) error {
	if len(errors) == 0 {
		return fmt.Errorf("unknown error")
	}
	if errors[0].Code == 10000 {
		return fmt.Errorf("%v (grant Account → Cloudflare Tunnel → Edit on the API token, or set CLOUDFLARE_TUNNEL_API_TOKEN)", errors)
	}
	return fmt.Errorf("%v", errors)
}
