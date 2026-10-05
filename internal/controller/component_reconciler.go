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
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/kernel/secrets"
	"github.com/gentian-org/gentian-os/internal/layout"
	"github.com/gentian-org/gentian-os/internal/security"
)

// ComponentReconciler turns a Component into what runs: the chart its profile
// names as a provider-helm Release in the component's namespace, the
// requirements its profile declares fulfilled beside it, and one route and
// one policy per gateway exposure in the tenant's zone. It writes nothing
// outside the component's namespace except the ReferenceGrant the zone's
// policy needs in the edge namespace.
type ComponentReconciler struct {
	client.Client
	Scheme         *runtime.Scheme
	KernelDomain   string
	KernelRealm    string
	TenancyMode    string
	Cluster        string
	BouncerService string
	// DirectorURL is where the desktop relays to; empty derives it from the
	// layout's control namespace.
	DirectorURL string
	// UsherURL is where a desktop asks what a person may open; empty derives
	// it from the edge namespace.
	UsherURL string
	// CredentialManagerURL overrides where a component is told the credential
	// manager is. Empty derives it from the layout.
	CredentialManagerURL string
	// Seeder holds the credentials of the databases this reconciler makes
	// for tenant components. Nil leaves those requirements waiting.
	Seeder *secrets.Seeder
}

// The markers are a free-floating block: controller-gen ignores a block that
// is part of a declaration's doc comment.
//
// +kubebuilder:rbac:groups=gentianos.io,resources=components,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=gentianos.io,resources=components/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=gentianos.io,resources=components/finalizers,verbs=update
// +kubebuilder:rbac:groups=gentianos.io,resources=componentprofiles,verbs=get;list;watch
// +kubebuilder:rbac:groups=helm.crossplane.io,resources=releases,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=referencegrants,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=postgresql.cnpg.io,resources=clusters,verbs=get;list;watch

const (
	conditionComponentReady = "Ready"
	componentFinalizer      = "gentianos.io/component-cleanup"
	componentLabel          = "gentianos.io/component"
	componentRequeue        = 15 * time.Second
	// bouncerRouteLabel marks an HTTPRoute whose L2 question the gateway
	// reconciler copies into the bouncer's table; the question is in the
	// annotations below.
	bouncerRouteLabel = "gentianos.io/bouncer"
)

// helmReleaseGVK is provider-helm's Release, the shape a component's chart is
// installed as.
var helmReleaseGVK = schema.GroupVersionKind{
	Group:   "helm.crossplane.io",
	Version: "v1beta1",
	Kind:    "Release",
}

const (
	bouncerRelationAnnotation = "gentianos.io/bouncer-relation"
	bouncerObjectAnnotation   = "gentianos.io/bouncer-object"
	bouncerForwardAnnotation  = "gentianos.io/bouncer-forward-token"
	bouncerCookieAnnotation   = "gentianos.io/bouncer-cookie"
	// The zone's id token cookie and its realm's end-session endpoint, for
	// the bouncer's sign-out.
	bouncerIDCookieAnnotation   = "gentianos.io/bouncer-id-cookie"
	bouncerEndSessionAnnotation = "gentianos.io/bouncer-end-session"
	bouncerAuthModeAnnotation   = "gentianos.io/bouncer-mode"
	// bouncerDenyPathsAnnotation carries the exposure's denyPaths to the
	// bouncer's table. Comma-separated because an annotation is a string and a
	// path cannot contain a comma without being escaped, which none are.
	bouncerDenyPathsAnnotation    = "gentianos.io/bouncer-deny-paths"
	componentDatabaseSecretSuffix = "-database"
)

