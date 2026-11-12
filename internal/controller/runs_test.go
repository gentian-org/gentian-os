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
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/componentkeys"
	"github.com/gentian-org/gentian-os/internal/dispatcher"
)

const (
	stepDigest   = "sha256:0000000000000000000000000000000000000000000000000000000000000000"
	runnerDigest = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	programImage = "registry.example/gentian-os@sha256:2222222222222222222222222222222222222222222222222222222222222222"
)

// runsProfile is a component that starts runs and calls back at flow:80,
// with one run of its own.
func runsProfile(name string) *gentianov1alpha1.ComponentProfile {
	profile := &gentianov1alpha1.ComponentProfile{ObjectMeta: metav1.ObjectMeta{Name: name}}
	profile.Spec.Version = "2.0.0"
	profile.Spec.TrustTier = gentianov1alpha1.TrustTierCertified
	profile.Spec.Package.Images = []gentianov1alpha1.PinnedImage{{Repository: "registry.example/flow/runner", Digest: runnerDigest}}
	profile.Spec.Requires = &gentianov1alpha1.RequirementSpec{Services: &gentianov1alpha1.ServiceRequirements{
		Runs: &gentianov1alpha1.RunsRequirement{
			Callback:    gentianov1alpha1.RunsCallback{Service: name, Port: 80, Path: "/runs"},
			Concurrency: 3,
			Own: []gentianov1alpha1.OwnRun{{
				Name: "runner", Image: "registry.example/flow/runner", Command: []string{"/runner"}, DeadlineSeconds: 900,
			}},
		},
	}}
	return profile
}

// actionFixture is an installed, Ready action of a tenant, with one egress
// it asks for and whether that was granted.
func actionFixture(tenant, name string, granted bool) (*gentianov1alpha1.Component, *gentianov1alpha1.ComponentProfile) {
	profile := profileFixture(name, false, gentianov1alpha1.ComponentClassAction)
	profile.Spec.Version = "1.0.0"
	profile.Spec.Package.Images = []gentianov1alpha1.PinnedImage{{Repository: "registry.example/steps/" + name, Digest: stepDigest}}
	cpu := resource.MustParse("500m")
	profile.Spec.Action = &gentianov1alpha1.ActionSpec{
		Command: []string{"/" + name}, DeadlineSeconds: 120, Resources: &gentianov1alpha1.ActionResources{CPU: &cpu},
	}
	tcp, port := corev1.ProtocolTCP, intstr.FromInt32(443)
	profile.Spec.Requires = &gentianov1alpha1.RequirementSpec{Privileges: &gentianov1alpha1.PrivilegeRequest{
		Egress: []gentianov1alpha1.EgressRequest{{Name: "service", Reason: "the service it calls", Rule: networkingv1.NetworkPolicyEgressRule{
			Ports: []networkingv1.NetworkPolicyPort{{Protocol: &tcp, Port: &port}},
		}}},
	}}
	comp := rightsComponent(tenant, name)
	comp.Spec.Class = gentianov1alpha1.ComponentClassAction
	comp.Status.Conditions = []metav1.Condition{{Type: "Ready", Status: metav1.ConditionTrue, Reason: "Ready"}}
	if granted {
		comp.Spec.Privileges = []gentianov1alpha1.PrivilegeGrant{{
			Privilege: "egress/service", Approver: "ada", ApprovedAt: metav1.Now(), Reason: "reviewed",
		}}
	}
	return comp, profile
}

func runsHarness(t *testing.T, objects ...client.Object) (client.Client, *RunsReconciler) {
	t.Helper()
	c := fake.NewClientBuilder().WithScheme(rightsScheme(t)).WithObjects(objects...).
		WithStatusSubresource(&gentianov1alpha1.Component{}).Build()
	return c, &RunsReconciler{Client: c, ProgramImage: programImage, Namespace: "kernel-control", DispatcherAccount: "gentian-os-dispatcher"}
}

