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
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"
)

const (
	guardTenantAppsHelm     = "system:serviceaccount:kernel-provisioning:provider-helm-tenant-apps"
	guardProviderKubernetes = "system:serviceaccount:kernel-provisioning:provider-kubernetes"
	guardAppAccount         = "system:serviceaccount:tenant-acme:wiki"
)

func guardPodTemplate(labels map[string]string) corev1.PodTemplateSpec {
	return corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{Labels: labels},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
			Containers:    []corev1.Container{{Name: "c", Image: "example.invalid/c:1"}},
		},
	}
}

func guardDeployment(ns, name string, labels map[string]string) *appsv1.Deployment {
	all := map[string]string{"name": name}
	for k, v := range labels {
		all[k] = v
	}
	tpl := guardPodTemplate(all)
	tpl.Spec.RestartPolicy = corev1.RestartPolicyAlways
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"name": name}},
			Template: tpl,
		},
	}
}

// Which app a tenant's pod belongs to is said by the accounts that deliver
// what the platform rendered. An account of the tenant's own may write a
// workload, without either label; it may neither set one nor change one.
func TestOnlyThePlatformSaysWhichAppAPodBelongsTo(t *testing.T) {
	as := guardEnv(t, "templates/app-label-guard.yaml")
	ctx := context.Background()
	admin := as("admin")
	for name, tier := range map[string]string{"tenant-acme": "tenant", "tenant-acme-dmz": "tenant-dmz", "elsewhere": ""} {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
		if tier != "" {
			ns.Labels = map[string]string{"gentianos.io/tier": tier}
		}
		if err := admin.Create(ctx, ns); err != nil {
			t.Fatal(err)
		}
	}
	app, helm, operator := as(guardAppAccount), as(guardTenantAppsHelm), as(guardOperator)
	other := map[string]string{"gentianos.io/app": "docs"}

	untilRefused(t, "a Deployment an app's account labels as another app", func(n int) error {
		return app.Create(ctx, guardDeployment("tenant-acme", fmt.Sprintf("probe-%d", n), other))
	})

	job := func(ns, name string, labels map[string]string) *batchv1.Job {
		return &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}, Spec: batchv1.JobSpec{Template: guardPodTemplate(labels)}}
	}
	cron := func(name string, labels map[string]string) *batchv1.CronJob {
		return &batchv1.CronJob{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "tenant-acme"}, Spec: batchv1.CronJobSpec{
			Schedule: "0 0 * * *", JobTemplate: batchv1.JobTemplateSpec{Spec: batchv1.JobSpec{Template: guardPodTemplate(labels)}}}}
	}
	pod := func(name string, labels map[string]string) *corev1.Pod {
		tpl := guardPodTemplate(labels)
		return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "tenant-acme", Labels: labels}, Spec: tpl.Spec}
	}
	sts := func(name string, labels map[string]string) *appsv1.StatefulSet {
		d := guardDeployment("tenant-acme", name, labels)
		return &appsv1.StatefulSet{ObjectMeta: d.ObjectMeta, Spec: appsv1.StatefulSetSpec{Selector: d.Spec.Selector, Template: d.Spec.Template, ServiceName: name}}
	}
	cases := []struct {
		name     string
		who      client.Client
		obj      client.Object
		admitted bool
	}{
		{"an app's account, a Job as another app", app, job("tenant-acme", "job-other", other), false},
		{"an app's account, a Job with the tenant label", app, job("tenant-acme", "job-tenant", map[string]string{"gentianos.io/tenant": "acme"}), false},
		{"an app's account, a StatefulSet as another app", app, sts("sts-other", other), false},
		{"an app's account, a CronJob as another app", app, cron("cron-other", other), false},
		{"an app's account, a Pod as another app", app, pod("pod-other", other), false},
		{"an app's account, a Deployment as another app in the DMZ", app, guardDeployment("tenant-acme-dmz", "dmz-other", other), false},
		{"an app's account, a Job with neither label", app, job("tenant-acme", "job-plain", map[string]string{"app.kubernetes.io/instance": "wiki"}), true},
		{"an app's account, a Job with no labels", app, job("tenant-acme", "job-bare", nil), true},
		{"an app's account, outside the tenant tiers", app, job("elsewhere", "job-elsewhere", other), true},
		{"the account a tenant's charts are installed as", helm, guardDeployment("tenant-acme", "wiki", map[string]string{"gentianos.io/app": "wiki", "gentianos.io/tenant": "acme"}), true},
		{"provider-kubernetes", as(guardProviderKubernetes), job("tenant-acme", "post-install", map[string]string{"gentianos.io/app": "wiki"}), true},
		{"the operator", operator, guardDeployment("tenant-acme", "wiki-sign-in", map[string]string{"gentianos.io/app": "wiki"}), true},
		{"a controller of the cluster", as("system:serviceaccount:kube-system:job-controller"), pod("derived", map[string]string{"gentianos.io/app": "wiki"}), true},
		{"a person", as("break-glass"), job("tenant-acme", "by-hand", other), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.who.Create(ctx, tc.obj)
			switch {
			case tc.admitted && err != nil:
				t.Errorf("refused: %v", err)
			case !tc.admitted && err == nil:
				t.Error("admitted")
			case !tc.admitted && !strings.Contains(err.Error(), "set by the platform alone"):
				t.Errorf("refused, but not by the guard: %v", err)
			}
		})
	}

	// What is there stays as it is: an app's account may change its
	// Deployment, and not the labels of its pods.
	wiki := &appsv1.Deployment{}
	if err := app.Get(ctx, client.ObjectKey{Namespace: "tenant-acme", Name: "wiki"}, wiki); err != nil {
		t.Fatal(err)
	}
	replicas := int32(2)
	wiki.Spec.Replicas = &replicas
	if err := app.Update(ctx, wiki); err != nil {
		t.Errorf("an app's account may not scale its own Deployment: %v", err)
	}
	for what, change := range map[string]func(map[string]string){
		"renames the app":         func(l map[string]string) { l["gentianos.io/app"] = "docs" },
		"renames the tenant":      func(l map[string]string) { l["gentianos.io/tenant"] = "other" },
		"takes the app label off": func(l map[string]string) { delete(l, "gentianos.io/app") },
	} {
		changed := wiki.DeepCopy()
		change(changed.Spec.Template.Labels)
		if err := app.Update(ctx, changed); !apierrors.IsForbidden(err) {
			t.Errorf("an update that %s: %v", what, err)
		}
	}
}

