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
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/catalogue"
	"github.com/gentian-org/gentian-os/internal/controller/provisioner"
	"github.com/gentian-org/gentian-os/internal/kernel/secrets"
	"github.com/gentian-org/gentian-os/internal/kernel/stagingca"
	"github.com/gentian-org/gentian-os/internal/meta"
)

const (
	tenantFinalizer = "gentianos.io/tenant-cleanup"
	tenantLabel     = meta.TenantLabel
	managedByLabel  = meta.ManagedByLabel
	managedByValue  = meta.ManagedByValue
	// portalRedirectComponentLabel marks Ingress objects owned by tenant portal redirect
	// (shared kernel portal). They must not be deleted by app ingress stale cleanup.
	portalRedirectComponentLabel = meta.PortalRedirectComponentLabel
	portalRedirectComponentValue = meta.PortalRedirectComponentValue
	kernelNamespace = meta.KernelNamespace
	conditionNamespaceReady = "NamespaceReady"
)

// servicesNamespace is read from SERVICES_NAMESPACE at process startup.
// When unset, derives gentian-{GENTIAN_STAGE|ENV} so the operator never hardcodes
// a cluster-specific namespace name.
var servicesNamespace = defaultServicesNamespace()

func defaultServicesNamespace() string {
	if v := os.Getenv("SERVICES_NAMESPACE"); v != "" {
		return v
	}
	stage := envOrDefault("GENTIAN_STAGE", envOrDefault("ENV", "dev"))
	return "gentian-" + stage
}

// errDeleteJobPending is returned by delete helpers when a cleanup Job has been
// created but has not yet completed. reconcileDelete treats this as a signal to
// requeue rather than a hard error, so the finalizer is only removed once all
// cleanup Jobs have finished.
var errDeleteJobPending = provisioner.ErrDeleteJobPending

// deleteProvisioningJobs removes completed provisioning Jobs by name, ignoring
// not-found and transient errors. Call this after a cleanup Job completes so
// that the provisioning Jobs are re-created (and the resource re-provisioned)
// on the next tenant deploy.
func (r *TenantReconciler) deleteProvisioningJobs(ctx context.Context, jobNames ...string) {
	prop := metav1.DeletePropagationBackground
	for _, name := range jobNames {
		job := &batchv1.Job{}
		if err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: kernelNamespace}, job); err != nil {
			continue
		}
		_ = r.Delete(ctx, job, &client.DeleteOptions{PropagationPolicy: &prop})
	}
}

func envOrDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func EnvBool(key string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(key))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

// xTenantGVK is the GroupVersionKind for the XTenant composite resource managed
// by Crossplane. The TenantReconciler creates one XTenant per Tenant so the
// Crossplane Composition can provision namespace, networking, OpenBao policy,
// and App claims declaratively alongside the imperative Go provisioners.
var xTenantGVK = schema.GroupVersionKind{
	Group:   "gentianos.io",
	Version: "v1alpha1",
	Kind:    "XTenant",
}

// TenantReconciler reconciles Tenant objects.
//
// +kubebuilder:rbac:groups=gentianos.io,resources=tenants,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=gentianos.io,resources=tenants/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=gentianos.io,resources=tenants/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=resourcequotas,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=limitranges,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=networking.k8s.io,resources=networkpolicies,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=gentianos.io,resources=appprofiles,verbs=get;list;watch
// +kubebuilder:rbac:groups=postgresql.cnpg.io,resources=databases,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;create;update;patch;delete
type TenantReconciler struct {
	client.Client
	// APIReader is an optional uncached client for kernel Secret lookups. The
	// default cached Client can lag behind direct API writes (e.g. envtest).
	APIReader client.Reader
	Scheme *runtime.Scheme
	// Seeder derives and persists per-tenant-per-app credentials into OpenBao.
	// May be nil — in which case all reconcilers skip the seeding step and behave
	// exactly as they did before Inc 21a. This keeps existing envtest suites
	// passing without requiring an OpenBao test double.
	Seeder *secrets.Seeder
	// KernelDomain is the cluster-wide platform domain (e.g. `desk.gentian.org`)
	// on which kernel UIs (Keycloak, Argo CD, portal) are served.
	// Tenant app domains default from tenancy mode when Tenant.spec.domain is
	// unset. Sourced from KERNEL_DOMAIN at startup.
	// See docs/design/multi-tenancy.md §3.
	KernelDomain string
	// TenancyMode controls default app URL shape: multi → {sub}.{tenant}.{kernel};
	// single → {sub}.{kernel}. Sourced from TENANCY_MODE (default multi).
	TenancyMode string
	// TenantDNS01ClusterIssuer is the cert-manager ClusterIssuer used to issue
	// per-tenant wildcard certificates (*.<effectiveDomain>). Defaults to
	// letsencrypt-dns01-cloudflare when unset. Sourced from TENANT_DNS01_CLUSTER_ISSUER.
	TenantDNS01ClusterIssuer string
	// KernelRealm is the name of the shared Keycloak realm for platform identity.
	// Sourced from the KERNEL_REALM env var at startup.
	KernelRealm string
	// CloudflareDNS is an optional edge-DNS adapter: when set, the operator
	// ensures a proxied CNAME *.<effectiveDomain> → tunnel so Cloudflare Total
	// TLS can provision edge certs for tenant app hostnames (e.g.
	// meet.demo.desk.gentian.org). Nil when CLOUDFLARE_* env vars are unset;
	// use DNS-only (grey cloud) or passthrough to origin in that case.
	CloudflareDNS *CloudflareDNSClient
	// RoutingMode is always gateway (Gateway API + Envoy). Sourced from ROUTING_MODE.
	RoutingMode string
	// CrossplaneOnly skips shared-kernel side effects (mail, portal redirect)
	// so tenant lifecycle is driven by the Crossplane graph alone.
	CrossplaneOnly bool
	// CommerceEnabled flags if licensing checks and metering reports are active.
	CommerceEnabled bool
	// CorpAPIURL is the backend commercial API base URL.
	CorpAPIURL string
	// OperatorToken is the bearer token used to authenticate with gentian-corp.
	OperatorToken string
}