func runsTable(t *testing.T, c client.Client) *dispatcher.Table {
	t.Helper()
	cm := &corev1.ConfigMap{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: dispatcher.TableConfigMap, Namespace: "kernel-control"}, cm); err != nil {
		t.Fatalf("the dispatcher's table: %v", err)
	}
	table, err := dispatcher.ParseTable([]byte(cm.Data[dispatcher.TableKey]))
	if err != nil {
		t.Fatalf("the table the operator wrote is not one: %v\n%s", err, cm.Data[dispatcher.TableKey])
	}
	return table
}

func policies(t *testing.T, c client.Client, namespace string) map[string]*networkingv1.NetworkPolicy {
	t.Helper()
	list := &networkingv1.NetworkPolicyList{}
	if err := c.List(context.Background(), list, client.InNamespace(namespace)); err != nil {
		t.Fatal(err)
	}
	out := map[string]*networkingv1.NetworkPolicy{}
	for i := range list.Items {
		out[list.Items[i].Name] = &list.Items[i]
	}
	return out
}

// A component that starts runs is given a key once and the dispatcher's
// address. One whose own run names an image its profile does not pin is
// refused, and one that declares nothing is given nothing.
func TestARunsKeyIsMadeOnceAndAnUnpinnedRunIsRefused(t *testing.T) {
	ctx := context.Background()
	scheme := rightsScheme(t)
	flow, plain := rightsComponent("acme", "flow"), rightsComponent("acme", "plain")
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(flow, plain).Build()
	r := &ComponentReconciler{Client: c, Scheme: scheme}

	if err := r.ensureRuns(ctx, plain, "acme", rightsProfile("plain", false)); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, types.NamespacedName{Name: "runs-plain", Namespace: "tenant-acme"}, &corev1.Secret{}); !errors.IsNotFound(err) {
		t.Fatalf("a component that starts no runs was given a key: %v", err)
	}

	read := func() *corev1.Secret {
		secret := &corev1.Secret{}
		if err := c.Get(ctx, types.NamespacedName{Name: "runs-flow", Namespace: "tenant-acme"}, secret); err != nil {
			t.Fatal(err)
		}
		return secret
	}
	if err := r.ensureRuns(ctx, flow, "acme", runsProfile("flow")); err != nil {
		t.Fatal(err)
	}
	first := read()
	if len(first.Data[dispatcher.KeyKey]) < 32 || first.Labels[runsCallerLabel] != "true" ||
		string(first.Data[dispatcher.URLKey]) != "http://gentian-os-dispatcher.kernel-control.svc.cluster.local:8080" {
		t.Fatalf("secret = %v / %v", first.Labels, first.Data)
	}
	if err := r.ensureRuns(ctx, flow, "acme", runsProfile("flow")); err != nil {
		t.Fatal(err)
	}
	if string(read().Data[dispatcher.KeyKey]) != string(first.Data[dispatcher.KeyKey]) {
		t.Fatal("the key changed on a second pass")
	}

	if refusal := runsRefusal(runsProfile("flow")); refusal != "" {
		t.Fatalf("a pinned run was refused: %s", refusal)
	}
	unpinned := runsProfile("flow")
	unpinned.Spec.Package.Images = nil
	if refusal := runsRefusal(unpinned); !strings.Contains(refusal, "does not pin") {
		t.Fatalf("refusal = %q", refusal)
	}

	rule := runsEgress()
	if got := rule.To[0].PodSelector.MatchLabels["app.kubernetes.io/component"]; got != "dispatcher" ||
		rule.To[0].NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"] != "kernel-control" ||
		rule.Ports[0].Port.IntVal != dispatcher.Port {
		t.Fatalf("the way to the dispatcher = %+v", rule)
	}
}

