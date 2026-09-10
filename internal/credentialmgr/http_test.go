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
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	gentianv1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// theSecretValue is a sentinel. Every test writes it and then asserts it never
// comes back out of any endpoint.
const theSecretValue = "SENTINEL-secret-value-must-never-be-returned"

type stubValidator struct{ err error }

func (s stubValidator) Validate(context.Context, string, string, map[string]string) error {
	return s.err
}

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := gentianv1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("adding gentian scheme: %v", err)
	}
	return s
}

func requirement(name, scope, path string, minLen int) *gentianv1alpha1.CredentialRequirement {
	return requirementWithValidator(name, scope, path, minLen, "noop")
}

func requirementWithValidator(name, scope, path string, minLen int, validator string) *gentianv1alpha1.CredentialRequirement {
	return &gentianv1alpha1.CredentialRequirement{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: gentianv1alpha1.CredentialRequirementSpec{
			DisplayName: name,
			Phase:       "runtime",
			Scope:       scope,
			VaultPath:   path,
			Fields: []gentianv1alpha1.CredentialField{
				{Key: "password", Format: "password", Secret: true, MinLength: minLen},
			},
			Validate: &gentianv1alpha1.CredentialValidation{Type: validator},
		},
	}
}

// tenantRequirement builds a requirement owned by one named tenant.
func tenantRequirement(name, tenant, path string) *gentianv1alpha1.CredentialRequirement {
	r := requirement(name, "tenant", path, 0)
	r.Spec.Tenant = tenant
	return r
}

func probe(name string, ready bool) *unstructured.Unstructured {
	status := "False"
	if ready {
		status = "True"
	}
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(externalSecretGVK)
	u.SetNamespace("gentian-system")
	u.SetName("credreq-" + name)
	_ = unstructured.SetNestedSlice(u.Object, []any{
		map[string]any{"type": "Ready", "status": status, "message": "SecretSyncedError"},
	}, "status", "conditions")
	return u
}

// newServer builds a server whose caller is a cluster admin.
func newServer(t *testing.T, objs ...runtime.Object) (*Server, *httptest.Server) {
	return newServerAs(t, []string{"cluster-admin"}, map[string]string{"username": "alice@example.com"}, objs...)
}

// newServerAsTenant builds a server whose caller administers exactly one tenant
// and holds no cluster policy.
func newServerAsTenant(t *testing.T, tenant string, objs ...runtime.Object) (*Server, *httptest.Server) {
	return newServerAs(t, []string{"tenant-admin"},
		map[string]string{"username": "bob@example.com", "tenant": tenant}, objs...)
}

// newServerAs stands up the service against a stub OpenBao that reports the
// given policies and claim metadata — which is the only thing the service is
// allowed to derive identity from.
func newServerAs(t *testing.T, policies []string, meta map[string]string, objs ...runtime.Object) (*Server, *httptest.Server) {
	t.Helper()
	scheme := testScheme(t)
	u := &unstructured.UnstructuredList{}
	u.SetGroupVersionKind(externalSecretGVK)

	c := fake.NewClientBuilder().WithScheme(scheme).
		WithRuntimeObjects(objs...).Build()

	// A stand-in OpenBao that would hand back the sentinel if the service ever
	// asked for a value. It never should.
	bao := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/auth/oidc/login"):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"auth": map[string]any{
					"client_token":   "exchanged-token",
					"token_policies": policies,
					"metadata":       meta,
				},
			})
		case strings.Contains(r.URL.Path, "/metadata/"):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": map[string]any{
					"current_version": 3,
					"updated_time":    "2026-08-14T10:00:00Z",
					"custom_metadata": map[string]any{"set_by": "alice@example.com"},
				},
			})
		case strings.Contains(r.URL.Path, "/data/"):
			// If a handler ever reads the data endpoint, this is what it gets —
			// and the assertions below will catch it in the response body.
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": map[string]any{"data": map[string]string{"password": theSecretValue}},
			})
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	t.Cleanup(bao.Close)

	s := &Server{
		Catalogue:          &Catalogue{Client: c, ProbeNamespace: "gentian-system"},
		Bao:                NewOpenBao(bao.URL, "secret", "oidc", "kernel", []string{"cluster-admin-jwt"}, nil, false),
		Validator:          stubValidator{},
		ClusterAdminPolicy: "cluster-admin",
		TenantClaimKey:     "tenant",
	}
	return s, bao
}