// SetupWithManager registers the controller with the controller-manager.
func (r *TenantReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// mapToTenant maps any labelled object back to a reconcile request for the owning Tenant.
	mapToTenant := func(_ context.Context, obj client.Object) []reconcile.Request {
		tenantName := obj.GetLabels()[tenantLabel]
		if tenantName == "" {
			return nil
		}
		return []reconcile.Request{{NamespacedName: types.NamespacedName{Name: tenantName}}}
	}

	// mapAppProfileToTenants maps an AppProfile change to reconcile requests for
	// every Tenant that references the profile in its spec.apps list.
	mapAppProfileToTenants := func(ctx context.Context, obj client.Object) []reconcile.Request {
		profileName := obj.GetName()
		tenantList := &gentianov1alpha1.TenantList{}
		if err := mgr.GetClient().List(ctx, tenantList); err != nil {
			return nil
		}
		var requests []reconcile.Request
		for _, t := range tenantList.Items {
			for _, app := range t.Spec.Apps {
				if app.Profile == profileName {
					requests = append(requests, reconcile.Request{
						NamespacedName: types.NamespacedName{Name: t.Name},
					})
					break
				}
			}
		}
		return requests
	}

	// cnpgDB is an unstructured object used to watch CloudNativePG Database CRs
	// across all tenant namespaces. Status updates (Ready=True) trigger reconciliation
	// to advance the database provisioning sequence.
	cnpgDB := &unstructured.Unstructured{}
	cnpgDB.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   cnpgGroup,
		Version: cnpgVersion,
		Kind:    cnpgDatabaseKind,
	})

	mapAllTenants := func(ctx context.Context, _ client.Object) []reconcile.Request {
		tenantList := &gentianov1alpha1.TenantList{}
		if err := mgr.GetClient().List(ctx, tenantList); err != nil {
			return nil
		}
		requests := make([]reconcile.Request, 0, len(tenantList.Items))
		for _, t := range tenantList.Items {
			requests = append(requests, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: t.Name},
			})
		}
		return requests
	}

	envoyKernelServicePredicate := predicate.NewPredicateFuncs(func(obj client.Object) bool {
		svc, ok := obj.(*corev1.Service)
		if !ok {
			return false
		}
		if svc.GetNamespace() != envoyGatewayInstallNamespace {
			return false
		}
		return svc.GetLabels()["gateway.envoyproxy.io/owning-gateway-name"] == KernelPublicGatewayName &&
			svc.GetLabels()["gateway.envoyproxy.io/owning-gateway-namespace"] == servicesNamespace
	})

	ctrlBuilder := ctrl.NewControllerManagedBy(mgr).
		For(&gentianov1alpha1.Tenant{}).
		Owns(&corev1.Namespace{}).
		Watches(
			&batchv1.Job{},
			handler.EnqueueRequestsFromMapFunc(mapToTenant),
			builder.WithPredicates(predicate.NewPredicateFuncs(func(obj client.Object) bool {
				_, hasLabel := obj.GetLabels()[tenantLabel]
				return hasLabel && obj.GetNamespace() == kernelNamespace
			})),
		).
		Watches(
			cnpgDB,
			handler.EnqueueRequestsFromMapFunc(mapToTenant),
			builder.WithPredicates(predicate.NewPredicateFuncs(func(obj client.Object) bool {
				_, hasLabel := obj.GetLabels()[tenantLabel]
				return hasLabel
			})),
		).
		Watches(
			&gentianov1alpha1.AppProfile{},
			handler.EnqueueRequestsFromMapFunc(mapAppProfileToTenants),
		)

	if isGatewayRoutingMode(r.RoutingMode) {
		ctrlBuilder = ctrlBuilder.Watches(
			&corev1.Service{},
			handler.EnqueueRequestsFromMapFunc(mapAllTenants),
			builder.WithPredicates(envoyKernelServicePredicate),
		)
	}

	return ctrlBuilder.Complete(r)
}

