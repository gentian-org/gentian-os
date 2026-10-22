/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package applifecycle

import (
	"context"
	"errors"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/security"
)

func appsService(t *testing.T, objects ...runtime.Object) *Service {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := gentianov1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return &Service{client: fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(objects...).Build()}
}

func component(name, reason, message string, ready bool) *gentianov1alpha1.Component {
	status := metav1.ConditionFalse
	if ready {
		status = metav1.ConditionTrue
	}
	c := &gentianov1alpha1.Component{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "tenant-demo"},
		Spec:       gentianov1alpha1.ComponentSpec{ProfileRef: gentianov1alpha1.ProfileRef{Name: name}},
	}
	if reason != "" {
		c.Status.Conditions = []metav1.Condition{{
			Type: "Ready", Status: status, Reason: reason, Message: message,
		}}
	}
	return c
}

func profile(name string, annotations map[string]string) *gentianov1alpha1.ComponentProfile {
	return &gentianov1alpha1.ComponentProfile{ObjectMeta: metav1.ObjectMeta{Name: name, Annotations: annotations}}
}

func byProfile(t *testing.T, states []AppState) map[string]AppState {
	t.Helper()
	out := map[string]AppState{}
	for _, s := range states {
		out[s.Profile] = s
	}
	return out
}

// What the platform places in every tenant is not one of the tenant's apps,
// whatever its profile says of itself: the operator's label is what tells.
func TestThePlatformsOwnComponentsAreNotTheTenantsApps(t *testing.T) {
	desktop := component("desktop", "Ready", "release deployed", true)
	desktop.Labels = map[string]string{componentOriginLabel: componentOriginDefault}
	installed := component("wiki", "Ready", "release deployed", true)
	installed.Labels = map[string]string{componentOriginLabel: "install"}
	s := appsService(t, demoTenant(), desktop, installed)
	states, err := s.AppStates(context.Background(), "demo")
	if err != nil {
		t.Fatal(err)
	}
	got := byProfile(t, states)
	if _, listed := got["desktop"]; listed {
		t.Fatalf("a component the platform placed was listed as the tenant's app: %+v", states)
	}
	if _, listed := got["wiki"]; !listed {
		t.Fatalf("an installed app is missing: %+v", states)
	}
}

// Three phases, because those are the three things a person can do about an
// app: wait, use it, or look at it. What separates waiting from looking is
// whether the reason resolves without anybody changing the install.
func TestAnAppIsInstallingReadyOrFailing(t *testing.T) {
	s := appsService(t, demoTenant(),
		component("wiki", "Ready", "release deployed", true),
		component("drive", "Installing", "release created; waiting for the chart to deploy", false),
		component("crm", "PrivilegesPending", "waiting for podSecurity/run-as-root", false),
		component("notes", "ProfileMissing", "profile notes is not in the catalogue", false),
		component("fresh", "", "", false),
	)
	states, err := s.AppStates(context.Background(), "demo")
	if err != nil {
		t.Fatal(err)
	}
	got := byProfile(t, states)
	for name, want := range map[string]string{
		"wiki": AppPhaseReady, "drive": AppPhaseInstalling, "crm": AppPhaseInstalling,
		"notes": AppPhaseFailing, "fresh": AppPhaseInstalling,
	} {
		if got[name].Phase != want {
			t.Errorf("%s is %q, want %q (%+v)", name, got[name].Phase, want, got[name])
		}
	}
	if !got["wiki"].Ready || got["drive"].Ready {
		t.Errorf("ready: wiki=%v drive=%v", got["wiki"].Ready, got["drive"].Ready)
	}
	if got["notes"].Failure != "ProfileMissing" || !strings.Contains(got["notes"].Message, "not in the catalogue") {
		t.Errorf("the failing app does not say why: %+v", got["notes"])
	}
}

