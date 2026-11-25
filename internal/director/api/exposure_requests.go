/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package api

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/addresses"
	"github.com/gentian-org/gentian-os/internal/director/gitops"
)

// What a tenant's apps ASK to have on the internet, beside what was approved.
//
// A perimeter approver decides that an entry is published. Before this they
// were shown nothing to decide on: the registry lists what somebody already
// approved, and the entry a profile declares -- its address, its paths,
// whether anybody signs in -- was in a catalog file the approver had no
// reason to have open. So the read of the registry also answers, for every
// app the tenant has installed, each entry its profile declares for the
// internet, with what approving it would publish and whether it was approved.
//
// The same list is what an approval is checked against. An approval naming an
// app the tenant does not have, or an entry its profile does not declare, was
// recorded as published although the operator publishes nothing for it; it is
// refused now, and the ones recorded before are shown as matching nothing.
//
// Not everything an approver decides here is a public address. An entry
// behind sign-in may ask that the Authorization header be left as the app's
// own page sent it; the same approver decides that, in the same list, and
// every entry says which kind it is. What each kind does is said in the
// director's words (kindLabel, access, rateLimit), which the console and the
// command line show as they are.
//
// Everything here is read from git, which is all the director reads: the
// tenant's apps, the profiles in the cluster's catalog directory, the
// domain bound to the tenant, and the cluster's tenancy mode and domain. The
// address is resolved by the function the operator publishes it with
// (internal/addresses), given those.

// The states of an entry.
const (
	// exposureRequested is declared by an installed app and not approved:
	// nothing is published.
	exposureRequested = "requested"
	// exposureApproved is approved, not expired, and not due for review.
	exposureApproved = "approved"
	// exposureReviewDue is approved and still published, with its review
	// date passed.
	exposureReviewDue = "reviewDue"
	// exposureExpired is approved until a date that has passed: nothing is
	// published any more.
	exposureExpired = "expired"
	// exposureUnmatched is in the registry and matches no perimeter entry of
	// an installed app: the operator publishes nothing for it.
	exposureUnmatched = "unmatched"
)

// exposureEntry is one entry for the internet: what it would publish, and
// whether it was approved.
type exposureEntry struct {
	// Install is the app instance and ExposureName its profile's entry.
	Install      string `json:"install"`
	ExposureName string `json:"exposureName"`
	// State is requested, approved, reviewDue, expired or unmatched.
	State string `json:"state"`
	// Kind is what approving the entry allows: public (a public address
	// that passes no credential on), publicAppCredential (a public address
	// that passes the caller's Authorization header to the app),
	// insideAppCredential (no public address: the same, for callers inside
	// the cluster alone) or signInAppAuthorization (no public address: an
	// entry behind sign-in whose Authorization header is the app's own).
	// KindLabel is the same in a few words, for a list.
	Kind      string `json:"kind,omitempty"`
	KindLabel string `json:"kindLabel,omitempty"`
	// PublicAddress says approving the entry publishes something on the
	// internet. False for an entry behind sign-in and for an inside entry.
	PublicAddress bool `json:"publicAddress"`
	// InsideOnly says approving the entry publishes its paths to the
	// cluster's own pods alone: at a name only the cluster's DNS knows, on a
	// Gateway the internet has no way to, for the components granted the
	// host as egress.
	InsideOnly bool `json:"insideOnly"`
	// PassesCredential says the caller's Authorization header reaches the
	// app as the caller sent it once the entry is approved.
	PassesCredential bool `json:"passesCredential"`
	// RateLimit is the limit one client address is held to on a public
	// address, in a sentence.
	RateLimit string `json:"rateLimit,omitempty"`
	// Host is the public address, without scheme: where the entry answers
	// once approved. Empty for an entry that is published nowhere; Note
	// says why.
	Host string `json:"host,omitempty"`
	// Alias is a second address the surface answers at, directly under
	// the cluster's own domain, where the cluster's administrator gave it
	// one. Only on a published surface.
	Alias string `json:"alias,omitempty"`
	// Paths are the path prefixes published, and nothing else on the host
	// is. DenyPaths are refused even below one of them.
	Paths     []string `json:"paths,omitempty"`
	DenyPaths []string `json:"denyPaths,omitempty"`
	// ClientRedirectURIs are, for a token entry, the addresses a program may
	// be sent back to after the person signed in: approving the entry
	// registers them at the tenant's sign-in service.
	ClientRedirectURIs []string `json:"clientRedirectURIs,omitempty"`
	// SignInCookies are, for publicAppSignIn, the cookies of the app's own
	// sign-in and the paths they pass on, as the profile declares them.
	SignInCookies *gentianov1alpha1.SignInCookies `json:"signInCookies,omitempty"`
	// AuthMode is the profile's own word for who the app expects to call.
	AuthMode string `json:"authMode,omitempty"`
	// AnyoneWithoutSignIn says the profile declares that nobody signs in
	// (authMode none).
	AnyoneWithoutSignIn bool `json:"anyoneWithoutSignIn"`
	// Access is the same in a sentence, for a person.
	Access string `json:"access,omitempty"`
	// MainAddress says the entry is for a bare name -- the cluster's main
	// address, or the tenant's own domain (Host says which) -- and
	// MainAddressRule is what the approver has to acknowledge to publish it
	// there.
	MainAddress     bool   `json:"mainAddress"`
	MainAddressRule string `json:"mainAddressRule,omitempty"`
	// Note is why the entry is published nowhere, or why it matches
	// nothing.
	Note string `json:"note,omitempty"`
	// Approval is the registry's entry: by whom, when, until when, why.
	// Absent for a request.
	Approval *gitops.Exposure `json:"approval,omitempty"`
}

