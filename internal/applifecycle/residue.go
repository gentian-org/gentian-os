/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package applifecycle

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	runtimeschema "k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/layout"
	"github.com/gentian-org/gentian-os/internal/oidc"
	"github.com/gentian-org/gentian-os/internal/profilebundle"
)

// What the cluster's catalogue left behind.
//
// A profile arrives as a bundle: the profile, and the few objects that
// travel with it (profilebundle/bundle.go). The Application that applies
// the catalogue directory does not prune, so nothing a bundle brought is
// taken away when a newer build stops bringing it, and a profile stays when
// the last tenant uninstalled it. That is deliberate -- a tenant moved back
// to the older build finds its pieces -- and it means a cluster collects
// objects that no bundle owns: unverified, and for an OIDC pack still read.
//
// This file is the list of them and the one way to remove one. The list is
// a read. Removing is a command, for one object at a time, and it removes
// only an object that is on the list at the moment it is asked: the list is
// worked out again, from the API server and not from a cache, and a request
// that names anything else is refused with the reason.
//
// What is never on the list: the platform's own Composition, anything a
// chart ships, anything a bundle on this cluster brings, and anything in a
// namespace other than the one the catalogue is applied in.

// The rights the list and the removal need. The four companion kinds are
// listed where they live and deleted one at a time by name; the operator
// already reads each of them for the rollout check.
//
// A Composition decides what exists for an app, so the right to delete one
// is narrowed in code: only one that composes the platform's app, is named
// "app-<profile>", is not "app-default" and is on the list (removeResidue).
//
// +kubebuilder:rbac:groups=apiextensions.crossplane.io,resources=compositions,verbs=get;list;delete
// +kubebuilder:rbac:groups=gentianos.io,resources=oidcpackcatalogs,verbs=get;list;delete
// +kubebuilder:rbac:groups=gentianos.io,resources=customizations,verbs=get;list;delete
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;delete
//
// A profile nobody uses is deleted here too, once Argo CD reports that the
// catalogue directory no longer declares it.
//
// +kubebuilder:rbac:groups=gentianos.io,resources=componentprofiles,verbs=get;list;delete
// +kubebuilder:rbac:groups=gentianos.io,resources=tenants;components,verbs=get;list
// +kubebuilder:rbac:groups=argoproj.io,resources=applications,verbs=get;list

// Why an object is on the list.
const (
	// ResidueDropped: its profile is on the cluster, and the bundle now
	// materialised for that profile does not bring it.
	ResidueDropped = "dropped"
	// ResidueOrphaned: it names a profile that is not on the cluster.
	ResidueOrphaned = "orphaned"
	// ResidueUnowned: no bundle on the cluster owns it and it names no
	// profile that has one -- what copying from before bundles left.
	ResidueUnowned = "unowned"
	// ResidueUnusedProfile: a materialised profile no tenant has installed
	// and no tenant retains data for.
	ResidueUnusedProfile = "unused-profile"
)

// Whether an OIDC pack on the list is still read.
const (
	EffectiveYes       = "yes"
	EffectiveContested = "contested"
	EffectiveNo        = "no"
)

// oidcRule is the rule ResidueOIDC applies, said once for whoever reads the
// answer. It is oidc.ResolvePack's, and the app Composition's.
const oidcRule = "A client is configured from the first OIDCPackCatalog on the cluster that holds a pack under " +
	"its client id, whoever brought the catalog; and an app's Composition reads the OIDCPackCatalog that carries " +
	"its profile's label. So a pack catalog on this list is still in effect (yes) when it is the only one holding " +
	"one of its client ids, or the only one labelled for a profile that is installed; contested when another " +
	"catalog holds the same, and which is read is not defined; and no otherwise."

// catalogueApplication is what the name of the Argo CD Application that
// applies the catalogue directory begins with (kernel/appsets/raw/11b-catalogue.yaml).
const catalogueApplication = "gentian-catalogue-"

var applicationGVK = runtimeschema.GroupVersionKind{Group: "argoproj.io", Version: "v1alpha1", Kind: "Application"}

// catalogueNamespace is where the catalogue is applied, and so where the
// companions that have a namespace live: the destination of the Application
// above. The rollout check looks for them in the same place.
func catalogueNamespace() string { return layout.Namespace(layout.Provisioning) }

