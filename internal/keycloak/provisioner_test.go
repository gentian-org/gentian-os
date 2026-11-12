/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package keycloak

import (
	"testing"

	batchv1 "k8s.io/api/batch/v1"
)

// A Keycloak Job runs as its own account, never the namespace's default,
// and mounts no Kubernetes token: it talks to Keycloak only.
func TestAsProvisioner(t *testing.T) {
	job := AsProvisioner(&batchv1.Job{})
	spec := job.Spec.Template.Spec
	if spec.ServiceAccountName != ProvisionerAccount {
		t.Errorf("service account = %q, want %q", spec.ServiceAccountName, ProvisionerAccount)
	}
	if spec.AutomountServiceAccountToken == nil || *spec.AutomountServiceAccountToken {
		t.Error("the Job mounts a Kubernetes token it does not need")
	}
}
