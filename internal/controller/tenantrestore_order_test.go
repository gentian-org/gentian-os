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
	"errors"
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/gentian-org/gentian-os/api/bundle"
	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// orderExecer records each command a restore runs in the app, and whether
// the app's pods had already been started again when it ran.
type orderExecer struct {
	c               client.Client
	failWith        string
	calls           []string
	restartedBefore []bool
}

func (e *orderExecer) Exec(ctx context.Context, _, _, _ string, argv []string) (string, error) {
	last := argv[len(argv)-1]
	e.calls = append(e.calls, last)
	dep := deployment("wiki", "wiki", 0)
	if err := e.c.Get(ctx, types.NamespacedName{Name: "wiki", Namespace: "tenant-demo"}, dep); err == nil {
		_, restarted := dep.Spec.Template.Annotations[restartWorkloadAnnotation]
		e.restartedBefore = append(e.restartedBefore, restarted)
	}
	if e.failWith != "" && last == e.failWith {
		return "", errors.New("pod not ready")
	}
	return "", nil
}

// restoreWithCommands runs a restore of wiki whose profile declares backup,
// with a running pod and a Deployment for the app.
func restoreWithCommands(t *testing.T, backupSpec *gentianov1alpha1.BackupSpec, failWith string) (*gentianov1alpha1.TenantRestore, *orderExecer, *restoreWorld) {
	t.Helper()
	w := newRestoreWorld(t, nil)
	ctx := context.Background()
	profile := &gentianov1alpha1.ComponentProfile{}
	if err := w.c.Get(ctx, types.NamespacedName{Name: "wiki"}, profile); err != nil {
		t.Fatal(err)
	}
	profile.Spec.Backup = backupSpec
	if err := w.c.Update(ctx, profile); err != nil {
		t.Fatal(err)
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "wiki-0", Namespace: "tenant-demo", Labels: map[string]string{"gentianos.io/app": "wiki"}},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "wiki"}}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning,
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}},
	}
	for _, obj := range []client.Object{pod, deployment("wiki", "wiki", 1)} {
		if err := w.c.Create(ctx, obj); err != nil {
			t.Fatal(err)
		}
	}
	exec := &orderExecer{c: w.c, failWith: failWith}
	w.r.Tenant.Exec = exec
	return w.run(t), exec, w
}

// An app paused by scaling down is resumed before its post-restore
// commands, which need a pod; its pods started on the data as it was put
// back, before the commands rewrote what it remembers of where it ran. They
// are started again once the commands have run, and not before.
func TestAnAppResumedByScalingIsStartedAgainAfterItsPostRestoreCommands(t *testing.T) {
	got, exec, w := restoreWithCommands(t, &gentianov1alpha1.BackupSpec{
		Restore: &gentianov1alpha1.BackupRestore{Post: [][]string{{"wiki", "rewrite"}}},
	}, "")
	if got.Status.Phase != gentianov1alpha1.TenantExportPhaseReady {
		t.Fatalf("phase = %s: %+v", got.Status.Phase, got.Status.Conditions)
	}
	if !slices.Equal(exec.calls, []string{"rewrite"}) || !slices.Equal(exec.restartedBefore, []bool{false}) {
		t.Fatalf("calls = %v, restarted before each = %v", exec.calls, exec.restartedBefore)
	}
	dep := getDeployment(t, w.c, "wiki")
	if _, ok := dep.Spec.Template.Annotations[restartWorkloadAnnotation]; !ok {
		t.Error("the app was not started again after its post-restore commands")
	}
	if *dep.Spec.Replicas != 1 {
		t.Errorf("replicas = %d, want the app resumed", *dep.Spec.Replicas)
	}
}

// Without commands to run, an app resumed by scaling has new pods already
// and is not started again.
func TestAnAppResumedByScalingWithNoCommandsIsNotStartedAgain(t *testing.T) {
	_, exec, w := restoreWithCommands(t, nil, "")
	if len(exec.calls) != 0 {
		t.Fatalf("calls = %v", exec.calls)
	}
	if _, ok := getDeployment(t, w.c, "wiki").Spec.Template.Annotations[restartWorkloadAnnotation]; ok {
		t.Error("an app resumed by scaling, with nothing run in it, was started again")
	}
}

