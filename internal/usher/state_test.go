/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package usher

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/gentian-org/gentian-os/internal/director/lifecycle"
)

// fakeOperator is the operator's listener as the usher may use it. It records
// what it was asked and answers with the path, so a test can tell which
// question reached it.
type fakeOperator struct {
	paths       []string
	queries     []url.Values
	selfService []bool
	status      int
	body        string
	err         error
}

func (f *fakeOperator) Get(_ context.Context, path string, query url.Values) (int, []byte, error) {
	f.paths = append(f.paths, path)
	f.queries = append(f.queries, query)
	if f.err != nil {
		return 0, nil, f.err
	}
	if f.status != 0 {
		return f.status, []byte(f.body), nil
	}
	if path == "/v1/resources" {
		return http.StatusOK, []byte(`{"tenants":[{"tenant":"acme"}],"unavailable":[{"tenant":"other","reason":"no quota"}]}`), nil
	}
	return http.StatusOK, []byte(`{"asked":"` + path + `"}`), nil
}

func (f *fakeOperator) Plans(_ context.Context, tenant string, selfService bool) ([]lifecycle.Plan, error) {
	f.paths = append(f.paths, "/v1/tenants/"+tenant+"/resources/plans")
	f.selfService = append(f.selfService, selfService)
	if f.err != nil {
		return nil, f.err
	}
	return []lifecycle.Plan{{Name: "nodes-2", Current: true, Selectable: true}}, nil
}

func stateServer(op Lifecycle, held ...string) (*Server, *fakeStore) {
	store := &fakeStore{held: map[string]bool{}}
	for _, line := range held {
		store.held[line] = true
	}
	return New(Config{
		Authn:   fakeAuthn{"ada": "ada", "mel": "mel", "aud": "aud", "pat": "pat"},
		Authz:   store,
		Cluster: "demo", Lifecycle: op,
	}), store
}

// Every read of live state asks the relation the director asked, on the same
// object, and passes the operator's answer to the same question through. The
// table is the contract: a route added without a line here is a route nobody
// decided the relation of.
func TestEachReadAsksItsRelationOnItsObject(t *testing.T) {
	for _, c := range []struct {
		path, relation, object, asked string
	}{
		{"/v1/tenants/acme/apps/status", "can_view", "tenant:acme", "/v1/tenants/acme/apps/status"},
		{"/v1/tenants/acme/resources", "can_view", "tenant:acme", "/v1/tenants/acme/resources"},
		{"/v1/tenants/acme/resources/plans", "can_view", "tenant:acme", "/v1/tenants/acme/resources/plans"},
		{"/v1/tenants/acme/resources/usage", "can_view", "tenant:acme", "/v1/tenants/acme/resources/usage"},
		{"/v1/tenants/acme/resources/report", "can_view", "tenant:acme", "/v1/tenants/acme/resources/report"},
		{"/v1/tenants/acme/backups", "can_view", "tenant:acme", "/v1/tenants/acme/backups"},
		{"/v1/tenants/acme/backups/nightly-1", "can_view", "tenant:acme", "/v1/tenants/acme/backups/nightly-1"},
		{"/v1/tenants/acme/backup-policy", "can_view", "tenant:acme", "/v1/tenants/acme/backup-policy"},
		{"/v1/tenants/acme/backup-schedules", "can_view", "tenant:acme", "/v1/tenants/acme/backup-schedules"},
		{"/v1/tenants/acme/integrations", "can_view", "tenant:acme", "/v1/tenants/acme/integrations"},
		{"/v1/tenants/acme/notifications", "can_view", "tenant:acme", "/v1/tenants/acme/notifications"},
		{"/v1/clusters/demo/resources", "can_audit", "cluster:demo", "/v1/resources"},
		{"/v1/clusters/demo/backup-policy", "can_audit", "cluster:demo", "/v1/backup-policy"},
		{"/v1/clusters/demo/backup-schedules", "can_audit", "cluster:demo", "/v1/backup-schedules"},
		{"/v1/clusters/demo/platform-security", "can_audit", "cluster:demo", "/v1/platform-security"},
		{"/v1/clusters/demo/customizations", "can_audit", "cluster:demo", "/v1/customizations"},
		{"/v1/clusters/demo/licence-report", "can_audit", "cluster:demo", "/v1/licence-report"},
	} {
		t.Run(c.path, func(t *testing.T) {
			op := &fakeOperator{}
			// Ada holds exactly the relation the route names. Mel holds every
			// other relation there is on the same object and on the other
			// kind of object, and it must not help.
			s, _ := stateServer(op,
				"user:ada "+c.relation+" "+c.object,
				"user:mel can_enter "+c.object, "user:mel can_administer "+c.object,
				"user:mel can_view tenant:other", "user:mel can_audit cluster:elsewhere",
				"user:mel can_configure "+c.object,
			)
			if c.relation == "can_view" {
				// A cluster relation is not a tenant's.
				s.cfg.Authz.(*fakeStore).held["user:mel can_audit cluster:demo"] = true
			} else {
				s.cfg.Authz.(*fakeStore).held["user:mel can_view tenant:acme"] = true
			}

			if code, _ := ask(t, s, c.path, ""); code != http.StatusUnauthorized {
				t.Fatalf("no token: %d", code)
			}
			if code, _ := ask(t, s, c.path, "mel"); code != http.StatusForbidden {
				t.Fatalf("a caller without %s on %s was answered: %d", c.relation, c.object, code)
			}
			if len(op.paths) != 0 {
				t.Fatalf("the operator was asked before the store allowed it: %v", op.paths)
			}
			code, _ := ask(t, s, c.path, "ada")
			if code != http.StatusOK {
				t.Fatalf("the holder of %s on %s: %d", c.relation, c.object, code)
			}
			if len(op.paths) != 1 || op.paths[0] != c.asked {
				t.Fatalf("the operator was asked %v, want %s", op.paths, c.asked)
			}
		})
	}
}