func (r *ComponentReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	comp := &gentianov1alpha1.Component{}
	if err := r.Get(ctx, req.NamespacedName, comp); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !comp.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(comp, componentFinalizer) {
			if err := r.deleteZoneGrant(ctx, comp); err != nil {
				return ctrl.Result{}, err
			}
			controllerutil.RemoveFinalizer(comp, componentFinalizer)
			return ctrl.Result{}, r.Update(ctx, comp)
		}
		return ctrl.Result{}, nil
	}
	if !controllerutil.ContainsFinalizer(comp, componentFinalizer) {
		controllerutil.AddFinalizer(comp, componentFinalizer)
		return ctrl.Result{}, r.Update(ctx, comp)
	}

	profile := &gentianov1alpha1.ComponentProfile{}
	if err := r.Get(ctx, types.NamespacedName{Name: comp.Spec.ProfileRef.Name}, profile); err != nil {
		if errors.IsNotFound(err) {
			return r.status(ctx, comp, metav1.ConditionFalse, "ProfileMissing",
				fmt.Sprintf("ComponentProfile %q is not installed", comp.Spec.ProfileRef.Name), componentRequeue)
		}
		return ctrl.Result{}, err
	}
	if comp.Spec.Class != gentianov1alpha1.ComponentClassApp {
		return r.status(ctx, comp, metav1.ConditionFalse, "ClassUnsupported",
			fmt.Sprintf("class %q is not reconciled yet; only apps are", comp.Spec.Class), 0)
	}
	tenant, err := r.tenantOf(ctx, comp.Namespace)
	if err != nil {
		return ctrl.Result{}, err
	}
	if tenant == nil {
		return r.status(ctx, comp, metav1.ConditionFalse, "NoTenant",
			fmt.Sprintf("namespace %q belongs to no Tenant", comp.Namespace), componentRequeue)
	}

	// Privileges are requests, never grants (AD-5). Anything the profile asks
	// for that no live grant answers holds the install here: not rejected,
	// because the approver may still say yes and a rejected Component would
	// have to be reinstalled to receive the answer; and not run without it,
	// because a component that quietly starts unprivileged is the exact
	// failure the mechanism exists to prevent. The reconciler stops before it
	// has written anything -- no network policy, no release -- so a component
	// waiting for an approval has no half-built footprint in the namespace.
	comp.Status.PendingPrivileges = security.PendingPrivileges(profile, comp, time.Now())
	if len(comp.Status.PendingPrivileges) > 0 {
		return r.status(ctx, comp, metav1.ConditionFalse, "PrivilegesPending",
			fmt.Sprintf("waiting for approval of %s", strings.Join(comp.Status.PendingPrivileges, ", ")),
			componentRequeue)
	}

	zone := r.zoneOf(tenant)
	values := map[string]interface{}{}
	if profile.Spec.Package.ExtraValues != nil && len(profile.Spec.Package.ExtraValues.Raw) > 0 {
		if err := decodeJSONObject(profile.Spec.Package.ExtraValues.Raw, &values); err != nil {
			return r.status(ctx, comp, metav1.ConditionFalse, "ProfileInvalid", "package.extraValues is not an object", 0)
		}
	}

	// A chart that needs what the app Composition renders is delivered
	// through it, whole: its requirements, its values and its release are
	// the Composition's, and nothing of them is written here as well.
	composed := composedDelivery(profile)

	// Requirements first: a chart whose database does not exist yet is not
	// installed, it is waited for.
	if !composed && profile.Spec.Requires != nil && profile.Spec.Requires.Services != nil && profile.Spec.Requires.Services.Database != nil {
		ready, reason, message, err := r.ensureDatabaseRequirement(ctx, comp, tenant)
		if err != nil {
			return ctrl.Result{}, err
		}
		if !ready {
			return r.status(ctx, comp, metav1.ConditionFalse, reason, message, componentRequeue)
		}
		mergeValues(values, databaseValues(profile, comp.Name+componentDatabaseSecretSuffix))
	}
	// What the platform tells any component about itself, where its profile
	// says its chart takes it. Nothing is keyed on which component this is.
	mergeValues(values, r.platformValues(profile, tenant, zone))
	// The namespace is closed by default; the component's pods may reach
	// what its requirements were fulfilled with, written down before the
	// chart runs so its first connection is not the one that is refused.
	if !composed {
		if err := r.ensureNetworkPolicy(ctx, comp, profile, tenant); err != nil {
			return ctrl.Result{}, fmt.Errorf("network policy: %w", err)
		}
	}

	// The package is exactly one of chart, composition, api or addon, and what
	// "installed" means differs for each. Two of them run nothing at all, and
	// that is not a gap to refuse -- it is the answer.
	releaseReady, releaseMessage := true, ""
	switch {
	case composed:
		var err error
		releaseReady, releaseMessage, err = r.ensureAppClaim(ctx, comp, tenant, zone)
		if err != nil {
			return ctrl.Result{}, err
		}

	case profile.Spec.Package.Chart != nil:
		var err error
		releaseReady, releaseMessage, err = r.ensureRelease(ctx, comp, profile, values)
		if err != nil {
			return ctrl.Result{}, err
		}

	case profile.Spec.Package.Addon != nil:
		// An addon is activation state inside another component, not a
		// deployment. There is nothing to install; what has to be true is that
		// the thing it activates into is there, because an addon whose base is
		// missing is a tile pointing at a host nobody serves.
		//
		// The activation itself is the base's: the base's chart is what turns
		// the feature on, from the addon list the tenant carries. This
		// component exists so the addon is a first-class thing that can be
		// named, granted privileges, and given a tile.
		ready, message, err := r.addonBaseReady(ctx, comp, profile)
		if err != nil {
			return ctrl.Result{}, err
		}
		if !ready {
			return r.status(ctx, comp, metav1.ConditionFalse, "AddonBaseNotReady", message, componentRequeue)
		}
		releaseMessage = message

	case profile.Spec.Package.API != nil:
		// An API entry runs nothing here: it is an external service the tenant
		// is pointed at. The platform's part is the tile and whatever
		// integration the profile declares, both of which are already done by
		// the time this is reached.
		if profile.Spec.Package.API.Runtime == gentianov1alpha1.APIIntegrationRuntimePortalProxy {
			// The proxy runtime needs something in the tenant's DMZ to
			// terminate the caller and hold the credential, and that is not
			// built. Refusing names it rather than reporting Ready for an
			// entry nothing serves.
			return r.status(ctx, comp, metav1.ConditionFalse, "PackageUnsupported",
				"an API entry with runtime portal-proxy needs a publishing proxy, which is not built yet", 0)
		}
		releaseMessage = "external service at " + profile.Spec.Package.API.BaseURL

	default:
		// A composition package: the claim chooses the Composition and
		// Crossplane renders it, so there is nothing for this reconciler to
		// create. Naming the delivery makes the refusal actionable.
		return r.status(ctx, comp, metav1.ConditionFalse, "PackageUnsupported",
			fmt.Sprintf("delivery %q is not reconciled here", profile.Spec.Delivery()), 0)
	}

	// Exposures: every gateway entry becomes a route in this namespace and
	// a policy carrying the zone's session and the bouncer. Not before the
	// zone's client exists -- a policy naming a missing Secret is invalid,
	// and an invalid policy leaves its route open.
	// What this component actually PUBLISHES, which is not the same as what it
	// declares. An entry naming another component's Service routes nothing
	// here -- the named component already serves that host -- so a component
	// whose every entry is somebody else's needs no zone client, no
	// ReferenceGrant and no policy, and must not wait for a secret it will
	// never use. That is every addon.
	routable, refused := routableExposures(comp, profile)
	for _, e := range refused {
		logger.Info("gateway exposure not routed: only authMode oidc is served on the authenticated Gateway",
			"component", comp.Name, "namespace", comp.Namespace, "exposure", e.Name, "authMode", string(e.AuthMode))
	}

	exposed := 0
	if len(routable) > 0 {
		zoneReady, err := r.zoneReady(ctx, zone)
		if err != nil {
			return ctrl.Result{}, err
		}
		if !zoneReady {
			return r.status(ctx, comp, metav1.ConditionFalse, "ZoneNotReady",
				fmt.Sprintf("the zone's client secret %s/%s does not exist yet; nothing is routed until it does", servicesNamespace, zone.secretName), componentRequeue)
		}
		if err := r.ensureZoneGrant(ctx, comp); err != nil {
			return ctrl.Result{}, err
		}
		if err := r.ensureZoneSecret(ctx, comp, zone); err != nil {
			return ctrl.Result{}, fmt.Errorf("zone secret: %w", err)
		}
		var oidcRoutes []string
		forward := false
		for _, e := range routable {
			routeName, err := r.ensureExposureRoute(ctx, comp, profile, tenant, zone, e)
			if err != nil {
				return ctrl.Result{}, fmt.Errorf("expose %s: %w", e.Name, err)
			}
			if e.AuthMode == gentianov1alpha1.AuthModeOIDC {
				oidcRoutes = append(oidcRoutes, routeName)
				forward = forward || e.ForwardToken
			}
			exposed++
		}
		// One policy over every oidc route of the component. Envoy Gateway
		// binds a session's cookies to the policy that made it, so two
		// policies on one host would be two sessions; forwardToken is
		// therefore the component's, held if any of its entries holds it.
		if len(oidcRoutes) > 0 {
			authz := exposureAuthz(tenant, comp, profile, forward)
			if err := r.ensureZonePolicy(ctx, comp, zone, oidcRoutes, authz); err != nil {
				return ctrl.Result{}, fmt.Errorf("zone policy: %w", err)
			}
		}
	}
	if !releaseReady {
		return r.status(ctx, comp, metav1.ConditionFalse, "Installing", releaseMessage, componentRequeue)
	}
	logger.V(1).Info("component reconciled", "component", comp.Name, "namespace", comp.Namespace, "exposures", exposed)
	// The perimeter: a surface the profile declared and a perimeter approver
	// enabled, published from the tenant's DMZ with no session in front of it
	// (AD-6). Declaring one publishes nothing; this only acts on enablements.
	published, err := r.ensurePerimeter(ctx, comp, profile, tenant, zone)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("perimeter: %w", err)
	}

	// What Ready MEANS differs by package, so the message says which it is
	// rather than claiming a release for a component that deploys none.
	installed := "release deployed"
	if releaseMessage != "" && profile.Spec.Package.Chart == nil {
		installed = releaseMessage
	}
	message := fmt.Sprintf("%s; %d gateway exposure(s) routed in zone %s", installed, exposed, zone.domain)
	if published > 0 {
		// Said out loud, because a public surface is the one thing about a
		// component that somebody should never discover by accident.
		message += fmt.Sprintf("; %d published on the perimeter", published)
	}
	return r.status(ctx, comp, metav1.ConditionTrue, "Ready", message, 0)
}

