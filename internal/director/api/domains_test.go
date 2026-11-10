/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package api_test

import (
	"context"
	"net/http"
	"net/url"
	"testing"

	"github.com/gentian-org/gentian-os/internal/applifecycle"
	"github.com/gentian-org/gentian-os/internal/director/directortest"
)

// domainsOperator answers the operator's domains route with a fixed status,
// and everything else as the importing operator does.
type domainsOperator struct{ importingOperator }

const domainsAnswer = `{"checkedAt":"2026-11-10T00:00:00Z","domains":[{"domain":"gentian.org","tenant":"gentian",` +
	`"records":[{"name":"gentian.org","type":"MX","want":["0 mail.k.example"],"got":["16 route1.mx.cloudflare.net."],"state":"heldByOther","since":"2026-11-09T00:00:00Z"}]}]}`

func (o *domainsOperator) Get(ctx context.Context, path string, q url.Values) (int, []byte, error) {
	if path == applifecycle.DomainsPath {
		return http.StatusOK, []byte(domainsAnswer), nil
	}
	return o.importingOperator.Get(ctx, path, q)
}

// The cluster's administrators read each served domain's DNS records as the
// operator's check found them; a tenant's administrator may not.
func TestTheClusterAdministratorReadsTheDNSStatusOfEveryDomain(t *testing.T) {
	op := &domainsOperator{}
	h := startWith(t, op)
	path := "/v1/clusters/" + directortest.Cluster + "/domains"

	code, out := h.do(t, "GET", path, h.token(t, "gentian", "alice"), "")
	if code != http.StatusOK {
		t.Fatalf("GET %s = %d %v", path, code, out)
	}
	domains, _ := out["domains"].([]any)
	if len(domains) != 1 {
		t.Fatalf("domains = %v", out)
	}
	d := domains[0].(map[string]any)
	record := d["records"].([]any)[0].(map[string]any)
	if d["domain"] != "gentian.org" || record["state"] != "heldByOther" || record["got"].([]any)[0] != "16 route1.mx.cloudflare.net." {
		t.Fatalf("the domain as passed on = %v", d)
	}
	if code, _ := h.do(t, "GET", path, h.token(t, "tenant-demo", "tom"), ""); code != http.StatusForbidden {
		t.Fatalf("a tenant administrator read the cluster's DNS status: %d", code)
	}
}