// ResidueItem is one object on the list.
type ResidueItem struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
	// Namespace is empty for a kind that has none.
	Namespace string `json:"namespace,omitempty"`
	// Profile is the profile the object names: by its label, or failing
	// that by its name. Empty when it names none.
	Profile string `json:"profile,omitempty"`
	Class   string `json:"class"`
	// Reason says why it is on the list, for a person.
	Reason string `json:"reason"`
	// Created is when the object was created. When it stopped being owned
	// is recorded nowhere on the cluster.
	Created string `json:"created,omitempty"`
	// Removable is whether the removal command would take this object.
	// NotRemovable says why not.
	Removable    bool   `json:"removable"`
	NotRemovable string `json:"notRemovable,omitempty"`
	// OIDC says whether a pack catalog is still read. Only on an
	// OIDCPackCatalog.
	OIDC *ResidueOIDC `json:"oidc,omitempty"`
	// Companions are what an unused profile's bundle brings. They stay when
	// the profile is removed, and are then on this list as orphaned.
	Companions []string `json:"companions,omitempty"`
}

// ResidueOIDC is whether a pack catalog on the list still decides anything.
type ResidueOIDC struct {
	// Effective is yes, contested or no (oidcRule).
	Effective string `json:"effective"`
	// Clients are the client ids only this catalog holds a pack for.
	Clients []string `json:"clients"`
	// Contested are the client ids another catalog holds a pack for too.
	Contested []string `json:"contested"`
	// Composition is the installed profile whose Composition reads this
	// catalog by its label; empty when none does.
	Composition string `json:"composition,omitempty"`
}

// CatalogueResidue is the answer of the read.
type CatalogueResidue struct {
	// Namespace is where the catalogue is applied.
	Namespace string        `json:"catalogueNamespace"`
	Residue   []ResidueItem `json:"residue"`
	// Incomplete says what could not be established, and so what the list
	// may be missing. An object the list is unsure of is left out of it,
	// never put on it.
	Incomplete []string `json:"incomplete"`
	OIDCRule   string   `json:"oidcRule"`
}

// profileState is one profile on the cluster and what its bundle says.
type profileState struct {
	profile *gentianov1alpha1.ComponentProfile
	// bundle is the bundle the profile carries; nil when it carries none or
	// it could not be read (unreadable).
	bundle     *profilebundle.Bundle
	unreadable error
}

// found is an item with the object it was read from.
type found struct {
	item ResidueItem
	obj  client.Object
	gvk  runtimeschema.GroupVersionKind
}

// residueView is the cluster as one derivation saw it.
type residueView struct {
	namespace string
	profiles  map[string]*profileState
	// owned is the profile whose bundle brings each object, by kind/name.
	owned map[string]string
	// inUse are the profiles a tenant has installed or switched on, or a
	// Component runs from.
	inUse map[string]bool
	// useUnknown says a tenant's manifest names an app in a way that does
	// not say which profile it is, so no profile can be called unused.
	useUnknown bool
	// retained are the profiles a tenant retains data for; read only when
	// some profile might be unused, and retainedRead says it was.
	retained     map[string]bool
	retainedRead bool
	items        []found
	incomplete   []string
}

// live is where the list is read from: the API server itself where the
// service was given a reader for it. What is deleted is decided on this
// answer, and a cache answers for a moment ago.
func (s *Service) live() client.Reader {
	if s.opts.LiveReader != nil {
		return s.opts.LiveReader
	}
	return s.client
}

// CatalogueResidue lists what the catalogue left behind.
func (s *Service) CatalogueResidue(ctx context.Context) (*CatalogueResidue, error) {
	view, err := s.residue(ctx, true)
	if err != nil {
		return nil, err
	}
	out := &CatalogueResidue{Namespace: view.namespace, Residue: []ResidueItem{}, Incomplete: view.incomplete, OIDCRule: oidcRule}
	if out.Incomplete == nil {
		out.Incomplete = []string{}
	}
	for _, f := range view.items {
		out.Residue = append(out.Residue, f.item)
	}
	return out, nil
}

// unreadableBundle says what the list is missing for a profile whose bundle
// cannot be read.
func unreadableBundle(profile string, err error) string {
	return fmt.Sprintf(
		"the bundle of ComponentProfile %s cannot be read, so what it owns is not known and nothing that names it is listed: %v",
		profile, err)
}