// addonBaseReady reports whether the component this addon activates into is
// installed and Ready.
//
// An addon deploys nothing, so "is it working" is a question about something
// else: the base. Until the base is there, an addon that reported Ready would
// be advertising a tile pointing at a host nobody serves.
//
// The base is looked for in this component's own namespace, which is the
// tenant's. An addon cannot activate into another tenant's install, and not
// looking outside the namespace is what makes that true rather than intended.
func (r *ComponentReconciler) addonBaseReady(
	ctx context.Context, comp *gentianov1alpha1.Component, profile *gentianov1alpha1.ComponentProfile,
) (bool, string, error) {
	base := profile.Spec.Package.Addon.Of
	if base == "" {
		return false, "package.addon names no base to activate into", nil
	}
	installed := &gentianov1alpha1.Component{}
	err := r.Get(ctx, types.NamespacedName{Name: base, Namespace: comp.Namespace}, installed)
	if errors.IsNotFound(err) {
		return false, fmt.Sprintf("%s is not installed in this tenant, and this activates inside it", base), nil
	}
	if err != nil {
		return false, "", err
	}
	for i := range installed.Status.Conditions {
		c := &installed.Status.Conditions[i]
		if c.Type == conditionComponentReady && c.Status == metav1.ConditionTrue {
			return true, "activated inside " + base, nil
		}
	}
	return false, fmt.Sprintf("%s is installed but not ready yet", base), nil
}

