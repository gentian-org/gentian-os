/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package api_test

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"
	"testing"

	"github.com/gentian-org/gentian-os/internal/director/api"
	dt "github.com/gentian-org/gentian-os/internal/director/directortest"
)

const shopKey = "the-shop-s-own-key-0123456789abcdef"

func withCommissioner(tenants ...string) func(*api.Config) {
	return func(cfg *api.Config) {
		sum := sha256.Sum256([]byte(shopKey))
		keys := &api.CommissionKeys{}
		keys.Set([]api.CommissionKey{{
			Tenant: "other", Component: "shop", KeyHash: hex.EncodeToString(sum[:]), Tenants: tenants,
		}})
		cfg.CommissionKeys = keys
	}
}

const abcLink = `{"displayName": "ABC AG", "issuer": "https://id.abc.example/auth/realms/main",
	"clientID": "broker-to-aluvian", "domain": "abc.example"}`

// A component the tenant names commissions an outside realm with its own
// key: one commit, which says the component asked and for which tenant. It
// reads the list back and takes the entry away the same way.
func TestACommissionerLinksAnOutsideRealmWithItsKey(t *testing.T) {
	h := startWith(t, nil, withCommissioner("demo"))
	before := h.tip(t)
	code, body := h.do(t, "PUT", "/v1/tenants/demo/brokered-from/abc", shopKey, abcLink)
	if code != http.StatusAccepted || h.tip(t) == before {
		t.Fatalf("the commissioner's write: %d %v", code, body)
	}
	message := dt.Git(t, "", "--git-dir", h.remote, "log", "-1", "--format=%B", "main")
	if !strings.Contains(message, "component:other/shop") || !strings.Contains(message, "commissions_organizations tenant:demo") {
		t.Fatalf("the commit does not say who asked:\n%s", message)
	}
	code, body = h.do(t, "GET", "/v1/tenants/demo/brokered-from", shopKey, "")
	list, _ := body["brokeredFrom"].([]any)
	if code != http.StatusOK || len(list) != 1 || list[0].(map[string]any)["domain"] != "abc.example" {
		t.Fatalf("read back: %d %v", code, body)
	}

	// The same domain under another name is another organization's.
	if code, body := h.do(t, "PUT", "/v1/tenants/demo/brokered-from/abc2", shopKey, abcLink); code != http.StatusConflict {
		t.Fatalf("a second entry for one domain: %d %v", code, body)
	}

	code, body = h.do(t, "DELETE", "/v1/tenants/demo/brokered-from/abc", shopKey, "")
	if code != http.StatusAccepted {
		t.Fatalf("the commissioner's removal: %d %v", code, body)
	}
	if code, body := h.do(t, "DELETE", "/v1/tenants/demo/brokered-from/abc", shopKey, ""); code != http.StatusOK || body["status"] != "unchanged" {
		t.Fatalf("removing what is not there: %d %v", code, body)
	}
}

// The key is good for the tenants that name the component and nothing else:
// not another tenant's outside realms, and no other route.
func TestACommissionersKeyOpensOnlyItsTenantsOutsideRealms(t *testing.T) {
	h := startWith(t, nil, withCommissioner("demo"))
	before := h.tip(t)
	if code, body := h.do(t, "PUT", "/v1/tenants/solo/brokered-from/abc", shopKey, abcLink); code != http.StatusForbidden {
		t.Fatalf("another tenant: %d %v", code, body)
	}
	if code, body := h.do(t, "POST", "/v1/tenants/demo/apps/element", shopKey, ""); code != http.StatusUnauthorized {
		t.Fatalf("another route: %d %v", code, body)
	}
	if code, body := h.do(t, "PUT", "/v1/tenants/demo/brokered-from/abc", shopKey+"x", abcLink); code != http.StatusUnauthorized {
		t.Fatalf("another key: %d %v", code, body)
	}
	if h.tip(t) != before {
		t.Fatal("a refused request committed")
	}
}

