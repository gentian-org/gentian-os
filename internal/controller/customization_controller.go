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

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/customization"
)

const (
	// conditionCustomizationValid reports whether the record satisfies the ladder
	// policy rules (cost matrix, upstream-first, justification chain).
	conditionCustomizationValid = "Valid"
	// conditionCustomizationCurrent reports whether the record is still within its
	// review window and still matches the target it was written against.
	conditionCustomizationCurrent = "Current"

	// customizationResyncInterval re-evaluates records so a review date passing, or
	// an upstream release landing, changes status without an edit.
	customizationResyncInterval = 12 * time.Hour
)

// CustomizationReconciler computes the derived state behind the customization debt
// report: whether a record is overdue for review, whether the app it targets has
// moved underneath it, and whether a cheaper rung has since become available.
//
// The report is deliberately computed here rather than by a CI script over YAML:
// the interesting signals — has the chart version drifted past what this record was
// tested against, does the target still exist — are properties of the live cluster,
// not of the file.
//
// +kubebuilder:rbac:groups=gentianos.io,resources=customizations,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=gentianos.io,resources=customizations/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=gentianos.io,resources=appprofiles,verbs=get;list;watch
type CustomizationReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// Now is injectable so review-date behaviour is testable.
	Now func() time.Time
}

// SetupWithManager registers the Customization controller. AppProfile changes are
// mapped back to the records that target them, so bumping a chart version
// immediately re-evaluates every customization riding on it.
func (r *CustomizationReconciler) SetupWithManager(mgr ctrl.Manager) error {
	mapProfileToRecords := func(ctx context.Context, obj client.Object) []reconcile.Request {
		profile, ok := obj.(*gentianov1alpha1.AppProfile)
		if !ok {
			return nil
		}
		var records gentianov1alpha1.CustomizationList
		if err := r.List(ctx, &records); err != nil {
			return nil
		}
		var requests []reconcile.Request
		for i := range records.Items {
			record := &records.Items[i]
			if record.Spec.Target.Profile != profile.Name {
				continue
			}
			requests = append(requests, reconcile.Request{
				NamespacedName: client.ObjectKeyFromObject(record),
			})
		}
		return requests
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&gentianov1alpha1.Customization{}).
		Watches(
			&gentianov1alpha1.AppProfile{},
			handler.EnqueueRequestsFromMapFunc(mapProfileToRecords),
		).
		Complete(r)
}

// Reconcile evaluates one Customization record and writes its derived status.
func (r *CustomizationReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var record gentianov1alpha1.Customization
	if err := r.Get(ctx, req.NamespacedName, &record); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	profile, profileFound, err := r.resolveTargetProfile(ctx, record.Spec.Target.Profile)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("resolve target profile: %w", err)
	}

	status := r.evaluate(&record, profile, profileFound, r.ladderSurfaceFor(ctx, profile))
	if statusUnchanged(record.Status, status) {
		return ctrl.Result{RequeueAfter: customizationResyncInterval}, nil
	}

	record.Status = status
	record.Status.ObservedGeneration = record.Generation
	if err := r.Status().Update(ctx, &record); err != nil {
		return ctrl.Result{}, fmt.Errorf("update Customization status: %w", err)
	}
	logger.Info("reconciled customization record",
		"name", record.Name, "rung", record.Spec.Rung, "phase", record.Status.Phase)

	return ctrl.Result{RequeueAfter: customizationResyncInterval}, nil
}