func (r *ComponentReconciler) status(ctx context.Context, comp *gentianov1alpha1.Component, st metav1.ConditionStatus, reason, message string, requeue time.Duration) (ctrl.Result, error) {
	now := metav1.Now()
	updated := false
	for i, c := range comp.Status.Conditions {
		if c.Type == conditionComponentReady {
			if c.Status != st || c.Reason != reason || c.Message != message {
				comp.Status.Conditions[i] = metav1.Condition{Type: conditionComponentReady, Status: st, Reason: reason, Message: message, LastTransitionTime: now, ObservedGeneration: comp.Generation}
			}
			updated = true
		}
	}
	if !updated {
		comp.Status.Conditions = append(comp.Status.Conditions, metav1.Condition{Type: conditionComponentReady, Status: st, Reason: reason, Message: message, LastTransitionTime: now, ObservedGeneration: comp.Generation})
	}
	if comp.Status.Fulfilment == "" {
		comp.Status.Fulfilment = "dedicated"
	}
	if err := r.Status().Update(ctx, comp); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if requeue > 0 {
		return ctrl.Result{RequeueAfter: requeue}, nil
	}
	return ctrl.Result{}, nil
}

// tenantOf finds the Tenant whose namespace this is.
func (r *ComponentReconciler) tenantOf(ctx context.Context, namespace string) (*gentianov1alpha1.Tenant, error) {
	list := &gentianov1alpha1.TenantList{}
	if err := r.List(ctx, list); err != nil {
		return nil, err
	}
	for i := range list.Items {
		if list.Items[i].NamespaceName() == namespace {
			return &list.Items[i], nil
		}
	}
	return nil, nil
}

// edgeZone is the session a component's routes live under: one confidential
// client, one cookie, one realm (networking.md §4). The platform tenant's
// zone is the kernel's (AD-10); every other tenant's is its own.
type edgeZone struct {
	domain      string // hosts are <subDomain>.<domain>
	realm       string
	clientID    string
	secretName  string // in the edge namespace
	cookie      string
	idCookie    string
	sectionName string // the authenticated Gateway's listener
	kernel      bool
}

// routableExposures are the entries of a profile this component routes on the
// authenticated Gateway, and the ones it refuses to.
//
// A gateway entry is behind the zone's session or it is not routed. The
// policy that puts a session and the authorization question in front of a
// route is written for oidc entries only, so an entry with any other mode
// would be a route on the authenticated Gateway with nothing in front of it
// at all -- not the session, not even the bouncer. That includes an entry that
// pins its caller with `source`: the schema admits it, and nothing enforces a
// source yet, so routing it would publish it to everyone. What needs no
// session is a perimeter surface, enabled and published from the DMZ.
func routableExposures(comp *gentianov1alpha1.Component, profile *gentianov1alpha1.ComponentProfile) (routable, refused []*gentianov1alpha1.ExposureSpec) {
	for i := range profile.Spec.Expose {
		e := &profile.Spec.Expose[i]
		if e.Surface != gentianov1alpha1.SurfaceGateway {
			continue // perimeter surfaces are enabled per tenant (§5.1)
		}
		// An entry that routes into another component is not this one's to
		// route: the named component already serves that host.
		if e.Backend.Component != "" && e.Backend.Component != comp.Name {
			continue
		}
		if e.AuthMode != gentianov1alpha1.AuthModeOIDC {
			refused = append(refused, e)
			continue
		}
		routable = append(routable, e)
	}
	return routable, refused
}

func (r *ComponentReconciler) zoneOf(tenant *gentianov1alpha1.Tenant) edgeZone {
	if tenantAdoptsKernelRealm(tenant, r.KernelRealm) {
		return edgeZone{
			domain: r.KernelDomain, realm: r.kernelRealm(), clientID: edgeKernelClientID,
			secretName: edgeKernelSecretName, cookie: edgeKernelAccessTokenCookie, idCookie: edgeKernelIDTokenCookie,
			sectionName: wildcardListenerName, kernel: true,
		}
	}
	return edgeZone{
		domain:      tenant.EffectiveDomain(r.KernelDomain, r.TenancyMode),
		realm:       keycloakRealmName(tenant),
		clientID:    "gentian-edge-" + tenant.Name,
		secretName:  "edge-" + tenant.Name + "-oidc",
		cookie:      "gentian-" + tenant.Name + "-access",
		idCookie:    "gentian-" + tenant.Name + "-id",
		sectionName: tenantGatewayListenerName(tenant.Name),
	}
}

func tenantAdoptsKernelRealm(tenant *gentianov1alpha1.Tenant, kernelRealm string) bool {
	return kernelRealm != "" && keycloakRealmName(tenant) == kernelRealm
}