// residue works the list out. The profiles nobody uses are part of it only
// when asked for: saying so takes reading what every tenant retains.
func (s *Service) residue(ctx context.Context, withUnused bool) (*residueView, error) {
	view := &residueView{
		namespace: catalogueNamespace(),
		profiles:  map[string]*profileState{}, owned: map[string]string{}, inUse: map[string]bool{},
	}
	reader := s.live()

	// Every profile, and what the bundle each carries brings: the bytes the
	// rollout check reads.
	var profiles gentianov1alpha1.ComponentProfileList
	if err := reader.List(ctx, &profiles); err != nil {
		return nil, fmt.Errorf("list component profiles: %w", err)
	}
	for i := range profiles.Items {
		p := &profiles.Items[i]
		state := &profileState{profile: p}
		state.bundle, state.unreadable = profilebundle.Carried(p)
		view.profiles[p.Name] = state
		if state.unreadable != nil {
			view.incomplete = append(view.incomplete, unreadableBundle(p.Name, state.unreadable))
			continue
		}
		if state.bundle == nil {
			continue
		}
		for _, c := range state.bundle.Companions {
			view.owned[c.Kind+"/"+c.Name] = p.Name
		}
	}

	if err := view.readUse(ctx, reader); err != nil {
		return nil, err
	}

	// The objects of the four kinds, where a companion can be: the whole
	// cluster for a kind that has no namespace, the catalogue's namespace
	// for one that has.
	var packs []found
	for _, kind := range profilebundle.CompanionKinds() {
		list := &unstructured.UnstructuredList{}
		list.SetGroupVersionKind(kind.GVK.GroupVersion().WithKind(kind.Kind + "List"))
		var opts []client.ListOption
		if kind.Namespaced {
			opts = append(opts, client.InNamespace(view.namespace))
		}
		if err := reader.List(ctx, list, opts...); err != nil {
			if apimeta.IsNoMatchError(err) || apierrors.IsNotFound(err) {
				// The kind is not served here, so there is none of it.
				continue
			}
			return nil, fmt.Errorf("list %s: %w", kind.Kind, err)
		}
		for i := range list.Items {
			obj := &list.Items[i]
			item, _ := view.classify(kind, obj)
			if item == nil {
				continue
			}
			f := found{item: *item, obj: obj, gvk: kind.GVK}
			if kind.Kind == profilebundle.KindOIDCPacks {
				packs = append(packs, f)
				continue
			}
			view.items = append(view.items, f)
		}
	}
	if len(packs) > 0 {
		if err := view.effective(ctx, reader, packs); err != nil {
			return nil, err
		}
		view.items = append(view.items, packs...)
	}

	if withUnused {
		unused, err := s.unusedProfiles(ctx, reader, view)
		if err != nil {
			return nil, err
		}
		view.items = append(view.items, unused...)
	}

	sort.SliceStable(view.items, func(a, b int) bool {
		x, y := view.items[a].item, view.items[b].item
		if x.Kind != y.Kind {
			return x.Kind < y.Kind
		}
		return x.Name < y.Name
	})
	return view, nil
}

// readUse records which profiles are in use: named by a tenant's manifest,
// as an app or as an add-on, or by a Component in any namespace. A profile
// every tenant gets is in use without being named anywhere.
func (v *residueView) readUse(ctx context.Context, reader client.Reader) error {
	var tenants gentianov1alpha1.TenantList
	if err := reader.List(ctx, &tenants); err != nil {
		return fmt.Errorf("list tenants: %w", err)
	}
	for i := range tenants.Items {
		for _, app := range tenants.Items[i].Spec.Apps {
			v.inUse[app.Profile] = true
			if app.ProfileRef != nil {
				if app.ProfileRef.Name != "" {
					v.inUse[app.ProfileRef.Name] = true
				} else if app.Profile == "" {
					v.useUnknown = true
					v.incomplete = append(v.incomplete, fmt.Sprintf(
						"tenant %s has an app that names no profile by name, so which profiles are unused is not known and none is listed",
						tenants.Items[i].Name))
				}
			}
			for _, addon := range app.Addons {
				v.inUse[addon] = true
			}
			for _, pin := range app.AddonPins {
				v.inUse[pin.Name] = true
			}
		}
	}
	var components gentianov1alpha1.ComponentList
	if err := reader.List(ctx, &components); err != nil {
		return fmt.Errorf("list components: %w", err)
	}
	for i := range components.Items {
		c := &components.Items[i]
		v.inUse[c.Spec.ProfileRef.Name] = true
		for _, addon := range c.Spec.Addons {
			v.inUse[addon] = true
		}
		for _, pin := range c.Spec.AddonPins {
			v.inUse[pin.Name] = true
		}
	}
	delete(v.inUse, "")
	return nil
}