// tenantEffectiveDomain returns the ingress/mail domain for tenant app hostnames.
func (r *TenantReconciler) tenantEffectiveDomain(tenant *gentianov1alpha1.Tenant) string {
	return tenant.EffectiveDomain(r.KernelDomain, r.TenancyMode)
}

// validateTenancyConstraints enforces single-tenancy cluster rules.
func (r *TenantReconciler) validateTenancyConstraints(ctx context.Context, tenant *gentianov1alpha1.Tenant) error {
	if gentianov1alpha1.NormalizeTenancyMode(r.TenancyMode) != gentianov1alpha1.TenancyModeSingle {
		return nil
	}
	if tenant.Name != gentianov1alpha1.SingleTenantName {
		return fmt.Errorf(
			"cluster TENANCY_MODE=single allows only Tenant %q (got %q)",
			gentianov1alpha1.SingleTenantName, tenant.Name,
		)
	}
	var others gentianov1alpha1.TenantList
	if err := r.List(ctx, &others); err != nil {
		return err
	}
	for i := range others.Items {
		other := &others.Items[i]
		if other.Name == tenant.Name || !other.DeletionTimestamp.IsZero() {
			continue
		}
		return fmt.Errorf(
			"cluster TENANCY_MODE=single allows only one Tenant CR (found %q and %q)",
			tenant.Name, other.Name,
		)
	}
	return nil
}

// Reconcile is the main reconciliation loop for Tenant resources.
func (r *TenantReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	start := time.Now()

	tenant := &gentianov1alpha1.Tenant{}
	if err := r.Get(ctx, req.NamespacedName, tenant); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		reconcileErrors.WithLabelValues("tenant").Inc()
		return ctrl.Result{}, err
	}

	// Handle deletion
	if !tenant.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, tenant)
	}

	// Ensure finalizer is present
	if !controllerutil.ContainsFinalizer(tenant, tenantFinalizer) {
		controllerutil.AddFinalizer(tenant, tenantFinalizer)
		if err := r.Update(ctx, tenant); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	return r.runTenantReconcileStages(ctx, &tenantReconcileState{
		tenant: tenant,
		start:  start,
	})
}

// validateTenantPrerequisites checks that all requested AppProfiles exist.
func (r *TenantReconciler) validateTenantPrerequisites(ctx context.Context, tenant *gentianov1alpha1.Tenant) ([]string, error) {
	profileIndex, err := loadAppProfileIndex(ctx, r.Client)
	if err != nil {
		return nil, err
	}
	missingMap := map[string]struct{}{}

	for _, app := range tenant.Spec.Apps {
		profileName, err := catalogue.ResolveTenantAppProfile(ctx, r.Client, app)
		if err != nil {
			return nil, err
		}
		if _, ok := appProfileFromIndex(profileIndex, profileName); !ok {
			missingMap[profileName] = struct{}{}
		}
	}

	missing := make([]string, 0, len(missingMap))
	for name := range missingMap {
		missing = append(missing, name)
	}

	return missing, nil
}