func (r *ComponentReconciler) kernelRealm() string {
	if r.KernelRealm == "" {
		return "kernel"
	}
	return r.KernelRealm
}

func (r *ComponentReconciler) zoneReady(ctx context.Context, zone edgeZone) (bool, error) {
	secret := &corev1.Secret{}
	err := r.Get(ctx, types.NamespacedName{Name: zone.secretName, Namespace: servicesNamespace}, secret)
	if errors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return len(secret.Data["client-secret"]) > 0, nil
}

// ensureRelease keeps the provider-helm Release the profile's chart becomes.
func (r *ComponentReconciler) ensureRelease(ctx context.Context, comp *gentianov1alpha1.Component, profile *gentianov1alpha1.ComponentProfile, values map[string]interface{}) (bool, string, error) {
	chart := profile.Spec.Package.Chart
	spec := map[string]interface{}{
		"rollbackLimit": int64(3),
		"forProvider": map[string]interface{}{
			"chart": map[string]interface{}{
				"repository": chart.Repository,
				"name":       chart.Name,
				"version":    chart.Version,
			},
			"namespace":   comp.Namespace,
			"wait":        true,
			"waitTimeout": "10m",
			"skipCRDs":    true,
			"values":      values,
		},
		"providerConfigRef": map[string]interface{}{"name": "kubernetes"},
	}
	desired := &unstructured.Unstructured{}
	desired.SetGroupVersionKind(helmReleaseGVK)
	desired.SetName(releaseName(comp))
	desired.SetLabels(componentLabels(comp))
	if err := unstructured.SetNestedField(desired.Object, spec, "spec"); err != nil {
		return false, "", err
	}
	existing := &unstructured.Unstructured{}
	existing.SetGroupVersionKind(helmReleaseGVK)
	err := r.Get(ctx, types.NamespacedName{Name: desired.GetName()}, existing)
	if errors.IsNotFound(err) {
		return false, "release created; waiting for the chart to deploy", r.Create(ctx, desired)
	}
	if err != nil {
		return false, "", err
	}
	// Only what this reconciler writes is compared: the provider fills the
	// rest of the spec (deletionPolicy, managementPolicies, rollbackLimit)
	// with defaults, and comparing the whole spec against a desired one
	// without them found drift on every pass and never let the component
	// be Ready.
	existingFor, _, _ := unstructured.NestedMap(existing.Object, "spec", "forProvider")
	existingRef, _, _ := unstructured.NestedMap(existing.Object, "spec", "providerConfigRef")
	if !equality.Semantic.DeepEqual(existingFor, spec["forProvider"]) || !equality.Semantic.DeepEqual(existingRef, spec["providerConfigRef"]) {
		patch := client.MergeFrom(existing.DeepCopy())
		if err := unstructured.SetNestedField(existing.Object, spec["forProvider"], "spec", "forProvider"); err != nil {
			return false, "", err
		}
		if err := unstructured.SetNestedField(existing.Object, spec["providerConfigRef"], "spec", "providerConfigRef"); err != nil {
			return false, "", err
		}
		if err := r.Patch(ctx, existing, patch); err != nil {
			return false, "", err
		}
		return false, "release updated; waiting for the chart to deploy", nil
	}
	if crossplaneObjectReady(existing) {
		return true, "", nil
	}
	return false, releaseMessageOf(existing), nil
}

// releaseName is deterministic per component and namespace: a Release is
// cluster-scoped, so it carries both.
func releaseName(comp *gentianov1alpha1.Component) string {
	return comp.Namespace + "-" + comp.Name
}

func releaseMessageOf(obj *unstructured.Unstructured) string {
	conds, _, _ := unstructured.NestedSlice(obj.Object, "status", "conditions")
	for _, c := range conds {
		m, _ := c.(map[string]interface{})
		if m["type"] == "Ready" || m["type"] == "Synced" {
			if msg, _ := m["message"].(string); msg != "" && m["status"] != "True" {
				return fmt.Sprintf("%s: %s", m["type"], msg)
			}
		}
	}
	return "waiting for the release to be ready"
}

func componentLabels(comp *gentianov1alpha1.Component) map[string]string {
	return map[string]string{
		managedByLabel: managedByValue,
		componentLabel: comp.Name,
		tenantLabel:    strings.TrimPrefix(comp.Namespace, "tenant-"),
	}
}

// ensureExposureRoute keeps one gateway exposure's route.
func (r *ComponentReconciler) ensureExposureRoute(ctx context.Context, comp *gentianov1alpha1.Component, profile *gentianov1alpha1.ComponentProfile, tenant *gentianov1alpha1.Tenant, zone edgeZone, e *gentianov1alpha1.ExposureSpec) (string, error) {
	host := exposureHost(zone, comp, e)
	routeName := comp.Name + "-" + e.Name
	route := buildExposureRoute(comp, routeName, host, zone, e, exposureAuthz(tenant, comp, profile, e.ForwardToken), r.KernelDomain)
	if err := controllerutil.SetControllerReference(comp, route, r.Scheme); err != nil {
		return "", err
	}
	return routeName, ensureHTTPRouteResource(ctx, r.Client, route)
}

