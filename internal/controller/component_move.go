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
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/profilebundle"
)

// A move is the one guarded operation that takes an installed app from the
// build it runs to the build it is pinned to (docs/plans/versioning.md §5).
// The pin is git's: the director commits it, the tenant reconciler copies it
// onto the Component. What this file decides is WHEN the pinned build is
// rendered, and what happens when it does not come up:
//
//  1. The app's data is exported first, to the platform's own storage, and
//     the instance keeps running the build it ran until the export is Ready.
//     An export that fails ends the move (Failed) with nothing changed.
//  2. The pinned build is rolled out and, once its release is ready, the
//     profile's verification command is run in the app's pod, the one the
//     restore runs (spec.backup.restore.verify).
//  3. A build that does not come up within the deadline, or does not verify,
//     is rolled back: the instance is rendered from the build it ran, which
//     the cluster still holds as a ProfileBuild, and the reason is recorded.
//     It stays there until a later request (a newer MovedAt) asks again.
//
// The record of all this is status.rollout: the build the instance runs,
// the target, the phase and why. The pin says what was asked; the rollout
// says what is.

const (
	// moveDeadline is how long the pinned build has to come up before the
	// move is rolled back. Longer than the release's own wait (10 minutes),
	// so a slow first boot is not mistaken for a broken build.
	moveDeadline = 15 * time.Minute
	// moveExportRequeue is how often an export under way is looked at.
	moveExportRequeue = 30 * time.Second

	rolloutReasonExportFailed   = "ExportFailed"
	rolloutReasonNotReadyInTime = "NotReadyInTime"
	rolloutReasonVerifyFailed   = "VerifyFailed"

	conditionReasonExporting  = "Exporting"
	conditionReasonRolledBack = "RolledBack"
	conditionReasonMoveFailed = "MoveFailed"
)

// +kubebuilder:rbac:groups=gentianos.io,resources=tenantexports,verbs=get;list;watch;create

// moveExportName names the export taken before a move: the app, the build it
// moves to and the request, short enough for an object's name. The request
// is in the name so that each one exports afresh: a retry that found the
// export of the attempt before would stop on its failure, or roll out on a
// copy of data older than what the app holds now.
func moveExportName(component, digest, movedAt string) string {
	app := component
	if len(app) > 33 {
		app = app[:33]
	}
	request := sha256.Sum256([]byte(movedAt))
	return "move-" + app + "-" + strings.TrimPrefix(profilebundle.Short(digest), "sha256:") + "-" + hex.EncodeToString(request[:3])
}

// beginMove decides which build this pass renders: the pinned one, or the one
// the instance runs while a move waits for its export or has been rolled
// back. It moves the rollout record through the phases that happen before
// anything is rendered. stop is non-nil when the pass ends here, with the
// status written.
func (r *ComponentReconciler) beginMove(ctx context.Context, comp *gentianov1alpha1.Component) (effective string, stop *ctrl.Result, err error) {
	requested := comp.Spec.ProfileRef.Digest
	ro := comp.Status.Rollout
	if requested == "" || ro == nil || ro.Digest == "" || ro.Digest == requested {
		// Unpinned, not rolled out yet, or already on the pinned build.
		return requested, nil, nil
	}
	logger := log.FromContext(ctx)
	// A move: from ro.Digest to requested.
	newTarget := ro.Target != requested || ro.Phase == ""
	retried := (ro.Phase == gentianov1alpha1.RolloutRolledBack || ro.Phase == gentianov1alpha1.RolloutFailed) &&
		comp.Spec.MovedAt != "" && comp.Spec.MovedAt != ro.MovedAt
	if newTarget || retried {
		comp.Status.Rollout = &gentianov1alpha1.RolloutStatus{
			Digest: ro.Digest, Previous: ro.Previous, Target: requested,
			Phase:   gentianov1alpha1.RolloutExporting,
			Export:  moveExportName(comp.Name, requested, comp.Spec.MovedAt),
			MovedAt: comp.Spec.MovedAt, MovedBy: comp.Spec.MovedBy,
		}
		if err := r.ensureMoveExport(ctx, comp); err != nil {
			return "", nil, err
		}
		logger.Info("move started: exporting before the pinned build rolls out",
			"component", comp.Name, "namespace", comp.Namespace, "from", profilebundle.Short(ro.Digest), "to", profilebundle.Short(requested))
		res, err := r.status(ctx, comp, metav1.ConditionFalse, conditionReasonExporting, fmt.Sprintf(
			"moving from %s to %s: exporting the app's data first (TenantExport %s); the instance still runs %s",
			profilebundle.Short(ro.Digest), profilebundle.Short(requested), comp.Status.Rollout.Export, profilebundle.Short(ro.Digest)), moveExportRequeue)
		return ro.Digest, &res, err
	}
	switch ro.Phase {
	case gentianov1alpha1.RolloutExporting:
		export := &gentianov1alpha1.TenantExport{}
		if err := r.Get(ctx, types.NamespacedName{Name: ro.Export, Namespace: comp.Namespace}, export); err != nil {
			if !errors.IsNotFound(err) {
				return "", nil, err
			}
			if err := r.ensureMoveExport(ctx, comp); err != nil {
				return "", nil, err
			}
			res, err := r.status(ctx, comp, metav1.ConditionFalse, conditionReasonExporting,
				"moving to "+profilebundle.Short(requested)+": waiting for the export before it", moveExportRequeue)
			return ro.Digest, &res, err
		}
		switch export.Status.Phase {
		case gentianov1alpha1.TenantExportPhaseReady:
			now := metav1.Now()
			ro.Phase = gentianov1alpha1.RolloutRolling
			ro.StartedAt = &now
			if err := r.Status().Update(ctx, comp); err != nil {
				return "", nil, err
			}
			logger.Info("move: the export is ready; the pinned build rolls out",
				"component", comp.Name, "namespace", comp.Namespace, "to", profilebundle.Short(requested))
			return requested, nil, nil
		case gentianov1alpha1.TenantExportPhaseFailed:
			now := metav1.Now()
			ro.Phase = gentianov1alpha1.RolloutFailed
			ro.Reason = rolloutReasonExportFailed
			ro.Message = "the export before the move failed: " + exportFailure(export)
			ro.FinishedAt = &now
			res, err := r.status(ctx, comp, metav1.ConditionFalse, conditionReasonMoveFailed, fmt.Sprintf(
				"the move to %s did not start: %s; the instance still runs %s",
				profilebundle.Short(requested), ro.Message, profilebundle.Short(ro.Digest)), 0)
			return ro.Digest, &res, err
		default:
			res, err := r.status(ctx, comp, metav1.ConditionFalse, conditionReasonExporting, fmt.Sprintf(
				"moving to %s: the export before it is %s", profilebundle.Short(requested), orPending(string(export.Status.Phase))), moveExportRequeue)
			return ro.Digest, &res, err
		}
	case gentianov1alpha1.RolloutRolling:
		return requested, nil, nil
	default:
		// RolledBack or Failed, and no later request: the instance runs the
		// build it ran, rendered as before so that it is kept whole.
		return ro.Digest, nil, nil
	}
}