// reconcileDelete handles Tenant deletion based on deletionPolicy.
func (r *TenantReconciler) reconcileDelete(ctx context.Context, tenant *gentianov1alpha1.Tenant) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// awaitJob wraps each delete helper: if the helper returns errDeleteJobPending
	// the reconciler requeues rather than treating it as a hard error.
	awaitJob := func(err error) (bool, ctrl.Result, error) {
		if err == nil {
			return false, ctrl.Result{}, nil
		}
		if err == errDeleteJobPending {
			return true, ctrl.Result{RequeueAfter: 2 * time.Second}, nil
		}
		return true, ctrl.Result{}, err
	}

	// Clean up identity resources before removing the namespace.
	if requeue, res, err := awaitJob(r.deleteIdentity(ctx, tenant)); requeue {
		return res, err
	}

	// Clean up database resources before removing the namespace.
	if err := r.deleteDatabase(ctx, tenant); err != nil {
		return ctrl.Result{}, err
	}

	// Clean up MariaDB resources before removing the namespace.
	if requeue, res, err := awaitJob(r.deleteMariaDB(ctx, tenant)); requeue {
		return res, err
	}

	// Clean up storage resources before removing the namespace.
	if requeue, res, err := awaitJob(r.deleteStorage(ctx, tenant)); requeue {
		return res, err
	}

	// Clean up cache resources before removing the namespace.
	if requeue, res, err := awaitJob(r.deleteCache(ctx, tenant)); requeue {
		return res, err
	}

	// Clean up app deployment resources (always, regardless of DeletionPolicy).
	if err := r.deleteAppDeployment(ctx, tenant); err != nil {
		return ctrl.Result{}, err
	}

	// Clean up edge routing (Ingress or Gateway API), wildcard cert, and DNS.
	if err := r.deleteEdgeRouting(ctx, tenant); err != nil {
		return ctrl.Result{}, err
	}

	// Clean up IntegrationBinding CRs (always deleted regardless of DeletionPolicy).
	if err := r.deleteIntegrationBindings(ctx, tenant); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.deleteAppGrants(ctx, tenant); err != nil {
		return ctrl.Result{}, err
	}

	// Clean up mail resources (Application CRs always; Secrets under DeletionPolicy=Delete).
	if err := r.deleteMail(ctx, tenant); err != nil {
		return ctrl.Result{}, err
	}

	// Clean up portal redirect routes.
	if err := r.deletePortalRedirect(ctx, tenant); err != nil {
		return ctrl.Result{}, err
	}

	// Delete the XTenant composite so Crossplane cascades deletion of
	// the Composition-managed resources (Namespace, NetworkPolicy, OpenBao policy,
	// App claims). "Not found" is treated as already deleted.
	if err := r.deleteXTenant(ctx, tenant); err != nil {
		return ctrl.Result{}, err
	}

	// Apply per-DeletionPolicy cleanup in addition to the Crossplane cascade.
	// This is necessary both as a direct/fallback mechanism (e.g. in environments
	// without Crossplane) and to satisfy the controller's own ownership invariants.
	nsName := tenantNamespaceName(tenant)
	if tenant.Spec.DeletionPolicy == gentianov1alpha1.DeletionPolicyDelete {
		logger.Info("deletionPolicy=Delete: removing tenant namespace", "namespace", nsName)
		ns := &corev1.Namespace{}
		if err := r.Get(ctx, types.NamespacedName{Name: nsName}, ns); err == nil {
			if err := r.Delete(ctx, ns); client.IgnoreNotFound(err) != nil {
				return ctrl.Result{}, err
			}
		}
	} else {
		// DeletionPolicyRetain: keep namespace, only remove orchestrator-owned sub-resources.
		logger.Info("deletionPolicy=Retain: preserving tenant namespace", "namespace", nsName)
		if err := r.deleteOwnedResourcesInNamespace(ctx, nsName); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Wait for Crossplane to fully process the XTenant deletion before removing
	// the Tenant finalizer. This guarantees the cascade has completed on clusters
	// where Crossplane is running, so undeploy never leaves a dangling XTenant.
	xr := &unstructured.Unstructured{}
	xr.SetGroupVersionKind(xTenantGVK)
	if err := r.Get(ctx, types.NamespacedName{Name: tenant.Name}, xr); err == nil {
		logger.Info("waiting for XTenant deletion to complete", "tenant", tenant.Name)
		return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
	} else if client.IgnoreNotFound(err) != nil {
		return ctrl.Result{}, err
	}

	if err := r.purgeTenantKernelResources(ctx, tenant); err != nil {
		return ctrl.Result{}, err
	}

	if err := r.deleteTenantProvisioningConfigMap(ctx, tenant.Name); err != nil {
		return ctrl.Result{}, err
	}

	controllerutil.RemoveFinalizer(tenant, tenantFinalizer)
	return ctrl.Result{}, r.Update(ctx, tenant)
}

// deleteOwnedResourcesInNamespace removes ResourceQuota, LimitRange, and NetworkPolicy
// owned by the orchestrator from the namespace, leaving customer workloads intact.
func (r *TenantReconciler) deleteOwnedResourcesInNamespace(ctx context.Context, nsName string) error {
	rq := &corev1.ResourceQuota{ObjectMeta: metav1.ObjectMeta{
		Name: "tenant-quota", Namespace: nsName,
	}}
	if err := r.Delete(ctx, rq); client.IgnoreNotFound(err) != nil {
		return err
	}

	lr := &corev1.LimitRange{ObjectMeta: metav1.ObjectMeta{
		Name: "tenant-limits", Namespace: nsName,
	}}
	if err := r.Delete(ctx, lr); client.IgnoreNotFound(err) != nil {
		return err
	}

	np := &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{
		Name: "tenant-isolation", Namespace: nsName,
	}}
	if err := r.Delete(ctx, np); client.IgnoreNotFound(err) != nil {
		return err
	}

	return nil
}

