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

package controller

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/go-logr/logr"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/backup"
	"github.com/gentian-org/gentian-os/internal/meta"
)

const (
	conditionExportAccepted = "Accepted"
	conditionExportComplete = "Complete"

	// exportCaptureBackoffLimit bounds a capture Job's own retries.
	//
	// Low on purpose. waitForProvisioningJob deletes a failed Job so it is
	// recreated, which is right for provisioning — a Job that failed because a
	// dependency was not ready yet should simply run again — and wrong here: an
	// export failing because a database is unreachable would retry forever
	// while holding the app paused. The attempt counter in status is the outer
	// bound; this is the inner one.
	exportCaptureBackoffLimit int32 = 2

	// exportMaxAttempts is how many times a unit's Job may be recreated before
	// the export gives up and resumes the app.
	exportMaxAttempts int32 = 2

	// exportScratchLimit bounds the staging volume a dump is written to before
	// upload. A tenant larger than this fails its capture rather than filling a
	// node's ephemeral storage and taking unrelated workloads down with it.
	exportScratchLimit = "20Gi"

	exportRequeueAfter = 5 * time.Second

	// exportFinalizer holds a TenantExport until its bundle is gone from the
	// object store. Without it "delete the backup" deletes the kubectl view of
	// the backup and leaves the data — the part deletion is actually about.
	exportFinalizer = "gentianos.io/tenantexport-bundle"
)

// TenantExportReconciler captures a tenant's data into a bundle.
//
// The loop is deliberately sequential: one app is paused, captured and resumed
// before the next is touched. Capturing several at once would shorten the
// export and lengthen every individual app's outage, and the thing a tenant
// notices is their app being down — not how long the export took.
type TenantExportReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// Reconciler supplies the tenant-side helpers (Job waiting, requeue
	// backoff, catalogue lookups) that already exist on the tenant loop.
	Reconciler *TenantReconciler

	// VolumeReader lists a tenant's PVCs, uncached, and deliberately so — see
	// appVolumes for why the cached client is the wrong tool for this one read.
	// Optional: nil falls back to the cached Client, which is what the unit
	// suites build with and is correct there, since a fake client has no
	// informer to wedge.
	VolumeReader client.Reader
}

// +kubebuilder:rbac:groups=gentianos.io,resources=tenantexports,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=gentianos.io,resources=tenantexports/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=gentianos.io,resources=tenantexports/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps,resources=deployments;statefulsets,verbs=get;list;watch;update;patch

// persistentvolumeclaims, for three call sites that had no rule between them:
// appVolumes lists a tenant's claims to decide what an export captures and a
// restore puts back, and applifecycle's purge lists, gets and deletes them when
// an app is uninstalled. No verb here is speculative — get is the purge's
// wait-for-gone poll, delete is the purge itself.
//
// No watch: nothing reads this type through the manager's cache, and after
// appVolumes stopped doing so, granting it would only invite the read back.
// +kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=get;list;delete