// The table names who may start runs by the hash of its key, where its runs
// call back, and what may run in its tenant: the installed actions that are
// Ready, and each caller's own runs.
func TestTheDispatchersTableSaysWhoMayStartWhat(t *testing.T) {
	ctx := context.Background()
	flow := rightsComponent("acme", "flow")
	ocr, ocrProfile := actionFixture("acme", "ocr", true)
	pending, pendingProfile := actionFixture("acme", "pending", false)
	pending.Status.Conditions[0].Status = metav1.ConditionFalse
	elsewhere, elsewhereProfile := actionFixture("other", "translate", false)
	service := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "flow", Namespace: "tenant-acme"}, Spec: corev1.ServiceSpec{
		Ports: []corev1.ServicePort{{Port: 80, TargetPort: intstr.FromString("http")}},
	}}
	c, r := runsHarness(t, rightsTenant("acme"), rightsTenant("other"), flow, runsProfile("flow"),
		ocr, ocrProfile, pending, pendingProfile, elsewhere, elsewhereProfile, service)
	if err := (&ComponentReconciler{Client: c, Scheme: c.Scheme()}).ensureRuns(ctx, flow, "acme", runsProfile("flow")); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(ctx, runsRequest); err != nil {
		t.Fatal(err)
	}

	table := runsTable(t, c)
	if len(table.Callers) != 1 || len(table.Tenants) != 1 {
		t.Fatalf("table = %+v", table)
	}
	secret := &corev1.Secret{}
	if err := c.Get(ctx, types.NamespacedName{Name: "runs-flow", Namespace: "tenant-acme"}, secret); err != nil {
		t.Fatal(err)
	}
	key := string(secret.Data[dispatcher.KeyKey])
	caller := table.CallerByKey(key)
	if caller == nil || caller.Tenant != "acme" || caller.Component != "flow" || caller.KeyHash != componentkeys.Hash(key) ||
		caller.Concurrency != 3 || caller.Callback.URL() != "http://flow.tenant-acme.svc:80/runs" {
		t.Fatalf("caller = %+v", caller)
	}
	if table.CallerByKey("not-the-key") != nil {
		t.Fatal("a key nobody holds found a caller")
	}
	// The other tenant has an action and nobody to start it: it is not in
	// the table, and has no namespace for runs.
	if table.TenantOf("other") != nil {
		t.Fatalf("a tenant without a caller is in the table: %+v", table.Tenants)
	}
	if err := c.Get(ctx, types.NamespacedName{Name: "tenant-other-runs"}, &corev1.Namespace{}); !errors.IsNotFound(err) {
		t.Fatalf("a namespace for runs was made for a tenant that starts none: %v", err)
	}

	action, own, found := table.Runnable(caller, "ocr")
	if !found || own || action.Ref() != "registry.example/steps/ocr@"+stepDigest || action.CPU != "500m" ||
		action.DeadlineSeconds != 120 || action.Command[0] != "/ocr" {
		t.Fatalf("ocr = %+v own=%v found=%v", action, own, found)
	}
	runner, own, found := table.Runnable(caller, "runner")
	if !found || !own || runner.Ref() != "registry.example/flow/runner@"+runnerDigest || runner.DeadlineSeconds != 900 {
		t.Fatalf("runner = %+v own=%v found=%v", runner, own, found)
	}
	for _, name := range []string{"pending", "translate", "nothing"} {
		if _, _, found := table.Runnable(caller, name); found {
			t.Errorf("%s can be run: it is not Ready, another tenant's, or not there", name)
		}
	}
}