// A person writes them as an administrator of the cluster, not of the
// tenant: who signs in to the cluster is the cluster's decision.
func TestOnlyTheClustersAdministratorsLinkOutsideRealms(t *testing.T) {
	h := startWith(t, nil)
	if code, body := h.do(t, "PUT", "/v1/tenants/demo/brokered-from/abc", h.token(t, "tenant-demo", "tom"), abcLink); code != http.StatusForbidden {
		t.Fatalf("the tenant's administrator: %d %v", code, body)
	}
	if code, body := h.do(t, "PUT", "/v1/tenants/demo/brokered-from/abc", h.token(t, "gentian", "alice"), abcLink); code != http.StatusAccepted {
		t.Fatalf("the cluster's administrator: %d %v", code, body)
	}
}

// What would let an outside realm claim this cluster's own people's
// addresses, or another domain than its own, is refused before a commit.
func TestAnOutsideRealmDoesNotClaimTheClustersDomain(t *testing.T) {
	h := startWith(t, nil, withCommissioner("demo"))
	before := h.tip(t)
	for name, body := range map[string]string{
		"the kernel domain": `{"displayName": "K", "issuer": "https://id.` + dt.KernelDomain + `/auth/realms/x",
			"clientID": "c", "domain": "` + dt.KernelDomain + `"}`,
		"below the kernel domain": `{"displayName": "K", "issuer": "https://id.team.` + dt.KernelDomain + `/auth/realms/x",
			"clientID": "c", "domain": "team.` + dt.KernelDomain + `"}`,
		"not the issuer's cluster's": `{"displayName": "G", "issuer": "https://id.abc.example/auth/realms/x",
			"clientID": "c", "domain": "gmail.com"}`,
	} {
		if code, out := h.do(t, "PUT", "/v1/tenants/demo/brokered-from/x", shopKey, body); code != http.StatusUnprocessableEntity {
			t.Errorf("%s: %d %v", name, code, out)
		}
	}
	if h.tip(t) != before {
		t.Fatal("a refused request committed")
	}
}

// An entry that links existing accounts (the store's sign-in through a
// customer tenant's realm) has no organization: it names no domain, takes
// none, and stands beside an organization of the issuer's domain.
func TestAnEntryThatLinksExistingAccountsNamesNoDomain(t *testing.T) {
	h := startWith(t, nil, withCommissioner("demo"))
	if code, body := h.do(t, "PUT", "/v1/tenants/demo/brokered-from/abc", shopKey, abcLink); code != http.StatusAccepted {
		t.Fatalf("the organization: %d %v", code, body)
	}
	link := `{"displayName": "abc.example (0f8e7d6c)", "issuer": "https://id.abc.example/auth/realms/main",
		"clientID": "broker-to-store", "linkExisting": true}`
	name := "0f8e7d6c-5b4a-4321-9876-0123456789ab"
	if code, body := h.do(t, "PUT", "/v1/tenants/demo/brokered-from/"+name, shopKey, link); code != http.StatusAccepted {
		t.Fatalf("the link: %d %v", code, body)
	}
	patch := dt.RemoteFile(t, h.remote, strings.TrimSuffix(dt.TenantPath("demo"), "tenant.yaml")+"brokered-from.yaml")
	if !strings.Contains(patch, "    - name: "+name+"\n") || !strings.Contains(patch, "      linkExisting: true\n") {
		t.Fatalf("the patch:\n%s", patch)
	}
	withDomain := strings.Replace(link, `"linkExisting": true`, `"linkExisting": true, "domain": "abc.example"`, 1)
	if code, body := h.do(t, "PUT", "/v1/tenants/demo/brokered-from/"+name, shopKey, withDomain); code != http.StatusUnprocessableEntity {
		t.Fatalf("a link naming a domain: %d %v", code, body)
	}
}
