/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package applifecycle

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/gentian-org/gentian-os/internal/licencereport"
)

const (
	testAudience   = "gentian-os-operator"
	testNamespace  = "kernel-control"
	directorUser   = "system:serviceaccount:kernel-control:gentian-os-director"
	usherUser      = "system:serviceaccount:kernel-control:gentian-os-usher"
	custodianUser  = "system:serviceaccount:kernel-control:gentian-os-custodian"
	otherNamespace = "system:serviceaccount:tenant-demo:gentian-os-director"
)

// issued is one token the stand-in API server knows: whose it is and which
// audiences it was issued for.
type issued struct {
	username  string
	audiences []string
}

// apiServer stands in for the TokenReview API. It answers as the API server
// does: a token it does not know, or one issued for none of the audiences
// asked about, is not authenticated; otherwise the answer names the user and
// the audiences the token and the question have in common.
type apiServer struct {
	tokens map[string]issued
	// err, when set, is the API server not answering.
	err error
	// ignoresAudiences answers as an authenticator that does not know about
	// audiences does: genuine, and no audience named.
	ignoresAudiences bool
	asked            int
}

func (a *apiServer) Create(_ context.Context, review *authenticationv1.TokenReview, _ metav1.CreateOptions) (*authenticationv1.TokenReview, error) {
	a.asked++
	if a.err != nil {
		return nil, a.err
	}
	out := review.DeepCopy()
	token, ok := a.tokens[review.Spec.Token]
	if !ok {
		out.Status = authenticationv1.TokenReviewStatus{Error: "token is not known"}
		return out, nil
	}
	if a.ignoresAudiences {
		out.Status = authenticationv1.TokenReviewStatus{Authenticated: true, User: authenticationv1.UserInfo{Username: token.username}}
		return out, nil
	}
	var common []string
	for _, audience := range review.Spec.Audiences {
		if slices.Contains(token.audiences, audience) {
			common = append(common, audience)
		}
	}
	if len(common) == 0 {
		out.Status = authenticationv1.TokenReviewStatus{Error: "token audiences do not include any asked for"}
		return out, nil
	}
	out.Status = authenticationv1.TokenReviewStatus{
		Authenticated: true,
		User:          authenticationv1.UserInfo{Username: token.username},
		Audiences:     common,
	}
	return out, nil
}

// cluster is an API server that has issued one token to each workload a test
// asks about, and the listener's check configured as the chart configures it.
// The token strings are the workloads' names, so a test reads as who called.
func cluster() (*apiServer, *CallerAuth) {
	api := &apiServer{tokens: map[string]issued{
		"director":  {directorUser, []string{testAudience}},
		"usher":     {usherUser, []string{testAudience}},
		"custodian": {custodianUser, []string{testAudience}},
		// The director's name in another namespace is not the director.
		"impostor": {otherNamespace, []string{testAudience}},
		// The director's own identity, on a token issued for the API server
		// and not for this listener.
		"director-for-the-api-server": {directorUser, []string{"https://kubernetes.default.svc"}},
	}}
	return api, NewCallerAuth(api, Callers{
		Audience: testAudience,
		Director: ServiceAccountUsername(testNamespace, "gentian-os-director"),
		Reader:   ServiceAccountUsername(testNamespace, "gentian-os-usher"),
	})
}

// What a tenant is meant to have is declared in git by the director. The
// routes that once changed it here -- install, uninstall, the addon selection
// and the resource plan -- must stay gone: each was a second way to change
// declared state, with no author and no review.
func TestTheListenerHasNoWriteOfDeclaredState(t *testing.T) {
	_, auth := cluster()
	h := &HTTPServer{Auth: auth}
	mux := h.routes()
	for _, c := range []struct{ method, path, body string }{
		{"POST", "/v1/tenants/demo/apps/nextcloud", ""},
		{"DELETE", "/v1/tenants/demo/apps/nextcloud", ""},
		{"PUT", "/v1/tenants/demo/apps/nextcloud/addons", `{"addons":[]}`},
		{"PUT", "/v1/tenants/demo/resources", `{"plan":"small"}`},
		{"PUT", "/v1/tenants/demo/backup-policy", `{}`},
	} {
		r := httptest.NewRequest(c.method, c.path, strings.NewReader(c.body))
		r.Header.Set("Authorization", "Bearer director")
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		if w.Code != http.StatusNotFound && w.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s %s answered %d; the route must not exist", c.method, c.path, w.Code)
		}
	}
}

