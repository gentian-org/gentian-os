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
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/branding"
)

// brandingConfigMap is where the brand is published, twice. In the platform
// tenant's namespace, beside the concierge, which serves it on the cluster's
// bare domain to every page. And beside the identity provider, labelled so
// the tenant Composition finds it the way it finds the cluster config, for
// the name its realms' mail is sent under; that copy is read, never served.
const (
	brandingConfigMap  = "branding"
	configTypeLabel    = "gentianos.io/config-type"
	brandingConfigType = "branding"
)

// brandName is what the cluster calls itself: the Branding's name, or the
// platform's own when it sets none.
func brandName(ctx context.Context, c client.Reader) string {
	b := &gentianov1alpha1.Branding{}
	if err := c.Get(ctx, brandingRequest.NamespacedName, b); err == nil {
		if n := strings.TrimSpace(b.Spec.Identity.Name); n != "" {
			return n
		}
	}
	return branding.DefaultName
}

// BrandingReconciler renders the cluster's Branding into what the pages
// load: brand.css, brand.json and brand.webmanifest. Without a Branding it
// publishes the platform's own, so a page never has to know whether one was
// set.
//
// A brand whose tokens cannot be rendered leaves the published files as they
// were and says why on its status: a typo in a colour must not take every
// page's styling away.
type BrandingReconciler struct {
	client.Client
	// KernelRealm is the realm the platform tenant adopts, which is how that
	// tenant, and so the concierge's namespace, is found.
	KernelRealm string
}

// The markers are a free-floating block: controller-gen ignores a block that
// is part of a declaration's doc comment, and the operator then cannot list
// the kind it watches and never starts.
//
// +kubebuilder:rbac:groups=gentianos.io,resources=brandings,verbs=get;list;watch
// +kubebuilder:rbac:groups=gentianos.io,resources=brandings/status,verbs=get;update;patch

var brandingRequest = reconcile.Request{NamespacedName: types.NamespacedName{Name: gentianov1alpha1.BrandingName}}

func (r *BrandingReconciler) Reconcile(ctx context.Context, _ ctrl.Request) (ctrl.Result, error) {
	b := &gentianov1alpha1.Branding{}
	err := r.Get(ctx, brandingRequest.NamespacedName, b)
	if err != nil && !errors.IsNotFound(err) {
		return ctrl.Result{}, err
	}
	var spec *gentianov1alpha1.BrandingSpec
	if err == nil {
		spec = &b.Spec
	} else {
		b = nil
	}
	files, renderErr := branding.Render(spec)
	if renderErr != nil {
		return ctrl.Result{}, r.setRendered(ctx, b, metav1.ConditionFalse, "TokensInvalid", renderErr.Error())
	}
	if err := r.publish(ctx, files); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, r.setRendered(ctx, b, metav1.ConditionTrue, "Published", "The pages show this brand")
}

func (r *BrandingReconciler) publish(ctx context.Context, files branding.Files) error {
	if err := r.publishTo(ctx, identityNamespace, true, files); err != nil {
		return err
	}
	tenants := &gentianov1alpha1.TenantList{}
	if err := r.List(ctx, tenants); err != nil {
		return err
	}
	// No platform tenant yet means no concierge to serve it; the tenant's
	// arrival is watched and brings this back.
	if ns := platformTenantNamespace(tenants.Items, r.KernelRealm); ns != "" {
		return r.publishTo(ctx, ns, false, files)
	}
	return nil
}

// publishTo writes the brand's files into one namespace. Only the copy the
// Composition selects by label carries it: a selector that matched two
// ConfigMaps would have two answers to what the cluster is called.
func (r *BrandingReconciler) publishTo(ctx context.Context, namespace string, labelled bool, files branding.Files) error {
	labels := map[string]string{managedByLabel: managedByValue}
	if labelled {
		labels[configTypeLabel] = brandingConfigType
	}
	existing := &corev1.ConfigMap{}
	err := r.Get(ctx, types.NamespacedName{Name: brandingConfigMap, Namespace: namespace}, existing)
	if errors.IsNotFound(err) {
		cm := &corev1.ConfigMap{}
		cm.Name, cm.Namespace = brandingConfigMap, namespace
		cm.Labels = labels
		cm.Data, cm.BinaryData = files.Text, files.Binary
		return r.Create(ctx, cm)
	}
	if err != nil {
		return err
	}
	if equality.Semantic.DeepEqual(existing.Data, files.Text) && equality.Semantic.DeepEqual(nonNilBinary(existing.BinaryData), files.Binary) {
		return nil
	}
	// A whole replacement, not a merge: an icon the brand no longer names
	// has to leave the ConfigMap, and a merge patch keeps keys it omits.
	existing.Data, existing.BinaryData = files.Text, files.Binary
	if existing.Labels == nil {
		existing.Labels = map[string]string{}
	}
	for k, v := range labels {
		existing.Labels[k] = v
	}
	return r.Update(ctx, existing)
}

func nonNilBinary(m map[string][]byte) map[string][]byte {
	if m == nil {
		return map[string][]byte{}
	}
	return m
}

func (r *BrandingReconciler) setRendered(ctx context.Context, b *gentianov1alpha1.Branding, status metav1.ConditionStatus, reason, message string) error {
	if b == nil {
		return nil
	}
	before := b.DeepCopy()
	meta.SetStatusCondition(&b.Status.Conditions, metav1.Condition{
		Type: "Rendered", Status: status, Reason: reason, Message: message, ObservedGeneration: b.Generation,
	})
	b.Status.ObservedGeneration = b.Generation
	if equality.Semantic.DeepEqual(before.Status, b.Status) {
		return nil
	}
	return r.Status().Patch(ctx, b, client.MergeFrom(before))
}

func (r *BrandingReconciler) SetupWithManager(mgr ctrl.Manager) error {
	toBranding := handler.EnqueueRequestsFromMapFunc(func(context.Context, client.Object) []reconcile.Request {
		return []reconcile.Request{brandingRequest}
	})
	return ctrl.NewControllerManagedBy(mgr).
		Named("branding").
		Watches(&gentianov1alpha1.Branding{}, toBranding).
		// The platform tenant exists from the first install, so its events
		// are what publish the default brand on a cluster that never sets
		// one; a Branding watch alone would wait for one forever.
		Watches(&gentianov1alpha1.Tenant{}, toBranding).
		Complete(r)
}