// A run's image is pulled with the credentials of the repositories its
// tenant declared, as an app's pods are: a repository of the whole tenant
// for every run, one bound to a profile for that profile's runs alone, and
// never another tenant's or the cluster's.
func TestARunIsPulledWithItsTenantsRepositories(t *testing.T) {
	ctx := context.Background()
	flow := rightsComponent("acme", "flow")
	ocr, ocrProfile := actionFixture("acme", "ocr", true)
	scan, scanProfile := actionFixture("acme", "scan", true)
	bound := ociRepositorySpec("acme", "https://registry.example/steps/scan")
	bound["app"] = "scan"
	c, r := runsHarness(t, rightsTenant("acme"), flow, runsProfile("flow"), ocr, ocrProfile, scan, scanProfile,
		repositoryClaimFixture(repositoryClaimNamespace, "vendor", ociRepositorySpec("acme", "https://registry.example")),
		repositoryClaimFixture(repositoryClaimNamespace, "scan-only", bound),
		repositoryClaimFixture(repositoryClaimNamespace, "theirs", ociRepositorySpec("other", "https://registry.example")),
		repositoryClaimFixture(repositoryClaimNamespace, "cluster", ociRepositorySpec("", "https://registry.example")))
	if err := (&ComponentReconciler{Client: c, Scheme: c.Scheme()}).ensureRuns(ctx, flow, "acme", runsProfile("flow")); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(ctx, runsRequest); err != nil {
		t.Fatal(err)
	}
	table := runsTable(t, c)
	caller := &table.Callers[0]
	for name, want := range map[string]string{
		"ocr":    "repository-vendor-pull",
		"runner": "repository-vendor-pull",
		"scan":   "repository-scan-only-pull",
	} {
		runnable, _, found := table.Runnable(caller, name)
		if !found || strings.Join(runnable.PullSecrets, ",") != want {
			t.Errorf("%s is pulled with %v, want %s", name, runnable.PullSecrets, want)
		}
	}
}

