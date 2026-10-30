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

	"k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/composed"
)

// An app the platform's app Composition of earlier releases installed is
// taken over from its composite in
// place: what the composite composed stays where it is, under its names, and
// becomes the component's. Nothing is installed again, no Helm revision is
// made and nothing at the realm changes, because what is rendered here for
// the app is what the Composition rendered for it (apprender).
//
// Crossplane deletes what a composite composed only through owner
// references. So the composite is paused, so that it puts nothing back; each
// object it composed loses the reference; the claim is deleted; and the
// composite is let go, which then only drops its finalizer. The objects the
// Composition wrapped in a provider-kubernetes Object -- the Secrets' and
// the job's, the Service aliases -- are marked the component's and their
// wrappers removed without them.

// +kubebuilder:rbac:groups=gentianos.io,resources=xapps,verbs=get;list;watch;patch;update
// +kubebuilder:rbac:groups=gentianos.io,resources=apps,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=kubernetes.crossplane.io,resources=objects,verbs=get;list;watch;patch;update;delete
// +kubebuilder:rbac:groups=kv.vault.upbound.io,resources=secretv2s,verbs=get;list;watch;patch;update;delete

const pausedAnnotation = "crossplane.io/paused"

var objectWrapperGVK = schema.GroupVersionKind{Group: "kubernetes.crossplane.io", Version: "v1alpha2", Kind: "Object"}

// handOff takes a component's app over from the composite the app
// Composition installed it as. It says what it waits for, and "" once
// nothing of the composite is left to take over.
func (r *ComponentReconciler) handOff(ctx context.Context, comp *gentianov1alpha1.Component) (string, error) {
	claim := &unstructured.Unstructured{}
	claim.SetGroupVersionKind(appClaimGVK)
	err := r.Get(ctx, types.NamespacedName{Name: comp.Name, Namespace: comp.Namespace}, claim)
	if apimeta.IsNoMatchError(err) {
		return "", nil
	}
	if err != nil && !errors.IsNotFound(err) {
		return "", err
	}
	claimed := err == nil
	xrName := comp.Annotations[compositeAnnotation]
	if claimed {
		// A claim that is not this component's is not taken from whoever
		// it is.
		if !controlledBy(claim, comp) {
			return fmt.Sprintf("waiting for the App claim %s/%s, which is not this component's, to go", comp.Namespace, comp.Name), nil
		}
		if current, _, _ := unstructured.NestedString(claim.Object, "spec", "resourceRef", "name"); current != "" && current != xrName {
			// Written down before anything is taken: the names of what is
			// taken over follow from it on every pass after this one.
			if err := r.annotate(ctx, comp, compositeAnnotation, current); err != nil {
				return "", err
			}
			xrName = current
		}
	}
	if xrName == "" {
		if claimed && claim.GetDeletionTimestamp().IsZero() {
			// Nothing was composed for it yet: there is nothing to take.
			return "waiting for the App claim, which composed nothing, to go", client.IgnoreNotFound(r.Delete(ctx, claim))
		}
		if claimed {
			return "waiting for the App claim to go", nil
		}
		return "", nil
	}

	xr := &unstructured.Unstructured{}
	xr.SetGroupVersionKind(composed.XAppGVK)
	err = r.Get(ctx, types.NamespacedName{Name: xrName}, xr)
	if err != nil && !errors.IsNotFound(err) && !apimeta.IsNoMatchError(err) {
		return "", err
	}
	if err == nil {
		paused := xr.GetAnnotations()[pausedAnnotation] == "true"
		if xr.GetDeletionTimestamp().IsZero() {
			if !paused {
				return "pausing the composite " + xrName + " to take its resources over", r.setPaused(ctx, xr, true)
			}
			// Not before Crossplane says it has stopped: a pass of its own
			// already under way would put back what is taken off here.
			if !compositePaused(xr) {
				return "waiting for Crossplane to pause the composite " + xrName, nil
			}
			if err := r.releaseComposed(ctx, comp, xr); err != nil {
				return "", err
			}
			if claimed && claim.GetDeletionTimestamp().IsZero() {
				if err := client.IgnoreNotFound(r.Delete(ctx, claim)); err != nil {
					return "", err
				}
			}
			return "waiting for the composite " + xrName + " to go", nil
		}
		// Deleting, and paused: let go, so that Crossplane drops its
		// finalizer. Nothing is composed for a composite being deleted.
		// Taken off once more first: whatever was composed after the last
		// pass would go with the composite.
		if paused {
			if err := r.releaseComposed(ctx, comp, xr); err != nil {
				return "", err
			}
			return "waiting for the composite " + xrName + " to go", r.setPaused(ctx, xr, false)
		}
		return "waiting for the composite " + xrName + " to go", nil
	}
	if claimed {
		return "waiting for the App claim to go", nil
	}
	return "", r.dropWrappers(ctx, xrName)
}

