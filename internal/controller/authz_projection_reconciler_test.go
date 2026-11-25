/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package controller

import (
	"context"
	"reflect"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/director/authz"
)

// While the Cluster claim names no administrator, the account the install
// made administers the cluster; once the claim names one, only who it names
// does. An install account that is not there holds nothing.
func TestTheInstallsAccountAdministersUntilTheClaimNamesSomebody(t *testing.T) {
	ctx := context.Background()
	r := &AuthzProjectionReconciler{BootstrapAdministrator: func(context.Context) (string, string, error) {
		return "kernel", "boot-1", nil
	}}
	officer := authz.RoleHolder{Relation: "security_officer", Realm: "kernel", Subject: "s-1"}
	got, err := r.withBootstrapAdministrator(ctx, []authz.RoleHolder{officer})
	if err != nil {
		t.Fatal(err)
	}
	want := []authz.RoleHolder{officer, {Relation: "admin", Realm: "kernel", Subject: "boot-1"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("holders = %+v, want %+v", got, want)
	}

	named := []authz.RoleHolder{{Relation: "admin", Realm: "kernel", Subject: "a-1"}}
	got, err = r.withBootstrapAdministrator(ctx, named)
	if err != nil || !reflect.DeepEqual(got, named) {
		t.Errorf("holders = %+v, err = %v, want the named administrator alone", got, err)
	}

	r.BootstrapAdministrator = func(context.Context) (string, string, error) { return "kernel", "", nil }
	got, err = r.withBootstrapAdministrator(ctx, nil)
	if err != nil || len(got) != 0 {
		t.Errorf("holders = %+v, err = %v, want nobody", got, err)
	}
}

// A role holder is keyed by the realm of the tenant the claim names -- the
// kernel realm for the platform tenant -- so the tuple answers only that
// realm's token. What a holder may not hold, a role the tenant's people may
// not hold, a scope other than the role's, a tenant the cluster does not
// have or that has no realm of its own, a subject that is no account id and
// an entry that has expired grant nothing, and nor does one whose person of
// a tenant is no longer its administrator. A perimeter approver of a tenant
// is held on that tenant; one of the platform tenant on the cluster. A
// tenant's auditor is a person of the platform tenant, held on the tenant its
// entry names.
func TestRoleHoldersAreProjectedByTheirTenantsRealm(t *testing.T) {
	claim := clusterClaim("")
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	holder := func(role, tenant, subject string, more ...string) interface{} {
		m := map[string]interface{}{"role": role, "tenant": tenant, "subject": subject, "username": "x@example.org"}
		for i := 0; i+1 < len(more); i += 2 {
			m[more[i]] = more[i+1]
		}
		return m
	}
	_ = unstructured.SetNestedSlice(claim.Object, []interface{}{
		holder("securityOfficer", "test", "6f1c"),
		holder("serviceAdmin", "platform", "k1", "scope", "cluster"),
		holder("perimeterApprover", "platform", "k2"),
		holder("perimeterApprover", "test", "6f1c", "scope", "tenant:test", "expiresAt", "2026-10-23T00:00:00Z"),
		holder("auditor", "test", "e1", "expiresAt", "2026-10-22T11:00:00Z"),
		holder("auditor", "test", "e2", "expiresAt", "2026-10-30T00:00:00Z"),
		holder("serviceAdmin", "test", "6f1c"),
		holder("perimeterApprover", "test", "6f1c", "scope", "cluster"),
		holder("auditor", "acme", "a1"),
		holder("admin", "test", "6f1c"),
		holder("breakGlass", "test", "6f1c"),
		holder("auditor", "gone", "b2"),
		holder("auditor", "test", "not a subject"),
		holder("auditor", "test", "n1"), // no longer an administrator of test
		holder("tenantAuditor", "platform", "k3", "scope", "tenant:test"),
		holder("tenantAuditor", "platform", "k4"),                       // names no tenant
		holder("tenantAuditor", "platform", "k5", "scope", "cluster"),   // reads every tenant: that is auditor
		holder("tenantAuditor", "test", "6f1c", "scope", "tenant:test"), // a person of the tenant itself
	}, "spec", "platformAccess", "roleHolders")
	test := &gentianov1alpha1.Tenant{}
	test.Name = "test"
	acme := &gentianov1alpha1.Tenant{}
	acme.Name = "acme"
	acme.Spec.Isolation = &gentianov1alpha1.TenantIsolation{KeycloakRealm: "acme-people"}
	platform := &gentianov1alpha1.Tenant{}
	platform.Name = "platform"
	platform.Spec.Isolation = &gentianov1alpha1.TenantIsolation{KeycloakRealm: "kernel"}

	r := &AuthzProjectionReconciler{
		Client:  fake.NewClientBuilder().WithScheme(appStoreScheme(t)).WithObjects(claim, test, acme, platform).Build(),
		Cluster: "test",
		Admins: func(_ context.Context, tenant, sub string) (bool, error) {
			if tenant == "platform" {
				t.Errorf("a person of the platform tenant was asked about: %s", sub)
			}
			return sub != "n1", nil
		},
	}
	got, next, err := r.roleHolders(context.Background(), now)
	if err != nil {
		t.Fatal(err)
	}
	want := []authz.RoleHolder{
		{Relation: "security_officer", Realm: "test", Subject: "6f1c"},
		{Relation: "service_admin", Realm: "kernel", Subject: "k1"},
		{Relation: "perimeter_approver", Realm: "kernel", Subject: "k2"},
		{Relation: "perimeter_approver", Realm: "test", Subject: "6f1c", Tenant: "test"},
		{Relation: "auditor", Realm: "test", Subject: "e2"},
		{Relation: "auditor", Realm: "kernel", Subject: "k3", Tenant: "test"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("roleHolders() = %v, want %v", got, want)
	}
	if !r.notInEffectWarned["auditor/test/n1"] {
		t.Error("the entry that is not in effect was not reported")
	}
	if want := time.Date(2026, 10, 23, 0, 0, 0, 0, time.UTC); !next.Equal(want) {
		t.Errorf("next expiry %v, want %v", next, want)
	}
	rights, err := r.tenantRights(context.Background(), got)
	if err != nil {
		t.Fatal(err)
	}
	for _, tr := range rights {
		if tr.Name == "test" && !reflect.DeepEqual(tr.PerimeterApprovers, []string{"user:test/6f1c"}) {
			t.Errorf("test's perimeter approvers: %v", tr.PerimeterApprovers)
		}
		if tr.Name != "test" && len(tr.PerimeterApprovers) != 0 {
			t.Errorf("%s's perimeter approvers: %v", tr.Name, tr.PerimeterApprovers)
		}
		if tr.Name == "test" && !reflect.DeepEqual(tr.Auditors, []string{"user:kernel/k3"}) {
			t.Errorf("test's auditors: %v", tr.Auditors)
		}
		if tr.Name != "test" && len(tr.Auditors) != 0 {
			t.Errorf("%s's auditors: %v", tr.Name, tr.Auditors)
		}
	}
}

// A role on something still installed is taken from a position's holders;
// one on an app that is gone is not, and neither is anything of another
// tenant.
func TestStillInstalledKnowsTheAppsTheirAddonsAndTheirRoles(t *testing.T) {
	present := stillInstalled(map[string][]authz.InstalledApp{
		"acme": {{Profile: "odoo-base-ce", Roles: []authz.DeclaredRole{{Name: "manager"}},
			Addons: []authz.InstalledApp{{Profile: "crm-ce", Roles: []authz.DeclaredRole{{Name: "salesman"}}}}}},
	})
	for object, want := range map[string]bool{
		authz.App("acme", "odoo-base-ce"):             true,
		authz.Role("acme", "odoo-base-ce", "manager"): true,
		authz.App("acme", "crm-ce"):                   true,
		authz.Role("acme", "crm-ce", "salesman"):      true,
		authz.App("acme", "xwiki-ce"):                 false,
		authz.Role("acme", "odoo-base-ce", "auditor"): false,
		authz.App("other", "odoo-base-ce"):            false,
	} {
		if got := present(object); got != want {
			t.Errorf("%s: %v, want %v", object, got, want)
		}
	}
}

// What a change of positions took is told by tenant, each once; nothing is
// told where nothing was taken.
func TestTheProjectionTellsWhomAChangeOfPositionsTookFrom(t *testing.T) {
	told := map[string][]string{}
	r := &AuthzProjectionReconciler{RightsTaken: func(_ context.Context, tenant string, subjects []string) error {
		told[tenant] = append(told[tenant], subjects...)
		return nil
	}}
	r.endSessionsOfTaken(context.Background(), map[string][]string{"acme": {"anna", "ben"}, "shop": {"carl"}})
	if len(told) != 2 || len(told["acme"]) != 2 || told["shop"][0] != "carl" {
		t.Fatalf("told %v", told)
	}
	(&AuthzProjectionReconciler{}).endSessionsOfTaken(context.Background(), map[string][]string{"acme": {"anna"}})
}
