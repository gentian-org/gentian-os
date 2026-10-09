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
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/gentian-org/gentian-os/api/bundle"
	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/backup"
	"github.com/gentian-org/gentian-os/internal/controller/provisioner"
	"github.com/gentian-org/gentian-os/internal/kernel/secrets"
)

// Putting back the data of an app that is not installed.
//
// The app stays uninstalled: nothing is installed, nothing is paused, and
// when the restore is done the tenant holds the app's data as an uninstall
// would have left it -- the stores on record, the claims carrying the
// release that the next install and a purge find them by -- so the read of
// what uninstalled apps hold shows it and installing the app finds it.
//
// Into a tenant that still holds the data, the units are the ones an
// installed app's restore runs, into the same stores. Into a tenant made new
// for the bundle the stores are not there, and are made first, by the code
// install makes them with: the record of what was provisioned before
// anything else, then the database with its role and its vault record, the
// claims from what the bundle recorded of them; the bucket's unit makes the
// bucket itself. Which of the two it is, the plan decided.

// restoreRetained puts one uninstalled app's data back.
func (r *TenantRestoreReconciler) restoreRetained(
	ctx context.Context,
	restore *gentianov1alpha1.TenantRestore,
	tenant *gentianov1alpha1.Tenant,
	appName string,
	decryption backup.Decryption,
) (ctrl.Result, error) {
	entry := appStatus(&restore.Status.Apps, appName)
	failed := func(message string) (ctrl.Result, error) {
		entry.Phase = gentianov1alpha1.TenantExportPhaseFailed
		entry.Message = message
		return r.fail(ctx, restore, "RestoreFailed",
			fmt.Sprintf("%s (uninstalled, data retained): %s. %s", appName, message, restoreStateText(restore, appName)))
	}
	entry.Phase = gentianov1alpha1.TenantExportPhaseRunning

	ready, waiting, err := r.ensureRetainedStores(ctx, tenant, appName, entry.Artefacts)
	if err != nil {
		return failed(fmt.Sprintf("make the stores its data is put back into: %v", err))
	}
	if !ready {
		entry.Message = "making the stores its data is put back into; waiting on " + waiting
		return ctrl.Result{RequeueAfter: exportRequeueAfter}, r.persist(ctx, restore)
	}

	units, err := r.restoreUnits(ctx, tenant, appName, restore, decryption)
	if err != nil {
		return failed(fmt.Sprintf("enumerate what to restore: %v", err))
	}
	if err := r.stageFor(ctx, restore, units, decryption); err != nil {
		return failed(err.Error())
	}
	var pending []string
	for _, unit := range units {
		done, err := r.ensureRestoreJob(ctx, restore, unit)
		if err != nil {
			return ctrl.Result{}, err
		}
		if !done {
			pending = append(pending, unit.JobName)
		}
	}
	if len(pending) > 0 {
		if entry.Attempts > exportMaxAttempts {
			why := fmt.Sprintf("restore did not succeed after %d attempts (waiting on %s)", entry.Attempts, strings.Join(pending, ", "))
			if entry.LastFailure != "" {
				why += " — " + entry.LastFailure
			}
			return failed(why)
		}
		entry.Message = fmt.Sprintf("restoring retained data; waiting on %s", strings.Join(pending, ", "))
		return ctrl.Result{RequeueAfter: exportRequeueAfter}, r.persist(ctx, restore)
	}
	entry.Phase = gentianov1alpha1.TenantExportPhaseReady
	entry.Message = ""
	if err := r.persist(ctx, restore); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{Requeue: true}, nil
}

