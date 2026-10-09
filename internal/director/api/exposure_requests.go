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
// whether anybody signs in -- was in a catalogue file the approver had no
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
// tenant's apps, the profiles in the cluster's catalogue directory, the
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
	// that passes the caller's Authorization header to the app) or
	// signInAppAuthorization (no public address: an entry behind sign-in
	// whose Authorization header is the app's own). KindLabel is the same
	// in a few words, for a list.
	Kind      string `json:"kind,omitempty"`
	KindLabel string `json:"kindLabel,omitempty"`
	// PublicAddress says approving the entry publishes something on the
	// internet. False for an entry behind sign-in.
	PublicAddress bool `json:"publicAddress"`
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
	// Paths are the path prefixes published, and nothing else on the host
	// is. DenyPaths are refused even below one of them.
	Paths     []string `json:"paths,omitempty"`
	DenyPaths []string `json:"denyPaths,omitempty"`
	// AuthMode is the profile's own word for who the app expects to call.
	AuthMode string `json:"authMode,omitempty"`
	// AnyoneWithoutSignIn says the profile declares that nobody signs in
	// (authMode none).
	AnyoneWithoutSignIn bool `json:"anyoneWithoutSignIn"`
	// Access is the same in a sentence, for a person.
	Access string `json:"access,omitempty"`
	// MainAddress says the entry is for the cluster's main address, the
	// bare domain, and MainAddressRule is what the approver has to
	// acknowledge to publish it there.
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
	kindSignInAppAuthorization = gitops.KindSignInAppAuthorization
)

// kindLabels are the kinds in a few words, for a list.
var kindLabels = map[string]string{
	kindPublic:                 "Public address",
	kindPublicAppCredential:    "Public address that passes the caller's credential to the app",
	kindSignInAppAuthorization: "Behind sign-in: keeps the app's own Authorization header",
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
//   - signInAppAuthorization: the session and the bouncer as on any entry
//     behind sign-in; only the header is left alone (internal/bouncer).
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
	case kindSignInAppAuthorization:
		return "This is not a public address. People still have to sign in, and still have to be allowed to use the app, exactly as before. " +
			"Once approved, the platform leaves the Authorization header on requests to this address as the app's own page sent it: " +
			"it no longer replaces or removes that header, and puts no token of its own there. " +
			"The app receives whatever a signed-in person's browser sends in that header, and the app alone checks it. " +
			"Until approved, the header is removed before the app, and calls the app's own pages make with a token of the app's are refused by the app."
	}
	return ""
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
	case kindPublicAppCredential:
		rate = addresses.PerimeterRateFor(true)
		why = " The limit is lower than on other public addresses, because every request may be a guess at a password."
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
	apps, err := s.cfg.Repo.Apps(ctx, tenant)
	if err != nil {
		return nil, err
	}
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
		Now: time.Now(),
	}
	return v, nil
}

// registryEntry is a registry entry as the Tenant carries it, which is what
// the address rule reads.
func registryEntry(e gitops.Exposure) gentianov1alpha1.TenantExposure {
	out := gentianov1alpha1.TenantExposure{
		Install: e.Install, ExposureName: e.ExposureName, Owner: e.Owner, Reason: e.Reason, Apex: e.Apex,
		Kind: gentianov1alpha1.ExposureKind(e.Kind),
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
	out.ExpiresAt, out.PublishedAt = stamp(e.ExpiresAt), stamp(e.PublishedAt)
	return out
}

// approvableEntry is the profile's entry of this name that asks a perimeter
// approver for something, or nil: a perimeter entry in a mode the platform
// serves, or an entry behind sign-in that declares the Authorization header
// the app's own. exists says the profile has an entry of the name at all,
// and declared names the entries that do ask, for a refusal.
func approvableEntry(profile *gentianov1alpha1.ComponentProfile, name string) (entry *gentianov1alpha1.ExposureSpec, exists bool, declared []string) {
	for i := range profile.Spec.Expose {
		e := &profile.Spec.Expose[i]
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
	out.PassesCredential = kind == kindPublicAppCredential || kind == kindSignInAppAuthorization
	out.AuthMode = string(entry.AuthMode)
	out.AnyoneWithoutSignIn = kind == kindPublic
	out.Access, out.RateLimit = accessOf(kind), rateLimitOf(kind)
	for _, p := range entry.DenyPaths {
		if p = strings.TrimSpace(p); p != "" {
			out.DenyPaths = append(out.DenyPaths, p)
		}
	}
	sort.Strings(out.DenyPaths)
	if !out.PublicAddress {
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
	if was == now {
		return ""
	}
	return fmt.Sprintf(
		"This entry was approved by %s as %q, and the app's catalogue entry now asks for %q. "+
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
		for i := range profile.Spec.Expose {
			entry := &profile.Spec.Expose[i]
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
		e.PublicAddress = e.Kind != kindSignInAppAuthorization
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
			"entry %s of %s declares a way of checking callers that the platform does not offer on a public address, so nothing is published for it. Its catalogue entry has to declare authMode none or authMode app",
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
	for i := range profile.Spec.Expose {
		if profile.Spec.Expose[i].Name == name {
			return profile.Spec.Expose[i].Surface
		}
	}
	return ""
}

// mainAddressMismatch is why an approval's word on the main address is not
// the profile's, or "".
//
// The main address takes two people saying so: the profile's author (apex on
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
	if entry.Surface != gentianov1alpha1.SurfacePerimeter {
		if apex {
			return fmt.Sprintf(
				"the request asks for the cluster's main address (\"apex\": true), and entry %s of %s is not a public address: it is served behind sign-in. Send the request without \"apex\"",
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
			"the request asks for the cluster's main address (\"apex\": true), and the profile of %s does not declare entry %s for the main address. "+
				"The platform would publish nothing for it. Send the request without \"apex\" to publish the entry at its own address",
			install, name)
	}
	return fmt.Sprintf(
		"the profile of %s declares entry %s for the cluster's main address, and the request does not ask for the main address (\"apex\": true is missing). "+
			"The platform would publish nothing for it. Such an entry is approved only as one for the main address, with \"apex\": true and \"acknowledgeMainAddressRule\": true",
		install, name)
}
