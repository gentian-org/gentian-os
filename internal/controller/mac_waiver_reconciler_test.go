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
	"encoding/json"
	"testing"
	"time"

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
	profile := &gentianov1alpha1.ComponentProfile{
		ObjectMeta: metav1.ObjectMeta{Name: "catalogue-test-app"},
		Spec: gentianov1alpha1.ComponentProfileSpec{
			Classes: []gentianov1alpha1.ComponentClass{gentianov1alpha1.ComponentClassApp}, Launch: gentianov1alpha1.ComponentLaunchNone, TrustTier: gentianov1alpha1.TrustTierCertified, Version: "1.0.0",
			Requires: &gentianov1alpha1.RequirementSpec{
				Privileges: &gentianov1alpha1.PrivilegeRequest{
					PodSecurity: []gentianov1alpha1.PodSecurityWaiver{
						{Name: "run-as-root", Policy: "gentian-require-non-root", Scope: "sidecar-meet",
							Reason: "the meet sidecar needs a privileged port"},
						{Name: "other", Policy: "other-policy", Scope: "other-scope",
							Reason: "asks for something the cluster does not allow"},
					},
				},
			},
		},
	}
	tenant := &gentianov1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: "demo"},
		Spec: gentianov1alpha1.TenantSpec{
			DisplayName: "Test Tenant",
			Apps:        []gentianov1alpha1.TenantApp{{Profile: "catalogue-test-app"}},
			Privileges: []gentianov1alpha1.TenantPrivilegeGrant{
				waiverGrant("catalogue-test-app", "run-as-root"),
				// Granted, and not on the allowlist: a grant widens nothing.
				waiverGrant("catalogue-test-app", "other"),
			},
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

// waiverGrant is the security officer's yes to one pod-security request of
// one install.
func waiverGrant(install, name string) gentianov1alpha1.TenantPrivilegeGrant {
	return gentianov1alpha1.TenantPrivilegeGrant{
		Install:    install,
		Privilege:  "podSecurity/" + name,
		Approver:   "officer",
		ApprovedAt: metav1.Now(),
		Reason:     "reviewed for this install",
	}
}

// macWaiverFixture builds a tenant whose single app requests one waiver that the
// platform policy allows and that was granted on the install, so the waiver
// takes effect.
func macWaiverFixture(extra ...client.Object) (*gentianov1alpha1.Tenant, []client.Object) {
	psp := &gentianov1alpha1.PlatformSecurityPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: gentianov1alpha1.PlatformSecurityPolicyName},
		Spec: gentianov1alpha1.PlatformSecurityPolicySpec{
			AllowedMacWaivers: []gentianov1alpha1.AllowedMacWaiver{
				{Profile: "app-a", Policy: "gentian-require-non-root", Scope: "main"},
			},
		},
	}
	profile := &gentianov1alpha1.ComponentProfile{
		ObjectMeta: metav1.ObjectMeta{Name: "app-a"},
		Spec: gentianov1alpha1.ComponentProfileSpec{
			Classes: []gentianov1alpha1.ComponentClass{gentianov1alpha1.ComponentClassApp}, Launch: gentianov1alpha1.ComponentLaunchNone, TrustTier: gentianov1alpha1.TrustTierCertified, Version: "1.0.0",
			Requires: &gentianov1alpha1.RequirementSpec{
				Privileges: &gentianov1alpha1.PrivilegeRequest{
					PodSecurity: []gentianov1alpha1.PodSecurityWaiver{
						{Name: "run-as-root", Policy: "gentian-require-non-root", Scope: "main",
							Reason: "the main container writes as root on first start"},
					},
				},
			},
		},
	}
	tenant := &gentianov1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: "demo"},
		Spec: gentianov1alpha1.TenantSpec{
			DisplayName: "Test Tenant",
			Apps:        []gentianov1alpha1.TenantApp{{Profile: "app-a"}},
			Privileges:  []gentianov1alpha1.TenantPrivilegeGrant{waiverGrant("app-a", "run-as-root")},
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

// The allowlist alone waives nothing. It says what the cluster may ever
// waive; whether this tenant's install gets it is the grant's to say, and
// without one the namespace is not labelled and the tenant says why.
func TestEnsureMacWaivers_allowlistWithoutGrantWaivesNothing(t *testing.T) {
	t.Parallel()
	expired := waiverGrant("app-a", "run-as-root")
	expired.ExpiresAt = &metav1.Time{Time: time.Now().Add(-time.Hour)}
	for name, grants := range map[string][]gentianov1alpha1.TenantPrivilegeGrant{
		"no grant":                nil,
		"a grant that expired":    {expired},
		"another install's grant": {waiverGrant("app-b", "run-as-root")},
		"another privilege's":     {waiverGrant("app-a", "something-else")},
		"an egress grant, same name": {{Install: "app-a", Privilege: "egress/run-as-root", Approver: "admin",
			ApprovedAt: metav1.Now(), Reason: "outbound mail relay"}},
	} {
		tenant, objs := macWaiverFixture()
		tenant.Spec.Privileges = grants
		c := fake.NewClientBuilder().WithScheme(macWaiverScheme(t)).WithObjects(objs...).Build()
		r := &TenantReconciler{Client: c}
		if _, err := r.ensureMacWaivers(context.Background(), tenant); err != nil {
			t.Fatalf("%s: ensureMacWaivers: %v", name, err)
		}
		ns := &corev1.Namespace{}
		if err := c.Get(context.Background(), client.ObjectKey{Name: "tenant-demo"}, ns); err != nil {
			t.Fatalf("%s: get namespace: %v", name, err)
		}
		key := gentianov1alpha1.MacWaiverLabelKey("gentian-require-non-root")
		if got := ns.Labels[key]; got != "" {
			t.Fatalf("%s: namespace label %s = %q, want none without a grant on the install", name, key, got)
		}
		reported := false
		for _, cond := range tenant.Status.Conditions {
			if cond.Type == conditionMacWaiversReady && cond.Status == metav1.ConditionFalse && cond.Reason == "WaiverNotGranted" {
				reported = true
			}
		}
		if !reported {
			t.Fatalf("%s: conditions = %+v, want WaiverNotGranted", name, tenant.Status.Conditions)
		}
	}
}

// A grant that is withdrawn takes the namespace label with it.
func TestEnsureMacWaivers_withdrawnGrantRemovesLabel(t *testing.T) {
	t.Parallel()
	tenant, objs := macWaiverFixture()
	c := fake.NewClientBuilder().WithScheme(macWaiverScheme(t)).WithObjects(objs...).Build()
	r := &TenantReconciler{Client: c}
	key := gentianov1alpha1.MacWaiverLabelKey("gentian-require-non-root")
	label := func() string {
		ns := &corev1.Namespace{}
		if err := c.Get(context.Background(), client.ObjectKey{Name: "tenant-demo"}, ns); err != nil {
			t.Fatalf("get namespace: %v", err)
		}
		return ns.Labels[key]
	}
	if _, err := r.ensureMacWaivers(context.Background(), tenant); err != nil {
		t.Fatalf("ensureMacWaivers: %v", err)
	}
	if label() != gentianov1alpha1.MacWaiverApprovedValue {
		t.Fatal("a waiver that is permitted and granted was not put into effect")
	}
	tenant.Spec.Privileges = nil
	if _, err := r.ensureMacWaivers(context.Background(), tenant); err != nil {
		t.Fatalf("ensureMacWaivers: %v", err)
	}
	if got := label(); got != "" {
		t.Fatalf("label = %q after the grant was withdrawn, want none", got)
	}
}