// The kinds, by the names the registry records (gitops, and the Tenant's
// schema behind it).
const (
	kindPublic                 = gitops.KindPublic
	kindPublicAppCredential    = gitops.KindPublicAppCredential
	kindPublicAppSignIn        = gitops.KindPublicAppSignIn
	kindInsideAppCredential    = gitops.KindInsideAppCredential
	kindSignInAppAuthorization = gitops.KindSignInAppAuthorization
	kindTokenEntry             = gitops.KindTokenEntry
)

// kindLabels are the kinds in a few words, for a list.
var kindLabels = map[string]string{
	kindPublic:                 "Public address",
	kindPublicAppCredential:    "Public address that passes the caller's credential to the app",
	kindPublicAppSignIn:        "Public address that passes the caller's credential, and the app's own sign-in cookies, to the app",
	kindInsideAppCredential:    "Inside the cluster only: passes the caller's credential to the app",
	kindSignInAppAuthorization: "Behind sign-in: keeps the app's own Authorization header",
	kindTokenEntry:             "Token entry: programs reach it as a person, with a token of the tenant's realm",
}

// normalKind reads a registry entry that names no kind as what every entry
// was before kinds were recorded: a public address.
func normalKind(k string) string {
	if k == "" {
		return kindPublic
	}
	return k
}