func (r *TenantExportReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithName("tenantexport")

	export := &gentianov1alpha1.TenantExport{}
	if err := r.Get(ctx, req.NamespacedName, export); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	tenantName := tenantNameFromNamespace(export.Namespace)
	if tenantName == "" {
		return r.fail(ctx, export, "NotATenantNamespace",
			fmt.Sprintf("namespace %q is not a tenant namespace", export.Namespace))
	}

	if !export.DeletionTimestamp.IsZero() {
		return r.finalize(ctx, export, tenantName)
	}
	if !controllerutil.ContainsFinalizer(export, exportFinalizer) {
		controllerutil.AddFinalizer(export, exportFinalizer)
		if err := r.Update(ctx, export); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Resuming anything left paused comes before every other decision,
	// including the terminal check. A crash between pausing an app and
	// capturing it leaves the app scaled to zero, and nothing else in the
	// system knows it should not be — so this runs on the very next reconcile,
	// whatever else is true.
	if export.IsTerminal() {
		if err := r.resumeAll(ctx, export, tenantName); err != nil {
			return ctrl.Result{}, err
		}
		if err := r.discardPassphrase(ctx, export); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, r.discardDestinationCredential(ctx, export)
	}

	tenant := &gentianov1alpha1.Tenant{}
	if err := r.Get(ctx, types.NamespacedName{Name: tenantName}, tenant); err != nil {
		if apierrors.IsNotFound(err) {
			return r.fail(ctx, export, "TenantNotFound",
				fmt.Sprintf("tenant %q does not exist", tenantName))
		}
		return ctrl.Result{}, err
	}

	// One export or restore at a time per tenant. Two concurrent captures would
	// pause the same app twice and race each other's resume.
	if holder, blocked, err := r.blockedByAnotherExport(ctx, export); err != nil {
		return ctrl.Result{}, err
	} else if blocked {
		setExportCondition(export, conditionExportAccepted, metav1.ConditionFalse, "Busy",
			fmt.Sprintf("waiting for export %q to finish", holder))
		export.Status.Phase = gentianov1alpha1.TenantExportPhasePending
		return ctrl.Result{RequeueAfter: exportRequeueAfter}, r.persist(ctx, export)
	}
	setExportCondition(export, conditionExportAccepted, metav1.ConditionTrue, "Accepted",
		"this export holds the tenant")

	if export.Status.Bundle == nil {
		// Resolved once, when the bundle is assigned, and recorded on it. An
		// export that resolved its destination afresh on every reconcile could
		// have half its artefacts in one bucket and half in another if the
		// policy changed while it ran.
		eff, effErr := r.effectivePolicy(ctx, tenant)
		if effErr != nil {
			return r.fail(ctx, export, "PolicyUnusable", effErr.Error())
		}
		// The export's own choice, applied over the policy. Recorded on the
		// bundle like everything else here, so a restore reads where this
		// bundle actually went rather than where the policy points today.
		eff = backup.ApplyExportDestination(eff, export.Spec.Destination, tenant)
		export.Status.Bundle = &gentianov1alpha1.BundleRef{
			Bucket:           eff.Bucket,
			Prefix:           export.Name,
			Endpoint:         eff.Endpoint,
			Region:           eff.Region,
			CredentialSecret: eff.CredentialSecret,
		}
		export.Status.Phase = gentianov1alpha1.TenantExportPhaseRunning
		export.Status.StartedAt = ptrNow()
		if err := r.persist(ctx, export); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Encryption is resolved before any capture starts. Discovering halfway
	// through that a bundle cannot be protected would leave half a tenant's
	// data already written in the clear.
	encryption, err := r.resolveEncryption(ctx, export)
	if err != nil {
		return r.fail(ctx, export, "EncryptionUnavailable", err.Error())
	}

	// Before any capture, for the reason encryption is: finding out mid-run
	// that the destination cannot be authenticated leaves an app paused and a
	// bundle half written.
	if err := r.stageDestinationCredential(ctx, export); err != nil {
		return r.fail(ctx, export, "DestinationUnavailable", err.Error())
	}
	recordEncryption(export, encryption)

	apps, err := r.exportAppSet(ctx, tenant, export)
	if err != nil {
		return ctrl.Result{}, err
	}

	// Resume anything paused that is not the app currently being captured. This
	// is the crash-recovery path: after a restart the controller has no memory
	// of what it was doing, and status.quiesced is the only record.
	current := nextPendingApp(export.Status.Apps, apps)
	if err := r.resumeStale(ctx, export, tenantName, current); err != nil {
		return ctrl.Result{}, err
	}

	if current != "" {
		return r.captureApp(ctx, export, tenant, current, encryption, logger)
	}

	// Every app is done; capture what belongs to the tenant rather than to an
	// app. Neither is quiesced: the realm and the shell database are low-write
	// and internally consistent, and pausing identity would lock every member
	// out of every app for the duration.
	if done, err := r.captureTenantWide(ctx, export, tenant, encryption); err != nil {
		return ctrl.Result{}, err
	} else if !done {
		return r.requeueExport(ctx, export, tenant)
	}

	return r.complete(ctx, export, tenant, encryption)
}

// captureApp drives one app through pause, capture and resume.
func (r *TenantExportReconciler) captureApp(
	ctx context.Context,
	export *gentianov1alpha1.TenantExport,
	tenant *gentianov1alpha1.Tenant,
	appName string,
	encryption backup.Encryption,
	logger logr.Logger,
) (ctrl.Result, error) {
	profile, err := resolveProfile(ctx, r.Client, tenant, appName)
	if err != nil {
		return ctrl.Result{}, err
	}
	spec := profileBackupSpec(profile)
	entry := appStatus(&export.Status.Apps, appName)

	if entry.QuiesceStart == nil {
		mode, qErr := r.Reconciler.quiesceApp(ctx, tenant.Name, appName, spec)
		if qErr != nil {
			return r.failApp(ctx, export, tenant, appName, fmt.Sprintf("pause failed: %v", qErr))
		}
		entry.QuiesceStart = ptrNow()
		entry.QuiesceMode = string(mode)
		entry.Phase = gentianov1alpha1.TenantExportPhaseRunning
		entry.Message = fmt.Sprintf("paused (%s)", mode)
		markQuiesced(&export.Status.Quiesced, appName)
		logger.Info("paused app for capture", "app", appName, "mode", mode)
		if err := r.persist(ctx, export); err != nil {
			return ctrl.Result{}, err
		}
	}

	units, err := r.captureUnits(ctx, tenant, appName, profile, export, encryption)
	if err != nil {
		// The app is paused by this point. failApp resumes it and records why,
		// which is the whole difference between a failed export and a hung one.
		return r.failApp(ctx, export, tenant, appName,
			fmt.Sprintf("enumerate what to capture: %v", err))
	}
	for _, unit := range units {
		if unit.Kind == "volume" {
			if err := r.ensureVolumeUploadSecret(ctx, export, tenant.Name, encryption); err != nil {
				return ctrl.Result{}, fmt.Errorf("stage volume credentials: %w", err)
			}
			break
		}
	}
	allDone := true
	var pending []string
	for _, unit := range units {
		done, err := r.ensureCaptureJob(ctx, export, unit)
		if err != nil {
			return ctrl.Result{}, err
		}
		if !done {
			allDone = false
			pending = append(pending, unit.JobName)
		}
	}

	if !allDone {
		if entry.Attempts > exportMaxAttempts {
			return r.failApp(ctx, export, tenant, appName,
				fmt.Sprintf("capture did not succeed after %d attempts (last waiting on %s)",
					entry.Attempts, strings.Join(pending, ", ")))
		}
		// Name what is outstanding. An export sitting at Running with an app
		// paused is the situation an operator most needs to diagnose, and
		// "waiting" without saying for what sends them to the Job list to guess.
		entry.Message = fmt.Sprintf("capturing; waiting on %s", strings.Join(pending, ", "))
		return r.requeueExport(ctx, export, tenant)
	}

	if err := resumeQuiescedApp(ctx, r.Client, r.Reconciler,
		tenant.Name, appName, entry.QuiesceMode, entry.Message); err != nil {
		return ctrl.Result{}, fmt.Errorf("resume %s: %w", appName, err)
	}
	entry.QuiesceEnd = ptrNow()
	if entry.QuiesceStart != nil {
		tenantExportQuiesceDuration.
			WithLabelValues(tenant.Name, appName).
			Observe(entry.QuiesceEnd.Sub(entry.QuiesceStart.Time).Seconds())
	}
	entry.Phase = gentianov1alpha1.TenantExportPhaseReady
	entry.Message = ""
	entry.ChartVersion = profileChartVersion(profile)
	entry.Stores = unitKinds(units)
	unmarkQuiesced(&export.Status.Quiesced, appName)
	logger.Info("captured app", "app", appName, "stores", entry.Stores)
	if err := r.persist(ctx, export); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{Requeue: true}, nil
}

// captureUnit is one artefact to produce.
type captureUnit struct {
	Kind    string
	Name    string
	Path    string
	JobName string
	Job     *batchv1.Job
}

// captureUnits enumerates what to capture for one app, entirely from the
// profile's declared kernelRequirements. No app name appears here, and none may.
func (r *TenantExportReconciler) captureUnits(
	ctx context.Context,
	tenant *gentianov1alpha1.Tenant,
	appName string,
	profile *gentianov1alpha1.AppProfile,
	export *gentianov1alpha1.TenantExport,
	encryption backup.Encryption,
) ([]captureUnit, error) {
	stores := backup.ProfileStores(profile)
	spec := profileBackupSpec(profile)
	params := r.jobParams(tenant, appName, export, encryption)

	var units []captureUnit
	switch stores.Database {
	case gentianov1alpha1.DatabaseEnginePostgreSQL:
		db := backup.DatabaseName(tenant, appName)
		p := params
		p.Name = exportJobName(export.Name, appName, "pg")
		units = append(units, captureUnit{
			Kind: "postgres", Name: db, Path: "postgres/" + db + ".pgc",
			JobName: p.Name, Job: backup.PostgresDumpJob(p, db),
		})
	case gentianov1alpha1.DatabaseEngineMariaDB:
		db := backup.DatabaseName(tenant, appName)
		p := params
		p.Name = exportJobName(export.Name, appName, "maria")
		units = append(units, captureUnit{
			Kind: "mariadb", Name: db, Path: "mariadb/" + db + ".sql.gz",
			JobName: p.Name, Job: backup.MariaDBDumpJob(p, db),
		})
	}

	if stores.S3 {
		bucket := backup.S3Bucket(tenant, appName)
		p := params
		p.Name = exportJobName(export.Name, appName, "s3")
		units = append(units, captureUnit{
			Kind: "s3", Name: bucket, Path: "s3/" + bucket + ".tar.gz",
			JobName: p.Name, Job: backup.S3ArchiveJob(p, bucket),
		})
	}

	// Volume Jobs run in the tenant namespace: a PVC is only mountable from
	// its own namespace, and the first live volume capture hung Pending
	// forever in the kernel namespace, holding its app paused the whole time.
	// Their credentials come from the staged copy (see ensureVolumeUploadSecret),
	// which also carries the passphrase for a passphrase-mode export.
	volParams := params
	volParams.Namespace = backup.TenantNamespace(tenant.Name)
	volParams.UploadCredentialsSecret = volumeUploadSecretName(export.Name)
	if volParams.Encryption.Mode == gentianov1alpha1.ExportEncryptionPassphrase {
		volParams.Encryption.PassphraseSecret = volumeUploadSecretName(export.Name)
	}
	claims, err := r.appVolumes(ctx, tenant.Name, appName, profile, spec)
	if err != nil {
		return nil, err
	}
	for i, claim := range claims {
		p := volParams
		p.Name = exportJobName(export.Name, appName, fmt.Sprintf("vol%d", i))
		units = append(units, captureUnit{
			Kind: "volume", Name: claim, Path: "volumes/" + claim + ".tar.gz",
			JobName: p.Name, Job: backup.VolumeArchiveJob(p, claim, spec.ExcludedPaths()),
		})
	}
	return units, nil
}

// appVolumes resolves which claims to capture: the profile's explicit list when
// it has one, otherwise every claim the app owns.
//
// Reads uncached. The manager's cached client establishes an informer the first
// time a type is read through it, and an informer that may not list never syncs
// — it retries on a loop while the read that started it blocks waiting for a
// sync that will not come. A reconcile carries no deadline, so on 2026-08-30 a
// missing `persistentvolumeclaims` rule did not surface as the Forbidden it was.
// It surfaced as an export that paused app-store-me, logged "paused app for
// capture", and never logged again: one worker wedged forever, the tenant's app
// offline, no error anywhere. An uncached read returns the Forbidden and the
// export fails the way a failure should look.
//
// Threading the context through here was worth doing, but it was never the
// bound it looked like — cancellation only helps if something cancels.
//
// The list not caching is also just right for the work: this enumerates one
// namespace's claims once per app per export, which does not warrant holding
// every PVC in the cluster in memory for the life of the process.
//
// A failed list is an error, not an empty result. Returning nil on failure
// captured no volumes at all and still reported the app Ready — a bundle that
// looks complete and restores a database without the files it refers to, which
// is the one outcome a backup must never produce quietly.
func (r *TenantExportReconciler) appVolumes(
	ctx context.Context,
	tenantName, appName string,
	profile *gentianov1alpha1.AppProfile,
	spec *gentianov1alpha1.BackupSpec,
) ([]string, error) {
	if included := spec.IncludedVolumes(); len(included) > 0 {
		return included, nil
	}

	namespace := backup.TenantNamespace(tenantName)
	reader := client.Reader(r.Client)
	if r.VolumeReader != nil {
		reader = r.VolumeReader
	}
	pvcs := &corev1.PersistentVolumeClaimList{}
	if err := reader.List(ctx, pvcs, client.InNamespace(namespace)); err != nil {
		return nil, fmt.Errorf("list claims in %s: %w", namespace, err)
	}
	family := ""
	if profile != nil {
		family = profile.Spec.Family
	}
	var claims []string
	for _, pvc := range pvcs.Items {
		if backup.PVCBelongsToApp(pvc, appName, family) {
			claims = append(claims, pvc.Name)
		}
	}
	sort.Strings(claims)
	return claims, nil
}

// ensureCaptureJob creates a Job if absent and reports whether it has finished.
func (r *TenantExportReconciler) ensureCaptureJob(
	ctx context.Context,
	export *gentianov1alpha1.TenantExport,
	unit captureUnit,
) (bool, error) {
	entry := appStatus(&export.Status.Apps, unit.Job.Labels[meta.AppLabel])
	// The status record decides, not the Job's existence: a finished Job can
	// be TTL-collected or swept by the kernel Job GC while a sibling unit is
	// still running, and recreating it here re-ran a dump that had already
	// been uploaded.
	if slices.Contains(entry.CompletedUnits, unit.JobName) {
		return true, nil
	}

	existing := &batchv1.Job{}
	err := r.Get(ctx, types.NamespacedName{Name: unit.JobName, Namespace: unit.Job.Namespace}, existing)
	switch {
	case apierrors.IsNotFound(err):
		if err := r.Create(ctx, unit.Job); err != nil && !apierrors.IsAlreadyExists(err) {
			return false, fmt.Errorf("create capture Job %s: %w", unit.JobName, err)
		}
		return false, nil
	case err != nil:
		return false, err
	}

	if jobIsComplete(existing) {
		entry.CompletedUnits = append(entry.CompletedUnits, unit.JobName)
		return true, nil
	}
	if jobIsFailed(existing) {
		// Count the failure against the app, then delete so the next pass can
		// retry — bounded by exportMaxAttempts, unlike the provisioning waiter.
		entry.Attempts++
		if err := r.Delete(ctx, existing, client.PropagationPolicy(metav1.DeletePropagationBackground)); err != nil &&
			!apierrors.IsNotFound(err) {
			return false, err
		}
	}
	return false, nil
}

// captureTenantWide captures the realm and the portal shell database.
func (r *TenantExportReconciler) captureTenantWide(
	ctx context.Context,
	export *gentianov1alpha1.TenantExport,
	tenant *gentianov1alpha1.Tenant,
	encryption backup.Encryption,
) (bool, error) {
	params := r.jobParams(tenant, backupTenantComponent, export, encryption)

	realmParams := params
	realmParams.Name = exportJobName(export.Name, backupTenantComponent, "realm")
	realm := keycloakRealmName(tenant)
	units := []captureUnit{{
		Kind: "identity", Name: realm, Path: "identity/realm.tar.gz",
		JobName: realmParams.Name, Job: backup.RealmExportJob(realmParams, realm),
	}}

	shellParams := params
	shellParams.Name = exportJobName(export.Name, backupTenantComponent, "shell")
	shellDB := databaseName(tenant, portalShellAppName)
	units = append(units, captureUnit{
		Kind: "postgres", Name: shellDB, Path: "postgres/" + shellDB + ".pgc",
		JobName: shellParams.Name, Job: backup.PostgresDumpJob(shellParams, shellDB),
	})

	allDone := true
	for _, unit := range units {
		done, err := r.ensureCaptureJob(ctx, export, unit)
		if err != nil {
			return false, err
		}
		if !done {
			allDone = false
		}
	}
	return allDone, nil
}

func (r *TenantExportReconciler) jobParams(
	tenant *gentianov1alpha1.Tenant,
	appName string,
	export *gentianov1alpha1.TenantExport,
	encryption backup.Encryption,
) backup.JobParams {
	bundle := export.Status.Bundle
	p := backup.JobParams{
		Namespace:    kernelNamespace,
		Tenant:       tenant.Name,
		App:          appName,
		Export:       export.Name,
		Bucket:       bundle.Bucket,
		Prefix:       bundle.Prefix,
		Endpoint:     bundle.Endpoint,
		Region:       bundle.Region,
		ScratchLimit: exportScratchLimit,
		BackoffLimit: exportCaptureBackoffLimit,
		Encryption:   encryption,
	}
	// The bundle's own record, not the policy's current answer: these Jobs
	// write into a bundle whose address was fixed when it was assigned.
	if bundle.CredentialSecret != "" {
		p.UploadCredentialsSecret = bundle.CredentialSecret
	}
	return p
}

// effectivePolicy resolves what this tenant's backups do right now.
func (r *TenantExportReconciler) effectivePolicy(
	ctx context.Context,
	tenant *gentianov1alpha1.Tenant,
) (backup.Effective, error) {
	cluster := &gentianov1alpha1.BackupPolicy{}
	if err := r.Get(ctx, types.NamespacedName{Name: clusterBackupPolicy}, cluster); err != nil {
		if !apierrors.IsNotFound(err) {
			return backup.Effective{}, err
		}
		cluster = nil
	}
	override := &gentianov1alpha1.BackupPolicy{}
	if err := r.Get(ctx, types.NamespacedName{Name: tenant.Name}, override); err != nil {
		if !apierrors.IsNotFound(err) {
			return backup.Effective{}, err
		}
		override = nil
	}
	// A policy named after the tenant but scoped to the cluster is not this
	// tenant's override; treating it as one would apply the cluster default
	// twice and, worse, silently.
	if override != nil && (override.Spec.Scope != "tenant" || override.Spec.Tenant != tenant.Name) {
		override = nil
	}
	return backup.ResolveEffective(tenant, cluster, override)
}

// complete writes the manifest and marks the export done. The manifest is
// written last, so its presence is what makes a bundle restorable.
func (r *TenantExportReconciler) complete(
	ctx context.Context,
	export *gentianov1alpha1.TenantExport,
	tenant *gentianov1alpha1.Tenant,
	encryption backup.Encryption,
) (ctrl.Result, error) {
	params := r.jobParams(tenant, backupTenantComponent, export, encryption)
	params.Name = exportJobName(export.Name, backupTenantComponent, "manifest")

	info := backup.NewBundleInfo(tenant.Name, export.Name, timeOrNow(export.Status.StartedAt), encryption)
	job, err := backup.ManifestJob(params, r.buildManifest(export, tenant), info)
	if err != nil {
		return r.fail(ctx, export, "ManifestFailed", err.Error())
	}
	done, err := r.ensureCaptureJob(ctx, export, captureUnit{
		Kind: "manifest", Name: "manifest.json", Path: "manifest.json",
		JobName: params.Name, Job: job,
	})
	if err != nil {
		return ctrl.Result{}, err
	}
	if !done {
		return r.requeueExport(ctx, export, tenant)
	}

	if err := r.discardPassphrase(ctx, export); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.discardVolumeUploadSecret(ctx, export); err != nil {
		return ctrl.Result{}, err
	}
	export.Status.Phase = gentianov1alpha1.TenantExportPhaseReady
	export.Status.CompletedAt = ptrNow()
	tenantExportTotal.WithLabelValues(tenant.Name, string(gentianov1alpha1.TenantExportPhaseReady)).Inc()
	setExportCondition(export, conditionExportComplete, metav1.ConditionTrue, "Captured",
		fmt.Sprintf("%d app(s) captured", len(export.Status.Apps)))
	return ctrl.Result{}, r.persist(ctx, export)
}

func (r *TenantExportReconciler) buildManifest(
	export *gentianov1alpha1.TenantExport,
	tenant *gentianov1alpha1.Tenant,
) *backup.Manifest {
	spec := tenant.Spec.DeepCopy()
	m := &backup.Manifest{
		SchemaVersion: backup.ManifestSchemaVersion,
		Tenant:        tenant.Name,
		TenantSpec:    spec,
		Export:        export.Name,
		CreatedAt:     timeOrNow(export.Status.StartedAt),
		Identity: &backup.ManifestIdentity{
			Realm: keycloakRealmName(tenant),
			Path:  "identity/realm.tar.gz",
			// Keycloak's partial-export carries no credentials, so a restore
			// must re-invite rather than pretend the old passwords survived.
			PasswordsIncluded: false,
		},
		Shell: &backup.ManifestStore{
			Kind: "postgres",
			Name: databaseName(tenant, portalShellAppName),
			Path: "postgres/" + databaseName(tenant, portalShellAppName) + ".pgc",
		},
	}
	for _, app := range export.Status.Apps {
		m.Apps = append(m.Apps, backup.ManifestApp{
			Name:         app.Name,
			ChartVersion: app.ChartVersion,
			QuiesceStart: timeOrEmpty(app.QuiesceStart),
			QuiesceEnd:   timeOrEmpty(app.QuiesceEnd),
			QuiesceMode:  strings.TrimPrefix(app.Message, "paused "),
			Stores:       manifestStores(app),
		})
	}
	return m
}

// failApp resumes the app and fails the whole export. A partial bundle is left
// in place: it is evidence, and deleting it would remove the only record of
// what went wrong.
// finalize is the deletion path: it aborts a running export and removes the
// bundle from the object store before the CR is allowed to disappear.
//
// Resume comes first, unconditionally. Deleting the CR is the only abort a
// tenant admin has, and this reconcile loop is the only thing that knows an
// app is paused — without this, deleting a running export would freeze its
// current app at zero replicas with nothing left to notice.
func (r *TenantExportReconciler) finalize(
	ctx context.Context,
	export *gentianov1alpha1.TenantExport,
	tenantName string,
) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(export, exportFinalizer) {
		// An export from before the finalizer existed; nothing holds it.
		return ctrl.Result{}, nil
	}
	logger := log.FromContext(ctx).WithName("tenantexport")

	if err := r.resumeAll(ctx, export, tenantName); err != nil {
		return ctrl.Result{}, err
	}
	// Best effort, as on the failure path: staged Secrets are transient state
	// and must not be able to block deletion.
	_ = r.discardPassphrase(ctx, export)
	_ = r.discardDestinationCredential(ctx, export)
	_ = r.discardVolumeUploadSecret(ctx, export)

	// Outstanding capture Jobs upload into the very prefix being deleted;
	// stop them before the cleanup Job races them.
	if err := r.deleteExportJobs(ctx, export); err != nil {
		return ctrl.Result{}, err
	}

	// A backup exists to survive the deletion of what it backs up.
	//
	// These CRs live in the tenant's namespace, so tearing a tenant down
	// deletes them — and deleting them used to delete their bundles, which
	// made "purge the tenant and restore it" destroy the only thing that could
	// have restored it. Teardown therefore keeps the bundles and says where
	// they are; only deleting an export while its tenant still exists means
	// "delete this backup".
	//
	// The cost is deliberate: bundles outlive their tenant and are removed by
	// hand, which matters for an erasure request. That is the right way round
	// — retained data can still be deleted afterwards, and a bundle deleted
	// during teardown is gone at the one moment it was most needed.
	if torn, why := r.tenantTeardown(ctx, export, tenantName); torn {
		if b := export.Status.Bundle; b != nil && b.Prefix != "" {
			logger.Info("keeping bundle: the tenant is being torn down, and a backup must outlive it",
				"export", export.Name, "bucket", b.Bucket, "prefix", b.Prefix, "reason", why)
		}
		controllerutil.RemoveFinalizer(export, exportFinalizer)
		return ctrl.Result{}, r.Update(ctx, export)
	}

	done, err := r.ensureBundleDeleted(ctx, export, tenantName, logger)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !done {
		return ctrl.Result{RequeueAfter: exportRequeueAfter}, nil
	}
	controllerutil.RemoveFinalizer(export, exportFinalizer)
	return ctrl.Result{}, r.Update(ctx, export)
}

// tenantTeardown reports whether this export is disappearing because its tenant
// is, rather than because someone deleted the backup.
//
// Either signal is enough on its own: the namespace going away sweeps every
// export in it, and the Tenant going away is what the namespace is following.
// A lookup that fails for any other reason is treated as teardown too — the
// safe direction, since the consequence is a retained bundle rather than a
// deleted one.
func (r *TenantExportReconciler) tenantTeardown(
	ctx context.Context,
	export *gentianov1alpha1.TenantExport,
	tenantName string,
) (bool, string) {
	ns := &corev1.Namespace{}
	switch err := r.Get(ctx, types.NamespacedName{Name: export.Namespace}, ns); {
	case apierrors.IsNotFound(err):
		return true, "namespace is gone"
	case err != nil:
		return true, fmt.Sprintf("namespace could not be read: %v", err)
	case ns.DeletionTimestamp != nil:
		return true, "namespace is terminating"
	}

	tenant := &gentianov1alpha1.Tenant{}
	switch err := r.Get(ctx, types.NamespacedName{Name: tenantName}, tenant); {
	case apierrors.IsNotFound(err):
		return true, "tenant is gone"
	case err != nil:
		return true, fmt.Sprintf("tenant could not be read: %v", err)
	case tenant.DeletionTimestamp != nil:
		return true, "tenant is terminating"
	}
	return false, ""
}

// bundleDeleteJobName names the cleanup Job; deleteExportJobs spares it.
func bundleDeleteJobName(exportName string) string {
	return exportJobName(exportName, "bundle", "rm")
}

// deleteExportJobs removes every capture Job belonging to this export — in
// the kernel namespace, and in the tenant namespace where volume Jobs run.
func (r *TenantExportReconciler) deleteExportJobs(ctx context.Context, export *gentianov1alpha1.TenantExport) error {
	keep := bundleDeleteJobName(export.Name)
	for _, ns := range []string{kernelNamespace, export.Namespace} {
		jobs := &batchv1.JobList{}
		if err := r.List(ctx, jobs,
			client.InNamespace(ns),
			client.MatchingLabels{backup.ExportLabel: export.Name}); err != nil {
			return fmt.Errorf("list capture jobs for %s in %s: %w", export.Name, ns, err)
		}
		for i := range jobs.Items {
			if jobs.Items[i].Name == keep {
				continue
			}
			if err := r.Delete(ctx, &jobs.Items[i],
				client.PropagationPolicy(metav1.DeletePropagationBackground)); err != nil && !apierrors.IsNotFound(err) {
				return fmt.Errorf("delete capture job %s: %w", jobs.Items[i].Name, err)
			}
		}
	}
	return nil
}

// ensureBundleDeleted runs the bundle-cleanup Job and reports when the
// bundle is gone. A cleanup Job that exhausts its retries releases the CR
// anyway: an undeletable object is the worse failure, and the log names the
// bucket and prefix the leftover objects can be removed from by hand.
func (r *TenantExportReconciler) ensureBundleDeleted(
	ctx context.Context,
	export *gentianov1alpha1.TenantExport,
	tenantName string,
	logger logr.Logger,
) (bool, error) {
	bundle := export.Status.Bundle
	if bundle == nil || bundle.Bucket == "" || bundle.Prefix == "" {
		// Nothing was ever written; there is nothing to clean.
		return true, nil
	}

	name := bundleDeleteJobName(export.Name)
	existing := &batchv1.Job{}
	err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: kernelNamespace}, existing)
	switch {
	case apierrors.IsNotFound(err):
		// Deleting a bundle must address the storage it was written to. A
		// cleanup that assumed the platform's own would report success having
		// deleted a prefix that was never there, leaving the real objects.
		deleteParams := backup.JobParams{
			Namespace:    kernelNamespace,
			Name:         name,
			Tenant:       tenantName,
			App:          "bundle",
			Export:       export.Name,
			Bucket:       bundle.Bucket,
			Prefix:       bundle.Prefix,
			Endpoint:     bundle.Endpoint,
			Region:       bundle.Region,
			BackoffLimit: exportCaptureBackoffLimit,
		}
		if bundle.CredentialSecret != "" {
			deleteParams.UploadCredentialsSecret = bundle.CredentialSecret
		}
		job := backup.BundleDeleteJob(deleteParams)
		if err := r.Create(ctx, job); err != nil && !apierrors.IsAlreadyExists(err) {
			return false, fmt.Errorf("create bundle cleanup Job %s: %w", name, err)
		}
		return false, nil
	case err != nil:
		return false, err
	}

	switch {
	case jobIsComplete(existing):
	case jobIsFailed(existing):
		logger.Error(nil, "bundle cleanup failed; releasing the export anyway - remove the objects by hand",
			"export", export.Name, "bucket", bundle.Bucket, "prefix", bundle.Prefix, "job", name)
	default:
		return false, nil
	}
	if err := r.Delete(ctx, existing,
		client.PropagationPolicy(metav1.DeletePropagationBackground)); err != nil && !apierrors.IsNotFound(err) {
		return false, err
	}
	return true, nil
}