// ensureRegistryCredentials replicates the kernel-managed `registry-credentials`
// Secret from the services namespace into the tenant namespace, and manages
// scoped registry credentials for proprietary applications.
func (r *TenantReconciler) ensureRegistryCredentials(ctx context.Context, tenant *gentianov1alpha1.Tenant, nsName string) error {
	const defaultSecretName = "registry-credentials"

	// 1. Replicate default registry credentials
	source := &corev1.Secret{}
	if err := r.Get(ctx, types.NamespacedName{Name: defaultSecretName, Namespace: servicesNamespace}, source); err != nil {
		if !errors.IsNotFound(err) {
			return fmt.Errorf("failed to read source registry-credentials in %s: %w", servicesNamespace, err)
		}
	} else {
		desired := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      defaultSecretName,
				Namespace: nsName,
				Labels: map[string]string{
					tenantLabel:    tenant.Name,
					managedByLabel: managedByValue,
				},
			},
			Type: source.Type,
			Data: source.Data,
		}

		existing := &corev1.Secret{}
		err := r.Get(ctx, types.NamespacedName{Name: defaultSecretName, Namespace: nsName}, existing)
		if errors.IsNotFound(err) {
			if err := r.Create(ctx, desired); err != nil {
				return err
			}
		} else if err != nil {
			return err
		} else if !equality.Semantic.DeepEqual(existing.Data, desired.Data) || existing.Type != desired.Type {
			patch := client.MergeFrom(existing.DeepCopy())
			existing.Type = desired.Type
			existing.Data = desired.Data
			if existing.Labels == nil {
				existing.Labels = map[string]string{}
			}
			existing.Labels[tenantLabel] = tenant.Name
			existing.Labels[managedByLabel] = managedByValue
			if err := r.Patch(ctx, existing, patch); err != nil {
				return err
			}
		}
	}

	// 2. Fetch scoped credentials for proprietary apps if commerce integration is enabled
	for _, app := range tenant.Spec.Apps {
		profileName := app.Profile
		if profileName == "" {
			continue
		}

		profile := &gentianov1alpha1.AppProfile{}
		if err := r.Get(ctx, types.NamespacedName{Name: profileName}, profile); err != nil {
			return fmt.Errorf("failed to read AppProfile %s: %w", profileName, err)
		}

		if profile.Spec.License != "proprietary" {
			continue
		}

		secretName := "registry-credentials-" + profileName
		existingSec := &corev1.Secret{}
		err := r.Get(ctx, types.NamespacedName{Name: secretName, Namespace: nsName}, existingSec)
		if err == nil {
			// Secret exists already
			continue
		}
		if !errors.IsNotFound(err) {
			return fmt.Errorf("failed to check existing secret %s: %w", secretName, err)
		}

		// Scoped secret is missing; exchange the grant
		if !r.CommerceEnabled {
			return fmt.Errorf("app %s has a proprietary license but commercial integration (GENTIAN_COMMERCE_ENABLED) is disabled", profileName)
		}

		annoKey := "gentianos.io/install-grant-" + profileName
		log.FromContext(ctx).Info("checking registry credentials for proprietary app", "profile", profileName, "annoKey", annoKey, "annotations", tenant.Annotations)
		jwtToken := tenant.Annotations[annoKey]
		if jwtToken == "" {
			return fmt.Errorf("app %s has a proprietary license; please purchase a subscription and provide the install grant JWT annotation %s", profileName, annoKey)
		}

		jti, err := extractJTIFromJWT(jwtToken)
		if err != nil {
			return fmt.Errorf("failed to extract token ID from grant for %s: %w", profileName, err)
		}

		log.FromContext(ctx).Info("exchanging install grant", "profile", profileName, "jti", jti)
		exchangeRes, err := r.exchangeInstallGrant(ctx, jti, jwtToken)
		if err != nil {
			log.FromContext(ctx).Error(err, "failed to exchange install grant", "profile", profileName, "jti", jti)
			return fmt.Errorf("failed to exchange install grant for %s: %w", profileName, err)
		}
		log.FromContext(ctx).Info("exchanged install grant successfully", "profile", profileName, "entitlementId", exchangeRes.EntitlementId)

		dockerConfigJSON, err := buildDockerConfigJSON(
			exchangeRes.RegistryCredential.Host,
			exchangeRes.RegistryCredential.Username,
			exchangeRes.RegistryCredential.Password,
		)
		if err != nil {
			return fmt.Errorf("failed to build dockerconfig JSON for %s: %w", profileName, err)
		}

		registrySec := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      secretName,
				Namespace: nsName,
				Labels: map[string]string{
					tenantLabel:    tenant.Name,
					managedByLabel: managedByValue,
				},
			},
			Type: corev1.SecretTypeDockerConfigJson,
			Data: map[string][]byte{
				corev1.DockerConfigJsonKey: dockerConfigJSON,
			},
		}
		log.FromContext(ctx).Info("creating registry credentials secret", "secret", secretName, "namespace", nsName)
		if err := r.Create(ctx, registrySec); err != nil {
			log.FromContext(ctx).Error(err, "failed to create registry credentials secret", "secret", secretName)
			return fmt.Errorf("failed to create secret %s: %w", secretName, err)
		}
		log.FromContext(ctx).Info("created registry credentials secret successfully", "secret", secretName)

		meteringSecName := "metering-secret-" + profileName
		meteringSec := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      meteringSecName,
				Namespace: nsName,
				Labels: map[string]string{
					tenantLabel:    tenant.Name,
					managedByLabel: managedByValue,
				},
			},
			Type: corev1.SecretTypeOpaque,
			StringData: map[string]string{
				"metering-secret": exchangeRes.MeteringSecret,
				"entitlement-id":  exchangeRes.EntitlementId,
				"product-sku":     profileName,
			},
		}
		log.FromContext(ctx).Info("creating metering secret", "secret", meteringSecName, "namespace", nsName)
		if err := r.Create(ctx, meteringSec); err != nil {
			log.FromContext(ctx).Error(err, "failed to create metering secret", "secret", meteringSecName)
			return fmt.Errorf("failed to create secret %s: %w", meteringSecName, err)
		}
		log.FromContext(ctx).Info("created metering secret successfully", "secret", meteringSecName)
	}

	return nil
}

