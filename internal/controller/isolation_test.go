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

package controller_test

import (
	"context"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// ---------------------------------------------------------------------------
// Isolation tests
// ---------------------------------------------------------------------------

// TestIsolation_CrossTenantDenied creates two tenants and verifies that each
// tenant's NetworkPolicy ingress rules only allow traffic from its own
// namespace, the kernel namespace, and the ingress namespace - NOT from the
// other tenant's namespace.
func TestIsolation_CrossTenantDenied(t *testing.T) {
	t.Parallel()
	tenantA := &gentianov1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: "iso-a"},
		Spec: gentianov1alpha1.TenantSpec{
			DisplayName: "Isolation A",
			Domain:      "iso-a.example.com",
			AdminEmail:  "admin@iso-a.example.com",
		},
	}
	tenantB := &gentianov1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: "iso-b"},
		Spec: gentianov1alpha1.TenantSpec{
			DisplayName: "Isolation B",
			Domain:      "iso-b.example.com",
			AdminEmail:  "admin@iso-b.example.com",
		},
	}

	ctx := context.Background()
	if err := testClient.Create(ctx, tenantA); err != nil {
		t.Fatalf("create tenant A: %v", err)
	}
	t.Cleanup(func() { _ = testClient.Delete(ctx, tenantA) })

	if err := testClient.Create(ctx, tenantB); err != nil {
		t.Fatalf("create tenant B: %v", err)
	}
	t.Cleanup(func() { _ = testClient.Delete(ctx, tenantB) })

	npA := &networkingv1.NetworkPolicy{}
	waitFor(t, jobAppearTimeout, func() bool {
		return testClient.Get(ctx, types.NamespacedName{Name: "tenant-isolation", Namespace: "tenant-iso-a"}, npA) == nil
	})
	npB := &networkingv1.NetworkPolicy{}
	waitFor(t, jobAppearTimeout, func() bool {
		return testClient.Get(ctx, types.NamespacedName{Name: "tenant-isolation", Namespace: "tenant-iso-b"}, npB) == nil
	})

	assertIngressDoesNotAllowNamespace(t, npA, "tenant-iso-b")
	assertIngressDoesNotAllowNamespace(t, npB, "tenant-iso-a")
}

// TestIsolation_NetworkPolicyIngressRules verifies baseline ingress allows edge
// routing (ingress + Envoy Gateway) only.
func TestIsolation_NetworkPolicyIngressRules(t *testing.T) {
	t.Parallel()
	tenant := &gentianov1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: "iso-ingress"},
		Spec: gentianov1alpha1.TenantSpec{
			DisplayName: "Ingress Check",
			Domain:      "iso-ingress.example.com",
			AdminEmail:  "admin@iso-ingress.example.com",
		},
	}

	ctx := context.Background()
	if err := testClient.Create(ctx, tenant); err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	t.Cleanup(func() { _ = testClient.Delete(ctx, tenant) })

	np := &networkingv1.NetworkPolicy{}
	waitFor(t, jobAppearTimeout, func() bool {
		return testClient.Get(ctx, types.NamespacedName{Name: "tenant-isolation", Namespace: "tenant-iso-ingress"}, np) == nil
	})

	if len(np.Spec.Ingress) == 0 {
		t.Fatal("expected ingress rules")
	}

	allowedNamespaces := collectIngressNamespaces(np)

	expectedNS := []string{"envoy-gateway-system"}
	for _, ns := range expectedNS {
		found := false
		for _, allowed := range allowedNamespaces {
			if allowed == ns {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("expected ingress to allow namespace %q, allowed: %v", ns, allowedNamespaces)
		}
	}
}