// Kubernetes retries a container that cannot start for ever, so the chart is
// "still deploying" for ever too. The pods are what say it never will.
func TestAWorkloadThatCannotStartIsFailingNotInstalling(t *testing.T) {
	s := appsService(t, demoTenant(),
		component("wiki", "Installing", "waiting for the release to be ready", false),
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name: "wiki-0", Namespace: "tenant-demo",
				Labels: map[string]string{"app.kubernetes.io/instance": "tenant-demo-wiki"},
			},
			Spec: corev1.PodSpec{Containers: []corev1.Container{{
				Name: "xwiki",
				Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("250m"),
					corev1.ResourceMemory: resource.MustParse("512Mi"),
				}},
			}}},
			Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
				Name: "xwiki",
				State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{
					Reason: "ImagePullBackOff", Message: "Back-off pulling image",
				}},
			}}},
		},
		// Somebody else's pod in the same namespace says nothing about this app.
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name: "drive-0", Namespace: "tenant-demo",
				Labels: map[string]string{"app.kubernetes.io/instance": "tenant-demo-drive"},
			},
			Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
				Name:  "drive",
				State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}},
			}}},
		},
	)
	states, err := s.AppStates(context.Background(), "demo")
	if err != nil {
		t.Fatal(err)
	}
	if len(states) != 1 {
		t.Fatalf("states = %+v", states)
	}
	wiki := states[0]
	if wiki.Reserved == nil || wiki.Reserved.CPUMilli != 250 || wiki.Reserved.MemoryBytes != 512<<20 {
		t.Fatalf("reserved = %+v", wiki.Reserved)
	}
	if wiki.Phase != AppPhaseFailing || !strings.Contains(wiki.Failure, "ImagePullBackOff") ||
		!strings.Contains(wiki.Failure, "wiki-0") {
		t.Fatalf("wiki = %+v", wiki)
	}
}

// The platform's own components and add-ons are not apps a tenant
// administrator installed, and the store offers nothing to do about them.
func TestThePlatformsOwnComponentsAndAddonsAreNotListed(t *testing.T) {
	s := appsService(t, demoTenant(),
		component("wiki", "Ready", "", true),
		component("desktop", "Ready", "", true),
		component("nextcloud-calendar-ce", "Ready", "", true),
		profile("desktop", map[string]string{platformAppAnnotation: "true"}),
		profile("nextcloud-calendar-ce", map[string]string{deploymentRoleAnnotation: deploymentRoleAddon}),
		profile("wiki", nil),
	)
	states, err := s.AppStates(context.Background(), "demo")
	if err != nil {
		t.Fatal(err)
	}
	if len(states) != 1 || states[0].Profile != "wiki" {
		t.Fatalf("states = %+v", states)
	}
}

// An add-on that runs as its own Component asks for privileges under its own
// name, and is listed under the app it activates in, so that whoever watches
// the app sees what the add-on waits for and whom to ask.
func TestAnAddonsPendingPrivilegesAreListedUnderItsApp(t *testing.T) {
	base := component("nextcloud-base-ce", "Ready", "", true)
	base.Spec.Addons = []string{"nextcloud-calendar-ce", "nextcloud-deck-ce"}
	calendar := component("nextcloud-calendar-ce", "PrivilegesPending", "waiting for egress/caldav", false)
	calendar.Status.PendingPrivileges = []string{"egress/caldav"}
	s := appsService(t, demoTenant(), base, calendar,
		profile("nextcloud-base-ce", nil),
		profile("nextcloud-calendar-ce", map[string]string{deploymentRoleAnnotation: deploymentRoleAddon}),
	)
	states, err := s.AppStates(context.Background(), "demo")
	if err != nil {
		t.Fatal(err)
	}
	if len(states) != 1 || len(states[0].Addons) != 1 {
		t.Fatalf("states = %+v", states)
	}
	addon := states[0].Addons[0]
	if addon.Name != "nextcloud-calendar-ce" || addon.Ready || addon.Phase != AppPhaseInstalling ||
		len(addon.PendingPrivilegeApprovals) != 1 || addon.PendingPrivilegeApprovals[0].Privilege != "egress/caldav" ||
		addon.PendingPrivilegeApprovals[0].ApprovedBy != security.PrivilegeApproverPerimeter {
		t.Fatalf("addon = %+v", addon)
	}
}

func TestATenantTheClusterDoesNotHaveIsAnError(t *testing.T) {
	s := appsService(t)
	if _, err := s.AppStates(context.Background(), "nobody"); err == nil {
		t.Fatal("no error for a tenant that does not exist")
	}
}

// Taking an app away must never take its data with it by accident, so a purge
// is refused while the tenant still has the app.
func TestAPurgeIsRefusedWhileTheAppIsInstalled(t *testing.T) {
	tenant := demoTenant()
	tenant.Spec.Apps = []gentianov1alpha1.TenantApp{{Profile: "wiki"}}
	s := appsService(t, tenant, component("wiki", "Ready", "", true))

	_, err := s.PurgeApp(context.Background(), "demo", "wiki", "tom@example.com")
	if !errors.Is(err, ErrStillInstalled) {
		t.Fatalf("err = %v", err)
	}
}

