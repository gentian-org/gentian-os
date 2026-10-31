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
	"net/http/httptest"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/keycloak"
	"github.com/gentian-org/gentian-os/internal/layout"
)

// An addon installed before addons had grants of their own gets its app's:
// the app's group's members when the app is given per person, everybody --
// and the marker -- when it is for everyone. An addon somebody already has
// is left alone, and one whose group is not there yet is waited for.
func TestAnAddonWithNobodyInheritsItsAppsAccess(t *testing.T) {
	kc := &realm{groups: map[string]map[string][]string{}, members: map[string][]string{}, users: []string{"ada", "bob", "cy"}}
	srv := httptest.NewServer(kc)
	defer srv.Close()
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = gentianov1alpha1.AddToScheme(scheme)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "keycloak-admin", Namespace: layout.Namespace(layout.Authentication)},
			Data:       map[string][]byte{"url": []byte(srv.URL), "client_id": []byte("admin"), "client_secret": []byte("pw")},
		},
		&gentianov1alpha1.ComponentProfile{ObjectMeta: metav1.ObjectMeta{Name: "cloud-deck"}},
		&gentianov1alpha1.Tenant{ObjectMeta: metav1.ObjectMeta{Name: "acme"},
			Spec: gentianov1alpha1.TenantSpec{Isolation: &gentianov1alpha1.TenantIsolation{KeycloakRealm: "demo"}}},
	).Build()
	s := &Service{client: c}
	ctx := context.Background()
	app := keycloak.TenantAppGroup("acme", "cloud")
	calendar := keycloak.TenantAppGroup("acme", "cloud-calendar")
	deck := keycloak.TenantAppGroup("acme", "cloud-deck")
	tasks := keycloak.TenantAppGroup("acme", "cloud-tasks")
	kc.groups[app] = map[string][]string{}
	kc.members[app] = []string{"ada"}

	if done, err := s.InheritAddonGrant(ctx, "acme", "cloud", "cloud-calendar", false); err != nil || done {
		t.Fatalf("before the addon's group exists: done=%v err=%v", done, err)
	}

	kc.groups[calendar] = map[string][]string{}
	if done, err := s.InheritAddonGrant(ctx, "acme", "cloud", "cloud-calendar", false); err != nil || !done {
		t.Fatalf("per person: done=%v err=%v", done, err)
	}
	if got := strings.Join(kc.members[calendar], ","); got != "ada" {
		t.Fatalf("calendar members = %q, want the app's", got)
	}

	kc.groups[deck] = map[string][]string{}
	if done, err := s.InheritAddonGrant(ctx, "acme", "cloud", "cloud-deck", true); err != nil || !done {
		t.Fatalf("for everyone: done=%v err=%v", done, err)
	}
	if got := strings.Join(kc.members[deck], ","); got != "ada,bob,cy" {
		t.Fatalf("deck members = %q, want everybody", got)
	}
	if v := kc.groups[deck][keycloak.DefaultGrantAttribute]; len(v) != 1 || v[0] != "true" {
		t.Fatalf("deck is not marked for people invited later: %v", kc.groups[deck])
	}

	kc.groups[tasks] = map[string][]string{}
	kc.members[tasks] = []string{"cy"}
	if done, err := s.InheritAddonGrant(ctx, "acme", "cloud", "cloud-tasks", false); err != nil || !done {
		t.Fatalf("already granted: done=%v err=%v", done, err)
	}
	if got := strings.Join(kc.members[tasks], ","); got != "cy" {
		t.Fatalf("an addon somebody had was changed: %q", got)
	}
}
