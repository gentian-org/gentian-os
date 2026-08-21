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
	"time"

	"github.com/robfig/cron/v3"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/backup"
)

const (
	conditionScheduleReady = "Ready"

	// scheduleMissedWindow is how late an export may start and still be taken.
	//
	// A schedule that fires while the operator is down should catch up, but not
	// indefinitely: waking up to six hours of missed nightly backups and taking
	// them all at once would pause a tenant's apps repeatedly for data that is
	// now identical anyway.
	scheduleMissedWindow = 1 * time.Hour
)

var cronParser = cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)

// TenantExportScheduleReconciler takes exports on a schedule and expires the
// bundles that age out.
type TenantExportScheduleReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// Now is overridable so the schedule arithmetic is testable without waiting
	// for wall-clock time to pass.
	Now func() time.Time
}

// +kubebuilder:rbac:groups=gentianos.io,resources=tenantexportschedules,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=gentianos.io,resources=tenantexportschedules/status,verbs=get;update;patch

func (r *TenantExportScheduleReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithName("tenantexportschedule")

	schedule := &gentianov1alpha1.TenantExportSchedule{}
	if err := r.Get(ctx, req.NamespacedName, schedule); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	spec, err := cronParser.Parse(schedule.Spec.Schedule)
	if err != nil {
		// An unusable expression is reported rather than retried: nothing about
		// waiting will make it parse, and a schedule silently doing nothing is
		// the worst way for a backup regime to fail.
		setScheduleCondition(schedule, metav1.ConditionFalse, "InvalidSchedule",
			fmt.Sprintf("%q is not a valid cron expression: %v", schedule.Spec.Schedule, err))
		return ctrl.Result{}, r.persist(ctx, schedule)
	}

	now := r.now()
	exports, err := r.scheduledExports(ctx, schedule)
	if err != nil {
		return ctrl.Result{}, err
	}
	r.recordLastSuccess(schedule, exports)

	if err := r.expire(ctx, schedule, exports); err != nil {
		return ctrl.Result{}, err
	}

	if schedule.Spec.Suspend {
		setScheduleCondition(schedule, metav1.ConditionFalse, "Suspended",
			"suspended; no exports will be created")
		schedule.Status.NextScheduleTime = nil
		return ctrl.Result{}, r.persist(ctx, schedule)
	}

	// An export already running holds the tenant, and a second one would sit
	// blocked behind it until the window passed. Skipping is the honest
	// outcome, and it is recorded rather than silent.
	if running := firstRunning(exports); running != "" {
		setScheduleCondition(schedule, metav1.ConditionTrue, "Waiting",
			fmt.Sprintf("export %s is still running; this window is skipped", running))
		next := spec.Next(now.UTC())
		schedule.Status.NextScheduleTime = &metav1.Time{Time: next}
		return ctrl.Result{RequeueAfter: requeueUntil(now, next)}, r.persist(ctx, schedule)
	}

	due, next := r.dueAt(spec, schedule, now)
	if due {
		name := scheduledExportName(schedule.Name, now)
		if err := r.createExport(ctx, schedule, name); err != nil {
			return ctrl.Result{}, err
		}
		logger.Info("created scheduled export", "export", name)
		schedule.Status.LastScheduleTime = &metav1.Time{Time: now}
		schedule.Status.LastExportName = name
		next = spec.Next(now.UTC())
	}

	schedule.Status.NextScheduleTime = &metav1.Time{Time: next}
	setScheduleCondition(schedule, metav1.ConditionTrue, "Scheduled",
		fmt.Sprintf("next export due %s", next.UTC().Format(time.RFC3339)))
	return ctrl.Result{RequeueAfter: requeueUntil(now, next)}, r.persist(ctx, schedule)
}

// dueAt reports whether an export is owed now, and when the next one falls.
//
// A schedule that has never run does not immediately fire: creating a backup
// the moment someone declares a schedule would pause a tenant's apps as a side
// effect of writing YAML.
func (r *TenantExportScheduleReconciler) dueAt(
	spec cron.Schedule,
	schedule *gentianov1alpha1.TenantExportSchedule,
	now time.Time,
) (bool, time.Time) {
	last := schedule.Status.LastScheduleTime
	if last == nil {
		return false, spec.Next(now.UTC())
	}

	// UTC explicitly, on both sides. robfig/cron evaluates an expression in the
	// location of the time it is given, so a timestamp that came back from the
	// API in a local zone would shift every firing by that zone's offset —
	// which is the "silently moves by an hour" failure the schedule field
	// promises not to have.
	fireAt := spec.Next(last.UTC())
	if fireAt.After(now) {
		return false, fireAt
	}
	// Missed while the operator was down. Take one export if the window is
	// still recent, then resume the normal cadence — never a burst of catch-up
	// backups whose contents would be identical.
	if now.Sub(fireAt) > scheduleMissedWindow {
		return false, spec.Next(now.UTC())
	}
	return true, spec.Next(now.UTC())
}

func (r *TenantExportScheduleReconciler) createExport(
	ctx context.Context,
	schedule *gentianov1alpha1.TenantExportSchedule,
	name string,
) error {
	export := &gentianov1alpha1.TenantExport{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: schedule.Namespace,
			Labels: map[string]string{
				scheduleLabel:  schedule.Name,
				managedByLabel: managedByValue,
			},
		},
		Spec: gentianov1alpha1.TenantExportSpec{
			Apps:       schedule.Spec.Apps,
			Encryption: schedule.Spec.Encryption,
		},
	}
	if err := r.Create(ctx, export); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create scheduled export %s: %w", name, err)
	}
	return nil
}