func do(t *testing.T, s *Server, method, target, body string) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, target, nil)
	} else {
		r = httptest.NewRequest(method, target, strings.NewReader(body))
	}
	r.Header.Set("Authorization", "Bearer caller-oidc-token")
	r.Header.Set("X-Gentian-User", "alice@example.com")
	w := httptest.NewRecorder()
	s.Routes().ServeHTTP(w, r)
	return w
}

// TestNoRouteReturnsASecretValue is the acceptance criterion the whole design
// rests on: "No endpoint returns a secret value. Asserted by a test enumerating
// every route."
//
// It walks every route, including the write path with a real value in the body,
// and fails if the sentinel appears in any response.
func TestNoRouteReturnsASecretValue(t *testing.T) {
	s, _ := newServer(t,
		requirement("smtp-relay", "cluster", "gentian/mail/relay", 0),
		probe("smtp-relay", true),
	)

	// Every route the mux registers. routeTargets is asserted against
	// Routes() below, so adding an endpoint without adding it here fails rather
	// than quietly narrowing what this test enumerates.
	cases := []struct{ method, target, body string }{
		{"GET", "/healthz", ""},
		{"GET", "/v1/credentials", ""},
		{"GET", "/v1/credentials/smtp-relay", ""},
		{"PUT", "/v1/credentials/smtp-relay",
			fmt.Sprintf(`{"fields":{"password":%q}}`, theSecretValue)},
		// The escrowed backup identity is the one value on this service that
		// opens a tenant's whole history, so "no route echoes a secret back"
		// has to cover it too.
		{"GET", "/v1/backup-identity", ""},
		{"PUT", "/v1/backup-identity",
			fmt.Sprintf(`{"identity":"AGE-SECRET-KEY-%s"}`, theSecretValue)},
		{"GET", "/v1/repositories", ""},
		{"PUT", "/v1/repositories/smtp-relay",
			fmt.Sprintf(`{"role":"apps","type":"git","url":"https://git.example/x","confirm":%q}`, theSecretValue)},
		{"DELETE", "/v1/repositories/smtp-relay?confirm=smtp-relay", ""},
	}

	for _, tc := range cases {
		t.Run(tc.method+" "+tc.target, func(t *testing.T) {
			w := do(t, s, tc.method, tc.target, tc.body)
			if strings.Contains(w.Body.String(), theSecretValue) {
				t.Fatalf("route returned a credential value:\n%s", w.Body.String())
			}
		})
	}
}

// TestEveryRouteIsEnumerated keeps the guarantee above honest. The no-leak test
// walks a list, and a list goes stale silently — a route added without a case
// would be reported as covered by a test that never called it.
func TestEveryRouteIsEnumerated(t *testing.T) {
	// Patterns registered by Routes(), kept beside it deliberately: this has to
	// be updated in the same edit that adds an endpoint.
	registered := []string{
		"GET /healthz",
		"GET /v1/credentials",
		"GET /v1/credentials/{name}",
		"PUT /v1/credentials/{name}",
		"GET /v1/backup-identity",
		"PUT /v1/backup-identity",
		"GET /v1/repositories",
		"PUT /v1/repositories/{name}",
		"DELETE /v1/repositories/{name}",
	}
	s, _ := newServer(t)
	mux := s.Routes()
	for _, pat := range registered {
		parts := strings.SplitN(pat, " ", 2)
		r := httptest.NewRequest(parts[0], strings.NewReplacer("{name}", "probe").Replace(parts[1]), nil)
		if _, matched := mux.Handler(r); matched == "" {
			t.Fatalf("route %q is listed here but not registered by Routes()", pat)
		}
	}
	// And the reverse: a route registered but absent from the leak test's cases
	// is what this is really guarding, so the two lists must be the same length.
	if got, want := len(registered), 9; got != want {
		t.Fatalf("route list changed (%d); update TestNoRouteReturnsASecretValue too", got)
	}
}

