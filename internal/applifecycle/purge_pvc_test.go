/*
Copyright 2026 Gentian Organization.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package applifecycle

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
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
