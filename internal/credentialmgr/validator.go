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
	"fmt"
	"net/http"
	"strings"
	"time"
)

// EndpointValidator probes a credential against its target before it is stored.
//
// This is the service's justification. A form that tests a token against its
// endpoint turns a class of latent failure — "tenant provisioning stalled, the
// path exists, the password was pasted with a trailing newline" — into a red
// field, minutes after the mistake rather than hours.
//
// It covers the same types as the installer's shell validators plus the ones
// shell cannot reach. Where both implement a type they must agree; the shell
// set is deliberately tiny so that overlap stays small.
type EndpointValidator struct {
	HTTP *http.Client
}

// NewEndpointValidator builds a validator with a bounded timeout, so an
// unreachable endpoint fails the form rather than hanging the request.
func NewEndpointValidator() *EndpointValidator {
	return &EndpointValidator{HTTP: &http.Client{Timeout: 15 * time.Second}}
}

// Validate dispatches on spec.validate.type.
//
// An unknown type is an ERROR, not a pass. Silently accepting a credential the
// catalogue asked to have checked is the failure this exists to prevent.
func (v *EndpointValidator) Validate(ctx context.Context, kind string, fields map[string]string) error {
	switch kind {
	case "", "noop":
		return nil
	case "oci-registry":
		return v.basicAuthProbe(ctx, fields["host"], "/v2/", fields["username"], fields["password"])
	case "git-https":
		repo := strings.TrimSuffix(fields["url"], ".git")
		return v.basicAuthProbe(ctx, repo, ".git/info/refs?service=git-upload-pack",
			fields["username"], fields["token"])
	case "oidc-discovery":
		return v.bearerProbe(ctx, fields["url"], fields["api-token"])
	case "smtp":
		// Not reachable over HTTP, and the shell validator already covers it
		// with openssl s_client. Refusing is honest; pretending to check is not.
		return fmt.Errorf("smtp validation is not available in the credential manager yet")
	default:
		return fmt.Errorf("unknown validator type %q", kind)
	}
}

func (v *EndpointValidator) basicAuthProbe(ctx context.Context, base, suffix, user, pass string) error {
	if base == "" {
		return fmt.Errorf("no endpoint to probe: the requirement declares no host or url")
	}
	url := strings.TrimSuffix(base, "/") + suffix
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	// Only send credentials when there are some: an empty password makes a
	// public endpoint answer 401, reporting a good source as a bad credential.
	if pass != "" {
		req.SetBasicAuth(user, pass)
	}
	return v.classify(url, req)
}

func (v *EndpointValidator) bearerProbe(ctx context.Context, url, token string) error {
	if url == "" {
		return fmt.Errorf("no endpoint to probe: the requirement declares no url")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return v.classify(url, req)
}

// classify separates UNREACHABLE from REJECTED, because they need different
// fixes and an operator shown the wrong one looks in the wrong place.
func (v *EndpointValidator) classify(url string, req *http.Request) error {
	resp, err := v.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("%s is unreachable: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()

	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return nil
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return fmt.Errorf("%s rejected the credential (HTTP %d)", url, resp.StatusCode)
	case resp.StatusCode == http.StatusNotFound:
		return fmt.Errorf("%s returned 404 — a private resource also 404s when the credential cannot see it", url)
	default:
		return fmt.Errorf("%s returned an unexpected HTTP %d", url, resp.StatusCode)
	}
}
