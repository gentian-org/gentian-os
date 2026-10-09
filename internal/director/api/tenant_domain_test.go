/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package api_test

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	dt "github.com/gentian-org/gentian-os/internal/director/directortest"
	"github.com/gentian-org/gentian-os/internal/director/gitops"
)

func domainRoute(tenant string) string {
	return "/v1/clusters/" + dt.Cluster + "/tenants/" + tenant + "/domain"
}

func domainFile(tenant string) string {
	return strings.TrimSuffix(dt.TenantPath(tenant), "tenant.yaml") + gitops.TenantDomainFile
}

// On a cluster for many tenants a tenant for users is bound to a domain of
// its own and put back, as before.
func TestATenantOfAManyTenantClusterIsBoundToADomain(t *testing.T) {
	h := start(t)
	alice := h.token(t, "gentian", "alice") // platform administrator
	if code, out := h.do(t, "PUT", domainRoute("demo"), alice, `{"domain":"acme.example"}`); code >= 300 {
		t.Fatalf("bind: %d %v", code, out)
	}
	path := domainFile("demo")
	if doc := dt.RemoteFile(t, h.remote, path); !strings.Contains(doc, "domain: acme.example") {
		t.Fatalf("the binding is not in git:\n%s", doc)
	}
	if code, out := h.do(t, "DELETE", domainRoute("demo"), alice, ""); code >= 300 {
		t.Fatalf("unbind: %d %v", code, out)
	}
}

// A single-tenancy cluster's user tenant is on the cluster's own addresses.
// Bound to a domain it would leave them and give up the main address, so a
// bind is refused there for every tenant, with the reason, and nothing is
// committed. Unbinding stays open: a tenant that was bound anyway is put
// back with it.
func TestBindingADomainIsRefusedOnASingleTenancyCluster(t *testing.T) {
	// solo was bound before the cluster refused it: the binding is in git
	// as the director wrote it, beside the tenant and in its kustomization.
	path := domainFile("solo")
	h := startSeeded(t, nil, func(remote string) {
		dt.Commit(t, remote, map[string]string{
			dt.ClaimPath: singleClaim(),
			path:         "apiVersion: gentianos.io/v1alpha1\nkind: TenantDomain\nmetadata:\n  name: solo\nspec:\n  domain: held.example\n",
			strings.TrimSuffix(path, gitops.TenantDomainFile) + "kustomization.yaml": "resources:\n- tenant.yaml\n- " + gitops.TenantDomainFile + "\n",
		})
	})
	alice := h.token(t, "gentian", "alice")
	before := h.tip(t)
	for _, tenant := range []string{"demo", "solo"} {
		code, out := h.do(t, "PUT", domainRoute(tenant), alice, `{"domain":"acme.example"}`)
		if code != http.StatusUnprocessableEntity {
			t.Fatalf("%s: bind = %d %v, want 422", tenant, code, out)
		}
		msg := fmt.Sprint(out["error"])
		for _, want := range []string{"tenancy mode is single", "main address", "many tenants", "Nothing was changed"} {
			if !strings.Contains(msg, want) {
				t.Errorf("%s: the refusal does not say %q: %s", tenant, want, msg)
			}
		}
	}
	if h.tip(t) != before {
		t.Fatal("a refused bind moved the repository")
	}
	if code, out := h.do(t, "DELETE", domainRoute("solo"), alice, ""); code >= 300 {
		t.Fatalf("unbind on a single-tenancy cluster: %d %v", code, out)
	}
	if h.tip(t) == before {
		t.Fatal("unbinding committed nothing")
	}
	if left := dt.Git(t, "", "--git-dir", h.remote, "ls-tree", "-r", "--name-only", "main"); strings.Contains(left, path) {
		t.Fatalf("the binding is still in git:\n%s", left)
	}
}

// The platform tenant stays on the cluster's own addresses under either
// tenancy mode: a bind is refused with the reason, and unbinding is not.
func TestBindingADomainToThePlatformTenantIsRefused(t *testing.T) {
	h := start(t)
	alice := h.token(t, "gentian", "alice")
	adoptKernelRealm(t, h, "demo")
	before := h.tip(t)
	code, out := h.do(t, "PUT", domainRoute("demo"), alice, `{"domain":"acme.example"}`)
	if code != http.StatusUnprocessableEntity || out["error"] != gitops.ErrPlatformTenantDomain.Error() {
		t.Fatalf("bind = %d %v, want 422 with the reason", code, out)
	}
	msg := gitops.ErrPlatformTenantDomain.Error()
	if !strings.Contains(msg, "platform tenant") || !strings.Contains(msg, "Nothing was changed") {
		t.Fatalf("the refusal does not say what was refused: %q", msg)
	}
	if h.tip(t) != before {
		t.Fatal("a refused bind moved the repository")
	}
	if code, out := h.do(t, "DELETE", domainRoute("demo"), alice, ""); code >= 300 {
		t.Fatalf("unbind: %d %v", code, out)
	}
	// The tenant beside it is bound as before.
	if code, out := h.do(t, "PUT", domainRoute("solo"), alice, `{"domain":"acme.example"}`); code >= 300 {
		t.Fatalf("a user tenant beside it: %d %v", code, out)
	}
}
