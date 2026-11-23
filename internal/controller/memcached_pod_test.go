/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package controller

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// Tenant namespaces admit no pod whose containers may escalate privileges or
// that runs without a seccomp profile (Kyverno's
// gentian-disallow-privilege-escalation and gentian-require-seccomp). The
// memcached Deployment had neither, so its ReplicaSet was refused and the
// tenant's CacheReady never came (#1294).
func TestMemcachedPodPassesTheTenantPodPolicies(t *testing.T) {
	spec := makeMemcachedDeployment(&gentianov1alpha1.Tenant{ObjectMeta: metav1.ObjectMeta{Name: "acme"}}).Spec.Template.Spec
	podSeccomp := spec.SecurityContext != nil && spec.SecurityContext.SeccompProfile != nil &&
		spec.SecurityContext.SeccompProfile.Type == corev1.SeccompProfileTypeRuntimeDefault
	for _, c := range spec.Containers {
		sc := c.SecurityContext
		if sc == nil || sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation {
			t.Errorf("container %s does not set allowPrivilegeEscalation: false", c.Name)
		}
		if !podSeccomp && (sc == nil || sc.SeccompProfile == nil) {
			t.Errorf("container %s runs with no seccomp profile, on the pod or itself", c.Name)
		}
	}
	if spec.SecurityContext == nil || spec.SecurityContext.RunAsNonRoot == nil || !*spec.SecurityContext.RunAsNonRoot {
		t.Error("the memcached pod may run as root")
	}
}
