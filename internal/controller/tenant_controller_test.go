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
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
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
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	"sigs.k8s.io/yaml"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/controller"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// testClient is the shared client used by all tests in this package.
var testClient client.Client

// envtestWaitTimeout is the poll deadline for controller envtest waits.
//
// 140 t.Parallel() tests share one manager and one envtest apiserver, so a test
// that loses the scheduling race against the others exceeds a short deadline
// and fails with "timed out waiting for condition" — and which test loses is
// arbitrary. That makes a red build in this package ambiguous: a regression and
// a starved runner are indistinguishable without re-running, which is the worst
// property a suite can have.
//
// Three minutes is a bound on being starved, not on being wrong. These waits
// poll reconcile loops rather than burn CPU, so the extra ceiling costs nothing
// on a healthy run — it is only reached when the answer was never coming, and a
// genuinely hung reconciler still reports, just later.
//
// Three minutes is a bound tuned against CI's dedicated runner, not against
// every machine this suite runs on. Confirmed directly on a loaded local dev
// box (a live cluster plus its own services already resident, not merely other
// go test goroutines): captured controller logs showed every Tenant across the
// whole package — not only the one a failing test was waiting on — go
// completely silent for ~175 seconds, twice, independently, including after
// tenantRateLimiter (tenant_controller.go) closed off a separate, real
// amplifier — a burst of ordinary optimistic-concurrency conflicts able to
// throttle the shared workqueue for minutes under the default rate limiter.
// With that fixed, the same magnitude of silence persisted, which is why it
// reads as the host being starved of CPU or I/O for that whole span, not as
// anything this package's own logic can retry its way out of.
//
// GENTIAN_TEST_WAIT_TIMEOUT raises the ceiling for a run without changing what
// it means: a regression still fails, just after a longer, still-bounded wait,
// so "give it more time" cannot turn a real hang into a false pass — only a
// starved one into a clean one. CI never sets it, so this stays 3 minutes
// there, which is the value that catches a genuinely hung reconciler fastest.
var envtestWaitTimeout = func() time.Duration {
	if v := os.Getenv("GENTIAN_TEST_WAIT_TIMEOUT"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return 3 * time.Minute
}()

// tenantReadyTimeout is an alias for Phase=Ready waits (same ceiling as job waits).
var tenantReadyTimeout = envtestWaitTimeout

// jobAppearTimeout is an alias for waits on Job creation or intermediate conditions.
var jobAppearTimeout = envtestWaitTimeout

// dataPlaneManualTestTenants lists tenants whose data-plane Jobs are completed
// manually in reconciler tests (redis/pg/mariadb/s3 assertions).
var dataPlaneManualTestTenants = map[string]struct{}{
	"cacheready":   {},
	"storageready": {},
	"mariaready":   {},
	"dbcreate":     {},
	"rolejob":      {},
	"dbready":      {},
	"dbdelete":     {},
}

// deleteCleanupManualTestTenants lists tenants whose delete-cleanup Jobs must not be
// auto-completed so tests can observe Job creation.
var deleteCleanupManualTestTenants = map[string]struct{}{
	"storagedelete": {},
}

// deleteRetainManualTestTenants lists tenants that assert Retain cleanup Job creation.
var deleteRetainManualTestTenants = map[string]struct{}{
	"identretain": {},
}

func provisioningJobTenant(jobName string) (tenant string, ok bool) {
	for _, prefix := range []string{
		"redis-acl-",
		"pg-role-",
		"mariadb-setup-",
		"s3-bucket-",
	} {
		if strings.HasPrefix(jobName, prefix) {
			rest := strings.TrimPrefix(jobName, prefix)
			if idx := strings.Index(rest, "-"); idx > 0 {
				return rest[:idx], true
			}
			return rest, true
		}
	}
	return "", false
}

func shouldAutoCompleteProvisioningJob(jobName string) bool {
	tenant, ok := provisioningJobTenant(jobName)
	if !ok {
		return false
	}
	if _, manual := dataPlaneManualTestTenants[tenant]; manual {
		switch {
		case strings.HasPrefix(jobName, "redis-acl-"):
			return false
		case strings.HasPrefix(jobName, "pg-role-"):
			// Portal shell DB is required for every tenant; manual data-plane tests
			// control app-specific role jobs only.
			if strings.HasSuffix(jobName, "-shell") {
				return true
			}
			return false
		case strings.HasPrefix(jobName, "mariadb-setup-"):
			return false
		case strings.HasPrefix(jobName, "s3-bucket-"):
			return false
		}
	}
	return true
}

func deleteCleanupJobTenant(jobName string) (tenant string, ok bool) {
	for _, prefix := range []string{
		"keycloak-realm-delete-",
		"keycloak-realm-disable-",
		"mariadb-delete-",
		"s3-delete-",
		"redis-acl-delete-",
	} {
		if strings.HasPrefix(jobName, prefix) {
			rest := strings.TrimPrefix(jobName, prefix)
			if idx := strings.Index(rest, "-"); idx > 0 {
				return rest[:idx], true
			}
			return rest, true
		}
	}
	return "", false
}

func shouldAutoCompleteDeleteCleanupJob(jobName string) bool {
	tenant, ok := deleteCleanupJobTenant(jobName)
	if !ok {
		return false
	}
	if _, manual := deleteCleanupManualTestTenants[tenant]; manual {
		return false
	}
	_, manual := deleteRetainManualTestTenants[tenant]
	return !manual
}

// waitForTenantConditionTrue polls until the tenant has condType=True.
func waitForTenantConditionTrue(t *testing.T, tenantName, condType string) *gentianov1alpha1.Tenant {
	t.Helper()
	updated := &gentianov1alpha1.Tenant{}
	waitFor(t, tenantReadyTimeout, func() bool {
		if err := testClient.Get(context.Background(), types.NamespacedName{Name: tenantName}, updated); err != nil {
			return false
		}
		for _, c := range updated.Status.Conditions {
			if c.Type == condType && c.Status == metav1.ConditionTrue {
				return true
			}
		}
		return false
	})
	return updated
}

// waitForTenantConditionReason polls until the tenant has a status condition
// with the given type and reason (reconciler gates on conditions, not Job creation order).
func waitForTenantConditionReason(t *testing.T, tenantName, condType, reason string) {
	t.Helper()
	waitFor(t, jobAppearTimeout, func() bool {
		tenant := &gentianov1alpha1.Tenant{}
		if err := testClient.Get(context.Background(), types.NamespacedName{Name: tenantName}, tenant); err != nil {
			return false
		}
		for _, c := range tenant.Status.Conditions {
			if c.Type == condType && c.Reason == reason {
				return true
			}
		}
		return false
	})
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

// startFakeKeycloak stands in for the Keycloak Admin REST API. envtest has no
// real Keycloak, but the tenant reconcile now calls the Admin REST API
// in-process (browser security headers in the SharedKernel stage, app-admin
// group membership in the AppsAndEdge stage). Without a reachable endpoint the
// SharedKernel stage requeues forever and tenants never reach Ready. The
// handler issues admin tokens, accepts realm updates (204), and returns empty
// arrays for group/member/user list queries.
func startFakeKeycloak() *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/realms/master/protocol/openid-connect/token", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"access_token":"test-admin-token","expires_in":300}`)
	})
	mux.HandleFunc("/admin/realms/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `[]`)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	return httptest.NewServer(mux)
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
	if err := gatewayv1.Install(scheme.Scheme); err != nil {
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
		APIReader:                mgr.GetAPIReader(),
		Scheme:                   mgr.GetScheme(),
		KernelDomain:             "platform.example.test",
		TenantDNS01ClusterIssuer: "letsencrypt-dns01-cloudflare",
		KernelRealm:              "kernel",
		RoutingMode:              controller.RoutingModeGateway,
		// The mail tests assert the shared Postfix AND Dovecot artefacts, which
		// is what kernel mode provisions. With this unset the suite would run as
		// external, where Dovecot is deliberately not configured at all.
		MailServiceMode: "kernel",
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
	startXTenantShellSimulator(ctx, testClient)
	startTenantProvisioningJobSimulator(ctx, testClient)
	go func() { _ = mgr.Start(ctx) }()

	// Stand in for Crossplane and provider-keycloak, which do not run here.
	//
	// The mail path waits for the tenant's Dovecot OIDC client to be Ready before
	// it writes Dovecot's realm auth config, because that config carries the
	// client secret and introspection URL and is useless pointed at a client that
	// does not exist. On a real cluster the Composition creates the client and the
	// provider marks it Ready; under envtest nothing does either, so the wait
	// never ends and every tenant stops at mail.
	//
	// It has to do both halves, because neither runs here: create the client the
	// Composition would create for each XTenant, and mark it Ready the way the
	// provider would. Marking alone was not enough — with no Crossplane there is
	// nothing to mark, so the wait never ended.
	go fakeKeycloakClientProvider(ctx, mgr.GetClient())

	// platform-kernel namespace is required by the identity reconciler for Keycloak Jobs.
	if err := testClient.Create(context.Background(), &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: "platform-kernel"},
	}); err != nil {
		panic(err)
	}

	// gentian-dev holds shared service ConfigMaps (e.g. Dovecot OIDC introspection values).
	for _, ns := range []string{"gentian-dev", "gentian-infra-dev", "envoy-gateway-system"} {
		if err := testClient.Create(context.Background(), &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: ns},
		}); err != nil {
			panic(err)
		}
	}

	// dovecot-admin Secret provides OIDC + doveadm credentials for shared Dovecot.
	if err := testClient.Create(context.Background(), &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "dovecot-admin", Namespace: "platform-kernel"},
		Data: map[string][]byte{
			"doveadm_password":   []byte("test-doveadm-password"),
			"oidc_client_secret": []byte("test-oidc-secret"),
		},
	}); err != nil {
		panic(err)
	}

	// keycloak-admin Secret provides the Keycloak URL for OIDC token introspection
	// and Admin REST calls. Point it at the in-process fake so reconcile stages
	// that talk to Keycloak (browser security headers, app-admin membership)
	// succeed under envtest.
	fakeKC := startFakeKeycloak()
	if err := testClient.Create(context.Background(), &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "keycloak-admin", Namespace: "platform-kernel"},
		Data: map[string][]byte{
			"url":      []byte(fakeKC.URL),
			"username": []byte("kcadmin"),
			"password": []byte("test-kc-password"),
		},
	}); err != nil {
		panic(err)
	}

	// The kernel OIDCPackCatalog, as the operator chart ships it. Selfhosted mail
	// resolves the gentian-dovecot service pack from it to provision the client
	// Dovecot introspects IMAP XOAUTH2 tokens with, so without it every tenant
	// using mail waits on a catalogue that never arrives.
	if err := testClient.Create(context.Background(), &gentianov1alpha1.OIDCPackCatalog{
		ObjectMeta: metav1.ObjectMeta{Name: "gentian-kernel"},
		Spec: gentianov1alpha1.OIDCPackCatalogSpec{
			Packs: map[string]gentianov1alpha1.OIDCPackSpec{
				"gentian-dovecot": {ServiceClient: true},
			},
		},
	}); err != nil {
		panic(err)
	}

	// minio-admin Secret is required by S3 provisioning Jobs in the kernel namespace.
	if err := testClient.Create(context.Background(), &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "minio-admin", Namespace: "platform-kernel"},
		Data: map[string][]byte{
			"endpoint":  []byte("http://minio.platform-kernel.svc:9000"),
			"accessKey": []byte("minioadmin"),
			"secretKey": []byte("minioadmin"),
		},
	}); err != nil {
		panic(err)
	}

	catalogRaw, err := os.ReadFile(filepath.Join("..", "oidc", "testdata", "minimal-oidc-catalog.yaml"))
	if err != nil {
		panic(err)
	}
	var oidcCatalog gentianov1alpha1.OIDCPackCatalog
	if err := yaml.Unmarshal(catalogRaw, &oidcCatalog); err != nil {
		panic(err)
	}
	if err := testClient.Create(context.Background(), &oidcCatalog); err != nil {
		panic(err)
	}

	// Auto-complete Keycloak Jobs and data-plane provisioning Jobs for tests that
	// do not assert job ordering manually.
	go func() {
		for {
			time.Sleep(50 * time.Millisecond)
			var jobs batchv1.JobList
			if err := testClient.List(context.Background(), &jobs, client.InNamespace("platform-kernel")); err == nil {
				for _, job := range jobs.Items {
					j := job // copy loop variable
					if j.Status.Succeeded > 0 {
						continue
					}
					name := j.Name
					autoKeycloak := strings.HasPrefix(name, "keycloak-")
					autoProv := shouldAutoCompleteProvisioningJob(name)
					autoDeleteCleanup := shouldAutoCompleteDeleteCleanupJob(name)
					if !autoKeycloak && !autoProv && !autoDeleteCleanup {
						continue
					}
					if autoDeleteCleanup && strings.Contains(name, "realm-disable") {
						// identretain asserts disable Job creation before completion.
						if strings.Contains(name, "identretain") {
							continue
						}
					}
					if autoKeycloak && (strings.Contains(name, "clienttest") || strings.Contains(name, "admintest") || strings.Contains(name, "identretain") || strings.Contains(name, "del-tenant")) {
						// These tests control Keycloak job timing or test deletion flows.
						continue
					}
					markJobSucceeded(&j)
					for attempt := 0; attempt < 3; attempt++ {
						if err := testClient.Status().Update(context.Background(), &j); err == nil {
							break
						} else if k8serrors.IsConflict(err) {
							time.Sleep(20 * time.Millisecond)
							if err := testClient.Get(context.Background(), types.NamespacedName{Name: name, Namespace: "platform-kernel"}, &j); err != nil {
								break
							}
							if j.Status.Succeeded > 0 {
								break
							}
							markJobSucceeded(&j)
						}
					}
				}
			}
		}
	}()

	// Memcached Deployments are created from tenant data-plane manifests; envtest has
	// no kubelet to mark them ready, so integration tests would stall in CacheReady.
	go func() {
		for {
			time.Sleep(50 * time.Millisecond)
			var deps appsv1.DeploymentList
			if err := testClient.List(context.Background(), &deps); err != nil {
				continue
			}
			for _, dep := range deps.Items {
				if dep.Name != "memcached" || dep.Status.ReadyReplicas > 0 {
					continue
				}
				if !strings.HasPrefix(dep.Namespace, "tenant-") {
					continue
				}
				patchDeploymentReady(context.Background(), testClient, dep.Namespace, dep.Name)
			}
		}
	}()

	code := m.Run()

	cancel()
	fakeKC.Close()
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
		},
	}
	if err := testClient.Create(context.Background(), tenant); err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	t.Cleanup(func() { _ = testClient.Delete(context.Background(), tenant) })

	ns := &corev1.Namespace{}
	waitFor(t, jobAppearTimeout, func() bool {
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
		},
	}
	if err := testClient.Create(context.Background(), tenant); err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	t.Cleanup(func() { _ = testClient.Delete(context.Background(), tenant) })

	updated := &gentianov1alpha1.Tenant{}
	waitFor(t, tenantReadyTimeout, func() bool {
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
	waitFor(t, jobAppearTimeout, func() bool {
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
		},
	}
	if err := testClient.Create(context.Background(), tenant); err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	t.Cleanup(func() { _ = testClient.Delete(context.Background(), tenant) })

	lr := &corev1.LimitRange{}
	waitFor(t, jobAppearTimeout, func() bool {
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
		},
	}
	if err := testClient.Create(context.Background(), tenant); err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	t.Cleanup(func() { _ = testClient.Delete(context.Background(), tenant) })

	np := &networkingv1.NetworkPolicy{}
	waitFor(t, jobAppearTimeout, func() bool {
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
	waitFor(t, jobAppearTimeout, func() bool {
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
			Isolation:   &gentianov1alpha1.TenantIsolation{Namespace: "zeta-custom"},
		},
	}
	if err := testClient.Create(context.Background(), tenant); err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	t.Cleanup(func() { _ = testClient.Delete(context.Background(), tenant) })

	ns := &corev1.Namespace{}
	waitFor(t, jobAppearTimeout, func() bool {
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
			DeletionPolicy: gentianov1alpha1.DeletionPolicyRetain,
		},
	}
	if err := testClient.Create(context.Background(), tenant); err != nil {
		t.Fatalf("create tenant: %v", err)
	}

	// Wait for namespace to exist
	waitFor(t, jobAppearTimeout, func() bool {
		return testClient.Get(context.Background(), types.NamespacedName{Name: "tenant-retainer"}, &corev1.Namespace{}) == nil
	})

	// Delete the tenant
	if err := testClient.Delete(context.Background(), tenant); err != nil {
		t.Fatalf("delete tenant: %v", err)
	}
	// For Retain policy deleteIdentity is a no-op: retainer has no apps so no realm was provisioned.

	// Wait for Tenant CR to be gone (finalizer removed)
	waitFor(t, tenantReadyTimeout, func() bool {
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
			DeletionPolicy: gentianov1alpha1.DeletionPolicyDelete,
		},
	}
	if err := testClient.Create(context.Background(), tenant); err != nil {
		t.Fatalf("create tenant: %v", err)
	}

	// Wait for namespace to be created
	waitFor(t, jobAppearTimeout, func() bool {
		return testClient.Get(context.Background(), types.NamespacedName{Name: "tenant-destroyer"}, &corev1.Namespace{}) == nil
	})

	// Delete the tenant
	if err := testClient.Delete(context.Background(), tenant); err != nil {
		t.Fatalf("delete tenant: %v", err)
	}
	// For Delete policy deleteIdentity creates cleanup jobs.
	go markJobCompleteWhenReady("keycloak-realm-delete-destroyer", "platform-kernel")

	// Wait for Tenant CR to be gone
	waitFor(t, tenantReadyTimeout, func() bool {
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

// TestTenantReconciler_DataPlaneRedisAndPostgresJobs verifies a tenant with both
// cache and database profiles triggers kernel Jobs for each data-plane engine.
func TestTenantReconciler_DataPlaneRedisAndPostgresJobs(t *testing.T) {
	t.Parallel()
	pgProfile := newPostgresProfile("combo-pg")
	redisProfile := newRedisProfile("combo-redis")
	for _, p := range []*gentianov1alpha1.AppProfile{pgProfile, redisProfile} {
		if err := testClient.Create(context.Background(), p); err != nil {
			t.Fatalf("create AppProfile: %v", err)
		}
		t.Cleanup(func() { _ = testClient.Delete(context.Background(), p) })
	}

	tenant := &gentianov1alpha1.Tenant{
		ObjectMeta: metav1.ObjectMeta{Name: "combodp"},
		Spec: gentianov1alpha1.TenantSpec{
			DisplayName: "Combo DP Co",
			Domain:      "combodp.example.com",
			Apps: []gentianov1alpha1.TenantApp{
				{Profile: "combo-pg"},
				{Profile: "combo-redis"},
			},
		},
	}
	if err := testClient.Create(context.Background(), tenant); err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	t.Cleanup(func() { _ = testClient.Delete(context.Background(), tenant) })

	pgJob := &batchv1.Job{}
	waitFor(t, jobAppearTimeout, func() bool {
		return testClient.Get(context.Background(),
			types.NamespacedName{Name: "pg-role-combodp-combo-pg", Namespace: "platform-kernel"}, pgJob) == nil
	})
	redisJob := &batchv1.Job{}
	waitFor(t, jobAppearTimeout, func() bool {
		return testClient.Get(context.Background(),
			types.NamespacedName{Name: "redis-acl-combodp-combo-redis", Namespace: "platform-kernel"}, redisJob) == nil
	})
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

// fakeKeycloakClientProvider stands in for Crossplane and provider-keycloak.
//
// For every XTenant it ensures the Dovecot OIDC client the tenant Composition
// declares exists, and marks it Ready as the provider would. Polling rather than
// watching: the manager cache is already running by then, and a poll is easier to
// reason about in a test binary than another informer racing setup.
//
// The name matches the Composition's, because that is what the operator looks
// for. If the Composition renames it, this must move with it — a fake that
// answers to the wrong name would make the wait pass here and hang on a cluster.
func fakeKeycloakClientProvider(ctx context.Context, c client.Client) {
	xtenantGVK := schema.GroupVersionKind{Group: "gentianos.io", Version: "v1alpha1", Kind: "XTenantList"}
	clientGVK := schema.GroupVersionKind{Group: "openidclient.keycloak.crossplane.io", Version: "v1alpha1", Kind: "Client"}

	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			xts := &unstructured.UnstructuredList{}
			xts.SetGroupVersionKind(xtenantGVK)
			if err := c.List(ctx, xts); err != nil {
				continue
			}
			for i := range xts.Items {
				name := xts.Items[i].GetName() + "-dovecot-oidc-client"
				obj := &unstructured.Unstructured{}
				obj.SetGroupVersionKind(clientGVK)
				err := c.Get(ctx, types.NamespacedName{Name: name}, obj)
				if err != nil {
					created := &unstructured.Unstructured{}
					created.SetGroupVersionKind(clientGVK)
					created.SetName(name)
					_ = unstructured.SetNestedMap(created.Object, map[string]interface{}{
						"clientId": "gentian-dovecot",
						"realmId":  xts.Items[i].GetName(),
					}, "spec", "forProvider")
					if err := c.Create(ctx, created); err != nil {
						continue
					}
					obj = created
				}
				conds, _, _ := unstructured.NestedSlice(obj.Object, "status", "conditions")
				for _, cond := range conds {
					if m, ok := cond.(map[string]interface{}); ok && m["type"] == "Ready" && m["status"] == "True" {
						goto next
					}
				}
				_ = unstructured.SetNestedSlice(obj.Object, []interface{}{
					map[string]interface{}{
						"type":               "Ready",
						"status":             "True",
						"reason":             "Available",
						"lastTransitionTime": time.Now().UTC().Format(time.RFC3339),
					},
				}, "status", "conditions")
				_ = c.Status().Update(ctx, obj)
			next:
			}
		}
	}
}