func (r *TenantExportReconciler) failApp(
	ctx context.Context,
	export *gentianov1alpha1.TenantExport,
	tenant *gentianov1alpha1.Tenant,
	appName, message string,
) (ctrl.Result, error) {
	entry := appStatus(&export.Status.Apps, appName)
	if err := resumeQuiescedApp(ctx, r.Client, r.Reconciler,
		tenant.Name, appName, entry.QuiesceMode, entry.Message); err != nil {
		return ctrl.Result{}, fmt.Errorf("resume %s after failure: %w", appName, err)
	}
	unmarkQuiesced(&export.Status.Quiesced, appName)
	// The app is resumed by the time we get here, and the status must say so:
	// quiesceEnd left unset painted the app as "paused now" in the Admin
	// Console for as long as the failed export existed, which reads as an
	// outage when there is none.
	if entry.QuiesceStart != nil && entry.QuiesceEnd == nil {
		entry.QuiesceEnd = ptrNow()
		tenantExportQuiesceDuration.
			WithLabelValues(tenant.Name, appName).
			Observe(entry.QuiesceEnd.Sub(entry.QuiesceStart.Time).Seconds())
	}
	entry.Phase = gentianov1alpha1.TenantExportPhaseFailed
	entry.Message = message
	return r.fail(ctx, export, "CaptureFailed", fmt.Sprintf("%s: %s", appName, message))
}

