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
	"strings"

	ctrl "sigs.k8s.io/controller-runtime"
)

// CloudflareTunnelIngress implements EdgeIngress against a remotely-managed
// Cloudflare Tunnel. It programs which hostnames the tunnel routes to which
// origin, and reports the CNAME those hostnames must resolve to.
//
// Its credential is the tunnel credential -- Account -> Cloudflare One
// Connector: cloudflared -> Edit, which older accounts list as Cloudflare
// Tunnel. This is an ACCOUNT-scoped permission, where the DNS half's is
// ZONE-scoped: they are genuinely different grants and a token can hold either
// without the other, which is the failure formatCloudflareErrors explains.
//
// zoneID is here only to resolve the account id, which the tunnel endpoints
// are addressed by. It is not used to write anything.
//
// That lookup is the one place the split is not clean, and it is worth naming.
// Reading /zones/<id> needs Zone -> Zone -> Read, which is a DNS-side grant;
// before the split it ran on the DNS token, because there was only one client.
// Now that the halves hold their own credentials, either the tunnel token also
// carries Zone:Read, or the account id is supplied outright and no zone is
// read at all. accountID exists for the second, and CLOUDFLARE_ACCOUNT_ID is
// how a deployment provides it — the escape hatch for a tunnel token scoped to
// exactly the account permission it needs and nothing else.
type CloudflareTunnelIngress struct {
	token       string
	zoneID      string
	tunnelCNAME string // <uuid>.cfargotunnel.com
	accountID   string // supplied, or lazily resolved from zone metadata
	http        *http.Client
}

// NewCloudflareTunnelIngress builds the ingress half of a Cloudflare edge.
//
// accountID may be empty, in which case it is resolved from the zone on first
// use — see the note on the struct about what that asks of the token.
func NewCloudflareTunnelIngress(token, zoneID, tunnelCNAME, accountID string) *CloudflareTunnelIngress {
	return &CloudflareTunnelIngress{
		token:       token,
		zoneID:      zoneID,
		tunnelCNAME: tunnelCNAME,
		accountID:   accountID,
		http:        &http.Client{},
	}
}

// Target implements EdgeIngress: a tunnel is reached by a proxied CNAME to its
// own hostname. Proxied is not optional -- cfargotunnel.com resolves to
// nothing a client could connect to directly.
func (c *CloudflareTunnelIngress) Target() EdgeTarget {
	if c.tunnelCNAME == "" {
		return EdgeTarget{}
	}
	return EdgeTarget{Type: "CNAME", Value: c.tunnelCNAME, Proxied: true}
}

// EnsureRoute implements EdgeIngress.
func (c *CloudflareTunnelIngress) EnsureRoute(ctx context.Context, hostname, service string) error {
	return c.ensureTunnelIngress(ctx, hostname, service)
}

// DeleteRoute implements EdgeIngress.
func (c *CloudflareTunnelIngress) DeleteRoute(ctx context.Context, hostname string) error {
	return c.deleteTunnelIngress(ctx, hostname)
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

func (c *CloudflareTunnelIngress) ensureTunnelIngress(ctx context.Context, hostname, service string) error {
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
		// 1055 only: a tunnel with no configuration yet is a cluster whose
		// routes are set up by hand, which is a supported way to run and not an
		// error. An authorization failure is not — it is a token missing a
		// permission, it will not fix itself, and skipping it would leave tenant
		// hostnames unrouted while the tenant reported Ready. It is returned so
		// TunnelIngressReady carries the reason, which now names the permission.
		if strings.Contains(err.Error(), "1055") || strings.Contains(err.Error(), "not found") {
			ctrl.LoggerFrom(ctx).Info("Cloudflare tunnel has no configuration; skipping dynamic tunnel routing updates. Ensure a wildcard or manual ingress rule is set up in your Cloudflare dashboard.", "tunnel", tunnelID, "err", err)
			return nil
		}
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
func (c *CloudflareTunnelIngress) deleteTunnelIngress(ctx context.Context, hostname string) error {
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
		if strings.Contains(err.Error(), "1055") || strings.Contains(err.Error(), "not found") {
			return nil
		}
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

func (c *CloudflareTunnelIngress) accountIDForZone(ctx context.Context) (string, error) {
	if c.accountID != "" {
		return c.accountID, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		fmt.Sprintf("%s/zones/%s", cloudflareAPIBase, c.zoneID), nil)
	if err != nil {
		return "", err
	}
	c.setTunnelHeaders(req)
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

func (c *CloudflareTunnelIngress) getTunnelConfig(ctx context.Context, accountID, tunnelID string) (cfTunnelConfig, error) {
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

func (c *CloudflareTunnelIngress) putTunnelConfig(ctx context.Context, accountID, tunnelID string, config cfTunnelConfig) error {
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

func (c *CloudflareTunnelIngress) setTunnelHeaders(req *http.Request) {
	req.Header.Set("Authorization", "Bearer "+c.token)
}
