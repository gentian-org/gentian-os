/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package dispatcher

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strconv"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// What a run's pod says about itself, beside the labels the network rules
// select by (table.go).
const (
	// LabelAttempt is which attempt of its run a pod is, from 1. The
	// platform makes one attempt; the label and the pod's name carry the
	// number so that a later attempt is another pod of the same run.
	LabelAttempt = "gentianos.io/run-attempt"
	// AnnotationID is the run's id as the caller gave it, AnnotationName
	// what it runs, and AnnotationPerson who the caller says it is for.
	AnnotationID     = "gentianos.io/run-id"
	AnnotationName   = "gentianos.io/run-name"
	AnnotationPerson = "gentianos.io/run-person"

	// ProgramVolume is where the run program is put in a run's pod, and
	// ProgramPath the program there.
	ProgramVolume = "/gentian-run"
	ProgramPath   = ProgramVolume + "/run"
	// ProgramSource is the run program in the platform's image.
	ProgramSource = "/run-program"

	// Container is the container a run's command runs in.
	Container = "run"

	// What a run is told in its environment.
	EnvID          = "RUN_ID"
	EnvAttempt     = "RUN_ATTEMPT"
	EnvName        = "RUN_NAME"
	EnvCallbackURL = "RUN_CALLBACK_URL"
	EnvToken       = "RUN_TOKEN" //nolint:gosec // An environment variable's name, not a credential.

	// MaxID is the longest id a caller may give a run.
	MaxID = 253
)

// The states of a run.
const (
	StateWorking   = "working"
	StateCompleted = "completed"
	StateFailed    = "failed"
)

// Hash is what a run is found by: its tenant, its caller and the caller's id
// for it, as a label's value. Two callers' runs never share one, whatever
// ids they choose.
func Hash(tenant, component, id string) string {
	sum := sha256.Sum256([]byte(tenant + "\x00" + component + "\x00" + id))
	return hex.EncodeToString(sum[:])[:32]
}

// PodName is the pod of one attempt of a run.
func PodName(hash string, attempt int) string { return fmt.Sprintf("run-%s-%d", hash, attempt) }

// Start is one run a caller asked for.
type Start struct {
	ID       string
	Deadline int32
	Token    string
	Person   string
	Attempt  int
}

// Pod is the pod of one attempt: the runnable's image at its pinned build,
// the account without rights, the labels the namespace's rules select by,
// and the run program around the runnable's command.
func Pod(tenant *Tenant, caller *Caller, runnable Runnable, own bool, start Start, programImage string) *corev1.Pod {
	hash := Hash(caller.Tenant, caller.Component, start.ID)
	labels := map[string]string{
		LabelRun: hash, LabelCaller: caller.Component, LabelAttempt: strconv.Itoa(start.Attempt),
		"gentianos.io/tenant": caller.Tenant,
	}
	if !own {
		labels[LabelAction] = runnable.Name
	}
	annotations := map[string]string{AnnotationID: start.ID, AnnotationName: runnable.Name}
	if start.Person != "" {
		annotations[AnnotationPerson] = start.Person
	}
	no, yes := false, true
	user := int64(65532)
	deadline := int64(start.Deadline)
	restricted := &corev1.SecurityContext{
		AllowPrivilegeEscalation: &no,
		Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
	}
	resources := corev1.ResourceRequirements{}
	if list := quantities(runnable); len(list) > 0 {
		resources.Requests, resources.Limits = list, list
	}
	program := corev1.VolumeMount{Name: "run-program", MountPath: ProgramVolume}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: PodName(hash, start.Attempt), Namespace: tenant.Namespace, Labels: labels, Annotations: annotations,
		},
		Spec: corev1.PodSpec{
			RestartPolicy:                corev1.RestartPolicyNever,
			ActiveDeadlineSeconds:        &deadline,
			ServiceAccountName:           ServiceAccount,
			AutomountServiceAccountToken: &no,
			EnableServiceLinks:           &no,
			SecurityContext: &corev1.PodSecurityContext{
				// One user without rights for every run, whatever the
				// image would have run as.
				RunAsNonRoot: &yes, RunAsUser: &user, RunAsGroup: &user, FSGroup: &user,
				SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
			},
			Volumes: []corev1.Volume{{Name: program.Name, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}},
			InitContainers: []corev1.Container{{
				Name: "run-program", Image: programImage,
				Command:         []string{ProgramSource, "--install", ProgramPath},
				VolumeMounts:    []corev1.VolumeMount{program},
				SecurityContext: restricted,
			}},
			Containers: []corev1.Container{{
				Name: Container, Image: runnable.Ref(),
				Command: append([]string{ProgramPath, "--"}, runnable.Command...),
				Env: []corev1.EnvVar{
					{Name: EnvID, Value: start.ID},
					{Name: EnvAttempt, Value: strconv.Itoa(start.Attempt)},
					{Name: EnvName, Value: runnable.Name},
					{Name: EnvCallbackURL, Value: caller.Callback.URL()},
					{Name: EnvToken, Value: start.Token},
				},
				Resources:       resources,
				VolumeMounts:    []corev1.VolumeMount{{Name: program.Name, MountPath: ProgramVolume, ReadOnly: true}},
				SecurityContext: restricted,
			}},
		},
	}
}