// classify says whether one object of a companion kind is on the list, and
// why not when it is not.
func (v *residueView) classify(kind profilebundle.CompanionKind, obj *unstructured.Unstructured) (*ResidueItem, string) {
	name, labels := obj.GetName(), obj.GetLabels()
	if owner := v.owned[kind.Kind+"/"+name]; owner != "" {
		return nil, fmt.Sprintf("the bundle now materialised for profile %s brings it", owner)
	}
	shape, shaped := profilebundle.ShapeOwner(kind.Kind, name)
	removable, notRemovable := true, ""
	switch kind.Kind {
	case profilebundle.KindComposition:
		if profilebundle.PlatformComposition(name) {
			return nil, "it is the platform's own Composition, which renders every app that brings none"
		}
		if !profilebundle.ComposesApp(obj.Object) {
			return nil, "it does not compose the platform's app, and only an app's Composition is ever a bundle's"
		}
		if !shaped {
			removable = false
			notRemovable = "only a Composition named app-<profile> is removed here; this one is deleted by hand, if at all"
		}
	case profilebundle.KindOIDCPacks:
		packs, _, _ := unstructured.NestedMap(obj.Object, "spec", "packs")
		for _, pack := range packs {
			if p, _ := pack.(map[string]any); p["serviceClient"] == true {
				return nil, "it declares a service client, which only the platform's own pack catalog does"
			}
		}
	case profilebundle.KindConfigMap:
		if labels[profilebundle.ProfileLabel] == "" && labels[profilebundle.AssetLabel] == "" {
			return nil, "it carries neither a profile's label nor an asset's, so it is not something a bundle brought"
		}
	case profilebundle.KindCustomization:
		if labels[profilebundle.ProfileLabel] == "" {
			return nil, "it carries no profile's label, so it is not something a bundle brought"
		}
	}
	if kind.Namespaced && obj.GetNamespace() != v.namespace {
		return nil, fmt.Sprintf("it is in %s, and a bundle's objects are in %s or have no namespace", obj.GetNamespace(), v.namespace)
	}
	if labels["app.kubernetes.io/managed-by"] == "Helm" {
		return nil, "a chart ships it (Helm manages it)"
	}
	if len(obj.GetOwnerReferences()) > 0 {
		return nil, "another object owns it"
	}
	if obj.GetDeletionTimestamp() != nil {
		return nil, "it is already being deleted"
	}

	item := &ResidueItem{
		Kind: kind.Kind, Name: name, Namespace: obj.GetNamespace(),
		Removable: removable, NotRemovable: notRemovable, Created: created(obj),
	}
	claimed := labels[profilebundle.ProfileLabel]
	by := fmt.Sprintf("it carries the label %s: %s", profilebundle.ProfileLabel, claimed)
	if claimed == "" && shaped && v.profiles[shape] != nil {
		claimed = shape
		by = fmt.Sprintf("its name is the one a %s of profile %s has", kind.Kind, shape)
	}
	if claimed == "" {
		item.Class = ResidueUnowned
		item.Reason = "not owned by any bundle: it names no profile, and no bundle on this cluster brings it"
		return item, ""
	}
	item.Profile = claimed
	state := v.profiles[claimed]
	switch {
	case state == nil:
		item.Class = ResidueOrphaned
		item.Reason = fmt.Sprintf("%s, and no profile %s is on this cluster", by, claimed)
	case state.unreadable != nil:
		return nil, fmt.Sprintf("the bundle of profile %s cannot be read, so whether it brings this is not known", claimed)
	case state.bundle == nil:
		item.Class = ResidueUnowned
		item.Reason = fmt.Sprintf("not owned by any bundle: %s, and profile %s is on this cluster without a bundle", by, claimed)
	default:
		item.Class = ResidueDropped
		item.Reason = fmt.Sprintf("%s, and the bundle now materialised for %s does not bring it", by, claimed)
	}
	return item, ""
}

