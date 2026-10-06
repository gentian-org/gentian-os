/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package provisioner

import (
	"context"
	"errors"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// A tenant's deletion runs a cleanup Job per store and comes back until each
// is done. A Job that FAILED is not "not done yet": it is reported, and
// removed so the next pass runs it again -- the deletion resumes where it
// stopped and never moves past a store it could not destroy. It used to wait
// on a failed Job in silence, and go on once the Job's TTL had removed it.
func TestAFailedCleanupJobIsReportedAndRunAgain(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	tenant := &gentianov1alpha1.Tenant{ObjectMeta: metav1.ObjectMeta{Name: "demo"}}
	name := func(tenant, app string) string { return "s3-delete-" + tenant + "-" + app }
	job := func(tenant *gentianov1alpha1.Tenant, app string) *batchv1.Job {
		return &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: name(tenant.Name, app), Namespace: "system-s3"}}
	}
	complete := func(job *batchv1.Job) bool {
		for _, c := range job.Status.Conditions {
			if c.Type == batchv1.JobComplete && c.Status == corev1.ConditionTrue {
				return true
			}
		}
		return false
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&batchv1.Job{}).Build()
	ensure := func() error {
		return EnsureDeleteJobs(ctx, c, "system-s3", tenant, []string{"wiki", "drive"}, name, job, complete)
	}
	finish := func(app string, condition batchv1.JobConditionType) {
		t.Helper()
		job := &batchv1.Job{}
		if err := c.Get(ctx, types.NamespacedName{Name: name("demo", app), Namespace: "system-s3"}, job); err != nil {
			t.Fatal(err)
		}
		job.Status.Conditions = []batchv1.JobCondition{{Type: condition, Status: corev1.ConditionTrue,
			Reason: "BackoffLimitExceeded", Message: "Job has reached the specified backoff limit"}}
		if err := c.Status().Update(ctx, job); err != nil {
			t.Fatal(err)
		}
	}

	// First pass: both Jobs are made, and the deletion waits.
	if err := ensure(); !errors.Is(err, ErrDeleteJobPending) {
		t.Fatalf("first pass: %v", err)
	}
	finish("wiki", batchv1.JobComplete)
	finish("drive", batchv1.JobFailed)

	// The failure is an error naming the Job, not "pending" and not success.
	err := ensure()
	if !errors.Is(err, ErrDeleteJobFailed) || errors.Is(err, ErrDeleteJobPending) {
		t.Fatalf("a failed cleanup Job answered %v", err)
	}
	if !strings.Contains(err.Error(), "s3-delete-demo-drive") || !strings.Contains(err.Error(), "BackoffLimitExceeded") {
		t.Errorf("the error does not name the Job and why it failed: %v", err)
	}
	// It was removed, so the next pass runs it again; the one that
	// succeeded is left as it is.
	if err := c.Get(ctx, types.NamespacedName{Name: "s3-delete-demo-drive", Namespace: "system-s3"}, &batchv1.Job{}); !apierrors.IsNotFound(err) {
		t.Fatalf("the failed Job was left in place: %v", err)
	}
	if err := ensure(); !errors.Is(err, ErrDeleteJobPending) {
		t.Fatalf("the pass after a failure: %v", err)
	}
	finish("drive", batchv1.JobComplete)
	if err := ensure(); err != nil {
		t.Fatalf("with both done: %v", err)
	}
}
