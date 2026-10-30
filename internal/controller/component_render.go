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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	utiljson "k8s.io/apimachinery/pkg/util/json"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/apprender"
	"github.com/gentian-org/gentian-os/internal/layout"
)

// What a component's chart is installed with is rendered by apprender and
// applied here: the release, the Secrets its values read, its registration
// at the tenant's realm, its post-install job, its extensions. Each object
// carries the component's labels; one in the component's namespace is owned
// by it, and the rest -- cluster-scoped, or in a kernel namespace -- are
// found again through the component's inventory (renderedAnnotation), which
// is how an object no longer rendered is removed and how all of them go with
// the component.

// What a render makes outside the component's namespace: its registration at
// the tenant's realm. And the realm's redirect rule a request-bound client is
// registered against.
// +kubebuilder:rbac:groups=openidclient.keycloak.crossplane.io,resources=clients;clientscopes;clientdefaultscopes;clientoptionalscopes,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=client.keycloak.crossplane.io,resources=protocolmappers,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=group.keycloak.crossplane.io,resources=groups;roles,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=role.keycloak.crossplane.io,resources=roles,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=samlclient.keycloak.crossplane.io,resources=clients,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=realm.keycloak.crossplane.io,resources=clientpolicyprofilepolicies,verbs=get;list;watch

// renderedAnnotation lists, on the Component, every object applied for it
// outside its own namespace's owner references: one "apiVersion|kind|
// namespace|name" per object, sorted.
const renderedAnnotation = "gentianos.io/rendered"

// takenAnnotation lists, like renderedAnnotation, what a component's handoff
// took over from the composite it was installed as, outside its namespace's
// owner references. It is removed with the component, and never because a
// render does not name it: what the render and the Composition name
// differently is left where it is rather than deleted.
const takenAnnotation = "gentianos.io/taken-over"

// compositeAnnotation names, on a Component the platform's former app
// Composition installed, the composite it was installed as. The objects taken
// over from it keep their names, and the release and its keep rule are named
// after it as the Composition named them (renderNames).
const compositeAnnotation = "gentianos.io/composite"

// renderNames are the names of the release object, the Helm release it
// installs, the ConfigMap its keep rule is read from, and the name the rest
// of what the Composition named after its composite is named after.
func renderNames(comp *gentianov1alpha1.Component) (releaseObject, helmRelease, keepVolumes, composite string) {
	if xr := comp.Annotations[compositeAnnotation]; xr != "" {
		helm := comp.Name + composedReleaseSuffix
		return xr + composedReleaseSuffix, helm, helm + "-keep-volumes", xr
	}
	return releaseName(comp), releaseName(comp), keepVolumesName(comp), releaseName(comp)
}