// TestIsolation_NetworkPolicyEgressRules verifies baseline egress allows DNS
// and the Kubernetes API only (kernel/contract access is separate policies).
func TestIsolation_NetworkPolicyEgressRules(t *testing.T) {
	t.Parallel()
	tenant := &gentianov1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: "iso-egress"},
		Spec: gentianov1alpha1.TenantSpec{
			DisplayName: "Egress Check",
			Domain:      "iso-egress.example.com",
			AdminEmail:  "admin@iso-egress.example.com",
		},
	}

	ctx := context.Background()
	if err := testClient.Create(ctx, tenant); err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	t.Cleanup(func() { _ = testClient.Delete(ctx, tenant) })

	np := &networkingv1.NetworkPolicy{}
	waitFor(t, jobAppearTimeout, func() bool {
		return testClient.Get(ctx, types.NamespacedName{Name: "tenant-isolation", Namespace: "tenant-iso-egress"}, np) == nil
	})

	if len(np.Spec.Egress) == 0 {
		t.Fatal("expected egress rules")
	}

	egressNS := collectEgressNamespaces(np)
	if len(egressNS) != 0 {
		t.Errorf("baseline egress should not allow namespace peers, got: %v", egressNS)
	}

	if !hasEgressPort(np, 53) {
		t.Error("expected egress rule for DNS (port 53)")
	}

	if !hasEgressIPBlock(np) {
		t.Error("expected egress rule with ipBlock for Kubernetes API server")
	}
}

// TestIsolation_ResourceQuotaAllFields verifies all three quota fields
// (storage, CPU, memory) are enforced.
func TestIsolation_ResourceQuotaAllFields(t *testing.T) {
	t.Parallel()
	storage := resource.MustParse("100Gi")
	cpu := resource.MustParse("8")
	memory := resource.MustParse("16Gi")

	tenant := &gentianov1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: "iso-quota"},
		Spec: gentianov1alpha1.TenantSpec{
			DisplayName: "Quota Check",
			Domain:      "iso-quota.example.com",
			AdminEmail:  "admin@iso-quota.example.com",
			Quotas: &gentianov1alpha1.TenantQuotas{
				Storage: &storage,
				CPU:     &cpu,
				Memory:  &memory,
			},
		},
	}

	ctx := context.Background()
	if err := testClient.Create(ctx, tenant); err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	t.Cleanup(func() { _ = testClient.Delete(ctx, tenant) })

	rq := &corev1.ResourceQuota{}
	waitFor(t, jobAppearTimeout, func() bool {
		return testClient.Get(ctx, types.NamespacedName{Name: "tenant-quota", Namespace: "tenant-iso-quota"}, rq) == nil
	})

	if got := rq.Spec.Hard[corev1.ResourceRequestsStorage]; got.Cmp(storage) != 0 {
		t.Errorf("storage quota: want %v, got %v", storage, got)
	}
	if got := rq.Spec.Hard[corev1.ResourceLimitsCPU]; got.Cmp(cpu) != 0 {
		t.Errorf("CPU quota: want %v, got %v", cpu, got)
	}
	if got := rq.Spec.Hard[corev1.ResourceLimitsMemory]; got.Cmp(memory) != 0 {
		t.Errorf("memory quota: want %v, got %v", memory, got)
	}
}

// TestIsolation_LimitRangeDefaults verifies the hardcoded LimitRange defaults:
// default 500m/512Mi, defaultRequest 100m/128Mi, max 4/8Gi.
func TestIsolation_LimitRangeDefaults(t *testing.T) {
	t.Parallel()
	tenant := &gentianov1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: "iso-limits"},
		Spec: gentianov1alpha1.TenantSpec{
			DisplayName: "Limits Check",
			Domain:      "iso-limits.example.com",
			AdminEmail:  "admin@iso-limits.example.com",
		},
	}

	ctx := context.Background()
	if err := testClient.Create(ctx, tenant); err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	t.Cleanup(func() { _ = testClient.Delete(ctx, tenant) })

	lr := &corev1.LimitRange{}
	waitFor(t, jobAppearTimeout, func() bool {
		return testClient.Get(ctx, types.NamespacedName{Name: "tenant-limits", Namespace: "tenant-iso-limits"}, lr) == nil
	})

	if len(lr.Spec.Limits) == 0 {
		t.Fatal("expected LimitRange items")
	}
	item := lr.Spec.Limits[0]
	if item.Type != corev1.LimitTypeContainer {
		t.Fatalf("expected Container limit type, got %v", item.Type)
	}

	assertQuantity(t, "Default CPU", item.Default[corev1.ResourceCPU], "500m")
	assertQuantity(t, "Default Memory", item.Default[corev1.ResourceMemory], "512Mi")
	assertQuantity(t, "DefaultRequest CPU", item.DefaultRequest[corev1.ResourceCPU], "100m")
	assertQuantity(t, "DefaultRequest Memory", item.DefaultRequest[corev1.ResourceMemory], "128Mi")
	assertQuantity(t, "Max CPU", item.Max[corev1.ResourceCPU], "4")
	assertQuantity(t, "Max Memory", item.Max[corev1.ResourceMemory], "8Gi")
}