type installExchangeRequest struct {
	InstallGrantJwt string `json:"installGrantJwt"`
}

type registryCreds struct {
	Host      string    `json:"host"`
	Username  string    `json:"username"`
	Password  string    `json:"password"`
	Scopes    []string  `json:"scopes"`
	ExpiresAt time.Time `json:"expiresAt"`
}

type installExchangeResponse struct {
	Ok                 bool          `json:"ok"`
	Profile            string        `json:"profile"`
	EntitlementId      string        `json:"entitlementId"`
	RegistryCredential registryCreds `json:"registryCredential"`
	MeteringSecret     string        `json:"meteringSecret"`
}

func (r *TenantReconciler) exchangeInstallGrant(ctx context.Context, jti, jwtToken string) (*installExchangeResponse, error) {
	url := fmt.Sprintf("%s/api/v1/install-grants/%s/exchange", strings.TrimRight(r.CorpAPIURL, "/"), jti)
	reqBody, err := json.Marshal(installExchangeRequest{InstallGrantJwt: jwtToken})
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(reqBody))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+r.OperatorToken)

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("exchange failed with status %d: %s", resp.StatusCode, string(body))
	}

	var out installExchangeResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return &out, nil
}

func extractJTIFromJWT(token string) (string, error) {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return "", fmt.Errorf("invalid JWT format")
	}
	payloadSegment := parts[1]
	if l := len(payloadSegment) % 4; l > 0 {
		payloadSegment += strings.Repeat("=", 4-l)
	}
	decoded, err := base64.URLEncoding.DecodeString(payloadSegment)
	if err != nil {
		decoded, err = base64.StdEncoding.DecodeString(payloadSegment)
		if err != nil {
			return "", err
		}
	}
	var claims struct {
		JTI string `json:"jti"`
	}
	if err := json.Unmarshal(decoded, &claims); err != nil {
		return "", err
	}
	if claims.JTI == "" {
		return "", fmt.Errorf("jti claim not found in JWT")
	}
	return claims.JTI, nil
}

type dockerConfig struct {
	Auths map[string]dockerAuth `json:"auths"`
}

type dockerAuth struct {
	Username string `json:"username"`
	Password string `json:"password"`
	Auth     string `json:"auth"`
}

func buildDockerConfigJSON(host, username, password string) ([]byte, error) {
	authStr := username + ":" + password
	encodedAuth := base64.StdEncoding.EncodeToString([]byte(authStr))
	config := dockerConfig{
		Auths: map[string]dockerAuth{
			host: {
				Username: username,
				Password: password,
				Auth:     encodedAuth,
			},
		},
	}
	return json.Marshal(config)
}

// ensureStagingCaTrust bootstraps gentian-staging-ca-tls in the services namespace
// and replicates it into the tenant namespace for in-cluster OIDC clients.
func (r *TenantReconciler) ensureStagingCaTrust(ctx context.Context, tenant *gentianov1alpha1.Tenant, nsName string) error {
	const secretName = stagingca.SecretName

	if _, err := stagingca.EnsureStagingCASecret(ctx, r.Client, servicesNamespace,
		stagingca.DefaultCertManagerNS, stagingca.DefaultLeafSecret); err != nil {
		return fmt.Errorf("bootstrap staging CA in %s: %w", servicesNamespace, err)
	}

	source := &corev1.Secret{}
	if err := r.Get(ctx, types.NamespacedName{Name: secretName, Namespace: servicesNamespace}, source); err != nil {
		if errors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("failed to read source %s in %s: %w", secretName, servicesNamespace, err)
	}

	desired := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretName,
			Namespace: nsName,
			Labels: map[string]string{
				tenantLabel:    tenant.Name,
				managedByLabel: managedByValue,
			},
		},
		Type: source.Type,
		Data: source.Data,
	}

	existing := &corev1.Secret{}
	err := r.Get(ctx, types.NamespacedName{Name: secretName, Namespace: nsName}, existing)
	if errors.IsNotFound(err) {
		return r.Create(ctx, desired)
	}
	if err != nil {
		return err
	}
	if !equality.Semantic.DeepEqual(existing.Data, desired.Data) || existing.Type != desired.Type {
		patch := client.MergeFrom(existing.DeepCopy())
		existing.Type = desired.Type
		existing.Data = desired.Data
		if existing.Labels == nil {
			existing.Labels = map[string]string{}
		}
		existing.Labels[tenantLabel] = tenant.Name
		existing.Labels[managedByLabel] = managedByValue
		return r.Patch(ctx, existing, patch)
	}
	return nil
}

