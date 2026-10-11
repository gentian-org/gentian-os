/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package custodian

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	gentianv1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

func listed(t *testing.T, s *Server) (int, []string) {
	t.Helper()
	w := do(t, s, http.MethodGet, "/v1/credentials", "")
	var body struct {
		Credentials []struct {
			Name string `json:"name"`
		} `json:"credentials"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	var names []string
	for _, c := range body.Credentials {
		names = append(names, c.Name)
	}
	return w.Code, names
}

// What OpenBao says about a caller's token decides nothing. Here it reports
// the cluster administrator's policy and a tenant, and the store says the
// caller holds nothing: nothing is shown and nothing can be set.
func TestOpenBaosOpinionOfTheCallerDecidesNothing(t *testing.T) {
	s, _ := newServerAs(t, []string{"cluster-admin"}, map[string]string{"username": "eve", "tenant": "acme"},
		requirement("smtp-relay", "cluster", "gentian-os/kernel/mail/relay", 0),
		tenantRequirement("acme-smtp", "acme", "gentian-os/tenants/acme/mail"))
	s.Authz = &fakeStore{who: Principal{User: "user:eve", Tenant: "acme"}, held: map[string]bool{}}

	code, names := listed(t, s)
	if code != http.StatusOK || len(names) != 0 {
		t.Fatalf("list = %d %v, want an empty list", code, names)
	}
	if w := do(t, s, http.MethodPut, "/v1/credentials/smtp-relay", `{"fields":{"password":"correct-horse"}}`); w.Code != http.StatusNotFound {
		t.Fatalf("set of a credential the caller may not see = %d", w.Code)
	}
	// And they stand in no tenant, whatever realm signed them in.
	if w := do(t, s, http.MethodGet, "/v1/backup-identity", ""); w.Code != http.StatusForbidden {
		t.Fatalf("backup identity for a caller who administers nothing = %d", w.Code)
	}
}

// An auditor reads and does not write: they see that a credential is required
// and whether it is set, and are refused when they try to set it.
func TestAnAuditorSeesACredentialAndCannotSetIt(t *testing.T) {
	s, _ := newServerAs(t, nil, map[string]string{"username": "audrey"},
		requirement("smtp-relay", "cluster", "gentian-os/kernel/mail/relay", 0),
		tenantRequirement("acme-smtp", "acme", "gentian-os/tenants/acme/mail"))
	s.Authz = &fakeStore{who: Principal{User: "user:audrey"}, held: map[string]bool{
		relationRead + " cluster:test": true,
		relationRead + " tenant:acme":  true,
	}}

	code, names := listed(t, s)
	if code != http.StatusOK || strings.Join(names, " ") != "acme-smtp smtp-relay" {
		t.Fatalf("list = %d %v", code, names)
	}
	for _, name := range []string{"smtp-relay", "acme-smtp"} {
		w := do(t, s, http.MethodPut, "/v1/credentials/"+name, `{"fields":{"password":"correct-horse"}}`)
		if w.Code != http.StatusForbidden {
			t.Fatalf("an auditor setting %s = %d %s", name, w.Code, w.Body.String())
		}
	}
}

// A tenant's administrator holds their own tenant's and nothing of the
// cluster's or of another tenant's.
func TestATenantAdministratorHoldsTheirTenantsCredentialsOnly(t *testing.T) {
	s, _ := newServerAsTenant(t, "acme",
		requirement("smtp-relay", "cluster", "gentian-os/kernel/mail/relay", 0),
		tenantRequirement("acme-smtp", "acme", "gentian-os/tenants/acme/mail"),
		tenantRequirement("other-smtp", "other", "gentian-os/tenants/other/mail"))
	code, names := listed(t, s)
	if code != http.StatusOK || strings.Join(names, " ") != "acme-smtp" {
		t.Fatalf("list = %d %v", code, names)
	}
	if w := do(t, s, http.MethodPut, "/v1/credentials/acme-smtp", `{"fields":{"password":"correct-horse"}}`); w.Code != http.StatusOK {
		t.Fatalf("setting their own = %d %s", w.Code, w.Body.String())
	}
	if w := do(t, s, http.MethodPut, "/v1/credentials/other-smtp", `{"fields":{"password":"correct-horse"}}`); w.Code != http.StatusNotFound {
		t.Fatalf("setting another tenant's = %d", w.Code)
	}
}

// A store that does not answer fails the request. An empty list in its place
// would tell an administrator that nothing is required.
func TestAStoreThatDoesNotAnswerFailsTheRequest(t *testing.T) {
	s, _ := newServer(t, requirement("smtp-relay", "cluster", "gentian-os/kernel/mail/relay", 0))
	s.Authz = &fakeStore{who: Principal{User: "user:alice"}, err: errors.New("down")}
	if code, names := listed(t, s); code != http.StatusServiceUnavailable || names != nil {
		t.Fatalf("list = %d %v, want 503 and nothing", code, names)
	}
	// And with no store configured at all there is nobody to ask.
	s.Authz = nil
	if code, _ := listed(t, s); code != http.StatusServiceUnavailable {
		t.Fatalf("list with no store = %d, want 503", code)
	}
}

// The store is asked before OpenBao is: a caller the store does not know is
// refused without their token ever being offered to the vault.
func TestATokenThatDoesNotVerifyNeverReachesOpenBao(t *testing.T) {
	s, _ := newServer(t)
	s.Authz = refusing{}
	exchanged := false
	s.Bao = nil
	defer func() {
		if r := recover(); r != nil {
			exchanged = true
		}
		if exchanged {
			t.Fatal("OpenBao was reached for a caller who could not be identified")
		}
	}()
	if w := do(t, s, http.MethodGet, "/v1/credentials", ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("an unverifiable token = %d", w.Code)
	}
}

type refusing struct{}

func (refusing) Identify(context.Context, string) (Principal, error) {
	return Principal{}, ErrUnauthenticated
}
func (refusing) Check(context.Context, string, string, string) (bool, error) { return false, nil }
func (refusing) Object(string, string) (string, bool)                        { return "", false }

// A realm is its tenant's by name unless the tenant says otherwise; a realm no
// tenant has belongs to nobody.
func TestTheTenantOfARealm(t *testing.T) {
	scheme := testScheme(t)
	plain := &gentianv1alpha1.Tenant{ObjectMeta: metav1.ObjectMeta{Name: "acme"}}
	renamed := &gentianv1alpha1.Tenant{ObjectMeta: metav1.ObjectMeta{Name: "globex"}}
	renamed.Spec.Isolation = &gentianv1alpha1.TenantIsolation{KeycloakRealm: "globex-people"}
	a := &GraphAuthorizer{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(plain, renamed).Build()}
	for realm, want := range map[string]string{"acme": "acme", "globex-people": "globex", "globex": "", "nobody": ""} {
		got, err := a.tenantOfRealm(context.Background(), realm)
		if err != nil || got != want {
			t.Errorf("realm %q -> %q (%v), want %q", realm, got, err, want)
		}
	}
	// The store's names for the two scopes.
	a.Cluster = "c1"
	if obj, ok := a.Object(scopeCluster, ""); !ok || obj != "cluster:c1" {
		t.Errorf("cluster object = %q %v", obj, ok)
	}
	if obj, ok := a.Object(scopeTenant, "acme"); !ok || obj != "tenant:acme" {
		t.Errorf("tenant object = %q %v", obj, ok)
	}
	if _, ok := a.Object(scopeTenant, ""); ok {
		t.Error("a tenant scope with no tenant has an object")
	}
}

// The vault is shown the custodian's token and never the caller's. Every
// request the stand-in vault receives is looked at: a write, the metadata
// that records who set it, the reads behind a listing.
func TestTheCallersTokenNeverReachesTheVault(t *testing.T) {
	var seen []string
	vault := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Method+" "+r.URL.Path+" token="+r.Header.Get("X-Vault-Token")+" auth="+r.Header.Get("Authorization"))
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), "caller-oidc-token") {
			t.Errorf("the caller's token was sent to the vault in a body: %s %s", r.Method, r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"data":{"current_version":1,"custom_metadata":{}}}`))
	}))
	defer vault.Close()

	s, _ := newServer(t, requirement("smtp-relay", "cluster", "gentian-os/kernel/mail/relay", 0))
	b := NewOpenBao(vault.URL, "secret", "gentian-os-custodian", nil, false)
	b.SetStaticToken("custodian-token")
	s.Bao = b

	if w := do(t, s, http.MethodPut, "/v1/credentials/smtp-relay", `{"fields":{"password":"correct-horse"}}`); w.Code != http.StatusOK {
		t.Fatalf("set = %d %s", w.Code, w.Body.String())
	}
	if code, _ := listed(t, s); code != http.StatusOK {
		t.Fatalf("list = %d", code)
	}
	if len(seen) == 0 {
		t.Fatal("the vault was never reached, so this test showed nothing")
	}
	for _, req := range seen {
		if strings.Contains(req, "caller-oidc-token") {
			t.Errorf("the caller's token reached the vault: %s", req)
		}
		if !strings.Contains(req, "token=custodian-token") {
			t.Errorf("a request to the vault was not made as the custodian: %s", req)
		}
	}
}