// Another tenant's name in the path is another object: holding the relation
// on one's own tenant reads nothing of somebody else's.
func TestOneTenantsStateIsNotAnothers(t *testing.T) {
	op := &fakeOperator{}
	s, _ := stateServer(op, "user:ada can_view tenant:acme", "user:ada can_administer tenant:acme")
	for _, path := range []string{
		"/v1/tenants/other/backups", "/v1/tenants/other/resources", "/v1/tenants/other/apps/status",
		"/v1/tenants/other/notifications", "/v1/clusters/demo/resources",
	} {
		if code, _ := ask(t, s, path, "ada"); code != http.StatusForbidden {
			t.Errorf("%s: %d, want 403", path, code)
		}
	}
	if len(op.paths) != 0 {
		t.Fatalf("the operator was asked: %v", op.paths)
	}
}

// Only this cluster exists here, and a name that is not a tenant's never
// reaches the store or the operator.
func TestNamesAreCheckedBeforeAnythingIsAsked(t *testing.T) {
	op := &fakeOperator{}
	s, _ := stateServer(op, "user:aud can_audit cluster:demo", "user:aud can_audit cluster:other")
	if code, _ := ask(t, s, "/v1/clusters/other/backup-policy", "aud"); code != http.StatusBadRequest {
		t.Fatalf("another cluster's id: %d", code)
	}
	if code, _ := ask(t, s, "/v1/tenants/Not_A_Tenant/backups", "aud"); code != http.StatusBadRequest {
		t.Fatalf("a name that is not a tenant's: %d", code)
	}
	if len(op.paths) != 0 {
		t.Fatalf("the operator was asked: %v", op.paths)
	}
}

// The operator's answer goes through as it came, status included, and only
// the query parameters a route names go with the question.
func TestTheOperatorsAnswerIsPassedThrough(t *testing.T) {
	op := &fakeOperator{}
	s, _ := stateServer(op, "user:ada can_view tenant:acme")
	code, body := ask(t, s, "/v1/tenants/acme/resources/usage?from=2026-09-01T00:00:00Z&stepSeconds=3600&selfService=false&x=1", "ada")
	if code != http.StatusOK || body["asked"] != "/v1/tenants/acme/resources/usage" {
		t.Fatalf("usage: %d %v", code, body)
	}
	q := op.queries[0]
	if q.Get("from") != "2026-09-01T00:00:00Z" || q.Get("stepSeconds") != "3600" || len(q) != 2 {
		t.Fatalf("query passed on: %v", q)
	}
	// A read that takes no parameters passes none.
	if _, _ = ask(t, s, "/v1/tenants/acme/backups?from=x", "ada"); len(op.queries[1]) != 0 {
		t.Fatalf("a parameter was passed to a read that takes none: %v", op.queries[1])
	}

	op.status, op.body = http.StatusBadRequest, `{"detail":"tenant \"acme\" not found"}`
	if code, body := ask(t, s, "/v1/tenants/acme/resources", "ada"); code != http.StatusBadRequest || body["detail"] == nil {
		t.Fatalf("the operator's refusal was not passed through: %d %v", code, body)
	}

	op.status, op.err = 0, errors.New("connection refused")
	if code, _ := ask(t, s, "/v1/tenants/acme/resources", "ada"); code != http.StatusBadGateway {
		t.Fatalf("an operator that does not answer: %d", code)
	}
}

