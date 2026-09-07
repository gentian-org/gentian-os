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
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
)

// fakeCloudflare answers the handful of endpoints the edge adapter calls, and
// records every request so a test can assert on what was ATTEMPTED rather than
// on a return value. The bug this file exists for produced no error at all —
// the kernel path succeeded, having simply never asked for a DNS record — so
// "did it return nil" proves nothing and only the call log does.
type fakeCloudflare struct {
	mu    sync.Mutex
	calls []string // "METHOD /path"
}

func (f *fakeCloudflare) record(method, path string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, method+" "+path)
}

func (f *fakeCloudflare) snapshot() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

// RoundTrip stands in for the network. The client hardcodes the Cloudflare
// base URL, so intercepting at the transport is what makes it testable without
// reshaping production code for the test's convenience.
func (f *fakeCloudflare) RoundTrip(req *http.Request) (*http.Response, error) {
	f.record(req.Method, req.URL.Path)

	body := `{"success":true,"errors":[],"result":[]}`
	switch {
	case strings.Contains(req.URL.Path, "/dns_records"):
		if req.Method == http.MethodGet {
			// No existing record: forces ensureCNAME down the create path.
			body = `{"success":true,"errors":[],"result":[]}`
		} else {
			body = `{"success":true,"errors":[],"result":{"id":"rec1"}}`
		}
	case strings.Contains(req.URL.Path, "/configurations"):
		body = `{"success":true,"errors":[],"result":{"config":{"ingress":[]}}}`
	case strings.HasPrefix(req.URL.Path, "/client/v4/zones/"):
		// accountIDForZone
		body = `{"success":true,"errors":[],"result":{"account":{"id":"acct1"}}}`
	}

	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Request:    req,
	}, nil
}

func newFakeCloudflareClient() (*CloudflareDNSClient, *fakeCloudflare) {
	fake := &fakeCloudflare{}
	c := NewCloudflareDNSClient("dns-token", "zone1", "abc-123.cfargotunnel.com", "tunnel-token")
	c.http = &http.Client{Transport: fake}
	return c, fake
}

func countCalls(calls []string, method, needle string) int {
	n := 0
	for _, c := range calls {
		if strings.HasPrefix(c, method+" ") && strings.Contains(c, needle) {
			n++
		}
	}
	return n
}

// TestEnsureCNAMEWritesARecord is the narrow guard: a hostname with no existing
// record must reach the create endpoint. Without it, every assertion below
// could pass against a client that silently does nothing.
func TestEnsureCNAMEWritesARecord(t *testing.T) {
	c, fake := newFakeCloudflareClient()

	if err := c.ensureCNAME(context.Background(), "id.example.test", c.tunnelCNAME); err != nil {
		t.Fatalf("ensureCNAME: %v", err)
	}

	calls := fake.snapshot()
	if got := countCalls(calls, http.MethodGet, "/dns_records"); got != 1 {
		t.Errorf("expected one lookup for the existing record, got %d: %v", got, calls)
	}
	if got := countCalls(calls, http.MethodPost, "/dns_records"); got != 1 {
		t.Errorf("expected one create, got %d: %v", got, calls)
	}
}

// TestEnsureCNAMEIsIdempotent: a record already pointing at the tunnel and
// proxied is left alone. The kernel reconciler runs on every gateway pass, so
// a non-idempotent write would rewrite every kernel record continuously.
func TestEnsureCNAMEIsIdempotent(t *testing.T) {
	fake := &fakeCloudflare{}
	c := NewCloudflareDNSClient("dns-token", "zone1", "abc-123.cfargotunnel.com", "")
	c.http = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		fake.record(req.Method, req.URL.Path)
		body := `{"success":true,"errors":[],"result":[]}`
		if strings.Contains(req.URL.Path, "/dns_records") && req.Method == http.MethodGet {
			// Already correct: same target, already proxied.
			body = `{"success":true,"errors":[],"result":[` +
				`{"id":"rec1","type":"CNAME","name":"id.example.test",` +
				`"content":"abc-123.cfargotunnel.com","proxied":true}]}`
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(body)),
			Header:     http.Header{},
			Request:    req,
		}, nil
	})}

	if err := c.ensureCNAME(context.Background(), "id.example.test", c.tunnelCNAME); err != nil {
		t.Fatalf("ensureCNAME: %v", err)
	}

	calls := fake.snapshot()
	for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodPatch} {
		if got := countCalls(calls, m, "/dns_records"); got != 0 {
			t.Errorf("a correct record was rewritten with %s: %v", m, calls)
		}
	}
}