// guardOnly builds the caller check around a handler that only says it was
// reached, so the check is tested without a cluster behind it.
func guardOnly(h *HTTPServer) *http.ServeMux {
	mux := http.NewServeMux()
	g := &guardedMux{mux: mux, auth: h.authenticated}
	reached := func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }
	g.Read("GET /v1/tenants/{tenant}/backups", reached)
	g.HandleFunc("GET /v1/tenants/{tenant}/backups/{name}/download", reached)
	g.HandleFunc("POST /v1/tenants/{tenant}/actions/delete-backup", reached)
	return mux
}

func answer(mux http.Handler, method, path, token string) int {
	r := httptest.NewRequest(method, path, nil)
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	return w.Code
}

// The director's identity is admitted to every route; the usher's to the
// routes registered as reads and to nothing else. It is a second identity so
// that the process facing every console's backend cannot delete a backup or
// purge an app, whatever happens to it.
func TestTheCallerIsAdmittedByItsIdentity(t *testing.T) {
	_, auth := cluster()
	mux := guardOnly(&HTTPServer{Auth: auth})
	for _, c := range []struct {
		method, path, token string
		want                int
	}{
		{"GET", "/v1/tenants/demo/backups", "director", http.StatusNoContent},
		{"GET", "/v1/tenants/demo/backups", "usher", http.StatusNoContent},
		// A GET that was not registered as a read is the director's.
		{"GET", "/v1/tenants/demo/backups/b1/download", "director", http.StatusNoContent},
		{"GET", "/v1/tenants/demo/backups/b1/download", "usher", http.StatusForbidden},
		// No command, ever.
		{"POST", "/v1/tenants/demo/actions/delete-backup", "director", http.StatusNoContent},
		{"POST", "/v1/tenants/demo/actions/delete-backup", "usher", http.StatusForbidden},
	} {
		if got := answer(mux, c.method, c.path, c.token); got != c.want {
			t.Errorf("%s %s as %q answered %d, want %d", c.method, c.path, c.token, got, c.want)
		}
	}
}

// Everybody else is refused, on a read as on a command: no token, a string
// that is not a token, a token for another audience, and a genuine token for
// this audience that belongs to somebody else.
func TestNobodyElseIsAdmitted(t *testing.T) {
	_, auth := cluster()
	mux := guardOnly(&HTTPServer{Auth: auth})
	for _, c := range []struct {
		token string
		want  int
	}{
		{"", http.StatusUnauthorized},
		{"a-string-somebody-made-up", http.StatusUnauthorized},
		{"director-for-the-api-server", http.StatusUnauthorized},
		{"custodian", http.StatusForbidden},
		{"impostor", http.StatusForbidden},
	} {
		for _, route := range []struct{ method, path string }{
			{"GET", "/v1/tenants/demo/backups"},
			{"GET", "/v1/tenants/demo/backups/b1/download"},
			{"POST", "/v1/tenants/demo/actions/delete-backup"},
		} {
			if got := answer(mux, route.method, route.path, c.token); got != c.want {
				t.Errorf("%s %s as %q answered %d, want %d", route.method, route.path, c.token, got, c.want)
			}
		}
	}
}

// A request that presents nothing is refused without the API server being
// asked, and so is one whose header is not a bearer.
func TestARequestWithNoBearerAsksNobody(t *testing.T) {
	api, auth := cluster()
	mux := guardOnly(&HTTPServer{Auth: auth})
	if got := answer(mux, "GET", "/v1/tenants/demo/backups", ""); got != http.StatusUnauthorized {
		t.Fatalf("no token: %d, want 401", got)
	}
	r := httptest.NewRequest("GET", "/v1/tenants/demo/backups", nil)
	r.Header.Set("Authorization", "Basic director")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("a header that is not a bearer: %d, want 401", w.Code)
	}
	if api.asked != 0 {
		t.Fatalf("the API server was asked %d times about no token", api.asked)
	}
}

// An authenticator that reports a token as genuine without naming an audience
// has not said the token was issued for this listener, and the answer is not
// taken as if it had.
func TestAnAnswerThatNamesNoAudienceIsARefusal(t *testing.T) {
	api, auth := cluster()
	api.ignoresAudiences = true
	mux := guardOnly(&HTTPServer{Auth: auth})
	for _, token := range []string{"director", "usher", "director-for-the-api-server"} {
		if got := answer(mux, "GET", "/v1/tenants/demo/backups", token); got != http.StatusUnauthorized {
			t.Errorf("%q with no audience in the answer: %d, want 401", token, got)
		}
	}
}

