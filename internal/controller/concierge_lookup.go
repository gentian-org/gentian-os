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
//
// It lives in the platform tenant's namespace, because that is where the
// concierge runs: a component of the platform tenant, published from its DMZ.
const conciergeLookupConfigMap = "concierge-lookup"

// conciergeSingleKey is the one file that is not a domain's: while the
// cluster has exactly one user tenant it names that tenant's console, and the
// page sends everybody there without asking for an address.
const conciergeSingleKey = "_single.json"

// ConciergeLookupReconciler projects the tenants' custom domains, and the
// fact that there is exactly one user tenant, into that ConfigMap. Every
// tenant event re-derives the whole of it, under one key, so the forward
// starts and stops with the tenants and nobody switches it.
type ConciergeLookupReconciler struct {
	client.Client
	KernelDomain string
	KernelRealm  string
	TenancyMode  string
}

var conciergeLookupRequest = reconcile.Request{NamespacedName: types.NamespacedName{Name: conciergeLookupConfigMap}}

// platformTenantNamespace is the namespace of the tenant that adopts the
// kernel realm, or empty while there is none.
func platformTenantNamespace(tenants []gentianov1alpha1.Tenant, kernelRealm string) string {
	if kernelRealm == "" {
		kernelRealm = "kernel"
	}
	for i := range tenants {
		if tenants[i].DeletionTimestamp == nil && tenantAdoptsKernelRealm(&tenants[i], kernelRealm) {
			return tenantNamespaceName(&tenants[i])
		}
	}
	return ""
}

// conciergeLookupKey is the file name the page asks for an e-mail domain.
func conciergeLookupKey(domain string) string {
	sum := sha256.Sum256([]byte(domain))
	return hex.EncodeToString(sum[:]) + ".json"
}

// soleUserTenant is the one tenant a single-tenant cluster's users live in,
// or nil when the cluster is not one.
//
// Single-tenant is a property of what exists: the platform tenant and
// exactly one user tenant. The platform tenant -- the one that adopts the
// kernel realm -- is never counted: its people are the cluster's
// administrators and it runs no apps. A tenant being deleted is not counted
// either; it is on its way out, and counting it would keep a cluster of two
// from becoming a cluster of one until the last finalizer cleared.
func soleUserTenant(tenants []gentianov1alpha1.Tenant, kernelRealm string) *gentianov1alpha1.Tenant {
	if kernelRealm == "" {
		kernelRealm = "kernel"
	}
	var sole *gentianov1alpha1.Tenant
	for i := range tenants {
		t := &tenants[i]
		if t.DeletionTimestamp != nil || tenantAdoptsKernelRealm(t, kernelRealm) {
			continue
		}
		if sole != nil {
			return nil
		}
		sole = t
	}
	return sole
}

// conciergeLookupData is the ConfigMap's contents for these tenants.
func conciergeLookupData(tenants []gentianov1alpha1.Tenant, kernelDomain, kernelRealm, tenancyMode string) map[string]string {
	data := map[string]string{}
	for i := range tenants {
		t := &tenants[i]
		if t.DeletionTimestamp != nil || t.Status.Domain == "" {
			continue
		}
		body, _ := json.Marshal(map[string]string{"url": "https://" + consoleHost(t.EffectiveDomain(kernelDomain, tenancyMode)) + "/"})
		data[conciergeLookupKey(t.Status.Domain)] = string(body)
	}
	// The forward of a single-tenant cluster. Only once the tenant is Ready:
	// before that its console does not answer, and a browser sent to a name
	// that is not published yet remembers that it is missing. And never to
	// the kernel's own console, which is the administrators': a tenant whose
	// domain is the kernel domain has no console of its own to be sent to.
	if sole := soleUserTenant(tenants, kernelRealm); sole != nil && sole.Status.Phase == gentianov1alpha1.TenantPhaseReady {
		if domain := sole.EffectiveDomain(kernelDomain, tenancyMode); domain != "" && !servedByKernelEdge(domain, kernelDomain) {
			body, _ := json.Marshal(map[string]string{"url": "https://" + consoleHost(domain) + "/"})
			data[conciergeSingleKey] = string(body)
		}
	}
	return data
}

func (r *ConciergeLookupReconciler) Reconcile(ctx context.Context, _ ctrl.Request) (ctrl.Result, error) {
	tenants := &gentianov1alpha1.TenantList{}
	if err := r.List(ctx, tenants); err != nil {
		return ctrl.Result{}, err
	}
	data := conciergeLookupData(tenants.Items, r.KernelDomain, r.KernelRealm, r.TenancyMode)
	namespace := platformTenantNamespace(tenants.Items, r.KernelRealm)
	if namespace == "" {
		// No platform tenant yet, so nowhere the concierge could run. Its
		// arrival is a tenant event and brings this back.
		return ctrl.Result{}, nil
	}
	key := types.NamespacedName{Name: conciergeLookupConfigMap, Namespace: namespace}

	existing := &corev1.ConfigMap{}
	err := r.Get(ctx, key, existing)
	if errors.IsNotFound(err) {
		cm := &corev1.ConfigMap{}
		cm.Name, cm.Namespace = key.Name, key.Namespace
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