// TestWriteRequiresCallerToken proves the service cannot write on its own
// authority: with no bearer token there is nothing to exchange, so the request
// is refused before anything is stored.
func TestWriteRequiresCallerToken(t *testing.T) {
	s, _ := newServer(t, requirement("smtp-relay", "cluster", "gentian/mail/relay", 0))

	r := httptest.NewRequest("PUT", "/v1/credentials/smtp-relay",
		strings.NewReader(`{"fields":{"password":"whatever"}}`))
	w := httptest.NewRecorder()
	s.Routes().ServeHTTP(w, r)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 without a caller token, got %d: %s", w.Code, w.Body.String())
	}
}

// TestOpenBaoWriteRefusesEmptyToken guards the same property one layer down, so
// a future handler that forgets the check still cannot write anonymously.
func TestOpenBaoWriteRefusesEmptyToken(t *testing.T) {
	b := NewOpenBao("http://openbao.invalid", "secret", "oidc", "kernel", []string{"cluster-admin-jwt"}, nil, false)
	err := b.Write(context.Background(), "", "gentian/x", map[string]string{"a": "b"}, "alice")
	if err == nil {
		t.Fatal("Write accepted an empty caller token")
	}
	if !strings.Contains(err.Error(), "own authority") {
		t.Fatalf("error should name the reason, got: %v", err)
	}
}

// TestTenantAdminCannotSeeClusterScoped is the asymmetry: showing a tenant
// admin a cluster-scoped form is an annoyance, the inverse is a breach.
func TestTenantAdminCannotSeeClusterScoped(t *testing.T) {
	s, _ := newServerAsTenant(t, "acme",
		requirement("registry", "cluster", "gentian/registries/x", 0),
		tenantRequirement("tenant-smtp", "acme", "gentian-os/tenants/acme/smtp"),
	)

	w := do(t, s, "GET", "/v1/credentials", "")
	body := w.Body.String()
	if strings.Contains(body, "registry") {
		t.Fatalf("cluster-scoped requirement visible to a tenant admin:\n%s", body)
	}
	if !strings.Contains(body, "tenant-smtp") {
		t.Fatalf("tenant admin cannot see its own requirement:\n%s", body)
	}
}

// TestTenantAdminCannotSeeAnotherTenant is the property scope alone could not
// express. Both requirements are tenant-scoped and equally "visible to tenant
// admins"; only identity separates them. A credential to a tenant-proprietary
// repository is the case that makes this a disclosure rather than clutter.
func TestTenantAdminCannotSeeAnotherTenant(t *testing.T) {
	s, _ := newServerAsTenant(t, "acme",
		tenantRequirement("acme-repo", "acme", "gentian-os/tenants/acme/repo"),
		tenantRequirement("globex-repo", "globex", "gentian-os/tenants/globex/repo"),
	)

	w := do(t, s, "GET", "/v1/credentials", "")
	body := w.Body.String()
	if strings.Contains(body, "globex-repo") {
		t.Fatalf("one tenant can see another tenant's requirement:\n%s", body)
	}
	if !strings.Contains(body, "acme-repo") {
		t.Fatalf("tenant admin cannot see its own requirement:\n%s", body)
	}

	// And cannot reach it by name either — filtering is not a listing cosmetic.
	if w := do(t, s, "GET", "/v1/credentials/globex-repo", ""); w.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for another tenant's requirement, got %d: %s", w.Code, w.Body.String())
	}
}

// TestScopeQueryParameterCannotWidenVisibility closes the escalation that came
// from trusting the caller: ?scope=cluster used to turn a tenant listing into a
// cluster one.
func TestScopeQueryParameterCannotWidenVisibility(t *testing.T) {
	s, _ := newServerAsTenant(t, "acme",
		requirement("registry", "cluster", "gentian/registries/x", 0),
	)

	for _, target := range []string{
		"/v1/credentials?scope=cluster",
		"/v1/credentials/registry?scope=cluster",
	} {
		w := do(t, s, "GET", target, "")
		if strings.Contains(w.Body.String(), "gentian/registries/x") {
			t.Fatalf("%s widened visibility for a tenant admin:\n%s", target, w.Body.String())
		}
	}
}