// accessOf says what approving an entry of a kind allows, for a person who
// decides on it. The sentences are the whole of what the platform does and
// does not do; nothing here may read as a check the platform does not make.
//
//   - public: the publishing proxy forwards the declared paths and drops
//     every credential on the way in (component_perimeter_config.go).
//   - publicAppCredential: the same proxy, passing the one header on.
//   - publicAppSignIn: the same, and on the sign-in paths the named cookies
//     in and the app's Set-Cookie out (signInAccessOf).
//   - insideAppCredential: the same proxy and header, behind the inside
//     Gateway (component_perimeter.go, coredns_hairpin.go,
//     own_served_hosts.go).
//   - signInAppAuthorization: the session and the bouncer as on any entry
//     behind sign-in; only the header is left alone (internal/bouncer).
//   - tokenEntry: no sign-in filter on the route; the bouncer verifies the
//     token and asks the store, as on any entry (internal/bouncer,
//     Route.Audience; internal/controller, token_entry.go).
func accessOf(kind string) string {
	switch kind {
	case kindPublic:
		return "Reachable by anyone on the internet without sign-in. " +
			"No credential is passed to the app: the platform removes the Authorization header and all cookies from every request."
	case kindPublicAppCredential:
		return "Nobody signs in at the platform's edge: anyone on the internet can send requests to these paths. " +
			"The caller's credential (the Authorization header) is passed to the app as the caller sent it, and the app alone checks it. " +
			"The platform does not know or check who calls. " +
			"An app password or token of a person who was removed from the tenant keeps working until the app itself revokes it. " +
			"Cookies are not passed to the app and the app cannot set any, so a client that needs cookies does not work on this address."
	case kindPublicAppSignIn:
		return signInAccessOf(nil)
	case kindInsideAppCredential:
		return "This is not a public address: nothing on the internet can reach it, and no public DNS record names it. " +
			"Only programs running in this cluster reach these paths, at a name only the cluster's own DNS answers, " +
			"and only those whose app was granted this address as a way out. " +
			"Nobody signs in on the way: the caller's credential (the Authorization header) is passed to the app as the caller sent it, and the app alone checks it. " +
			"The platform does not know or check who calls. " +
			"An app password or token of a person who was removed from the tenant keeps working until the app itself revokes it. " +
			"Cookies are not passed to the app and the app cannot set any."
	case kindSignInAppAuthorization:
		return "This is not a public address. People still have to sign in, and still have to be allowed to use the app, exactly as before. " +
			"Once approved, the platform leaves the Authorization header on requests to this address as the app's own page sent it: " +
			"it no longer replaces or removes that header, and puts no token of its own there. " +
			"The app receives whatever a signed-in person's browser sends in that header, and the app alone checks it. " +
			"Until approved, the header is removed before the app, and calls the app's own pages make with a token of the app's are refused by the app."
	case kindTokenEntry:
		return "This is not a public address, and nobody signs in on it with a browser. " +
			"A program, such as an AI assistant on a person's computer, reaches this component as that person: " +
			"it presents a token the tenant's sign-in service issued to the person for this address alone, after the person signed in and agreed. " +
			"The platform checks every request: that the token is valid, was issued by this tenant's sign-in service for this address, " +
			"belongs to a sign-in that has not ended, and that the person may use the component. " +
			"A person who is removed from the tenant, or whose sign-in is ended, loses this access within minutes. " +
			"A token for this address is refused everywhere else on the platform, and a browser's session is refused here. " +
			"The component receives the person's token and acts for them in the tenant's apps, with that person's rights. " +
			"Until approved, the address answers nobody."
	}
	return ""
}

// signInAccessOf is accessOf's sentence for publicAppSignIn, naming the
// cookies and paths where the entry declares them.
func signInAccessOf(c *gentianov1alpha1.SignInCookies) string {
	cookies, paths := "the cookies the app's catalog entry names", "the paths of the app's own sign-in"
	if c != nil {
		cookies = "the cookies " + strings.Join(c.Names, ", ")
		paths = strings.Join(c.Paths, ", ")
	}
	return "Nobody signs in at the platform's edge: anyone on the internet can send requests to these paths. " +
		"The caller's credential (the Authorization header) is passed to the app as the caller sent it, and the app alone checks it. " +
		"The platform does not know or check who calls. " +
		"An app password or token of a person who was removed from the tenant keeps working until the app itself revokes it. " +
		fmt.Sprintf("On %s alone, where the app signs a person in through the tenant's sign-in service, %s are passed to the app, and the cookies the app sets there reach the browser. ", paths, cookies) +
		"No other cookie the browser holds is passed, the tenant's own sign-in session among them. " +
		"Everywhere else on this address no cookie is passed and the app cannot set any."
}

// bridgeAccessOf says what approving a way of a bridge allows: a token entry
// as accessOf's, except that the bouncer asks for the right to use the app
// the bridge connects and hands the app an exchanged token, never the one the
// program presented (internal/controller, bridge.go).
func bridgeAccessOf(app string, protocol gentianov1alpha1.BridgeProtocolName) string {
	return "This is not a public address, and nobody signs in on it with a browser. " +
		fmt.Sprintf("A program on a person's own device, such as a file sync, calendar or address book program, reaches these paths of %s in %s as that person: ", app, protocol) +
		"it presents a token the tenant's sign-in service issued to the person for this address alone, after the person signed in and agreed. " +
		"The platform checks every request: that the token is valid, was issued by this tenant's sign-in service for this address, " +
		fmt.Sprintf("belongs to a sign-in that has not ended, and that the person may use %s. ", app) +
		"A person who is removed from the tenant, or whose sign-in is ended, loses this access within minutes. " +
		"A token for this address is refused everywhere else on the platform, and a browser's session is refused here. " +
		fmt.Sprintf("%s never receives the token the program presented: the platform exchanges it for one made out to %s alone, and %s acts with the person's own rights in it. ", app, app, app) +
		"Until approved, these paths answer nobody."
}

