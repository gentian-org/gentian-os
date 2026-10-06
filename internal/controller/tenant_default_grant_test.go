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
	"errors"
	"strings"
	"testing"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// An app installed for everyone is granted to the tenant's members once.
// These tests drive applyDefaultGrants with a granter that is a tally of who
// is in which group, so that "an administrator took somebody out" is a thing
// a test can do between two reconciles.

// groups stands in for Keycloak: the members of each app's group, and whether
// the group exists at all.
type groups struct {
	people  []string
	members map[string]map[string]bool
	calls   int
	fail    error
}

func (g *groups) exists(profile string) { g.members[profile] = map[string]bool{} }

func (g *groups) grant(_ context.Context, _, profile string) (bool, error) {
	g.calls++
	if g.fail != nil {
		return false, g.fail
	}
	group, ok := g.members[profile]
	if !ok {
		return false, nil
	}
	for _, p := range g.people {
		group[p] = true
	}
	return true, nil
}

func grantTenant(apps ...gentianov1alpha1.TenantApp) *gentianov1alpha1.Tenant {
	return &gentianov1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: "demo"},
		Spec:       gentianov1alpha1.TenantSpec{Apps: apps},
	}
}

func grantCondition(t *testing.T, tenant *gentianov1alpha1.Tenant) *metav1.Condition {
	t.Helper()
	return apimeta.FindStatusCondition(tenant.Status.Conditions, conditionDefaultGrantsApplied)
}

func TestAnAppForEveryoneIsGrantedOnceItsGroupExists(t *testing.T) {
	kc := &groups{people: []string{"ada", "bob"}, members: map[string]map[string]bool{}}
	r := &TenantReconciler{DefaultGrant: kc.grant}
	tenant := grantTenant(
		gentianov1alpha1.TenantApp{Profile: "wiki", DefaultGrant: true},
		gentianov1alpha1.TenantApp{Profile: "mail"},
	)

	// The app is declared and its group is not there yet: nothing is
	// granted, the tenant says what it waits for, and asks to be run again.
	if retry := r.applyDefaultGrants(context.Background(), tenant); !retry {
		t.Fatal("a grant that is waiting for its group did not ask to be retried")
	}
	if c := grantCondition(t, tenant); c == nil || c.Status != metav1.ConditionFalse ||
		c.Reason != reasonDefaultGrantWaiting || !strings.Contains(c.Message, "wiki") {
		t.Fatalf("condition = %+v", c)
	}
	if len(tenant.Status.DefaultGrantedApps) != 0 {
		t.Fatalf("recorded as granted before the group existed: %v", tenant.Status.DefaultGrantedApps)
	}

	// The group appears. Everybody is added, the app is noted as done, and
	// nothing asks to be run again.
	kc.exists("wiki")
	if retry := r.applyDefaultGrants(context.Background(), tenant); retry {
		t.Fatal("a tenant with nothing outstanding asked to be retried")
	}
	if !kc.members["wiki"]["ada"] || !kc.members["wiki"]["bob"] {
		t.Fatalf("members of wiki = %v", kc.members["wiki"])
	}
	if got := tenant.Status.DefaultGrantedApps; len(got) != 1 || got[0] != "wiki" {
		t.Fatalf("DefaultGrantedApps = %v", got)
	}
	if c := grantCondition(t, tenant); c == nil || c.Status != metav1.ConditionTrue || c.Reason != reasonDefaultGrantsApplied {
		t.Fatalf("condition = %+v", c)
	}
	// An app that does not declare the grant was never asked about.
	if _, touched := kc.members["mail"]; touched {
		t.Fatal("an app installed per person was granted")
	}
}

// The point of doing it once: an administrator who takes somebody out of the
// group is not overruled by the next reconcile, or by any after it.
func TestAPersonRemovedFromTheGroupIsNotPutBack(t *testing.T) {
	kc := &groups{people: []string{"ada", "bob"}, members: map[string]map[string]bool{}}
	kc.exists("wiki")
	r := &TenantReconciler{DefaultGrant: kc.grant}
	tenant := grantTenant(gentianov1alpha1.TenantApp{Profile: "wiki", DefaultGrant: true})

	r.applyDefaultGrants(context.Background(), tenant)
	if !kc.members["wiki"]["bob"] || kc.calls != 1 {
		t.Fatalf("first reconcile: members = %v, calls = %d", kc.members["wiki"], kc.calls)
	}

	delete(kc.members["wiki"], "bob")
	for range 3 {
		if retry := r.applyDefaultGrants(context.Background(), tenant); retry {
			t.Fatal("a granted app asked to be retried")
		}
	}
	if kc.members["wiki"]["bob"] {
		t.Fatal("a person an administrator removed was added again")
	}
	if kc.calls != 1 {
		t.Fatalf("the grant ran %d times, want once", kc.calls)
	}
}