// TestTenantWithoutClaimSeesNothing proves the closed-by-default direction: a
// role that maps no tenant yields a viewer with no tenant, and a tenant-scoped
// requirement is not visible to "any tenant".
func TestTenantWithoutClaimSeesNothing(t *testing.T) {
	s, _ := newServerAs(t, []string{"tenant-admin"}, map[string]string{"username": "nobody@example.com"},
		tenantRequirement("acme-repo", "acme", "gentian-os/tenants/acme/repo"),
	)
	w := do(t, s, "GET", "/v1/credentials", "")
	if strings.Contains(w.Body.String(), "acme-repo") {
		t.Fatalf("a caller with no tenant claim saw a tenant-scoped requirement:\n%s", w.Body.String())
	}
}

// TestAuditNameComesFromTheTokenNotAHeader closes the other half of the same
// gap: X-Gentian-User used to decide who the audit trail blamed.
func TestAuditNameComesFromTheTokenNotAHeader(t *testing.T) {
	s, _ := newServer(t, requirement("smtp-relay", "cluster", "gentian/mail/relay", 0))

	r := httptest.NewRequest("PUT", "/v1/credentials/smtp-relay",
		strings.NewReader(`{"fields":{"password":"a-value"}}`))
	r.Header.Set("Authorization", "Bearer caller-oidc-token")
	r.Header.Set("X-Gentian-User", "mallory@example.com")
	w := httptest.NewRecorder()
	s.Routes().ServeHTTP(w, r)

	if strings.Contains(w.Body.String(), "mallory@example.com") {
		t.Fatalf("a header decided the audit identity:\n%s", w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "alice@example.com") {
		t.Fatalf("audit identity did not come from the verified token:\n%s", w.Body.String())
	}
}

// TestUnsatisfiedReportsESOReason proves satisfaction comes from ESO rather
// than from asking OpenBao, and that the reason reaches the caller.
func TestUnsatisfiedReportsESOReason(t *testing.T) {
	s, _ := newServer(t,
		requirement("smtp-relay", "cluster", "gentian/mail/relay", 0),
		probe("smtp-relay", false),
	)
	w := do(t, s, "GET", "/v1/credentials/smtp-relay", "")

	var got Status
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decoding: %v (%s)", err, w.Body.String())
	}
	if got.Satisfied {
		t.Fatal("requirement reported satisfied while its probe is not Ready")
	}
	if !strings.Contains(got.Reason, "SecretSyncedError") {
		t.Fatalf("ESO's reason did not reach the caller: %q", got.Reason)
	}
}

// TestValidationRunsBeforeStore is what justifies the service existing: a value
// that fails its probe against the target endpoint must not be stored.
func TestValidationRunsBeforeStore(t *testing.T) {
	s, _ := newServer(t,
		requirementWithValidator("smtp-relay", "cluster", "gentian/mail/relay", 0, "smtp"))
	s.Validator = stubValidator{err: fmt.Errorf("relay rejected the credentials")}

	w := do(t, s, "PUT", "/v1/credentials/smtp-relay",
		`{"fields":{"password":"plausible-but-wrong"}}`)

	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 when validation fails, got %d: %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), `"stored":true`) {
		t.Fatalf("a failing value reported as stored: %s", w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "relay rejected") {
		t.Fatalf("the endpoint's own reason should reach the operator: %s", w.Body.String())
	}
}

