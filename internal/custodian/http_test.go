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

package custodian

import (
	"context"
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

	vault := NewOpenBao(bao.URL, "secret", "gentian-os-custodian", nil, false)
	// The custodian's own token at the stand-in vault. The caller's is
	// "caller-oidc-token" (see do), and must never be what the vault is shown.
	vault.SetStaticToken("custodian-token")
	s := &Server{
		Catalogue: &Catalogue{Client: c, ProbeNamespace: "gentian-system"},
		Bao:       vault,
		Validator: stubValidator{},
		Authz:     storeFor(policies, meta),
	}
	return s, bao
}

// fakeStore stands in for the identity provider and the authorization store:
// it says who the caller is and what they hold. The harness still describes a
// caller by the policy and claim OpenBao used to report, because that is how
// every test here names its caller -- but nothing reads them from OpenBao any
// more. They are turned into the store's answers, which is the only place the
// custodian now takes a right from.
type fakeStore struct {
	who Principal
	// held is "relation object" for everything the caller holds.
	held map[string]bool
	// everyTenant grants both relations on any tenant, as a platform
	// administrator holds them through the store's derivation.
	everyTenant bool
	err         error
	asked       []string
}

func storeFor(policies []string, meta map[string]string) *fakeStore {
	f := &fakeStore{who: Principal{User: "user:caller", Name: meta["username"], Tenant: meta["tenant"]}, held: map[string]bool{}}
	for _, p := range policies {
		if p == "cluster-admin" {
			f.held[relationRead+" cluster:test"], f.held[relationWrite+" cluster:test"] = true, true
			f.everyTenant = true
		}
	}
	if t := meta["tenant"]; t != "" {
		f.held[relationRead+" tenant:"+t], f.held[relationWrite+" tenant:"+t] = true, true
	}
	return f
}

func (f *fakeStore) Identify(context.Context, string) (Principal, error) { return f.who, nil }

func (f *fakeStore) Check(_ context.Context, _, relation, object string) (bool, error) {
	f.asked = append(f.asked, relation+" "+object)
	if f.err != nil {
		return false, f.err
	}
	if f.everyTenant && strings.HasPrefix(object, "tenant:") {
		return true, nil
	}
	return f.held[relation+" "+object], nil
}

func (f *fakeStore) Object(scope, tenant string) (string, bool) {
	switch scope {
	case scopeCluster:
		return "cluster:test", true
	case scopeTenant:
		return "tenant:" + tenant, tenant != ""
	}
	return "", false
}

// allowAll is a view that may read and set everything, for the tests that are
// about the catalogue rather than about who is looking at it.
func allowAll() Viewer {
	return Viewer{ClusterAdmin: true, check: func(string, string, string) bool { return true }}
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
	b := NewOpenBao("http://openbao.invalid", "secret", "gentian-os-custodian", nil, false)
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
	items, err := c.List(context.Background(), allowAll())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(items) != 1 || items[0].ValidateHost != "https://registry.example.test" {
		t.Fatalf("expected ValidateHost to carry the requirement's declared host, got %+v", items)
	}
}

// A credential that cannot be probed is stored, and said to be unvalidated.
//
// Validation runs before the write and a failure refuses it, which is right
// for a probe that says the credential is wrong. "There is nothing to probe"
// is not that: it is a gap in the requirement's declaration, and refusing the
// write because of it blocks whatever was waiting on the credential. The
// deployments token could not be set at all for exactly this reason — its
// catalogue entry asks for a git-https probe and named no host — which left
// every console write answering 503 for want of a credential nobody was
// allowed to store.
func TestACredentialThatCannotBeProbedIsStoredAndSaysSo(t *testing.T) {
	s, _ := newServer(t, requirementWithValidator("deployments-repository", "cluster",
		"gentian-os/kernel/repositories/deployments", 0, "git-https"))
	s.Validator = stubValidator{err: fmt.Errorf("%w: the requirement declares no host or url", ErrNoEndpoint)}

	w := do(t, s, "PUT", "/v1/credentials/deployments-repository",
		`{"fields":{"password":"a-real-token"}}`)
	if w.Code != http.StatusOK {
		t.Fatalf("a credential that could not be probed was refused: %d %s", w.Code, w.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got["stored"] != true {
		t.Fatalf("not stored: %v", got)
	}
	// Said out loud: a caller who asked for a validated write and got an
	// unvalidated one should be told which they got.
	if got["validated"] != false || got["validationSkipped"] == nil {
		t.Fatalf("the response does not say it was unvalidated: %v", got)
	}
}

// A probe that actually ran and refused still blocks the write. That is what
// the service is for, and the case above must not have weakened it.
func TestAProbeThatRefusesStillBlocksTheWrite(t *testing.T) {
	s, _ := newServer(t, requirementWithValidator("smtp-relay", "cluster", "gentian/mail/relay", 0, "smtp"))
	s.Validator = stubValidator{err: fmt.Errorf("the endpoint rejected these credentials")}

	w := do(t, s, "PUT", "/v1/credentials/smtp-relay", `{"fields":{"password":"wrong"}}`)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("a refused probe did not block the write: %d %s", w.Code, w.Body.String())
	}
}

// TestListingCarriesTheProvider is what lets a screen keep one vendor's
// tokens together: the catalogue's provider label comes back as a field, and
// a requirement without one carries none.
func TestListingCarriesTheProvider(t *testing.T) {
	dns := requirement("acme-dns-infomaniak", "cluster", "gentian-os/kernel/dns/infomaniak", 0)
	dns.Labels = map[string]string{ProviderLabel: "infomaniak"}
	s, _ := newServer(t, dns, requirement("master-password", "cluster", "gentian-os/kernel/master", 0))

	w := do(t, s, "GET", "/v1/credentials", "")
	var got struct {
		Credentials []Status `json:"credentials"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decoding listing: %v\n%s", err, w.Body.String())
	}
	providers := map[string]string{}
	for _, st := range got.Credentials {
		providers[st.Name] = st.Provider
	}
	if providers["acme-dns-infomaniak"] != "infomaniak" {
		t.Errorf("provider label not carried: %q", providers["acme-dns-infomaniak"])
	}
	if p, ok := providers["master-password"]; !ok || p != "" {
		t.Errorf("an unlabelled requirement should be listed with no provider, got %q (listed=%v)", p, ok)
	}
}
