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
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// The fingerprint decides whether a refused workload is retried, so what it
// must NOT do is change on its own: a digest that varied with map ordering
// would roll every blocked Deployment on every reconcile.
func TestQuotaFingerprint(t *testing.T) {
	quota := func(cpu, mem string) corev1.ResourceList {
		return corev1.ResourceList{
			corev1.ResourceLimitsCPU:    resource.MustParse(cpu),
			corev1.ResourceLimitsMemory: resource.MustParse(mem),
		}
	}

	a := quotaFingerprint(quota("4", "4Gi"))
	if a != quotaFingerprint(quota("4", "4Gi")) {
		t.Fatal("fingerprint is not stable across calls")
	}
	if a == quotaFingerprint(quota("6", "6Gi")) {
		t.Fatal("a raised quota must not keep the same fingerprint")
	}
	if a == quotaFingerprint(quota("4", "6Gi")) {
		t.Fatal("raising only memory must change the fingerprint")
	}
	if quotaFingerprint(nil) != quotaFingerprint(corev1.ResourceList{}) {
		t.Fatal("absent and empty quotas should agree")
	}
}

// ReplicaFailure is the only signal that a workload was refused; a Deployment
// that is merely still rolling out must not be reported as stuck.
func TestDeploymentBlocked(t *testing.T) {
	withCondition := func(t appsv1.DeploymentConditionType, s corev1.ConditionStatus, msg string) *appsv1.Deployment {
		return &appsv1.Deployment{Status: appsv1.DeploymentStatus{
			Conditions: []appsv1.DeploymentCondition{{Type: t, Status: s, Message: msg}},
		}}
	}

	cases := []struct {
		name string
		dep  *appsv1.Deployment
		want bool
	}{
		{"no conditions", &appsv1.Deployment{}, false},
		{"refused", withCondition(appsv1.DeploymentReplicaFailure, corev1.ConditionTrue, "exceeded quota"), true},
		{"refusal cleared", withCondition(appsv1.DeploymentReplicaFailure, corev1.ConditionFalse, ""), false},
		{"still progressing", withCondition(appsv1.DeploymentProgressing, corev1.ConditionTrue, ""), false},
		{"available", withCondition(appsv1.DeploymentAvailable, corev1.ConditionTrue, ""), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, msg := deploymentBlocked(tc.dep)
			if got != tc.want {
				t.Fatalf("deploymentBlocked = %v, want %v", got, tc.want)
			}
			if got && msg == "" {
				t.Fatal("a blocked workload must report why")
			}
		})
	}
}

func TestFirstLine(t *testing.T) {
	quota := "pods \"nextcloud-74c\" is forbidden: exceeded quota: tenant-quota\nused: limits.cpu=3700m"
	if got := firstLine(quota); got != "pods \"nextcloud-74c\" is forbidden: exceeded quota: tenant-quota" {
		t.Fatalf("firstLine kept too much: %q", got)
	}
	if got := firstLine("single line"); got != "single line" {
		t.Fatalf("firstLine altered a single line: %q", got)
	}
}