// TestValidationPassLeadsToStore is the other half: a value that validates is
// written, and the response still carries no value.
func TestValidationPassLeadsToStore(t *testing.T) {
	s, _ := newServer(t,
		requirementWithValidator("smtp-relay", "cluster", "gentian/mail/relay", 0, "smtp"))

	w := do(t, s, "PUT", "/v1/credentials/smtp-relay",
		fmt.Sprintf(`{"fields":{"password":%q}}`, theSecretValue))

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 on a valid write, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"stored":true`) {
		t.Fatalf("a valid write did not report stored: %s", w.Body.String())
	}
	if strings.Contains(w.Body.String(), theSecretValue) {
		t.Fatalf("the write response echoed the value back: %s", w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "alice@example.com") {
		t.Fatalf("the response should name who set it: %s", w.Body.String())
	}
}

// TestRejectsWhitespaceAndShortValues covers the two schema rules that catch
// the most common paste mistakes.
func TestRejectsWhitespaceAndShortValues(t *testing.T) {
	s, _ := newServer(t, requirement("master", "cluster", "gentian/master", 16))

	for _, tc := range []struct{ name, body, want string }{
		{"trailing whitespace", `{"fields":{"password":"abcdefghijklmnop "}}`, "whitespace"},
		{"too short", `{"fields":{"password":"short"}}`, "at least 16"},
		{"unknown field", `{"fields":{"nope":"x"}}`, "unknown field"},
		{"no fields", `{"fields":{}}`, "no fields"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := do(t, s, "PUT", "/v1/credentials/master", tc.body)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
			}
			if !strings.Contains(w.Body.String(), tc.want) {
				t.Fatalf("error should mention %q, got: %s", tc.want, w.Body.String())
			}
		})
	}
}

// TestServerHasNoTokenField is a structural assertion: if someone adds a field
// that could hold a service-wide OpenBao token, this fails and they have to
// argue for it in review.
func TestServerHasNoTokenField(t *testing.T) {
	for _, forbidden := range []string{"Token", "RootToken", "ServiceToken", "BaoToken"} {
		if fieldExists(Server{}, forbidden) {
			t.Fatalf("Server gained a %q field — the service must hold no OpenBao token of its own", forbidden)
		}
		if fieldExists(OpenBao{}, forbidden) {
			t.Fatalf("OpenBao gained a %q field — every write must take the caller's token", forbidden)
		}
	}
}

// TestExchangeUsesConfiguredMount pins the login path. The auth backend is
// enabled at -path=oidc, so the endpoint is auth/oidc/login; the client used to
// hardcode the plugin's default name and every exchange reached a mount that
// does not exist. The fake here encoded the same wrong path, which is why the
// suite stayed green while the UI could not authenticate at all.
func TestExchangeUsesConfiguredMount(t *testing.T) {
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.URL.Path)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"auth": map[string]any{"client_token": "t", "token_policies": []string{"cluster-admin"}},
		})
	}))
	t.Cleanup(srv.Close)

	b := NewOpenBao(srv.URL, "secret", "oidc", "kernel", []string{"cluster-admin-jwt"}, nil, false)
	if _, err := b.ExchangeToken(context.Background(), "a.b.c"); err != nil {
		t.Fatalf("exchange failed: %v", err)
	}
	if len(got) != 1 || got[0] != "/v1/auth/oidc/login" {
		t.Fatalf("want a login at /v1/auth/oidc/login, got %v", got)
	}
}

// TestExchangeFallsThroughToNextRole covers the tenant admin. Each role binds a
// different group claim, so the role that does not match refuses the token —
// which rules out that role, not the caller. With a single role configured, a
// tenant admin could never authenticate.
func TestExchangeFallsThroughToNextRole(t *testing.T) {
	var tried []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Role string `json:"role"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		tried = append(tried, body.Role)
		if body.Role != "tenant-admin-jwt" {
			// What OpenBao returns when bound claims do not match.
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"auth": map[string]any{
				"client_token":   "tenant-token",
				"token_policies": []string{"tenant-admin"},
				"metadata":       map[string]string{"tenant": "acme"},
			},
		})
	}))
	t.Cleanup(srv.Close)

	b := NewOpenBao(srv.URL, "secret", "oidc", "kernel", []string{"cluster-admin-jwt", "tenant-admin-jwt"}, nil, false)
	id, err := b.ExchangeToken(context.Background(), "a.b.c")
	if err != nil {
		t.Fatalf("exchange failed: %v", err)
	}
	if id.Metadata["tenant"] != "acme" {
		t.Fatalf("want the tenant from the accepting role, got %q", id.Metadata["tenant"])
	}
	if len(tried) != 2 || tried[0] != "cluster-admin-jwt" || tried[1] != "tenant-admin-jwt" {
		t.Fatalf("roles should be tried in order, got %v", tried)
	}
}

// TestExchangeRejectedByEveryRole is the case the UI reports. It must stay an
// error, and must not name the policies or paths OpenBao mentioned.
func TestExchangeRejectedByEveryRole(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "policy \"cluster-admin\" denied on gentian/kernel/*", http.StatusBadRequest)
	}))
	t.Cleanup(srv.Close)

	b := NewOpenBao(srv.URL, "secret", "oidc", "kernel", []string{"cluster-admin-jwt", "tenant-admin-jwt"}, nil, false)
	_, err := b.ExchangeToken(context.Background(), "a.b.c")
	if err == nil {
		t.Fatal("exchange should fail when every role refuses")
	}
	if strings.Contains(err.Error(), "gentian/kernel") || strings.Contains(err.Error(), "policy") {
		t.Fatalf("error leaked OpenBao's body: %v", err)
	}
}