// When the API server cannot be asked the listener refuses. It does not fall
// back to admitting, and it does not remember the refusal.
func TestTheListenerIsClosedWhenTheAPIServerDoesNotAnswer(t *testing.T) {
	api, auth := cluster()
	api.err = errors.New("connection refused")
	mux := guardOnly(&HTTPServer{Auth: auth})
	for _, token := range []string{"director", "usher"} {
		if got := answer(mux, "GET", "/v1/tenants/demo/backups", token); got != http.StatusServiceUnavailable {
			t.Errorf("%q while the API server is away: %d, want 503", token, got)
		}
		if got := answer(mux, "POST", "/v1/tenants/demo/actions/delete-backup", token); got != http.StatusServiceUnavailable {
			t.Errorf("%q commanding while the API server is away: %d, want 503", token, got)
		}
	}
	api.err = nil
	if got := answer(mux, "GET", "/v1/tenants/demo/backups", "director"); got != http.StatusNoContent {
		t.Fatalf("the director once the API server is back: %d, want 204", got)
	}
}

// A positive answer is believed for a short time and then asked for again; a
// refusal is asked for every time. A token the API server stops vouching for
// is therefore refused once the short time is over, and an outage after it
// closes the listener for a caller it had admitted.
func TestAPositiveAnswerIsKeptBrieflyAndARefusalNotAtAll(t *testing.T) {
	api, auth := cluster()
	now := time.Unix(1_700_000_000, 0)
	auth.now = func() time.Time { return now }
	mux := guardOnly(&HTTPServer{Auth: auth})

	for range 3 {
		if got := answer(mux, "GET", "/v1/tenants/demo/backups", "usher"); got != http.StatusNoContent {
			t.Fatalf("usher's read: %d", got)
		}
	}
	if api.asked != 1 {
		t.Fatalf("the API server was asked %d times for one token within the cache lifetime, want 1", api.asked)
	}
	// What is kept is the identity, not a verdict on a route: the usher's
	// kept answer does not open a command.
	if got := answer(mux, "POST", "/v1/tenants/demo/actions/delete-backup", "usher"); got != http.StatusForbidden {
		t.Fatalf("usher's command from a kept answer: %d, want 403", got)
	}
	// The pod is gone and its token with it.
	delete(api.tokens, "usher")
	now = now.Add(reviewCacheTTL)
	if got := answer(mux, "GET", "/v1/tenants/demo/backups", "usher"); got != http.StatusUnauthorized {
		t.Fatalf("a token the API server no longer vouches for, after the cache lifetime: %d, want 401", got)
	}
	asked := api.asked
	for range 2 {
		_ = answer(mux, "GET", "/v1/tenants/demo/backups", "a-string-somebody-made-up")
	}
	if api.asked != asked+2 {
		t.Fatalf("a refusal was kept: asked %d more times, want 2", api.asked-asked)
	}

	_ = answer(mux, "GET", "/v1/tenants/demo/backups", "director")
	api.err = errors.New("connection refused")
	now = now.Add(reviewCacheTTL)
	if got := answer(mux, "GET", "/v1/tenants/demo/backups", "director"); got != http.StatusServiceUnavailable {
		t.Fatalf("the director after its kept answer ran out, with the API server away: %d, want 503", got)
	}
	if reviewCacheTTL > time.Minute {
		t.Fatalf("a positive answer is kept for %s; a minute is the most", reviewCacheTTL)
	}
}

// The cache is keyed by a hash. The token is not in it.
func TestTheTokenIsNotKept(t *testing.T) {
	_, auth := cluster()
	if _, err := auth.identify(context.Background(), "director"); err != nil {
		t.Fatal(err)
	}
	if len(auth.cache) != 1 {
		t.Fatalf("cache holds %d entries, want 1", len(auth.cache))
	}
	for key, kept := range auth.cache {
		if strings.Contains(string(key[:]), "director") || kept.username != directorUser {
			t.Fatalf("the cache holds the token, or the wrong name: %q", kept.username)
		}
	}
}

// A listener with nobody to admit says so and refuses every request, the
// caller who presents nothing included. Losing part of the configuration
// admits fewer callers and never more.
func TestAListenerWithNobodyToAdmitRefusesEverybody(t *testing.T) {
	api, _ := cluster()
	for name, h := range map[string]*HTTPServer{
		"no check at all":   {},
		"no audience":       {Auth: NewCallerAuth(api, Callers{Director: directorUser, Reader: usherUser})},
		"no identity":       {Auth: NewCallerAuth(api, Callers{Audience: testAudience})},
		"nobody to ask":     {Auth: NewCallerAuth(nil, Callers{Audience: testAudience, Director: directorUser})},
		"names with no SAs": {Auth: NewCallerAuth(api, Callers{Audience: testAudience, Director: ServiceAccountUsername(testNamespace, ""), Reader: ServiceAccountUsername("", "x")})},
	} {
		mux := guardOnly(h)
		for _, token := range []string{"", "director", "usher"} {
			if got := answer(mux, "GET", "/v1/tenants/demo/backups", token); got != http.StatusServiceUnavailable {
				t.Errorf("%s, %q presented: %d, want 503", name, token, got)
			}
		}
	}
	// Only the director: there is no reader, and the usher is somebody else.
	directorOnly := guardOnly(&HTTPServer{Auth: NewCallerAuth(api, Callers{Audience: testAudience, Director: directorUser})})
	if got := answer(directorOnly, "GET", "/v1/tenants/demo/backups", "usher"); got != http.StatusForbidden {
		t.Errorf("no reader configured, the usher asked: %d, want 403", got)
	}
	// Only the usher: reads work, and nothing admits a command.
	usherOnly := guardOnly(&HTTPServer{Auth: NewCallerAuth(api, Callers{Audience: testAudience, Reader: usherUser})})
	if got := answer(usherOnly, "GET", "/v1/tenants/demo/backups", "usher"); got != http.StatusNoContent {
		t.Errorf("usher's read: %d, want 204", got)
	}
	for _, token := range []string{"director", "usher"} {
		if got := answer(usherOnly, "POST", "/v1/tenants/demo/actions/delete-backup", token); got != http.StatusForbidden {
			t.Errorf("no director configured, %q presented a command: %d, want 403", token, got)
		}
	}
}

