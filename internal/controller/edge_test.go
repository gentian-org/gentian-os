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

// fakeCloudflare answers the endpoints the ingress calls and records every
// request, so a test can assert on what was ATTEMPTED. Both invariants here
// are about calls that must or must not happen, and neither shows up in a
// return value.
type fakeCloudflare struct {
	mu    sync.Mutex
	calls []string // "METHOD /path"
}

func (f *fakeCloudflare) RoundTrip(req *http.Request) (*http.Response, error) {
	f.mu.Lock()
	f.calls = append(f.calls, req.Method+" "+req.URL.Path)
	f.mu.Unlock()

	body := `{"success":true,"errors":[],"result":[]}`
	switch {
	case strings.Contains(req.URL.Path, "/configurations"):
		body = `{"success":true,"errors":[],"result":{"config":{"ingress":[]}}}`
	case strings.HasPrefix(req.URL.Path, "/client/v4/zones/"):
		body = `{"success":true,"errors":[],"result":{"account":{"id":"acct1"}}}`
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     http.Header{},
		Request:    req,
	}, nil
}

func (f *fakeCloudflare) snapshot() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func newFakeTunnel() (*CloudflareTunnelIngress, *fakeCloudflare) {
	fake := &fakeCloudflare{}
	ing := NewCloudflareTunnelIngress("tunnel-token", "zone1", "abc-123.cfargotunnel.com", "")
	ing.http = &http.Client{Transport: fake}
	return ing, fake
}

// TestIngressWritesNoDNSRecords is the invariant of the whole design: the
// operator is out of the DNS business. external-dns writes this cluster's
// records, for every provider, from the HTTPRoutes and the Gateway annotation
// below — an operator that also wrote them was a Cloudflare-only ninth
// implementation, which is why a Route 53 cluster had no writer at all.
//
// Asserted as an absence, because that is what regressed last time: the DNS
// call either happens or it does not, and no return value says which.
func TestIngressWritesNoDNSRecords(t *testing.T) {
	ing, fake := newFakeTunnel()
	ctx := context.Background()

	for _, h := range []string{"id.example.test", "portal.example.test"} {
		if err := ing.EnsureRoute(ctx, h, "http://gw.platform-kernel.svc:80"); err != nil {
			t.Fatalf("EnsureRoute(%s): %v", h, err)
		}
	}
	if err := ing.DeleteRoute(ctx, "*.example.test"); err != nil {
		t.Fatalf("DeleteRoute: %v", err)
	}

	for _, c := range fake.snapshot() {
		if strings.Contains(c, "/dns_records") {
			t.Errorf("the ingress touched the DNS record API (%s); external-dns owns records", c)
		}
	}
}

// TestIngressProgramsRoutes is the other half: having asserted it writes no
// DNS, prove it still does the thing it is for.
func TestIngressProgramsRoutes(t *testing.T) {
	ing, fake := newFakeTunnel()
	if err := ing.EnsureRoute(context.Background(), "id.example.test", "http://gw:80"); err != nil {
		t.Fatalf("EnsureRoute: %v", err)
	}
	var programmed bool
	for _, c := range fake.snapshot() {
		if strings.HasPrefix(c, http.MethodPut+" ") && strings.Contains(c, "/configurations") {
			programmed = true
		}
	}
	if !programmed {
		t.Errorf("no tunnel configuration was written: %v", fake.snapshot())
	}
}

// TestTunnelDeclaresItsDNSTarget pins the seam between the two halves. This
// annotation is the only thing that tells external-dns where a tunnelled
// hostname points, since a tunnelled Gateway has no address of its own.
//
// Proxied is not a preference: cfargotunnel.com resolves to nothing a client
// could connect to, so an unproxied record answers and then refuses.
func TestTunnelDeclaresItsDNSTarget(t *testing.T) {
	ing := NewCloudflareTunnelIngress("t", "zone1", "abc-123.cfargotunnel.com", "")
	ann := ing.DNSAnnotations()

	if got := ann["external-dns.alpha.kubernetes.io/target"]; got != "abc-123.cfargotunnel.com" {
		t.Errorf("target annotation = %q, want the tunnel CNAME", got)
	}
	if got := ann["external-dns.alpha.kubernetes.io/cloudflare-proxied"]; got != "true" {
		t.Errorf("cloudflare-proxied = %q, want \"true\" — a tunnel CNAME is unreachable unproxied", got)
	}
}

// TestNoTunnelDeclaresNoTarget: a cluster with a real address must not be told
// to point somewhere else. external-dns reads the Gateway's own LoadBalancer
// address, which is exactly right, and an annotation here would override it.
func TestNoTunnelDeclaresNoTarget(t *testing.T) {
	ing := NewCloudflareTunnelIngress("t", "zone1", "", "")
	if ann := ing.DNSAnnotations(); len(ann) != 0 {
		t.Errorf("expected no annotations without a tunnel, got %v", ann)
	}
}

// TestNilIngressIsInert covers the static-ip cluster, which programs nothing
// and must not panic on the way to doing so.
func TestNilIngressIsInert(t *testing.T) {
	ctx := context.Background()
	var ing EdgeIngress

	if err := edgeEnsureRoute(ctx, ing, "a.example.test", "http://gw:80"); err != nil {
		t.Errorf("edgeEnsureRoute on nil: %v", err)
	}
	if err := edgeDeleteRoute(ctx, ing, "a.example.test"); err != nil {
		t.Errorf("edgeDeleteRoute on nil: %v", err)
	}
	if ann := edgeDNSAnnotations(ing); len(ann) != 0 {
		t.Errorf("edgeDNSAnnotations on nil = %v, want none", ann)
	}
}

// TestIngressUsesItsOwnToken: the tunnel API is account-scoped and the DNS
// token is zone-scoped. They are different grants, and this asserts the
// ingress spends the one it was given rather than whatever else is around.
func TestIngressUsesItsOwnToken(t *testing.T) {
	var seen string
	var mu sync.Mutex
	ing := NewCloudflareTunnelIngress("tunnel-token", "zone1", "abc-123.cfargotunnel.com", "acct1")
	ing.http = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if strings.Contains(req.URL.Path, "/configurations") {
			mu.Lock()
			if seen == "" {
				seen = strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer ")
			}
			mu.Unlock()
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(`{"success":true,"errors":[],"result":{"config":{"ingress":[]}}}`)),
			Header:     http.Header{},
			Request:    req,
		}, nil
	})}

	if err := ing.EnsureRoute(context.Background(), "id.example.test", "http://gw:80"); err != nil {
		t.Fatalf("EnsureRoute: %v", err)
	}
	if seen != "tunnel-token" {
		t.Errorf("tunnel API used %q, want the tunnel token", seen)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