// The namespace for runs lets nothing in, lets a pod ask the DNS and nothing
// else, gives a run of an action the egress granted to that action, and lets
// a caller's run through to that caller on the port its Service sends to.
func TestTheNamespaceForRunsAndItsRules(t *testing.T) {
	ctx := context.Background()
	flow := rightsComponent("acme", "flow")
	ocr, ocrProfile := actionFixture("acme", "ocr", true)
	plain, plainProfile := actionFixture("acme", "plain-step", false)
	service := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "flow", Namespace: "tenant-acme"}, Spec: corev1.ServiceSpec{
		Ports: []corev1.ServicePort{{Port: 80, TargetPort: intstr.FromString("http")}},
	}}
	tenant := rightsTenant("acme")
	memory := resource.MustParse("1Gi")
	tenant.Spec.Quotas = &gentianov1alpha1.TenantQuotas{RunsMaxPods: 3, RunsMemory: &memory}
	c, r := runsHarness(t, tenant, flow, runsProfile("flow"), ocr, ocrProfile, plain, plainProfile, service)
	if err := (&ComponentReconciler{Client: c, Scheme: c.Scheme()}).ensureRuns(ctx, flow, "acme", runsProfile("flow")); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(ctx, runsRequest); err != nil {
		t.Fatal(err)
	}

	ns := &corev1.Namespace{}
	if err := c.Get(ctx, types.NamespacedName{Name: "tenant-acme-runs"}, ns); err != nil {
		t.Fatal(err)
	}
	if ns.Labels["gentianos.io/tier"] != "tenant-runs" || ns.Labels["gentianos.io/tenant"] != "acme" ||
		ns.Labels["pod-security.kubernetes.io/enforce"] != "restricted" {
		t.Fatalf("namespace labels = %v", ns.Labels)
	}
	account := &corev1.ServiceAccount{}
	if err := c.Get(ctx, types.NamespacedName{Name: "run", Namespace: ns.Name}, account); err != nil {
		t.Fatal(err)
	}
	if account.AutomountServiceAccountToken == nil || *account.AutomountServiceAccountToken {
		t.Fatal("a run's account mounts a token")
	}
	// The dispatcher's rights are bound here, to its account and its role.
	binding := &rbacv1.RoleBinding{}
	if err := c.Get(ctx, types.NamespacedName{Name: "gentian-dispatcher-runs", Namespace: ns.Name}, binding); err != nil {
		t.Fatal(err)
	}
	if binding.RoleRef.Kind != "ClusterRole" || binding.RoleRef.Name != "gentian-dispatcher-runs" || len(binding.Subjects) != 1 ||
		binding.Subjects[0].Name != "gentian-os-dispatcher" || binding.Subjects[0].Namespace != "kernel-control" {
		t.Fatalf("binding = %+v", binding)
	}
	if err := c.Get(ctx, types.NamespacedName{Name: "gentian-dispatcher-runs", Namespace: "tenant-acme"}, &rbacv1.RoleBinding{}); !errors.IsNotFound(err) {
		t.Fatalf("the dispatcher was given rights in the tenant's own namespace: %v", err)
	}
	quota := &corev1.ResourceQuota{}
	if err := c.Get(ctx, types.NamespacedName{Name: runsQuotaName, Namespace: ns.Name}, quota); err != nil {
		t.Fatal(err)
	}
	hard := quota.Spec.Hard
	if hard.Pods().Value() != 3 || hard[corev1.ResourceLimitsMemory] != memory || hard[corev1.ResourceRequestsMemory] != memory ||
		hard[corev1.ResourceLimitsCPU] != defaultRunsCPU {
		t.Fatalf("quota = %v", hard)
	}

	rules := policies(t, c, ns.Name)
	deny := rules[runsDefaultDeny]
	if deny == nil || len(deny.Spec.PodSelector.MatchLabels) != 0 || len(deny.Spec.PolicyTypes) != 2 ||
		len(deny.Spec.Ingress) != 0 || len(deny.Spec.Egress) != 1 || len(deny.Spec.Egress[0].To) != 0 ||
		deny.Spec.Egress[0].Ports[0].Port.IntVal != 53 {
		t.Fatalf("default = %+v", deny)
	}
	granted := rules["action-ocr"]
	if granted == nil || granted.Spec.PodSelector.MatchLabels[dispatcher.LabelAction] != "ocr" ||
		len(granted.Spec.Egress) != 1 || granted.Spec.Egress[0].Ports[0].Port.IntVal != 443 {
		t.Fatalf("action-ocr = %+v", granted)
	}
	if rules["action-plain-step"] != nil {
		t.Fatal("an action whose egress nobody granted was given it")
	}
	back := rules["caller-flow"]
	if back == nil || back.Spec.PodSelector.MatchLabels[dispatcher.LabelCaller] != "flow" || len(back.Spec.Egress) != 1 {
		t.Fatalf("caller-flow = %+v", back)
	}
	to := back.Spec.Egress[0]
	if to.To[0].NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"] != "tenant-acme" ||
		to.To[0].PodSelector.MatchLabels[appLabel] != "flow" || to.Ports[0].Port.StrVal != "http" {
		t.Fatalf("the way back = %+v", to)
	}
	if len(rules) != 3 {
		t.Fatalf("rules = %v", rules)
	}

	in := policies(t, c, "tenant-acme")["runs-flow"]
	if in == nil || in.Spec.PodSelector.MatchLabels[appLabel] != "flow" || len(in.Spec.Ingress) != 1 {
		t.Fatalf("runs-flow = %+v", in)
	}
	from := in.Spec.Ingress[0]
	if from.From[0].NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"] != "tenant-acme-runs" ||
		from.From[0].PodSelector.MatchLabels[dispatcher.LabelCaller] != "flow" || from.Ports[0].Port.StrVal != "http" {
		t.Fatalf("the caller's admission = %+v", from)
	}

	images := &unstructured.Unstructured{}
	images.SetGroupVersionKind(kyvernoPolicyGVK)
	if err := c.Get(ctx, types.NamespacedName{Name: runsImagesPolicy, Namespace: ns.Name}, images); err != nil {
		t.Fatal(err)
	}
	allowed, _, _ := unstructured.NestedSlice(images.Object, "spec", "rules")
	text := strings.Join(r.allowedImages(&tenantRuns{}), ",")
	if text != programImage || len(allowed) != 1 {
		t.Fatalf("allowed with nothing installed = %q, rules = %v", text, allowed)
	}
	raw, _ := images.MarshalJSON()
	for _, ref := range []string{programImage, "registry.example/steps/ocr@" + stepDigest, "registry.example/flow/runner@" + runnerDigest} {
		if !strings.Contains(string(raw), ref) {
			t.Errorf("the image rule does not admit %s", ref)
		}
	}

	// The grant runs out, the caller stops declaring runs: what was theirs
	// goes, and the namespace stays with nothing let through.
	r.Now = func() time.Time { return time.Now().Add(time.Hour) }
	ocr.Spec.Privileges[0].ExpiresAt = &metav1.Time{Time: time.Now().Add(time.Minute)}
	if err := c.Update(ctx, ocr); err != nil {
		t.Fatal(err)
	}
	stopped := runsProfile("flow")
	stopped.Spec.Requires = nil
	stopped.ResourceVersion = ""
	if err := c.Delete(ctx, runsProfile("flow")); err != nil {
		t.Fatal(err)
	}
	if err := c.Create(ctx, stopped); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(ctx, runsRequest); err != nil {
		t.Fatal(err)
	}
	rules = policies(t, c, ns.Name)
	if len(rules) != 1 || rules[runsDefaultDeny] == nil {
		t.Fatalf("rules after the caller stopped = %v", rules)
	}
	if len(policies(t, c, "tenant-acme")) != 0 {
		t.Fatal("the caller still admits runs")
	}
	if table := runsTable(t, c); len(table.Callers) != 0 || len(table.Tenants) != 0 {
		t.Fatalf("table after the caller stopped = %+v", table)
	}
}