func (r *TenantExportReconciler) fail(
	ctx context.Context,
	export *gentianov1alpha1.TenantExport,
	reason, message string,
) (ctrl.Result, error) {
	// Best effort: a failure to tidy the staged secrets must not mask the
	// failure being reported, but they are still attempted on the way out.
	_ = r.discardPassphrase(ctx, export)
	_ = r.discardDestinationCredential(ctx, export)
	_ = r.discardVolumeUploadSecret(ctx, export)
	export.Status.Phase = gentianov1alpha1.TenantExportPhaseFailed
	export.Status.CompletedAt = ptrNow()
	tenantExportTotal.WithLabelValues(
		tenantNameFromNamespace(export.Namespace),
		string(gentianov1alpha1.TenantExportPhaseFailed),
	).Inc()
	setExportCondition(export, conditionExportComplete, metav1.ConditionFalse, reason, message)
	return ctrl.Result{}, r.persist(ctx, export)
}

// resumeAll puts back everything this export paused, and is what makes a
// terminal export safe to leave alone.
func (r *TenantExportReconciler) resumeAll(
	ctx context.Context,
	export *gentianov1alpha1.TenantExport,
	tenantName string,
) error {
	if len(export.Status.Quiesced) == 0 {
		return nil
	}
	for _, appName := range append([]string(nil), export.Status.Quiesced...) {
		entry := appStatus(&export.Status.Apps, appName)
		if err := resumeQuiescedApp(ctx, r.Client, r.Reconciler,
			tenantName, appName, entry.QuiesceMode, entry.Message); err != nil {
			return fmt.Errorf("resume %s: %w", appName, err)
		}
		unmarkQuiesced(&export.Status.Quiesced, appName)
	}
	return r.persist(ctx, export)
}

