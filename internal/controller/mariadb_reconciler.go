/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package controller

import (
	"context"
	"fmt"
	"time"

	"github.com/gentian-org/gentian-os/internal/kernel"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/backup"
)

const (
	conditionMariaDBReady = "MariaDBReady"
	mariadbRequeueAfter   = 2 * time.Second
)

// ensureMariaDB provisions per-app-per-tenant MariaDB databases using idempotent
// SQL Jobs. It looks up which apps require MariaDB via AppProfile ServiceRequirements,
// then runs a setup Job for each (CREATE DATABASE IF NOT EXISTS + CREATE USER +
// the grants of backup.MariaDBGrants). Completion of all setup Jobs sets MariaDBReady=True.
func (r *TenantReconciler) ensureMariaDB(ctx context.Context, tenant *gentianov1alpha1.Tenant) (ctrl.Result, error) {
	return r.reconcileJobWaitRequirement(ctx, tenant, jobWaitRequirement{
		conditionType: conditionMariaDBReady,
		emptyReason:   "NoMariaDBRequired",
		readyReason:   "Provisioned",
		jobNameForApp: mariadbSetupJobName,
	}, r.collectMariaDBApps, r.ensureMariaDBSetupJob)
}

// collectMariaDBApps returns AppProfiles that require MariaDB provisioning or cleanup.
func (r *TenantReconciler) collectMariaDBApps(ctx context.Context, tenant *gentianov1alpha1.Tenant, mode AppCollectionMode) ([]string, error) {
	return r.collectKernelApps(ctx, tenant, mode, matchMariaDBProfile, func(tenantName string) string {
		return mariadbSetupJobName(tenantName, "")
	}, func(p backup.Provisioned) bool { return p.DatabaseEngine == gentianov1alpha1.DatabaseEngineMariaDB })
}

// ensureMariaDBSetupJob waits for the Crossplane-owned MariaDB setup Job.
func (r *TenantReconciler) ensureMariaDBSetupJob(ctx context.Context, tenant *gentianov1alpha1.Tenant, appName string) (bool, error) {
	return r.waitForProvisioningJob(ctx, tenant.Name, mariadbSetupJobName(tenant.Name, appName))
}

// deleteMariaDB handles MariaDB cleanup on tenant deletion.
// DeletionPolicy=Delete: creates Jobs that drop databases and users for every
// MariaDB app ever provisioned for the tenant.
// DeletionPolicy=Retain: no-op — data is preserved.
func (r *TenantReconciler) deleteMariaDB(ctx context.Context, tenant *gentianov1alpha1.Tenant) error {
	if tenant.Spec.DeletionPolicy != gentianov1alpha1.DeletionPolicyDelete {
		return nil
	}
	apps, err := r.collectMariaDBApps(ctx, tenant, CollectForDelete)
	if err != nil {
		return err
	}
	return r.ensureDeleteJobs(ctx, mariadbNamespace, tenant, apps, mariadbDeleteJobName, makeMariaDBDeleteJob)
}

// --- Job constructors --------------------------------------------------------

// makeMariaDBSetupJob builds the idempotent database + user provisioning Job.
// Credentials are injected from the mariadb-admin Secret in the kernel namespace.
// The script, with the grants it issues, is the inventory's
// (backup.MariaDBSetupScript): what the user may touch and what an export, a
// restore and a purge take to be the app's are one rule there.
func makeMariaDBSetupJob(tenant *gentianov1alpha1.Tenant, appName, dbPassword string, allowDynamic bool) *batchv1.Job {
	dbName := databaseName(tenant, appName)
	dbUser := mariadbUserName(tenant.Name, appName)
	c := mariadbContainer("provision-db", backup.MariaDBSetupScript(dbName, dbUser, allowDynamic))
	if dbPassword != "" {
		c.Env = append(c.Env, corev1.EnvVar{Name: "DB_PASS", Value: dbPassword})
	}
	return newKernelProvisioningJob(mariadbSetupJobName(tenant.Name, appName), mariadbNamespace, tenant, appName, c)
}

// makeMariaDBDeleteJob builds the DROP DATABASE / DROP USER cleanup Job: the
// Job the purge of one app runs too.
func makeMariaDBDeleteJob(tenant *gentianov1alpha1.Tenant, appName string) *batchv1.Job {
	return backup.MariaDBDestroyJob(tenant, appName, backup.DestroyInTheBackground)
}

// mariadbContainer returns a Container that runs a mariadb CLI script as the
// server's admin: credentials come from the mariadb-admin Secret, by the
// inventory's one block.
func mariadbContainer(name, script string) corev1.Container {
	return corev1.Container{
		Name:            name,
		Image:           kernel.MariaDBProvisionerImage(),
		Command:         []string{"/bin/bash", "-c", script},
		SecurityContext: provisioningSecurityContext(),
		Env:             backup.MariaDBAdminEnv(),
	}
}

// --- Name helpers ------------------------------------------------------------

// mariadbUserName returns the MariaDB username for a tenant + app.
func mariadbUserName(tenantName, appName string) string {
	return backup.MariaDBUser(tenantName, appName)
}

func mariadbSetupJobName(tenantName, appName string) string {
	return fmt.Sprintf("mariadb-setup-%s-%s", tenantName, appName)
}

func mariadbDeleteJobName(tenantName, appName string) string {
	return backup.MariaDBDestroyJobName(tenantName, appName)
}