// Every route the usher's identity admits is a GET, and the set is the one
// the usher serves: adding to it is a decision, so what stays the director's
// is written down here -- each command, and each GET that is not a listed
// read, the bundle download among them.
func TestTheRoutesOpenToTheReaderAreTheUshersReads(t *testing.T) {
	_, auth := cluster()
	h := &HTTPServer{Auth: auth}
	mux := h.routes()
	directorOnly := []struct{ method, path string }{
		{"GET", "/v1/tenants/demo"},
		{"GET", "/v1/tenants/demo/apps"},
		{"GET", "/v1/tenants/demo/backups/b1/download"},
		{"GET", "/v1/tenants/demo/restores/r1"},
		{"POST", "/v1/tenants/demo/actions/backup"},
		{"POST", "/v1/tenants/demo/actions/delete-backup"},
		{"POST", "/v1/tenants/demo/actions/restore"},
		{"POST", "/v1/tenants/demo/actions/notify"},
		{"POST", "/v1/tenants/demo/actions/purge-app"},
		{"POST", "/v1/tenants/demo/actions/provision-app"},
		{"POST", "/v1/bundles"},
		{"POST", "/v1/bundles/inspect"},
	}
	for _, c := range directorOnly {
		if got := answer(mux, c.method, c.path, "usher"); got != http.StatusForbidden {
			t.Errorf("%s %s as the usher answered %d, want 403", c.method, c.path, got)
		}
		// And nobody else gets further than the usher does.
		if got := answer(mux, c.method, c.path, "custodian"); got != http.StatusForbidden {
			t.Errorf("%s %s as another ServiceAccount answered %d, want 403", c.method, c.path, got)
		}
		if got := answer(mux, c.method, c.path, ""); got != http.StatusUnauthorized {
			t.Errorf("%s %s with no token answered %d, want 401", c.method, c.path, got)
		}
	}
}

// Registering anything but a GET as a read is a programming error, caught
// when the routes are built.
func TestOnlyAGetCanBeRegisteredAsARead(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("a POST was registered as a read")
		}
	}()
	_, auth := cluster()
	h := &HTTPServer{Auth: auth}
	g := &guardedMux{mux: http.NewServeMux(), auth: h.authenticated}
	g.Read("POST /v1/tenants/{tenant}/actions/backup", func(http.ResponseWriter, *http.Request) {})
}

// The last licence report is read under the usher's identity, and a cluster
// that does not report says exactly that.
func TestTheReaderIsToldWhetherTheClusterReports(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	_, auth := cluster()
	h := &HTTPServer{Auth: auth, Service: &Service{client: c, opts: Options{OperatorNamespace: "kernel-control"}}}

	ask := func() (int, string) {
		r := httptest.NewRequest("GET", "/v1/licence-report", nil)
		r.Header.Set("Authorization", "Bearer usher")
		w := httptest.NewRecorder()
		h.routes().ServeHTTP(w, r)
		return w.Code, strings.TrimSpace(w.Body.String())
	}
	if code, body := ask(); code != http.StatusOK || body != `{"enabled":false}` {
		t.Fatalf("reporting off: %d %s", code, body)
	}

	h.Service.opts.LicenceReport = licencereport.Settings{Enabled: true, URL: "https://reports.example/v1"}
	if code, body := ask(); code != http.StatusOK || body != `{"enabled":true,"url":"https://reports.example/v1"}` {
		t.Fatalf("reporting on, nothing sent yet: %d %s", code, body)
	}
	if got := answer(h.routes(), "GET", "/v1/licence-report", ""); got != http.StatusUnauthorized {
		t.Fatalf("no token: %d", got)
	}
}