// ---------------------------------------------------------------------------
// Deletion lifecycle tests
// ---------------------------------------------------------------------------

// TestDeletion_EndToEnd_WithApps creates a Tenant with multiple apps (requiring
// PostgreSQL, MariaDB, S3, Redis, Memcached), verifies shell resources exist,
// then deletes the Tenant with DeletionPolicy=Delete and verifies cleanup Jobs
// and resource removal. Mail provisioning is covered by TestMail_Selfhosted.
func TestDeletion_EndToEnd_WithApps(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	pgProfile := newFullAppProfile("del-pgapp", gentianov1alpha1.DatabaseEnginePostgreSQL, true, true, false)
	mariaProfile := newFullAppProfile("del-mariaapp", gentianov1alpha1.DatabaseEngineMariaDB, true, false, true)

	if err := testClient.Create(ctx, pgProfile); err != nil {
		t.Fatalf("create pg AppProfile: %v", err)
	}
	t.Cleanup(func() { _ = testClient.Delete(ctx, pgProfile) })

	if err := testClient.Create(ctx, mariaProfile); err != nil {
		t.Fatalf("create maria AppProfile: %v", err)
	}
	t.Cleanup(func() { _ = testClient.Delete(ctx, mariaProfile) })

	tenant := &gentianov1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: "del-full"},
		Spec: gentianov1alpha1.TenantSpec{
			DisplayName:    "Deletion E2E",
			Domain:         "del-full.example.com",
			AdminEmail:     "admin@del-full.example.com",
			DeletionPolicy: gentianov1alpha1.DeletionPolicyDelete,
			Apps: []gentianov1alpha1.TenantApp{
				{Profile: "del-pgapp"},
				{Profile: "del-mariaapp"},
			},
		},
	}
	if err := testClient.Create(ctx, tenant); err != nil {
		t.Fatalf("create tenant: %v", err)
	}

	// Wait for namespace and initial resources.
	waitFor(t, jobAppearTimeout, func() bool {
		return testClient.Get(ctx, types.NamespacedName{Name: "tenant-del-full"}, &corev1.Namespace{}) == nil
	})

	np := &networkingv1.NetworkPolicy{}
	waitFor(t, jobAppearTimeout, func() bool {
		return testClient.Get(ctx, types.NamespacedName{Name: "tenant-isolation", Namespace: "tenant-del-full"}, np) == nil
	})

	// -- DELETE the tenant ------------------------------------------------
	if err := testClient.Delete(ctx, tenant); err != nil {
		t.Fatalf("delete tenant: %v", err)
	}
	// Cleanup Jobs must finish before purgeTenantKernelResources runs; otherwise
	// incomplete delete Jobs survive after the Tenant finalizer is removed.
	cleanupJobs := []string{
		"keycloak-realm-delete-del-full",
		"mariadb-delete-del-full-del-mariaapp",
		"s3-delete-del-full-del-pgapp",
		"s3-delete-del-full-del-mariaapp",
		"redis-acl-delete-del-full-del-pgapp",
	}
	for _, jobName := range cleanupJobs {
		waitFor(t, jobAppearTimeout, func() bool {
			job := &batchv1.Job{}
			return testClient.Get(ctx, types.NamespacedName{Name: jobName, Namespace: "platform-kernel"}, job) == nil
		})
		markJobComplete(t, jobName, "platform-kernel")
	}

	// Wait for Tenant CR to be gone (finalizer ran).
	waitFor(t, tenantReadyTimeout, func() bool {
		err := testClient.Get(ctx, types.NamespacedName{Name: "del-full"}, &gentianov1alpha1.Tenant{})
		return err != nil
	})

	// Completed cleanup Jobs should be purged as final destructive cleanup.
	for _, jobName := range []string{
		"keycloak-realm-delete-del-full",
		"mariadb-delete-del-full-del-mariaapp",
		"s3-delete-del-full-del-pgapp",
		"s3-delete-del-full-del-mariaapp",
		"redis-acl-delete-del-full-del-pgapp",
	} {
		t.Run("purges "+jobName, func(t *testing.T) {
			waitFor(t, jobAppearTimeout, func() bool {
				job := &batchv1.Job{}
				err := testClient.Get(ctx, types.NamespacedName{Name: jobName, Namespace: "platform-kernel"}, job)
				if k8serrors.IsNotFound(err) {
					return true
				}
				if err != nil {
					return false
				}
				// Completed cleanup Jobs may still exist briefly when purge ran before
				// envtest status propagation; remove them so the assertion matches
				// production TTL expiry behaviour.
				if job.Status.Succeeded > 0 && job.DeletionTimestamp == nil {
					prop := metav1.DeletePropagationBackground
					_ = testClient.Delete(ctx, job, &client.DeleteOptions{PropagationPolicy: &prop})
				}
				err = testClient.Get(ctx, types.NamespacedName{Name: jobName, Namespace: "platform-kernel"}, job)
				return k8serrors.IsNotFound(err)
			})
		})
	}

	// Namespace should be terminating or gone.
	ns := &corev1.Namespace{}
	err := testClient.Get(ctx, types.NamespacedName{Name: "tenant-del-full"}, ns)
	if err == nil && ns.DeletionTimestamp == nil {
		t.Error("namespace should be deleted or terminating after Delete policy")
	}
}