// renderInput gathers what one component is rendered from.
func (r *ComponentReconciler) renderInput(
	ctx context.Context, comp *gentianov1alpha1.Component, profile *gentianov1alpha1.ComponentProfile,
	tenant *gentianov1alpha1.Tenant, zone edgeZone, pull pullSecrets, mailboxTokenSignIn bool,
	signIn *signInSidecar, operatorValues map[string]interface{},
) (*apprender.Input, error) {
	releaseObject, helmRelease, keepVolumes, composite := renderNames(comp)
	in := &apprender.Input{
		App:                comp.Name,
		Namespace:          comp.Namespace,
		Realm:              keycloakRealmName(tenant),
		Domain:             zone.domain,
		ReleaseObject:      releaseObject,
		HelmRelease:        helmRelease,
		KeepVolumes:        keepVolumes,
		Composite:          composite,
		ProfileName:        comp.Spec.ProfileRef.Name,
		Profile:            &profile.Spec,
		MailboxTokenSignIn: mailboxTokenSignIn,
		Observed:           map[string]*unstructured.Unstructured{},
	}
	// The profile's own annotations, not a build's: they say what the app is
	// across every build of it.
	own := &gentianov1alpha1.ComponentProfile{}
	if err := r.Get(ctx, types.NamespacedName{Name: comp.Spec.ProfileRef.Name}, own); err != nil {
		return nil, err
	}
	in.ProfileAnnotations = own.Annotations
	in.PullSecrets.Images = append(in.PullSecrets.Images, pull.images...)
	for _, c := range pull.charts {
		in.PullSecrets.Charts = append(in.PullSecrets.Charts, apprender.ChartPullSecret{Repository: c.repository, SecretName: c.secretName})
	}
	if signIn != nil {
		in.SignInSidecar = signIn.claim
	}
	ids, err := r.addonIDs(ctx, comp)
	if err != nil {
		return nil, err
	}
	in.Addons = ids
	values := map[string]interface{}{}
	// A placed component's configuration is the platform's: it was never
	// read from the Component, and is not now.
	if cfg := comp.Spec.Config; cfg != nil && !operatorPlaced(profile) {
		if cfg.Replicas != nil {
			replicas := int64(*cfg.Replicas)
			in.Replicas = &replicas
		}
		if cfg.ExtraValues != nil && len(cfg.ExtraValues.Raw) > 0 {
			if err := decodeJSONObject(cfg.ExtraValues.Raw, &values); err != nil {
				return nil, fmt.Errorf("config.extraValues is not an object: %w", err)
			}
		}
	}
	// What the operator tells the chart lies over the component's own
	// values, which cannot override the platform's facts.
	mergeValues(values, operatorValues)
	if len(values) > 0 {
		normalized, err := jsonNormalized(values)
		if err != nil {
			return nil, fmt.Errorf("values: %w", err)
		}
		in.ExtraValues = normalized
	}

	clusterConfig := &corev1.ConfigMap{}
	if err := r.Get(ctx, types.NamespacedName{Name: clusterConfigName, Namespace: layout.Namespace(layout.Provisioning)}, clusterConfig); err != nil && !errors.IsNotFound(err) {
		return nil, err
	}
	in.ClusterConfig = map[string]string{}
	for k, v := range clusterConfig.Data {
		in.ClusterConfig[k] = v
	}
	// Where the credential probes go is the kernel's layout, which the
	// operator knows whether or not the claim wrote it down.
	if in.ClusterConfig["kernel.controlNamespace"] == "" {
		in.ClusterConfig["kernel.controlNamespace"] = layout.Namespace(layout.Control)
	}
	in.ClusterConfigNamespace = layout.Namespace(layout.Provisioning)
	kernelServices := &corev1.ConfigMap{}
	if err := r.Get(ctx, types.NamespacedName{Name: kernelServicesName, Namespace: layout.Namespace(layout.Control)}, kernelServices); err != nil && !errors.IsNotFound(err) {
		return nil, err
	}
	in.KernelServices = kernelServices.Data

	packs := &gentianov1alpha1.OIDCPackCatalogList{}
	if err := r.List(ctx, packs, client.MatchingLabels{"gentianos.io/profile-name": comp.Spec.ProfileRef.Name}); err != nil && !apimeta.IsNoMatchError(err) {
		return nil, err
	}
	if len(packs.Items) > 0 {
		in.Packs = &packs.Items[0].Spec
	}
	policies := &unstructured.UnstructuredList{}
	policies.SetGroupVersionKind(redirectPolicyGVK.GroupVersion().WithKind(redirectPolicyGVK.Kind + "List"))
	if err := r.List(ctx, policies, client.MatchingLabels{
		"gentianos.io/realm-rule":       "request-bound-redirect",
		"gentianos.io/tenant-namespace": comp.Namespace,
	}); err != nil && !apimeta.IsNoMatchError(err) {
		return nil, err
	}
	if len(policies.Items) > 0 {
		in.RedirectPolicy = &policies.Items[0]
	}
	return in, nil
}

// kernelServicesName is the ConfigMap of the kernel's service endpoints a
// chart's values may name.
const kernelServicesName = "gentian-kernel-services"

// renderedLabel marks an object in the component's namespace as rendered for
// it, which is what lets a pass remove one no longer rendered: the component
// owns other objects there that are not rendered.
const renderedLabel = "gentianos.io/rendered"

// redirectPolicyGVK is the tenant realm's client policy a request-bound
// client is registered against.
var redirectPolicyGVK = schema.GroupVersionKind{
	Group: "realm.keycloak.crossplane.io", Version: "v1alpha1", Kind: "ClientPolicyProfilePolicy",
}

