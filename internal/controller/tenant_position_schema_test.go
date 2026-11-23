/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package controller_test

import (
	"context"
	"testing"

	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// A tenant declares positions of its own, never one under a name every
// tenant has: user, all-apps, app-administrator, tenant-administrator. The
// position all-apps administers nothing, so a sign-up may give it.
func TestTheTenantSchemaRefusesTheNamesOfThePositionsEveryTenantHas(t *testing.T) {
	t.Parallel()
	tenant := func(spec gentianov1alpha1.TenantSpec) *gentianov1alpha1.Tenant {
		spec.DisplayName = "Positions"
		return &gentianov1alpha1.Tenant{ObjectMeta: metav1.ObjectMeta{Name: "position-schema-probe"}, Spec: spec}
	}
	for name, c := range map[string]struct {
		spec     gentianov1alpha1.TenantSpec
		admitted bool
	}{
		"its own":           {gentianov1alpha1.TenantSpec{Positions: []gentianov1alpha1.TenantPosition{{Name: "sales"}}}, true},
		"user":              {gentianov1alpha1.TenantSpec{Positions: []gentianov1alpha1.TenantPosition{{Name: "user"}}}, false},
		"all-apps":          {gentianov1alpha1.TenantSpec{Positions: []gentianov1alpha1.TenantPosition{{Name: "all-apps"}}}, false},
		"app-administrator": {gentianov1alpha1.TenantSpec{Positions: []gentianov1alpha1.TenantPosition{{Name: "app-administrator"}}}, false},
		"tenant-administrator": {gentianov1alpha1.TenantSpec{
			Positions: []gentianov1alpha1.TenantPosition{{Name: "tenant-administrator"}}}, false},
		"all-apps at sign-up": {gentianov1alpha1.TenantSpec{
			SignUp: &gentianov1alpha1.TenantSignUp{Enabled: true, Positions: []string{"all-apps"}}}, true},
	} {
		err := testClient.Create(context.Background(), tenant(c.spec), client.DryRunAll)
		switch {
		case c.admitted && err != nil:
			t.Errorf("%s: refused: %v", name, err)
		case !c.admitted && err == nil:
			t.Errorf("%s: admitted", name)
		case !c.admitted && !k8serrors.IsInvalid(err):
			t.Errorf("%s: refused, but not by the schema: %v", name, err)
		}
	}
}