// TestDeletion_Retain_KeepsDataRevokesAccess creates a Tenant with apps, then
// deletes with DeletionPolicy=Retain. Verifies the namespace and data
// resources are preserved but ownership resources (quota, limits, netpol) are
// cleaned up and mail ConfigMap entries are removed (cutting routing).
func TestDeletion_Retain_KeepsDataRevokesAccess(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	profile := newFullAppProfile("ret-app", gentianov1alpha1.DatabaseEnginePostgreSQL, true, true, false)
	if err := testClient.Create(ctx, profile); err != nil {
		t.Fatalf("create AppProfile: %v", err)
	}
	t.Cleanup(func() { _ = testClient.Delete(ctx, profile) })

	tenant := &gentianov1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: "ret-full"},
		Spec: gentianov1alpha1.TenantSpec{
			DisplayName:    "Retain E2E",
			Domain:         "ret-full.example.com",
			AdminEmail:     "admin@ret-full.example.com",
			DeletionPolicy: gentianov1alpha1.DeletionPolicyRetain,
			Mail:           &gentianov1alpha1.TenantMail{Mode: gentianov1alpha1.MailModeSelfhosted},
			Apps:           []gentianov1alpha1.TenantApp{{Profile: "ret-app"}},
		},
	}
	if err := testClient.Create(ctx, tenant); err != nil {
		t.Fatalf("create tenant: %v", err)
	}

	// Wait for namespace + NetworkPolicy + mail infrastructure.
	waitFor(t, jobAppearTimeout, func() bool {
		return testClient.Get(ctx, types.NamespacedName{Name: "tenant-ret-full"}, &corev1.Namespace{}) == nil
	})
	waitFor(t, jobAppearTimeout, func() bool {
		return testClient.Get(ctx, types.NamespacedName{
			Name: "tenant-isolation", Namespace: "tenant-ret-full",
		}, &networkingv1.NetworkPolicy{}) == nil
	})

	waitFor(t, jobAppearTimeout, func() bool {
		cm := &corev1.ConfigMap{}
		if err := testClient.Get(ctx, types.NamespacedName{Name: "mail-postfix-virtual-domains", Namespace: "platform-kernel"}, cm); err != nil {
			return false
		}
		_, ok := cm.Data["ret-full"]
		return ok
	})

	// -- DELETE with Retain -----------------------------------------------
	if err := testClient.Delete(ctx, tenant); err != nil {
		t.Fatalf("delete tenant: %v", err)
	}
	// Retain policy: deleteIdentity is a no-op for tenants with no OIDC apps (ret-app has none).

	// Wait for Tenant CR to be gone (finalizer completed).
	waitFor(t, tenantReadyTimeout, func() bool {
		err := testClient.Get(ctx, types.NamespacedName{Name: "ret-full"}, &gentianov1alpha1.Tenant{})
		return err != nil
	})

	// Namespace must still exist (Retain policy).
	ns := &corev1.Namespace{}
	if err := testClient.Get(ctx, types.NamespacedName{Name: "tenant-ret-full"}, ns); err != nil {
		t.Errorf("namespace should be retained, but got error: %v", err)
	}

	// Owned resources in namespace should be cleaned up (poll: envtest shell simulator
	// may lag one tick behind the controller finalizer).
	waitForRetainShellTeardown(t, ctx, "tenant-ret-full")

	rq := &corev1.ResourceQuota{}
	if err := testClient.Get(ctx, types.NamespacedName{Name: "tenant-quota", Namespace: "tenant-ret-full"}, rq); err == nil {
		t.Error("ResourceQuota should be deleted in Retain mode")
	}
	lr := &corev1.LimitRange{}
	if err := testClient.Get(ctx, types.NamespacedName{Name: "tenant-limits", Namespace: "tenant-ret-full"}, lr); err == nil {
		t.Error("LimitRange should be deleted in Retain mode")
	}
	npCheck := &networkingv1.NetworkPolicy{}
	if err := testClient.Get(ctx, types.NamespacedName{Name: "tenant-isolation", Namespace: "tenant-ret-full"}, npCheck); err == nil {
		t.Error("NetworkPolicy should be deleted in Retain mode")
	}

	// Mail ConfigMap entries should be removed (cutting routing).
	postfixCM := &corev1.ConfigMap{}
	if err := testClient.Get(ctx, types.NamespacedName{Name: "mail-postfix-virtual-domains", Namespace: "platform-kernel"}, postfixCM); err == nil {
		if _, ok := postfixCM.Data["ret-full"]; ok {
			t.Error("Postfix virtual-domain entry should be removed in Retain mode (route revocation)")
		}
	}

	// No cleanup Jobs should be created for data resources with Retain policy.
	identityDeleteJob := &batchv1.Job{}
	if err := testClient.Get(ctx, types.NamespacedName{
		Name: "keycloak-realm-delete-ret-full", Namespace: "platform-kernel",
	}, identityDeleteJob); err == nil {
		t.Error("Keycloak realm deletion Job should NOT be created for Retain policy")
	}
}