// jsonNormalized passes values through JSON, so they hold only what the API
// server would give back: an int read back as int64 would otherwise differ
// from the int written, and the object be patched on every pass.
func jsonNormalized(values map[string]interface{}) (map[string]interface{}, error) {
	raw, err := json.Marshal(values)
	if err != nil {
		return nil, err
	}
	out := map[string]interface{}{}
	if err := utiljson.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// render renders a component: once to learn what it is made of, and again
// with those objects as they are now, which a few parts depend on (an id the
// realm assigned; an object that goes on being rendered once it exists).
func (r *ComponentReconciler) render(ctx context.Context, in *apprender.Input) (*apprender.Rendered, error) {
	first, err := apprender.Render(in)
	if err != nil {
		return nil, err
	}
	for _, ref := range append(apprender.Consulted(in), first.Objects...) {
		live, err := r.liveCopy(ctx, ref)
		if err != nil {
			return nil, err
		}
		if live != nil {
			in.Observed[live.GetName()] = live
		}
	}
	return apprender.Render(in)
}

// liveCopy reads the object a rendered one names, nil when there is none.
func (r *ComponentReconciler) liveCopy(ctx context.Context, ref *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	live := &unstructured.Unstructured{}
	live.SetGroupVersionKind(ref.GroupVersionKind())
	err := r.Get(ctx, types.NamespacedName{Name: ref.GetName(), Namespace: ref.GetNamespace()}, live)
	if errors.IsNotFound(err) || apimeta.IsNoMatchError(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return live, nil
}

func inventoryKey(obj *unstructured.Unstructured) string {
	return strings.Join([]string{obj.GetAPIVersion(), obj.GetKind(), obj.GetNamespace(), obj.GetName()}, "|")
}

func inventoryObject(key string) (*unstructured.Unstructured, bool) {
	parts := strings.Split(key, "|")
	if len(parts) != 4 {
		return nil, false
	}
	obj := &unstructured.Unstructured{}
	obj.SetAPIVersion(parts[0])
	obj.SetKind(parts[1])
	obj.SetNamespace(parts[2])
	obj.SetName(parts[3])
	return obj, true
}

func inventoryOf(comp *gentianov1alpha1.Component) []string {
	return keysIn(comp, renderedAnnotation)
}

func takenOf(comp *gentianov1alpha1.Component) []string {
	return keysIn(comp, takenAnnotation)
}

func keysIn(comp *gentianov1alpha1.Component, annotation string) []string {
	raw := comp.Annotations[annotation]
	if raw == "" {
		return nil
	}
	var keys []string
	if err := json.Unmarshal([]byte(raw), &keys); err != nil {
		return nil
	}
	return keys
}

// recordInventory writes the inventory onto the Component.
func (r *ComponentReconciler) recordInventory(ctx context.Context, comp *gentianov1alpha1.Component, keys []string) error {
	return r.recordKeys(ctx, comp, renderedAnnotation, keys)
}

// recordTaken writes what the handoff took over onto the Component.
func (r *ComponentReconciler) recordTaken(ctx context.Context, comp *gentianov1alpha1.Component, keys []string) error {
	return r.recordKeys(ctx, comp, takenAnnotation, keys)
}

func (r *ComponentReconciler) recordKeys(ctx context.Context, comp *gentianov1alpha1.Component, annotation string, keys []string) error {
	sort.Strings(keys)
	keys = compactStrings(keys)
	raw := ""
	if len(keys) > 0 {
		b, err := json.Marshal(keys)
		if err != nil {
			return err
		}
		raw = string(b)
	}
	if comp.Annotations[annotation] == raw {
		return nil
	}
	// On a copy: the patch's answer would replace what this pass has already
	// worked out of the status with the stored one.
	marked := comp.DeepCopy()
	patch := client.MergeFrom(marked.DeepCopy())
	if raw == "" {
		delete(marked.Annotations, annotation)
	} else {
		if marked.Annotations == nil {
			marked.Annotations = map[string]string{}
		}
		marked.Annotations[annotation] = raw
	}
	if err := r.Patch(ctx, marked, patch); err != nil {
		return err
	}
	if comp.Annotations == nil {
		comp.Annotations = map[string]string{}
	}
	comp.Annotations[annotation] = raw
	comp.ResourceVersion = marked.ResourceVersion
	return nil
}

func sortedUnion(a, b []string) []string {
	out := append(append([]string{}, a...), b...)
	sort.Strings(out)
	return out
}

func compactStrings(sorted []string) []string {
	out := sorted[:0]
	for i, s := range sorted {
		if i == 0 || s != sorted[i-1] {
			out = append(out, s)
		}
	}
	return out
}

// ownedHere reports whether an object lives where the component's owner
// reference reaches it.
func ownedHere(comp *gentianov1alpha1.Component, obj *unstructured.Unstructured) bool {
	return obj.GetNamespace() == comp.Namespace
}

// isRelease reports whether an object is a provider-helm Release.
func isRelease(obj *unstructured.Unstructured) bool {
	return obj.GroupVersionKind() == helmReleaseGVK
}

// applyRendered applies what a component was rendered into, and removes what
// it no longer is. The releases come last, once the Secret their values read
// has arrived: a set entry naming a Secret that is not there yet fails the
// release rather than waiting for it.
func (r *ComponentReconciler) applyRendered(ctx context.Context, comp *gentianov1alpha1.Component, rendered *apprender.Rendered) (ready bool, reason, message string, err error) {
	var releases, rest []*unstructured.Unstructured
	for _, obj := range rendered.Objects {
		if isRelease(obj) {
			releases = append(releases, obj)
		} else {
			rest = append(rest, obj)
		}
	}
	previous := inventoryOf(comp)
	var current []string
	for _, obj := range rendered.Objects {
		if !ownedHere(comp, obj) {
			current = append(current, inventoryKey(obj))
		}
	}
	// What is about to be written is recorded first: an object written and
	// then lost track of by a pass that ends early would outlive the
	// component.
	if err := r.recordInventory(ctx, comp, append(append([]string{}, previous...), current...)); err != nil {
		return false, "", "", err
	}
	for _, obj := range rest {
		if _, err := r.applyObject(ctx, comp, obj); err != nil {
			return false, "", "", fmt.Errorf("%s %s: %w", obj.GetKind(), obj.GetName(), err)
		}
	}
	if waiting, err := r.sensitiveValuesPending(ctx, comp, rest); err != nil || waiting != "" {
		return false, "ValuesPending", waiting, err
	}
	// A release written on this pass has not deployed what it now says,
	// whatever it reported before.
	var written string
	for _, obj := range releases {
		changed, err := r.applyObject(ctx, comp, obj)
		if err != nil {
			return false, "", "", fmt.Errorf("%s %s: %w", obj.GetKind(), obj.GetName(), err)
		}
		if changed && written == "" {
			written = "release " + obj.GetName() + " written; waiting for the chart to deploy"
		}
	}
	// What was rendered before and is not now.
	keep := map[string]bool{}
	for _, k := range current {
		keep[k] = true
	}
	for _, k := range previous {
		if keep[k] {
			continue
		}
		if err := r.deleteInventoried(ctx, comp, k); err != nil {
			return false, "", "", err
		}
	}
	if err := r.pruneOwned(ctx, comp, rendered.Objects); err != nil {
		return false, "", "", err
	}
	if err := r.recordInventory(ctx, comp, current); err != nil {
		return false, "", "", err
	}
	if written != "" {
		return false, "Installing", written, nil
	}
	ready, message, err = r.renderedReady(ctx, rendered.Objects)
	return ready, "Installing", message, err
}

// applyObject writes one rendered object: created when missing, and brought
// back to what was rendered when it differs. What it holds is compared only
// at the paths the render names: what the API server or a provider fills in
// beside it is left alone, and comparing it found drift on every pass. What
// the render stopped saying is told by the hash of the last render
// (renderHashAnnotation), and is taken off by replacing what the object
// holds with what is rendered. It reports whether what the object holds was
// written.
//
// Its labels, annotations and owner are the component's, and are written on
// their own: an object taken over from the app Composition holds what is
// rendered and lacks only those, and is not rewritten for them.
func (r *ComponentReconciler) applyObject(ctx context.Context, comp *gentianov1alpha1.Component, rendered *unstructured.Unstructured) (bool, error) {
	desired, err := jsonNormalized(rendered.Object)
	if err != nil {
		return false, err
	}
	obj := &unstructured.Unstructured{Object: desired}
	labels := obj.GetLabels()
	if labels == nil {
		labels = map[string]string{}
	}
	for k, v := range componentLabels(comp) {
		labels[k] = v
	}
	if ownedHere(comp, obj) {
		labels[renderedLabel] = "true"
	}
	obj.SetLabels(labels)
	if ownedHere(comp, obj) {
		if err := controllerutil.SetControllerReference(comp, obj, r.Scheme); err != nil {
			return false, err
		}
	}
	hash, err := renderHash(obj)
	if err != nil {
		return false, err
	}
	setAnnotation(obj, renderHashAnnotation, hash)
	existing := &unstructured.Unstructured{}
	existing.SetGroupVersionKind(obj.GroupVersionKind())
	err = r.Get(ctx, types.NamespacedName{Name: obj.GetName(), Namespace: obj.GetNamespace()}, existing)
	if errors.IsNotFound(err) {
		return true, r.Create(ctx, obj)
	}
	if err != nil {
		return false, err
	}
	if !r.mayWrite(comp, existing) {
		return false, fmt.Errorf("already exists and is not this component's")
	}
	last := existing.GetAnnotations()[renderHashAnnotation]
	// A Job runs what it was made with and cannot be changed: one that
	// should run something else is rendered under another name.
	content := contentHolds(existing, obj) && (last == "" || last == hash)
	if isJob(obj) {
		content = true
	}
	if content && metadataHolds(existing, obj) {
		return false, nil
	}
	patch := client.MergeFrom(existing.DeepCopy())
	if !content {
		for k, v := range obj.Object {
			switch k {
			case "apiVersion", "kind", "metadata", "status":
				continue
			}
			existing.Object[k] = v
		}
	}
	existingLabels := existing.GetLabels()
	if existingLabels == nil {
		existingLabels = map[string]string{}
	}
	for k, v := range obj.GetLabels() {
		existingLabels[k] = v
	}
	existing.SetLabels(existingLabels)
	existingAnnotations := existing.GetAnnotations()
	if existingAnnotations == nil {
		existingAnnotations = map[string]string{}
	}
	for k, v := range obj.GetAnnotations() {
		existingAnnotations[k] = v
	}
	existing.SetAnnotations(existingAnnotations)
	if ownedHere(comp, obj) {
		existing.SetOwnerReferences(obj.GetOwnerReferences())
	}
	return !content, r.Patch(ctx, existing, patch)
}

func isJob(obj *unstructured.Unstructured) bool {
	return obj.GroupVersionKind().Group == "batch" && obj.GetKind() == "Job"
}

// renderHashAnnotation records, on each rendered object, a hash of what was
// rendered for it last: a render that stopped saying something is told from
// one that says the same by it.
const renderHashAnnotation = "gentianos.io/render-hash"

// renderHash hashes what is rendered of an object beside its metadata.
func renderHash(obj *unstructured.Unstructured) (string, error) {
	content := map[string]interface{}{}
	for k, v := range obj.Object {
		switch k {
		case "apiVersion", "kind", "metadata", "status":
			continue
		}
		content[k] = v
	}
	raw, err := json.Marshal(content)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:8]), nil
}