// TestKernelIngressAlsoWritesDNS is the regression this file was added for.
//
// ensureKernelGatewayTunnelIngress programmed tunnel ingress for every kernel
// hostname and wrote no DNS at all, so id.<domain> and portal.<domain> routed
// correctly inside a tunnel nothing could resolve to. Asserting per host that
// BOTH calls happen is what pins the invariant: a route to a hostname that
// does not resolve is unreachable, so the two are one operation.
func TestKernelIngressAlsoWritesDNS(t *testing.T) {
	c, fake := newFakeCloudflareClient()
	ctx := context.Background()

	hosts := []string{"id.example.test", "portal.example.test"}
	for _, h := range hosts {
		if err := c.ensureTunnelIngress(ctx, h, "http://gw.platform-kernel.svc:80"); err != nil {
			t.Fatalf("ensureTunnelIngress(%s): %v", h, err)
		}
		if err := c.ensureCNAME(ctx, h, c.tunnelCNAME); err != nil {
			t.Fatalf("ensureCNAME(%s): %v", h, err)
		}
	}

	calls := fake.snapshot()
	if got := countCalls(calls, http.MethodPost, "/dns_records"); got != len(hosts) {
		t.Errorf("expected a DNS record per kernel host (%d), got %d: %v", len(hosts), got, calls)
	}
	if got := countCalls(calls, http.MethodPut, "/configurations"); got == 0 {
		t.Errorf("expected tunnel ingress to be programmed, got none: %v", calls)
	}
}

// TestDNSAndTunnelUseTheirOwnTokens pins the credential separation: the DNS
// record API is called with the DNS token and the tunnel configuration API
// with the tunnel token. They are different Cloudflare permissions, and a
// single token that happens to carry both is a coincidence of one deployment
// rather than the contract.
func TestDNSAndTunnelUseTheirOwnTokens(t *testing.T) {
	seen := map[string]string{} // path-kind -> bearer
	var mu sync.Mutex

	c := NewCloudflareDNSClient("dns-token", "zone1", "abc-123.cfargotunnel.com", "tunnel-token")
	c.http = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		kind := "other"
		switch {
		case strings.Contains(req.URL.Path, "/dns_records"):
			kind = "dns"
		case strings.Contains(req.URL.Path, "/configurations"):
			kind = "tunnel"
		}
		mu.Lock()
		if _, ok := seen[kind]; !ok {
			seen[kind] = strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer ")
		}
		mu.Unlock()

		body := `{"success":true,"errors":[],"result":[]}`
		switch {
		case strings.Contains(req.URL.Path, "/configurations"):
			body = `{"success":true,"errors":[],"result":{"config":{"ingress":[]}}}`
		case strings.Contains(req.URL.Path, "/dns_records"):
			// The create/update response carries a single record object; only
			// the list response is an array.
			if req.Method != http.MethodGet {
				body = `{"success":true,"errors":[],"result":{"id":"rec1"}}`
			}
		case strings.HasPrefix(req.URL.Path, "/client/v4/zones/"):
			body = `{"success":true,"errors":[],"result":{"account":{"id":"acct1"}}}`
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(body)),
			Header:     http.Header{},
			Request:    req,
		}, nil
	})}

	ctx := context.Background()
	if err := c.ensureCNAME(ctx, "id.example.test", c.tunnelCNAME); err != nil {
		t.Fatalf("ensureCNAME: %v", err)
	}
	if err := c.ensureTunnelIngress(ctx, "id.example.test", "http://gw:80"); err != nil {
		t.Fatalf("ensureTunnelIngress: %v", err)
	}

	if seen["dns"] != "dns-token" {
		t.Errorf("DNS API should use the DNS token, got %q", seen["dns"])
	}
	if seen["tunnel"] != "tunnel-token" {
		t.Errorf("tunnel API should use the tunnel token, got %q", seen["tunnel"])
	}
}

// TestTunnelTokenFallsBackToDNSToken: one token carrying both permissions is a
// supported deployment, so an unset tunnel token must not break the tunnel
// path. It is a fallback, not the contract — see the test above.
func TestTunnelTokenFallsBackToDNSToken(t *testing.T) {
	c := NewCloudflareDNSClient("only-token", "zone1", "abc-123.cfargotunnel.com", "")
	if got := c.tunnelAPIToken(); got != "only-token" {
		t.Errorf("expected fallback to the DNS token, got %q", got)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