// resumeStale resumes any paused app other than the one being captured now.
func (r *TenantExportReconciler) resumeStale(
	ctx context.Context,
	export *gentianov1alpha1.TenantExport,
	tenantName, current string,
) error {
	changed := false
	for _, appName := range append([]string(nil), export.Status.Quiesced...) {
		if appName == current {
			continue
		}
		entry := appStatus(&export.Status.Apps, appName)
		if err := resumeQuiescedApp(ctx, r.Client, r.Reconciler,
			tenantName, appName, entry.QuiesceMode, entry.Message); err != nil {
			return fmt.Errorf("resume stale %s: %w", appName, err)
		}
		unmarkQuiesced(&export.Status.Quiesced, appName)
		changed = true
	}
	if changed {
		return r.persist(ctx, export)
	}
	return nil
}

func (r *TenantExportReconciler) requeueExport(
	ctx context.Context,
	export *gentianov1alpha1.TenantExport,
	_ *gentianov1alpha1.Tenant,
) (ctrl.Result, error) {
	return ctrl.Result{RequeueAfter: exportRequeueAfter}, r.persist(ctx, export)
}

// blockedByAnotherExport reports whether an older, unfinished export holds the
// tenant. Ordering by creation time makes the winner deterministic, so two
// exports created together do not each decide the other should go first.
func (r *TenantExportReconciler) blockedByAnotherExport(
	ctx context.Context,
	export *gentianov1alpha1.TenantExport,
) (string, bool, error) {
	others := &gentianov1alpha1.TenantExportList{}
	if err := r.List(ctx, others, client.InNamespace(export.Namespace)); err != nil {
		return "", false, err
	}
	for i := range others.Items {
		other := &others.Items[i]
		if other.Name == export.Name || other.IsTerminal() {
			continue
		}
		if other.CreationTimestamp.Before(&export.CreationTimestamp) ||
			(other.CreationTimestamp.Equal(&export.CreationTimestamp) && other.Name < export.Name) {
			return other.Name, true, nil
		}
	}
	return "", false, nil
}

