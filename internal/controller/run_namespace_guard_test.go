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
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const guardDispatcher = "system:serviceaccount:kernel-control:gentian-os-dispatcher"

// In a tenant's namespace for runs the dispatcher starts every pod. No other
// account writes one, and none writes a workload that would have a
// controller write pods for it. Elsewhere the rule says nothing.
func TestOnlyTheDispatcherStartsAPodInANamespaceForRuns(t *testing.T) {
	as := guardEnv(t, "templates/run-namespace-guard.yaml")
	ctx := context.Background()
	admin := as("admin")
	for name, tier := range map[string]string{"tenant-acme-runs": "tenant-runs", "tenant-acme": "tenant"} {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{"gentianos.io/tier": tier}}}
		if err := admin.Create(ctx, ns); err != nil {
			t.Fatal(err)
		}
	}
	pod := func(ns, name string) *corev1.Pod {
		return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}, Spec: guardPodTemplate(nil).Spec}
	}
	job := func(ns, name string) *batchv1.Job {
		return &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}, Spec: batchv1.JobSpec{Template: guardPodTemplate(nil)}}
	}
	dispatcher, operator, app := as(guardDispatcher), as(guardOperator), as(guardAppAccount)

	// The rule is in force once another account is refused.
	untilRefused(t, "the operator starts a pod in a namespace for runs", func(n int) error {
		return operator.Create(ctx, pod("tenant-acme-runs", "by-operator"))
	})
	if err := dispatcher.Create(ctx, pod("tenant-acme-runs", "run-1")); err != nil {
		t.Fatalf("the dispatcher starts a run: %v", err)
	}
	untilRefused(t, "an app's account starts a pod in a namespace for runs", func(n int) error {
		return app.Create(ctx, pod("tenant-acme-runs", "by-app"))
	})
	for who, account := range map[string]client.Client{"the dispatcher": dispatcher, "the operator": operator} {
		untilRefused(t, who+" writes a Job in a namespace for runs", func(n int) error {
			return account.Create(ctx, job("tenant-acme-runs", "a-job"))
		})
	}

	// A person is break-glass, and another namespace is not this rule's.
	if err := admin.Create(ctx, pod("tenant-acme-runs", "by-a-person")); err != nil {
		t.Fatalf("a person with the right to: %v", err)
	}
	if err := operator.Create(ctx, pod("tenant-acme", "elsewhere")); err != nil {
		t.Fatalf("a pod in the tenant's own namespace: %v", err)
	}
	if err := app.Create(ctx, job("tenant-acme", "elsewhere")); err != nil {
		t.Fatalf("a Job in the tenant's own namespace: %v", err)
	}
}