func setAnnotation(obj *unstructured.Unstructured, key, value string) {
	annotations := obj.GetAnnotations()
	if annotations == nil {
		annotations = map[string]string{}
	}
	annotations[key] = value
	obj.SetAnnotations(annotations)
}

// contentHolds reports whether a live object already holds everything the
// render says of it beside its metadata.
func contentHolds(existing, desired *unstructured.Unstructured) bool {
	for k, v := range desired.Object {
		switch k {
		case "apiVersion", "kind", "metadata", "status":
			continue
		}
		if !covers(existing.Object[k], v) {
			return false
		}
	}
	return true
}

// metadataHolds reports whether a live object carries the labels,
// annotations and owner the render gives it.
func metadataHolds(existing, desired *unstructured.Unstructured) bool {
	if !covers(stringMap(existing.GetLabels()), stringMap(desired.GetLabels())) ||
		!covers(stringMap(existing.GetAnnotations()), stringMap(desired.GetAnnotations())) {
		return false
	}
	if len(desired.GetOwnerReferences()) > 0 && !equalOwners(existing.GetOwnerReferences(), desired.GetOwnerReferences()) {
		return false
	}
	return true
}

func stringMap(m map[string]string) map[string]interface{} {
	out := map[string]interface{}{}
	for k, v := range m {
		out[k] = v
	}
	return out
}

