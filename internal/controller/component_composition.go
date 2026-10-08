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

	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/profilebundle"
)

// How a chart package is delivered.
//
// A Component is the instance of every app a tenant has, and this reconciler
// is what installs it. For a chart it has two ways of doing that, and which
// one is decided by what the profile asks for, never by which profile it is.
//
// DIRECTLY, as a Helm release this reconciler writes, when the chart needs
// only what is rendered here: the profile's own values, the facts of the
// cluster (valueMapping.platform), and a database handed over as the name of
// a Secret. That is the desktop and the consoles.
//
// THROUGH THE APP COMPOSITION when it needs anything else: an identity
// provider client of its own, generated or derived secrets, a database, cache,
// object store, mail account or model gateway mapped into chart values key by
// key, a post-install job, a sidecar release. Rendering those is the app
// Composition's, and it is a great deal of rendering. The Component writes
// the App claim that Composition answers, owns it, and is Ready when it is.
// The claim used to be emitted by the tenant's Composition straight from the
// list of apps, which is why an installed app was a list entry and a claim
// and never a Component.
//
// The second way shrinks as this reconciler learns to render what it now
// hands over, and is gone when composedDelivery can no longer be true. Until
// then nothing is installed twice and nothing is half-installed: a profile is
// delivered wholly one way or wholly the other.

// composedDelivery reports whether a chart package needs the app Composition.
func composedDelivery(profile *gentianov1alpha1.ComponentProfile) bool {
	spec := &profile.Spec
	if spec.Package.Chart == nil {
		return false
	}
	if spec.Secrets != nil || spec.Hooks != nil || len(spec.Extensions) > 0 || len(spec.Integrations) > 0 {
		return true
	}
	if req := spec.Requires; req != nil && req.Services != nil {
		s := req.Services
		if s.Identity != nil || s.Storage != nil || s.Cache != nil || s.Mail != nil || s.MCP != nil || s.LLM != nil {
			return true
		}
	}
	if m := spec.Package.ValueMapping; m != nil {
		if m.OIDC != nil || m.Cache != nil || m.LLM != nil || m.SMTP != nil || m.IMAP != nil || m.Volumes != nil || len(m.Integrations) > 0 {
			return true
		}
		// A database handed over as a Secret's name is rendered here. One
		// mapped key by key is the Composition's.
		if db := m.Database; db != nil {
			only := gentianov1alpha1.DatabaseValueMapping{SecretNameKey: db.SecretNameKey}
			if *db != only {
				return true
			}
		}
	}
	return false
}

// defaultAppComposition renders every app whose bundle brings no Composition
// of its own (crossplane/compositions/app-default.yaml).
const defaultAppComposition = "app-default"

// appComposition answers which Composition renders a component: the one its
// profile's bundle brings, or "" for the platform's.
//
// A profile's own Composition is used on one condition, and it is the
// condition under which it is known to be the profile's own: the install is
// pinned to a digest, and the bundle that digest names -- verified against
// the cluster just before this -- carries it. That bundle can only have come
// from a catalogue of the whole cluster, and its Composition can only be
// app-<profile> (profilebundle.Check). spec.package.composition alone selects
// nothing: a name in a profile is not a Composition anybody vouched for, and
// a profile could otherwise have itself rendered by another app's.
func appComposition(comp *gentianov1alpha1.Component, profile *gentianov1alpha1.ComponentProfile) string {
	if comp.Spec.ProfileRef.Digest == "" || profile.Spec.Package.Composition == "" {
		return ""
	}
	if own := profilebundle.OwnComposition(profile); own == profile.Spec.Package.Composition {
		return own
	}
	return ""
}