// Provisioning hands an app to people, so there has to be an app.
func TestProvisioningNeedsTheAppToBeInstalled(t *testing.T) {
	s := appsService(t, demoTenant())
	_, err := s.ProvisionApp(context.Background(), "demo", "wiki")
	if err == nil || !strings.Contains(err.Error(), "not installed") {
		t.Fatalf("err = %v", err)
	}
}

// An add-on has a group of its own, so it is provisioned by its own name.
func TestAnAddonSwitchedOnCountsAsInstalledForProvisioning(t *testing.T) {
	tenant := demoTenant()
	tenant.Spec.Apps = []gentianov1alpha1.TenantApp{{Profile: "cloud", Addons: []string{"cloud-calendar"}}}
	s := appsService(t, tenant)

	// Past the "is it installed" question, which is the one asked here; what
	// follows needs a Keycloak this test does not have.
	_, err := s.ProvisionApp(context.Background(), "demo", "cloud-calendar")
	if err == nil || strings.Contains(err.Error(), "not installed") {
		t.Fatalf("err = %v", err)
	}
	if _, err := s.ProvisionApp(context.Background(), "demo", "cloud-notes"); err == nil || !strings.Contains(err.Error(), "not installed") {
		t.Fatalf("err = %v", err)
	}
}

// An app that waits for a credential only a person can provide says which:
// no bundle carries one, so an import would otherwise wait two hours for a
// reason nobody is told.
func TestAnAppWaitingForASuppliedCredentialNamesIt(t *testing.T) {
	shop := profile("shop", nil)
	shop.Spec.Secrets = &gentianov1alpha1.ComponentSecrets{Supplied: []gentianov1alpha1.SuppliedSecret{{Name: "payments", DisplayName: "Payment keys"}}}
	wiki := profile("wiki", nil)
	wiki.Spec.Secrets = &gentianov1alpha1.ComponentSecrets{Supplied: []gentianov1alpha1.SuppliedSecret{{Name: "token"}}}
	s := appsService(t, demoTenant(), shop, wiki,
		component("shop", "Installing", "waiting for the release", false),
		// Up: whatever it was given, it waits for nothing.
		component("wiki", "Ready", "release deployed", true),
	)
	states, err := s.AppStates(context.Background(), "demo")
	if err != nil {
		t.Fatal(err)
	}
	got := byProfile(t, states)
	if a := got["shop"].AwaitingCredentials; len(a) != 1 || a[0].Name != "payments" || a[0].DisplayName != "Payment keys" || a[0].Requirement != "app-demo-shop-payments" {
		t.Errorf("shop awaits %+v", a)
	}
	if a := got["wiki"].AwaitingCredentials; len(a) != 0 {
		t.Errorf("a ready app awaits %+v", a)
	}
}

// Each pending request says who answers it, so a console offers the approval
// to that person and nobody else; the plain list stays as it was for a reader
// that knows only that.
func TestEachPendingPrivilegeSaysWhoAnswersIt(t *testing.T) {
	crm := component("crm", "PrivilegesPending", "waiting for approval", false)
	crm.Status.PendingPrivileges = []string{"egress/smtp-relay", "podSecurity/run-as-root"}
	s := appsService(t, demoTenant(), crm, component("wiki", "Ready", "release deployed", true))
	states, err := s.AppStates(context.Background(), "demo")
	if err != nil {
		t.Fatal(err)
	}
	got := byProfile(t, states)
	if len(got["crm"].PendingPrivileges) != 2 {
		t.Fatalf("pendingPrivileges = %v", got["crm"].PendingPrivileges)
	}
	want := []PendingPrivilege{
		{Privilege: "egress/smtp-relay", ApprovedBy: security.PrivilegeApproverPerimeter},
		{Privilege: "podSecurity/run-as-root", ApprovedBy: security.PrivilegeApproverSecurityOfficer},
	}
	approvals := got["crm"].PendingPrivilegeApprovals
	if len(approvals) != len(want) {
		t.Fatalf("pendingPrivilegeApprovals = %+v, want %+v", approvals, want)
	}
	for i := range want {
		if approvals[i] != want[i] {
			t.Errorf("pendingPrivilegeApprovals[%d] = %+v, want %+v", i, approvals[i], want[i])
		}
	}
	if got["wiki"].PendingPrivilegeApprovals != nil {
		t.Errorf("an app waiting for nothing lists %+v", got["wiki"].PendingPrivilegeApprovals)
	}
}
