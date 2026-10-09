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

// A tenant's catalogue states where it is read from once -- an https address
// or a directory of the cluster's deployments repository -- and a directory
// only as the cluster's administrator's. The schema says so, so that a
// manifest written by hand is refused when it is applied and not only left
// unread by the director. Asked as a dry run: nothing is created.
func TestTheTenantSchemaAdmitsACatalogueAtAnAddressOrInADirectory(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		source   gentianov1alpha1.TenantCatalogueSource
		admitted bool
	}{
		"an address":                    {gentianov1alpha1.TenantCatalogueSource{Name: "a", URL: "https://catalogue.example.com"}, true},
		"an address the tenant added":   {gentianov1alpha1.TenantCatalogueSource{Name: "a", URL: "https://catalogue.example.com", AddedBy: "tenant"}, true},
		"a directory":                   {gentianov1alpha1.TenantCatalogueSource{Name: "a", Path: "catalogues/acme"}, true},
		"a directory, by the cluster":   {gentianov1alpha1.TenantCatalogueSource{Name: "a", Path: "catalogue", AddedBy: "cluster"}, true},
		"a directory the tenant added":  {gentianov1alpha1.TenantCatalogueSource{Name: "a", Path: "catalogue", AddedBy: "tenant"}, false},
		"both":                          {gentianov1alpha1.TenantCatalogueSource{Name: "a", URL: "https://catalogue.example.com", Path: "catalogue"}, false},
		"neither":                       {gentianov1alpha1.TenantCatalogueSource{Name: "a"}, false},
		"an address that is not https":  {gentianov1alpha1.TenantCatalogueSource{Name: "a", URL: "http://catalogue.example.com"}, false},
		"a way up":                      {gentianov1alpha1.TenantCatalogueSource{Name: "a", Path: "../catalogue"}, false},
		"a way up, further in":          {gentianov1alpha1.TenantCatalogueSource{Name: "a", Path: "catalogue/../.."}, false},
		"an absolute path":              {gentianov1alpha1.TenantCatalogueSource{Name: "a", Path: "/etc"}, false},
		"a hidden directory":            {gentianov1alpha1.TenantCatalogueSource{Name: "a", Path: ".git"}, false},
		"another repository's address":  {gentianov1alpha1.TenantCatalogueSource{Name: "a", Path: "git@example.com:other/repository.git"}, false},
		"a directory with a doubled /":  {gentianov1alpha1.TenantCatalogueSource{Name: "a", Path: "catalogue//profiles"}, false},
		"a directory with a trailing /": {gentianov1alpha1.TenantCatalogueSource{Name: "a", Path: "catalogue/"}, false},
	} {
		tenant := &gentianov1alpha1.Tenant{
			ObjectMeta: metav1.ObjectMeta{Name: "catalogue-schema"},
			Spec: gentianov1alpha1.TenantSpec{
				DisplayName: "Catalogue schema",
				Catalogue:   &gentianov1alpha1.TenantCatalogue{Sources: []gentianov1alpha1.TenantCatalogueSource{c.source}},
			},
		}
		err := testClient.Create(context.Background(), tenant, client.DryRunAll)
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