// rateLimitOf says the limit one client address is held to on a public
// address. The numbers are the ones the operator renders the proxy from
// (addresses.PerimeterRateFor); the cluster's administrator may have set
// others, which the repository does not say.
func rateLimitOf(kind string) string {
	var rate addresses.PerimeterRate
	why := ""
	switch kind {
	case kindPublic:
		rate = addresses.PerimeterRateFor(false)
	case kindPublicAppCredential, kindPublicAppSignIn:
		rate = addresses.PerimeterRateFor(true)
		why = " The limit is lower than on other public addresses, because every request may be a guess at a password."
	case kindInsideAppCredential:
		rate = addresses.PerimeterRateFor(true)
		why = " A client address here is a program's in the cluster. The limit is the one of a public address that passes a credential on, because every request may be a guess at a password."
	default:
		return ""
	}
	return fmt.Sprintf(
		"Each client address may make %d requests a second, with %d more at once and %d at a time; beyond that the platform answers 429 (too many requests). "+
			"These are the platform's defaults; the cluster's administrator may have set others.%s",
		rate.PerSecond, rate.Burst, rate.Concurrent, why)
}

// tenantInstalls is what a tenant has installed, by the name its Component
// has: every app of spec.apps and every add-on switched on inside one.
func tenantInstalls(apps []gitops.App) map[string]bool {
	out := map[string]bool{}
	for _, a := range apps {
		if a.Profile != "" {
			out[a.Profile] = true
		}
		for _, addon := range a.Addons {
			if addon != "" {
				out[addon] = true
			}
		}
	}
	return out
}

// exposureView is everything the list and the approval's check are decided
// from, read once.
type exposureView struct {
	tenant    string
	installs  map[string]bool
	profiles  map[string]*gentianov1alpha1.ComponentProfile
	unread    map[string]bool // installed, and its profile could not be read
	published []gitops.Exposure
	inputs    addresses.Inputs
	subject   *gentianov1alpha1.Tenant
}

// kernelRealmName is the realm the platform tenant's people are in. The
// operator can be told another (KERNEL_REALM); the repository does not say,
// and the director goes by the default as tenancy.IsPlatformTenant does.
const kernelRealmName = "kernel"

// readExposureView reads what a tenant installed, what those profiles
// declare, and what was approved.
func (s *Server) readExposureView(ctx context.Context, tenant string) (*exposureView, error) {
	published, err := s.cfg.Repo.TenantExposures(ctx, tenant)
	if err != nil {
		return nil, err
	}
	all, err := s.cfg.Repo.Apps(ctx, tenant)
	if err != nil {
		return nil, err
	}
	// The platform's own components publish what their profiles say, as
	// they always have; their surfaces are not a tenant's to request.
	apps := installedApps(all)
	placement, err := s.cfg.Repo.TenantPlacement(ctx, tenant)
	if err != nil {
		return nil, err
	}
	// No claim is a cluster with no domain: nothing has an address, and the
	// entries are listed without one.
	kernelDomain, err := s.cfg.Repo.KernelDomain(ctx)
	if err != nil && !errors.Is(err, gitops.ErrNoClusterClaim) {
		return nil, err
	}
	settings, err := s.cfg.Repo.ClusterSettingValues(ctx)
	if err != nil && !errors.Is(err, gitops.ErrNoClusterClaim) {
		return nil, err
	}

	v := &exposureView{
		tenant: tenant, installs: tenantInstalls(apps), published: published,
		profiles: map[string]*gentianov1alpha1.ComponentProfile{}, unread: map[string]bool{},
	}
	for name := range v.installs {
		definition, err := s.cfg.Repo.ProfileDefinition(ctx, name)
		switch {
		case errors.Is(err, gitops.ErrProfileUnreadable), errors.Is(err, gitops.ErrInvalidName):
			v.unread[name] = true
		case err != nil:
			return nil, err
		case definition == nil:
			v.unread[name] = true
		default:
			v.profiles[name] = definition
		}
	}

	// The tenant as the operator would hold it, from what git says: its
	// name and realm, and the bound domain where the operator will put it.
	// Ready is taken for granted -- the question is where an entry answers,
	// not whether the tenant has come up yet.
	subject := &gentianov1alpha1.Tenant{}
	subject.Name = tenant
	if placement.Realm != "" {
		subject.Spec.Isolation = &gentianov1alpha1.TenantIsolation{KeycloakRealm: placement.Realm}
	}
	subject.Status.Domain = placement.CustomDomain
	subject.Status.Phase = gentianov1alpha1.TenantPhaseReady
	for _, e := range published {
		subject.Spec.Exposures = append(subject.Spec.Exposures, registryEntry(e))
	}
	v.subject = subject
	v.inputs = addresses.Inputs{
		Profiles:     v.profiles,
		KernelDomain: kernelDomain, KernelRealm: kernelRealmName, TenancyMode: settings["tenancyMode"],
		MainAddressTenant: settings[mainAddressTenantSetting],
		Now:               time.Now(),
	}
	return v, nil
}

