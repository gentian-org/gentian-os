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
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

const tenantManifest = "apiVersion: gentianos.io/v1alpha1\nkind: Tenant\nmetadata:\n  name: demo\nspec:\n  displayName: Demo\n"

// admitting is a Service whose cluster answers every apply with answer, and
// records how it was asked.
func admitting(answer error) (*Service, *[]client.PatchOption) {
	asked := &[]client.PatchOption{}
	c := fake.NewClientBuilder().WithScheme(runtime.NewScheme()).WithInterceptorFuncs(interceptor.Funcs{
		Patch: func(_ context.Context, _ client.WithWatch, _ client.Object, _ client.Patch, opts ...client.PatchOption) error {
			*asked = append(*asked, opts...)
			return answer
		},
	}).Build()
	return &Service{client: c}, asked
}

// The manifest is applied as a dry run, and the cluster's answer is the
// answer: taken; refused by the Tenant webhook, by a value the schema does
// not take, or by a field it does not declare, each with the cluster's own
// reason; and no verdict at all where the cluster said something else.
func TestTheClusterIsAskedWhetherItTakesATenant(t *testing.T) {
	tenants := schema.GroupResource{Group: "gentianos.io", Resource: "tenants"}
	denied := apierrors.NewForbidden(tenants, "demo", errors.New(`admission webhook "vtenant.gentianos.io" denied the request: tenant "demo": duplicate app profile "notes" in spec.apps`))
	undeclared := apierrors.NewInternalError(errors.New("failed to create typed patch object (/demo; gentianos.io/v1alpha1, Kind=Tenant): .spec.gone: field not declared in schema"))
	invalid := apierrors.NewInvalid(schema.GroupKind{Group: "gentianos.io", Kind: "Tenant"}, "demo", nil)
	notAllowed := apierrors.NewForbidden(tenants, "demo", errors.New("User cannot patch resource"))

	for name, tc := range map[string]struct {
		answer   error
		admitted bool
		reason   string
		unasked  bool
	}{
		"taken":                    {answer: nil, admitted: true},
		"the webhook's refusal":    {answer: denied, reason: "duplicate app profile"},
		"an undeclared field":      {answer: undeclared, reason: ".spec.gone: field not declared in schema"},
		"a value not taken":        {answer: invalid, reason: "is invalid"},
		"this operator may not":    {answer: notAllowed, unasked: true},
		"the cluster is not there": {answer: errors.New("connection refused"), unasked: true},
	} {
		t.Run(name, func(t *testing.T) {
			s, asked := admitting(tc.answer)
			got, err := s.AdmitTenant(context.Background(), "demo", []byte(tenantManifest))
			if tc.unasked {
				if err == nil {
					t.Fatalf("an answer that is no verdict was read as one: %+v", got)
				}
				return
			}
			if err != nil || got.Admitted != tc.admitted || !strings.Contains(got.Reason, tc.reason) {
				t.Fatalf("answer = %+v, %v", got, err)
			}
			var dry bool
			for _, o := range *asked {
				dry = dry || o == client.DryRunAll
			}
			if !dry {
				t.Fatal("the manifest was applied for real")
			}
		})
	}
}

// A manifest that is not the Tenant named is not put to the cluster.
func TestOnlyTheTenantNamedIsAsked(t *testing.T) {
	for name, manifest := range map[string]string{
		"another tenant": strings.Replace(tenantManifest, "name: demo", "name: other", 1),
		"another kind":   strings.Replace(tenantManifest, "kind: Tenant", "kind: Component", 1),
		"no YAML":        "{",
	} {
		t.Run(name, func(t *testing.T) {
			s, asked := admitting(nil)
			if _, err := s.AdmitTenant(context.Background(), "demo", []byte(manifest)); !apierrors.IsBadRequest(err) {
				t.Fatalf("err = %v", err)
			}
			if len(*asked) != 0 {
				t.Fatal("the cluster was asked")
			}
		})
	}
}
