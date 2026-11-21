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
	"sort"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/layout"
)

// A zone's Keycloak client admits a redirect only to a host it lists, and the
// list was written by hand in the tenant Composition: desktop, admin, and for
// the kernel realm argocd, headlamp and id. A component whose profile declares
// any other subDomain got "Invalid parameter: redirect_uri" on an error page
// that names neither the component nor a redirect URI list. The administration
// console hit exactly that the first time its tile was opened.
//
// The operator is what knows every host a zone serves, because it composes the
// routes. This projects that per tenant, the way the tile catalog is already
// projected, so the list follows the components.
//
// What it does NOT cover, and cannot: argocd, headlamp and id are kernel tier.
// They are installed by install.sh and are not components, so nothing here
// sees them and the Composition still names them. Two of the five hosts follow
// the components; three are still a list, for a reason rather than an
// oversight.
const (
	// zoneHostsConfigType labels the ConfigMaps this writes, so the
	// Composition's extra-resources step can select them by kind rather than
	// by guessing a name.
	zoneHostsConfigType = "zone-hosts"
	// zoneHostsKey is the one key: a sorted, newline-separated list of host
	// labels, which is what a Composition can read without parsing.
	zoneHostsKey = "hosts"
	// zoneExchangeProfilesKey is the second thing the zone's realm has to be
	// told about the components, in the same shape: the sorted,
	// newline-separated names of the profiles whose exposures ask for an
	// exchanged token (exchangeToken), and of the apps calls through an
	// installed agent bridge are for (agentBridgeApp). The Composition makes one client scope
	// in the realm per name, called what AppTokenScope calls it, and that
	// scope is what the bouncer asks for on the component's routes.
	//
	// Here and not on the composite's spec.apps: that list holds what a
	// tenant installed, and the components every tenant is given -- the
	// desktop, the administration console -- are not on it. A route of one
	// of those would name a scope the realm never had. This follows the
	// Components, as the routes do.
	zoneExchangeProfilesKey = "exchangeProfiles"
	// zoneVouchingProfilesKey is the third: the components that vouch for
	// people at the realm (requires.services.vouching), and for each of them
	// where the realm fetches the keys its statements are signed with. One
	// line per profile, sorted, four or five fields separated by single
	// spaces:
	//
	//	<profile> <service> <port> <path> [<what else its client may do>]
	//
	// for example "scribe scribe-keys 8443 /.well-known/jwks.json", or
	// "steward - 0 - exchange" for a profile that names no keys. A space is
	// a safe separator because none of them can hold one: the first two
	// are DNS labels, the third is a number, the fourth is held to the
	// characters of a URL path by the profile's schema, and the fifth is a
	// list of words joined by commas: "exchange" where the profile says
	// requires.services.vouching.exchange, then the profile's
	// requires.services.vouching.director. The
	// path is always written out, the default included, so the Composition
	// does not have to know what the default is. The director's scopes are
	// written only in the platform tenant's projection: a token for the director is the cluster's administrators', who
	// sign in at that tenant's realm, and in another tenant's realm the
	// scopes are never made.
	//
	// The Composition makes, per line, the identity provider entry and the
	// client VouchingAlias names, and the Secret VouchingSecretName names. It
	// puts the service's address together itself, because the namespace in it
	// is the tenant's and that is the Composition's to say.
	//
	// Here for the reason the exchange profiles are: a component every tenant
	// is given may vouch as well as an installed one may.
	zoneVouchingProfilesKey = "vouchingProfiles"
	// zoneBridgeScopesKey is what the realm is told of the tenant's served
	// bridges: per app a bridge connects, the scope a token presented at
	// the door is exchanged for before that app sees a request. A JSON
	// document (bridgeScopesDoc), because a scope carries protocol mappers
	// and a line cannot.
	//
	// The Composition makes the client scope BridgeTokenScope names, puts
	// the audience and the mappers on it, and adds it to the exchange
	// client's optional scopes: that list is replaced wholesale, so it has
	// one writer. The key is absent while no bridge is served, and the
	// realm then holds none of this.
	zoneBridgeScopesKey = "bridgeScopes"
	// zoneHomeScopesKey is, in the same form, what the realm is told of the
	// tenant's homes that are called as the person: per home, the scope
	// HomeTokenScope names, carrying the token the home's own sign-in
	// issues. Only the clients of the components the home is granted to are
	// given it (the "home=" words of their vouching lines). Absent while no
	// home is called so.
	zoneHomeScopesKey = "homeScopes"
	// vouchingExchangeWord is, in the fifth field of a vouching line, the
	// word that switches token exchange on at the component's client.
	vouchingExchangeWord = "exchange"
	// vouchingNarrowWord is, in the fifth field, the mark of a component
	// below the platform's trust: its client is given no app scope, only the
	// scopes of the homes its "home=" words name.
	vouchingNarrowWord = "narrow"
	// vouchingHomePrefix starts a word of the fifth field that names a home
	// granted to the component, which it calls as the person: "home=" and
	// the home's profile. Its client is given HomeTokenScope of it.
	vouchingHomePrefix = "home="
	// vouchingNoKeys stands where a service and a path would, in the line of
	// a profile that names no keys. Neither a DNS label nor a path can be a
	// lone hyphen, so it is never mistaken for one.
	vouchingNoKeys = "-"

	// vouchingKeysDefaultPath is where a component's keys are fetched from
	// when its profile names no path: the default the profile's schema
	// states, repeated for a profile that was never defaulted by an API
	// server.
	vouchingKeysDefaultPath = "/.well-known/jwks.json"
)

