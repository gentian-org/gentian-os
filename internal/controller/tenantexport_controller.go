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
	"sort"
	"strings"
	"time"

	"github.com/go-logr/logr"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
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
}

// +kubebuilder:rbac:groups=gentianos.io,resources=tenantexports,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=gentianos.io,resources=tenantexports/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=gentianos.io,resources=tenantexports/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps,resources=deployments;statefulsets,verbs=get;list;watch;update;patch

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

	// Resuming anything left paused comes before every other decision,
	// including the terminal check. A crash between pausing an app and
	// capturing it leaves the app scaled to zero, and nothing else in the
	// system knows it should not be — so this runs on the very next reconcile,
	// whatever else is true.
	if export.IsTerminal() {
		if err := r.resumeAll(ctx, export, tenantName); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
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
		export.Status.Bundle = &gentianov1alpha1.BundleRef{
			Bucket: backup.BackupBucket(tenant),
			Prefix: export.Name,
		}
		export.Status.Phase = gentianov1alpha1.TenantExportPhaseRunning
		export.Status.StartedAt = ptrNow()
		if err := r.persist(ctx, export); err != nil {
			return ctrl.Result{}, err
		}
	}

	apps, err := r.exportAppSet(ctx, tenant, export)
	if err != nil {
		return ctrl.Result{}, err
	}

	// Resume anything paused that is not the app currently being captured. This
	// is the crash-recovery path: after a restart the controller has no memory
	// of what it was doing, and status.quiesced is the only record.
	current := nextPendingApp(export, apps)
	if err := r.resumeStale(ctx, export, tenantName, current); err != nil {
		return ctrl.Result{}, err
	}

	if current != "" {
		return r.captureApp(ctx, export, tenant, current, logger)
	}

	// Every app is done; capture what belongs to the tenant rather than to an
	// app. Neither is quiesced: the realm and the shell database are low-write
	// and internally consistent, and pausing identity would lock every member
	// out of every app for the duration.
	if done, err := r.captureTenantWide(ctx, export, tenant); err != nil {
		return ctrl.Result{}, err
	} else if !done {
		return r.requeueExport(ctx, export, tenant)
	}

	return r.complete(ctx, export, tenant)
}

// captureApp drives one app through pause, capture and resume.
func (r *TenantExportReconciler) captureApp(
	ctx context.Context,
	export *gentianov1alpha1.TenantExport,
	tenant *gentianov1alpha1.Tenant,
	appName string,
	logger logr.Logger,
) (ctrl.Result, error) {
	profile, err := resolveProfile(ctx, r.Client, tenant, appName)
	if err != nil {
		return ctrl.Result{}, err
	}
	spec := profileBackupSpec(profile)
	entry := appStatus(export, appName)

	if entry.QuiesceStart == nil {
		mode, qErr := r.Reconciler.quiesceApp(ctx, tenant.Name, appName, spec)
		if qErr != nil {
			return r.failApp(ctx, export, tenant, appName, fmt.Sprintf("pause failed: %v", qErr))
		}
		entry.QuiesceStart = ptrNow()
		entry.Phase = gentianov1alpha1.TenantExportPhaseRunning
		entry.Message = fmt.Sprintf("paused (%s)", mode)
		markQuiesced(export, appName)
		logger.Info("paused app for capture", "app", appName, "mode", mode)
		if err := r.persist(ctx, export); err != nil {
			return ctrl.Result{}, err
		}
	}

	units := r.captureUnits(tenant, appName, profile, export)
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
				fmt.Sprintf("capture did not succeed after %d attempts", entry.Attempts))
		}
		return r.requeueExport(ctx, export, tenant)
	}

	if err := r.Reconciler.resumeApp(ctx, tenant.Name, appName); err != nil {
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
	unmarkQuiesced(export, appName)
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
	tenant *gentianov1alpha1.Tenant,
	appName string,
	profile *gentianov1alpha1.AppProfile,
	export *gentianov1alpha1.TenantExport,
) []captureUnit {
	stores := backup.ProfileStores(profile)
	spec := profileBackupSpec(profile)
	params := r.jobParams(tenant, appName, export)

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
			Kind: "s3", Name: bucket, Path: "s3/" + bucket,
			JobName: p.Name, Job: backup.S3MirrorJob(p, bucket),
		})
	}

	for i, claim := range r.appVolumes(tenant.Name, appName, profile, spec) {
		p := params
		p.Name = exportJobName(export.Name, appName, fmt.Sprintf("vol%d", i))
		units = append(units, captureUnit{
			Kind: "volume", Name: claim, Path: "volumes/" + claim + ".tar.gz",
			JobName: p.Name, Job: backup.VolumeArchiveJob(p, claim, spec.ExcludedPaths()),
		})
	}
	return units
}