// registryEntry is a registry entry as the Tenant carries it, which is what
// the address rule reads.
func registryEntry(e gitops.Exposure) gentianov1alpha1.TenantExposure {
	out := gentianov1alpha1.TenantExposure{
		Install: e.Install, ExposureName: e.ExposureName, Owner: e.Owner, Reason: e.Reason, Apex: e.Apex,
		Kind: gentianov1alpha1.ExposureKind(e.Kind), SignInCookies: e.SignInCookies, Alias: e.Alias,
	}
	if at, err := time.Parse(time.RFC3339, e.ReviewAt); err == nil {
		out.ReviewAt = metav1.NewTime(at)
	}
	stamp := func(v string) *metav1.Time {
		at, err := time.Parse(time.RFC3339, v)
		if v == "" || err != nil {
			return nil
		}
		t := metav1.NewTime(at)
		return &t
	}
	out.ExpiresAt, out.PublishedAt, out.AliasSetAt = stamp(e.ExpiresAt), stamp(e.PublishedAt), stamp(e.AliasSetAt)
	return out
}

// entriesOf are the entries of a profile an approver may be asked about: the
// ones it declares, and, for a bridge, one per way the tenant's door serves.
// A bridge declares no exposure; what is approved of each way is that
// programs reach the app it connects, under the way's paths, with a token of
// the tenant's realm.
func entriesOf(profile *gentianov1alpha1.ComponentProfile) []*gentianov1alpha1.ExposureSpec {
	if profile == nil {
		return nil
	}
	var out []*gentianov1alpha1.ExposureSpec
	for i := range profile.Spec.Expose {
		out = append(out, &profile.Spec.Expose[i])
	}
	out = append(out, profile.BridgeWayEntries()...)
	return out
}

// approvableEntry is the profile's entry of this name that asks a perimeter
// approver for something, or nil: a perimeter entry in a mode the platform
// serves, or an entry behind sign-in that declares the Authorization header
// the app's own. exists says the profile has an entry of the name at all,
// and declared names the entries that do ask, for a refusal.
func approvableEntry(profile *gentianov1alpha1.ComponentProfile, name string) (entry *gentianov1alpha1.ExposureSpec, exists bool, declared []string) {
	for _, e := range entriesOf(profile) {
		asks := e.RequestKind() != ""
		if asks {
			declared = append(declared, e.Name)
		}
		if e.Name != name {
			continue
		}
		exists = true
		if asks {
			entry = e
		}
	}
	sort.Strings(declared)
	return entry, exists, declared
}

