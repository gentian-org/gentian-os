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
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/kernel/secrets"
)

// The values a chart is told without their being release values.
//
// What a profile maps from a vault path -- a contract's endpoint, user and
// password (valueMapping.integrations) -- reaches its chart through one
// ExternalSecret of the component, <component>-sensitive-values, read from
// the tenant's own store, and the release names each key it needs with a
// set entry that takes its value from that Secret. The value is in the
// release only as Helm renders it, never in the Release object; and the
// names are the ones the app Composition gives them, so an app it delivered
// is found the same when it is delivered here.

// sensitiveValuesName is the Secret, and its ExternalSecret, a component's
// mapped values are read from.
func sensitiveValuesName(comp *gentianov1alpha1.Component) string {
	return comp.Name + "-sensitive-values"
}

// sensitiveValues is what one component's ExternalSecret reads and what its
// release is told from it.
type sensitiveValues struct {
	// data are the ExternalSecret's entries: a key of the Secret and where
	// in the tenant's store its value is.
	data []interface{}
	// set are the release's set entries, each a chart value taken from a
	// key of the Secret.
	set []interface{}
}

func (v *sensitiveValues) add(comp *gentianov1alpha1.Component, valueKey, secretKey, path, property string) {
	if valueKey == "" {
		return
	}
	v.data = append(v.data, map[string]interface{}{
		"secretKey": secretKey,
		"remoteRef": map[string]interface{}{"key": path, "property": property},
	})
	v.set = append(v.set, map[string]interface{}{
		"name": valueKey,
		"valueFrom": map[string]interface{}{"secretKeyRef": map[string]interface{}{
			"name": sensitiveValuesName(comp), "namespace": comp.Namespace, "key": secretKey,
		}},
	})
}

// componentSensitiveValues is what a profile maps from the tenant's store,
// in a fixed order: by contract, then endpoint, user, password.
func componentSensitiveValues(comp *gentianov1alpha1.Component, profile *gentianov1alpha1.ComponentProfile, tenant string) sensitiveValues {
	var out sensitiveValues
	m := profile.Spec.Package.ValueMapping
	if m == nil {
		return out
	}
	contracts := make([]string, 0, len(m.Integrations))
	for contract := range m.Integrations {
		contracts = append(contracts, contract)
	}
	sort.Strings(contracts)
	for _, contract := range contracts {
		mapping := m.Integrations[contract]
		path := secrets.ContractPath(tenant, contract)
		out.add(comp, mapping.EndpointKey, "integration-"+contract+"-endpoint", path, "endpoint")
		out.add(comp, mapping.UserKey, "integration-"+contract+"-user", path, "user")
		out.add(comp, mapping.PasswordKey, "integration-"+contract+"-password", path, "password")
	}
	return out
}

// ensureSensitiveValues keeps the component's ExternalSecret for what its
// profile maps, and reports whether its Secret has arrived. With nothing
// mapped it does nothing: the release may still read the Secret, and the
// ExternalSecret is removed once it no longer does (dropSensitiveValues).
func (r *ComponentReconciler) ensureSensitiveValues(ctx context.Context, comp *gentianov1alpha1.Component, tenant string, v sensitiveValues) (bool, error) {
	if len(v.data) == 0 {
		return true, nil
	}
	name := sensitiveValuesName(comp)
	existing := &unstructured.Unstructured{}
	existing.SetGroupVersionKind(externalSecretGVK)
	err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: comp.Namespace}, existing)
	if err != nil && !errors.IsNotFound(err) {
		return false, err
	}
	spec := map[string]interface{}{
		"refreshInterval": "1h",
		// The tenant's own store: the kernel's is not usable from a
		// tenant's namespace.
		"secretStoreRef": secrets.StoreRef(secrets.TenantStore(tenant)),
		"target": map[string]interface{}{
			"name":           name,
			"creationPolicy": "Owner",
			"deletionPolicy": "Retain",
		},
		"data": v.data,
	}
	if errors.IsNotFound(err) {
		desired := &unstructured.Unstructured{}
		desired.SetGroupVersionKind(externalSecretGVK)
		desired.SetName(name)
		desired.SetNamespace(comp.Namespace)
		desired.SetLabels(componentLabels(comp))
		if err := unstructured.SetNestedField(desired.Object, spec, "spec"); err != nil {
			return false, err
		}
		if err := controllerutil.SetControllerReference(comp, desired, r.Scheme); err != nil {
			return false, err
		}
		return false, r.Create(ctx, desired)
	}
	// One this component did not make is not written over: the app
	// Composition writes the same name for an app it delivers, through an
	// object of its own that sets no owner, and two writers would correct
	// each other on every pass -- and the Composition's, once removed, would
	// take the Secret with it.
	if !metav1.IsControlledBy(existing, comp) {
		return false, fmt.Errorf("the ExternalSecret %s was not made by this component; it is left as it is", name)
	}
	// Only what is written here is compared: the API server fills in the
	// schema's defaults (remoteRef's strategies and policies), and a
	// comparison of the whole spec would find drift on every pass.
	if !covers(existing.Object["spec"], spec) {
		patch := client.MergeFrom(existing.DeepCopy())
		if err := unstructured.SetNestedField(existing.Object, spec, "spec"); err != nil {
			return false, err
		}
		if err := r.Patch(ctx, existing, patch); err != nil {
			return false, err
		}
		return false, nil
	}
	return externalSecretSynced(existing), nil
}

// dropSensitiveValues removes the component's ExternalSecret, and with it
// its Secret, once the release no longer reads it: the profile maps nothing
// any more and the release has rolled out without set entries.
func (r *ComponentReconciler) dropSensitiveValues(ctx context.Context, comp *gentianov1alpha1.Component) error {
	existing := &unstructured.Unstructured{}
	existing.SetGroupVersionKind(externalSecretGVK)
	err := r.Get(ctx, types.NamespacedName{Name: sensitiveValuesName(comp), Namespace: comp.Namespace}, existing)
	if errors.IsNotFound(err) || (err == nil && !metav1.IsControlledBy(existing, comp)) {
		return nil
	}
	if err != nil {
		return err
	}
	return client.IgnoreNotFound(r.Delete(ctx, existing))
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
