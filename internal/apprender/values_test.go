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

	"github.com/google/go-cmp/cmp"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// Branches of the values render no Composition fixture reaches.

func valuesTestInput(spec *gentianov1alpha1.ComponentProfileSpec) *Input {
	if spec.Package.Chart == nil {
		spec.Package.Chart = &gentianov1alpha1.ChartRef{Repository: "oci://example.invalid/x", Name: "x", Version: "1"}
	}
	return &Input{
		App:           "demo-app",
		Namespace:     "tenant-demo",
		ReleaseObject: "demo-app",
		HelmRelease:   "demo-app",
		KeepVolumes:   "demo-app-keep-volumes",
		ProfileName:   "demo-app",
		Profile:       spec,
	}
}

func valuesTestRender(t *testing.T, in *Input) (values map[string]interface{}, release, es *unstructured.Unstructured) {
	t.Helper()
	out := &Rendered{}
	if err := renderValues(in, out); err != nil {
		t.Fatal(err)
	}
	for _, obj := range out.Objects {
		switch obj.GetKind() {
		case "Release":
			release = obj
			values, _, _ = unstructured.NestedMap(obj.Object, "spec", "forProvider", "values")
		case "ExternalSecret":
			es = obj
		}
	}
	return values, release, es
}

func TestValuesMergeOverwriteIsSprigs(t *testing.T) {
	dst := map[string]interface{}{
		"keep":    "dst",
		"flag":    true,
		"list":    []interface{}{"a", "b"},
		"nested":  map[string]interface{}{"a": "1", "b": "2"},
		"empty":   map[string]interface{}{},
		"scalar":  "s",
		"dropped": "x",
	}
	valuesMergeOverwrite(dst, map[string]interface{}{
		"flag":    false,
		"list":    []interface{}{"c"},
		"nested":  map[string]interface{}{"b": "3", "c": "4"},
		"empty":   map[string]interface{}{"x": "y"},
		"scalar":  map[string]interface{}{"now": "a map"},
		"dropped": nil,
	})
	want := map[string]interface{}{
		"keep":    "dst",
		"flag":    false,
		"list":    []interface{}{"c"},
		"nested":  map[string]interface{}{"a": "1", "b": "3", "c": "4"},
		"empty":   map[string]interface{}{"x": "y"},
		"scalar":  map[string]interface{}{"now": "a map"},
		"dropped": nil,
	}
	if diff := cmp.Diff(want, dst); diff != "" {
		t.Errorf("merged (-want +got):\n%s", diff)
	}
}

func TestValuesTenantOverridesReplicasAndStandardPullSecrets(t *testing.T) {
	in := valuesTestInput(&gentianov1alpha1.ComponentProfileSpec{
		Package: gentianov1alpha1.PackageSpec{
			ExtraValues: &runtime.RawExtension{Raw: []byte(`{"host":"app.${TENANT_DOMAIN}","node":"${NODE_IP}","enabled":true,"imagePullSecrets":[{"name":"own"}]}`)},
		},
	})
	replicas := int64(2)
	in.Replicas = &replicas
	in.ExtraValues = map[string]interface{}{"enabled": false}
	in.ClusterConfig = map[string]string{"node.ip": "10.0.0.1"}
	in.PullSecrets.Images = []string{"own", "tenant-pull"}
	in.ProfileAnnotations = map[string]string{"gentianos.io/standard-image-pull-secrets": "true"}

	values, _, _ := valuesTestRender(t, in)
	pull := []interface{}{map[string]interface{}{"name": "own"}, map[string]interface{}{"name": "tenant-pull"}}
	want := map[string]interface{}{
		"host":             "app.demo.example.org",
		"node":             "10.0.0.1",
		"enabled":          false,
		"replicaCount":     float64(2),
		"imagePullSecrets": pull,
		"global":           map[string]interface{}{"imagePullSecrets": pull},
		"podLabels":        map[string]interface{}{"gentianos.io/app": "demo-app", "gentianos.io/tenant": "demo"},
	}
	if diff := cmp.Diff(want, values); diff != "" {
		t.Errorf("values (-want +got):\n%s", diff)
	}
	if in.ExtraValues["enabled"] != false || len(in.ExtraValues) != 1 {
		t.Errorf("the input's values were changed: %v", in.ExtraValues)
	}
}

func TestValuesDatabaseReadKeysAndIntegrations(t *testing.T) {
	in := valuesTestInput(&gentianov1alpha1.ComponentProfileSpec{
		Package: gentianov1alpha1.PackageSpec{
			ValueMapping: &gentianov1alpha1.ValueMapping{
				Database: &gentianov1alpha1.DatabaseValueMapping{NameKey: "db.name", ReadNameKey: "db.readName"},
				Integrations: map[string]gentianov1alpha1.IntegrationValueMapping{
					"zeta":  {EndpointKey: "zeta.url"},
					"alpha": {UserKey: "alpha.user"},
				},
			},
		},
	})
	_, release, es := valuesTestRender(t, in)
	var setNames, setKeys []string
	sets, _, _ := unstructured.NestedSlice(release.Object, "spec", "forProvider", "set")
	for _, s := range sets {
		m := s.(map[string]interface{})
		setNames = append(setNames, m["name"].(string))
		key, _, _ := unstructured.NestedString(m, "valueFrom", "secretKeyRef", "key")
		setKeys = append(setKeys, key)
	}
	if diff := cmp.Diff([]string{"db.name", "db.readName", "alpha.user", "zeta.url"}, setNames); diff != "" {
		t.Errorf("set names (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff([]string{"db-name", "db-name", "integration-alpha-user", "integration-zeta-endpoint"}, setKeys); diff != "" {
		t.Errorf("set keys (-want +got):\n%s", diff)
	}
	data, _, _ := unstructured.NestedSlice(es.Object, "spec", "data")
	var paths []string
	for _, d := range data {
		key, _, _ := unstructured.NestedString(d.(map[string]interface{}), "remoteRef", "key")
		paths = append(paths, key)
	}
	want := []string{
		"gentian-os/tenants/demo/apps/demo-app/database",
		"gentian-os/tenants/demo/contracts/alpha",
		"gentian-os/tenants/demo/contracts/zeta",
	}
	if diff := cmp.Diff(want, paths); diff != "" {
		t.Errorf("ExternalSecret paths (-want +got):\n%s", diff)
	}
}

func TestValuesTrustAnchorJavaOpts(t *testing.T) {
	in := valuesTestInput(&gentianov1alpha1.ComponentProfileSpec{
		Package: gentianov1alpha1.PackageSpec{
			ExtraValues: &runtime.RawExtension{Raw: []byte(`{"javaOpts":["-Xmx1g","-Djavax.net.ssl.trustStoreType=JKS"]}`)},
		},
	})
	in.ClusterConfig = map[string]string{"certificates.trustAnchorSecret": "cert-manager/gentian-root-ca-tls"}
	values, _, _ := valuesTestRender(t, in)
	want := []interface{}{
		"-Xmx1g",
		"-Djavax.net.ssl.trustStoreType=JKS",
		"-Djavax.net.ssl.trustStore=/opt/gentian-trust-anchor/truststore.jks",
		"-Djavax.net.ssl.trustStorePassword=changeit",
	}
	if diff := cmp.Diff(want, values["javaOpts"]); diff != "" {
		t.Errorf("javaOpts (-want +got):\n%s", diff)
	}
}