// setCondition upserts a metav1.Condition on the Tenant status.
func (r *TenantReconciler) setCondition(tenant *gentianov1alpha1.Tenant, condType string, status metav1.ConditionStatus, reason, message string) {
	now := metav1.Now()
	for i, c := range tenant.Status.Conditions {
		if c.Type == condType {
			if c.Status == status && c.Reason == reason {
				return
			}
			tenant.Status.Conditions[i] = metav1.Condition{
				Type:               condType,
				Status:             status,
				Reason:             reason,
				Message:            message,
				LastTransitionTime: now,
				ObservedGeneration: tenant.Generation,
			}
			return
		}
	}
	tenant.Status.Conditions = append(tenant.Status.Conditions, metav1.Condition{
		Type:               condType,
		Status:             status,
		Reason:             reason,
		Message:            message,
		LastTransitionTime: now,
		ObservedGeneration: tenant.Generation,
	})
}

// tenantNamespaceName returns the namespace name for the tenant.
func tenantNamespaceName(tenant *gentianov1alpha1.Tenant) string {
	if tenant.Spec.Isolation != nil && tenant.Spec.Isolation.Namespace != "" {
		return tenant.Spec.Isolation.Namespace
	}
	return fmt.Sprintf("tenant-%s", tenant.Name)
}

// ── XTenant helpers ───────────────────────────────────────────────────────────

// ensureTenantXR creates or updates the XTenant composite resource that drives
// the Crossplane Composition for this tenant. The Composition provisions the
// tenant namespace, networking, OpenBao policy, and App claims declaratively.
func (r *TenantReconciler) ensureTenantXR(ctx context.Context, tenant *gentianov1alpha1.Tenant) error {
	xr := &unstructured.Unstructured{}
	xr.SetGroupVersionKind(xTenantGVK)
	err := r.Get(ctx, types.NamespacedName{Name: tenant.Name}, xr)
	desired, buildErr := r.buildXTenant(ctx, tenant)
	if buildErr != nil {
		return buildErr
	}
	if errors.IsNotFound(err) {
		return r.Create(ctx, desired)
	}
	if err != nil {
		return err
	}
	// Patch only the user-controlled spec fields so that Crossplane-managed
	// fields (compositionRef, resourceRefs, managementPolicies, etc.) are not
	// overwritten. We build the desired state, then merge-patch just the fields
	// that differ.
	patch := client.MergeFrom(xr.DeepCopy())
	desiredSpec, _ := desired.Object["spec"].(map[string]interface{})
	if specMap, ok := xr.Object["spec"].(map[string]interface{}); ok {
		for k, v := range desiredSpec {
			specMap[k] = v
		}
	} else {
		xr.Object["spec"] = desiredSpec
	}
	return r.Patch(ctx, xr, patch)
}

// deleteXTenant deletes the XTenant composite. Crossplane cascades deletion to
// all composed resources (Namespace, NetworkPolicy, OpenBao policy, App claims).
// "Not found" is treated as already deleted (idempotent).
// The function removes the crossplane.io/paused annotation before issuing the
// DELETE so that a stale pause can never block the Crossplane finalizer handler.
func (r *TenantReconciler) deleteXTenant(ctx context.Context, tenant *gentianov1alpha1.Tenant) error {
	xr := &unstructured.Unstructured{}
	xr.SetGroupVersionKind(xTenantGVK)
	xr.SetName(tenant.Name)

	if err := r.Get(ctx, types.NamespacedName{Name: tenant.Name}, xr); err != nil {
		return client.IgnoreNotFound(err)
	}

	// Remove the paused annotation if present so Crossplane is free to run its
	// finalizer handler and cascade-delete the composed managed resources.
	annotations := xr.GetAnnotations()
	if _, paused := annotations["crossplane.io/paused"]; paused {
		delete(annotations, "crossplane.io/paused")
		xr.SetAnnotations(annotations)
		if err := r.Update(ctx, xr); err != nil {
			return fmt.Errorf("unpause XTenant %s: %w", tenant.Name, err)
		}
	}

	return client.IgnoreNotFound(r.Delete(ctx, xr))
}

