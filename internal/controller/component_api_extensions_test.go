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
	"encoding/json"
	"strings"
	"testing"
	"time"

	rbacv1 "k8s.io/api/rbac/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	fakediscovery "k8s.io/client-go/discovery/fake"
	clienttesting "k8s.io/client-go/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/security"
)

const extensionGroup = "backup.example.org"

func extensionDefinition(t *testing.T, group, plural string) runtime.RawExtension {
	t.Helper()
	crd := apiextensionsv1.CustomResourceDefinition{
		TypeMeta:   metav1.TypeMeta{APIVersion: "apiextensions.k8s.io/v1", Kind: "CustomResourceDefinition"},
		ObjectMeta: metav1.ObjectMeta{Name: plural + "." + group},
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{
			Group: group,
			Names: apiextensionsv1.CustomResourceDefinitionNames{Plural: plural, Kind: "Schedule"},
			Scope: apiextensionsv1.NamespaceScoped,
			Versions: []apiextensionsv1.CustomResourceDefinitionVersion{{
				Name: "v1", Served: true, Storage: true,
				Schema: &apiextensionsv1.CustomResourceValidation{OpenAPIV3Schema: &apiextensionsv1.JSONSchemaProps{Type: "object"}},
			}},
		},
	}
	raw, err := json.Marshal(crd)
	if err != nil {
		t.Fatal(err)
	}
	return runtime.RawExtension{Raw: raw}
}

type apiExtensionCase struct {
	definitions []runtime.RawExtension
	grants      []gentianov1alpha1.PrivilegeGrant
	existing    []client.Object
	// served is what discovery says the group serves.
	served []metav1.APIResource
}

func apiExtensionGranted(t *testing.T) apiExtensionCase {
	return apiExtensionCase{
		definitions: []runtime.RawExtension{extensionDefinition(t, extensionGroup, "schedules")},
		grants: []gentianov1alpha1.PrivilegeGrant{{
			Privilege: "apiExtensions/schedules", Approver: "bea",
			ApprovedAt: metav1.Now(), Reason: "the backup add-on's schedules",
		}},
	}
}

func (c apiExtensionCase) build(t *testing.T) (*ComponentReconciler, *gentianov1alpha1.Component, *gentianov1alpha1.ComponentProfile) {
	t.Helper()
	scheme := runtime.NewScheme()
	_ = gentianov1alpha1.AddToScheme(scheme)
	_ = rbacv1.AddToScheme(scheme)
	_ = apiextensionsv1.AddToScheme(scheme)
	profile := &gentianov1alpha1.ComponentProfile{
		ObjectMeta: metav1.ObjectMeta{Name: "backup"},
		Spec: gentianov1alpha1.ComponentProfileSpec{
			Requires: &gentianov1alpha1.RequirementSpec{
				Privileges: &gentianov1alpha1.PrivilegeRequest{
					APIExtensions: []gentianov1alpha1.APIExtensionRequest{{
						Name: "schedules", Group: extensionGroup, ServiceAccount: "backup",
						Definitions: c.definitions, Reason: "the schedules of the backup add-on",
					}},
				},
			},
		},
	}
	comp := &gentianov1alpha1.Component{
		ObjectMeta: metav1.ObjectMeta{Name: "backup", Namespace: "tenant-platform"},
		Spec: gentianov1alpha1.ComponentSpec{
			ProfileRef: gentianov1alpha1.ProfileRef{Name: "backup"},
			Privileges: c.grants,
		},
	}
	// A grant names the request it approved, as the director records it.
	for i := range comp.Spec.Privileges {
		if comp.Spec.Privileges[i].Digest == "" {
			d, err := security.APIExtensionDigest(&profile.Spec.Requires.Privileges.APIExtensions[0])
			if err != nil {
				t.Fatal(err)
			}
			comp.Spec.Privileges[i].Digest = d
		}
	}
	objs := append([]client.Object{profile}, c.existing...)
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
	disc := &fakediscovery.FakeDiscovery{Fake: &clienttesting.Fake{}}
	if len(c.served) > 0 {
		disc.Resources = []*metav1.APIResourceList{{GroupVersion: extensionGroup + "/v1", APIResources: c.served}}
	}
	return &ComponentReconciler{Client: cl, Scheme: scheme, CRDReader: cl, Discovery: disc}, comp, profile
}