// resolve is where an entry answers. approval is the registry's entry, or
// nil for a request, which is resolved as it would be approved: for the main
// address if the profile declares it for that, and published now.
func (v *exposureView) resolve(install string, entry *gentianov1alpha1.ExposureSpec, approval *gitops.Exposure) (host, why string) {
	in, subject := v.inputs, v.subject
	forMain := entry.Apex
	if approval != nil {
		forMain = approval.Apex
	} else {
		asked := subject.DeepCopy()
		now := metav1.NewTime(v.inputs.Now)
		asked.Spec.Exposures = append(asked.Spec.Exposures, gentianov1alpha1.TenantExposure{
			Install: install, ExposureName: entry.Name, Apex: entry.Apex, PublishedAt: &now,
		})
		subject = asked
	}
	in.Tenants = []gentianov1alpha1.Tenant{*subject}
	return addresses.Resolve(in, &in.Tenants[0], install, entry, forMain)
}

// describe fills in what approving an entry allows.
func (v *exposureView) describe(out *exposureEntry, entry *gentianov1alpha1.ExposureSpec) {
	kind := string(entry.RequestKind())
	out.Kind, out.KindLabel = kind, kindLabels[kind]
	out.PublicAddress = entry.Surface == gentianov1alpha1.SurfacePerimeter
	out.InsideOnly = entry.Surface == gentianov1alpha1.SurfaceInside
	out.PassesCredential = kind == kindPublicAppCredential || kind == kindPublicAppSignIn || kind == kindInsideAppCredential ||
		kind == kindSignInAppAuthorization || kind == kindTokenEntry
	out.AuthMode = string(entry.AuthMode)
	if kind == kindTokenEntry {
		out.ClientRedirectURIs = append([]string{}, entry.ClientRedirectURIs...)
	}
	out.AnyoneWithoutSignIn = kind == kindPublic
	out.Access, out.RateLimit = accessOf(kind), rateLimitOf(kind)
	if kind == kindPublicAppSignIn {
		out.Access = signInAccessOf(entry.SignInCookies)
		out.SignInCookies = entry.SignInCookies.DeepCopy()
	}
	for _, p := range entry.DenyPaths {
		if p = strings.TrimSpace(p); p != "" {
			out.DenyPaths = append(out.DenyPaths, p)
		}
	}
	sort.Strings(out.DenyPaths)
	if !out.PublicAddress && !out.InsideOnly {
		// Behind sign-in: the address is the one the entry already has on
		// the tenant's own domain, and approving publishes nothing. An
		// entry that names no paths there is the whole host.
		zone := addresses.ZoneOf(v.subject, v.inputs.KernelDomain, v.inputs.TenancyMode, v.inputs.KernelRealm)
		out.Host = addresses.Host(zone, out.Install, entry)
		out.Paths = append([]string{}, entry.Paths...)
		if len(out.Paths) == 0 {
			out.Paths = []string{"/"}
		}
		sort.Strings(out.Paths)
		return
	}
	out.Host, out.Note = v.resolve(out.Install, entry, out.Approval)
	if out.Host != "" && out.Approval != nil && !out.InsideOnly {
		// By the operator's rule, asked of this tenant alone: what another
		// tenant holds was refused when the alias was set.
		in := v.inputs
		in.Tenants = []gentianov1alpha1.Tenant{*v.subject}
		out.Alias = addresses.AliasHost(in, v.subject.Name, out.Install, out.ExposureName)
	}
	out.Paths = addresses.Prefixes(entry)
	out.MainAddress = entry.Apex
	if entry.Apex {
		out.MainAddressRule = mainAddressRule
	}
}

// approvedAsAnother is why an approval in the registry does not cover what
// the entry declares now, or "".
func approvedAsAnother(entry *gentianov1alpha1.ExposureSpec, approval *gitops.Exposure) string {
	was, now := normalKind(approval.Kind), string(entry.RequestKind())
	if was == now && now == kindPublicAppSignIn && !entry.SignInCookies.Within(approval.SignInCookies) {
		return fmt.Sprintf(
			"This entry was approved by %s with the sign-in cookies %s on %s, and the app's catalog entry now names more. "+
				"Only what was approved passes until it is approved again as it is now",
			approval.Owner, strings.Join(approval.SignInCookies.Names, ", "), strings.Join(approval.SignInCookies.Paths, ", "))
	}
	if was == now {
		return ""
	}
	return fmt.Sprintf(
		"This entry was approved by %s as %q, and the app's catalog entry now asks for %q. "+
			"The earlier approval does not cover that: the platform does nothing for this entry until it is approved again as what it is now. "+
			"Withdraw it to clear the earlier approval",
		approval.Owner, kindLabels[was], kindLabels[now])
}

