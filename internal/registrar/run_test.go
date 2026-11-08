/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package registrar_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	dt "github.com/gentian-org/gentian-os/internal/director/directortest"
	"github.com/gentian-org/gentian-os/internal/registrar"
)

// The registrar does not start half-configured: with no store to ask, no
// cluster to ask about or no issuer to verify against, it says which is
// missing instead of serving routes that could only refuse or, worse, allow.
func TestTheRegistrarNamesWhatItIsMissing(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	reader := fake.NewClientBuilder().WithScheme(scheme(t)).Build()
	t.Setenv("REGISTRAR_DATABASE_URL", "")

	if _, _, err := registrar.NewFromEnv(context.Background(), reader, nil, log); err == nil ||
		!strings.Contains(err.Error(), "OPENFGA_API_URL") {
		t.Errorf("no store: %v", err)
	}
	t.Setenv("GENTIAN_DEPLOYMENTS_CLUSTER_ID", "")
	if _, _, err := registrar.NewFromEnv(context.Background(), reader, facts, log); err == nil ||
		!strings.Contains(err.Error(), "GENTIAN_DEPLOYMENTS_CLUSTER_ID") {
		t.Errorf("no cluster: %v", err)
	}
	t.Setenv("GENTIAN_DEPLOYMENTS_CLUSTER_ID", dt.Cluster)
	t.Setenv("REGISTRAR_ISSUER_BASE_URL", "")
	if _, _, err := registrar.NewFromEnv(context.Background(), reader, facts, log); err == nil ||
		!strings.Contains(err.Error(), "REGISTRAR_ISSUER_BASE_URL") {
		t.Errorf("no issuer: %v", err)
	}
}

// Wired from its environment it serves, on the address it was given, and
// refuses a caller it cannot identify.
func TestTheRegistrarStartsFromItsEnvironment(t *testing.T) {
	is := dt.NewIssuer(t, "gentian")
	t.Setenv("GENTIAN_DEPLOYMENTS_CLUSTER_ID", dt.Cluster)
	t.Setenv("REGISTRAR_ISSUER_BASE_URL", is.URL)
	t.Setenv("REGISTRAR_REALM_CREDENTIALS_PATH", t.TempDir())
	t.Setenv("REGISTRAR_DATABASE_URL", "")
	t.Setenv("REGISTRAR_ADDR", ":19445")
	t.Setenv("KERNEL_DOMAIN", dt.KernelDomain)

	reader := fake.NewClientBuilder().WithScheme(scheme(t)).WithObjects(tenant("demo", nil)).Build()
	srv, closeRecord, err := registrar.NewFromEnv(context.Background(), reader, facts,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer closeRecord()
	if srv.Addr != ":19445" {
		t.Errorf("addr = %q", srv.Addr)
	}
	ts := httptest.NewServer(srv)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("healthz: %d", resp.StatusCode)
	}
	resp, err = http.Get(ts.URL + "/v1/tenants/demo/people")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("no token: %d, want 401", resp.StatusCode)
	}

	// alice may manage demo's people; the tenant is in the cluster and no
	// credential for its realm has been handed over, which is a 503 that
	// names the realm rather than a refusal of her.
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/v1/tenants/demo/people", nil)
	req.Header.Set("Authorization", "Bearer "+is.Token(t, dt.Claims{Realm: "gentian", Subject: "alice", Audience: audience}))
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("no credential for the realm: %d, want 503", resp.StatusCode)
	}
}
