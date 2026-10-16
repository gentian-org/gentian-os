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
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// hangingExecer is a hook that never returns on its own: it ends only when
// the context it was given does.
type hangingExecer struct{}

func (hangingExecer) Exec(ctx context.Context, _, _, _ string, _ []string) (string, error) {
	<-ctx.Done()
	return "", ctx.Err()
}

// A hook that hangs holds the reconcile, and with it the app paused, for as
// long as it hangs. It is given a bounded time and fails when that is up, as
// a hook that exits non-zero does -- and a restore fails the app on it.
func TestAHangingHookFailsWhenItsTimeIsUp(t *testing.T) {
	if AppCommandTimeout <= 0 || AppCommandTimeout > time.Hour {
		t.Fatalf("AppCommandTimeout = %s, want a bound of minutes", AppCommandTimeout)
	}
	s := quiesceScheme(t)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "wiki-0", Namespace: "tenant-demo", Labels: map[string]string{"gentianos.io/app": "wiki"}},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "wiki"}}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning,
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}},
	}
	tr := &TenantReconciler{Client: fake.NewClientBuilder().WithScheme(s).WithObjects(pod).Build(),
		Exec: hangingExecer{}, execTimeout: 50 * time.Millisecond}

	done := make(chan error, 1)
	go func() {
		_, err := tr.execAppCommand(context.Background(), "demo", "wiki", nil, []string{"wiki", "rewrite"})
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "did not finish within 50ms") {
			t.Fatalf("err = %v, want the timeout named", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the hook was not stopped when its time was up")
	}

	r := &TenantRestoreReconciler{Tenant: tr}
	spec := &gentianov1alpha1.BackupSpec{Restore: &gentianov1alpha1.BackupRestore{Post: [][]string{{"wiki", "rewrite"}}}}
	restore := &gentianov1alpha1.TenantRestore{}
	if err := r.runRestoreHooks(context.Background(), "demo", "wiki", spec, appStatus(&restore.Status.Apps, "wiki"), nil); err == nil {
		t.Fatal("a post-restore command that ran out of time was taken as done")
	}
}
