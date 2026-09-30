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

package applifecycle

import (
	"context"
	"errors"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
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
