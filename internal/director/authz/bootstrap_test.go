/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package authz

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// These run against a real OpenFGA: make test-director-contract.
func requireOpenFGA(t *testing.T) Options {
	t.Helper()
	base := os.Getenv("DIRECTOR_TEST_OPENFGA_URL")
	if base == "" {
		t.Skip("needs OpenFGA: make test-director-contract")
	}
	return Options{BaseURL: base, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

func TestBootstrapIsWhereTheStoreAndTheModelComeFrom(t *testing.T) {
	o := requireOpenFGA(t)
	ctx := context.Background()

	store, model, err := Bootstrap(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	if store == "" || model == "" {
		t.Fatalf("store=%q model=%q", store, model)
	}

	// Again: the same store, the same model. A restart must not add a version.
	store2, model2, err := Bootstrap(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	if store2 != store || model2 != model {
		t.Fatalf("first: store=%q model=%q\nsecond: store=%q model=%q", store, model, store2, model2)
	}

	// And the model that is there is the one this director embeds.
	c, err := newClient(o)
	if err != nil {
		t.Fatal(err)
	}
	var latest struct {
		Models []map[string]any `json:"authorization_models"`
	}
	if err := c.get(ctx, "/stores/"+store+"/authorization-models?page_size=10", &latest); err != nil {
		t.Fatal(err)
	}
	if len(latest.Models) != 1 {
		t.Fatalf("%d models in the store, want 1", len(latest.Models))
	}
	var want map[string]any
	_ = json.Unmarshal(modelV1, &want)
	if !sameModel(latest.Models[0], want) {
		t.Fatal("the store's model is not the embedded one")
	}
}

// What bootstrap produced must answer the questions model v1 defines.
func TestTheBootstrappedModelAnswersAsV1(t *testing.T) {
	o := requireOpenFGA(t)
	ctx := context.Background()
	store, model, err := Bootstrap(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	o.StoreID, o.ModelID = store, model
	c, err := NewOpenFGA(o)
	if err != nil {
		t.Fatal(err)
	}
	writes := []Tuple{
		{User: "user:tom", Relation: "member", Object: "group:gentian/tenant/demo/admins"},
		{User: "group:gentian/tenant/demo/admins#member", Relation: "admin", Object: "tenant:demo"},
	}
	if err := c.Write(ctx, writes, nil); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Write(ctx, nil, writes) }()
	ok, err := c.Check(ctx, "test", "user:tom", "can_install_app", "tenant:demo")
	if err != nil || !ok {
		t.Fatalf("can_install_app = %v, %v", ok, err)
	}
	if ok, _ := c.Check(ctx, "test", "user:mia", "can_install_app", "tenant:demo"); ok {
		t.Fatal("a stranger may install")
	}
}

// The embedded model must be the file the repository tests and the lint check.
func TestTheEmbeddedModelIsTheRepositorysModel(t *testing.T) {
	onDisk, err := os.ReadFile("../../../authz/model/v1/model.json")
	if err != nil {
		t.Fatal(err)
	}
	var a, b any
	if err := json.Unmarshal(onDisk, &a); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(modelV1, &b); err != nil {
		t.Fatal(err)
	}
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	if string(x) != string(y) {
		t.Fatal("internal/director/authz/model.json differs from authz/model/v1/model.json (make gen-all copies it)")
	}
}

// The claim decides who administers a cluster, so this is the tuple-writing
// path that grants and takes away authority over it.
func TestClusterRolesFollowTheClaim(t *testing.T) {
	o := requireOpenFGA(t)
	ctx := context.Background()

	store, model, err := Bootstrap(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	o.StoreID, o.ModelID = store, model
	c, err := NewOpenFGA(o)
	if err != nil {
		t.Fatal(err)
	}
	cluster := "roles-test"

	if err := c.ReconcileClusterRoles(ctx, cluster, map[string]string{
		"admin":       "gentian:platform:admin",
		"break_glass": "gentian:platform:break-glass",
	}, []RoleHolder{{Relation: "security_officer", Realm: "demo", Subject: "0b7e"}}); err != nil {
		t.Fatal(err)
	}
	officers, err := c.Read(ctx, Tuple{Relation: "security_officer", Object: Cluster(cluster)})
	if err != nil {
		t.Fatal(err)
	}
	if len(officers) != 1 || officers[0].User != "user:demo/0b7e" {
		t.Fatalf("security_officer tuples = %v, want the holder keyed by realm", officers)
	}
	if err := c.ReconcileClusterRoles(ctx, cluster, nil, []RoleHolder{{Relation: "admin", Realm: "demo", Subject: "0b7e"}}); err == nil {
		t.Fatal("admin is not a role a holder may hold")
	}
	if err := c.ReconcileClusterRoles(ctx, cluster, map[string]string{"auditor": "gentian:platform:auditor"}, nil); err == nil {
		t.Fatal("auditor is not a role a group may hold")
	}
	admins, err := c.Read(ctx, Tuple{Relation: "admin", Object: Cluster(cluster)})
	if err != nil {
		t.Fatal(err)
	}
	if len(admins) != 1 {
		t.Fatalf("admin tuples = %d, want 1", len(admins))
	}

	// Declarative: a role the claim stops assigning is taken away, rather than
	// left behind for whoever was in that group.
	if err := c.ReconcileClusterRoles(ctx, cluster, map[string]string{
		"admin": "gentian:platform:admin",
	}, nil); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{"break_glass", "security_officer"} {
		left, err := c.Read(ctx, Tuple{Relation: rel, Object: Cluster(cluster)})
		if err != nil {
			t.Fatal(err)
		}
		if len(left) != 0 {
			t.Fatalf("%s tuples = %d after the claim dropped the role, want 0", rel, len(left))
		}
	}

	// Running it again writes nothing: the second call has nothing to change.
	if err := c.ReconcileClusterRoles(ctx, cluster, map[string]string{
		"admin": "gentian:platform:admin",
	}, nil); err != nil {
		t.Fatal(err)
	}

	if err := c.ReconcileClusterRoles(ctx, cluster, map[string]string{"nonsense": "x"}, nil); err == nil {
		t.Fatal("a relation the model does not have should be refused, not written")
	}

	// Leave nothing behind for the next run.
	if err := c.ReconcileClusterRoles(ctx, cluster, nil, nil); err != nil {
		t.Fatal(err)
	}
}

func TestTenantsAreAttachedToTheirClusterFromGit(t *testing.T) {
	o := requireOpenFGA(t)
	ctx := context.Background()
	store, model, err := Bootstrap(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	o.StoreID, o.ModelID = store, model
	c, err := NewOpenFGA(o)
	if err != nil {
		t.Fatal(err)
	}
	cluster := "tenants-test"
	if err := c.ReconcileClusterRoles(ctx, cluster, map[string]string{"admin": "gentian:platform:admin"}, nil); err != nil {
		t.Fatal(err)
	}
	if err := c.ReconcileTenants(ctx, cluster, TenantsNamed("platform", "acme")); err != nil {
		t.Fatal(err)
	}
	// A platform administrator: in the group, so admin of the cluster, so
	// through operated_by an admin of every tenant deployed from git, and
	// through cluster able to enter the platform desktop.
	if err := c.Write(ctx, []Tuple{{User: "user:root", Relation: "member", Object: "group:gentian/platform/admin"}}, nil); err != nil {
		t.Fatal(err)
	}
	for _, tenant := range []string{"platform", "acme"} {
		for _, rel := range []string{"can_enter", "can_administer"} {
			ok, err := c.Check(ctx, "test", "user:root", rel, Tenant(tenant))
			if err != nil {
				t.Fatal(err)
			}
			if !ok {
				t.Errorf("platform admin lacks %s on %s", rel, Tenant(tenant))
			}
		}
	}
	// A tenant withdraws consent; a restart does not restore it -- except for
	// the platform tenant, which is always the cluster's.
	if err := c.Write(ctx, nil, []Tuple{{User: Cluster(cluster), Relation: "operated_by", Object: Tenant("acme")}}); err != nil {
		t.Fatal(err)
	}
	if err := c.ReconcileTenants(ctx, cluster, TenantsNamed("platform", "acme")); err != nil {
		t.Fatal(err)
	}
	if ok, _ := c.Check(ctx, "test", "user:root", "can_administer", Tenant("acme")); ok {
		t.Error("a withdrawn operated_by was written back")
	}
	if err := c.Write(ctx, nil, []Tuple{{User: Cluster(cluster), Relation: "operated_by", Object: Tenant("platform")}}); err != nil {
		t.Fatal(err)
	}
	if err := c.ReconcileTenants(ctx, cluster, TenantsNamed("platform")); err != nil {
		t.Fatal(err)
	}
	if ok, _ := c.Check(ctx, "test", "user:root", "can_administer", Tenant("platform")); !ok {
		t.Error("the platform tenant is always operated by its cluster")
	}
}

// Who approves a tenant's perimeter follows the Cluster claim and nothing
// else: a role holder of scope tenant:<t> holds perimeter_approver while the
// claim names them and loses it when it stops, the cluster's perimeter
// approver approves on every tenant, the cluster's administrator wherever the
// tenant is operated by its cluster, and a tenant's administrator only when
// named.
func TestWhoApprovesThePerimeterFollowsTheClaim(t *testing.T) {
	o := requireOpenFGA(t)
	ctx := context.Background()
	store, model, err := Bootstrap(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	o.StoreID, o.ModelID = store, model
	c, err := NewOpenFGA(o)
	if err != nil {
		t.Fatal(err)
	}
	cluster := "perimeter-test"
	if err := c.ReconcileClusterRoles(ctx, cluster, map[string]string{"admin": "gentian:platform:admin"},
		[]RoleHolder{{Relation: "perimeter_approver", Realm: "kernel", Subject: "p-petra"}}); err != nil {
		t.Fatal(err)
	}
	if err := c.Write(ctx, []Tuple{
		{User: "user:p-root", Relation: "member", Object: "group:gentian/platform/admin"},
		{User: "user:p-tom", Relation: "member", Object: "group:gentian/tenant/p-acme/admins"},
		{User: "user:p-mia", Relation: "member", Object: "group:gentian/tenant/p-acme/members"},
	}, nil); err != nil {
		t.Fatal(err)
	}
	expect := func(when string, want map[string]bool) {
		t.Helper()
		for user, allowed := range want {
			for _, rel := range []string{"can_expose", "can_approve_privilege"} {
				ok, err := c.Check(ctx, "test", "user:"+user, rel, Tenant("p-acme"))
				if err != nil {
					t.Fatal(err)
				}
				if ok != allowed {
					t.Errorf("%s: %s for %s = %v, want %v", when, rel, user, ok, allowed)
				}
			}
		}
	}
	project := func(approvers ...string) {
		t.Helper()
		if err := c.ReconcileTenants(ctx, cluster, []TenantRights{
			{Name: "platform"}, {Name: "p-acme", PerimeterApprovers: approvers},
		}); err != nil {
			t.Fatal(err)
		}
	}

	project()
	expect("a new tenant", map[string]bool{"p-root": true, "kernel/p-petra": true, "p-tom": false, "p-acme/p-tom": false, "p-mia": false})

	project("user:p-acme/p-tom")
	expect("named", map[string]bool{"p-root": true, "p-acme/p-tom": true, "p-tom": false, "p-mia": false})

	project()
	expect("no longer named", map[string]bool{"p-root": true, "p-acme/p-tom": false})

	// An entry written by hand does not outlive a projection: the claim is
	// where who approves is said.
	if err := c.Write(ctx, []Tuple{{User: "user:p-mia", Relation: "perimeter_approver", Object: Tenant("p-acme")}}, nil); err != nil {
		t.Fatal(err)
	}
	project()
	expect("written by hand", map[string]bool{"p-mia": false})

	// A tenant that withdrew the operator is not approved for by the
	// cluster's administrator; its named approver and the cluster's
	// perimeter approver still approve.
	if err := c.Write(ctx, nil, []Tuple{{User: Cluster(cluster), Relation: "operated_by", Object: Tenant("p-acme")}}); err != nil {
		t.Fatal(err)
	}
	project("user:p-acme/p-tom")
	expect("the operator withdrawn", map[string]bool{"p-root": false, "p-acme/p-tom": true, "kernel/p-petra": true})
}

// fakeStores is an OpenFGA holding the stores in have, each with no model.
type fakeStores struct {
	have    map[string]bool
	created []string
	listed  bool
}

func (f *fakeStores) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/stores":
		id := fmt.Sprintf("new-%d", len(f.created)+1)
		f.created = append(f.created, id)
		f.have[id] = true
		_ = json.NewEncoder(w).Encode(map[string]any{"id": id, "name": StoreName})
	case r.URL.Path == "/stores":
		f.listed = true
		_ = json.NewEncoder(w).Encode(map[string]any{"stores": []map[string]any{
			{"id": "by-name", "name": StoreName, "created_at": "2025-01-01T00:00:00Z"},
		}})
	case strings.HasSuffix(r.URL.Path, "/authorization-models") && r.Method == http.MethodPost:
		_ = json.NewEncoder(w).Encode(map[string]any{"authorization_model_id": "model-1"})
	case strings.HasSuffix(r.URL.Path, "/authorization-models"):
		_ = json.NewEncoder(w).Encode(map[string]any{"authorization_models": []any{}})
	case strings.HasPrefix(r.URL.Path, "/stores/"):
		id := strings.TrimPrefix(r.URL.Path, "/stores/")
		if !f.have[id] {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": id, "name": StoreName})
	default:
		http.NotFound(w, r)
	}
}

func fakeStoreOptions(t *testing.T, f *fakeStores, recorded string) Options {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return Options{BaseURL: srv.URL, StoreID: recorded, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

// The recorded store is kept, and a cluster whose recorded store is gone
// gets a new one rather than whichever store carries the name.
func TestBootstrapKeepsTheRecordedStoreAndNeverAdoptsOneByNameOverIt(t *testing.T) {
	f := &fakeStores{have: map[string]bool{"recorded": true, "by-name": true}}
	store, _, err := Bootstrap(context.Background(), fakeStoreOptions(t, f, "recorded"))
	if err != nil || store != "recorded" {
		t.Fatalf("store=%q err=%v, want the recorded one", store, err)
	}

	f = &fakeStores{have: map[string]bool{"by-name": true}}
	store, _, err = Bootstrap(context.Background(), fakeStoreOptions(t, f, "gone"))
	if err != nil {
		t.Fatal(err)
	}
	if store != "new-1" || f.listed {
		t.Fatalf("store=%q listed=%v, want a new store and no look by name", store, f.listed)
	}
}

// With nothing recorded the operator adopts the store a cluster made before
// the ids were recorded.
func TestBootstrapAdoptsTheNamedStoreOnlyWhenNothingIsRecorded(t *testing.T) {
	f := &fakeStores{have: map[string]bool{"by-name": true}}
	store, _, err := Bootstrap(context.Background(), fakeStoreOptions(t, f, ""))
	if err != nil || store != "by-name" {
		t.Fatalf("store=%q err=%v, want by-name", store, err)
	}
}
