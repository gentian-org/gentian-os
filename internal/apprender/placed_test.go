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
		App: "admin-console", Namespace: "tenant-acme", TenantName: "acme", Domain: "acme.example",
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
		App: "fellwort-ee", Namespace: "tenant-acme", TenantName: "acme", Domain: "acme.example",
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

// A component the platform places that provides a contract is handed the
// contract's credential like any other provider: read from the tenant's own
// store at the contract's path and set at the key its profile names. Nothing
// else of a placed component's is read from the store.
func TestAPlacedProviderIsHandedItsContractCredential(t *testing.T) {
	in := &Input{
		App: "desktop", Namespace: "tenant-acme", TenantName: "acme", Domain: "acme.example",
		ReleaseObject: "tenant-acme-desktop", HelmRelease: "tenant-acme-desktop",
		KeepVolumes: "desktop-keep-volumes", Composite: "tenant-acme-desktop",
		ProfileName: "desktop", Placed: true,
		Profile: &gentianov1alpha1.ComponentProfileSpec{
			Package: gentianov1alpha1.PackageSpec{
				Chart: &gentianov1alpha1.ChartRef{Repository: "oci://example.invalid/charts", Name: "desktop", Version: "1.0.0"},
				ValueMapping: &gentianov1alpha1.ValueMapping{
					Database: &gentianov1alpha1.DatabaseValueMapping{SecretNameKey: "existingSecret.name"},
					Integrations: map[string]gentianov1alpha1.IntegrationValueMapping{
						"notices": {PasswordKey: "notices.key"},
					},
				},
			},
			Provides: []gentianov1alpha1.ContractRef{{Name: "notices", Protocol: "http-json",
				Endpoint: &gentianov1alpha1.ContractEndpoint{Service: "desktop-api", Port: 8000, Path: "/contracts/v1/notices"}}},
		},
	}
	out, err := Render(in)
	if err != nil {
		t.Fatal(err)
	}
	var release, secret *unstructured.Unstructured
	for _, obj := range out.Objects {
		switch obj.GetKind() {
		case "Release":
			release = obj
		case "ExternalSecret":
			secret = obj
		}
	}
	if release == nil || secret == nil {
		t.Fatalf("release = %v, external secret = %v: want both", release != nil, secret != nil)
	}
	if got := secret.GetName(); got != "desktop-sensitive-values" {
		t.Fatalf("external secret = %q", got)
	}
	data, _, _ := unstructured.NestedSlice(secret.Object, "spec", "data")
	if len(data) != 1 {
		t.Fatalf("external secret reads %v, want the contract's credential and nothing else", data)
	}
	entry := data[0].(map[string]interface{})
	ref, _ := entry["remoteRef"].(map[string]interface{})
	if entry["secretKey"] != "integration-notices-password" ||
		ref["key"] != "gentian-os/tenants/acme/contracts/notices" || ref["property"] != "password" {
		t.Fatalf("external secret entry = %v", entry)
	}
	set, _, _ := unstructured.NestedSlice(release.Object, "spec", "forProvider", "set")
	if len(set) != 1 {
		t.Fatalf("release sets %v, want the contract's credential and nothing else", set)
	}
	item := set[0].(map[string]interface{})
	key, _, _ := unstructured.NestedString(item, "valueFrom", "secretKeyRef", "key")
	name, _, _ := unstructured.NestedString(item, "valueFrom", "secretKeyRef", "name")
	if item["name"] != "notices.key" || key != "integration-notices-password" || name != "desktop-sensitive-values" {
		t.Fatalf("release set entry = %v", item)
	}
}
