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
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func repoObj(name, tenant, url, role string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(repositoryGVK)
	u.SetNamespace(repositoryNamespace)
	u.SetName(name)
	spec := map[string]any{
		"type":      "git",
		"role":      role,
		"endpoints": map[string]any{"inCluster": url},
	}
	if tenant != "" {
		spec["tenant"] = tenant
	}
	u.Object["spec"] = spec
	return u
}

func repoBody(role, url, confirm string) string {
	return fmt.Sprintf(`{"role":%q,"type":"git","url":%q,"confirm":%q}`, role, url, confirm)
}

// TestTenantSeesOwnAndClusterRepositoriesOnly — a tenant needs the cluster's
// base repository in the list, because that is where most of their apps come
// from. Another tenant's is not theirs to know about.
func TestTenantSeesOwnAndClusterRepositoriesOnly(t *testing.T) {
	s, _ := newServerAsTenant(t, "acme",
		repoObj("base", "", "https://git.example/base", roleApps),
		repoObj("acme-private", "acme", "https://git.example/acme", roleApps),
		repoObj("globex-private", "globex", "https://git.example/globex", roleApps),
	)
	w := do(t, s, "GET", "/v1/repositories", "")
	body := w.Body.String()

	if strings.Contains(body, "globex-private") {
		t.Fatalf("another tenant's repository was listed:\n%s", body)
	}
	for _, want := range []string{"base", "acme-private"} {
		if !strings.Contains(body, want) {
			t.Fatalf("expected %q in the listing:\n%s", want, body)
		}
	}

	var got struct{ Repositories []RepositoryView }
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	for _, r := range got.Repositories {
		switch r.Name {
		case "base":
			if r.Owned {
				t.Fatal("the cluster's repository is marked owned by a tenant")
			}
		case "acme-private":
			if !r.Owned {
				t.Fatal("a tenant's own repository is not marked owned")
			}
		}
	}
}

// TestTenantCannotTouchAnotherTenantsRepository — and gets 404, not 403, since
// learning a name is taken is itself a disclosure.
func TestTenantCannotTouchAnotherTenantsRepository(t *testing.T) {
	s, _ := newServerAsTenant(t, "acme",
		repoObj("globex-private", "globex", "https://git.example/globex", roleApps),
	)

	w := do(t, s, "PUT", "/v1/repositories/globex-private",
		repoBody(roleApps, "https://evil.example/x", "globex-private"))
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404 writing another tenant's repository, got %d: %s", w.Code, w.Body.String())
	}

	w = do(t, s, "DELETE", "/v1/repositories/globex-private?confirm=globex-private", "")
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404 deleting another tenant's repository, got %d: %s", w.Code, w.Body.String())
	}
}

// TestAddingAnAppsRepositoryIsAdditive — the ordinary case must not demand
// ceremony, or the ceremony stops meaning anything where it matters.
func TestAddingAnAppsRepositoryIsAdditive(t *testing.T) {
	s, _ := newServerAsTenant(t, "acme")
	w := do(t, s, "PUT", "/v1/repositories/acme-private",
		repoBody(roleApps, "https://git.example/acme", ""))
	if w.Code != http.StatusOK {
		t.Fatalf("adding a private apps repository should need no confirmation, got %d: %s",
			w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"credentialName":"repository-acme-private"`) {
		t.Fatalf("response does not link the credential to supply:\n%s", w.Body.String())
	}
}

// TestDeploymentsRepositoryAlwaysConfirms — even when new, because it redirects
// everything the tenant reconciles.
func TestDeploymentsRepositoryAlwaysConfirms(t *testing.T) {
	s, _ := newServerAsTenant(t, "acme")

	w := do(t, s, "PUT", "/v1/repositories/acme-deploy",
		repoBody(roleDeployments, "https://git.example/acme-deploy", ""))
	if w.Code != http.StatusPreconditionRequired {
		t.Fatalf("expected 428 for a deployments repository, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"requiresRetype":true`) {
		t.Fatalf("response does not tell the console to make the operator retype:\n%s", w.Body.String())
	}

	// The wrong name must not pass — that is the whole point of retyping.
	w = do(t, s, "PUT", "/v1/repositories/acme-deploy",
		repoBody(roleDeployments, "https://git.example/acme-deploy", "acme-deployy"))
	if w.Code != http.StatusPreconditionRequired {
		t.Fatalf("a mistyped confirmation was accepted, got %d: %s", w.Code, w.Body.String())
	}

	w = do(t, s, "PUT", "/v1/repositories/acme-deploy",
		repoBody(roleDeployments, "https://git.example/acme-deploy", "acme-deploy"))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 once confirmed, got %d: %s", w.Code, w.Body.String())
	}
}

