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

package credentialmgr

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

// cloudflareValidator points the probe at srvURL instead of Cloudflare by
// swapping the transport, so the request the real code builds is the request
// under test.
func cloudflareValidator(domain string, handler http.RoundTripper) *EndpointValidator {
	return &EndpointValidator{
		HTTP:         &http.Client{Transport: handler},
		KernelDomain: domain,
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func zoneResponse(status int, _ string) roundTripFunc {
	return func(r *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: status,
			Body:       http.NoBody,
			Header:     http.Header{},
			Request:    r,
		}, nil
	}
}

func zoneJSON(status int, body string) roundTripFunc {
	return func(r *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: status,
			Body:       io.NopCloser(strings.NewReader(body)),
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Request:    r,
		}, nil
	}
}

// The kernel domain is routinely a subdomain of the Cloudflare zone. Matching
// on equality alone would report a working token as broken for every cluster
// that is not sitting on the zone apex.
func TestCloudflareProbeAcceptsZoneCoveringASubdomain(t *testing.T) {
	v := cloudflareValidator("cluster.example.com",
		zoneJSON(200, `{"success":true,"result":[{"name":"example.com"}]}`))
	if err := v.Validate(context.Background(), "cloudflare-dns", "", map[string]string{"api-token": "t"}); err != nil {
		t.Fatalf("rejected a token whose zone covers the domain: %v", err)
	}
}

func TestCloudflareProbeAcceptsExactZone(t *testing.T) {
	v := cloudflareValidator("example.com",
		zoneJSON(200, `{"success":true,"result":[{"name":"example.com"}]}`))
	if err := v.Validate(context.Background(), "cloudflare-dns", "", map[string]string{"api-token": "t"}); err != nil {
		t.Fatalf("rejected a token scoped to exactly this zone: %v", err)
	}
}

// A token can be perfectly valid and still carry no rights here. Accepting it
// moves the failure to the first certificate renewal, where it surfaces as an
// ACME timeout that never mentions a credential.
func TestCloudflareProbeRejectsTokenScopedElsewhere(t *testing.T) {
	v := cloudflareValidator("cluster.example.com",
		zoneJSON(200, `{"success":true,"result":[{"name":"other.net"}]}`))
	err := v.Validate(context.Background(), "cloudflare-dns", "", map[string]string{"api-token": "t"})
	if err == nil {
		t.Fatal("accepted a token with no zone covering the kernel domain")
	}
	// The other account's zone names must not come back in the error: whoever
	// pasted the token would learn what else it can reach.
	if strings.Contains(err.Error(), "other.net") {
		t.Errorf("error discloses zones the token can see: %v", err)
	}
}

// A suffix match must respect label boundaries. notexample.com ends with
// "example.com" as a string but is a different zone entirely.
func TestCloudflareProbeDoesNotMatchPartialLabel(t *testing.T) {
	v := cloudflareValidator("notexample.com",
		zoneJSON(200, `{"success":true,"result":[{"name":"example.com"}]}`))
	if err := v.Validate(context.Background(), "cloudflare-dns", "", map[string]string{"api-token": "t"}); err == nil {
		t.Fatal("matched notexample.com against the example.com zone")
	}
}

func TestCloudflareProbeReportsRejectedToken(t *testing.T) {
	v := cloudflareValidator("example.com", zoneResponse(401, ""))
	err := v.Validate(context.Background(), "cloudflare-dns", "", map[string]string{"api-token": "bad"})
	if err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("want a rejection naming the status, got %v", err)
	}
}

// Without a kernel domain the probe cannot ask the question it exists to ask.
// Passing would accept any token that authenticates.
func TestCloudflareProbeRefusesWithoutKernelDomain(t *testing.T) {
	v := cloudflareValidator("", zoneJSON(200, `{"success":true,"result":[{"name":"example.com"}]}`))
	if err := v.Validate(context.Background(), "cloudflare-dns", "", map[string]string{"api-token": "t"}); err == nil {
		t.Fatal("passed with no kernel domain to check the token against")
	}
}

func TestCloudflareProbeRequiresToken(t *testing.T) {
	v := cloudflareValidator("example.com", zoneJSON(200, `{"success":true,"result":[]}`))
	if err := v.Validate(context.Background(), "cloudflare-dns", "", map[string]string{"api-token": "  "}); err == nil {
		t.Fatal("accepted a blank api-token")
	}
}

// The guard that would have caught the original bug: the CRD's enum is the
// contract the Admin Console offers, so a type it accepts and the validator
// does not is a credential nobody can save. cloudflare-dns sat in that gap.
func TestEveryDeclaredValidatorTypeIsImplemented(t *testing.T) {
	// Kept in step with the +kubebuilder:validation:Enum on
	// CredentialValidation.Type.
	declared := []string{"oci-registry", "git-https", "oidc-discovery", "cloudflare-dns", "smtp", "noop"}
	v := &EndpointValidator{
		HTTP:         &http.Client{Transport: zoneResponse(599, "")},
		KernelDomain: "example.com",
		Relay: func(context.Context) (string, string, error) {
			return "", "", context.Canceled
		},
	}
	for _, kind := range declared {
		err := v.Validate(context.Background(), kind, "https://example.invalid", map[string]string{})
		if err != nil && strings.Contains(err.Error(), "unknown validator type") {
			t.Errorf("%s is offered by the CRD but not implemented: %v", kind, err)
		}
	}
}