// The patch a release is rendered through is the operator's. A tenant's
// chart, installed or running, writes and deletes none.
func TestAReleasesPatchIsThePlatformsToWrite(t *testing.T) {
	as := guardEnv(t, "templates/release-patch-guard.yaml")
	ctx := context.Background()
	if err := as("admin").Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "tenant-acme", Labels: map[string]string{"gentianos.io/tier": "tenant"}}}); err != nil {
		t.Fatal(err)
	}
	cm := func(name string) *corev1.ConfigMap {
		return &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "tenant-acme"}, Data: map[string]string{"patch.yaml": "patches: []\n"}}
	}
	app, helm, operator := as(guardAppAccount), as(guardTenantAppsHelm), as(guardOperator)
	untilRefused(t, "a patch an app's account writes", func(n int) error {
		return app.Create(ctx, cm(fmt.Sprintf("probe-%d-keep-volumes", n)))
	})
	if err := operator.Create(ctx, cm("wiki-keep-volumes")); err != nil {
		t.Fatalf("the operator's patch was refused: %v", err)
	}
	if err := helm.Create(ctx, cm("docs-keep-volumes")); !apierrors.IsForbidden(err) {
		t.Errorf("a patch written by a chart: %v", err)
	}
	theirs := cm("wiki-keep-volumes")
	theirs.Data["patch.yaml"] = "patches: [{}]\n"
	for who, c := range map[string]client.Client{"an app's account": app, "a chart": helm} {
		if err := c.Update(ctx, theirs.DeepCopy()); !apierrors.IsForbidden(err) {
			t.Errorf("%s changes the patch: %v", who, err)
		}
		if err := c.Delete(ctx, cm("wiki-keep-volumes")); !apierrors.IsForbidden(err) {
			t.Errorf("%s deletes the patch: %v", who, err)
		}
	}
	if err := app.Create(ctx, cm("wiki-settings")); err != nil {
		t.Errorf("an app's own ConfigMap was refused: %v", err)
	}
	if err := operator.Delete(ctx, cm("wiki-keep-volumes")); err != nil {
		t.Errorf("the operator may not delete its patch: %v", err)
	}
}

// The guard names two accounts of the provisioning namespace by the
// ServiceAccounts they act as. A rename there would lock a writer out.
func TestTheAppLabelGuardNamesTheAccountsThatExist(t *testing.T) {
	var values struct {
		AppLabelGuard struct {
			ProviderKubernetesServiceAccount string `json:"providerKubernetesServiceAccount"`
			TenantAppsHelmServiceAccount     string `json:"tenantAppsHelmServiceAccount"`
		} `json:"appLabelGuard"`
	}
	raw, err := os.ReadFile(filepath.Join("..", "..", "charts", "gentian-os", "values.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if err := yaml.Unmarshal(raw, &values); err != nil {
		t.Fatal(err)
	}
	if got := values.AppLabelGuard.TenantAppsHelmServiceAccount; got != tenantAppsHelmServiceAccount {
		t.Errorf("a tenant's charts are installed as %q and the guard names %q", tenantAppsHelmServiceAccount, got)
	}
	providers, err := os.ReadFile(filepath.Join("..", "..", "crossplane", "providers", "providers.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if want := "name: " + values.AppLabelGuard.ProviderKubernetesServiceAccount + "\n"; !strings.Contains(string(providers), want) {
		t.Errorf("crossplane/providers/providers.yaml pins no ServiceAccount %q", values.AppLabelGuard.ProviderKubernetesServiceAccount)
	}
}