func created(obj client.Object) string {
	if t := obj.GetCreationTimestamp(); !t.IsZero() {
		return t.UTC().Format(time.RFC3339)
	}
	return ""
}

// effective says, for each pack catalog on the list, whether it is still
// read, by the rule of oidc.ResolvePack and of the app Composition.
func (v *residueView) effective(ctx context.Context, reader client.Reader, packs []found) error {
	holders, err := oidc.PackHolders(ctx, reader)
	if err != nil {
		return err
	}
	// How many pack catalogs carry each profile's label: a Composition asks
	// for the one that carries its profile's.
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(packs[0].gvk.GroupVersion().WithKind(profilebundle.KindOIDCPacks + "List"))
	if err := reader.List(ctx, list); err != nil {
		return fmt.Errorf("list %s: %w", profilebundle.KindOIDCPacks, err)
	}
	labelled := map[string]int{}
	for i := range list.Items {
		if profile := list.Items[i].GetLabels()[profilebundle.ProfileLabel]; profile != "" {
			labelled[profile]++
		}
	}
	for i := range packs {
		obj := packs[i].obj.(*unstructured.Unstructured)
		state := &ResidueOIDC{Effective: EffectiveNo, Clients: []string{}, Contested: []string{}}
		held, _, _ := unstructured.NestedMap(obj.Object, "spec", "packs")
		for clientID := range held {
			if len(holders[clientID]) > 1 {
				state.Contested = append(state.Contested, clientID)
			} else {
				state.Clients = append(state.Clients, clientID)
			}
		}
		sort.Strings(state.Clients)
		sort.Strings(state.Contested)
		sole := len(state.Clients) > 0
		contested := len(state.Contested) > 0
		if profile := obj.GetLabels()[profilebundle.ProfileLabel]; profile != "" && v.inUse[profile] {
			state.Composition = profile
			if labelled[profile] > 1 {
				contested = true
			} else {
				sole = true
			}
		}
		switch {
		case sole:
			state.Effective = EffectiveYes
		case contested:
			state.Effective = EffectiveContested
		}
		packs[i].item.OIDC = state
	}
	return nil
}

// unusedProfiles lists the materialised profiles nobody uses: no tenant has
// one installed or switched on, no Component runs from one, and no tenant
// retains data for one. Whether data is retained is the retained-data read's
// to say (retained.go) and is asked of it, tenant by tenant; where it could
// not say for certain, no profile is listed.
func (s *Service) unusedProfiles(ctx context.Context, reader client.Reader, view *residueView) ([]found, error) {
	var candidates []*profileState
	for _, state := range view.profiles {
		p := state.profile
		switch {
		case strings.TrimSpace(p.Annotations[profilebundle.Annotation]) == "":
			// Not materialised from a catalogue: the platform's chart
			// shipped it, or the installer wrote it.
		case p.Labels["app.kubernetes.io/managed-by"] == "Helm", p.Annotations[platformAppAnnotation] == "true":
		case p.Spec.DefaultForTenants, p.Spec.DefaultForPlatform:
		case p.DeletionTimestamp != nil, len(p.OwnerReferences) > 0:
		case view.inUse[p.Name]:
		default:
			candidates = append(candidates, state)
		}
	}
	if len(candidates) == 0 || view.useUnknown {
		return nil, nil
	}

	var tenants gentianov1alpha1.TenantList
	if err := reader.List(ctx, &tenants); err != nil {
		return nil, fmt.Errorf("list tenants: %w", err)
	}
	retained := map[string]bool{}
	view.retained = retained
	for i := range tenants.Items {
		tenant := tenants.Items[i].Name
		held, err := s.RetainedApps(ctx, tenant)
		if err != nil {
			view.incomplete = append(view.incomplete, fmt.Sprintf(
				"unused profiles are not listed: what tenant %s retains could not be read: %v", tenant, err))
			return nil, nil
		}
		if len(held.Unknown) > 0 {
			kinds := make([]string, 0, len(held.Unknown))
			for kind := range held.Unknown {
				kinds = append(kinds, kind)
			}
			sort.Strings(kinds)
			view.incomplete = append(view.incomplete, fmt.Sprintf(
				"unused profiles are not listed: whether tenant %s retains data could not be established for %s (%s)",
				tenant, strings.Join(kinds, ", "), held.Unknown[kinds[0]]))
			return nil, nil
		}
		for _, app := range held.Apps {
			retained[app.Profile] = true
		}
	}

	view.retainedRead = true

	var out []found
	for _, state := range candidates {
		p := state.profile
		if retained[p.Name] {
			continue
		}
		item := ResidueItem{
			Kind: profilebundle.KindProfile, Name: p.Name, Profile: p.Name, Class: ResidueUnusedProfile,
			Reason: "unused profile: no tenant has it installed or switched on as an add-on, " +
				"and no tenant retains data for it",
			Created: created(p), Removable: true,
		}
		if state.bundle != nil {
			for _, c := range state.bundle.Companions {
				item.Companions = append(item.Companions, c.String())
			}
		}
		out = append(out, found{item: item, obj: p, gvk: gentianov1alpha1.GroupVersion.WithKind(profilebundle.KindProfile)})
	}
	return out, nil
}

