/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package backup

import (
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// The names data is held under are read out of what is durable -- the
// record, the database records, the claims -- by one rule, which the read
// of what uninstalled apps hold and an export both go by.
func TestTheNamesDataIsHeldUnder(t *testing.T) {
	tenant := &gentianov1alpha1.Tenant{ObjectMeta: metav1.ObjectMeta{Name: "demo"}}
	withExtension := &gentianov1alpha1.ComponentProfile{
		ObjectMeta: metav1.ObjectMeta{Name: "docs"},
		Spec:       gentianov1alpha1.ComponentProfileSpec{Extensions: []gentianov1alpha1.AppSidecarSpec{{Name: "mcp"}}},
	}
	profiles := func(name string) (*gentianov1alpha1.ComponentProfile, error) {
		if name == "docs" {
			return withExtension, nil
		}
		return nil, nil
	}
	claim := func(name string, labels, annotations map[string]string) corev1.PersistentVolumeClaim {
		return corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels, Annotations: annotations}}
	}
	held := Held{
		Provisioned: map[string]Provisioned{"crm": {Bucket: "demo-crm"}, "notes": {DatabaseEngine: gentianov1alpha1.DatabaseEngineMariaDB}},
		Databases:   map[string]bool{"files": true},
		Claims: []corev1.PersistentVolumeClaim{
			claim("a", map[string]string{"gentianos.io/app": "labelled"}, nil),
			claim("b", nil, map[string]string{"meta.helm.sh/release-name": "wiki-release"}),
			// An extension's release is its app's; a name that only looks
			// like one stands for itself.
			claim("c", nil, map[string]string{"meta.helm.sh/release-name": "docs-mcp-release"}),
			claim("d", map[string]string{"app.kubernetes.io/instance": "docs-other-release"}, nil),
			claim("e", map[string]string{"app.kubernetes.io/instance": "tenant-demo-direct"}, nil),
			claim("f", nil, nil),
		},
	}
	got := DataHolders(tenant, held, profiles)
	want := []string{"crm", "direct", "docs", "docs-other", "files", "labelled", "notes", "wiki"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("names = %v\nwant %v", got, want)
	}

	// What is held for an app is what was made and not destroyed: the
	// record, and the database record the cluster keeps -- not what a
	// profile says an install would make.
	if s := HeldStores("notes", held); s != (Stores{Database: gentianov1alpha1.DatabaseEngineMariaDB}) {
		t.Errorf("notes: %+v", s)
	}
	if s := HeldStores("files", held); s != (Stores{Database: gentianov1alpha1.DatabaseEnginePostgreSQL}) {
		t.Errorf("files: %+v", s)
	}
	if s := HeldStores("crm", held); s != (Stores{S3: true}) {
		t.Errorf("crm: %+v", s)
	}
	if s := HeldStores("purged", held); s != (Stores{}) {
		t.Errorf("an app nothing is held for: %+v", s)
	}
}