// TestReplacingASourceConfirms — changing where an existing repository points
// is a replacement, not an edit.
func TestReplacingASourceConfirms(t *testing.T) {
	s, _ := newServerAsTenant(t, "acme",
		repoObj("acme-private", "acme", "https://git.example/old", roleApps),
	)

	w := do(t, s, "PUT", "/v1/repositories/acme-private",
		repoBody(roleApps, "https://git.example/new", ""))
	if w.Code != http.StatusPreconditionRequired {
		t.Fatalf("expected 428 when repointing a repository, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "https://git.example/old") {
		t.Fatalf("the warning does not name what is being replaced:\n%s", w.Body.String())
	}

	// Re-submitting the same URL is not a replacement and needs no confirmation.
	w = do(t, s, "PUT", "/v1/repositories/acme-private",
		repoBody(roleApps, "https://git.example/old", ""))
	if w.Code != http.StatusOK {
		t.Fatalf("an unchanged URL should not require confirmation, got %d: %s", w.Code, w.Body.String())
	}
}

// TestDeleteAlwaysConfirms — removal stops apps reconciling, with no additive
// case to exempt.
func TestDeleteAlwaysConfirms(t *testing.T) {
	s, _ := newServerAsTenant(t, "acme",
		repoObj("acme-private", "acme", "https://git.example/acme", roleApps),
	)
	if w := do(t, s, "DELETE", "/v1/repositories/acme-private", ""); w.Code != http.StatusPreconditionRequired {
		t.Fatalf("expected 428 deleting without confirmation, got %d: %s", w.Code, w.Body.String())
	}
	if w := do(t, s, "DELETE", "/v1/repositories/acme-private?confirm=acme-private", ""); w.Code != http.StatusOK {
		t.Fatalf("expected 200 deleting with confirmation, got %d: %s", w.Code, w.Body.String())
	}
}

// TestVaultPathStaysInsideTheTenantPrefix is the one the caller cannot
// influence. A path outside it produces a requirement the tenant can see, and
// OpenBao refuses the write against — visible and unsatisfiable.
func TestVaultPathStaysInsideTheTenantPrefix(t *testing.T) {
	if got := repositoryVaultPath("acme", "private"); got != "gentian-os/tenants/acme/repositories/private" {
		t.Fatalf("tenant path is outside the tenant prefix: %s", got)
	}
	if got := repositoryVaultPath("", "base"); got != "gentian-os/kernel/repositories/base" {
		t.Fatalf("cluster path is not under kernel: %s", got)
	}
}

// TestRepositoryRequestValidation rejects what the composition or OpenBao would
// reject later, where the message can still name the field.
func TestRepositoryRequestValidation(t *testing.T) {
	cases := []struct{ name, body, want string }{
		{"bad type", `{"role":"apps","type":"svn","url":"https://x"}`, "type must be git or oci"},
		{"bad role", `{"role":"secrets","type":"git","url":"https://x"}`, "role must be"},
		{"empty url", `{"role":"apps","type":"git","url":""}`, "url is required"},
		{"padded url", `{"role":"apps","type":"git","url":" https://x "}`, "whitespace"},
		{"oci deployments", `{"role":"deployments","type":"oci","url":"oci://x"}`, "must be git"},
	}
	s, _ := newServerAsTenant(t, "acme")
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := do(t, s, "PUT", "/v1/repositories/x", tc.body)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
			}
			if !strings.Contains(w.Body.String(), tc.want) {
				t.Fatalf("error does not name the problem (%s):\n%s", tc.want, w.Body.String())
			}
		})
	}
}