func equalOwners(have, want []metav1.OwnerReference) bool {
	if len(have) != len(want) {
		return false
	}
	for i := range want {
		if have[i].UID != want[i].UID || have[i].Kind != want[i].Kind || have[i].Name != want[i].Name ||
			(have[i].Controller == nil) != (want[i].Controller == nil) {
			return false
		}
	}
	return true
}

// mayWrite reports whether an existing object is this component's to write:
// one it rendered, or one its handoff took over from the composite it was
// installed as. An object of the same name that is neither is somebody
// else's, and is not taken.
func (r *ComponentReconciler) mayWrite(comp *gentianov1alpha1.Component, existing *unstructured.Unstructured) bool {
	labels := existing.GetLabels()
	if labels[componentLabel] == comp.Name && labels[tenantLabel] == componentLabels(comp)[tenantLabel] {
		return true
	}
	if ownedBy(existing, comp) {
		return true
	}
	// A supplied credential's declaration as this reconciler wrote it before
	// it rendered it: named for the component as the provider.
	if labels[suppliedProviderLabel] == comp.Name && labels[tenantLabel] == componentLabels(comp)[tenantLabel] &&
		labels[managedByLabel] == managedByValue {
		return true
	}
	xr := comp.Annotations[compositeAnnotation]
	return xr != "" && labels["crossplane.io/composite"] == xr
}