// expire deletes finished exports beyond the retention count, oldest first.
//
// Only finished ones: deleting a running export would strand the app it has
// paused, because the controller that would have resumed it is gone with it.
func (r *TenantExportScheduleReconciler) expire(
	ctx context.Context,
	schedule *gentianov1alpha1.TenantExportSchedule,
	exports []gentianov1alpha1.TenantExport,
) error {
	retention := schedule.Spec.EffectiveRetention()
	if !retention.IsSet() {
		return nil
	}

	// Only finished exports are candidates. A running one has no completion
	// time to place in a period, and deleting it would abort a capture that a
	// tenant is waiting on.
	byName := make(map[string]*gentianov1alpha1.TenantExport, len(exports))
	var candidates []backup.Candidate
	for i := range exports {
		export := &exports[i]
		if !export.IsTerminal() {
			continue
		}
		byName[export.Name] = export
		// CompletedAt when it exists, creation otherwise: an export that
		// failed before finishing still occupies the night it ran.
		at := export.CreationTimestamp.Time
		if export.Status.CompletedAt != nil {
			at = export.Status.CompletedAt.Time
		}
		candidates = append(candidates, backup.Candidate{Name: export.Name, FinishedAt: at})
	}

	for _, name := range backup.SelectForDeletion(candidates, retention, time.Now().UTC()) {
		victim := byName[name]
		if victim == nil {
			continue
		}
		if err := r.Delete(ctx, victim); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("expire export %s: %w", name, err)
		}
	}
	return nil
}

// scheduledExports returns this schedule's own exports, newest first.
func (r *TenantExportScheduleReconciler) scheduledExports(
	ctx context.Context,
	schedule *gentianov1alpha1.TenantExportSchedule,
) ([]gentianov1alpha1.TenantExport, error) {
	list := &gentianov1alpha1.TenantExportList{}
	if err := r.List(ctx, list,
		client.InNamespace(schedule.Namespace),
		client.MatchingLabels{scheduleLabel: schedule.Name},
	); err != nil {
		return nil, err
	}
	items := list.Items
	sort.Slice(items, func(i, j int) bool {
		return items[j].CreationTimestamp.Before(&items[i].CreationTimestamp)
	})
	return items, nil
}

// recordLastSuccess surfaces when this schedule last actually worked.
func (r *TenantExportScheduleReconciler) recordLastSuccess(
	schedule *gentianov1alpha1.TenantExportSchedule,
	exports []gentianov1alpha1.TenantExport,
) {
	for _, export := range exports {
		if export.Status.Phase == gentianov1alpha1.TenantExportPhaseReady && export.Status.CompletedAt != nil {
			if schedule.Status.LastSuccessfulTime == nil ||
				schedule.Status.LastSuccessfulTime.Before(export.Status.CompletedAt) {
				schedule.Status.LastSuccessfulTime = export.Status.CompletedAt
			}
		}
	}
}

func firstRunning(exports []gentianov1alpha1.TenantExport) string {
	for _, export := range exports {
		if !export.IsTerminal() {
			return export.Name
		}
	}
	return ""
}

func scheduledExportName(scheduleName string, at time.Time) string {
	return fmt.Sprintf("%s-%s", scheduleName, at.UTC().Format("20060102-1504"))
}

// requeueUntil bounds how long the controller sleeps between checks.
//
// Capped so a schedule far in the future still reconciles occasionally —
// retention and the last-success readout stay current between firings.
func requeueUntil(now, next time.Time) time.Duration {
	wait := next.Sub(now)
	if wait <= 0 {
		return time.Minute
	}
	if wait > 10*time.Minute {
		return 10 * time.Minute
	}
	return wait
}

func (r *TenantExportScheduleReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now().UTC()
}

func setScheduleCondition(
	schedule *gentianov1alpha1.TenantExportSchedule,
	status metav1.ConditionStatus,
	reason, message string,
) {
	now := metav1.Now()
	for i := range schedule.Status.Conditions {
		c := &schedule.Status.Conditions[i]
		if c.Type != conditionScheduleReady {
			continue
		}
		if c.Status != status {
			c.LastTransitionTime = now
		}
		c.Status, c.Reason, c.Message = status, reason, message
		c.ObservedGeneration = schedule.Generation
		return
	}
	schedule.Status.Conditions = append(schedule.Status.Conditions, metav1.Condition{
		Type:               conditionScheduleReady,
		Status:             status,
		Reason:             reason,
		Message:            message,
		LastTransitionTime: now,
		ObservedGeneration: schedule.Generation,
	})
}

func (r *TenantExportScheduleReconciler) persist(
	ctx context.Context,
	schedule *gentianov1alpha1.TenantExportSchedule,
) error {
	schedule.Status.ObservedGeneration = schedule.Generation
	return r.Status().Update(ctx, schedule)
}

func (r *TenantExportScheduleReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&gentianov1alpha1.TenantExportSchedule{}).
		Owns(&gentianov1alpha1.TenantExport{}).
		Named("tenantexportschedule").
		Complete(r)
}