// dropWrappers removes, once the composite is gone, its wrappers without what
// they wrap and its seeders without the paths they seeded.
func (r *ComponentReconciler) dropWrappers(ctx context.Context, xrName string) error {
	for _, gvk := range []schema.GroupVersionKind{objectWrapperGVK, secretV2GVK} {
		list := &unstructured.UnstructuredList{}
		list.SetGroupVersionKind(gvk.GroupVersion().WithKind(gvk.Kind + "List"))
		if err := r.List(ctx, list, client.MatchingLabels{composed.CompositeLabel: xrName}); err != nil {
			if apimeta.IsNoMatchError(err) {
				continue
			}
			return err
		}
		for i := range list.Items {
			item := &list.Items[i]
			if len(item.GetOwnerReferences()) > 0 || !item.GetDeletionTimestamp().IsZero() {
				continue
			}
			if policy, _, _ := unstructured.NestedString(item.Object, "spec", "deletionPolicy"); policy != "Orphan" {
				continue
			}
			if err := client.IgnoreNotFound(r.Delete(ctx, item)); err != nil {
				return err
			}
		}
	}
	return nil
}

// handOffOnDelete finishes, for a component being removed, a handoff it had
// begun: what the composite composed is taken off it and written into the
// component's inventory, and so removed with the component as what was
// rendered is. A composite whose handoff had not begun goes with its claim,
// and takes what it composed with it.
func (r *ComponentReconciler) handOffOnDelete(ctx context.Context, comp *gentianov1alpha1.Component) (bool, error) {
	xrName := comp.Annotations[compositeAnnotation]
	if xrName == "" {
		return true, nil
	}
	xr := &unstructured.Unstructured{}
	xr.SetGroupVersionKind(composed.XAppGVK)
	err := r.Get(ctx, types.NamespacedName{Name: xrName}, xr)
	if errors.IsNotFound(err) || apimeta.IsNoMatchError(err) {
		return true, r.dropWrappers(ctx, xrName)
	}
	if err != nil {
		return false, err
	}
	if xr.GetAnnotations()[pausedAnnotation] != "true" {
		return true, nil
	}
	if err := r.releaseComposed(ctx, comp, xr); err != nil {
		return false, err
	}
	// The wrappers now orphan what they wrap and nothing owns them: they go
	// now, or they would make it again once it is removed.
	if err := r.dropWrappers(ctx, xrName); err != nil {
		return false, err
	}
	// Let go only of a composite being deleted: one that is not would
	// compose everything again. Its claim goes first.
	if xr.GetDeletionTimestamp().IsZero() {
		claim := &unstructured.Unstructured{}
		claim.SetGroupVersionKind(appClaimGVK)
		claim.SetName(comp.Name)
		claim.SetNamespace(comp.Namespace)
		if err := r.Delete(ctx, claim); err != nil && !errors.IsNotFound(err) && !apimeta.IsNoMatchError(err) {
			return false, err
		}
		return false, nil
	}
	return true, r.setPaused(ctx, xr, false)
}

// compositePaused reports whether Crossplane has stopped reconciling a
// composite: it says so in its Synced condition.
func compositePaused(xr *unstructured.Unstructured) bool {
	conditions, _, _ := unstructured.NestedSlice(xr.Object, "status", "conditions")
	for _, raw := range conditions {
		c, _ := raw.(map[string]interface{})
		if c["type"] == "Synced" && c["reason"] == "ReconcilePaused" {
			return true
		}
	}
	return false
}

var secretV2GVK = schema.GroupVersionKind{Group: "kv.vault.upbound.io", Version: "v1alpha1", Kind: "SecretV2"}