func quantities(runnable Runnable) corev1.ResourceList {
	out := corev1.ResourceList{}
	for name, text := range map[corev1.ResourceName]string{corev1.ResourceCPU: runnable.CPU, corev1.ResourceMemory: runnable.Memory} {
		if text == "" {
			continue
		}
		if q, err := resource.ParseQuantity(text); err == nil {
			out[name] = q
		}
	}
	return out
}

// Run is a run as a caller is told of it.
type Run struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	State   string `json:"state"`
	Attempt int    `json:"attempt"`
	// Reason says why a run failed: DeadlineExceeded, an exit code, or what
	// kept its pod from starting.
	Reason     string     `json:"reason,omitempty"`
	ExitCode   *int32     `json:"exitCode,omitempty"`
	StartedAt  *time.Time `json:"startedAt,omitempty"`
	FinishedAt *time.Time `json:"finishedAt,omitempty"`
}

// RunOf reads a run off the pod of its attempt.
func RunOf(pod *corev1.Pod) Run {
	attempt, _ := strconv.Atoi(pod.Labels[LabelAttempt])
	run := Run{ID: pod.Annotations[AnnotationID], Name: pod.Annotations[AnnotationName], State: StateWorking, Attempt: attempt}
	if pod.Status.StartTime != nil {
		started := pod.Status.StartTime.Time
		run.StartedAt = &started
	}
	var ended *corev1.ContainerStateTerminated
	for _, status := range pod.Status.ContainerStatuses {
		if status.Name == Container && status.State.Terminated != nil {
			ended = status.State.Terminated
		}
	}
	if ended != nil {
		finished := ended.FinishedAt.Time
		run.FinishedAt, run.ExitCode = &finished, &ended.ExitCode
	}
	switch pod.Status.Phase {
	case corev1.PodSucceeded:
		run.State = StateCompleted
	case corev1.PodFailed:
		run.State = StateFailed
		run.Reason = pod.Status.Reason
		if run.Reason == "" && ended != nil {
			run.Reason = ended.Reason
		}
		if run.Reason == "" {
			run.Reason = "Failed"
		}
	}
	return run
}

// finishedAt is when a pod's run ended, zero while it has not.
func finishedAt(pod *corev1.Pod) time.Time {
	if pod.Status.Phase != corev1.PodSucceeded && pod.Status.Phase != corev1.PodFailed {
		return time.Time{}
	}
	latest := pod.CreationTimestamp.Time
	for _, status := range pod.Status.ContainerStatuses {
		if ended := status.State.Terminated; ended != nil && ended.FinishedAt.After(latest) {
			latest = ended.FinishedAt.Time
		}
	}
	return latest
}

// latest is the pod of each run's last attempt, by the run's hash.
func latest(pods []corev1.Pod) []*corev1.Pod {
	by := map[string]*corev1.Pod{}
	for i := range pods {
		pod := &pods[i]
		hash := pod.Labels[LabelRun]
		a, _ := strconv.Atoi(pod.Labels[LabelAttempt])
		if have := by[hash]; have != nil {
			if b, _ := strconv.Atoi(have.Labels[LabelAttempt]); b >= a {
				continue
			}
		}
		by[hash] = pod
	}
	out := make([]*corev1.Pod, 0, len(by))
	for _, pod := range by {
		out = append(out, pod)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreationTimestamp.Before(&out[j].CreationTimestamp) })
	return out
}

// Sweep deletes, in the namespaces for runs the table names, the pods whose
// run ended longer ago than keep. A caller reads a finished run within that
// time; after it the pod is gone and the caller's own record is what there
// is.
func Sweep(ctx context.Context, pods PodClient, table *Table, keep time.Duration, now time.Time) (int, error) {
	if table == nil {
		return 0, nil
	}
	swept := 0
	for _, tenant := range table.Tenants {
		list, err := pods.List(ctx, tenant.Namespace, map[string]string{})
		if err != nil {
			return swept, err
		}
		for i := range list {
			pod := &list[i]
			if pod.Labels[LabelRun] == "" {
				continue
			}
			if ended := finishedAt(pod); !ended.IsZero() && now.Sub(ended) > keep {
				if err := pods.Delete(ctx, pod.Namespace, pod.Name); err != nil {
					return swept, err
				}
				swept++
			}
		}
	}
	return swept, nil
}
