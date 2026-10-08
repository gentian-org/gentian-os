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
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/kernel/secrets"
	"github.com/gentian-org/gentian-os/internal/layout"
	"github.com/gentian-org/gentian-os/internal/profilebundle"
	"github.com/gentian-org/gentian-os/internal/schemacheck/crdcheck"
	"github.com/gentian-org/gentian-os/internal/security"
)

// ComponentReconciler turns a Component into what runs: the chart its profile
// names as a provider-helm Release in the component's namespace, the
// requirements its profile declares fulfilled beside it, and one route and
// one policy per gateway exposure in the tenant's zone. It writes nothing
// outside the component's namespace except the ReferenceGrant the zone's
// policy needs in the edge namespace.
type ComponentReconciler struct {
	// Definitions holds this reconciler while the cluster's resource
	// definitions would drop fields it writes (internal/schemacheck). Nil
	// holds nothing.
	Definitions *crdcheck.Holder
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
	// CustodianURL overrides where a component is told the credential
	// manager is. Empty derives it from the layout.
	CustodianURL string
	// RegistrarURL overrides where a component is told the registrar is.
	// Empty derives it from the layout.
	RegistrarURL string
	// Seeder holds the credentials of the databases this reconciler makes
	// for tenant components. Nil leaves those requirements waiting.
	Seeder *secrets.Seeder
	// Recorder writes the events a person should see on a Component without
	// reading its conditions. Nil writes none.
	Recorder record.EventRecorder
	// LicenceReporting is whether this cluster reports what it runs: half of
	// whether it offers an App Store, which decides what a component that
	// asked where the store is gets told and may reach.
	LicenceReporting bool
	// WatchClusterClaim re-runs the components told where the store is when
	// the Cluster claim changes. Off where the claim's kind is not installed.
	WatchClusterClaim bool
}

// The markers are a free-floating block: controller-gen ignores a block that
// is part of a declaration's doc comment.
//
// +kubebuilder:rbac:groups=gentianos.io,resources=components,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=gentianos.io,resources=components/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=gentianos.io,resources=components/finalizers,verbs=update
// +kubebuilder:rbac:groups=gentianos.io,resources=componentprofiles,verbs=get;list;watch
// What a profile's bundle may bring beside it, read one at a time by name to
// compare with the bundle before a pinned install is rolled out
// (internal/profilebundle). The other three kinds are read elsewhere already.
// +kubebuilder:rbac:groups=apiextensions.crossplane.io,resources=compositions,verbs=get
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
	bouncerAuthModeAnnotation = "gentianos.io/bouncer-mode"
	// bouncerDenyPathsAnnotation carries the exposure's denyPaths to the
	// bouncer's table. Comma-separated because an annotation is a string and a
	// path cannot contain a comma without being escaped, which none are.
	bouncerDenyPathsAnnotation = "gentianos.io/bouncer-deny-paths"
	// bouncerSessionCookiesAnnotation names the zone's two token cookies on
	// a route behind a session, comma-separated, for the bouncer's table:
	// the bouncer takes them out of the request before the backend sees it.
	bouncerSessionCookiesAnnotation = "gentianos.io/bouncer-session-cookies"
	componentDatabaseSecretSuffix   = "-database"
)