// zoneHostsConfigMapName is one ConfigMap per tenant, in the control
// namespace beside the tile catalog rather than in the tenant's own, so a
// tenant that has not been provisioned yet still has somewhere for this to
// land.
func zoneHostsConfigMapName(tenant string) string { return "gentian-zone-hosts-" + tenant }

// projectZoneHosts writes, for every tenant, the host labels its components'
// gateway exposures serve, the profiles among them that ask for an exchanged
// token, and the ones that vouch for people.
//
// Labels and not full hostnames: the Composition builds the redirect URI from
// the tenant's effective domain, which it knows and this does not -- a tenant
// with a custom domain is served at a different suffix than the one the
// operator's KernelDomain would produce.
func (r *TileProjectionReconciler) projectZoneHosts(ctx context.Context) error {
	tenants := &gentianov1alpha1.TenantList{}
	if err := r.List(ctx, tenants); err != nil {
		return err
	}
	components := &gentianov1alpha1.ComponentList{}
	if err := r.List(ctx, components); err != nil {
		return err
	}

	byNamespace := map[string]string{}
	byName := map[string]*gentianov1alpha1.Tenant{}
	for i := range tenants.Items {
		byNamespace[tenants.Items[i].NamespaceName()] = tenants.Items[i].Name
		byName[tenants.Items[i].Name] = &tenants.Items[i]
	}

	// Every tenant gets an entry, including one whose components declare no
	// host of their own: an empty projection is the answer "these components
	// need nothing added", and its absence would be indistinguishable from
	// "the operator has not looked yet".
	hosts := map[string]map[string]bool{}
	bridged := map[string]map[string]bool{}
	exchange := map[string]map[string]bool{}
	vouching := map[string]map[string]string{}
	// Per tenant namespace, the profiles that vouch, and whether each signs
	// statements (names keys): what keepVouchingIssuer is told.
	issuers := map[string]map[string]bool{}
	partners := map[string]map[string]string{}
	// The components that call a home as the person (integrations[].asPerson)
	// by profile, per tenant: their vouching line names the homes granted to
	// them, which are only known once every binding of the tenant is read.
	asPerson := map[string]map[string]*gentianov1alpha1.ComponentProfile{}
	// The components that act for people at every home of the tenant
	// (vouching.homes), and the installed providers of each home's
	// contract: the tenant's homes are known once all are read.
	syncers := map[string]map[string]*gentianov1alpha1.ComponentProfile{}
	providers := map[string]map[string][]string{}
	for i := range tenants.Items {
		syncers[tenants.Items[i].Name] = map[string]*gentianov1alpha1.ComponentProfile{}
		providers[tenants.Items[i].Name] = map[string][]string{}
		asPerson[tenants.Items[i].Name] = map[string]*gentianov1alpha1.ComponentProfile{}
		hosts[tenants.Items[i].Name] = map[string]bool{}
		bridged[tenants.Items[i].Name] = map[string]bool{}
		exchange[tenants.Items[i].Name] = map[string]bool{}
		vouching[tenants.Items[i].Name] = map[string]string{}
		partners[tenants.Items[i].Name] = map[string]string{}
	}

	for i := range components.Items {
		comp := &components.Items[i]
		if comp.DeletionTimestamp != nil {
			continue
		}
		tenant, ok := byNamespace[comp.Namespace]
		if !ok {
			// A service or a shared backend is in nobody's zone.
			continue
		}
		profile, err := getProfileFor(ctx, r.Client, comp.Spec.ProfileRef.Name, comp.Spec.ProfileRef.Digest)
		if err != nil {
			if errors.IsNotFound(err) {
				continue
			}
			return err
		}
		// The app a served bridge connects is handed a token at the door.
		// Only while the bridge is served: approved, its app installed and
		// its paths its own.
		if bridgeEntryOf(profile) != nil &&
			apimeta.IsStatusConditionTrue(comp.Status.Conditions, ConditionTokenEntry) &&
			apimeta.IsStatusConditionTrue(comp.Status.Conditions, ConditionBridge) {
			bridged[tenant][profile.Spec.Bridge.App.Profile] = true
		}
		if asksForExchangedToken(profile) {
			// The profile's name and not the component's: the route builder
			// names the scope after the profile (AppTokenScope), and two
			// components of one profile share the one audience.
			exchange[tenant][profile.Name] = true
		}
		if app := agentBridgeApp(profile.Name, profile); app != "" {
			// An installed agent bridge's app is handed tokens too: a
			// component that vouches for people obtains one for it, by this
			// scope, to call the bridge as the person. Without it the realm refuses
			// that exchange for an app with no exchangeToken entry of its own.
			exchange[tenant][app] = true
		}
		if wantsVouching(profile) {
			// By the profile's name, as above and for the same reason: the
			// realm knows the component as VouchingAlias(profile), and two
			// components of one profile are one issuer with one set of keys.
			vouching[tenant][profile.Name] = vouchingLine(profile, tenant, nil)
			if issuers[comp.Namespace] == nil {
				issuers[comp.Namespace] = map[string]bool{}
			}
			issuers[comp.Namespace][profile.Name] = profile.Services().Vouching.Keys != nil
			if callsHomesAsPerson(profile) {
				asPerson[tenant][profile.Name] = profile
			}
			if profile.Services().Vouching.Homes {
				syncers[tenant][profile.Name] = profile
			}
		}
		for _, c := range profile.Spec.Provides {
			if isHomeContract(c.Name) {
				providers[tenant][c.Name] = append(providers[tenant][c.Name], profile.Name)
			}
		}
		if profile.Spec.HomeSync != nil {
			// The home sync obtains a person's token for this app by its
			// scope, as a component that vouches does for any app.
			exchange[tenant][profile.Name] = true
		}
		if wantsPartners(profile) {
			// By the profile's name, as above: the realm knows the
			// component as partner-<profile>.
			partners[tenant][profile.Name] = partnerLine(profile)
		}
		for j := range profile.Spec.Expose {
			e := &profile.Spec.Expose[j]
			// Gateway entries only. A perimeter surface is published through
			// its own proxy with its own credential and never reaches the
			// zone's client, so listing it as a redirect URI would widen the
			// client for a host the zone does not serve.
			if e.Surface != gentianov1alpha1.SurfaceGateway {
				continue
			}
			label := e.SubDomain
			if label == "" {
				// An entry with no subDomain is served on the component's own
				// name, which is what the route builder uses.
				label = comp.Name
			}
			hosts[tenant][label] = true
		}
	}

	homeApps := map[string]map[string]bool{}
	for ns, tenant := range byNamespace {
		homes, err := r.grantedHomes(ctx, ns, asPerson[tenant])
		if err != nil {
			return err
		}
		homes = withPlacedHomes(homes, asPerson[tenant], byName[tenant], providers[tenant])
		homeApps[tenant] = map[string]bool{}
		for consumer, byContract := range homes {
			set := map[string]bool{}
			for _, p := range byContract {
				set[p] = true
			}
			providers := sortedKeys(set)
			vouching[tenant][consumer] = vouchingLine(asPerson[tenant][consumer], tenant, providers)
			for _, p := range providers {
				homeApps[tenant][p] = true
			}
		}
		// Every component that calls a home as the person, with or without
		// one granted now: a grant taken back takes the scope's name away.
		for consumer := range asPerson[tenant] {
			if err := r.keepVouchingHomes(ctx, ns, consumer, homes[consumer]); err != nil {
				return err
			}
		}
	}

	for i := range tenants.Items {
		tenant := &tenants.Items[i]
		homes := dataHomes(tenant, providers[tenant.Name])
		if len(homes) == 0 {
			continue
		}
		if homeApps[tenant.Name] == nil {
			homeApps[tenant.Name] = map[string]bool{}
		}
		for name, profile := range syncers[tenant.Name] {
			vouching[tenant.Name][name] = vouchingLine(profile, tenant.Name, homes)
			for _, h := range homes {
				homeApps[tenant.Name][h] = true
			}
		}
	}

	for ns, profiles := range issuers {
		for profile, signs := range profiles {
			if err := r.keepVouchingIssuer(ctx, ns, profile, signs); err != nil {
				return err
			}
		}
	}

	for tenant, set := range hosts {
		vouchingLines := make([]string, 0, len(vouching[tenant]))
		for _, line := range vouching[tenant] {
			vouchingLines = append(vouchingLines, line)
		}
		// Every line starts with its profile's name and no two share one, so
		// sorting the lines sorts by profile.
		sort.Strings(vouchingLines)
		bridgeScopes, err := r.bridgeScopes(ctx, sortedKeys(bridged[tenant]))
		if err != nil {
			return err
		}
		// A home's token is the one its own sign-in issues, as a bridged
		// app's is: the same audience and claims, under a scope of its own.
		homeScopes, err := r.bridgeScopes(ctx, sortedKeys(homeApps[tenant]))
		if err != nil {
			return err
		}
		if err := r.writeZoneHosts(ctx, tenant, sortedKeys(set), sortedKeys(exchange[tenant]), vouchingLines, sortedValues(partners[tenant]), bridgeScopes, homeScopes); err != nil {
			return err
		}
	}
	return nil
}