// ── Removal ─────────────────────────────────────────────────────────────────

// ErrNotResidue is a removal asked of an object that is not on the list.
// Nothing is deleted.
var ErrNotResidue = errors.New("not on the residue list")

// ErrStillDeclared is a removal of an object Argo CD still finds declared
// in the catalogue directory, or cannot say is not. Nothing is deleted.
var ErrStillDeclared = errors.New("still declared")

// ErrResidueChanged is a removal that lost a race: the object changed, or
// went, between the check and the deletion. Nothing was deleted by it.
var ErrResidueChanged = errors.New("changed while it was being removed")

// ErrNotARemovableKind is a removal that names a kind this never removes.
var ErrNotARemovableKind = errors.New("not a kind that is removed here")

// RemoveResidueResult is the answer of a removal that did something.
type RemoveResidueResult struct {
	// Status is "deleted" when the object is gone, and "deleting" when the
	// API server took the deletion and a finalizer still holds the object.
	Status  string       `json:"status"`
	Deleted *ResidueItem `json:"deleted"`
	Message string       `json:"message"`
}

// RemoveResidue deletes one object that is on the residue list, and only
// such an object.
//
// The list is worked out again here, from the API server. A request for
// anything that is not on it -- a companion a bundle brings, the platform's
// Composition, a ConfigMap that is nobody's companion, an object a new
// install adopted a moment ago -- is refused with the reason, and so is one
// for an object Argo CD still finds declared in the catalogue directory:
// whatever is declared there would be applied again, and deleting it would
// report something that did not stay done.
//
// An unused profile is removed from the deployments repository by the
// director first; it is deleted here only once Argo CD reports that the
// directory no longer declares it.
//
// The deletion names the object as it was checked, by its identifier and
// its version, so anything that touched it in between makes the API server
// refuse.
func (s *Service) RemoveResidue(ctx context.Context, kind, name, namespace, actor string) (*RemoveResidueResult, error) {
	return s.removeResidue(ctx, kind, name, namespace, actor, nil)
}

