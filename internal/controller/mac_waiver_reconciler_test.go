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
	"encoding/json"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// macWaiverScheme registers corev1 as well: granting a waiver writes a label on the
// tenant Namespace and reads Pods to report whether anything claims it.
func macWaiverScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := gentianov1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add gentian scheme: %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add corev1 scheme: %v", err)
	}
	return scheme
}

func TestEnsureMacWaivers_annotatesApprovedWaivers(t *testing.T) {
	t.Parallel()

	psp := &gentianov1alpha1.PlatformSecurityPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: gentianov1alpha1.PlatformSecurityPolicyName},
		Spec: gentianov1alpha1.PlatformSecurityPolicySpec{
			AllowedMacWaivers: []gentianov1alpha1.AllowedMacWaiver{
				{Profile: "catalogue-test-app", Policy: "gentian-require-non-root", Scope: "sidecar-meet"},
			},
		},
	}
	profile := &gentianov1alpha1.AppProfile{
		ObjectMeta: metav1.ObjectMeta{Name: "catalogue-test-app"},
		Spec: gentianov1alpha1.AppProfileSpec{
			Security: &gentianov1alpha1.SecuritySpec{
				MacWaivers: []gentianov1alpha1.MacWaiverRequest{
					{Policy: "gentian-require-non-root", Scope: "sidecar-meet"},
					{Policy: "other-policy", Scope: "other-scope"},
				},
			},
		},
	}
	tenant := &gentianov1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: "demo"},
		Spec: gentianov1alpha1.TenantSpec{
			Apps: []gentianov1alpha1.TenantApp{{Profile: "catalogue-test-app"}},
		},
	}

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "tenant-demo"}}
	c := fake.NewClientBuilder().WithScheme(macWaiverScheme(t)).
		WithObjects(psp, profile, tenant, ns).Build()
	r := &TenantReconciler{Client: c}

	if _, err := r.ensureMacWaivers(context.Background(), tenant); err != nil {
		t.Fatalf("ensureMacWaivers: %v", err)
	}

	raw := tenant.Annotations[approvedMacWaiversAnnotation]
	if raw == "" {
		t.Fatal("expected approved waivers annotation")
	}
	var approved map[string][]gentianov1alpha1.MacWaiverRequest
	if err := json.Unmarshal([]byte(raw), &approved); err != nil {
		t.Fatalf("unmarshal annotation: %v", err)
	}
	if len(approved["catalogue-test-app"]) != 1 {
		t.Fatalf("approved = %#v", approved)
	}

	found := false
	for _, cond := range tenant.Status.Conditions {
		if cond.Type == conditionMacWaiversReady && cond.Reason == "WaiverNotApproved" {
			found = true
		}
	}
	if !found {
		t.Fatal("expected MacWaiversReady=False for unapproved waiver request")
	}
}

// macWaiverFixture builds a tenant whose single app requests one waiver that the
// platform policy allows, so the waiver is approved and should be granted.
func macWaiverFixture(extra ...client.Object) (*gentianov1alpha1.Tenant, []client.Object) {
	psp := &gentianov1alpha1.PlatformSecurityPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: gentianov1alpha1.PlatformSecurityPolicyName},
		Spec: gentianov1alpha1.PlatformSecurityPolicySpec{
			AllowedMacWaivers: []gentianov1alpha1.AllowedMacWaiver{
				{Profile: "app-a", Policy: "gentian-require-non-root", Scope: "main"},
			},
		},
	}
	profile := &gentianov1alpha1.AppProfile{
		ObjectMeta: metav1.ObjectMeta{Name: "app-a"},
		Spec: gentianov1alpha1.AppProfileSpec{
			Security: &gentianov1alpha1.SecuritySpec{
				MacWaivers: []gentianov1alpha1.MacWaiverRequest{
					{Policy: "gentian-require-non-root", Scope: "main"},
				},
			},
		},
	}
	tenant := &gentianov1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: "demo"},
		Spec: gentianov1alpha1.TenantSpec{
			Apps: []gentianov1alpha1.TenantApp{{Profile: "app-a"}},
		},
	}
	objs := append([]client.Object{psp, profile, tenant,
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "tenant-demo"}}}, extra...)
	return tenant, objs
}

// An approved waiver has to reach the NAMESPACE. Kyverno excludes a Pod only when
// the Pod label and the namespace label are both present, and the namespace half is
// the operator-written one no chart can forge — so without this the approval is
// recorded and the Pod is still denied.
func TestEnsureMacWaivers_grantsNamespaceLabel(t *testing.T) {
	t.Parallel()
	tenant, objs := macWaiverFixture()
	c := fake.NewClientBuilder().WithScheme(macWaiverScheme(t)).WithObjects(objs...).Build()
	r := &TenantReconciler{Client: c}

	if _, err := r.ensureMacWaivers(context.Background(), tenant); err != nil {
		t.Fatalf("ensureMacWaivers: %v", err)
	}

	ns := &corev1.Namespace{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "tenant-demo"}, ns); err != nil {
		t.Fatalf("get namespace: %v", err)
	}
	key := gentianov1alpha1.MacWaiverLabelKey("gentian-require-non-root")
	if got := ns.Labels[key]; got != gentianov1alpha1.MacWaiverApprovedValue {
		t.Fatalf("namespace label %s = %q, want %q", key, got, gentianov1alpha1.MacWaiverApprovedValue)
	}
}