// stateOf is what is true of an approved entry now.
func stateOf(e gitops.Exposure, now time.Time) string {
	live, expired, due := sortExposures([]gitops.Exposure{e}, now)
	switch {
	case len(expired) > 0:
		return exposureExpired
	case len(due) > 0:
		return exposureReviewDue
	case len(live) > 0:
		return exposureApproved
	}
	return exposureApproved
}

// entries is the list: every entry an installed app declares for the
// internet, approved or not, and every registry entry that matches none.
func (v *exposureView) entries() []exposureEntry {
	out := []exposureEntry{}
	approved := map[string]*gitops.Exposure{}
	for i := range v.published {
		approved[v.published[i].Key()] = &v.published[i]
	}
	matched := map[string]bool{}

	for install, profile := range v.profiles {
		for _, entry := range entriesOf(profile) {
			if entry.RequestKind() == "" {
				// Asks for nothing: an ordinary entry behind sign-in, or
				// one in a mode the platform does not serve.
				continue
			}
			e := exposureEntry{Install: install, ExposureName: entry.Name, State: exposureRequested}
			key := gitops.Exposure{Install: install, ExposureName: entry.Name}.Key()
			changed := ""
			if approval, ok := approved[key]; ok {
				matched[key] = true
				e.Approval, e.State = approval, stateOf(*approval, v.inputs.Now)
				if changed = approvedAsAnother(entry, approval); changed != "" {
					// Approved as something else: it is a request again,
					// with the earlier approval beside it.
					e.State = exposureRequested
				}
			}
			v.describe(&e, entry)
			// A bridge: what the approval lets programs reach, and by
			// what right.
			if way := profile.BridgeWay(entry.Name); way != nil && way.DoorWay() && len(profile.Spec.Expose) == 0 {
				app := profile.Spec.Bridge.App.Profile
				e.Access = bridgeAccessOf(app, way.Protocol)
				e.Note = strings.TrimSpace(fmt.Sprintf(
					"A way of a bridge to %s, in %s: programs on a person's own devices reach these paths of %s as that person, who is asked the right to use %s as at its own address. Each way of the bridge is approved on its own. %s",
					app, way.Protocol, app, app, e.Note))
			}
			if changed != "" {
				e.Note = strings.TrimSpace(changed + ". " + e.Note)
				e.Note = strings.TrimSuffix(e.Note, ". ")
			}
			out = append(out, e)
		}
	}
	for i := range v.published {
		p := &v.published[i]
		if matched[p.Key()] {
			continue
		}
		e := exposureEntry{Install: p.Install, ExposureName: p.ExposureName, Approval: p, MainAddress: p.Apex}
		// What the registry says was approved; what the entry declares is
		// not known here.
		e.Kind = normalKind(p.Kind)
		e.KindLabel = kindLabels[e.Kind]
		e.PublicAddress = e.Kind != kindSignInAppAuthorization && e.Kind != kindTokenEntry && e.Kind != kindInsideAppCredential
		e.InsideOnly = e.Kind == kindInsideAppCredential
		if why := v.unknownToTheRepository(p.Install); why != "" {
			// Not shown as matching nothing: what it matches is not the
			// repository's to say.
			e.State, e.Note = stateOf(*p, v.inputs.Now), why
		} else {
			e.State, e.Note = exposureUnmatched, v.unmatched(p.Install, p.ExposureName)+
				". The operator publishes nothing and changes nothing for it; withdraw it to clear the registry"
		}
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Install != out[j].Install {
			return out[i].Install < out[j].Install
		}
		return out[i].ExposureName < out[j].ExposureName
	})
	return out
}

// unknownToTheRepository is why nothing can be said of an install's entries,
// or "": a component the platform's own chart ships has no profile in the
// repository, and the platform tenant's page on the bare domain is one.
func (v *exposureView) unknownToTheRepository(install string) string {
	if !gitops.PlatformProfile(install) || v.profiles[install] != nil || v.unread[install] {
		return ""
	}
	return install + " is a component the platform itself ships. Its profile is not in the repository, " +
		"so its address and paths are not shown here"
}