func apiExtensionsCondition(comp *gentianov1alpha1.Component) *metav1.Condition {
	for i := range comp.Status.Conditions {
		if comp.Status.Conditions[i].Type == conditionAPIExtensionsInstalled {
			return &comp.Status.Conditions[i]
		}
	}
	return nil
}

func installedExtension(t *testing.T, r *ComponentReconciler, name string) *apiextensionsv1.CustomResourceDefinition {
	t.Helper()
	crd := &apiextensionsv1.CustomResourceDefinition{}
	if err := r.Get(context.Background(), client.ObjectKey{Name: name}, crd); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		t.Fatal(err)
	}
	return crd
}

// Granted, the types are installed as the profile's, the one reading role is
// given exactly them, and it is bound to the account the request names.
func TestAGrantedAPIExtensionIsInstalledAndRead(t *testing.T) {
	r, comp, profile := apiExtensionGranted(t).build(t)
	hold, err := r.ensureAPIExtensions(context.Background(), comp, profile)
	if err != nil || hold != "" {
		t.Fatalf("ensureAPIExtensions = %q, %v", hold, err)
	}
	crd := installedExtension(t, r, "schedules."+extensionGroup)
	if crd == nil || crd.Labels[security.APIExtensionLabel] != "backup" {
		t.Fatalf("the type is not installed as the profile's: %+v", crd)
	}

	role := &rbacv1.ClusterRole{}
	if err := r.Get(context.Background(), client.ObjectKey{Name: security.APIExtensionsClusterRole}, role); err != nil {
		t.Fatal(err)
	}
	want := []rbacv1.PolicyRule{
		{APIGroups: []string{extensionGroup}, Resources: []string{"schedules"}, Verbs: []string{"get", "list", "watch"}},
		{APIGroups: []string{extensionGroup}, Resources: []string{"schedules/status"}, Verbs: []string{"get", "update", "patch"}},
	}
	if len(role.Rules) != len(want) {
		t.Fatalf("the reading role = %+v, want %+v", role.Rules, want)
	}
	for i := range want {
		if strings.Join(role.Rules[i].Resources, ",") != strings.Join(want[i].Resources, ",") ||
			strings.Join(role.Rules[i].Verbs, ",") != strings.Join(want[i].Verbs, ",") {
			t.Fatalf("the reading role = %+v, want %+v", role.Rules, want)
		}
	}

	bindings := &rbacv1.ClusterRoleBindingList{}
	if err := r.List(context.Background(), bindings); err != nil {
		t.Fatal(err)
	}
	if len(bindings.Items) != 1 {
		t.Fatalf("bindings = %d, want one", len(bindings.Items))
	}
	b := bindings.Items[0]
	subject := rbacv1.Subject{Kind: "ServiceAccount", Name: "backup", Namespace: "tenant-platform"}
	if b.RoleRef.Name != security.APIExtensionsClusterRole || len(b.Subjects) != 1 || b.Subjects[0] != subject {
		t.Fatalf("binding = %+v", b)
	}
	if cond := apiExtensionsCondition(comp); cond == nil || cond.Status != metav1.ConditionTrue {
		t.Fatalf("condition = %+v, want True", cond)
	}

	// The cluster roles' own bookkeeping does not take the binding for one
	// of its own and remove it.
	r.ClusterRoles = testClusterRoles
	if err := r.ensureClusterRoles(context.Background(), comp, profile); err != nil {
		t.Fatal(err)
	}
	if err := r.List(context.Background(), bindings); err != nil || len(bindings.Items) != 1 {
		t.Fatalf("the cluster roles removed the extension's binding: %d, %v", len(bindings.Items), err)
	}
}

