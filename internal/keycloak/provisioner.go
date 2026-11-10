/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package keycloak

import (
	batchv1 "k8s.io/api/batch/v1"
)

// ProvisionerSecret holds the credential of the Jobs that configure Keycloak
// (url, client_id, client_secret): the master-realm service account
// keycloak-provisioner, which the installer makes beside the operator's own.
// External Secrets projects it beside Keycloak, where the Jobs run.
const ProvisionerSecret = "keycloak-provisioner"

// ProvisionerAccount is the Kubernetes service account those Jobs run as,
// instead of the namespace's default one. It mounts no token: the Jobs talk
// to Keycloak only.
const ProvisionerAccount = "keycloak-provisioner"

// AsProvisioner runs a Job's pod as ProvisionerAccount.
func AsProvisioner(job *batchv1.Job) *batchv1.Job {
	no := false
	job.Spec.Template.Spec.ServiceAccountName = ProvisionerAccount
	job.Spec.Template.Spec.AutomountServiceAccountToken = &no
	return job
}
