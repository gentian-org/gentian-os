/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package apprender

import (
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// A chart the platform places is told what it was told before its delivery
// was the same as an installed app's: its profile's values and the
// operator's, and none of a catalog chart's conventions, which a chart held
// to a schema refuses. It has no entitlement group either.
func TestAPlacedChartKeepsItsOwnValues(t *testing.T) {
	in := &Input{
		App: "admin-console", Namespace: "tenant-acme", Domain: "acme.example",
		ReleaseObject: "tenant-acme-admin-console", HelmRelease: "tenant-acme-admin-console",
		KeepVolumes: "admin-console-keep-volumes", Composite: "tenant-acme-admin-console",
		ProfileName: "admin-console", Placed: true,
		Profile: &gentianov1alpha1.ComponentProfileSpec{
			Package: gentianov1alpha1.PackageSpec{
				Chart:       &gentianov1alpha1.ChartRef{Repository: "oci://example.invalid/charts", Name: "admin-console", Version: "1.0.0"},
				ExtraValues: &runtime.RawExtension{Raw: []byte(`{"ui":{"title":"Admin"}}`)},
			},
		},
		ExtraValues:   map[string]interface{}{"director": map[string]interface{}{"url": "http://director"}},
		ClusterConfig: map[string]string{"certificates.trustAnchorSecret": "internal-ca"},
	}
	out, err := Render(in)
	if err != nil {
		t.Fatal(err)
	}
	var release *unstructured.Unstructured
	for _, obj := range out.Objects {
		switch obj.GetKind() {
		case "Release":
			release = obj
		case "Group":
			t.Fatalf("a placed component was given an entitlement group: %v", obj.GetName())
		}
	}
	if release == nil {
		t.Fatal("no release")
	}
	values, _, _ := unstructured.NestedMap(release.Object, "spec", "forProvider", "values")
	want := map[string]interface{}{
		"ui":       map[string]interface{}{"title": "Admin"},
		"director": map[string]interface{}{"url": "http://director"},
	}
	if len(values) != len(want) || values["ui"] == nil || values["director"] == nil {
		t.Fatalf("values = %v, want only the profile's and the operator's %v", values, want)
	}
}

// The API alias is the chart's own only when it is exactly
// <fullnameOverride>-api. An app named past its override starts with it too,
// and its alias is made, not merely observed: nobody else makes it.
func TestAnAppNamedPastItsOverrideGetsItsAPIAlias(t *testing.T) {
	in := &Input{
		App: "fellwort-ee", Namespace: "tenant-acme", Domain: "acme.example",
		ReleaseObject: "tenant-acme-fellwort-ee", HelmRelease: "tenant-acme-fellwort-ee",
		KeepVolumes: "fellwort-ee-keep-volumes", Composite: "tenant-acme-fellwort-ee", ProfileName: "fellwort-ee",
		Profile: &gentianov1alpha1.ComponentProfileSpec{
			Package: gentianov1alpha1.PackageSpec{
				Chart:       &gentianov1alpha1.ChartRef{Repository: "oci://example.invalid/charts", Name: "fellwort", Version: "1.0.0"},
				ExtraValues: &runtime.RawExtension{Raw: []byte(`{"fullnameOverride":"fellwort"}`)},
			},
			Expose: []gentianov1alpha1.ExposureSpec{{Name: "web", Surface: "gateway",
				Backend: gentianov1alpha1.BackendRef{Service: "fellwort-web", Port: 80}}},
		},
	}
	out, err := Render(in)
	if err != nil {
		t.Fatal(err)
	}
	for _, obj := range out.ObserveOnly {
		if obj.GetName() == "fellwort-ee-api" {
			t.Fatal("fellwort-ee-api is only observed, and nothing makes it")
		}
	}
	found := false
	for _, obj := range out.Objects {
		found = found || (obj.GetKind() == "Service" && obj.GetName() == "fellwort-ee-api")
	}
	if !found {
		t.Fatal("fellwort-ee-api is not made")
	}
}