func (r *ComponentReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	comp := &gentianov1alpha1.Component{}
	if err := r.Get(ctx, req.NamespacedName, comp); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !comp.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(comp, componentFinalizer) {
			// The release first, and the component stays until it is gone:
			// "the component is gone" is what a purge waits for before it
			// drops a database, and it must not be true while the chart's
			// pods are still running.
			gone, err := r.deleteDirectRelease(ctx, comp)
			if err != nil {
				return ctrl.Result{}, err
			}
			if !gone {
				return ctrl.Result{RequeueAfter: componentRequeue}, nil
			}
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
			// Said with what to do: no profile is on a cluster ahead of
			// an install, so this is an app named without the build to
			// fetch -- by hand in the manifest, by an import, or left
			// from when profiles were copied in wholesale.
			return r.status(ctx, comp, metav1.ConditionFalse, "ProfileMissing", fmt.Sprintf(
				"ComponentProfile %q is not on this cluster, so there is nothing to install it from: "+
					"install it with its coordinate and digest so that the profile is fetched "+
					"(kubectl gentian apps install %s --tenant <tenant>)",
				comp.Spec.ProfileRef.Name, comp.Spec.ProfileRef.Name), componentRequeue)
		}
		return ctrl.Result{}, err
	}
	if comp.Spec.Class != gentianov1alpha1.ComponentClassApp {
		return r.status(ctx, comp, metav1.ConditionFalse, "ClassUnsupported",
			fmt.Sprintf("class %q is not reconciled yet; only apps are", comp.Spec.Class), 0)
	}
	// An install pinned to a digest rolls out that build and no other. The
	// director checked the bytes it fetched before it committed them; this is
	// the check that what the cluster now holds under that name is still
	// those bytes' profile -- which nothing between the commit and here
	// guarantees: the file can be edited in git, the object in the cluster,
	// and another tenant's install of the same entry at another digest
	// replaces the one profile both of them name.
	//
	// Before anything else is read or written: no privilege is asked for, no
	// network policy, release, App claim or route is rendered from a profile
	// that is not the one installed. And nothing is taken away either. What
	// an earlier pass rolled out from the right profile stays as it is, held
	// at that state, until the profile is the pinned build again or the pin
	// is moved.
	if digest := comp.Spec.ProfileRef.Digest; digest != "" {
		//
		// The pin is to the whole bundle: the profile, and every object the
		// bundle brings beside it. Each of those is read from the cluster
		// and compared with the bundle too, and one that is missing or is
		// not what the bundle says holds the rollout the same way.
		refusal, err := profilebundle.VerifyOnCluster(ctx, r.Client, catalogueNamespace(), profile, digest)
		if err != nil {
			return ctrl.Result{}, err
		}
		if refusal != nil {
			message := refusal.Message + "; nothing is rolled out from it, and what is running is left as it is"
			if r.Recorder != nil && !componentReports(comp, refusal.Reason, message) {
				r.Recorder.Event(comp, corev1.EventTypeWarning, refusal.Reason, message)
			}
			logger.Info("component held: its profile is not the build the install is pinned to",
				"component", comp.Name, "namespace", comp.Namespace, "reason", refusal.Reason, "detail", refusal.Message)
			// No requeue for the profile: a change to it or to the pin
			// re-runs this. A companion is not watched, and one Argo CD
			// has yet to apply arrives without either changing.
			return r.status(ctx, comp, metav1.ConditionFalse, refusal.Reason, message, heldRequeue(refusal))
		}
	}
	// The same for every addon this instance activates at a pinned build.
	//
	// An addon's own Component deploys nothing, so holding it would hold
	// nothing: the addon takes effect in THIS component's release, through
	// the list this component hands on. The check therefore sits here, on
	// the base, and at the same point as the base's own -- before anything
	// is rendered -- so an addon whose profile is not the pinned build never
	// reaches the release values, by any path through what follows.
	//
	// The base is held whole rather than rolled out without the addon. The
	// list reconciles: a release rendered without an addon that is already
	// active switches it off, and "nothing already rolled out is removed"
	// is the promise a pin makes.
	refusal, err := r.unverifiedAddon(ctx, comp)
	if err != nil {
		return ctrl.Result{}, err
	}
	if refusal != nil {
		message := refusal.Message + "; nothing is rolled out for " + comp.Name + " while it is activated, and what is running is left as it is"
		if r.Recorder != nil && !componentReports(comp, refusal.Reason, message) {
			r.Recorder.Event(comp, corev1.EventTypeWarning, refusal.Reason, message)
		}
		logger.Info("component held: an addon's profile is not the build it is pinned to",
			"component", comp.Name, "namespace", comp.Namespace, "reason", refusal.Reason, "detail", refusal.Message)
		// No requeue for the profile: a change to the addon's profile or to
		// the pin re-runs this. A companion is looked for again.
		return r.status(ctx, comp, metav1.ConditionFalse, refusal.Reason, message, heldRequeue(refusal))
	}
	tenant, err := r.tenantOf(ctx, comp.Namespace)
	if err != nil {
		return ctrl.Result{}, err
	}
	if tenant == nil {
		return r.status(ctx, comp, metav1.ConditionFalse, "NoTenant",
			fmt.Sprintf("namespace %q belongs to no Tenant", comp.Namespace), componentRequeue)
	}
	// Whose the profile is, and whether every add-on has one.
	//
	// A ComponentProfile is cluster-scoped, and a catalogue need not be: a
	// tenant may have one only it sees, and what was materialised from it
	// says so (profilebundle.OriginAnnotation). The director refuses to
	// install such a profile anywhere else, but the object is there for the
	// whole cluster and a Component can be written naming it by other means,
	// so it is refused here as well -- at the same point as the digest, before
	// anything is rendered, and leaving what is running as it is.
	//
	// An add-on switched on by name whose profile is not on the cluster is
	// held the same way. Profiles are not on a cluster ahead of an install,
	// and an add-on that resolves to nothing would otherwise be dropped from
	// the release without a word: selected on every screen, active nowhere.
	if refusal, err := r.unusableProfile(ctx, comp, profile, tenant.Name); err != nil {
		return ctrl.Result{}, err
	} else if refusal != nil {
		message := refusal.Message + "; nothing is rolled out for " + comp.Name + ", and what is running is left as it is"
		if r.Recorder != nil && !componentReports(comp, refusal.Reason, message) {
			r.Recorder.Event(comp, corev1.EventTypeWarning, refusal.Reason, message)
		}
		logger.Info("component held: a profile it needs is not one this tenant can use",
			"component", comp.Name, "namespace", comp.Namespace, "reason", refusal.Reason, "detail", refusal.Message)
		// Requeued: an add-on's profile arriving is not an event on this
		// Component.
		return r.status(ctx, comp, metav1.ConditionFalse, refusal.Reason, message, componentRequeue)
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
	// A host that is the kernel's own, asked for by the user tenant of a
	// single-tenancy cluster: refused here, before anything is written.
	if refusal := reservedHostRefusal(comp, profile, zone.zoneNames, r.KernelDomain); refusal != "" {
		if r.Recorder != nil && !componentReports(comp, "HostReserved", refusal) {
			r.Recorder.Event(comp, corev1.EventTypeWarning, "HostReserved", refusal)
		}
		return r.status(ctx, comp, metav1.ConditionFalse, "HostReserved", refusal, 0)
	}
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
	// The model gateway, for a component that declares it: nothing is
	// installed while the cluster has none, or before the component's key is
	// registered there and delivered. Asked for every delivery, because the
	// requirement is met by the tenant's reconciler either way.
	if reason, message, err := modelAccessHold(ctx, r.Client, comp, profile, tenant); err != nil {
		return ctrl.Result{}, err
	} else if reason != "" {
		return r.status(ctx, comp, metav1.ConditionFalse, reason, message, componentRequeue)
	}
	// What the platform tells any component about itself, where its profile
	// says its chart takes it. Nothing is keyed on which component this is.
	mergeValues(values, r.platformValues(profile, tenant, zone))
	// And the two that are this component's own: the host it answers on and
	// where the store is, for a profile that asked for either.
	placed, err := r.placementValues(ctx, comp, profile, zone)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("read where the App Store is: %w", err)
	}
	mergeValues(values, placed)
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
	// What the tenant declared it installs from, and so which of its pull
	// credentials this component's chart and pods are told about. Names
	// only: the Secrets are the repository Composition's, in this namespace.
	var pull pullSecrets
	if profile.Spec.Package.Chart != nil {
		repos, err := r.tenantPullRepositories(ctx, tenant.Name)
		if err != nil {
			return ctrl.Result{}, err
		}
		var refusal string
		if pull, refusal = resolvePullSecrets(profile, repos); refusal != "" {
			return r.status(ctx, comp, metav1.ConditionFalse, "RepositoryAmbiguous", refusal, componentRequeue)
		}
	}
	switch {
	case composed:
		var err error
		releaseReady, releaseMessage, err = r.ensureAppClaim(ctx, comp, tenant, zone, pull, appComposition(comp, profile))
		if err != nil {
			return ctrl.Result{}, err
		}
		if !releaseReady {
			releaseMessage = pull.withPullHint(releaseMessage, profile)
		}

	case profile.Spec.Package.Chart != nil:
		var err error
		valuesWithPullSecrets(values, profile, pull.images)
		releaseReady, releaseMessage, err = r.ensureRelease(ctx, comp, profile, values, pull)
		if err != nil {
			return ctrl.Result{}, err
		}
		if !releaseReady {
			releaseMessage = pull.withPullHint(releaseMessage, profile)
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

// unverifiedAddon answers the first addon this component activates at a
// pinned build whose profile is not shown to be that build, or nil when
// every pinned addon is. An addon with no pin is not checked, like an app
// installed with no digest.
func (r *ComponentReconciler) unverifiedAddon(ctx context.Context, comp *gentianov1alpha1.Component) (*profilebundle.Refusal, error) {
	for _, pin := range comp.Spec.AddonPins {
		if !slices.Contains(comp.Spec.Addons, pin.Name) {
			continue
		}
		addon := &gentianov1alpha1.ComponentProfile{}
		if err := r.Get(ctx, types.NamespacedName{Name: pin.Name}, addon); err != nil {
			if !errors.IsNotFound(err) {
				return nil, err
			}
			return &profilebundle.Refusal{Reason: profilebundle.ReasonUnverifiable, Message: fmt.Sprintf(
				"addon %s is pinned to %s and its ComponentProfile is not installed", pin.Name, profilebundle.Short(pin.Digest))}, nil
		}
		refusal, err := profilebundle.VerifyOnCluster(ctx, r.Client, catalogueNamespace(), addon, pin.Digest)
		if err != nil {
			return nil, err
		}
		if refusal != nil {
			return &profilebundle.Refusal{
				Reason: refusal.Reason, Message: "addon " + pin.Name + ": " + refusal.Message, Retry: refusal.Retry,
			}, nil
		}
	}
	return nil, nil
}

// catalogueNamespace is where the cluster's catalogue is applied, and so
// where the companions of a bundle that have a namespace are: the destination
// of the Application that syncs the catalogue directory
// (kernel/appsets/raw/11b-catalogue.yaml).
func catalogueNamespace() string { return layout.Namespace(layout.Provisioning) }

// heldRequeue is when a held component is looked at again: not at all for a
// refusal only a change to the profile or the pin can end, and soon for one
// that ends when Argo CD has applied what the bundle brings.
func heldRequeue(refusal *profilebundle.Refusal) time.Duration {
	if refusal.Retry {
		return componentRequeue
	}
	return 0
}

// reasonAddonProfileMissing is the condition reason of a Component that
// activates an add-on whose ComponentProfile is not on the cluster.
const reasonAddonProfileMissing = "AddonProfileMissing"

// unusableProfile answers why this component cannot be rolled out for its
// tenant from the profiles it names, or nil: its own profile or an add-on's
// belongs to another tenant, or an add-on has no profile on the cluster.
func (r *ComponentReconciler) unusableProfile(
	ctx context.Context, comp *gentianov1alpha1.Component, profile *gentianov1alpha1.ComponentProfile, tenant string,
) (*profilebundle.Refusal, error) {
	if refusal := profilebundle.OwnedByAnother(profile, tenant); refusal != nil {
		return refusal, nil
	}
	for _, name := range comp.Spec.Addons {
		addon := &gentianov1alpha1.ComponentProfile{}
		if err := r.Get(ctx, types.NamespacedName{Name: name}, addon); err != nil {
			if !errors.IsNotFound(err) {
				return nil, err
			}
			return &profilebundle.Refusal{Reason: reasonAddonProfileMissing, Message: fmt.Sprintf(
				"add-on %s is switched on and its ComponentProfile is not on this cluster, so it would activate nothing: "+
					"switch it on with its coordinate and digest (<catalogue>/%s and sha256:<hex>) so that it is fetched, or switch it off",
				name, name)}, nil
		}
		if refusal := profilebundle.OwnedByAnother(addon, tenant); refusal != nil {
			return &profilebundle.Refusal{Reason: refusal.Reason, Message: "add-on " + name + ": " + refusal.Message}, nil
		}
	}
	return nil, nil
}

// componentReports says whether the component's Ready condition already
// carries this reason and message, so that what is said once as an event is
// not said again on every pass.
func componentReports(comp *gentianov1alpha1.Component, reason, message string) bool {
	for i := range comp.Status.Conditions {
		c := &comp.Status.Conditions[i]
		if c.Type == conditionComponentReady {
			return c.Reason == reason && c.Message == message
		}
	}
	return false
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
	zoneNames
	realm      string
	clientID   string
	secretName string // in the edge namespace
	cookie     string
	idCookie   string
	// sectionName is the authenticated Gateway's listener for the zone's
	// hosts below its domain. A host directly under the cluster's domain is
	// on the catch-all listener instead: see listenerFor.
	sectionName string
}

// zoneNames is where a zone's hosts are: what the routes, the perimeter
// listeners and the redirect URIs all have to agree on, and so derived in one
// place (zoneNamesOf) from the tenant and the cluster's tenancy mode.
type zoneNames struct {
	// domain is what the zone's hosts are under: <subDomain>.<domain>.
	//   platform tenant            platform.<kernel>
	//   a tenant, tenancy multi    <tenant>.<kernel>, or its custom domain
	//   the user tenant, single    <kernel> itself
	domain string
	// kernel marks the platform tenant's zone, whose session is the kernel
	// realm's. Its desktop answers on the zone's domain itself --
	// platform.<kernel>, not console.platform.<kernel> -- and everything else
	// of it one label below, as admin.platform.<kernel>.
	kernel bool
	// apex is where an apex entry answers: the cluster's bare domain, for the
	// platform tenant, and nowhere for anybody else. Under either tenancy
	// mode: the concierge is published there on a single-tenancy cluster too,
	// because every desktop loads the cluster's brand from it, and the edge
	// sends the front page on to the user tenant's desktop
	// (kernelFrontDoor).
	apex string
}

// zoneNamesOf is where one tenant's hosts are, under this cluster's mode.
func zoneNamesOf(tenant *gentianov1alpha1.Tenant, kernelDomain, tenancyMode, kernelRealm string) zoneNames {
	names := zoneNames{domain: tenant.EffectiveDomain(kernelDomain, tenancyMode)}
	if tenantAdoptsKernelRealm(tenant, kernelRealm) {
		names.kernel = true
		names.apex = kernelDomain
	}
	return names
}

// platformDesktopHost is where the platform administrator's desktop answers,
// on every cluster: platform.<kernel>. The platform tenant's zone is that
// domain and its desktop is on the zone's own name.
func platformDesktopHost(kernelDomain string) string {
	return gentianov1alpha1.PlatformTenantName + "." + kernelDomain
}

// directlyUnder reports a host that is the domain or exactly one label below
// it: the names the cluster's own certificate covers and its catch-all
// listener serves.
func directlyUnder(host, domain string) bool {
	if domain == "" {
		return false
	}
	if host == domain {
		return true
	}
	label, ok := strings.CutSuffix(host, "."+domain)
	return ok && label != "" && !strings.Contains(label, ".")
}

// listenerFor is the authenticated Gateway's listener a host of this zone is
// served on, which a route has to name (gateway_platform_reconciler.go).
//
// A host directly under the cluster's domain is on the catch-all listener,
// whose certificate names the domain and one label below it. That is the
// platform's desktop, platform.<kernel>, and every host of the user tenant of
// a single-tenancy cluster. Anything deeper is on the zone's own listener,
// with the zone's own wildcard certificate: admin.platform.<kernel>, and
// every host of a tenant with a domain of its own.
func (z edgeZone) listenerFor(host, kernelDomain string) string {
	if directlyUnder(host, kernelDomain) {
		return wildcardListenerName
	}
	return z.sectionName
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
	names := zoneNamesOf(tenant, r.KernelDomain, r.TenancyMode, r.KernelRealm)
	section := tenantGatewayListenerName(tenant.Name)
	if servedByKernelEdge(names.domain, r.KernelDomain) {
		// The user tenant of a single-tenancy cluster: no listener of its
		// own, because *.<kernel> is the catch-all's.
		section = wildcardListenerName
	}
	if names.kernel {
		return edgeZone{
			zoneNames: names, realm: r.kernelRealm(), clientID: edgeKernelClientID,
			secretName: edgeKernelSecretName, cookie: edgeKernelAccessTokenCookie, idCookie: edgeKernelIDTokenCookie,
			sectionName: section,
		}
	}
	return edgeZone{
		zoneNames:   names,
		realm:       keycloakRealmName(tenant),
		clientID:    "gentian-edge-" + tenant.Name,
		secretName:  "edge-" + tenant.Name + "-oidc",
		cookie:      "gentian-" + tenant.Name + "-access",
		idCookie:    "gentian-" + tenant.Name + "-id",
		sectionName: section,
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
func (r *ComponentReconciler) ensureRelease(ctx context.Context, comp *gentianov1alpha1.Component, profile *gentianov1alpha1.ComponentProfile, values map[string]interface{}, pull pullSecrets) (bool, string, error) {
	chart := profile.Spec.Package.Chart
	chartSpec := map[string]interface{}{
		"repository": chart.Repository,
		"name":       chart.Name,
		"version":    chart.Version,
	}
	// A chart inside a repository the tenant declared is pulled with that
	// repository's credential, read by the provider from the component's own
	// namespace and from nowhere else.
	if c := pull.chart(chart.Repository); c != nil {
		chartSpec["pullSecretRef"] = map[string]interface{}{"name": c.secretName, "namespace": comp.Namespace}
	}
	// Uninstalling keeps the files: the rule the release is rendered with,
	// before the release that names it.
	if err := r.ensureKeepVolumes(ctx, comp); err != nil {
		return false, "", err
	}
	spec := map[string]interface{}{
		"rollbackLimit": int64(3),
		"forProvider": map[string]interface{}{
			"chart":       chartSpec,
			"namespace":   comp.Namespace,
			"wait":        true,
			"waitTimeout": "10m",
			"skipCRDs":    true,
			"values":      values,
			"patchesFrom": []interface{}{map[string]interface{}{
				"configMapKeyRef": map[string]interface{}{
					"name":      keepVolumesName(comp),
					"namespace": comp.Namespace,
					"key":       keepVolumesKey,
					// Not optional: a release that could not read the rule
					// would be installed without it, and uninstalling it
					// would delete files.
					"optional": false,
				},
			}},
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

// keepVolumesPatch marks every PersistentVolumeClaim a release renders for
// Helm to leave in place when the release is uninstalled.
//
// Uninstalling an app takes its workloads away and keeps its data; destroying
// the data is a purge, a separate act. A chart that templates a claim has it
// deleted by `helm uninstall` with everything else unless the claim carries
// helm.sh/resource-policy: keep, and no chart can be relied on to. So the
// provider applies this to what the chart rendered (forProvider.patchesFrom,
// a Kustomize patch run as Helm's post-renderer): the annotation is then in
// the manifest Helm records, which is what an uninstall reads.
//
// The target names no object and so matches every claim; the name inside the
// patch is required by the format and is not applied. The app Composition
// carries the same text for the releases it renders.
const keepVolumesPatch = `patches:
  - target:
      version: v1
      kind: PersistentVolumeClaim
    patch: |-
      apiVersion: v1
      kind: PersistentVolumeClaim
      metadata:
        name: every-claim-of-the-release
        annotations:
          helm.sh/resource-policy: keep
`

const keepVolumesKey = "patch.yaml"

func keepVolumesName(comp *gentianov1alpha1.Component) string {
	return comp.Name + "-keep-volumes"
}

// ensureKeepVolumes keeps the ConfigMap a component's release reads the keep
// rule from: in the component's namespace, owned by it.
func (r *ComponentReconciler) ensureKeepVolumes(ctx context.Context, comp *gentianov1alpha1.Component) error {
	desired := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      keepVolumesName(comp),
			Namespace: comp.Namespace,
			Labels:    componentLabels(comp),
		},
		Data: map[string]string{keepVolumesKey: keepVolumesPatch},
	}
	if err := controllerutil.SetControllerReference(comp, desired, r.Scheme); err != nil {
		return err
	}
	existing := &corev1.ConfigMap{}
	err := r.Get(ctx, types.NamespacedName{Name: desired.Name, Namespace: desired.Namespace}, existing)
	if errors.IsNotFound(err) {
		return r.Create(ctx, desired)
	}
	if err != nil {
		return err
	}
	if equality.Semantic.DeepEqual(existing.Data, desired.Data) && ownedBy(existing, comp) {
		return nil
	}
	patch := client.MergeFrom(existing.DeepCopy())
	existing.Data = desired.Data
	existing.Labels = desired.Labels
	existing.OwnerReferences = desired.OwnerReferences
	return r.Patch(ctx, existing, patch)
}

// deleteDirectRelease removes the Release this reconciler wrote for a
// component, and reports whether it is gone.
//
// A Release is cluster-scoped, so nothing deletes it with the component's
// namespace and it cannot be owned by a namespaced Component: left alone it
// outlived the component, and provider-helm kept the chart's workloads
// running in a tenant that had uninstalled them. A component delivered
// through the app Composition has no Release of this name; its claim is
// owned by the component and takes its own Release with it.
//
// Workloads only. What the release leaves behind is decided by the keep rule
// it was rendered with, and nothing here touches a volume, a database or a
// secret.
func (r *ComponentReconciler) deleteDirectRelease(ctx context.Context, comp *gentianov1alpha1.Component) (bool, error) {
	release := &unstructured.Unstructured{}
	release.SetGroupVersionKind(helmReleaseGVK)
	err := r.Get(ctx, types.NamespacedName{Name: releaseName(comp)}, release)
	if errors.IsNotFound(err) || apimeta.IsNoMatchError(err) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	// Only the one this reconciler made for this component. The name is
	// deterministic, but a Release is not namespaced and a name is a weak
	// reason to delete one.
	labels := release.GetLabels()
	if labels[componentLabel] != comp.Name || labels[tenantLabel] != componentLabels(comp)[tenantLabel] ||
		labels[managedByLabel] != managedByValue {
		return true, nil
	}
	if release.GetDeletionTimestamp().IsZero() {
		if err := r.Delete(ctx, release); err != nil && !errors.IsNotFound(err) {
			return false, err
		}
	}
	return false, nil
}

func releaseMessageOf(obj *unstructured.Unstructured) string {
	if msg := releaseFailureOf(obj); msg != "" {
		return msg
	}
	return "waiting for the release to be ready"
}

// releaseFailureOf is what the provider says is wrong with a release, or
// nothing when it has said nothing.
func releaseFailureOf(obj *unstructured.Unstructured) string {
	conds, _, _ := unstructured.NestedSlice(obj.Object, "status", "conditions")
	for _, c := range conds {
		m, _ := c.(map[string]interface{})
		if m["type"] == "Ready" || m["type"] == "Synced" {
			if msg, _ := m["message"].(string); msg != "" && m["status"] != "True" {
				return fmt.Sprintf("%s: %s", m["type"], msg)
			}
		}
	}
	return ""
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
	route := buildExposureRoute(comp, routeName, host, zone, e, exposureAuthz(tenant, comp, profile, e.ForwardToken), r.KernelDomain,
		componentFramers(zone, comp, profile, host))
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

// exposureHost is where one entry of a component answers in its zone.
func exposureHost(zone edgeZone, comp *gentianov1alpha1.Component, e *gentianov1alpha1.ExposureSpec) string {
	return exposureHostIn(zone.zoneNames, comp.Name, e)
}

// exposureHostIn is where an entry answers: a label under the zone's domain,
// the entry's own or the component's name.
//
// Two entries are not a label under the domain.
//
// The desktop's, in the platform tenant's zone, answers on the zone's domain
// itself: platform.<kernel> is the platform administrator's desktop, and
// console.<kernel> is not the platform's at all -- it is the user tenant's
// desktop on a single-tenancy cluster and an alias of the bare domain on a
// multi-tenancy one.
//
// An apex entry answers on the cluster's bare domain, and only where the zone
// has it (zoneNames.apex). It has no host anywhere else, which is the empty
// string here and "not published" to every caller: a tenant's bare domain is
// its own to route.
func exposureHostIn(zone zoneNames, component string, e *gentianov1alpha1.ExposureSpec) string {
	if zone.domain == "" {
		return ""
	}
	if e.Apex {
		return zone.apex
	}
	sub := e.SubDomain
	if sub == "" {
		sub = component
	}
	if zone.kernel && sub == consoleSubdomain {
		return zone.domain
	}
	return sub + "." + zone.domain
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
//
// One tenant-held tile is not entered by entering the tenant. A component
// whose tile asks can_install_app -- the App Store app -- is for the people
// who may install apps in the tenant and for nobody else, so its route asks
// what its tile asks: a member who knows the host is refused at the edge, not
// by the component's own courtesy. It is the one relation besides can_enter a
// tenant-held route asks; every other tenant-held tile (the consoles'
// can_administer) still shows to fewer people than its route admits, as
// before.
func exposureAuthz(tenant *gentianov1alpha1.Tenant, comp *gentianov1alpha1.Component, profile *gentianov1alpha1.ComponentProfile, forwardToken bool) routeAuthz {
	if tenantScoped(profile) {
		relation := "can_enter"
		if tenantTileAsks(profile, relationInstallApp) {
			relation = relationInstallApp
		}
		return routeAuthz{relation: relation, object: "tenant:" + tenant.Name, forwardToken: forwardToken}
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

// relationInstallApp is tenant#can_install_app: who may install apps in a
// tenant (authz/model/v1/model.fga).
const relationInstallApp = "can_install_app"

// tenantTileAsks reports whether a profile carries a tile held on the tenant
// that asks the given relation.
func tenantTileAsks(profile *gentianov1alpha1.ComponentProfile, relation string) bool {
	for i := range profile.Spec.Expose {
		t := profile.Spec.Expose[i].Tile
		if t != nil && t.Object == gentianov1alpha1.TileObjectTenant && t.Relation == relation {
			return true
		}
	}
	return false
}

// zoneDesktopHost is where a zone's desktop answers: the zone's own domain
// for the platform tenant, console.<domain> for every other.
func zoneDesktopHost(zone zoneNames) string {
	if zone.domain == "" {
		return ""
	}
	if zone.kernel {
		return zone.domain
	}
	return consoleHost(zone.domain)
}

// componentFramers are the hosts that may put one of a component's pages in a
// frame, besides the page's own: the desktop of the tenant the component
// belongs to, which opens it in a window, and the component's own other
// hosts, because a component may be several programs that embed one another
// (a file store and the document editor it opens).
//
// Named, each of them, and never as a wildcard over the zone's domain. The
// user tenant of a single-tenancy cluster is under the cluster's own domain,
// beside the platform's desktop and the kernel's consoles, so a wildcard
// there would name those too.
//
// The platform's desktop is not among a tenant's framers. It used to be, on
// the argument that its tiles open a tenant's apps; platform administrators
// do not open tenants' apps, and a desktop that may frame every tenant's
// every page is a place from which all of them can be overlaid. The platform
// tenant's own components are framed by the platform's desktop for the same
// reason any tenant's are framed by its own: it is their zone's desktop.
func componentFramers(zone edgeZone, comp *gentianov1alpha1.Component, profile *gentianov1alpha1.ComponentProfile, host string) []string {
	seen := map[string]bool{"": true, host: true}
	var framers []string
	add := func(h string) {
		if !seen[h] {
			seen[h] = true
			framers = append(framers, h)
		}
	}
	add(zoneDesktopHost(zone.zoneNames))
	var own []string
	if profile != nil {
		for i := range profile.Spec.Expose {
			e := &profile.Spec.Expose[i]
			if e.Surface != gentianov1alpha1.SurfaceGateway {
				continue
			}
			if e.Backend.Component != "" && e.Backend.Component != comp.Name {
				continue
			}
			own = append(own, exposureHost(zone, comp, e))
		}
	}
	sort.Strings(own)
	for _, h := range own {
		add(h)
	}
	return framers
}

// buildExposureRoute is one exposure as an HTTPRoute on the zone's Gateway.
//
// Every rule carries a frame policy: the component may be embedded by the
// hosts in framers -- its tenant's desktop and its own other hosts
// (componentFramers) -- and by nothing else. Without one a component is
// embeddable by any origin, which is the clickjacking exposure the kernel
// routes closed, and the desktop opens components in frames, so the policy
// has to admit exactly that and no more. It is not the component's to choose:
// a component that answered X-Frame-Options: DENY would silently break its
// own tile.
func buildExposureRoute(comp *gentianov1alpha1.Component, name, host string, zone edgeZone, e *gentianov1alpha1.ExposureSpec, authz routeAuthz, kernelDomain string, framers []string) *gatewayv1.HTTPRoute {
	parent := gatewayParentRef(AuthenticatedGatewayName)
	ns := gatewayv1.Namespace(servicesNamespace)
	parent.Namespace = &ns
	section := gatewayv1.SectionName(zone.listenerFor(host, kernelDomain))
	parent.SectionName = &section
	paths := e.Paths
	if len(paths) == 0 {
		paths = []string{"/"}
	}
	var rules []gatewayv1.HTTPRouteRule
	wholeHost := false
	frame := frameAncestorsFilters(framers)
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
		bouncerAuthModeAnnotation: mode,
	}
	// denyPaths is not a route rule. A gateway route matches by prefix, so
	// the denied path is already inside the rule that serves the host, and
	// the more specific rule that would shadow it still needs a backend to
	// send the request to. It is refused at L2 instead, which is the one
	// place that sees every request to this host.
	if len(e.DenyPaths) > 0 {
		annotations[bouncerDenyPathsAnnotation] = strings.Join(e.DenyPaths, ",")
	}
	// The session's cookies are the zone's, and only this reconciler knows
	// the zone. A bearer route has no session and names none.
	if cookies := zone.sessionCookies(); mode == "oidc" && len(cookies) > 0 {
		annotations[bouncerSessionCookiesAnnotation] = strings.Join(cookies, ",")
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
// operator's chart puts in the control namespace (templates/usher.yaml),
// beside the director's.
func (r *ComponentReconciler) usherURL() string {
	if r.UsherURL != "" {
		return r.UsherURL
	}
	return fmt.Sprintf("http://gentian-os-usher.%s.svc.cluster.local:8080", layout.Namespace(layout.Control))
}

// custodianPort is charts/gentian-os/values.yaml's
// custodian.port. Named rather than repeated, because a component
// told the wrong port fails at the first credential write with a connection
// refused that names nothing.
const custodianPort = 9444

// custodianURL is where a component relays a person's credential
// writes. In the control namespace beside the director, and for the same
// reason: it holds the OpenBao connection and no authority of its own.
func (r *ComponentReconciler) custodianURL() string {
	if r.CustodianURL != "" {
		return r.CustodianURL
	}
	return fmt.Sprintf("http://gentian-os-custodian.%s.svc.cluster.local:%d",
		layout.Namespace(layout.Control), custodianPort)
}

// registrarPort is charts/gentian-os/values.yaml's registrar.port, named
// for the reason custodianPort is.
const registrarPort = 9445

// registrarURL is where a component relays the managing of people. In the
// control namespace beside the director and the custodian.
func (r *ComponentReconciler) registrarURL() string {
	if r.RegistrarURL != "" {
		return r.RegistrarURL
	}
	return fmt.Sprintf("http://gentian-os-registrar.%s.svc.cluster.local:%d",
		layout.Namespace(layout.Control), registrarPort)
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
	b := ctrl.NewControllerManagedBy(mgr).
		Named("component").
		For(&gentianov1alpha1.Component{}).
		Owns(&gatewayv1.HTTPRoute{}).
		Watches(release, componentOfRelease()).
		Watches(&gentianov1alpha1.ComponentProfile{}, componentsOfProfile(mgr.GetClient())).
		Watches(&corev1.Secret{}, componentsOfZoneSecret(mgr.GetClient()))
	if r.WatchClusterClaim {
		b = b.Watches(clusterClaimObject(), componentsToldTheStore(mgr.GetClient()),
			builder.WithPredicates(storeAddressChanged()))
	}
	return b.Complete(r.guarded())
}