func (r *CustomizationReconciler) resolveTargetProfile(
	ctx context.Context,
	name string,
) (*gentianov1alpha1.AppProfile, bool, error) {
	if name == "" {
		return nil, false, nil
	}
	var profile gentianov1alpha1.AppProfile
	err := r.Get(ctx, client.ObjectKey{Name: name}, &profile)
	if errors.IsNotFound(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return &profile, true, nil
}

// ladderSurfaceFor returns the customization surface that governs this profile.
//
// An addon inherits its base's ladder. It is activation state inside the base —
// same image, same drop-in directories, same plugin API — so grading it separately
// would fork a mutable fact, which is why addon profiles deliberately carry only
// spec.customization.addon and never restate grade or supportedRungs.
//
// Reading spec.customization directly therefore saw no supportedRungs on an addon
// and fell back to the [L0, L4] default, so a record targeting one was rejected as
// "target app does not declare support for rung L3" for an app whose base supports
// exactly that. CI resolved the inheritance and the operator did not, so the two
// disagreed about the same record.
//
// Editions are not addons and inherit nothing: nextcloud-base-od shares a family
// name with nextcloud-base-ce and no artifact, so it carries its own declaration.
func (r *CustomizationReconciler) ladderSurfaceFor(
	ctx context.Context,
	profile *gentianov1alpha1.AppProfile,
) *gentianov1alpha1.CustomizationSurface {
	if profile == nil || profile.Spec.Customization == nil {
		return nil
	}
	surface := profile.Spec.Customization
	addon := surface.Addon
	if addon == nil || addon.Of == "" {
		return surface
	}
	base, found, err := r.resolveTargetProfile(ctx, addon.Of)
	if err != nil || !found || base.Spec.Customization == nil {
		// The base is missing or uncharacterised. Fall through to the addon's own
		// surface rather than inventing one; the default applies as it would for
		// any uncharacterised app.
		return surface
	}
	return base.Spec.Customization
}

// evaluate computes the full derived status for a record.
func (r *CustomizationReconciler) evaluate(
	record *gentianov1alpha1.Customization,
	profile *gentianov1alpha1.AppProfile,
	profileFound bool,
	surface *gentianov1alpha1.CustomizationSurface,
) gentianov1alpha1.CustomizationStatus {
	status := gentianov1alpha1.CustomizationStatus{}

	// A record whose target no longer exists is dangling debt: something is being
	// maintained for an app nobody installs.
	if !profileFound {
		status.Phase = gentianov1alpha1.CustomizationPhaseInvalid
		setRecordCondition(&status, conditionCustomizationValid, metav1.ConditionFalse,
			"TargetNotFound",
			fmt.Sprintf("AppProfile %q does not exist", record.Spec.Target.Profile))
		return status
	}

	policyErrs := customization.ValidateRecord(record)
	if !customization.SupportsRung(surface, record.Spec.Rung) {
		policyErrs = append(policyErrs, fmt.Errorf(
			"target app does not declare support for rung %s", record.Spec.Rung))
	}

	if len(policyErrs) > 0 {
		status.Phase = gentianov1alpha1.CustomizationPhaseInvalid
		setRecordCondition(&status, conditionCustomizationValid, metav1.ConditionFalse,
			"PolicyViolation", joinErrors(policyErrs))
	} else {
		setRecordCondition(&status, conditionCustomizationValid, metav1.ConditionTrue,
			"PolicySatisfied", "Record satisfies the customization ladder policy")
	}

	status.ReviewOverdue = r.reviewOverdue(record)
	status.UpstreamStale = upstreamStale(record)
	status.TargetVersionDrift = targetVersionDrift(record, profile)
	status.RungAboveRecommended = rungAboveRecommended(record, surface)

	// Upstream taking the change is the outcome the framework is designed to
	// produce — surface it as its own phase so it stands out in the debt report.
	if record.Spec.UpstreamFirst != nil && record.Spec.UpstreamFirst.AppliedUpstream {
		status.Phase = gentianov1alpha1.CustomizationPhaseSuperseded
		setRecordCondition(&status, conditionCustomizationCurrent, metav1.ConditionFalse,
			"AppliedUpstream", "Upstream has accepted this change — schedule removal of the local delta")
		return status
	}

	reasons := currentReasons(status)
	if len(reasons) > 0 {
		if status.Phase == "" {
			status.Phase = gentianov1alpha1.CustomizationPhaseNeedsReview
		}
		setRecordCondition(&status, conditionCustomizationCurrent, metav1.ConditionFalse,
			"NeedsReview", strings.Join(reasons, "; "))
		return status
	}

	if status.Phase == "" {
		status.Phase = gentianov1alpha1.CustomizationPhaseActive
	}
	setRecordCondition(&status, conditionCustomizationCurrent, metav1.ConditionTrue,
		"Current", "Within review window and matching the target as pinned")
	return status
}

func (r *CustomizationReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// reviewOverdue reports whether the record's review date has passed. P5 makes
// descending an obligation, and an unreviewed record is how that obligation is
// quietly dropped.
func (r *CustomizationReconciler) reviewOverdue(record *gentianov1alpha1.Customization) bool {
	if record.Spec.ReviewBy == "" {
		return false
	}
	reviewBy, err := parseLadderDate(record.Spec.ReviewBy)
	if err != nil {
		// An unparseable date is caught by CRD pattern validation; treat it as
		// overdue rather than silently ignoring it.
		return true
	}
	return r.now().After(reviewBy)
}

// parseLadderDate parses a Customization date field. The CRD pattern accepts
// both a bare date and the midnight-UTC RFC3339 form YAML parsers commonly
// normalize an unquoted date scalar into on the way to JSON — see the field
// comments on CustomizationSpec.ReviewBy/Created.
func parseLadderDate(value string) (time.Time, error) {
	if t, err := time.Parse("2006-01-02", value); err == nil {
		return t, nil
	}
	return time.Parse(time.RFC3339, value)
}

// upstreamStale reports a broken upstream-first obligation: either the change was
// never offered upstream and no reason was recorded, or upstream has taken it and
// the local delta is still being carried.
func upstreamStale(record *gentianov1alpha1.Customization) bool {
	if !customization.AtOrAbove(record.Spec.Rung, gentianov1alpha1.RungRepackage) {
		return false
	}
	uf := record.Spec.UpstreamFirst
	if uf == nil || !uf.Attempted {
		return true
	}
	if uf.Forwarded == gentianov1alpha1.CustomizationForwarded("no") &&
		strings.TrimSpace(uf.Reason) == "" {
		return true
	}
	return uf.AppliedUpstream
}

// targetVersionDrift reports that the profile now pins a version this record was
// never tested against — the upgrade blast radius signal.
func targetVersionDrift(record *gentianov1alpha1.Customization, profile *gentianov1alpha1.AppProfile) bool {
	if profile == nil || len(record.Spec.TestedAgainst) == 0 {
		return false
	}
	current := profile.Spec.Chart.Version
	if current == "" {
		return false
	}
	if record.Spec.Target.ChartVersion != "" && record.Spec.Target.ChartVersion != current {
		return true
	}
	return false
}

// rungAboveRecommended reports that the target supports a cheaper rung the
// record itself never accounted for — a concrete, actionable "you could
// descend" signal, which is the only way the debt trend goes down.
//
// Merely supporting a cheaper mechanism is not enough to flag: admission
// already requires spec.rungJustification to explain, per rung, why every
// rung below the chosen one was rejected (see policy.go). A record that
// passed validation has therefore already considered every cheaper rung the
// app supported *at authoring time* — re-flagging those would make every
// well-justified L3+ record against a grade-A app permanently "flagged" and
// drown the one case this exists to catch: a rung the app has started
// supporting *since* the record was written, which the justification map
// necessarily has no entry for.
func rungAboveRecommended(
	record *gentianov1alpha1.Customization,
	surface *gentianov1alpha1.CustomizationSurface,
) bool {
	chosen, ok := customization.RungIndex(record.Spec.Rung)
	if !ok || chosen == 0 {
		return false
	}
	for _, candidate := range []gentianov1alpha1.CustomizationRung{
		gentianov1alpha1.RungConfigure,
		gentianov1alpha1.RungDropIn,
		gentianov1alpha1.RungExtension,
	} {
		idx, known := customization.RungIndex(candidate)
		if !known || idx >= chosen {
			continue
		}
		if !customization.SupportsRung(surface, candidate) {
			continue
		}
		if _, justified := record.Spec.RungJustification[string(candidate)]; justified {
			continue
		}
		return true
	}
	return false
}

func currentReasons(status gentianov1alpha1.CustomizationStatus) []string {
	var reasons []string
	if status.ReviewOverdue {
		reasons = append(reasons, "review date has passed")
	}
	if status.UpstreamStale {
		reasons = append(reasons, "upstream-first obligation unmet or superseded")
	}
	if status.TargetVersionDrift {
		reasons = append(reasons, "target pins a version outside testedAgainst")
	}
	if status.RungAboveRecommended {
		reasons = append(reasons, "target now supports a cheaper rung")
	}
	return reasons
}

func setRecordCondition(
	status *gentianov1alpha1.CustomizationStatus,
	condType string,
	state metav1.ConditionStatus,
	reason, message string,
) {
	condition := metav1.Condition{
		Type:               condType,
		Status:             state,
		Reason:             reason,
		Message:            message,
		LastTransitionTime: metav1.Now(),
	}
	for i := range status.Conditions {
		if status.Conditions[i].Type != condType {
			continue
		}
		if status.Conditions[i].Status == state {
			condition.LastTransitionTime = status.Conditions[i].LastTransitionTime
		}
		status.Conditions[i] = condition
		return
	}
	status.Conditions = append(status.Conditions, condition)
}

// statusUnchanged compares the fields that matter, ignoring condition timestamps so
// an unchanged record does not generate a write on every resync.
func statusUnchanged(oldStatus, newStatus gentianov1alpha1.CustomizationStatus) bool {
	if oldStatus.Phase != newStatus.Phase ||
		oldStatus.ReviewOverdue != newStatus.ReviewOverdue ||
		oldStatus.UpstreamStale != newStatus.UpstreamStale ||
		oldStatus.TargetVersionDrift != newStatus.TargetVersionDrift ||
		oldStatus.RungAboveRecommended != newStatus.RungAboveRecommended ||
		len(oldStatus.Conditions) != len(newStatus.Conditions) {
		return false
	}
	oldConds := conditionSummary(oldStatus.Conditions)
	newConds := conditionSummary(newStatus.Conditions)
	for i := range oldConds {
		if oldConds[i] != newConds[i] {
			return false
		}
	}
	return true
}

func conditionSummary(conditions []metav1.Condition) []string {
	summary := make([]string, 0, len(conditions))
	for _, condition := range conditions {
		summary = append(summary, fmt.Sprintf("%s/%s/%s/%s",
			condition.Type, condition.Status, condition.Reason, condition.Message))
	}
	sort.Strings(summary)
	return summary
}

func joinErrors(errs []error) string {
	messages := make([]string, 0, len(errs))
	for _, err := range errs {
		messages = append(messages, err.Error())
	}
	sort.Strings(messages)
	return strings.Join(messages, "; ")
}
