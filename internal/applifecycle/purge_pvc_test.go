/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package applifecycle

import (
	"testing"

	corev1 "k8s.io/api/core/v1"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func pvc(name string, labels map[string]string) corev1.PersistentVolumeClaim {
	return corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels}}
}

func TestPVCBelongsToApp(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		claim  corev1.PersistentVolumeClaim
		app    string
		family string
		want   bool
	}{
		{"gentian app label", pvc("x", map[string]string{"gentianos.io/app": "odoo-base-ce"}), "odoo-base-ce", "odoo", true},
		{"helm instance prefix", pvc("x", map[string]string{"app.kubernetes.io/instance": "odoo-base-ce-abc-release"}), "odoo-base-ce", "odoo", true},
		{"family name label", pvc("x", map[string]string{"app.kubernetes.io/name": "odoo"}), "odoo-base-ce", "odoo", true},
		{"name contains app", pvc("demo-odoo-base-ce-git-modules-pvc", nil), "odoo-base-ce", "odoo", true},
		{"name contains family", pvc("odoo-data", nil), "odoo-base-ce", "odoo", true},

		// The dangerous direction: another app's volume must never be swept up.
		{"other app by label", pvc("x", map[string]string{"gentianos.io/app": "nextcloud-base-ce"}), "odoo-base-ce", "odoo", false},
		{"other app by name", pvc("nextcloud-nextcloud", nil), "odoo-base-ce", "odoo", false},
		{"unrelated volume", pvc("open-webui", nil), "odoo-base-ce", "odoo", false},
		{"empty family does not match everything", pvc("open-webui", nil), "odoo-base-ce", "", false},
	}
	for _, c := range cases {
		if got := pvcBelongsToApp(c.claim, c.app, c.family); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

func TestPodReferencesAny(t *testing.T) {
	t.Parallel()
	withClaims := func(names ...string) corev1.Pod {
		var vols []corev1.Volume
		for _, n := range names {
			vols = append(vols, corev1.Volume{VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: n},
			}})
		}
		vols = append(vols, corev1.Volume{VolumeSource: corev1.VolumeSource{
			EmptyDir: &corev1.EmptyDirVolumeSource{},
		}})
		return corev1.Pod{Spec: corev1.PodSpec{Volumes: vols}}
	}
	doomed := map[string]struct{}{"odoo-data": {}}

	// The finished install Job is what actually wedged this: Succeeded, but still
	// holding the claim, so pvc-protection never cleared.
	if !podReferencesAny(withClaims("odoo-data", "other"), doomed) {
		t.Error("a pod holding the claim must be detected")
	}
	if podReferencesAny(withClaims("nextcloud-nextcloud"), doomed) {
		t.Error("an unrelated pod must not be deleted")
	}
	if podReferencesAny(corev1.Pod{}, doomed) {
		t.Error("a pod with no volumes must not match")
	}
}

// A resource Helm owns must only go with its own release. provider-helm
// reconciles release state rather than cluster contents, so anything deleted
// from under a live release stays deleted. Release names are exact -- the
// app's, its declared extensions', or a directly delivered chart's -- so a
// claim that names a release is the app's only when the release is.
func TestAClaimIsTheAppsOnlyWhenItsReleaseIs(t *testing.T) {
	t.Parallel()
	tenant := &gentianov1alpha1.Tenant{ObjectMeta: metav1.ObjectMeta{Name: "demo"}}
	tenant.Spec.Apps = []gentianov1alpha1.TenantApp{{Profile: "nextcloud-base-ce-talk"}}
	profile := &gentianov1alpha1.ComponentProfile{
		ObjectMeta: metav1.ObjectMeta{Name: "nextcloud-base-ce"},
		Spec: gentianov1alpha1.ComponentProfileSpec{
			Package:    gentianov1alpha1.PackageSpec{Chart: &gentianov1alpha1.ChartRef{Name: "nextcloud"}},
			Extensions: []gentianov1alpha1.AppSidecarSpec{{Name: "mcp"}, {Name: "talk"}},
		},
	}
	helm := func(name, release string) corev1.PersistentVolumeClaim {
		return corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{
			Name: name, Annotations: map[string]string{"meta.helm.sh/release-name": release}}}
	}
	statefulSet := func(name, release string) corev1.PersistentVolumeClaim {
		return corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{
			Name: name, Labels: map[string]string{"app.kubernetes.io/instance": release}}}
	}
	cases := []struct {
		name    string
		claim   corev1.PersistentVolumeClaim
		profile *gentianov1alpha1.ComponentProfile
		want    bool
	}{
		{"its own release", helm("nextcloud-base-ce-release-data", "nextcloud-base-ce-release"), profile, true},
		{"its declared extension's release", helm("nextcloud-base-ce-mcp-release-cache", "nextcloud-base-ce-mcp-release"), profile, true},
		{"its chart delivered directly", helm("tenant-demo-nextcloud-base-ce-data", "tenant-demo-nextcloud-base-ce"), profile, true},
		{"a claim its StatefulSet made", statefulSet("data-nextcloud-base-ce-release-0", "nextcloud-base-ce-release"), profile, true},
		// Not Helm's at all: matched by name alone, or a claim the operator
		// made directly would survive every purge.
		{"no release on record", pvc("nextcloud-base-ce-uploads", nil), profile, true},

		// The dangerous direction.
		{"a sibling profile sharing the chart", helm("nextcloud-suite-data", "nextcloud-suite-release"), profile, false},
		{"an app whose name begins with this one's", helm("nextcloud-base-ce-pro-release-data", "nextcloud-base-ce-pro-release"), profile, false},
		{"a StatefulSet claim of such an app", statefulSet("data-nextcloud-base-ce-pro-release-0", "nextcloud-base-ce-pro-release"), profile, false},
		{"an installed app named like an extension's key", helm("nextcloud-base-ce-talk-release-data", "nextcloud-base-ce-talk-release"), profile, false},
		{"the same app in another tenant's naming", helm("tenant-other-nextcloud-base-ce-data", "tenant-other-nextcloud-base-ce"), profile, false},

		// With the profile gone the extensions are not known: a release
		// shaped like one counts, but never an installed app's.
		{"profile gone: shaped like an extension's", helm("nextcloud-base-ce-mcp-release-cache", "nextcloud-base-ce-mcp-release"), nil, true},
		{"profile gone: an installed app's", helm("nextcloud-base-ce-talk-release-data", "nextcloud-base-ce-talk-release"), nil, false},
		{"profile gone: a sibling sharing the chart", helm("nextcloud-suite-data", "nextcloud-suite-release"), nil, false},
	}
	for _, c := range cases {
		own, vetoed := appVolumes([]corev1.PersistentVolumeClaim{c.claim}, tenant, "nextcloud-base-ce", c.profile)
		if got := len(own) == 1; got != c.want {
			t.Errorf("%s: the app's = %v, want %v (vetoed %v)", c.name, got, c.want, vetoed)
		}
	}
}