// removeResidue is the one removal there is. With a scope it removes for a
// tenant's administrator, which is the same removal with more refused
// (residue_app.go).
func (s *Service) removeResidue(ctx context.Context, kind, name, namespace, actor string, scope *ResidueScope) (*RemoveResidueResult, error) {
	removable := kind == profilebundle.KindProfile
	namespaced := false
	for _, k := range profilebundle.CompanionKinds() {
		if k.Kind == kind {
			removable, namespaced = true, k.Namespaced
		}
	}
	if !removable {
		return nil, fmt.Errorf("%w: %q; the kinds are Composition, OIDCPackCatalog, ConfigMap, Customization and ComponentProfile",
			ErrNotARemovableKind, kind)
	}
	if name == "" {
		return nil, fmt.Errorf("%w: no name was given", ErrNotARemovableKind)
	}
	where := catalogueNamespace()
	if namespace != "" && (!namespaced || namespace != where) {
		if !namespaced {
			return nil, fmt.Errorf("%w: a %s has no namespace. Nothing was deleted", ErrNotResidue, kind)
		}
		return nil, fmt.Errorf("%w: a bundle's objects are in %s, and nothing in another namespace is ever removed here. "+
			"Nothing was deleted", ErrNotResidue, where)
	}

	s.residueMu.Lock()
	defer s.residueMu.Unlock()

	if scope != nil {
		if err := s.admitsScope(ctx, scope, kind, name); err != nil {
			return nil, err
		}
	}
	view, err := s.residue(ctx, true)
	if err != nil {
		return nil, err
	}
	var target *found
	for i := range view.items {
		if item := view.items[i].item; item.Kind == kind && item.Name == name {
			target = &view.items[i]
		}
	}
	if scope != nil && (target == nil || !scope.holds(target.item)) {
		// Said without the reason the cluster's administrator is given: why
		// an object is not this app's leftover can name another profile.
		return nil, fmt.Errorf("%w: the %s %s is not something a newer build of %s left behind, as this cluster lists it "+
			"now. Nothing was deleted; ask for the list again", ErrNotResidue, kind, name, scope.Profile)
	}
	if target == nil {
		return nil, fmt.Errorf("%w: %s. Nothing was deleted", ErrNotResidue, s.whyNot(ctx, view, kind, name))
	}
	if !target.item.Removable {
		return nil, fmt.Errorf("%w: the %s %s is listed and is not removed here: %s. Nothing was deleted",
			ErrNotResidue, kind, name, target.item.NotRemovable)
	}
	// Said again where the right is used: a Composition is deleted only
	// under a name a bundle gives one, and never the platform's.
	if kind == profilebundle.KindComposition {
		if _, shaped := profilebundle.ShapeOwner(kind, name); !shaped || profilebundle.PlatformComposition(name) {
			return nil, fmt.Errorf("%w: the Composition %s is not one a bundle brings. Nothing was deleted", ErrNotResidue, name)
		}
	}
	if err := s.notDeclared(ctx, target); err != nil {
		return nil, err
	}

	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(target.gvk)
	obj.SetName(name)
	obj.SetNamespace(target.item.Namespace)
	uid, version := target.obj.GetUID(), target.obj.GetResourceVersion()
	err = s.client.Delete(ctx, obj, client.Preconditions{UID: &uid, ResourceVersion: &version})
	switch {
	case apierrors.IsConflict(err), apierrors.IsNotFound(err):
		return nil, fmt.Errorf("%w: the %s %s was changed or removed by something else between the check and the "+
			"deletion. Nothing was deleted by this request; ask for the list again", ErrResidueChanged, kind, name)
	case err != nil:
		return nil, fmt.Errorf("delete %s %s: %w", kind, name, err)
	}
	log.FromContext(ctx).WithName("catalogue-residue").Info("removed catalogue residue",
		"kind", kind, "name", name, "namespace", target.item.Namespace, "class", target.item.Class, "actor", actor)

	// Said only when it is so: the object is looked for again.
	left := &unstructured.Unstructured{}
	left.SetGroupVersionKind(target.gvk)
	err = s.live().Get(ctx, types.NamespacedName{Name: name, Namespace: target.item.Namespace}, left)
	item := target.item
	switch {
	case apierrors.IsNotFound(err):
		return &RemoveResidueResult{Status: "deleted", Deleted: &item,
			Message: fmt.Sprintf("the %s %s was deleted, asked for by %s", kind, name, actor)}, nil
	case err != nil:
		return nil, fmt.Errorf("the %s %s was asked to be deleted, and whether it is gone could not be read: %w", kind, name, err)
	case left.GetUID() != uid:
		return &RemoveResidueResult{Status: "deleted", Deleted: &item, Message: fmt.Sprintf(
			"the %s %s was deleted, asked for by %s; an object of the same name has been created since", kind, name, actor)}, nil
	}
	return &RemoveResidueResult{Status: "deleting", Deleted: &item, Message: fmt.Sprintf(
		"the API server took the deletion of the %s %s and still holds the object (finalizers: %s); it goes when they are done",
		kind, name, strings.Join(left.GetFinalizers(), ", "))}, nil
}

