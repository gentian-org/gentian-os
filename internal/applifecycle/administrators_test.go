/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package applifecycle

import (
	"reflect"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/keycloak"
)

func TestHowTheAppAdministratorsAdministerInsideAnApp(t *testing.T) {
	const group = "gentian:tenant:acme:app-admins"
	annotated := func(value string) metav1.ObjectMeta {
		return metav1.ObjectMeta{Annotations: map[string]string{keycloak.GroupAttributesAnnotation: value}}
	}
	for _, tc := range []struct {
		name    string
		profile *gentianov1alpha1.ComponentProfile
		want    *Administrators
	}{
		{name: "a profile that maps them to nothing", profile: &gentianov1alpha1.ComponentProfile{}},
		{name: "no profile"},
		{
			name: "the sync job puts them in the role the profile names",
			profile: &gentianov1alpha1.ComponentProfile{Spec: gentianov1alpha1.ComponentProfileSpec{
				Hooks: &gentianov1alpha1.HookSpec{Provisioning: &gentianov1alpha1.ProvisioningSpec{
					PrivilegedRole: &gentianov1alpha1.PrivilegedRoleSpec{Kind: gentianov1alpha1.PrivilegedRoleKindGroup, Name: "admin"},
				}},
			}},
			want: &Administrators{Group: group, Ways: []string{AdministersBySync}, Role: "admin"},
		},
		{
			name: "the roles a module declares for administrators ride on the group",
			profile: &gentianov1alpha1.ComponentProfile{ObjectMeta: annotated(
				`{"gentianOdooAdminRoles":["sales_team.group_sale_manager","account.group_account_manager"],"gentianOdooGroupRoles":["x"]}`)},
			want: &Administrators{Group: group, Ways: []string{AdministersByToken},
				TokenRoles: []string{"account.group_account_manager", "sales_team.group_sale_manager"}},
		},
		{
			name:    "member roles alone map no administrator, nor does an annotation that cannot be read",
			profile: &gentianov1alpha1.ComponentProfile{ObjectMeta: annotated(`{"gentianOdooGroupRoles":["x"]}`)},
		},
		{
			name:    "an annotation that cannot be read",
			profile: &gentianov1alpha1.ComponentProfile{ObjectMeta: annotated(`{`)},
		},
		{
			name: "the sign-in sidecar is told, beside a sync job",
			profile: &gentianov1alpha1.ComponentProfile{Spec: gentianov1alpha1.ComponentProfileSpec{
				Hooks: &gentianov1alpha1.HookSpec{Provisioning: &gentianov1alpha1.ProvisioningSpec{
					PrivilegedRole: &gentianov1alpha1.PrivilegedRoleSpec{Kind: gentianov1alpha1.PrivilegedRoleKindGroup, Name: "Administrator"},
				}},
				Requires: &gentianov1alpha1.RequirementSpec{Services: &gentianov1alpha1.ServiceRequirements{
					Identity: &gentianov1alpha1.IdentityRequirement{Sidecar: &gentianov1alpha1.SignInSidecarSpec{}},
				}},
			}},
			want: &Administrators{Group: group, Ways: []string{AdministersBySignIn, AdministersBySync}, Role: "Administrator"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := administratorsOf("acme", tc.profile); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("administrators:\n got %+v\nwant %+v", got, tc.want)
			}
		})
	}
}