// suppliedProviderLabel names, on a supplied credential's declaration, the
// component that asks for it.
const suppliedProviderLabel = "gentianos.io/credential-provider"

// sensitiveValuesPending says what the release waits for while the Secret
// its values read has not arrived; empty once it has, or when nothing is
// read from one.
func (r *ComponentReconciler) sensitiveValuesPending(ctx context.Context, comp *gentianov1alpha1.Component, objs []*unstructured.Unstructured) (string, error) {
	for _, obj := range objs {
		if obj.GetKind() != "ExternalSecret" || obj.GetNamespace() != comp.Namespace || obj.GetName() != sensitiveValuesName(comp) {
			continue
		}
		live, err := r.liveCopy(ctx, obj)
		if err != nil {
			return "", err
		}
		if live == nil || !externalSecretSynced(live) {
			return "waiting for the values the profile maps from the tenant's store (" + sensitiveValuesName(comp) + ")", nil
		}
	}
	return "", nil
}

// renderedReady reports whether what was rendered is in place: every
// release deployed and every object of the realm's registered. What is
// still missing is said in its provider's words.
func (r *ComponentReconciler) renderedReady(ctx context.Context, objs []*unstructured.Unstructured) (bool, string, error) {
	for _, obj := range objs {
		if !waitedFor(obj) {
			continue
		}
		live, err := r.liveCopy(ctx, obj)
		if err != nil {
			return false, "", err
		}
		if live == nil {
			return false, fmt.Sprintf("waiting for %s %s", obj.GetKind(), obj.GetName()), nil
		}
		if crossplaneObjectReady(live) {
			continue
		}
		if isRelease(obj) {
			return false, releaseMessageOf(live), nil
		}
		if msg := releaseFailureOf(live); msg != "" {
			return false, fmt.Sprintf("%s %s: %s", obj.GetKind(), obj.GetName(), msg), nil
		}
		return false, fmt.Sprintf("waiting for %s %s", obj.GetKind(), obj.GetName()), nil
	}
	return true, "", nil
}

// waitedFor are the objects a component is not ready without: its
// releases, and what registers it at the realm.
func waitedFor(obj *unstructured.Unstructured) bool {
	group := obj.GroupVersionKind().Group
	return isRelease(obj) || strings.HasSuffix(group, ".keycloak.crossplane.io")
}

// deleteInventoried deletes one object of the inventory, if it is still
// this component's.
func (r *ComponentReconciler) deleteInventoried(ctx context.Context, comp *gentianov1alpha1.Component, key string) error {
	ref, ok := inventoryObject(key)
	if !ok {
		return nil
	}
	live, err := r.liveCopy(ctx, ref)
	if err != nil || live == nil {
		return err
	}
	if !r.mayWrite(comp, live) || !live.GetDeletionTimestamp().IsZero() {
		return nil
	}
	return client.IgnoreNotFound(r.Delete(ctx, live))
}