// ensureZonePolicy keeps the component's one SecurityPolicy: the zone's
// session and the bouncer, over every oidc route the component has.
func (r *ComponentReconciler) ensureZonePolicy(ctx context.Context, comp *gentianov1alpha1.Component, zone edgeZone, routes []string, authz routeAuthz) error {
	spec := zoneSecurityPolicySpec(r.KernelDomain, zone, routes[0], authz, servicesNamespace, r.bouncerService())
	targets := make([]interface{}, 0, len(routes))
	for _, name := range routes {
		targets = append(targets, map[string]interface{}{"group": gatewayv1.GroupName, "kind": "HTTPRoute", "name": name})
	}
	spec["targetRefs"] = targets
	policy := &unstructured.Unstructured{}
	policy.SetGroupVersionKind(securityPolicyGVK)
	policy.SetName("sp-" + comp.Name)
	policy.SetNamespace(comp.Namespace)
	policy.SetLabels(componentLabels(comp))
	if err := unstructured.SetNestedField(policy.Object, spec, "spec"); err != nil {
		return err
	}
	if err := controllerutil.SetControllerReference(comp, policy, r.Scheme); err != nil {
		return err
	}
	// One policy per component: any other policy of this component's is a
	// session of its own, and goes.
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(schema.GroupVersionKind{Group: securityPolicyGVK.Group, Version: securityPolicyGVK.Version, Kind: "SecurityPolicyList"})
	if err := r.List(ctx, list, client.InNamespace(comp.Namespace), client.MatchingLabels{componentLabel: comp.Name, managedByLabel: managedByValue}); err != nil {
		return err
	}
	for i := range list.Items {
		if list.Items[i].GetName() == policy.GetName() {
			continue
		}
		if err := r.Delete(ctx, &list.Items[i]); client.IgnoreNotFound(err) != nil {
			return err
		}
	}
	existing := &unstructured.Unstructured{}
	existing.SetGroupVersionKind(securityPolicyGVK)
	err := r.Get(ctx, client.ObjectKey{Name: policy.GetName(), Namespace: comp.Namespace}, existing)
	if errors.IsNotFound(err) {
		return r.Create(ctx, policy)
	}
	if err != nil {
		return err
	}
	if !equality.Semantic.DeepEqual(existing.Object["spec"], policy.Object["spec"]) {
		patch := client.MergeFrom(existing.DeepCopy())
		if err := unstructured.SetNestedField(existing.Object, spec, "spec"); err != nil {
			return err
		}
		return r.Patch(ctx, existing, patch)
	}
	return nil
}

// exposureHost is <subDomain>.<zone>, the component's own name when the entry
// names no subDomain. A desktop's console entry in the kernel zone is
// console.<kernel> (networking.md §3).
func exposureHost(zone edgeZone, comp *gentianov1alpha1.Component, e *gentianov1alpha1.ExposureSpec) string {
	return exposureHostIn(zone.domain, zone.kernel, comp.Name, e)
}

// exposureHostIn is where an entry answers: a label under the zone's domain,
// the entry's own or the component's name.
//
// An apex entry answers on the bare domain, and only in the kernel zone. It
// has no host anywhere else, which is the empty string here and "not
// published" to every caller: a tenant's bare domain is its own to route.
func exposureHostIn(domain string, kernelZone bool, component string, e *gentianov1alpha1.ExposureSpec) string {
	if domain == "" {
		return ""
	}
	if e.Apex {
		if !kernelZone {
			return ""
		}
		return domain
	}
	sub := e.SubDomain
	if sub == "" {
		sub = component
	}
	return sub + "." + domain
}

// exposureAuthz is the L2 question a component's routes ask (networking.md
// §3): whoever may enter the tenant, for what the tenant itself is entered
// through; whoever may use the app, for an app.
//
// Which it is follows from what the profile says about itself, not from its
// name. A component that is the launcher (launch: none) or whose tile is held
// on the tenant -- the desktop, the consoles -- is part of the tenant, and
// entering the tenant is what reaches it. Everything else is an app, and the
// route asks the same relation on the same object its tile does
// (app:<tenant>/<profile>, can_use), so the route cannot be more open than
// the tile: knowing an app's hostname is not a way in.
func exposureAuthz(tenant *gentianov1alpha1.Tenant, comp *gentianov1alpha1.Component, profile *gentianov1alpha1.ComponentProfile, forwardToken bool) routeAuthz {
	if tenantScoped(profile) {
		return routeAuthz{relation: "can_enter", object: "tenant:" + tenant.Name, forwardToken: forwardToken}
	}
	return routeAuthz{relation: "can_use", object: "app:" + tenant.Name + "/" + comp.Spec.ProfileRef.Name, forwardToken: forwardToken}
}

// tenantScoped reports whether a profile is part of the tenant itself rather
// than an app inside it.
func tenantScoped(profile *gentianov1alpha1.ComponentProfile) bool {
	if profile.Spec.Launch == gentianov1alpha1.ComponentLaunchNone {
		return true
	}
	for i := range profile.Spec.Expose {
		if t := profile.Spec.Expose[i].Tile; t != nil && t.Object == gentianov1alpha1.TileObjectTenant {
			return true
		}
	}
	return false
}