// Once per install, not once per name: an app that was uninstalled, or
// stopped declaring the grant, is granted again when it declares it again.
func TestDeclaringTheGrantAgainAppliesItAgain(t *testing.T) {
	kc := &groups{people: []string{"ada"}, members: map[string]map[string]bool{}}
	kc.exists("wiki")
	r := &TenantReconciler{DefaultGrant: kc.grant}
	tenant := grantTenant(gentianov1alpha1.TenantApp{Profile: "wiki", DefaultGrant: true})
	r.applyDefaultGrants(context.Background(), tenant)

	// Withdrawn: the note goes, and so does the condition, which described
	// nothing any more.
	tenant.Spec.Apps[0].DefaultGrant = false
	if retry := r.applyDefaultGrants(context.Background(), tenant); retry {
		t.Fatal("a tenant declaring no grant asked to be retried")
	}
	if len(tenant.Status.DefaultGrantedApps) != 0 || grantCondition(t, tenant) != nil {
		t.Fatalf("status kept a grant nothing declares: %v %+v", tenant.Status.DefaultGrantedApps, grantCondition(t, tenant))
	}

	delete(kc.members["wiki"], "ada")
	tenant.Spec.Apps[0].DefaultGrant = true
	r.applyDefaultGrants(context.Background(), tenant)
	if !kc.members["wiki"]["ada"] || kc.calls != 2 {
		t.Fatalf("declared again: members = %v, calls = %d", kc.members["wiki"], kc.calls)
	}
}

// A failure is a condition with a reason and a retry, and it does not keep
// another app's grant from being applied.
func TestAFailedGrantIsReportedAndRetried(t *testing.T) {
	kc := &groups{people: []string{"ada"}, members: map[string]map[string]bool{}}
	kc.exists("wiki")
	kc.fail = errors.New("keycloak: connection refused")
	r := &TenantReconciler{DefaultGrant: kc.grant}
	tenant := grantTenant(gentianov1alpha1.TenantApp{Profile: "wiki", DefaultGrant: true})

	if retry := r.applyDefaultGrants(context.Background(), tenant); !retry {
		t.Fatal("a failed grant did not ask to be retried")
	}
	c := grantCondition(t, tenant)
	if c == nil || c.Status != metav1.ConditionFalse || c.Reason != reasonDefaultGrantFailed ||
		!strings.Contains(c.Message, "wiki") || !strings.Contains(c.Message, "connection refused") {
		t.Fatalf("condition = %+v", c)
	}
	if len(tenant.Status.DefaultGrantedApps) != 0 {
		t.Fatalf("a failed grant was recorded as done: %v", tenant.Status.DefaultGrantedApps)
	}

	// Keycloak answers again: the next reconcile grants it.
	kc.fail = nil
	if retry := r.applyDefaultGrants(context.Background(), tenant); retry {
		t.Fatal("still asking to be retried after the grant went through")
	}
	if !kc.members["wiki"]["ada"] || len(tenant.Status.DefaultGrantedApps) != 1 {
		t.Fatalf("after recovery: members = %v, granted = %v", kc.members["wiki"], tenant.Status.DefaultGrantedApps)
	}
	if c := grantCondition(t, tenant); c == nil || c.Status != metav1.ConditionTrue {
		t.Fatalf("condition = %+v", c)
	}
}

// An operator started without its app-lifecycle service has nothing to grant
// with. That is said, and not retried: nothing a reconcile does changes it.
func TestWithoutTheLifecycleServiceTheGrantIsReportedNotRetried(t *testing.T) {
	r := &TenantReconciler{}
	tenant := grantTenant(gentianov1alpha1.TenantApp{Profile: "wiki", DefaultGrant: true})
	if retry := r.applyDefaultGrants(context.Background(), tenant); retry {
		t.Fatal("asked to retry something no retry can change")
	}
	if c := grantCondition(t, tenant); c == nil || c.Status != metav1.ConditionFalse || c.Reason != reasonDefaultGrantNoMeans {
		t.Fatalf("condition = %+v", c)
	}
}