// buildXTenant constructs an XTenant composite object from a Tenant's spec.
// The XTenant is cluster-scoped; its name matches the Tenant name.
func (r *TenantReconciler) buildXTenant(ctx context.Context, tenant *gentianov1alpha1.Tenant) (*unstructured.Unstructured, error) {
	xr := &unstructured.Unstructured{}
	xr.SetGroupVersionKind(xTenantGVK)
	xr.SetName(tenant.Name)
	xr.SetLabels(map[string]string{
		tenantLabel:    tenant.Name,
		managedByLabel: managedByValue,
	})

	spec := map[string]interface{}{
		"displayName":  tenant.Spec.DisplayName,
		"adminEmail":   tenant.Spec.AdminEmail,
		"kernelDomain": r.KernelDomain,
	}
	if tenant.Spec.Domain != "" {
		spec["domain"] = tenant.Spec.Domain
	}
	if tenant.Spec.DeletionPolicy != "" {
		spec["deletionPolicy"] = string(tenant.Spec.DeletionPolicy)
	}

	if tenant.Spec.Isolation != nil {
		iso := map[string]interface{}{}
		if tenant.Spec.Isolation.Mode != "" {
			iso["mode"] = string(tenant.Spec.Isolation.Mode)
		}
		if tenant.Spec.Isolation.Namespace != "" {
			iso["namespace"] = tenant.Spec.Isolation.Namespace
		}
		if tenant.Spec.Isolation.KeycloakRealm != "" {
			iso["keycloakRealm"] = tenant.Spec.Isolation.KeycloakRealm
		}
		if tenant.Spec.Isolation.DatabasePrefix != "" {
			iso["databasePrefix"] = tenant.Spec.Isolation.DatabasePrefix
		}
		if tenant.Spec.Isolation.S3Prefix != "" {
			iso["s3Prefix"] = tenant.Spec.Isolation.S3Prefix
		}
		if len(iso) > 0 {
			spec["isolation"] = iso
		}
	}

	if tenant.Spec.Quotas != nil {
		quotas := map[string]interface{}{}
		if tenant.Spec.Quotas.Storage != nil {
			quotas["storage"] = tenant.Spec.Quotas.Storage.String()
		}
		if tenant.Spec.Quotas.CPU != nil {
			quotas["cpu"] = tenant.Spec.Quotas.CPU.String()
		}
		if tenant.Spec.Quotas.Memory != nil {
			quotas["memory"] = tenant.Spec.Quotas.Memory.String()
		}
		if tenant.Spec.Quotas.MaxApps > 0 {
			quotas["maxApps"] = int64(tenant.Spec.Quotas.MaxApps)
		}
		if tenant.Spec.Quotas.MaxPods > 0 {
			quotas["maxPods"] = int64(tenant.Spec.Quotas.MaxPods)
		}
		if len(quotas) > 0 {
			spec["quotas"] = quotas
		}
	}

	profileIndex, err := loadAppProfileIndex(ctx, r.Client)
	if err != nil {
		return nil, fmt.Errorf("load AppProfile index: %w", err)
	}

	apps := make([]interface{}, 0, len(tenant.Spec.Apps))
	for _, app := range tenant.Spec.Apps {
		entry := map[string]interface{}{"profile": app.Profile}
		profile, exists := appProfileFromIndex(profileIndex, app.Profile)
		if exists {
			// ApiProfiles run no workload; keep them out of the XTenant so the
			// composition creates no App claim / Helm release for them.
			if gentianov1alpha1.ProfileIsAPI(profile) {
				continue
			}
			if profile.Spec.CompositionRef != "" {
				variant := strings.TrimPrefix(profile.Spec.CompositionRef, "app-")
				if variant != "" {
					entry["variant"] = variant
				}
			}
		} else {
			log.FromContext(ctx).Info("AppProfile not found in index in buildXTenant", "profile", app.Profile)
		}
		if app.Config != nil {
			cfg := map[string]interface{}{}
			if app.Config.Replicas != nil {
				cfg["replicas"] = int64(*app.Config.Replicas)
			}
			if app.Config.ExtraValues != nil && len(app.Config.ExtraValues.Raw) > 0 {
				var extra map[string]interface{}
				if err := json.Unmarshal(app.Config.ExtraValues.Raw, &extra); err == nil && len(extra) > 0 {
					cfg["extraValues"] = extra
				}
			}
			if len(cfg) > 0 {
				entry["config"] = cfg
			}
		}
		apps = append(apps, entry)
	}
	spec["apps"] = apps

	_ = unstructured.SetNestedField(xr.Object, spec, "spec")
	return xr, nil
}