// buildExposureRoute is one exposure as an HTTPRoute on the zone's Gateway.
//
// Every rule carries the same frame policy the kernel consoles carry: this
// component may be embedded by a page on the kernel domain, which is where the
// desktop lives, and by nothing else. Without it a component is embeddable by
// any origin, which is the clickjacking exposure the kernel routes closed, and
// the desktop opens components in frames, so the policy has to admit exactly
// that and no more. It is not the component's to choose: a component that
// answered X-Frame-Options: DENY would silently break its own tile.
func buildExposureRoute(comp *gentianov1alpha1.Component, name, host string, zone edgeZone, e *gentianov1alpha1.ExposureSpec, authz routeAuthz, kernelDomain string) *gatewayv1.HTTPRoute {
	parent := gatewayParentRef(AuthenticatedGatewayName)
	ns := gatewayv1.Namespace(servicesNamespace)
	parent.Namespace = &ns
	section := gatewayv1.SectionName(zone.sectionName)
	parent.SectionName = &section
	paths := e.Paths
	if len(paths) == 0 {
		paths = []string{"/"}
	}
	var rules []gatewayv1.HTTPRouteRule
	wholeHost := false
	frame := kernelConsoleFrameFilters(kernelDomain)
	for _, p := range paths {
		rules = append(rules, kernelBackendRulePrefixNS(e.Backend.Service, comp.Namespace, e.Backend.Port, p, frame...))
		wholeHost = wholeHost || p == "/"
	}
	// A route behind a session must carry the path the code flow lands on.
	if e.AuthMode == gentianov1alpha1.AuthModeOIDC && !wholeHost {
		rules = append(rules, kernelBackendRulePrefixNS(e.Backend.Service, comp.Namespace, e.Backend.Port, edgeOAuth2Prefix, frame...))
	}
	labels := componentLabels(comp)
	labels[bouncerRouteLabel] = "true"
	mode := string(e.AuthMode)
	if e.AuthMode == gentianov1alpha1.AuthModeJWT || e.AuthMode == gentianov1alpha1.AuthModeBearer {
		mode = "bearer"
	}
	annotations := map[string]string{
		bouncerRelationAnnotation: authz.relation,
		bouncerObjectAnnotation:   authz.object,
		bouncerForwardAnnotation:  fmt.Sprint(authz.forwardToken),
		bouncerCookieAnnotation:   zone.cookie,
		bouncerAuthModeAnnotation: mode,
		// What sign-out needs: the zone's id token, which is the hint the
		// realm ends a session on without asking, and where that realm ends
		// one. Without them the edge drops its own cookies, the realm's
		// session stands, and the next request is signed straight back in --
		// a sign-out that reloads the page.
		bouncerIDCookieAnnotation:   zone.idCookie,
		bouncerEndSessionAnnotation: endSessionURL(kernelDomain, zone.realm),
	}
	// denyPaths is not a route rule. A gateway route matches by prefix, so
	// the denied path is already inside the rule that serves the host, and
	// the more specific rule that would shadow it still needs a backend to
	// send the request to. It is refused at L2 instead, which is the one
	// place that sees every request to this host.
	if len(e.DenyPaths) > 0 {
		annotations[bouncerDenyPathsAnnotation] = strings.Join(e.DenyPaths, ",")
	}
	return &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   comp.Namespace,
			Labels:      labels,
			Annotations: annotations,
		},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{ParentRefs: []gatewayv1.ParentReference{parent}},
			Hostnames:       []gatewayv1.Hostname{gatewayv1.Hostname(host)},
			Rules:           rules,
		},
	}
}

// zoneGrantName is the ReferenceGrant in the edge namespace that lets a
// component namespace's SecurityPolicies name the zone's client Secret and
// the bouncer's Service.
func zoneGrantName(namespace string) string { return "allow-zone-policy-" + namespace }

func (r *ComponentReconciler) ensureZoneGrant(ctx context.Context, comp *gentianov1alpha1.Component) error {
	spec := map[string]interface{}{
		"from": []interface{}{
			map[string]interface{}{"group": "gateway.envoyproxy.io", "kind": "SecurityPolicy", "namespace": comp.Namespace},
		},
		// The bouncer's Service only: the zone's client secret is copied beside
		// the policy, because Envoy Gateway reads it from no other namespace.
		"to": []interface{}{
			map[string]interface{}{"group": "", "kind": "Service"},
		},
	}
	desired := buildReferenceGrantObject(servicesNamespace, zoneGrantName(comp.Namespace), spec, map[string]string{
		managedByLabel: managedByValue,
		tenantLabel:    strings.TrimPrefix(comp.Namespace, "tenant-"),
	})
	existing := &unstructured.Unstructured{}
	existing.SetGroupVersionKind(referenceGrantGVK)
	err := r.Get(ctx, client.ObjectKey{Name: desired.GetName(), Namespace: servicesNamespace}, existing)
	if errors.IsNotFound(err) {
		return r.Create(ctx, desired)
	}
	if err != nil {
		return err
	}
	if !equality.Semantic.DeepEqual(existing.Object["spec"], desired.Object["spec"]) {
		patch := client.MergeFrom(existing.DeepCopy())
		if err := unstructured.SetNestedField(existing.Object, spec, "spec"); err != nil {
			return err
		}
		return r.Patch(ctx, existing, patch)
	}
	return nil
}

