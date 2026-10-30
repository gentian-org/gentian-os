/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

// Package composed finds and removes what an app's composite resource
// composed and left behind when the composite went.
//
// Crossplane deletes a composite's resources through their owner reference.
// One it could not finish with stays: a provider-helm Release whose install
// was interrupted carries crossplane.io/external-create-pending, and the
// provider then refuses every reconcile of it -- its deletion included --
// until somebody takes the annotation off. The composite is gone by then, so
// nothing else ever looks at the Release again. It still names the app's Helm
// release, and the next install's Release fights it for that release.
package composed

import (
	"context"
	"fmt"
	"sort"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// The labels Crossplane puts on every resource a claim's composite composes.
const (
	ClaimNameLabel      = "crossplane.io/claim-name"
	ClaimNamespaceLabel = "crossplane.io/claim-namespace"
	CompositeLabel      = "crossplane.io/composite"
)

// externalCreatePending is the annotation crossplane-runtime sets before it
// creates an external resource. While it is there without a recorded result
// the provider reconciles nothing, deletion included.
const externalCreatePending = "crossplane.io/external-create-pending"

// Finding a leftover lists its kind and reads its composite; removing it
// takes the unfinished-create annotation off and deletes it.
// +kubebuilder:rbac:groups=helm.crossplane.io,resources=releases,verbs=get;list;watch;patch;delete
// +kubebuilder:rbac:groups=kubernetes.crossplane.io,resources=objects,verbs=get;list;watch;patch;delete
// +kubebuilder:rbac:groups=gentianos.io,resources=xapps,verbs=get;list;watch

// Kinds are the composed kinds looked at. Each is a kind an app Composition
// composed under a name the app's next install composes again or that
// manages something under the app's own name -- a Helm release, an object in
// the tenant's namespace -- and the operator holds the RBAC to list, patch
// and delete it (the markers above). Adding a kind needs its marker.
var Kinds = []schema.GroupVersionKind{
	{Group: "helm.crossplane.io", Version: "v1beta1", Kind: "Release"},
	{Group: "kubernetes.crossplane.io", Version: "v1alpha2", Kind: "Object"},
}

// XAppGVK is the app's composite.
var XAppGVK = schema.GroupVersionKind{Group: "gentianos.io", Version: "v1alpha1", Kind: "XApp"}

// Leftover is one composed resource of an app that is not its current
// composite's.
type Leftover struct {
	Object *unstructured.Unstructured
	// Composite is the composite it was composed for; Orphaned says that
	// composite is gone, so nothing but this package will remove it.
	Composite string
	Orphaned  bool
}

// String names the leftover for a message.
func (l Leftover) String() string {
	return fmt.Sprintf("%s %s (of %s)", l.Object.GetKind(), l.Object.GetName(), l.Composite)
}

// Find lists the resources composed for the app's claim (claimName in
// claimNamespace) whose composite is not current. current is the composite
// the app's live claim names; "" when there is no claim, and then every
// composed resource of the app is a leftover. claimName "" finds those of
// every claim in the namespace. A kind this cluster does not serve is
// skipped.
func Find(ctx context.Context, c client.Client, claimNamespace, claimName, current string) ([]Leftover, error) {
	var out []Leftover
	exists := map[string]bool{}
	for _, gvk := range Kinds {
		list := &unstructured.UnstructuredList{}
		list.SetGroupVersionKind(gvk.GroupVersion().WithKind(gvk.Kind + "List"))
		labels := client.MatchingLabels{ClaimNamespaceLabel: claimNamespace}
		if claimName != "" {
			labels[ClaimNameLabel] = claimName
		}
		err := c.List(ctx, list, labels)
		if apimeta.IsNoMatchError(err) || apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("list the %s resources composed for %s/%s: %w", gvk.Kind, claimNamespace, claimName, err)
		}
		for i := range list.Items {
			obj := &list.Items[i]
			composite := obj.GetLabels()[CompositeLabel]
			if composite == "" || composite == current || !composedByAnApp(obj) {
				continue
			}
			live, seen := exists[composite]
			if !seen {
				if live, err = compositeExists(ctx, c, composite); err != nil {
					return nil, err
				}
				exists[composite] = live
			}
			out = append(out, Leftover{Object: obj, Composite: composite, Orphaned: !live})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].String() < out[j].String() })
	return out, nil
}

// composedByAnApp says the resource is an app composite's: its controller is
// an XApp, or it has none left. One another kind of composite controls -- a
// tenant's, a cluster's -- is never an app's leftover, whatever its labels.
func composedByAnApp(obj *unstructured.Unstructured) bool {
	for _, ref := range obj.GetOwnerReferences() {
		if ref.Controller != nil && *ref.Controller {
			return ref.Kind == XAppGVK.Kind && strings.HasPrefix(ref.APIVersion, XAppGVK.Group+"/")
		}
	}
	return true
}

func compositeExists(ctx context.Context, c client.Client, name string) (bool, error) {
	xapp := &unstructured.Unstructured{}
	xapp.SetGroupVersionKind(XAppGVK)
	err := c.Get(ctx, client.ObjectKey{Name: name}, xapp)
	switch {
	case err == nil:
		return true, nil
	case apierrors.IsNotFound(err) || apimeta.IsNoMatchError(err):
		return false, nil
	}
	return false, fmt.Errorf("get the composite %s: %w", name, err)
}

// Remove deletes an orphaned leftover so that its provider can finish with
// it. One whose composite still exists is that composite's to remove and is
// left alone; Remove reports whether it acted.
//
// An unfinished create is unblocked: the pending annotation is taken off, so
// the provider observes the resource again and carries out the deletion. That
// is the annotation's documented remedy, and the duplicate it guards against
// cannot arise for a resource being deleted.
//
// keepExternal orphans the external resource: the app is installed again and
// its current composite manages the same Helm release or object, which a
// deletion with the leftover's own policy would take from under it.
func Remove(ctx context.Context, c client.Client, l Leftover, keepExternal bool) (bool, error) {
	if !l.Orphaned {
		return false, nil
	}
	obj := l.Object
	before := obj.DeepCopy()
	changed := false
	if ann := obj.GetAnnotations(); ann[externalCreatePending] != "" {
		delete(ann, externalCreatePending)
		obj.SetAnnotations(ann)
		changed = true
	}
	if keepExternal {
		if policy, _, _ := unstructured.NestedString(obj.Object, "spec", "deletionPolicy"); policy != "Orphan" {
			if err := unstructured.SetNestedField(obj.Object, "Orphan", "spec", "deletionPolicy"); err != nil {
				return false, err
			}
			changed = true
		}
	}
	if changed {
		if err := c.Patch(ctx, obj, client.MergeFrom(before)); err != nil && !apierrors.IsNotFound(err) {
			return false, fmt.Errorf("unblock %s: %w", l, err)
		}
	}
	if obj.GetDeletionTimestamp() == nil {
		if err := c.Delete(ctx, obj); err != nil && !apierrors.IsNotFound(err) {
			return false, fmt.Errorf("delete %s: %w", l, err)
		}
	}
	return true, nil
}