// A callback Service that is not there yet, or has no such port, opens no
// way: the caller is in the table, and its runs reach nothing until it is.
func TestACallbackThatIsNotThereOpensNoWay(t *testing.T) {
	ctx := context.Background()
	flow := rightsComponent("acme", "flow")
	c, r := runsHarness(t, rightsTenant("acme"), flow, runsProfile("flow"))
	if err := (&ComponentReconciler{Client: c, Scheme: c.Scheme()}).ensureRuns(ctx, flow, "acme", runsProfile("flow")); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(ctx, runsRequest); err != nil {
		t.Fatal(err)
	}
	if rules := policies(t, c, "tenant-acme-runs"); len(rules) != 1 {
		t.Fatalf("rules = %v", rules)
	}
	if len(runsTable(t, c).Callers) != 1 {
		t.Fatal("the caller is not in the table")
	}
}

// The namespace for runs of a tenant that is gone goes with it, and only a
// namespace named and labeled as the operator's is deleted.
func TestTheNamespaceForRunsOfATenantThatIsGoneGoesWithIt(t *testing.T) {
	ctx := context.Background()
	mine := func(name, tenant string) *corev1.Namespace {
		return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{
			"gentianos.io/tier": "tenant-runs", "gentianos.io/tenant": tenant, "app.kubernetes.io/managed-by": managedByValue,
		}}}
	}
	foreign := mine("somebody-elses", "gone")
	unmanaged := mine("tenant-left-runs", "left")
	delete(unmanaged.Labels, "app.kubernetes.io/managed-by")
	c, r := runsHarness(t, rightsTenant("acme"), mine("tenant-acme-runs", "acme"), mine("tenant-gone-runs", "gone"), foreign, unmanaged)
	if _, err := r.Reconcile(ctx, runsRequest); err != nil {
		t.Fatal(err)
	}
	for name, kept := range map[string]bool{
		"tenant-acme-runs": true, "tenant-gone-runs": false, "somebody-elses": true, "tenant-left-runs": true,
	} {
		err := c.Get(ctx, types.NamespacedName{Name: name}, &corev1.Namespace{})
		if kept && err != nil || !kept && !errors.IsNotFound(err) {
			t.Errorf("%s: kept=%v, err=%v", name, kept, err)
		}
	}
}