// ensureZoneSecret keeps a copy of the zone's edge client secret in the
// component's namespace, under the same name the policy uses. The zone is
// the tenant's own (the kernel's for the platform tenant), so its secret in
// the tenant's namespace crosses no trust boundary.
func (r *ComponentReconciler) ensureZoneSecret(ctx context.Context, comp *gentianov1alpha1.Component, zone edgeZone) error {
	source := &corev1.Secret{}
	if err := r.Get(ctx, types.NamespacedName{Name: zone.secretName, Namespace: servicesNamespace}, source); err != nil {
		return err
	}
	desired := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      zone.secretName,
			Namespace: comp.Namespace,
			Labels: map[string]string{
				managedByLabel: managedByValue,
				tenantLabel:    strings.TrimPrefix(comp.Namespace, "tenant-"),
			},
		},
		Type: source.Type,
		Data: source.Data,
	}
	existing := &corev1.Secret{}
	err := r.Get(ctx, types.NamespacedName{Name: desired.Name, Namespace: desired.Namespace}, existing)
	if errors.IsNotFound(err) {
		return r.Create(ctx, desired)
	}
	if err != nil {
		return err
	}
	if equality.Semantic.DeepEqual(existing.Data, desired.Data) && equality.Semantic.DeepEqual(existing.Labels, desired.Labels) {
		return nil
	}
	patch := client.MergeFrom(existing.DeepCopy())
	existing.Data = desired.Data
	existing.Labels = desired.Labels
	return r.Patch(ctx, existing, patch)
}

func (r *ComponentReconciler) deleteZoneGrant(ctx context.Context, comp *gentianov1alpha1.Component) error {
	// The grant is per namespace; another component in it may still need it.
	list := &gentianov1alpha1.ComponentList{}
	if err := r.List(ctx, list, client.InNamespace(comp.Namespace)); err != nil {
		return err
	}
	for i := range list.Items {
		if list.Items[i].Name != comp.Name && list.Items[i].DeletionTimestamp.IsZero() {
			return nil
		}
	}
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(referenceGrantGVK)
	obj.SetName(zoneGrantName(comp.Namespace))
	obj.SetNamespace(servicesNamespace)
	return client.IgnoreNotFound(r.Delete(ctx, obj))
}

func (r *ComponentReconciler) bouncerService() string {
	if r.BouncerService == "" {
		return "gentian-os-bouncer"
	}
	return r.BouncerService
}

func (r *ComponentReconciler) directorURL() string {
	if r.DirectorURL != "" {
		return r.DirectorURL
	}
	return fmt.Sprintf("http://gentian-os-director.%s.svc.cluster.local:8080", layout.Namespace(layout.Control))
}

// usherURL is where the usher answers inside the cluster: the Service the
// operator's chart puts in the edge namespace (templates/usher.yaml).
func (r *ComponentReconciler) usherURL() string {
	if r.UsherURL != "" {
		return r.UsherURL
	}
	return fmt.Sprintf("http://gentian-os-usher.%s.svc.cluster.local:8080", layout.Namespace(layout.Edge))
}

// credentialManagerPort is charts/gentian-os/values.yaml's
// credentialManager.port. Named rather than repeated, because a component
// told the wrong port fails at the first credential write with a connection
// refused that names nothing.
const credentialManagerPort = 9444

// credentialManagerURL is where a component relays a person's credential
// writes. In the control namespace beside the director, and for the same
// reason: it holds the OpenBao connection and no authority of its own.
func (r *ComponentReconciler) credentialManagerURL() string {
	if r.CredentialManagerURL != "" {
		return r.CredentialManagerURL
	}
	return fmt.Sprintf("http://gentian-os-credentials.%s.svc.cluster.local:%d",
		layout.Namespace(layout.Control), credentialManagerPort)
}

func mergeValues(dst, src map[string]interface{}) {
	for k, v := range src {
		if sub, ok := v.(map[string]interface{}); ok {
			if cur, ok := dst[k].(map[string]interface{}); ok {
				mergeValues(cur, sub)
				continue
			}
		}
		dst[k] = v
	}
}

// SetupWithManager registers the controller. A Release, a route or a policy
// changing under a component re-runs it; so does the zone's secret arriving.
func (r *ComponentReconciler) SetupWithManager(mgr ctrl.Manager) error {
	release := &unstructured.Unstructured{}
	release.SetGroupVersionKind(helmReleaseGVK)
	return ctrl.NewControllerManagedBy(mgr).
		Named("component").
		For(&gentianov1alpha1.Component{}).
		Owns(&gatewayv1.HTTPRoute{}).
		Watches(release, componentOfRelease()).
		Watches(&gentianov1alpha1.ComponentProfile{}, componentsOfProfile(mgr.GetClient())).
		Watches(&corev1.Secret{}, componentsOfZoneSecret(mgr.GetClient())).
		Complete(r)
}