// releaseComposed takes the composite's owner reference off everything it
// composed. A wrapper and a seeder are set to orphan what they manage, and
// what a wrapper manages is marked the component's, which is what lets the
// next render write it.
func (r *ComponentReconciler) releaseComposed(ctx context.Context, comp *gentianov1alpha1.Component, xr *unstructured.Unstructured) error {
	refs, _, _ := unstructured.NestedSlice(xr.Object, "spec", "resourceRefs")
	if len(refs) == 0 {
		refs, _, _ = unstructured.NestedSlice(xr.Object, "spec", "crossplane", "resourceRefs")
	}
	// What is taken over is the component's from here on, before a render
	// has said so: written into its inventory first, so that it goes with
	// the component whatever ends a pass before that.
	var taken []string
	for _, raw := range refs {
		ref, _ := raw.(map[string]interface{})
		apiVersion, _ := ref["apiVersion"].(string)
		kind, _ := ref["kind"].(string)
		name, _ := ref["name"].(string)
		if apiVersion == "" || kind == "" || name == "" {
			continue
		}
		obj := &unstructured.Unstructured{}
		obj.SetAPIVersion(apiVersion)
		obj.SetKind(kind)
		if err := r.Get(ctx, types.NamespacedName{Name: name}, obj); err != nil {
			if errors.IsNotFound(err) || apimeta.IsNoMatchError(err) {
				continue
			}
			return err
		}
		gvk := obj.GroupVersionKind()
		wrapper := gvk == objectWrapperGVK
		observeOnly := false
		if policies, _, _ := unstructured.NestedStringSlice(obj.Object, "spec", "managementPolicies"); len(policies) == 1 && policies[0] == "Observe" {
			observeOnly = true
		}
		switch {
		case wrapper && observeOnly:
			// It only watched what somebody else -- the chart -- manages:
			// what it watched is not the component's to own or remove.
		case wrapper:
			key, err := r.markWrapped(ctx, comp, obj)
			if err != nil {
				return err
			}
			if key != "" {
				taken = append(taken, key)
			}
		case gvk != secretV2GVK:
			taken = append(taken, inventoryKey(obj))
		}
		patch := client.MergeFrom(obj.DeepCopy())
		changed := false
		kept := obj.GetOwnerReferences()[:0]
		for _, o := range obj.GetOwnerReferences() {
			if o.UID == xr.GetUID() {
				changed = true
				continue
			}
			kept = append(kept, o)
		}
		obj.SetOwnerReferences(kept)
		if wrapper || gvk == secretV2GVK {
			if policy, _, _ := unstructured.NestedString(obj.Object, "spec", "deletionPolicy"); policy != "Orphan" {
				if err := unstructured.SetNestedField(obj.Object, "Orphan", "spec", "deletionPolicy"); err != nil {
					return err
				}
				changed = true
			}
		}
		if !changed {
			continue
		}
		if err := r.Patch(ctx, obj, patch); err != nil {
			return err
		}
	}
	if len(taken) == 0 {
		return nil
	}
	return r.recordTaken(ctx, comp, append(takenOf(comp), taken...))
}

// markWrapped gives what a provider-kubernetes Object manages the
// component's labels, and, in the component's namespace, the component as
// its owner. It answers the inventory key of one outside that namespace,
// where no owner reference reaches.
func (r *ComponentReconciler) markWrapped(ctx context.Context, comp *gentianov1alpha1.Component, wrapper *unstructured.Unstructured) (string, error) {
	manifest, found, _ := unstructured.NestedMap(wrapper.Object, "spec", "forProvider", "manifest")
	if !found {
		return "", nil
	}
	ref := &unstructured.Unstructured{Object: manifest}
	live, err := r.liveCopy(ctx, ref)
	if err != nil || live == nil {
		return "", err
	}
	key := ""
	if live.GetNamespace() != comp.Namespace {
		key = inventoryKey(live)
	}
	labels := live.GetLabels()
	if labels == nil {
		labels = map[string]string{}
	}
	want := componentLabels(comp)
	if live.GetNamespace() == comp.Namespace {
		want[renderedLabel] = "true"
	}
	patch := client.MergeFrom(live.DeepCopy())
	changed := false
	for k, v := range want {
		if labels[k] != v {
			labels[k] = v
			changed = true
		}
	}
	live.SetLabels(labels)
	if live.GetNamespace() == comp.Namespace && metav1.GetControllerOf(live) == nil {
		if err := controllerutil.SetControllerReference(comp, live, r.Scheme); err != nil {
			return "", err
		}
		changed = true
	}
	if !changed {
		return key, nil
	}
	return key, r.Patch(ctx, live, patch)
}

func (r *ComponentReconciler) setPaused(ctx context.Context, xr *unstructured.Unstructured, paused bool) error {
	patch := client.MergeFrom(xr.DeepCopy())
	annotations := xr.GetAnnotations()
	if annotations == nil {
		annotations = map[string]string{}
	}
	if paused {
		annotations[pausedAnnotation] = "true"
	} else {
		delete(annotations, pausedAnnotation)
	}
	xr.SetAnnotations(annotations)
	return r.Patch(ctx, xr, patch)
}

// annotate sets one annotation on the Component, on a copy: the patch's
// answer would replace what this pass has worked out of the status.
func (r *ComponentReconciler) annotate(ctx context.Context, comp *gentianov1alpha1.Component, key, value string) error {
	marked := comp.DeepCopy()
	patch := client.MergeFrom(marked.DeepCopy())
	if marked.Annotations == nil {
		marked.Annotations = map[string]string{}
	}
	marked.Annotations[key] = value
	if err := r.Patch(ctx, marked, patch); err != nil {
		return err
	}
	if comp.Annotations == nil {
		comp.Annotations = map[string]string{}
	}
	comp.Annotations[key] = value
	comp.ResourceVersion = marked.ResourceVersion
	return nil
}

func controlledBy(obj *unstructured.Unstructured, comp *gentianov1alpha1.Component) bool {
	for _, o := range obj.GetOwnerReferences() {
		if o.Controller != nil && *o.Controller {
			return o.UID == comp.UID
		}
	}
	return false
}