// An app paused by its maintenance command whose command could not run is
// scaled down instead. The data put back may have been captured in
// maintenance mode, and scaling up alone leaves it there: the profile's
// resume command is run once a pod answers, after the post-restore
// commands, and the app is started again after both.
func TestAnAppWhoseMaintenanceCommandFailedIsTakenOutOfMaintenanceAfterARestore(t *testing.T) {
	got, exec, w := restoreWithCommands(t, &gentianov1alpha1.BackupSpec{
		Quiesce: &gentianov1alpha1.BackupQuiesce{
			Mode: gentianov1alpha1.BackupQuiesceCommand,
			Pre:  []string{"wiki", "maintenance", "--on"},
			Post: []string{"wiki", "maintenance", "--off"},
		},
		Restore: &gentianov1alpha1.BackupRestore{Post: [][]string{{"wiki", "rewrite"}}},
	}, "--on")
	if got.Status.Phase != gentianov1alpha1.TenantExportPhaseReady {
		t.Fatalf("phase = %s: %+v", got.Status.Phase, got.Status.Conditions)
	}
	if mode := appStatus(&got.Status.Apps, "wiki").QuiesceMode; mode != string(gentianov1alpha1.BackupQuiesceScaleDown) {
		t.Fatalf("quiesce mode = %q, want the scale-down fallback", mode)
	}
	if !slices.Equal(exec.calls, []string{"--on", "rewrite", "--off"}) {
		t.Fatalf("calls = %v, want the failed pause, the post-restore command, then the resume command", exec.calls)
	}
	if _, ok := getDeployment(t, w.c, "wiki").Spec.Template.Annotations[restartWorkloadAnnotation]; !ok {
		t.Error("the app was not started again after its commands")
	}
}

// The resume command owed after a fallback failing fails the app, as a
// post-restore command failing does: the app would otherwise be reported
// restored while holding users out.
func TestAFailedOwedResumeCommandFailsTheApp(t *testing.T) {
	got, _, _ := restoreWithCommands(t, &gentianov1alpha1.BackupSpec{
		Quiesce: &gentianov1alpha1.BackupQuiesce{
			Mode: gentianov1alpha1.BackupQuiesceCommand,
			Pre:  []string{"wiki", "maintenance", "--on"},
			Post: []string{"wiki", "maintenance", "--on"},
		},
	}, "--on")
	entry := appStatus(&got.Status.Apps, "wiki")
	if entry.Phase != gentianov1alpha1.TenantExportPhaseFailed || !strings.Contains(entry.Message, "resume command failed") {
		t.Fatalf("app = %s %q", entry.Phase, entry.Message)
	}
}

// The databases an app made itself are named only inside their archive, and
// are known only once the restore's Job has put them back. What it reports
// is kept on the artifact, for the app's post-restore commands to be told.
func TestTheNamesOwnedDatabasesWerePutBackUnderAreKept(t *testing.T) {
	w := newRestoreWorld(t, nil)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "tx-demo-r1-wiki-pgor-abcde", Namespace: postgresNamespace,
			Labels: map[string]string{"job-name": "tx-demo-r1-wiki-pgor"}},
		Status: corev1.PodStatus{Phase: corev1.PodSucceeded, ContainerStatuses: []corev1.ContainerStatus{{
			Name: "pg-restore-owned",
			State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
				ExitCode: 0, Message: "old_wiki_reports=demo_wiki_reports\nmine=demo_wiki_mine\n"}},
		}}},
	}
	if err := w.c.Create(context.Background(), pod); err != nil {
		t.Fatal(err)
	}
	got := w.run(t)
	for _, a := range appStatus(&got.Status.Apps, "wiki").Artefacts {
		if a.Kind == bundle.ArtefactPostgresOwned {
			if !slices.Equal(a.Databases, []string{"old_wiki_reports=demo_wiki_reports", "mine=demo_wiki_mine"}) {
				t.Errorf("databases = %v", a.Databases)
			}
			return
		}
	}
	t.Fatal("no owned artifact in the plan")
}
