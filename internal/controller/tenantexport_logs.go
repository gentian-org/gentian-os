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
	"io"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// failureLogLines is how much of a failed container's output is kept.
	//
	// The last few lines, because these scripts run with `set -eu` and die on
	// the command that failed, so the error is at the end. Enough to carry an
	// S3 error and the line before it; not so much that a status field becomes
	// a log viewer.
	failureLogLines = 12

	// failureMessageLimit bounds what reaches status. A status field is read in
	// a console and in `kubectl describe`, and etcd holds every revision of it.
	failureMessageLimit = 1024
)

// PodLogTailer reads the tail of one container's log.
//
// An interface because the controller-runtime client cannot: logs are a
// subresource served as a stream, not an object, so this needs a clientset —
// and a clientset is the one thing the reconciler suites do not have.
type PodLogTailer interface {
	Tail(ctx context.Context, namespace, pod, container string, lines int64) (string, error)
}

// ClientsetLogTailer is the real implementation.
type ClientsetLogTailer struct{ Clientset kubernetes.Interface }

func (t ClientsetLogTailer) Tail(
	ctx context.Context,
	namespace, pod, container string,
	lines int64,
) (string, error) {
	req := t.Clientset.CoreV1().Pods(namespace).GetLogs(pod, &corev1.PodLogOptions{
		Container: container,
		TailLines: &lines,
	})
	stream, err := req.Stream(ctx)
	if err != nil {
		return "", err
	}
	defer func() { _ = stream.Close() }()

	// Bounded read. A container that logged a gigabyte before failing must not
	// be able to make the operator hold it.
	body, err := io.ReadAll(io.LimitReader(stream, 64*1024))
	if err != nil {
		return "", err
	}
	return string(body), nil
}

// captureFailureReason finds what a failed capture Job's pod actually said.
//
// Best effort throughout. Every branch that cannot answer returns empty rather
// than an error: this runs while a failure is already being recorded, and a
// diagnosis that fails to be collected must not replace the failure it was
// meant to explain.
func (r *TenantExportReconciler) captureFailureReason(
	ctx context.Context,
	namespace, jobName string,
) string {
	if r.LogTailer == nil {
		return ""
	}

	pods := &corev1.PodList{}
	if err := r.List(ctx, pods,
		client.InNamespace(namespace),
		client.MatchingLabels{"job-name": jobName},
	); err != nil {
		return ""
	}

	for i := range pods.Items {
		pod := &pods.Items[i]
		container := firstFailedContainer(pod)
		if container == "" {
			continue
		}
		out, err := r.LogTailer.Tail(ctx, namespace, pod.Name, container, failureLogLines)
		if err != nil {
			continue
		}
		if trimmed := tidyFailureOutput(out); trimmed != "" {
			return fmt.Sprintf("%s: %s", container, trimmed)
		}
	}
	return ""
}

// firstFailedContainer names the container that ended non-zero.
//
// Init containers first and in order, because these Jobs put the work in them —
// dump, then encrypt — and the first one to fail is the one that explains the
// rest. A later container in a pod whose init failed never ran at all, so its
// empty log would be a confident answer about nothing.
func firstFailedContainer(pod *corev1.Pod) string {
	for _, s := range pod.Status.InitContainerStatuses {
		if failed(s) {
			return s.Name
		}
	}
	for _, s := range pod.Status.ContainerStatuses {
		if failed(s) {
			return s.Name
		}
	}
	return ""
}

func failed(s corev1.ContainerStatus) bool {
	if s.State.Terminated != nil && s.State.Terminated.ExitCode != 0 {
		return true
	}
	// A container in back-off has a terminated *last* state and is waiting to
	// try again. That is the state these pods are usually observed in, because
	// the Job's backoffLimit is what finally fails them.
	return s.LastTerminationState.Terminated != nil &&
		s.LastTerminationState.Terminated.ExitCode != 0
}

// tidyFailureOutput reduces a log tail to something a status field can carry.
//
// Blank lines and shell trace noise go; the last lines survive, because `set -e`
// means the failure is at the end. Truncated from the front rather than the
// back for the same reason — the last thing said is the thing that failed.
func tidyFailureOutput(out string) string {
	var kept []string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimRight(line, " \t\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		kept = append(kept, line)
	}
	if len(kept) == 0 {
		return ""
	}
	joined := strings.Join(kept, " | ")
	if len(joined) > failureMessageLimit {
		joined = "…" + joined[len(joined)-failureMessageLimit:]
	}
	return joined
}