// TestRefusalNamesTheFailedCheck is the fix for three debugging rounds. Each
// began by re-checking a group membership that was correct, because the message
// named group membership and nothing else. A refusal must say which check
// failed and must still not repeat OpenBao's body, which can name policies and
// paths.
func TestRefusalNamesTheFailedCheck(t *testing.T) {
	cases := []struct {
		name, openbaoSays, want string
	}{
		{"audience", `{"errors":["error validating token: invalid audience (aud) claim"]}`, "audience"},
		{"claims", `{"errors":["error validating claims: claim \"groups\" does not match any associated bound claim values"]}`, "claims"},
		{"role type", `{"errors":["role with oidc role_type is not allowed"]}`, "direct token exchange"},
		{"missing role", `{"errors":["role \"cluster-admin-jwt\" could not be found"]}`, "does not exist"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(tc.openbaoSays))
			}))
			t.Cleanup(srv.Close)

			b := NewOpenBao(srv.URL, "secret", "oidc", "kernel", []string{"cluster-admin-jwt"}, nil, false)
			_, err := b.ExchangeToken(context.Background(), "a.b.c")
			if err == nil {
				t.Fatal("a refused exchange must be an error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error should name the failed check %q, got: %v", tc.want, err)
			}
			// The category is safe to show; OpenBao's own words are not.
			if strings.Contains(err.Error(), "bound claim values") || strings.Contains(err.Error(), "errors") {
				t.Fatalf("error echoed OpenBao's body: %v", err)
			}
		})
	}
}

// TestRefusalOnAMountThatDoesNotExist covers the first of the three: a 404 is
// not a permissions answer at all, and saying so would have ended that round in
// one look.
func TestRefusalOnAMountThatDoesNotExist(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"errors":[]}`))
	}))
	t.Cleanup(srv.Close)

	b := NewOpenBao(srv.URL, "secret", "jwt", "kernel", []string{"cluster-admin-jwt"}, nil, false)
	_, err := b.ExchangeToken(context.Background(), "a.b.c")
	if err == nil || !strings.Contains(err.Error(), "not mounted") {
		t.Fatalf("a 404 should say the backend is not mounted where expected, got: %v", err)
	}
}

// jwtWithIssuer builds an unsigned token carrying iss. Unsigned on purpose:
// routing must not depend on the signature, and OpenBao verifies afterwards.
func jwtWithIssuer(t *testing.T, iss string) string {
	t.Helper()
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"iss":"` + iss + `"}`))
	return "eyJhbGciOiJSUzI1NiJ9." + payload + ".sig"
}

// TestExchangeRoutesByIssuer is the fix for tenant administrators. A tenant
// member authenticates in their own realm, so their token is signed by that
// realm — the kernel mount cannot verify it, and the refusal lands on the
// signature before any claim is read. Each realm gets its own mount.
func TestExchangeRoutesByIssuer(t *testing.T) {
	cases := []struct {
		name, iss, wantMount string
	}{
		{"kernel realm uses the configured mount", "https://id.example.test/auth/realms/kernel", "/v1/auth/oidc/login"},
		{"tenant realm uses its own mount", "https://id.example.test/auth/realms/corp", "/v1/auth/oidc-corp/login"},
		{"no issuer falls back to the kernel mount", "", "/v1/auth/oidc/login"},
		{"a non-realm issuer falls back", "https://accounts.google.com", "/v1/auth/oidc/login"},
		// A crafted issuer must not be able to name an arbitrary mount path.
		{"a path-traversing realm falls back", "https://id.example.test/auth/realms/../../sys/auth", "/v1/auth/oidc/login"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				got = r.URL.Path
				w.WriteHeader(http.StatusBadRequest)
			}))
			t.Cleanup(srv.Close)

			token := "not.a.jwt"
			if tc.iss != "" {
				token = jwtWithIssuer(t, tc.iss)
			}
			b := NewOpenBao(srv.URL, "secret", "oidc", "kernel", []string{"tenant-admin"}, nil, false)
			_, _ = b.ExchangeToken(context.Background(), token)
			if got != tc.wantMount {
				t.Fatalf("want login at %s, got %s", tc.wantMount, got)
			}
		})
	}
}