// finishMove ends a pass for a pinned instance: records the first build that
// rolled out, verifies a build that is rolling and ends the move one way or
// the other, and says so on an instance left on its old build. handled is
// true when the status has been written here.
func (r *ComponentReconciler) finishMove(
	ctx context.Context, comp *gentianov1alpha1.Component, profile *gentianov1alpha1.ComponentProfile,
	requested, effective string, releaseReady bool,
) (handled bool, res ctrl.Result, err error) {
	if requested == "" {
		return false, ctrl.Result{}, nil
	}
	ro := comp.Status.Rollout
	now := metav1.Now()
	if ro == nil || ro.Digest == "" {
		// The first build: recorded once it is up, with nothing to go back to.
		if releaseReady {
			comp.Status.Rollout = &gentianov1alpha1.RolloutStatus{
				Digest: requested, Target: requested, Phase: gentianov1alpha1.RolloutDone,
				MovedAt: comp.Spec.MovedAt, MovedBy: comp.Spec.MovedBy, FinishedAt: &now,
			}
		}
		return false, ctrl.Result{}, nil
	}
	logger := log.FromContext(ctx)
	switch {
	case ro.Phase == gentianov1alpha1.RolloutRolling && effective == requested:
		if !releaseReady {
			if ro.StartedAt != nil && time.Since(ro.StartedAt.Time) > moveDeadline {
				return r.rollBack(ctx, comp, rolloutReasonNotReadyInTime,
					fmt.Sprintf("the build did not come up within %s", moveDeadline))
			}
			return false, ctrl.Result{}, nil
		}
		if verify := verifyCommand(profile); len(verify) > 0 {
			out, err := r.runAppCommand(ctx, comp, profile, verify)
			if err != nil {
				return r.rollBack(ctx, comp, rolloutReasonVerifyFailed,
					"the verification command failed: "+strings.TrimSpace(err.Error()+" "+clipOutput(out)))
			}
		}
		ro.Previous, ro.Digest = ro.Digest, requested
		ro.Phase = gentianov1alpha1.RolloutDone
		ro.Reason, ro.Message = "", ""
		ro.FinishedAt = &now
		logger.Info("move done: the pinned build is up and verified",
			"component", comp.Name, "namespace", comp.Namespace, "build", profilebundle.Short(requested), "from", profilebundle.Short(ro.Previous))
		if r.Recorder != nil {
			r.Recorder.Eventf(comp, corev1.EventTypeNormal, "Moved", "moved from %s to %s", profilebundle.Short(ro.Previous), profilebundle.Short(requested))
		}
		return false, ctrl.Result{}, nil
	case (ro.Phase == gentianov1alpha1.RolloutRolledBack || ro.Phase == gentianov1alpha1.RolloutFailed) && effective == ro.Digest:
		if !releaseReady {
			return false, ctrl.Result{}, nil
		}
		reason := conditionReasonRolledBack
		what := "was rolled back"
		if ro.Phase == gentianov1alpha1.RolloutFailed {
			reason, what = conditionReasonMoveFailed, "did not start"
		}
		res, err := r.status(ctx, comp, metav1.ConditionTrue, reason, fmt.Sprintf(
			"runs %s; the move to %s %s: %s. Install the build again to try once more",
			profilebundle.Short(ro.Digest), profilebundle.Short(ro.Target), what, ro.Message), 0)
		return true, res, err
	}
	return false, ctrl.Result{}, nil
}