// agentBridgeApp is the app a call through an agent bridge is for, where the
// bridge is called (its way from the agents names an endpoint), and empty for
// anything else: the app it connects, or, for an agent bridge that connects
// none of its tenant's apps and reaches a service elsewhere, the bridge
// itself (name, its install name). The rights check asks can_use on this
// app, and the token the bridge is handed is for it (scope app-<app>): so a
// person may use a bridge of the second kind exactly where they were given
// the bridge.
func agentBridgeApp(name string, profile *gentianov1alpha1.ComponentProfile) string {
	way := profile.AgentWay()
	if way == nil || way.Endpoint == nil {
		return ""
	}
	if app := profile.BridgedAppName(); app != "" {
		return app
	}
	return name
}

// asksForExchangedToken reports whether any exposure of the profile declares
// exchangeToken. One entry is enough: the scope is the app's, not the
// entry's, and every route of the app that asks is given the same one.
//
// Deliberately wider than the route builder, which also wants the entry
// behind a session and not already forwarding the session's own token. A
// scope no route asks for costs the realm one unused object; a route asking
// for a scope the realm does not hold costs the person the page.
func asksForExchangedToken(profile *gentianov1alpha1.ComponentProfile) bool {
	for i := range profile.Spec.Expose {
		if profile.Spec.Expose[i].ExchangeToken {
			return true
		}
	}
	return false
}