// unmatched is why an app instance and an entry name are not something this
// tenant can publish, or "" when they are.
func (v *exposureView) unmatched(install, name string) string {
	if !v.installs[install] {
		return fmt.Sprintf("tenant %s has no app instance named %s installed", v.tenant, install)
	}
	profile := v.profiles[install]
	if profile == nil {
		return fmt.Sprintf("the profile of %s on this cluster could not be read, so what it would publish is not known", install)
	}
	entry, exists, declared := approvableEntry(profile, name)
	switch {
	case entry != nil:
		return ""
	case exists && surfaceOf(profile, name) == gentianov1alpha1.SurfacePerimeter:
		return fmt.Sprintf(
			"entry %s of %s declares a way of checking callers that the platform does not offer on a public address, so nothing is published for it. Its catalog entry has to declare authMode none or authMode app",
			name, install)
	case exists && surfaceOf(profile, name) == gentianov1alpha1.SurfaceInside:
		return fmt.Sprintf(
			"entry %s of %s declares a way of checking callers that the platform does not offer inside the cluster, so nothing is published for it. Its catalog entry has to declare authMode app",
			name, install)
	case exists:
		return fmt.Sprintf(
			"entry %s of %s is not one for the internet and asks for nothing else an approver decides: it is served behind sign-in on the tenant's own address (surface gateway), and approval does not publish it",
			name, install)
	case len(declared) == 0:
		return fmt.Sprintf("%s declares nothing for the internet and asks for nothing else an approver decides", install)
	}
	return fmt.Sprintf("%s declares no entry named %s for the internet, and none of that name that asks for anything else an approver decides. What it declares: %s",
		install, name, strings.Join(declared, ", "))
}

// surfaceOf is the surface of the profile's entry of this name.
func surfaceOf(profile *gentianov1alpha1.ComponentProfile, name string) gentianov1alpha1.SurfaceKind {
	for _, e := range entriesOf(profile) {
		if e.Name == name {
			return e.Surface
		}
	}
	return ""
}

// mainAddressMismatch is why an approval's word on a bare name is not
// the profile's, or "".
//
// A bare name takes two people saying so: the profile's author (apex on
// the entry) and the approver (apex on the approval). The operator publishes
// nothing for one without the other (addresses.PerimeterHost), and an
// approval recorded that way lists as published something that is not. The
// platform tenant's own zone is the one place the operator does not ask, and
// neither does this.
func (v *exposureView) mainAddressMismatch(install, name string, apex bool) string {
	profile := v.profiles[install]
	if profile == nil {
		return ""
	}
	entry, _, _ := approvableEntry(profile, name)
	if entry == nil {
		return ""
	}
	if entry.Surface == gentianov1alpha1.SurfaceInside {
		if apex {
			return fmt.Sprintf(
				"the request asks for a bare name (\"apex\": true), and entry %s of %s answers inside the cluster only: it has no public address. Send the request without \"apex\"",
				name, install)
		}
		return ""
	}
	if entry.Surface != gentianov1alpha1.SurfacePerimeter {
		if apex {
			return fmt.Sprintf(
				"the request asks for a bare name (\"apex\": true), and entry %s of %s is not a public address: it is served behind sign-in. Send the request without \"apex\"",
				name, install)
		}
		return ""
	}
	if entry.Apex == apex {
		return ""
	}
	if addresses.ZoneOf(v.subject, v.inputs.KernelDomain, v.inputs.TenancyMode, v.inputs.KernelRealm).Kernel {
		return ""
	}
	if apex {
		return fmt.Sprintf(
			"the request asks for a bare name (\"apex\": true), and the profile of %s does not declare entry %s for a bare name. "+
				"The platform would publish nothing for it. Send the request without \"apex\" to publish the entry at its own address",
			install, name)
	}
	return fmt.Sprintf(
		"the profile of %s declares entry %s for a bare name, and the request does not ask for one (\"apex\": true is missing). "+
			"The platform would publish nothing for it. Such an entry is approved only as one for a bare name, with \"apex\": true and \"acknowledgeMainAddressRule\": true",
		install, name)
}