// ensureRetainedStores makes whatever the plan's artefacts go into and the
// tenant does not have, and reports whether all of it is there. Where the
// tenant holds the app's stores already -- every restore but one into a
// tenant made new -- it finds them and makes nothing.
//
// The record first: a store is on record before it is made, here as at an
// install, so that one made by a restore that then failed is still found by
// a purge and by the tenant's deletion.
func (r *TenantRestoreReconciler) ensureRetainedStores(
	ctx context.Context,
	tenant *gentianov1alpha1.Tenant,
	appName string,
	artefacts []gentianov1alpha1.BundleArtefact,
) (ready bool, waiting string, err error) {
	var engine gentianov1alpha1.DatabaseEngine
	bucket := false
	var claims []gentianov1alpha1.BundleArtefact
	for _, a := range artefacts {
		switch a.Kind {
		case bundle.ArtefactPostgres, bundle.ArtefactPostgresOwned:
			engine = gentianov1alpha1.DatabaseEnginePostgreSQL
		case bundle.ArtefactMariaDB, bundle.ArtefactMariaDBOwned:
			engine = gentianov1alpha1.DatabaseEngineMariaDB
		case bundle.ArtefactS3:
			bucket = true
		case bundle.ArtefactVolume:
			if a.Claim != nil {
				claims = append(claims, a)
			}
		}
	}
	held, err := r.Reconciler.heldData(ctx, tenant)
	if err != nil {
		return false, "", err
	}
	have := backup.HeldStores(appName, held)
	needDatabase := engine != "" && have.Database == ""
	needBucket := bucket && !have.S3

	if needDatabase || needBucket {
		made := backup.Provisioned{}
		if needDatabase {
			made.DatabaseEngine, made.Database = engine, backup.DatabaseName(tenant, appName)
			made.DatabaseUser = backup.PostgresRole(tenant.Name, appName)
			if engine == gentianov1alpha1.DatabaseEngineMariaDB {
				made.DatabaseUser = backup.MariaDBUser(tenant.Name, appName)
			}
		}
		if needBucket {
			made.Bucket = backup.S3Bucket(tenant, appName)
		}
		if err := r.Tenant.recordProvisioned(ctx, tenant.Name, appName, made); err != nil {
			return false, "", err
		}
	}

	// The database. Whether it is there is asked of what was made, not of
	// the record written above: a PostgreSQL one is there once the cluster
	// keeps its database record, which is made after the role and the
	// database are; a MariaDB one once the setup Job this made has succeeded.
	profile := &gentianov1alpha1.ComponentProfile{}
	if err := r.Get(ctx, types.NamespacedName{Name: appName}, profile); err != nil {
		if !apierrors.IsNotFound(err) {
			return false, "", err
		}
		profile = nil
	}
	switch {
	case engine == gentianov1alpha1.DatabaseEnginePostgreSQL && !held.Databases[appName]:
		if profile == nil {
			return false, "", fmt.Errorf("the app has no ComponentProfile on this cluster, so nothing says how its database is to be made")
		}
		ok, reason, message, err := ensurePostgresStore(ctx, r.Client, r.Tenant.Seeder, tenant, appName,
			schemaPreferenceFor(profile), allowsDynamicDatabaseCreation(profile))
		if err != nil {
			return false, "", err
		}
		if reason == "DatabaseUnavailable" {
			// A desktop waits for a server that is not there yet; a restore
			// does not: what is missing now is not made by waiting.
			return false, "", fmt.Errorf("%s", message)
		}
		if !ok {
			return false, "its PostgreSQL database (" + message + ")", nil
		}
	case engine == gentianov1alpha1.DatabaseEngineMariaDB:
		if needDatabase && profile == nil {
			return false, "", fmt.Errorf("the app has no ComponentProfile on this cluster, so nothing says how its database is to be made")
		}
		ok, err := r.ensureRetainedMariaDB(ctx, tenant, appName, needDatabase, allowsDynamicDatabaseCreation(profile))
		if err != nil || !ok {
			return false, "its MariaDB database", err
		}
	}

	// The claims the bundle recorded and the tenant does not have.
	for _, a := range claims {
		if err := r.ensureRetainedClaim(ctx, tenant, a); err != nil {
			return false, "", err
		}
	}
	return true, "", nil
}