// vouchingLine is a vouching profile's entry in the projection: its name and
// where its keys are published, in the form zoneVouchingProfilesKey
// describes. Only called for a profile wantsVouching says yes to.
func vouchingLine(profile *gentianov1alpha1.ComponentProfile, tenant string, homes []string) string {
	vouching := profile.Services().Vouching
	// A profile that names no keys exchanges and does not vouch: the three
	// fields say so, and the Composition makes no issuer of it.
	fields := []string{profile.Name, vouchingNoKeys, "0", vouchingNoKeys}
	if keys := vouching.Keys; keys != nil {
		path := keys.Path
		if path == "" {
			path = vouchingKeysDefaultPath
		}
		fields = []string{profile.Name, keys.Service, strconv.Itoa(int(keys.Port)), path}
	}
	// The fifth field: what the client may do beside vouching.
	var extra []string
	if vouching.Exchange {
		extra = append(extra, vouchingExchangeWord)
	}
	if tenant == gentianov1alpha1.PlatformTenantName && len(vouching.Director) > 0 {
		scopes := make([]string, 0, len(vouching.Director))
		for _, d := range vouching.Director {
			scopes = append(scopes, string(d))
		}
		sort.Strings(scopes)
		extra = append(extra, scopes...)
	}
	// A certified component vouches only towards its homes: its client is
	// given their scopes and no app scope. Admission lets no other tier
	// below the platform's vouch at all.
	//
	// Nor is a component that names no keys and calls a home as the person
	// given more, whatever its trust: it exchanges the token of a person at
	// its address for a home's, and that is all its client is for.
	if profile.Spec.TrustTier == gentianov1alpha1.TrustTierCertified || (vouching.Keys == nil && callsHomesAsPerson(profile)) {
		extra = append(extra, vouchingNarrowWord)
	}
	for _, home := range homes {
		extra = append(extra, vouchingHomePrefix+home)
	}
	if len(extra) > 0 {
		fields = append(fields, strings.Join(extra, ","))
	}
	return strings.Join(fields, " ")
}