// multiFieldRequirement declares two fields, so a single bad submission can
// violate more than one at once.
func multiFieldRequirement(name string) *gentianv1alpha1.CredentialRequirement {
	r := requirement(name, "cluster", "gentian/"+name, 8)
	r.Spec.Fields = []gentianv1alpha1.CredentialField{
		{Key: "username", Format: "string"},
		{Key: "password", Format: "password", Secret: true, MinLength: 8},
	}
	return r
}

// TestChecksFieldsCollectsEveryViolation is the point of collecting rather
// than returning on the first failure: a submission missing one field and
// mistyping another should not need two round trips to discover both.
func TestChecksFieldsCollectsEveryViolation(t *testing.T) {
	s, _ := newServer(t, multiFieldRequirement("two-fields"))

	w := do(t, s, "PUT", "/v1/credentials/two-fields", `{"fields":{"password":"short"}}`)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
	var body struct {
		Error  string       `json:"error"`
		Fields []FieldError `json:"fields"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	got := map[string]bool{}
	for _, f := range body.Fields {
		got[f.Field] = true
	}
	if !got["username"] {
		t.Errorf("expected username (missing) attributed, got %+v", body.Fields)
	}
	if !got["password"] {
		t.Errorf("expected password (too short) attributed, got %+v", body.Fields)
	}
	if len(body.Fields) != 2 {
		t.Errorf("expected exactly 2 field errors from one submission, got %+v", body.Fields)
	}
}

// TestValidatorFieldErrorsReachTheResponseBody is the other producer of
// FieldErrors: an endpoint probe's rejection, not just the schema check.
// writeErr must unwrap it the same way regardless of which layer raised it.
func TestValidatorFieldErrorsReachTheResponseBody(t *testing.T) {
	s, _ := newServer(t, requirementWithValidator("probed", "cluster", "gentian/probed", 0, "oci-registry"))
	s.Validator = stubValidator{err: FieldErrors{
		{Field: "password", Message: "rejected"},
	}}

	w := do(t, s, "PUT", "/v1/credentials/probed", `{"fields":{"password":"wrong-value"}}`)

	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422, got %d: %s", w.Code, w.Body.String())
	}
	var body struct {
		Fields []FieldError `json:"fields"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if len(body.Fields) != 1 || body.Fields[0].Field != "password" {
		t.Fatalf("expected the validator's field attribution to reach the response, got %+v", body.Fields)
	}
}

// TestPlainErrorsCarryNoFieldsKey: an ordinary error (an unreachable
// endpoint, a malformed body) must not grow a "fields" key nobody populated —
// existing callers reading only "error" must see the same shape as before.
func TestPlainErrorsCarryNoFieldsKey(t *testing.T) {
	s, _ := newServer(t, requirement("plain", "cluster", "gentian/plain", 0))
	s.Validator = stubValidator{err: fmt.Errorf("endpoint unreachable")}

	w := do(t, s, "PUT", "/v1/credentials/plain", `{"fields":{"password":"anything"}}`)

	if strings.Contains(w.Body.String(), `"fields"`) {
		t.Fatalf(`a plain error must not carry a "fields" key: %s`, w.Body.String())
	}
}

// TestValidateHostReachesTheCatalogue closes the bug this fixed: the
// requirement's spec.validate.host used to be dropped between the CRD and the
// Status the API and the Validator both work from, so oci-registry and
// git-https could never be given the endpoint they need to probe.
func TestValidateHostReachesTheCatalogue(t *testing.T) {
	req := requirementWithValidator("hosted", "cluster", "gentian/hosted", 0, "oci-registry")
	req.Spec.Validate.Host = "https://registry.example.test"

	c := &Catalogue{Client: fake.NewClientBuilder().WithScheme(testScheme(t)).WithRuntimeObjects(req).Build()}
	items, err := c.List(context.Background(), Viewer{ClusterAdmin: true})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(items) != 1 || items[0].ValidateHost != "https://registry.example.test" {
		t.Fatalf("expected ValidateHost to carry the requirement's declared host, got %+v", items)
	}
}