// pruneOwned removes what is owned by the component in its namespace, was
// rendered for it, and is not rendered now. Only the kinds a render makes.
func (r *ComponentReconciler) pruneOwned(ctx context.Context, comp *gentianov1alpha1.Component, objs []*unstructured.Unstructured) error {
	keep := map[string]bool{}
	for _, obj := range objs {
		if ownedHere(comp, obj) {
			keep[inventoryKey(obj)] = true
		}
	}
	for _, gvk := range apprender.NamespacedKinds {
		list := &unstructured.UnstructuredList{}
		list.SetGroupVersionKind(gvk.GroupVersion().WithKind(gvk.Kind + "List"))
		if err := r.List(ctx, list, client.InNamespace(comp.Namespace), client.MatchingLabels(componentLabels(comp))); err != nil {
			if apimeta.IsNoMatchError(err) {
				continue
			}
			return err
		}
		for i := range list.Items {
			item := &list.Items[i]
			item.SetGroupVersionKind(gvk)
			if keep[inventoryKey(item)] || !ownedBy(item, comp) || !item.GetDeletionTimestamp().IsZero() {
				continue
			}
			if item.GetLabels()[renderedLabel] != "true" {
				continue
			}
			if err := client.IgnoreNotFound(r.Delete(ctx, item, client.PropagationPolicy(metav1.DeletePropagationBackground))); err != nil {
				return err
			}
		}
	}
	return nil
}

// deleteRendered removes everything rendered for a component outside its
// namespace's owner references, and reports whether it is all gone. The
// releases are waited for: "the component is gone" is what a purge waits
// for before it drops a database.
func (r *ComponentReconciler) deleteRendered(ctx context.Context, comp *gentianov1alpha1.Component) (bool, error) {
	gone := true
	var left []string
	for _, k := range compactStrings(sortedUnion(inventoryOf(comp), takenOf(comp))) {
		ref, ok := inventoryObject(k)
		if !ok {
			continue
		}
		live, err := r.liveCopy(ctx, ref)
		if err != nil {
			return false, err
		}
		if live == nil || !r.mayWrite(comp, live) {
			continue
		}
		left = append(left, k)
		if live.GetDeletionTimestamp().IsZero() {
			if err := client.IgnoreNotFound(r.Delete(ctx, live)); err != nil {
				return false, err
			}
		}
		if isRelease(live) {
			gone = false
		}
	}
	if gone {
		return true, nil
	}
	return false, r.recordInventory(ctx, comp, left)
}

// sensitiveValuesName is the Secret, and its ExternalSecret, a component's
// mapped values are read from.
func sensitiveValuesName(comp *gentianov1alpha1.Component) string {
	return comp.Name + "-sensitive-values"
}

// externalSecretSynced reports whether an ExternalSecret's Secret is the one
// its current spec asks for: Ready, and synced from this generation, so a
// Ready left from before a change does not count.
func externalSecretSynced(es *unstructured.Unstructured) bool {
	if !crossplaneObjectReady(es) {
		return false
	}
	synced, _, _ := unstructured.NestedString(es.Object, "status", "syncedResourceVersion")
	return synced == "" || strings.HasPrefix(synced, fmt.Sprintf("%d-", es.GetGeneration()))
}

// covers reports whether have holds everything want says, the same at every
// path; a key want does not name may hold anything. Lists are compared item
// by item and must be of the same length.
func covers(have, want interface{}) bool {
	switch w := want.(type) {
	case map[string]interface{}:
		h, ok := have.(map[string]interface{})
		if !ok {
			return false
		}
		for k, v := range w {
			if !covers(h[k], v) {
				return false
			}
		}
		return true
	case []interface{}:
		h, ok := have.([]interface{})
		if !ok || len(h) != len(w) {
			return false
		}
		for i := range w {
			if !covers(h[i], w[i]) {
				return false
			}
		}
		return true
	default:
		return equality.Semantic.DeepEqual(have, want)
	}
}