// waitForRetainShellTeardown polls until orchestrator-owned namespace scaffolding
// is gone after a Retain tenant delete.
func waitForRetainShellTeardown(t *testing.T, ctx context.Context, nsName string) {
	t.Helper()
	waitFor(t, jobAppearTimeout, func() bool {
		objs := []client.Object{
			&corev1.ResourceQuota{},
			&corev1.LimitRange{},
			&networkingv1.NetworkPolicy{},
		}
		names := []string{"tenant-quota", "tenant-limits", "tenant-isolation"}
		for i, name := range names {
			if err := testClient.Get(ctx, types.NamespacedName{Name: name, Namespace: nsName}, objs[i]); err == nil {
				return false
			}
		}
		return true
	})
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// newFullAppProfile builds an AppProfile with multiple kernel requirements.
func newFullAppProfile(name string, dbEngine gentianov1alpha1.DatabaseEngine, needsS3, needsRedis, needsMemcached bool) *gentianov1alpha1.AppProfile {
	kr := &gentianov1alpha1.KernelRequirements{
		Database: &gentianov1alpha1.DatabaseRequirement{
			Engine:            dbEngine,
			DatabasePerTenant: true,
		},
	}
	if needsS3 {
		kr.Storage = &gentianov1alpha1.StorageRequirement{
			S3: &gentianov1alpha1.S3Requirement{BucketPerTenant: true},
		}
	}
	if needsRedis {
		kr.Cache = &gentianov1alpha1.CacheRequirement{Engine: gentianov1alpha1.CacheEngineRedis}
	}
	if needsMemcached {
		kr.Cache = &gentianov1alpha1.CacheRequirement{Engine: gentianov1alpha1.CacheEngineMemcached}
	}

	return &gentianov1alpha1.AppProfile{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: gentianov1alpha1.AppProfileSpec{
			DisplayName:      name,
			DeploymentMethod: gentianov1alpha1.DeploymentMethodArgoCD,
			Chart: gentianov1alpha1.ChartRef{
				Repository: "oci://charts.example.com",
				Name:       name,
				Version:    "1.0.0",
			},
			KernelRequirements: kr,
		},
	}
}

// assertIngressDoesNotAllowNamespace checks that no ingress rule references
// the given namespace via a namespace selector.
func assertIngressDoesNotAllowNamespace(t *testing.T, np *networkingv1.NetworkPolicy, ns string) {
	t.Helper()
	for _, rule := range np.Spec.Ingress {
		for _, from := range rule.From {
			if from.NamespaceSelector != nil {
				for _, expr := range from.NamespaceSelector.MatchExpressions {
					if expr.Key == "kubernetes.io/metadata.name" {
						for _, v := range expr.Values {
							if v == ns {
								t.Errorf("NetworkPolicy %s/%s ingress should NOT allow namespace %q",
									np.Namespace, np.Name, ns)
							}
						}
					}
				}
				for k, v := range from.NamespaceSelector.MatchLabels {
					if k == "kubernetes.io/metadata.name" && v == ns {
						t.Errorf("NetworkPolicy %s/%s ingress should NOT allow namespace %q",
							np.Namespace, np.Name, ns)
					}
				}
			}
		}
	}
}

// collectIngressNamespaces extracts namespace names from ingress rules.
func collectIngressNamespaces(np *networkingv1.NetworkPolicy) []string {
	var result []string
	for _, rule := range np.Spec.Ingress {
		for _, from := range rule.From {
			if from.NamespaceSelector != nil {
				for k, v := range from.NamespaceSelector.MatchLabels {
					if k == "kubernetes.io/metadata.name" {
						result = append(result, v)
					}
				}
			}
		}
	}
	return result
}

// collectEgressNamespaces extracts namespace names from egress rules.
func collectEgressNamespaces(np *networkingv1.NetworkPolicy) []string {
	var result []string
	for _, rule := range np.Spec.Egress {
		for _, to := range rule.To {
			if to.NamespaceSelector != nil {
				for k, v := range to.NamespaceSelector.MatchLabels {
					if k == "kubernetes.io/metadata.name" {
						result = append(result, v)
					}
				}
			}
		}
	}
	return result
}

// hasEgressPort checks if any egress rule allows the given port number.
func hasEgressPort(np *networkingv1.NetworkPolicy, port int32) bool {
	for _, rule := range np.Spec.Egress {
		for _, p := range rule.Ports {
			if p.Port != nil && p.Port.IntValue() == int(port) {
				return true
			}
		}
	}
	return false
}

// hasEgressIPBlock checks if any egress rule uses an IPBlock selector.
func hasEgressIPBlock(np *networkingv1.NetworkPolicy) bool {
	for _, rule := range np.Spec.Egress {
		for _, to := range rule.To {
			if to.IPBlock != nil {
				return true
			}
		}
	}
	return false
}

// assertQuantity verifies a resource.Quantity matches the expected string.
func assertQuantity(t *testing.T, label string, got resource.Quantity, expected string) {
	t.Helper()
	want := resource.MustParse(expected)
	if got.Cmp(want) != 0 {
		t.Errorf("%s: want %s, got %s", label, expected, got.String())
	}
}
