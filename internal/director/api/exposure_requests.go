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

// accessOf says who can reach a perimeter entry, in a sentence.
//
// The publishing proxy checks nobody, whatever the profile says: it forwards
// the declared paths and drops every credential on the way in
// (component_perimeter_config.go). So a mode other than none is the app's
// own check, and the sentence must not read as one the platform makes.
func accessOf(mode gentianov1alpha1.AuthMode) (anyone bool, sentence string) {
	if mode == gentianov1alpha1.AuthModeNone {
		return true, "Reachable by anyone on the internet without sign-in."
	}
	return false, fmt.Sprintf(
		"Nobody signs in at the platform's edge: anyone on the internet can send requests to these paths. "+
			"The app's catalogue entry says the app itself checks each caller (authMode %s); the platform does not check that.",
		mode)
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

// perimeterEntry is the profile's entry of this name for the internet, or
// nil; other names what the profile declares instead, for a refusal.
func perimeterEntry(profile *gentianov1alpha1.ComponentProfile, name string) (entry *gentianov1alpha1.ExposureSpec, gateway bool, declared []string) {
	for i := range profile.Spec.Expose {
		e := &profile.Spec.Expose[i]
		if e.Surface == gentianov1alpha1.SurfacePerimeter {
			declared = append(declared, e.Name)
		}
		if e.Name != name {
			continue
		}
		if e.Surface == gentianov1alpha1.SurfacePerimeter {
			entry = e
		} else {
			gateway = true
		}
	}
	sort.Strings(declared)
	return entry, gateway, declared
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

// describe fills in what an entry would publish.
func (v *exposureView) describe(out *exposureEntry, entry *gentianov1alpha1.ExposureSpec) {
	out.Host, out.Note = v.resolve(out.Install, entry, out.Approval)
	out.Paths = addresses.Prefixes(entry)
	for _, p := range entry.DenyPaths {
		if p = strings.TrimSpace(p); p != "" {
			out.DenyPaths = append(out.DenyPaths, p)
		}
	}
	sort.Strings(out.DenyPaths)
	out.AuthMode = string(entry.AuthMode)
	out.AnyoneWithoutSignIn, out.Access = accessOf(entry.AuthMode)
	out.MainAddress = entry.Apex
	if entry.Apex {
		out.MainAddressRule = mainAddressRule
	}
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
			if entry.Surface != gentianov1alpha1.SurfacePerimeter {
				continue
			}
			e := exposureEntry{Install: install, ExposureName: entry.Name, State: exposureRequested}
			key := gitops.Exposure{Install: install, ExposureName: entry.Name}.Key()
			if approval, ok := approved[key]; ok {
				matched[key] = true
				e.Approval, e.State = approval, stateOf(*approval, v.inputs.Now)
			}
			v.describe(&e, entry)
			out = append(out, e)
		}
	}
	for i := range v.published {
		p := &v.published[i]
		if matched[p.Key()] {
			continue
		}
		e := exposureEntry{Install: p.Install, ExposureName: p.ExposureName, Approval: p, MainAddress: p.Apex}
		if why := v.unknownToTheRepository(p.Install); why != "" {
			// Not shown as matching nothing: what it matches is not the
			// repository's to say.
			e.State, e.Note = stateOf(*p, v.inputs.Now), why
		} else {
			e.State, e.Note = exposureUnmatched, v.unmatched(p.Install, p.ExposureName)+
				". The operator publishes nothing for it; withdraw it to clear the registry"
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
	entry, gateway, declared := perimeterEntry(profile, name)
	switch {
	case entry != nil:
		return ""
	case gateway:
		return fmt.Sprintf(
			"entry %s of %s is not one for the internet: it is served behind sign-in on the tenant's own address (surface gateway), and approval does not publish it",
			name, install)
	case len(declared) == 0:
		return fmt.Sprintf("%s declares nothing for the internet", install)
	}
	return fmt.Sprintf("%s declares no entry named %s for the internet. What it declares: %s",
		install, name, strings.Join(declared, ", "))
}