// capturePodStartDeadline bounds how long a capture pod may fail to start.
//
// Not a bound on the capture, which may legitimately run for hours and is
// deliberately excluded from the provisioning Jobs' one-hour ceiling. This
// bounds a pod that has not begun: no container has run, so nothing is being
// copied and no time is being usefully spent.
//
// Generous, because a cold image pull on a busy node is minutes. Short enough
// that an app is not held offline for an afternoon by a pod that will never
// start — which is what happened: a volume already attached elsewhere gave
// "Multi-Attach error", the pod stayed Pending for ever, and a pod that never
// starts never fails, so nothing counted it and the export stayed Running.
const capturePodStartDeadline = 8 * time.Minute

// stuckCapture reports why a capture Job's pod has not begun, once it has had
// long enough that not beginning is the answer.
//
// Empty while there is still reason to wait, so the caller treats it as an
// ordinary in-progress capture.
func (r *TenantExportReconciler) stuckCapture(ctx context.Context, job *batchv1.Job) string {
	pods := &corev1.PodList{}
	if err := r.List(ctx, pods,
		client.InNamespace(job.Namespace),
		client.MatchingLabels{"job-name": job.Name},
	); err != nil {
		return ""
	}

	for i := range pods.Items {
		pod := &pods.Items[i]
		if pod.Status.Phase != corev1.PodPending {
			continue
		}
		if anyContainerStarted(pod) {
			continue
		}
		if time.Since(pod.CreationTimestamp.Time) < capturePodStartDeadline {
			continue
		}
		return fmt.Sprintf("capture pod %s did not start within %s: %s",
			pod.Name, capturePodStartDeadline, podStallReason(pod))
	}
	return ""
}

// anyContainerStarted reports whether the pod ever got as far as running
// something. A container that has run and exited counts: the pod is making
// progress, or has already failed in a way the Job will report.
func anyContainerStarted(pod *corev1.Pod) bool {
	all := append(append([]corev1.ContainerStatus{},
		pod.Status.InitContainerStatuses...), pod.Status.ContainerStatuses...)
	for _, s := range all {
		if s.State.Running != nil || s.State.Terminated != nil ||
			s.LastTerminationState.Terminated != nil {
			return true
		}
	}
	return false
}

// podStallReason says what the pod is waiting on, in the words the cluster used.
//
// Conditions first, because an unschedulable pod says so there. Then the
// waiting container, which carries an image-pull failure. A pod that cannot
// attach a volume says neither — the attach lives in an event, which the
// operator has no permission to read — so that case falls through to a
// statement of what is true and a pointer at the usual cause.
func podStallReason(pod *corev1.Pod) string {
	for _, c := range pod.Status.Conditions {
		if c.Status == corev1.ConditionFalse && c.Message != "" {
			return fmt.Sprintf("%s: %s", c.Reason, c.Message)
		}
	}
	all := append(append([]corev1.ContainerStatus{},
		pod.Status.InitContainerStatuses...), pod.Status.ContainerStatuses...)
	for _, s := range all {
		if s.State.Waiting != nil && s.State.Waiting.Reason != "" {
			return fmt.Sprintf("%s waiting: %s %s",
				s.Name, s.State.Waiting.Reason, s.State.Waiting.Message)
		}
	}
	return "no container started; a ReadWriteOnce volume already attached to " +
		"another node is the usual cause"
}

// nodeHoldingClaim names the node a claim is currently attached to, via a pod
// that is running with it mounted.
//
// A ReadWriteOnce claim attaches to one node, and a pod running with it keeps it
// there. A capture that mounts the same claim must therefore run on that node or
// wait for ever, and waiting for ever is what it did.
//
// Only Running pods count. A Pending one proves nothing — it may be a previous
// capture attempt stuck against this very problem, and pinning to its node
// would copy the mistake forward.
func (r *TenantExportReconciler) nodeHoldingClaim(
	ctx context.Context,
	namespace, claim string,
) string {
	pods := &corev1.PodList{}
	if err := r.List(ctx, pods, client.InNamespace(namespace)); err != nil {
		return ""
	}
	for i := range pods.Items {
		pod := &pods.Items[i]
		if pod.Status.Phase != corev1.PodRunning || pod.Spec.NodeName == "" {
			continue
		}
		for _, v := range pod.Spec.Volumes {
			if v.PersistentVolumeClaim != nil && v.PersistentVolumeClaim.ClaimName == claim {
				return pod.Spec.NodeName
			}
		}
	}
	return ""
}