// ensureRetainedMariaDB runs the setup Job install runs for a MariaDB app:
// the database, the user with the password the vault holds, the grants.
// start says to make the Job when there is none; a Job that is there is
// waited for either way, which is how a later pass finds the one an earlier
// pass started.
func (r *TenantRestoreReconciler) ensureRetainedMariaDB(ctx context.Context, tenant *gentianov1alpha1.Tenant, appName string, start, allowDynamic bool) (bool, error) {
	job := &batchv1.Job{}
	err := r.Get(ctx, types.NamespacedName{Name: mariadbSetupJobName(tenant.Name, appName), Namespace: mariadbNamespace}, job)
	if apierrors.IsNotFound(err) && !start {
		return true, nil
	}
	password := ""
	if apierrors.IsNotFound(err) && r.Tenant.Seeder != nil {
		creds, err := r.Tenant.Seeder.SeedMariaDB(ctx, tenant.Name, appName, secrets.DatabaseCreds{
			Host: fmt.Sprintf("%s.%s.svc.cluster.local", "mariadb", mariadbNamespace),
			Port: fmt.Sprint(provisioner.MariaDBPort),
			Name: databaseName(tenant, appName),
			User: mariadbUserName(tenant.Name, appName),
		})
		if err != nil {
			return false, fmt.Errorf("seed mariadb for %s: %w", appName, err)
		}
		password = creds.Password
	}
	switch {
	case apierrors.IsNotFound(err):
		if err := r.Create(ctx, makeMariaDBSetupJob(tenant, appName, password, allowDynamic)); err != nil && !apierrors.IsAlreadyExists(err) {
			return false, err
		}
		return false, nil
	case err != nil:
		return false, err
	case jobIsFailed(job):
		prop := metav1.DeletePropagationBackground
		if err := r.Delete(ctx, job, &client.DeleteOptions{PropagationPolicy: &prop}); err != nil && !apierrors.IsNotFound(err) {
			return false, err
		}
		return false, fmt.Errorf("the MariaDB setup Job %s failed", job.Name)
	}
	return jobIsComplete(job), nil
}

// ensureRetainedClaim makes a volume claim the bundle recorded, where the
// tenant has none of that name: with the size, the access modes and the
// labels and annotations it had, which say whose it is. Its storage class is
// this cluster's default, whatever class it named where the bundle was
// taken: a bundle may come from a cluster with other classes, and a claim
// that names one this cluster lacks is never bound. The result says so.
//
// The namespace Helm recorded on the claim is the tenant's here: left as it
// was, the release that installs the app again would refuse to adopt it.
func (r *TenantRestoreReconciler) ensureRetainedClaim(ctx context.Context, tenant *gentianov1alpha1.Tenant, a gentianov1alpha1.BundleArtefact) error {
	ns := backup.TenantNamespace(tenant)
	existing := &corev1.PersistentVolumeClaim{}
	err := r.Get(ctx, types.NamespacedName{Name: a.Target, Namespace: ns}, existing)
	if err == nil {
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return err
	}
	size, err := resource.ParseQuantity(a.Claim.Size)
	if err != nil {
		return fmt.Errorf("the bundle records the volume claim %s with a size that is none (%q)", a.Target, a.Claim.Size)
	}
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: a.Target, Namespace: ns, Labels: map[string]string{}, Annotations: map[string]string{}},
		Spec: corev1.PersistentVolumeClaimSpec{
			Resources: corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: size}},
		},
	}
	for key, value := range a.Claim.Labels {
		if claimIdentityKey(key) {
			pvc.Labels[key] = value
		}
	}
	for key, value := range a.Claim.Annotations {
		if claimIdentityKey(key) {
			pvc.Annotations[key] = value
		}
	}
	if _, ok := pvc.Annotations["meta.helm.sh/release-namespace"]; ok {
		pvc.Annotations["meta.helm.sh/release-namespace"] = ns
	}
	for _, mode := range a.Claim.AccessModes {
		pvc.Spec.AccessModes = append(pvc.Spec.AccessModes, corev1.PersistentVolumeAccessMode(mode))
	}
	if len(pvc.Spec.AccessModes) == 0 {
		pvc.Spec.AccessModes = []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}
	}
	if err := r.Create(ctx, pvc); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("make the volume claim %s: %w", a.Target, err)
	}
	return nil
}