// sortedValues are a map's values, sorted. Every partner line starts with its
// profile's name, so this sorts them by profile.
func sortedValues(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for _, v := range m {
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

func sortedKeys(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// writeZoneHosts creates the ConfigMap the first time and patches it only
// when the content differs, so a projection that did not change does not wake
// every Composition that reads it.
func (r *TileProjectionReconciler) writeZoneHosts(ctx context.Context, tenant string, hosts, exchangeProfiles, vouchingProfiles, partnerProfiles []string, bridgeScopes, homeScopes string) error {
	key := types.NamespacedName{
		Name:      zoneHostsConfigMapName(tenant),
		Namespace: layout.Namespace(layout.Control),
	}
	desired := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      key.Name,
			Namespace: key.Namespace,
			Labels: map[string]string{
				managedByLabel:             managedByValue,
				"gentianos.io/config-type": zoneHostsConfigType,
				tenantLabel:                tenant,
			},
		},
		Data: map[string]string{
			zoneHostsKey:            strings.Join(hosts, "\n"),
			zoneExchangeProfilesKey: strings.Join(exchangeProfiles, "\n"),
			zoneVouchingProfilesKey: strings.Join(vouchingProfiles, "\n"),
			partnerProfilesKey:      strings.Join(partnerProfiles, "\n"),
		},
	}
	if bridgeScopes != "" {
		desired.Data[zoneBridgeScopesKey] = bridgeScopes
	}
	if homeScopes != "" {
		desired.Data[zoneHomeScopesKey] = homeScopes
	}

	return ensureObject(ctx, r.Client, desired, func(existing *corev1.ConfigMap) bool {
		if equality.Semantic.DeepEqual(existing.Data, desired.Data) && equality.Semantic.DeepEqual(existing.Labels, desired.Labels) {
			return false
		}
		existing.Data, existing.Labels = desired.Data, desired.Labels
		return true
	})
}

// HomeTokenScope is the scope a component that calls a home as the person
// asks the tenant's realm for: a token of that person, made out as the
// home's own sign-in makes them, valid there and nowhere else.
func HomeTokenScope(homeProfile string) string { return "home-" + homeProfile }

// callsHomesAsPerson reports whether a profile calls any home as the person
// (integrations[].asPerson).
func callsHomesAsPerson(profile *gentianov1alpha1.ComponentProfile) bool {
	for _, i := range profile.Spec.Integrations {
		if i.AsPerson {
			return true
		}
	}
	return false
}

// grantedHomes are, for each component of a tenant's namespace that calls a
// home as the person, the homes it may: the providers of its contracts
// marked asPerson whose binding the tenant's administrator granted in full.
// By contract: a contract has one home.
func (r *TileProjectionReconciler) grantedHomes(
	ctx context.Context, namespace string, consumers map[string]*gentianov1alpha1.ComponentProfile,
) (map[string]map[string]string, error) {
	if len(consumers) == 0 {
		return nil, nil
	}
	bindings := &gentianov1alpha1.IntegrationBindingList{}
	if err := r.List(ctx, bindings, client.InNamespace(namespace)); err != nil {
		if apimeta.IsNoMatchError(err) {
			return nil, nil
		}
		return nil, err
	}
	found := map[string]map[string]string{}
	for i := range bindings.Items {
		ib := &bindings.Items[i]
		profile := consumers[ib.Spec.Consumer.App]
		if profile == nil || ib.DeletionTimestamp != nil ||
			!apimeta.IsStatusConditionTrue(ib.Status.Conditions, conditionBindingGranted) {
			continue
		}
		for _, in := range profile.Spec.Integrations {
			if in.AsPerson && in.Contract == ib.Spec.Contract {
				if found[ib.Spec.Consumer.App] == nil {
					found[ib.Spec.Consumer.App] = map[string]string{}
				}
				found[ib.Spec.Consumer.App][ib.Spec.Contract] = ib.Spec.Provider.App
			}
		}
	}
	return found, nil
}

// withPlacedHomes adds, to the homes granted to a tenant's components, the
// homes of the components the platform places in every tenant and that call
// a home as the person: the tenant's home for each such contract, with
// nobody asked. A placed component is the platform's own (the porter, its
// file dialog), and what it may hand on is decided where an app is granted
// the contract, not here.
func withPlacedHomes(
	homes map[string]map[string]string, consumers map[string]*gentianov1alpha1.ComponentProfile,
	tenant *gentianov1alpha1.Tenant, providers map[string][]string,
) map[string]map[string]string {
	if tenant == nil {
		return homes
	}
	for name, profile := range consumers {
		if profile.Spec.Placement == nil || profile.Spec.TrustTier != gentianov1alpha1.TrustTierPlatform {
			continue
		}
		for _, in := range profile.Spec.Integrations {
			if !in.AsPerson {
				continue
			}
			home := homeOf(tenant, providers, in.Contract)
			if home == "" {
				continue
			}
			if homes == nil {
				homes = map[string]map[string]string{}
			}
			if homes[name] == nil {
				homes[name] = map[string]string{}
			}
			homes[name][in.Contract] = home
		}
	}
	return homes
}

// homeOf is the tenant's home for one contract: the provider the tenant
// names (spec.homes), or the first installed one by name; empty where none
// is installed, or the one named is not.
func homeOf(tenant *gentianov1alpha1.Tenant, providers map[string][]string, contract string) string {
	installed := providers[contract]
	if len(installed) == 0 {
		return ""
	}
	sorted := append([]string(nil), installed...)
	sort.Strings(sorted)
	if named := tenant.Spec.Homes[contract]; named != "" {
		for _, p := range sorted {
			if p == named {
				return named
			}
		}
		return ""
	}
	return sorted[0]
}

// homeContracts are the contracts a home provides.
var homeContracts = []string{"calendar-store", "contacts-store", "file-store"}

func isHomeContract(name string) bool {
	for _, c := range homeContracts {
		if c == name {
			return true
		}
	}
	return false
}

// dataHomes are the tenant's homes, sorted and without repeats: per home
// contract, the provider the tenant names in spec.homes where it is installed,
// and otherwise the first installed provider by name -- the provider every
// consumer of the contract is bound to.
func dataHomes(tenant *gentianov1alpha1.Tenant, providers map[string][]string) []string {
	set := map[string]bool{}
	for _, contract := range homeContracts {
		if home := homeOf(tenant, providers, contract); home != "" {
			set[home] = true
		}
	}
	return sortedKeys(set)
}
