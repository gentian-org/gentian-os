/*
Copyright 2026 The Gentian Authors.

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

package controller_test

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/controller"
)

// testClient is the shared client used by all tests in this package.
var testClient client.Client

// ldapManualTestTenants lists tenant names whose LDAP base jobs must not be
// auto-completed because ldap_reconciler_test.go asserts provisioning order.
var ldapManualTestTenants = map[string]struct{}{
	"adminpolicy": {},
	"bindtest":    {},
}

func ldapBaseJobTenant(jobName string) (tenant string, ok bool) {
	for _, prefix := range []string{
		"ldap-ou-",
		"ldap-app-user-template-",
		"ldap-app-user-capabilities-",
		"ldap-admin-user-",
		"ldap-admin-policy-",
	} {
		if strings.HasPrefix(jobName, prefix) {
			return strings.TrimPrefix(jobName, prefix), true
		}
	}
	return "", false
}

func shouldAutoCompleteLDAPJob(jobName string) bool {
	tenant, ok := ldapBaseJobTenant(jobName)
	if !ok {
		return false
	}
	_, manual := ldapManualTestTenants[tenant]
	return !manual
}

func markJobSucceeded(job *batchv1.Job) {
	now := metav1.Now()
	job.Status.StartTime = &now
	job.Status.CompletionTime = &now
	job.Status.Succeeded = 1
	job.Status.Conditions = []batchv1.JobCondition{
		{Type: batchv1.JobComplete, Status: corev1.ConditionTrue, LastProbeTime: now, LastTransitionTime: now},
		{Type: batchv1.JobSuccessCriteriaMet, Status: corev1.ConditionTrue, LastProbeTime: now, LastTransitionTime: now},
	}
}

// TestMain sets up a single envtest environment and controller manager shared
// across all tests. Each test creates its own Tenant with a unique name.
func TestMain(m *testing.M) {
	ctrl.SetLogger(zap.New(zap.UseDevMode(true), zap.WriteTo(io.Discard)))

	binDir := "/tmp/envtest-bins/k8s/1.32.0-linux-amd64"
	if v := os.Getenv("KUBEBUILDER_ASSETS"); v != "" {
		binDir = v
	}

	if err := gentianov1alpha1.AddToScheme(scheme.Scheme); err != nil {
		panic(err)
	}
	if err := networkingv1.AddToScheme(scheme.Scheme); err != nil {
		panic(err)
	}

	env := &envtest.Environment{
		CRDDirectoryPaths: []string{
			filepath.Join("..", "..", "config", "crd"),
		},
		BinaryAssetsDirectory: binDir,
		ErrorIfCRDPathMissing: true,
	}

	cfg, err := env.Start()
	if err != nil {
		panic(err)
	}

	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:  scheme.Scheme,
		Metrics: metricsserver.Options{BindAddress: "0"},
	})
	if err != nil {
		panic(err)
	}

	if err := (&controller.TenantReconciler{
		Client:                   mgr.GetClient(),
		Scheme:                   mgr.GetScheme(),
		KernelDomain:             "desk.gentian.org",
		TenantDNS01ClusterIssuer: "letsencrypt-dns01-cloudflare",
		KernelRealm:              "kernel",
	}).SetupWithManager(mgr); err != nil {
		panic(err)
	}

	if err := (&controller.AppStoreReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		panic(err)
	}

	testClient = mgr.GetClient()

	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = mgr.Start(ctx) }()

	// platform-kernel namespace is required by the identity reconciler for Keycloak Jobs.
	if err := testClient.Create(context.Background(), &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: "platform-kernel"},
	}); err != nil {
		panic(err)
	}

	// argocd namespace is required by the cache reconciler for Memcached Application CRs.
	if err := testClient.Create(context.Background(), &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: "argocd"},
	}); err != nil {
		panic(err)
	}

	// udm-admin Secret is required by the mail reconciler for Dovecot LDAP config.
	if err := testClient.Create(context.Background(), &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "udm-admin", Namespace: "platform-kernel"},
		Data: map[string][]byte{
			"ldapHost":          []byte("nubus-dev-ldap-server.gentian-dev.svc.cluster.local"),
			"ldapBase":          []byte("dc=swp-ldap,dc=internal"),
			"ldapsearchDovecot": []byte("test-ldap-password"),
		},
	}); err != nil {
		panic(err)
	}

	// dovecot-admin Secret provides OIDC + doveadm credentials for the opendesk-dovecot chart.
	if err := testClient.Create(context.Background(), &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "dovecot-admin", Namespace: "platform-kernel"},
		Data: map[string][]byte{
			"doveadm_password":   []byte("test-doveadm-password"),
			"oidc_client_secret": []byte("test-oidc-secret"),
		},
	}); err != nil {
		panic(err)
	}

	// keycloak-admin Secret provides the Keycloak URL for OIDC token introspection.
	if err := testClient.Create(context.Background(), &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "keycloak-admin", Namespace: "platform-kernel"},
		Data: map[string][]byte{
			"url":      []byte("http://nubus-dev-keycloak.gentian-dev.svc.cluster.local:8080"),
			"username": []byte("kcadmin"),
			"password": []byte("test-kc-password"),
		},
	}); err != nil {
		panic(err)
	}

	// Auto-complete Keycloak Jobs and LDAP base provisioning Jobs for tests that
	// do not assert job ordering manually. ensureIdentity blocks on the LDAP
	// admin-user Job when ensureLDAPBase has created it; without auto-completion
	// most controller tests time out waiting for Phase=Ready.
	go func() {
		for {
			time.Sleep(200 * time.Millisecond)
			var jobs batchv1.JobList
			if err := testClient.List(context.Background(), &jobs, client.InNamespace("platform-kernel")); err == nil {
				for _, job := range jobs.Items {
					j := job // copy loop variable
					if j.Status.Succeeded > 0 {
						continue
					}
					name := j.Name
					autoKeycloak := strings.HasPrefix(name, "keycloak-")
					autoLDAP := shouldAutoCompleteLDAPJob(name)
					if !autoKeycloak && !autoLDAP {
						continue
					}
					if autoKeycloak && (strings.Contains(name, "clienttest") || strings.Contains(name, "admintest") || strings.Contains(name, "identretain") || strings.Contains(name, "del-tenant")) {
						// These tests control Keycloak job timing or test deletion flows.
						continue
					}
					markJobSucceeded(&j)
					_ = testClient.Status().Update(context.Background(), &j)
				}
			}
		}
	}()

	code := m.Run()

	cancel()
	_ = env.Stop()
	os.Exit(code)
}

// waitFor polls cond every 200ms until it returns true or timeout elapses.
// It logs progress every 5s so CI output shows the test is alive.
func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	logEvery := 5 * time.Second
	nextLog := time.Now().Add(logEvery)
	for {
		if cond() {
			return
		}
		if time.Now().After(deadline) {
			break
		}
		if time.Now().After(nextLog) {
			t.Logf("still waiting... (%s remaining)", time.Until(deadline).Round(time.Second))
			nextLog = time.Now().Add(logEvery)
		}
		time.Sleep(200 * time.Millisecond)
	}
	if !cond() {
		t.Fatal("timed out waiting for condition")
	}
}

// ----- Tests -----------------------------------------------------------------

func TestTenantReconciler_CreatesNamespace(t *testing.T) {
	t.Parallel()
	tenant := &gentianov1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: "acme"},
		Spec: gentianov1alpha1.TenantSpec{
			DisplayName: "GTN Demo",
			Domain:      "acme.example.com",
			AdminEmail:  "admin@acme.example.com",
		},
	}
	if err := testClient.Create(context.Background(), tenant); err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	t.Cleanup(func() { _ = testClient.Delete(context.Background(), tenant) })

	ns := &corev1.Namespace{}
	waitFor(t, 10*time.Second, func() bool {
		return testClient.Get(context.Background(), types.NamespacedName{Name: "tenant-acme"}, ns) == nil
	})

	if ns.Labels["gentianos.io/tenant"] != "acme" {
		t.Errorf("expected tenant label acme, got %q", ns.Labels["gentianos.io/tenant"])
	}
	if ns.Labels["app.kubernetes.io/managed-by"] != "gentian-os" {
		t.Errorf("expected managed-by label, got %q", ns.Labels["app.kubernetes.io/managed-by"])
	}
}

func TestTenantReconciler_SetsStatusReady(t *testing.T) {
	t.Parallel()
	tenant := &gentianov1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: "beta"},
		Spec: gentianov1alpha1.TenantSpec{
			DisplayName: "Beta Co",
			Domain:      "beta.example.com",
			AdminEmail:  "admin@beta.example.com",
		},
	}
	if err := testClient.Create(context.Background(), tenant); err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	t.Cleanup(func() { _ = testClient.Delete(context.Background(), tenant) })

	updated := &gentianov1alpha1.Tenant{}
	waitFor(t, 10*time.Second, func() bool {
		_ = testClient.Get(context.Background(), types.NamespacedName{Name: "beta"}, updated)
		return updated.Status.Phase == gentianov1alpha1.TenantPhaseReady
	})

	if updated.Status.Namespace != "tenant-beta" {
		t.Errorf("expected status.namespace tenant-beta, got %q", updated.Status.Namespace)
	}

	var nsReadyCond *metav1.Condition
	for i := range updated.Status.Conditions {
		if updated.Status.Conditions[i].Type == "NamespaceReady" {
			nsReadyCond = &updated.Status.Conditions[i]
			break
		}
	}
	if nsReadyCond == nil {
		t.Fatal("expected NamespaceReady condition")
	}
	if nsReadyCond.Status != metav1.ConditionTrue {
		t.Errorf("expected NamespaceReady=True, got %v", nsReadyCond.Status)
	}
}

func TestTenantReconciler_AppliesResourceQuota(t *testing.T) {
	t.Parallel()
	storage := resource.MustParse("50Gi")
	cpu := resource.MustParse("4")
	memory := resource.MustParse("8Gi")

	tenant := &gentianov1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: "gamma"},
		Spec: gentianov1alpha1.TenantSpec{
			DisplayName: "Gamma Inc",
			Domain:      "gamma.example.com",
			AdminEmail:  "admin@gamma.example.com",
			Quotas: &gentianov1alpha1.TenantQuotas{
				Storage: &storage,
				CPU:     &cpu,
				Memory:  &memory,
			},
		},
	}
	if err := testClient.Create(context.Background(), tenant); err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	t.Cleanup(func() { _ = testClient.Delete(context.Background(), tenant) })

	rq := &corev1.ResourceQuota{}
	waitFor(t, 10*time.Second, func() bool {
		return testClient.Get(context.Background(), types.NamespacedName{Name: "tenant-quota", Namespace: "tenant-gamma"}, rq) == nil
	})

	if rq.Spec.Hard[corev1.ResourceRequestsStorage] != storage {
		t.Errorf("storage quota mismatch: got %v", rq.Spec.Hard[corev1.ResourceRequestsStorage])
	}
	if rq.Spec.Hard[corev1.ResourceLimitsCPU] != cpu {
		t.Errorf("cpu quota mismatch: got %v", rq.Spec.Hard[corev1.ResourceLimitsCPU])
	}
}

func TestTenantReconciler_AppliesLimitRange(t *testing.T) {
	t.Parallel()
	tenant := &gentianov1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: "delta"},
		Spec: gentianov1alpha1.TenantSpec{
			DisplayName: "Delta Ltd",
			Domain:      "delta.example.com",
			AdminEmail:  "admin@delta.example.com",
		},
	}
	if err := testClient.Create(context.Background(), tenant); err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	t.Cleanup(func() { _ = testClient.Delete(context.Background(), tenant) })

	lr := &corev1.LimitRange{}
	waitFor(t, 10*time.Second, func() bool {
		return testClient.Get(context.Background(), types.NamespacedName{Name: "tenant-limits", Namespace: "tenant-delta"}, lr) == nil
	})

	if len(lr.Spec.Limits) == 0 {
		t.Fatal("expected at least one LimitRange item")
	}
	item := lr.Spec.Limits[0]
	if item.Type != corev1.LimitTypeContainer {
		t.Errorf("expected Container limit type, got %v", item.Type)
	}
	if _, ok := item.Default[corev1.ResourceCPU]; !ok {
		t.Error("expected default CPU limit")
	}
}

func TestTenantReconciler_AppliesNetworkPolicy(t *testing.T) {
	t.Parallel()
	tenant := &gentianov1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: "epsilon"},
		Spec: gentianov1alpha1.TenantSpec{
			DisplayName: "Epsilon GmbH",
			Domain:      "epsilon.example.com",
			AdminEmail:  "admin@epsilon.example.com",
		},
	}
	if err := testClient.Create(context.Background(), tenant); err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	t.Cleanup(func() { _ = testClient.Delete(context.Background(), tenant) })

	np := &networkingv1.NetworkPolicy{}
	waitFor(t, 10*time.Second, func() bool {
		return testClient.Get(context.Background(), types.NamespacedName{Name: "tenant-isolation", Namespace: "tenant-epsilon"}, np) == nil
	})

	if len(np.Spec.Ingress) == 0 {
		t.Fatal("expected ingress rules")
	}
	if len(np.Spec.Egress) == 0 {
		t.Fatal("expected egress rules")
	}
	if !containsPolicyType(np.Spec.PolicyTypes, networkingv1.PolicyTypeIngress) {
		t.Error("expected Ingress policy type")
	}
	if !containsPolicyType(np.Spec.PolicyTypes, networkingv1.PolicyTypeEgress) {
		t.Error("expected Egress policy type")
	}
}

func TestTenantReconciler_ProfilesMissingBlocksProvisioning(t *testing.T) {
	t.Parallel()
	tenant := &gentianov1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: "missing-profile"},
		Spec: gentianov1alpha1.TenantSpec{
			DisplayName: "Missing Profile Co",
			Domain:      "missing-profile.example.com",
			AdminEmail:  "admin@missing-profile.example.com",
			Apps: []gentianov1alpha1.TenantApp{
				{Profile: "does-not-exist"},
			},
		},
	}
	if err := testClient.Create(context.Background(), tenant); err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	t.Cleanup(func() { _ = testClient.Delete(context.Background(), tenant) })

	updated := &gentianov1alpha1.Tenant{}
	waitFor(t, 10*time.Second, func() bool {
		_ = testClient.Get(context.Background(), types.NamespacedName{Name: "missing-profile"}, updated)
		return updated.Status.Phase == gentianov1alpha1.TenantPhaseDegraded
	})

	if updated.Status.Phase != gentianov1alpha1.TenantPhaseDegraded {
		t.Fatalf("expected Phase=Degraded, got %v", updated.Status.Phase)
	}

	var appsCond *metav1.Condition
	for i := range updated.Status.Conditions {
		if updated.Status.Conditions[i].Type == "AppsReady" {
			appsCond = &updated.Status.Conditions[i]
			break
		}
	}
	if appsCond == nil {
		t.Fatal("expected AppsReady condition")
	}
	if appsCond.Reason != "ProfileNotFound" {
		t.Errorf("expected AppsReady reason ProfileNotFound, got %q", appsCond.Reason)
	}

	ns := &corev1.Namespace{}
	err := testClient.Get(context.Background(), types.NamespacedName{Name: "tenant-missing-profile"}, ns)
	if err == nil {
		t.Fatal("expected tenant namespace to not be created when profiles are missing")
	}
	if !k8serrors.IsNotFound(err) {
		t.Fatalf("expected NotFound for tenant namespace, got: %v", err)
	}
}

func TestTenantReconciler_CustomNamespace(t *testing.T) {
	t.Parallel()
	tenant := &gentianov1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: "zeta"},
		Spec: gentianov1alpha1.TenantSpec{
			DisplayName: "Zeta Corp",
			Domain:      "zeta.example.com",
			AdminEmail:  "admin@zeta.example.com",
			Isolation:   &gentianov1alpha1.TenantIsolation{Namespace: "zeta-custom"},
		},
	}
	if err := testClient.Create(context.Background(), tenant); err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	t.Cleanup(func() { _ = testClient.Delete(context.Background(), tenant) })

	ns := &corev1.Namespace{}
	waitFor(t, 10*time.Second, func() bool {
		return testClient.Get(context.Background(), types.NamespacedName{Name: "zeta-custom"}, ns) == nil
	})

	if ns.Labels["gentianos.io/tenant"] != "zeta" {
		t.Errorf("expected tenant label zeta, got %q", ns.Labels["gentianos.io/tenant"])
	}
}

func TestTenantReconciler_DeleteRetainKeepsNamespace(t *testing.T) {
	t.Parallel()
	tenant := &gentianov1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: "retainer"},
		Spec: gentianov1alpha1.TenantSpec{
			DisplayName:    "Retainer LLC",
			Domain:         "retainer.example.com",
			AdminEmail:     "admin@retainer.example.com",
			DeletionPolicy: gentianov1alpha1.DeletionPolicyRetain,
		},
	}
	if err := testClient.Create(context.Background(), tenant); err != nil {
		t.Fatalf("create tenant: %v", err)
	}

	// Wait for namespace to exist
	waitFor(t, 10*time.Second, func() bool {
		return testClient.Get(context.Background(), types.NamespacedName{Name: "tenant-retainer"}, &corev1.Namespace{}) == nil
	})

	// Delete the tenant
	if err := testClient.Delete(context.Background(), tenant); err != nil {
		t.Fatalf("delete tenant: %v", err)
	}
	// For Retain policy deleteLDAP preserves the admin user (no delete job created).
	// For Retain policy deleteIdentity is a no-op: retainer has no apps so no realm was provisioned.

	// Wait for Tenant CR to be gone (finalizer removed)
	waitFor(t, 15*time.Second, func() bool {
		err := testClient.Get(context.Background(), types.NamespacedName{Name: "retainer"}, &gentianov1alpha1.Tenant{})
		return err != nil // NotFound = gone
	})

	// Namespace must still exist
	ns := &corev1.Namespace{}
	if err := testClient.Get(context.Background(), types.NamespacedName{Name: "tenant-retainer"}, ns); err != nil {
		t.Errorf("namespace should be retained after Retain delete, but got error: %v", err)
	}
}

func TestTenantReconciler_DeleteDeleteRemovesNamespace(t *testing.T) {
	t.Parallel()
	tenant := &gentianov1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: "destroyer"},
		Spec: gentianov1alpha1.TenantSpec{
			DisplayName:    "Destroyer Co",
			Domain:         "destroyer.example.com",
			AdminEmail:     "admin@destroyer.example.com",
			DeletionPolicy: gentianov1alpha1.DeletionPolicyDelete,
		},
	}
	if err := testClient.Create(context.Background(), tenant); err != nil {
		t.Fatalf("create tenant: %v", err)
	}

	// Wait for namespace to be created
	waitFor(t, 10*time.Second, func() bool {
		return testClient.Get(context.Background(), types.NamespacedName{Name: "tenant-destroyer"}, &corev1.Namespace{}) == nil
	})

	// Delete the tenant
	if err := testClient.Delete(context.Background(), tenant); err != nil {
		t.Fatalf("delete tenant: %v", err)
	}
	// For Delete policy deleteIdentity and deleteLDAP create cleanup jobs.
	go markJobCompleteWhenReady("keycloak-realm-delete-destroyer", "platform-kernel")
	go markJobCompleteWhenReady("ldap-ou-delete-destroyer", "platform-kernel")
	go markJobCompleteWhenReady("nc-group-delete-destroyer", "platform-kernel")

	// Wait for Tenant CR to be gone
	waitFor(t, 15*time.Second, func() bool {
		err := testClient.Get(context.Background(), types.NamespacedName{Name: "destroyer"}, &gentianov1alpha1.Tenant{})
		return err != nil
	})

	// Namespace should be terminating or gone
	ns := &corev1.Namespace{}
	err := testClient.Get(context.Background(), types.NamespacedName{Name: "tenant-destroyer"}, ns)
	if err == nil && ns.DeletionTimestamp == nil {
		t.Error("namespace should be deleted or terminating after Delete policy")
	}
}

// containsPolicyType checks if the policy type is in the list.
func containsPolicyType(types []networkingv1.PolicyType, t networkingv1.PolicyType) bool {
	for _, pt := range types {
		if pt == t {
			return true
		}
	}
	return false
}
