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
	"io"
	"log/slog"
	"os"
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
		"admin":   "gentian:platform:admin",
		"auditor": "gentian:platform:auditor",
	}); err != nil {
		t.Fatal(err)
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
	}); err != nil {
		t.Fatal(err)
	}
	auditors, err := c.Read(ctx, Tuple{Relation: "auditor", Object: Cluster(cluster)})
	if err != nil {
		t.Fatal(err)
	}
	if len(auditors) != 0 {
		t.Fatalf("auditor tuples = %d after the claim dropped the role, want 0", len(auditors))
	}

	// Running it again writes nothing: the second call has nothing to change.
	if err := c.ReconcileClusterRoles(ctx, cluster, map[string]string{
		"admin": "gentian:platform:admin",
	}); err != nil {
		t.Fatal(err)
	}

	if err := c.ReconcileClusterRoles(ctx, cluster, map[string]string{"nonsense": "x"}); err == nil {
		t.Fatal("a relation the model does not have should be refused, not written")
	}

	// Leave nothing behind for the next run.
	if err := c.ReconcileClusterRoles(ctx, cluster, nil); err != nil {
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
	if err := c.ReconcileClusterRoles(ctx, cluster, map[string]string{"admin": "gentian:platform:admin"}); err != nil {
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

// Who approves a tenant's public addresses follows the tenant's manifest and
// nothing else: the administrators' group holds perimeter_approver while the
// manifest says so and loses it when it stops, the cluster's administrator
// approves wherever the tenant is operated by its cluster, and a member of
// the perimeter group always does.
func TestWhoApprovesPublicAddressesFollowsTheManifest(t *testing.T) {
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
	if err := c.ReconcileClusterRoles(ctx, cluster, map[string]string{"admin": "gentian:platform:admin"}); err != nil {
		t.Fatal(err)
	}
	if err := c.Write(ctx, []Tuple{
		{User: "user:p-root", Relation: "member", Object: "group:gentian/platform/admin"},
		{User: "user:p-tom", Relation: "member", Object: "group:gentian/tenant/p-acme/admins"},
		{User: "user:p-pat", Relation: "member", Object: "group:gentian/tenant/p-acme/perimeter"},
		{User: "user:p-mia", Relation: "member", Object: "group:gentian/tenant/p-acme/members"},
		{User: "user:p-root", Relation: "member", Object: "group:gentian/tenant/platform/admins"},
	}, nil); err != nil {
		t.Fatal(err)
	}
	expect := func(when string, want map[string]bool) {
		t.Helper()
		for user, allowed := range want {
			ok, err := c.Check(ctx, "test", "user:"+user, "can_expose", Tenant("p-acme"))
			if err != nil {
				t.Fatal(err)
			}
			if ok != allowed {
				t.Errorf("%s: can_expose for %s = %v, want %v", when, user, ok, allowed)
			}
		}
	}
	project := func(approve bool) {
		t.Helper()
		if err := c.ReconcileTenants(ctx, cluster, []TenantRights{
			{Name: "platform", AdminsApprove: approve}, {Name: "p-acme", AdminsApprove: approve},
		}); err != nil {
			t.Fatal(err)
		}
	}

	project(false)
	expect("a new tenant", map[string]bool{"p-root": true, "p-pat": true, "p-tom": false, "p-mia": false})

	project(true)
	expect("switched on", map[string]bool{"p-root": true, "p-pat": true, "p-tom": true, "p-mia": false})
	// The platform tenant's administrators are the cluster's: the switch
	// writes nothing there.
	approve, _ := adminsApprove(PlatformTenant)
	if have, err := c.Read(ctx, approve); err != nil || len(have) != 0 {
		t.Errorf("the switch wrote %v for the platform tenant (%v)", have, err)
	}

	project(false)
	expect("switched off again", map[string]bool{"p-root": true, "p-pat": true, "p-tom": false, "p-mia": false})

	// An entry of that shape written by hand does not outlive a projection:
	// the manifest is where the switch is.
	approve, _ = adminsApprove("p-acme")
	if err := c.Write(ctx, []Tuple{approve}, nil); err != nil {
		t.Fatal(err)
	}
	project(false)
	expect("written by hand", map[string]bool{"p-tom": false})

	// A tenant that withdrew the operator is not approved for by the
	// cluster's administrator; its own approvers still approve.
	if err := c.Write(ctx, nil, []Tuple{{User: Cluster(cluster), Relation: "operated_by", Object: Tenant("p-acme")}}); err != nil {
		t.Fatal(err)
	}
	project(false)
	expect("the operator withdrawn", map[string]bool{"p-root": false, "p-pat": true, "p-tom": false})
	project(true)
	expect("the operator withdrawn, switched on", map[string]bool{"p-root": false, "p-pat": true, "p-tom": true})
}