// whyNot says why an object named in a removal is not on the list.
func (s *Service) whyNot(ctx context.Context, view *residueView, kind, name string) string {
	if kind == profilebundle.KindProfile {
		state := view.profiles[name]
		switch {
		case state == nil:
			return fmt.Sprintf("there is no ComponentProfile %s on this cluster", name)
		case strings.TrimSpace(state.profile.Annotations[profilebundle.Annotation]) == "":
			return fmt.Sprintf("the ComponentProfile %s was not materialised from a catalogue: the platform ships it, or the installer wrote it", name)
		case view.inUse[name]:
			return fmt.Sprintf("the ComponentProfile %s is in use: a tenant has it installed or switched on as an add-on", name)
		case state.profile.Spec.DefaultForTenants || state.profile.Spec.DefaultForPlatform:
			return fmt.Sprintf("the ComponentProfile %s is one every tenant gets", name)
		case view.retained[name]:
			return fmt.Sprintf("the ComponentProfile %s is not unused: a tenant retains data for it", name)
		case !view.retainedRead && len(view.incomplete) > 0:
			return fmt.Sprintf("the ComponentProfile %s is not shown to be unused: %s", name, strings.Join(view.incomplete, "; "))
		}
		return fmt.Sprintf("the ComponentProfile %s is not listed as an unused profile", name)
	}
	for _, k := range profilebundle.CompanionKinds() {
		if k.Kind != kind {
			continue
		}
		obj := &unstructured.Unstructured{}
		obj.SetGroupVersionKind(k.GVK)
		key := types.NamespacedName{Name: name}
		if k.Namespaced {
			key.Namespace = view.namespace
		}
		if err := s.live().Get(ctx, key, obj); err != nil {
			if apierrors.IsNotFound(err) || apimeta.IsNoMatchError(err) {
				return fmt.Sprintf("there is no %s %s where a bundle's objects are", kind, name)
			}
			return fmt.Sprintf("the %s %s could not be read: %v", kind, name, err)
		}
		if _, why := view.classify(k, obj); why != "" {
			return fmt.Sprintf("the %s %s is not residue: %s", kind, name, why)
		}
	}
	return fmt.Sprintf("the %s %s is not on the residue list", kind, name)
}

// notDeclared asks Argo CD whether the catalogue directory still declares
// an object, and answers ErrStillDeclared when it does or when that cannot
// be told.
//
// The Application that applies the directory lists every object it tracks,
// and marks the ones that are on the cluster and no longer in the directory
// as requiring pruning -- which it never does itself. An object it lists
// without that mark is declared, and is not deleted. One it does not list
// at all was not applied by it: that is the ordinary case for what copying
// from before bundles left, and such an object is deleted. A profile is the
// exception: it reaches the cluster only through that Application, so one
// the Application does not report as requiring pruning stays.
func (s *Service) notDeclared(ctx context.Context, target *found) error {
	apps := &unstructured.UnstructuredList{}
	apps.SetGroupVersionKind(applicationGVK.GroupVersion().WithKind("ApplicationList"))
	if err := s.live().List(ctx, apps, client.InNamespace(layout.Namespace(layout.GitOps))); err != nil {
		return fmt.Errorf("%w: Argo CD's Application for the catalogue could not be read, so whether the %s %s is still "+
			"declared is not known (%v). Nothing was deleted", ErrStillDeclared, target.item.Kind, target.item.Name, err)
	}
	seen, prunable := false, false
	for i := range apps.Items {
		app := &apps.Items[i]
		if !strings.HasPrefix(app.GetName(), catalogueApplication) {
			continue
		}
		seen = true
		resources, _, _ := unstructured.NestedSlice(app.Object, "status", "resources")
		for _, raw := range resources {
			r, _ := raw.(map[string]any)
			group, _ := r["group"].(string)
			if group != target.gvk.Group || r["kind"] != target.item.Kind || r["name"] != target.item.Name {
				continue
			}
			if ns, _ := r["namespace"].(string); ns != target.item.Namespace {
				continue
			}
			if r["requiresPruning"] != true {
				return fmt.Errorf("%w: Argo CD (%s) finds the %s %s declared in the catalogue directory as it last "+
					"compared it, and would apply it again. Nothing was deleted", ErrStillDeclared,
					app.GetName(), target.item.Kind, target.item.Name)
			}
			prunable = true
		}
	}
	if !seen {
		return fmt.Errorf("%w: Argo CD has no Application %s* in %s, so whether the %s %s is still declared is not "+
			"known. Nothing was deleted", ErrStillDeclared, catalogueApplication, layout.Namespace(layout.GitOps),
			target.item.Kind, target.item.Name)
	}
	if target.item.Kind == profilebundle.KindProfile && !prunable {
		return fmt.Errorf("%w: Argo CD does not yet report the ComponentProfile %s as gone from the catalogue directory. "+
			"It is deleted once Argo CD has synced the commit that removed it; nothing was deleted now",
			ErrStillDeclared, target.item.Name)
	}
	return nil
}
