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
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/keycloak"
	"github.com/gentian-org/gentian-os/internal/layout"
)

// realm is a Keycloak with one realm's groups and users, as much of the
// admin API as granting an app touches.
type realm struct {
	mu      sync.Mutex
	groups  map[string]map[string][]string // name -> attributes
	members map[string][]string            // group name -> user ids
	users   []string
	created []string
}

func (k *realm) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	k.mu.Lock()
	defer k.mu.Unlock()
	answer := func(v any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(v)
	}
	const base = "/admin/realms/demo"
	path := r.URL.Path
	switch {
	case strings.HasSuffix(path, "/protocol/openid-connect/token"):
		answer(map[string]any{"access_token": "t", "expires_in": 300})
	case r.Method == http.MethodGet && path == base+"/groups":
		out := []map[string]any{}
		for name := range k.groups {
			out = append(out, map[string]any{"id": "id-" + name, "name": name})
		}
		answer(out)
	case r.Method == http.MethodPost && path == base+"/groups":
		var body struct {
			Name string `json:"name"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		k.groups[body.Name] = map[string][]string{}
		k.created = append(k.created, body.Name)
		w.WriteHeader(http.StatusCreated)
	case r.Method == http.MethodGet && path == base+"/users":
		out := []map[string]any{}
		for _, u := range k.users {
			out = append(out, map[string]any{"id": u, "username": u, "enabled": true})
		}
		answer(out)
	case strings.HasPrefix(path, base+"/groups/id-"):
		name := strings.TrimPrefix(path, base+"/groups/id-")
		if r.Method == http.MethodPut {
			var body struct {
				Attributes map[string][]string `json:"attributes"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			k.groups[name] = body.Attributes
			w.WriteHeader(http.StatusNoContent)
			return
		}
		answer(map[string]any{"id": "id-" + name, "name": name, "attributes": k.groups[name]})
	case r.Method == http.MethodPut && strings.HasPrefix(path, base+"/users/"):
		parts := strings.Split(strings.TrimPrefix(path, base+"/users/"), "/groups/id-")
		if len(parts) != 2 {
			http.NotFound(w, r)
			return
		}
		k.members[parts[1]] = append(k.members[parts[1]], parts[0])
		w.WriteHeader(http.StatusNoContent)
	default:
		http.NotFound(w, r)
	}
}

// A reconciler's grant waits for the app's group and does not make it: until
// the tenant's identity has created the group, nothing is added and nothing
// is created. Once it is there, every member is added and the group is
// marked, with the credential the operator already holds for provision-app.
func TestGrantAppByDefaultWaitsForTheGroupThenAddsEverybody(t *testing.T) {
	kc := &realm{groups: map[string]map[string][]string{}, members: map[string][]string{}, users: []string{"ada", "bob"}}
	srv := httptest.NewServer(kc)
	defer srv.Close()

	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := gentianov1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "keycloak-admin", Namespace: layout.Namespace(layout.Authentication)},
			Data:       map[string][]byte{"url": []byte(srv.URL), "username": []byte("admin"), "password": []byte("pw")},
		},
		&gentianov1alpha1.ComponentProfile{ObjectMeta: metav1.ObjectMeta{Name: "wiki"}},
		// Named apart from its realm: the grant is made in the realm the
		// tenant says is its own, not in one assumed to carry its name.
		&gentianov1alpha1.Tenant{ObjectMeta: metav1.ObjectMeta{Name: "acme"},
			Spec: gentianov1alpha1.TenantSpec{Isolation: &gentianov1alpha1.TenantIsolation{KeycloakRealm: "demo"}}},
	).Build()
	s := &Service{client: c}
	group := keycloak.TenantAppGroup("acme", "wiki")

	granted, err := s.GrantAppByDefault(context.Background(), "acme", "wiki")
	if err != nil || granted {
		t.Fatalf("before the group exists: granted=%v err=%v", granted, err)
	}
	if len(kc.created) != 0 || len(kc.members[group]) != 0 {
		t.Fatalf("a waiting grant made the group or added people: created=%v members=%v", kc.created, kc.members)
	}

	kc.groups[group] = map[string][]string{"kept": {"yes"}}
	granted, err = s.GrantAppByDefault(context.Background(), "acme", "wiki")
	if err != nil || !granted {
		t.Fatalf("with the group there: granted=%v err=%v", granted, err)
	}
	if got := strings.Join(kc.members[group], ","); got != "ada,bob" {
		t.Fatalf("members = %q, want everybody in the realm", got)
	}
	attrs := kc.groups[group]
	if len(attrs[keycloak.DefaultGrantAttribute]) != 1 || attrs[keycloak.DefaultGrantAttribute][0] != "true" {
		t.Fatalf("the group is not marked for people invited later: %v", attrs)
	}
	if len(attrs["kept"]) != 1 {
		t.Fatalf("marking the group dropped an attribute somebody else set: %v", attrs)
	}
}
