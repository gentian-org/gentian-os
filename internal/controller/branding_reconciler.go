/*
Copyright 2026 Gentian Organization.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
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

// brandingConfigMap is where the brand is published: beside the sign-in
// router, which serves it at id.<kernel>/branding/ to every page. Labelled so
// the tenant Composition finds it the way it finds the cluster config, for
// the name its realms' mail is sent under.
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
//
// +kubebuilder:rbac:groups=gentianos.io,resources=brandings,verbs=get;list;watch
// +kubebuilder:rbac:groups=gentianos.io,resources=brandings/status,verbs=get;update;patch
type BrandingReconciler struct {
	client.Client
}

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
	existing := &corev1.ConfigMap{}
	err := r.Get(ctx, types.NamespacedName{Name: brandingConfigMap, Namespace: identityNamespace}, existing)
	if errors.IsNotFound(err) {
		cm := &corev1.ConfigMap{}
		cm.Name, cm.Namespace = brandingConfigMap, identityNamespace
		cm.Labels = map[string]string{managedByLabel: managedByValue, configTypeLabel: brandingConfigType}
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
	existing.Labels[configTypeLabel] = brandingConfigType
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