// rollBack ends a move on the build the instance ran: the next pass renders
// it, and the record says why.
func (r *ComponentReconciler) rollBack(ctx context.Context, comp *gentianov1alpha1.Component, reason, message string) (bool, ctrl.Result, error) {
	ro := comp.Status.Rollout
	now := metav1.Now()
	ro.Phase = gentianov1alpha1.RolloutRolledBack
	ro.Reason, ro.Message = reason, message
	ro.FinishedAt = &now
	log.FromContext(ctx).Info("move rolled back", "component", comp.Name, "namespace", comp.Namespace,
		"to", profilebundle.Short(ro.Target), "back_on", profilebundle.Short(ro.Digest), "reason", reason, "detail", message)
	if r.Recorder != nil {
		r.Recorder.Eventf(comp, corev1.EventTypeWarning, conditionReasonRolledBack, "the move to %s was rolled back: %s", profilebundle.Short(ro.Target), message)
	}
	res, err := r.status(ctx, comp, metav1.ConditionFalse, conditionReasonRolledBack, fmt.Sprintf(
		"the move to %s was rolled back (%s); putting %s back", profilebundle.Short(ro.Target), message, profilebundle.Short(ro.Digest)), time.Second)
	return true, res, err
}

// ensureMoveExport creates the export a move takes first, once.
func (r *ComponentReconciler) ensureMoveExport(ctx context.Context, comp *gentianov1alpha1.Component) error {
	export := &gentianov1alpha1.TenantExport{
		ObjectMeta: metav1.ObjectMeta{
			Name:      comp.Status.Rollout.Export,
			Namespace: comp.Namespace,
			Labels:    map[string]string{componentLabel: comp.Name, "gentianos.io/export-reason": "move"},
		},
		Spec: gentianov1alpha1.TenantExportSpec{
			Apps: []string{comp.Name},
			// The platform's own storage whatever the tenant's policy says: a
			// backup taken just before a risky operation.
			Destination: &gentianov1alpha1.ExportDestination{Mode: gentianov1alpha1.ExportDestinationPlatform},
		},
	}
	if err := controllerutil.SetControllerReference(comp, export, r.Scheme); err != nil {
		return err
	}
	if err := r.Create(ctx, export); err != nil && !errors.IsAlreadyExists(err) {
		return fmt.Errorf("create the export before the move: %w", err)
	}
	return nil
}

// verifyCommand is the profile's verification command, the one the restore
// runs after it has put data back; "" where the profile declares none.
func verifyCommand(profile *gentianov1alpha1.ComponentProfile) []string {
	if profile == nil || profile.Spec.Backup == nil {
		return nil
	}
	_, verify := profile.Spec.Backup.RestoreCommands()
	return verify
}

// runAppCommand runs one of the profile's commands in the app's own pod, as
// the restore does, for at most AppCommandTimeout.
func (r *ComponentReconciler) runAppCommand(ctx context.Context, comp *gentianov1alpha1.Component, profile *gentianov1alpha1.ComponentProfile, argv []string) (string, error) {
	if r.Exec == nil {
		return "", fmt.Errorf("exec is not configured on this operator")
	}
	container := profile.Spec.Backup.QuiesceContainer()
	pod, err := runningPodIn(ctx, r.Client, comp.Namespace, comp.Name, container)
	if err != nil {
		return "", err
	}
	if container == "" && len(pod.Spec.Containers) > 0 {
		container = pod.Spec.Containers[0].Name
	}
	timeout := r.ExecTimeout
	if timeout <= 0 {
		timeout = AppCommandTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return r.Exec.Exec(ctx, pod.Namespace, pod.Name, container, argv)
}

func exportFailure(export *gentianov1alpha1.TenantExport) string {
	for _, c := range export.Status.Conditions {
		if c.Status == metav1.ConditionFalse && c.Message != "" {
			return c.Message
		}
	}
	return "see the TenantExport's status"
}

func orPending(phase string) string {
	if phase == "" {
		return "pending"
	}
	return strings.ToLower(phase)
}

func clipOutput(out string) string {
	out = strings.TrimSpace(out)
	if len(out) > 300 {
		return out[:300] + "…"
	}
	return out
}