// Whether a caller chooses a plan for themselves is the store's answer and
// never the request's: the tenant's administrator is shown the self-service
// catalogue, whoever may configure the cluster is shown all of it.
func TestSelfServiceIsTheStoresAnswer(t *testing.T) {
	op := &fakeOperator{}
	s, _ := stateServer(op,
		"user:ada can_view tenant:acme",
		"user:pat can_view tenant:acme", "user:pat can_configure cluster:demo",
	)
	code, body := ask(t, s, "/v1/tenants/acme/resources/plans?selfService=false", "ada")
	if code != http.StatusOK || body["tenant"] != "acme" || len(body["plans"].([]any)) != 1 {
		t.Fatalf("plans: %d %v", code, body)
	}
	if _, _ = ask(t, s, "/v1/tenants/acme/resources/plans?selfService=true", "pat"); len(op.selfService) != 2 {
		t.Fatalf("the operator was asked %d times", len(op.selfService))
	}
	if !op.selfService[0] || op.selfService[1] {
		t.Fatalf("self-service was taken from the request: tenant admin %v, platform admin %v", op.selfService[0], op.selfService[1])
	}
}

// The cluster's view names every tenant the operator answers for, and the
// ones it could not, under this cluster's id.
func TestTheClustersViewNamesEveryTenant(t *testing.T) {
	s, _ := stateServer(&fakeOperator{}, "user:aud can_audit cluster:demo")
	code, body := ask(t, s, "/v1/clusters/demo/resources", "aud")
	if code != http.StatusOK || body["cluster"] != "demo" {
		t.Fatalf("cluster view: %d %v", code, body)
	}
	if len(body["tenants"].([]any)) != 1 || len(body["unavailable"].([]any)) != 1 {
		t.Fatalf("cluster view: %v", body)
	}
}

// The usher reads. Nothing it serves makes anything happen, no bundle leaves
// through it, and without an operator to ask it serves no live state at all.
func TestTheUsherHasNoCommandAndNoBundle(t *testing.T) {
	op := &fakeOperator{}
	s, _ := stateServer(op, "user:ada can_view tenant:acme", "user:ada can_administer tenant:acme")
	for _, c := range []struct{ method, path string }{
		{"POST", "/v1/tenants/acme/actions/backup"},
		{"POST", "/v1/tenants/acme/actions/delete-backup"},
		{"POST", "/v1/tenants/acme/actions/purge-app"},
		{"POST", "/v1/tenants/acme/actions/notify"},
		{"PUT", "/v1/tenants/acme/resources"},
		{"PUT", "/v1/tenants/acme/backup-policy"},
		{"DELETE", "/v1/tenants/acme/backups/nightly-1"},
		{"GET", "/v1/tenants/acme/backups/nightly-1/download"},
	} {
		req := httptest.NewRequest(c.method, c.path, nil)
		req.Header.Set("Authorization", "Bearer ada")
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound && rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s %s answered %d", c.method, c.path, rec.Code)
		}
	}
	if len(op.paths) != 0 {
		t.Fatalf("the operator was asked: %v", op.paths)
	}

	bare := New(Config{Authn: fakeAuthn{"ada": "ada"}, Authz: &fakeStore{held: map[string]bool{"user:ada can_view tenant:acme": true}}})
	if code, _ := ask(t, bare, "/v1/tenants/acme/backups", "ada"); code != http.StatusNotFound {
		t.Fatalf("live state without an operator: %d", code)
	}
}

// The last licence report is the operator's record, passed through as it
// came: the body exactly as it was sent, or the plain statement that this
// cluster does not report.
func TestTheLicenceReportIsTheOperatorsRecord(t *testing.T) {
	for _, record := range []string{
		`{"enabled":false}`,
		`{"enabled":true,"url":"https://reports.example/v1","attempt":{"at":"2026-01-02T03:04:05Z","outcome":"accepted","httpStatus":202},` +
			`"report":{"sequence":7,"body":"{\"version\":1}","signature":"ed25519=c2ln","keyId":"0011223344556677"}}`,
	} {
		op := &fakeOperator{status: http.StatusOK, body: record}
		s, _ := stateServer(op, "user:aud can_audit cluster:demo")
		req := httptest.NewRequest(http.MethodGet, "/v1/clusters/demo/licence-report", nil)
		req.Header.Set("Authorization", "Bearer aud")
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK || rec.Body.String() != record {
			t.Fatalf("answer = %d %s, want the record as the operator gave it", rec.Code, rec.Body.String())
		}
	}
}