// appVolumes resolves which claims to capture: the profile's explicit list when
// it has one, otherwise every claim the app owns.
func (r *TenantExportReconciler) appVolumes(
	tenantName, appName string,
	profile *gentianov1alpha1.AppProfile,
	spec *gentianov1alpha1.BackupSpec,
) []string {
	if included := spec.IncludedVolumes(); len(included) > 0 {
		return included
	}

	pvcs := &corev1.PersistentVolumeClaimList{}
	if err := r.List(context.Background(), pvcs, client.InNamespace(backup.TenantNamespace(tenantName))); err != nil {
		return nil
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
	return claims
}

// ensureCaptureJob creates a Job if absent and reports whether it has finished.
func (r *TenantExportReconciler) ensureCaptureJob(
	ctx context.Context,
	export *gentianov1alpha1.TenantExport,
	unit captureUnit,
) (bool, error) {
	existing := &batchv1.Job{}
	err := r.Get(ctx, types.NamespacedName{Name: unit.JobName, Namespace: kernelNamespace}, existing)
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
		return true, nil
	}
	if jobIsFailed(existing) {
		// Count the failure against the app, then delete so the next pass can
		// retry — bounded by exportMaxAttempts, unlike the provisioning waiter.
		entry := appStatus(export, unit.Job.Labels[meta.AppLabel])
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
) (bool, error) {
	params := r.jobParams(tenant, backupTenantComponent, export)

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
) backup.JobParams {
	return backup.JobParams{
		Namespace:    kernelNamespace,
		Tenant:       tenant.Name,
		App:          appName,
		Export:       export.Name,
		Bucket:       export.Status.Bundle.Bucket,
		Prefix:       export.Status.Bundle.Prefix,
		ScratchLimit: exportScratchLimit,
		BackoffLimit: exportCaptureBackoffLimit,
	}
}

// complete writes the manifest and marks the export done. The manifest is
// written last, so its presence is what makes a bundle restorable.
func (r *TenantExportReconciler) complete(
	ctx context.Context,
	export *gentianov1alpha1.TenantExport,
	tenant *gentianov1alpha1.Tenant,
) (ctrl.Result, error) {
	params := r.jobParams(tenant, backupTenantComponent, export)
	params.Name = exportJobName(export.Name, backupTenantComponent, "manifest")

	job, err := backup.ManifestJob(params, r.buildManifest(export, tenant))
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
func (r *TenantExportReconciler) failApp(
	ctx context.Context,
	export *gentianov1alpha1.TenantExport,
	tenant *gentianov1alpha1.Tenant,
	appName, message string,
) (ctrl.Result, error) {
	if err := r.Reconciler.resumeApp(ctx, tenant.Name, appName); err != nil {
		return ctrl.Result{}, fmt.Errorf("resume %s after failure: %w", appName, err)
	}
	unmarkQuiesced(export, appName)
	entry := appStatus(export, appName)
	entry.Phase = gentianov1alpha1.TenantExportPhaseFailed
	entry.Message = message
	return r.fail(ctx, export, "CaptureFailed", fmt.Sprintf("%s: %s", appName, message))
}

func (r *TenantExportReconciler) fail(
	ctx context.Context,
	export *gentianov1alpha1.TenantExport,
	reason, message string,
) (ctrl.Result, error) {
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
		if err := r.Reconciler.resumeApp(ctx, tenantName, appName); err != nil {
			return fmt.Errorf("resume %s: %w", appName, err)
		}
		unmarkQuiesced(export, appName)
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
		if err := r.Reconciler.resumeApp(ctx, tenantName, appName); err != nil {
			return fmt.Errorf("resume stale %s: %w", appName, err)
		}
		unmarkQuiesced(export, appName)
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

// exportAppSet resolves which apps to capture, honouring spec.apps when set.
func (r *TenantExportReconciler) exportAppSet(
	_ context.Context,
	tenant *gentianov1alpha1.Tenant,
	export *gentianov1alpha1.TenantExport,
) ([]string, error) {
	installed := make([]string, 0, len(tenant.Spec.Apps))
	for _, app := range tenant.Spec.Apps {
		if app.Profile != "" {
			installed = append(installed, app.Profile)
		}
	}
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

func (r *TenantExportReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&gentianov1alpha1.TenantExport{}).
		Named("tenantexport").
		Complete(r)
}
