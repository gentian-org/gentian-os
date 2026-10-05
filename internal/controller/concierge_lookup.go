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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// conciergeLookupConfigMap is what the concierge answers an address on a
// custom domain from: one key per bound domain, named by the domain's
// SHA-256, holding {"url": <its console>}. Mounted beside the page as
// /sign-in/lookup/, so the page finds a domain it already knows and nobody
// can list the domains a cluster serves.
const conciergeLookupConfigMap = "concierge-lookup"

// ConciergeLookupReconciler projects the tenants' custom domains into that
// ConfigMap. Every tenant event re-derives the whole of it, under one key.
type ConciergeLookupReconciler struct {
	client.Client
	KernelDomain string
	TenancyMode  string
}

var conciergeLookupRequest = reconcile.Request{NamespacedName: types.NamespacedName{Name: conciergeLookupConfigMap, Namespace: identityNamespace}}

// conciergeLookupKey is the file name the page asks for an e-mail domain.
func conciergeLookupKey(domain string) string {
	sum := sha256.Sum256([]byte(domain))
	return hex.EncodeToString(sum[:]) + ".json"
}

// conciergeLookupData is the ConfigMap's contents for these tenants.
func conciergeLookupData(tenants []gentianov1alpha1.Tenant, kernelDomain, tenancyMode string) map[string]string {
	data := map[string]string{}
	for i := range tenants {
		t := &tenants[i]
		if t.DeletionTimestamp != nil || t.Status.Domain == "" {
			continue
		}
		body, _ := json.Marshal(map[string]string{"url": "https://" + consoleHost(t.EffectiveDomain(kernelDomain, tenancyMode)) + "/"})
		data[conciergeLookupKey(t.Status.Domain)] = string(body)
	}
	return data
}

func (r *ConciergeLookupReconciler) Reconcile(ctx context.Context, _ ctrl.Request) (ctrl.Result, error) {
	tenants := &gentianov1alpha1.TenantList{}
	if err := r.List(ctx, tenants); err != nil {
		return ctrl.Result{}, err
	}
	data := conciergeLookupData(tenants.Items, r.KernelDomain, r.TenancyMode)

	existing := &corev1.ConfigMap{}
	err := r.Get(ctx, conciergeLookupRequest.NamespacedName, existing)
	if errors.IsNotFound(err) {
		cm := &corev1.ConfigMap{}
		cm.Name, cm.Namespace = conciergeLookupConfigMap, identityNamespace
		cm.Labels = map[string]string{managedByLabel: managedByValue}
		cm.Data = data
		return ctrl.Result{}, r.Create(ctx, cm)
	}
	if err != nil {
		return ctrl.Result{}, err
	}
	if equality.Semantic.DeepEqual(existing.Data, data) || (len(existing.Data) == 0 && len(data) == 0) {
		return ctrl.Result{}, nil
	}
	patch := client.MergeFrom(existing.DeepCopy())
	existing.Data = data
	return ctrl.Result{}, r.Patch(ctx, existing, patch)
}

func (r *ConciergeLookupReconciler) SetupWithManager(mgr ctrl.Manager) error {
	toLookup := handler.EnqueueRequestsFromMapFunc(func(context.Context, client.Object) []reconcile.Request {
		return []reconcile.Request{conciergeLookupRequest}
	})
	return ctrl.NewControllerManagedBy(mgr).
		Named("concierge-lookup").
		Watches(&gentianov1alpha1.Tenant{}, toLookup).
		Complete(r)
}