// What is not installed, and why: not granted, granted and expired, a group
// that is somebody else's, a type already there, a definition that is not
// one the platform installs. Only a granted one that could not be installed
// holds the install.
func TestAnAPIExtensionIsNotInstalledWhenItMayNotBe(t *testing.T) {
	expired := metav1.NewTime(time.Now().Add(-time.Hour))
	cases := map[string]struct {
		change func(*apiExtensionCase)
		reason string
		holds  bool
	}{
		"not granted": {func(c *apiExtensionCase) { c.grants = nil }, reasonAPIExtensionNotGranted, false},
		"granted, expired": {func(c *apiExtensionCase) { c.grants[0].ExpiresAt = &expired },
			reasonAPIExtensionNotGranted, false},
		"granted, and the definitions changed since": {func(c *apiExtensionCase) {
			c.grants[0].Digest = "sha256:" + strings.Repeat("0", 64)
		}, reasonAPIExtensionNotGranted, false},
		"another's type in the group": {func(c *apiExtensionCase) {
			c.served = []metav1.APIResource{{Name: "others"}, {Name: "others/status"}}
		}, reasonAPIExtensionGroupTaken, true},
		"the type is there, nobody's": {func(c *apiExtensionCase) {
			c.existing = []client.Object{&apiextensionsv1.CustomResourceDefinition{
				ObjectMeta: metav1.ObjectMeta{Name: "schedules." + extensionGroup},
			}}
		}, reasonAPIExtensionTypeTaken, true},
		"a definition in another group": {func(c *apiExtensionCase) {
			c.definitions = []runtime.RawExtension{extensionDefinition(t, "other.example.org", "schedules")}
		}, reasonAPIExtensionInvalid, true},
		"a definition in the platform's group": {func(c *apiExtensionCase) {
			c.definitions = []runtime.RawExtension{extensionDefinition(t, "gentianos.io", "schedules")}
		}, reasonAPIExtensionInvalid, true},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			c := apiExtensionGranted(t)
			tc.change(&c)
			r, comp, profile := c.build(t)
			hold, err := r.ensureAPIExtensions(context.Background(), comp, profile)
			if err != nil {
				t.Fatal(err)
			}
			if (hold != "") != tc.holds {
				t.Errorf("holds the install = %q, want %v", hold, tc.holds)
			}
			if crd := installedExtension(t, r, "schedules."+extensionGroup); crd != nil && crd.Labels[security.APIExtensionLabel] == "backup" {
				t.Error("installed anyway")
			}
			bindings := &rbacv1.ClusterRoleBindingList{}
			if err := r.List(context.Background(), bindings); err != nil || len(bindings.Items) != 0 {
				t.Errorf("bound anyway: %d, %v", len(bindings.Items), err)
			}
			if cond := apiExtensionsCondition(comp); cond == nil || cond.Reason != tc.reason {
				t.Errorf("condition = %+v, want reason %s", cond, tc.reason)
			}
		})
	}
}

// A grant that runs out unbinds the role; the types stay, with every object
// of them, since removing a type deletes its objects.
func TestALapsedGrantUnbindsAndLeavesTheTypes(t *testing.T) {
	r, comp, profile := apiExtensionGranted(t).build(t)
	if _, err := r.ensureAPIExtensions(context.Background(), comp, profile); err != nil {
		t.Fatal(err)
	}
	expired := metav1.NewTime(time.Now().Add(-time.Minute))
	comp.Spec.Privileges[0].ExpiresAt = &expired
	if _, err := r.ensureAPIExtensions(context.Background(), comp, profile); err != nil {
		t.Fatal(err)
	}
	bindings := &rbacv1.ClusterRoleBindingList{}
	if err := r.List(context.Background(), bindings); err != nil || len(bindings.Items) != 0 {
		t.Fatalf("still bound after the grant ran out: %d, %v", len(bindings.Items), err)
	}
	if installedExtension(t, r, "schedules."+extensionGroup) == nil {
		t.Fatal("the type was removed with the grant")
	}
}