// Revoking an approval must remove the grant. Otherwise withdrawing a waiver from
// the allowlist would leave the namespace exempt forever.
func TestEnsureMacWaivers_revokesNamespaceLabel(t *testing.T) {
	t.Parallel()
	stale := gentianov1alpha1.MacWaiverLabelKey("gentian-restrict-capabilities")
	tenant, objs := macWaiverFixture()
	for _, o := range objs {
		if ns, ok := o.(*corev1.Namespace); ok {
			ns.Labels = map[string]string{
				stale:                         gentianov1alpha1.MacWaiverApprovedValue,
				"kubernetes.io/metadata.name": "tenant-demo",
			}
		}
	}
	c := fake.NewClientBuilder().WithScheme(macWaiverScheme(t)).WithObjects(objs...).Build()
	r := &TenantReconciler{Client: c}

	if _, err := r.ensureMacWaivers(context.Background(), tenant); err != nil {
		t.Fatalf("ensureMacWaivers: %v", err)
	}

	ns := &corev1.Namespace{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "tenant-demo"}, ns); err != nil {
		t.Fatalf("get namespace: %v", err)
	}
	if _, present := ns.Labels[stale]; present {
		t.Errorf("stale waiver label %s survived revocation", stale)
	}
	if ns.Labels["kubernetes.io/metadata.name"] != "tenant-demo" {
		t.Error("rebuilding waiver labels dropped an unrelated label")
	}
	if ns.Labels[gentianov1alpha1.MacWaiverLabelKey("gentian-require-non-root")] == "" {
		t.Error("approved waiver was not granted")
	}
}

// Approved and granted, but no Pod claims it — so nothing is exempt yet. Reported
// rather than left reading Approved while the workload is still denied.
func TestEnsureMacWaivers_reportsAwaitingWorkloadOptIn(t *testing.T) {
	t.Parallel()
	unlabelled := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "tenant-demo"},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "busybox"}}},
	}
	tenant, objs := macWaiverFixture(unlabelled)
	c := fake.NewClientBuilder().WithScheme(macWaiverScheme(t)).WithObjects(objs...).Build()
	r := &TenantReconciler{Client: c}

	if _, err := r.ensureMacWaivers(context.Background(), tenant); err != nil {
		t.Fatalf("ensureMacWaivers: %v", err)
	}
	if !macWaiverConditionHasReason(tenant, "AwaitingWorkloadOptIn") {
		t.Fatalf("conditions = %#v, want AwaitingWorkloadOptIn", tenant.Status.Conditions)
	}
}

// A Pod that carries the label means the exemption is live, so the condition is
// Approved rather than awaiting anything.
func TestEnsureMacWaivers_approvedWhenPodClaimsWaiver(t *testing.T) {
	t.Parallel()
	claimed := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "web", Namespace: "tenant-demo",
			Labels: map[string]string{
				gentianov1alpha1.MacWaiverLabelKey("gentian-require-non-root"): gentianov1alpha1.MacWaiverApprovedValue,
			},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "busybox"}}},
	}
	tenant, objs := macWaiverFixture(claimed)
	c := fake.NewClientBuilder().WithScheme(macWaiverScheme(t)).WithObjects(objs...).Build()
	r := &TenantReconciler{Client: c}

	if _, err := r.ensureMacWaivers(context.Background(), tenant); err != nil {
		t.Fatalf("ensureMacWaivers: %v", err)
	}
	if !macWaiverConditionHasReason(tenant, "Approved") {
		t.Fatalf("conditions = %#v, want Approved", tenant.Status.Conditions)
	}
}

// No Pods at all is not a finding — the workload simply has not been created yet,
// so reporting "not in effect" would be noise on every fresh tenant.
func TestEnsureMacWaivers_silentBeforeAnyPodExists(t *testing.T) {
	t.Parallel()
	tenant, objs := macWaiverFixture()
	c := fake.NewClientBuilder().WithScheme(macWaiverScheme(t)).WithObjects(objs...).Build()
	r := &TenantReconciler{Client: c}

	if _, err := r.ensureMacWaivers(context.Background(), tenant); err != nil {
		t.Fatalf("ensureMacWaivers: %v", err)
	}
	if macWaiverConditionHasReason(tenant, "AwaitingWorkloadOptIn") {
		t.Error("reported AwaitingWorkloadOptIn with no Pods in the namespace")
	}
}

func macWaiverConditionHasReason(tenant *gentianov1alpha1.Tenant, reason string) bool {
	for _, cond := range tenant.Status.Conditions {
		if cond.Type == conditionMacWaiversReady && cond.Reason == reason {
			return true
		}
	}
	return false
}