// ensureAppClaim keeps the App claim the app Composition renders this
// component from, and reports whether what it rendered is ready.
//
// Named after the component, in its namespace, and owned by it: removing the
// Component removes the claim, and with it everything the Composition made.
func (r *ComponentReconciler) ensureAppClaim(
	ctx context.Context, comp *gentianov1alpha1.Component, tenant *gentianov1alpha1.Tenant, zone edgeZone, pull pullSecrets,
	composition string,
) (bool, string, error) {
	if composition == "" {
		composition = defaultAppComposition
	}
	spec := map[string]interface{}{
		"compositionUpdatePolicy": "Automatic",
		// Which Composition renders the app, said rather than left to the
		// definition's default: a claim that names none keeps whichever it
		// was given last, and that has to stop being the profile's own when
		// the profile no longer brings one.
		"compositionRef":  map[string]interface{}{"name": composition},
		"profileRef":      map[string]interface{}{"name": comp.Spec.ProfileRef.Name},
		"tenantNamespace": comp.Namespace,
		"domain":          zone.domain,
		// The tenant's realm, by the one rule (keycloak.RealmName). The
		// Composition used to take the tenant's name for it, which is the
		// same realm only for a tenant that names none of its own.
		"realm": keycloakRealmName(tenant),
	}
	// Which of the tenant's pull Secrets the Composition names on the release
	// and in its values. Names, and the Composition looks for them in
	// tenantNamespace only.
	if claimed := appClaimPullSecrets(pull); len(claimed) > 0 {
		spec["pullSecrets"] = claimed
	}
	if len(comp.Spec.Addons) > 0 {
		addons := make([]interface{}, 0, len(comp.Spec.Addons))
		for _, a := range comp.Spec.Addons {
			addons = append(addons, a)
		}
		spec["addons"] = addons
	}
	if cfg := comp.Spec.Config; cfg != nil {
		config := map[string]interface{}{}
		if cfg.Replicas != nil {
			config["replicas"] = int64(*cfg.Replicas)
		}
		if cfg.ExtraValues != nil && len(cfg.ExtraValues.Raw) > 0 {
			values := map[string]interface{}{}
			if err := decodeJSONObject(cfg.ExtraValues.Raw, &values); err != nil {
				return false, "", fmt.Errorf("config.extraValues is not an object: %w", err)
			}
			config["extraValues"] = values
		}
		if len(config) > 0 {
			spec["config"] = config
		}
	}

	desired := &unstructured.Unstructured{}
	desired.SetGroupVersionKind(appClaimGVK)
	desired.SetName(comp.Name)
	desired.SetNamespace(comp.Namespace)
	desired.SetLabels(map[string]string{
		tenantLabel:                    tenant.Name,
		"gentianos.io/app":             comp.Name,
		componentLabel:                 comp.Name,
		"gentianos.io/managed-by":      "crossplane",
		"app.kubernetes.io/managed-by": managedByValue,
	})
	if err := unstructured.SetNestedField(desired.Object, spec, "spec"); err != nil {
		return false, "", err
	}
	if err := controllerutil.SetControllerReference(comp, desired, r.Scheme); err != nil {
		return false, "", err
	}

	existing := &unstructured.Unstructured{}
	existing.SetGroupVersionKind(appClaimGVK)
	err := r.Get(ctx, types.NamespacedName{Name: comp.Name, Namespace: comp.Namespace}, existing)
	if errors.IsNotFound(err) {
		return false, "claim created; waiting for the app to be composed", r.Create(ctx, desired)
	}
	if err != nil {
		return false, "", err
	}
	// A claim something else controls is not taken from it. On a cluster
	// installed before components owned their claims that something is the
	// tenant's Composition, which stops rendering the claim once it is
	// updated and removes it; this then writes its own. Until then the app is
	// not this component's to report on, and saying so is better than two
	// controllers correcting each other's writes.
	if owner := metav1.GetControllerOf(existing); owner != nil && owner.UID != comp.GetUID() {
		return false, fmt.Sprintf("the App claim is still controlled by %s %s; waiting for it to be released", owner.Kind, owner.Name), nil
	}
	// Only what this reconciler writes is compared. Crossplane adds to a
	// claim's spec (resourceRef, the composition it selected), and comparing
	// the whole of it would find a difference on every pass.
	patch := client.MergeFrom(existing.DeepCopy())
	changed := false
	for _, field := range []string{"profileRef", "tenantNamespace", "domain", "realm", "addons", "config", "pullSecrets", "compositionUpdatePolicy", "compositionRef"} {
		want, wanted := spec[field]
		have, has, _ := unstructured.NestedFieldNoCopy(existing.Object, "spec", field)
		switch {
		case !wanted && has:
			unstructured.RemoveNestedField(existing.Object, "spec", field)
			changed = true
		case wanted && (!has || !equality.Semantic.DeepEqual(have, want)):
			if err := unstructured.SetNestedField(existing.Object, want, "spec", field); err != nil {
				return false, "", err
			}
			changed = true
		}
	}
	// A claim nobody controls is adopted, not replaced: replacing it would
	// uninstall the app.
	if !ownedBy(existing, comp) {
		if err := controllerutil.SetControllerReference(comp, existing, r.Scheme); err != nil {
			return false, "", fmt.Errorf("adopt the App claim: %w", err)
		}
		changed = true
	}
	if changed {
		if err := r.Patch(ctx, existing, patch); err != nil {
			return false, "", err
		}
	}
	if !appClaimIsReady(existing) {
		// The provider's own account of a release that is failing, when it
		// has one: "waiting" is no answer to a chart that cannot be pulled.
		if failure := r.composedReleaseMessage(ctx, existing); failure != "" {
			return false, failure, nil
		}
		return false, "waiting for the app to be composed and its release to deploy", nil
	}
	return true, "composed and deployed", nil
}

func ownedBy(obj client.Object, owner client.Object) bool {
	for _, ref := range obj.GetOwnerReferences() {
		if ref.UID == owner.GetUID() {
			return true
		}
	}
	return false
}