func (r *TenantExportReconciler) persist(ctx context.Context, export *gentianov1alpha1.TenantExport) error {
	export.Status.ObservedGeneration = export.Generation
	return r.Status().Update(ctx, export)
}

// exportAppSet resolves which apps to capture.
//
// The App claims in the tenant namespace are the authority, not
// Tenant.spec.apps. They are not the same list: a Composition may install an
// app the tenant never asked for by name — the LLM wiring adds open-webui to
// every tenant on a cluster with llmSupport enabled — and such an app has a
// database and volumes like any other. Reading spec.apps alone silently
// skipped it, producing a backup that looked complete and was not, which is
// the one failure this whole subsystem exists to prevent.
//
// spec.apps is still unioned in, so an app whose claim has not appeared yet is
// captured rather than quietly dropped.
func (r *TenantExportReconciler) exportAppSet(
	ctx context.Context,
	tenant *gentianov1alpha1.Tenant,
	export *gentianov1alpha1.TenantExport,
) ([]string, error) {
	seen := map[string]struct{}{}
	var installed []string
	add := func(name string) {
		if name == "" {
			return
		}
		if _, ok := seen[name]; ok {
			return
		}
		seen[name] = struct{}{}
		installed = append(installed, name)
	}

	claims := &unstructured.UnstructuredList{}
	claims.SetGroupVersionKind(appClaimGVK.GroupVersion().WithKind("AppList"))
	if err := r.List(ctx, claims, client.InNamespace(backup.TenantNamespace(tenant.Name))); err != nil {
		// Not fatal on its own: spec.apps still gives a usable set, and an
		// export that captures the declared apps beats one that refuses.
		log.FromContext(ctx).Error(err, "listing App claims; falling back to spec.apps",
			"tenant", tenant.Name)
	} else {
		for i := range claims.Items {
			add(appClaimProfile(&claims.Items[i]))
		}
	}

	for _, app := range tenant.Spec.Apps {
		add(app.Profile)
	}
	sort.Strings(installed)

	if len(export.Spec.Apps) == 0 {
		return installed, nil
	}

	wanted := make(map[string]struct{}, len(export.Spec.Apps))
	for _, name := range export.Spec.Apps {
		wanted[name] = struct{}{}
	}
	var selected []string
	for _, name := range installed {
		if _, ok := wanted[name]; ok {
			selected = append(selected, name)
		}
	}
	return selected, nil
}

// appClaimProfile returns the profile an App claim installs.
//
// spec.profileRef.name is what the Compositions set; spec.profile is the older
// spelling, and the claim's own name is the last resort, since app-default
// names each claim after its profile.
func appClaimProfile(claim *unstructured.Unstructured) string {
	if name, ok, _ := unstructured.NestedString(claim.Object, "spec", "profileRef", "name"); ok && name != "" {
		return name
	}
	if name, ok, _ := unstructured.NestedString(claim.Object, "spec", "profile"); ok && name != "" {
		return name
	}
	return claim.GetName()
}

func (r *TenantExportReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&gentianov1alpha1.TenantExport{}).
		Named("tenantexport").
		Complete(r)
}
