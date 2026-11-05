/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package custodian

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

// A catalog the custodian cannot read is its own fault, answered 502 on every
// route that reads it, and not a requirement that does not exist.
func TestACatalogThatCannotBeReadIsNotANotFound(t *testing.T) {
	s, _ := newServer(t, requirement("smtp-relay", "cluster", "gentian-os/kernel/mail/relay", 0))
	s.Catalogue.Client = fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
				return errors.New("apiserver says: connection refused to 10.0.0.1")
			},
		}).Build()

	for _, r := range []struct{ method, target, body string }{
		{http.MethodGet, "/v1/credentials", ""},
		{http.MethodGet, "/v1/credentials/smtp-relay", ""},
		{http.MethodPut, "/v1/credentials/smtp-relay", `{"fields":{"password":"correct-horse"}}`},
	} {
		w := do(t, s, r.method, r.target, r.body)
		if w.Code != http.StatusBadGateway {
			t.Errorf("%s %s = %d %s, want 502", r.method, r.target, w.Code, w.Body.String())
		}
		if strings.Contains(w.Body.String(), "10.0.0.1") {
			t.Errorf("%s %s hands the cluster's error to the caller: %s", r.method, r.target, w.Body.String())
		}
	}
}

// A store that fails on a scope asked about while listing fails the request,
// rather than hiding that scope's requirements as if the caller could not see
// them.
func TestAStoreThatFailsMidListIsNotANotFound(t *testing.T) {
	s, _ := newServer(t,
		requirement("smtp-relay", "cluster", "gentian-os/kernel/mail/relay", 0),
		tenantRequirement("acme-smtp", "acme", "gentian-os/tenants/acme/mail"))
	s.Authz = &failingOn{fakeStore: storeFor([]string{"cluster-admin"}, nil), object: "tenant:acme"}

	if code, names := listed(t, s); code != http.StatusServiceUnavailable || names != nil {
		t.Fatalf("list = %d %v, want 503 and nothing", code, names)
	}
	for _, name := range []string{"acme-smtp", "smtp-relay"} {
		if w := do(t, s, http.MethodGet, "/v1/credentials/"+name, ""); w.Code != http.StatusServiceUnavailable {
			t.Errorf("get %s = %d %s, want 503", name, w.Code, w.Body.String())
		}
	}
}

// A requirement the caller may not see and one that does not exist are the
// same answer, so the answer does not say which tenants have what.
func TestNotVisibleAndAbsentAreOneAnswer(t *testing.T) {
	s, _ := newServerAsTenant(t, "acme",
		tenantRequirement("other-smtp", "other", "gentian-os/tenants/other/mail"))

	hidden := do(t, s, http.MethodGet, "/v1/credentials/other-smtp", "")
	absent := do(t, s, http.MethodGet, "/v1/credentials/nothing-here", "")
	if hidden.Code != http.StatusNotFound || absent.Code != http.StatusNotFound {
		t.Fatalf("hidden = %d, absent = %d, want 404 for both", hidden.Code, absent.Code)
	}
	if strings.ReplaceAll(hidden.Body.String(), "other-smtp", "X") != strings.ReplaceAll(absent.Body.String(), "nothing-here", "X") {
		t.Fatalf("the answers differ:\n %s\n %s", hidden.Body.String(), absent.Body.String())
	}
}

// failingOn is a store that cannot answer about one object.
type failingOn struct {
	*fakeStore
	object string
}

func (f *failingOn) Check(ctx context.Context, user, relation, object string) (bool, error) {
	if object == f.object {
		return false, errors.New("store unreachable")
	}
	return f.fakeStore.Check(ctx, user, relation, object)
}
