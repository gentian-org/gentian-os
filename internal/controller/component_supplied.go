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
	"strings"

	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/kernel/secrets"
	"github.com/gentian-org/gentian-os/internal/meta"
)

// Credentials only a person can provide (spec.secrets.supplied).
//
// Each becomes a CredentialRequirement of the tenant -- the declaration its
// Admin Console lists and the custodian writes to -- and a probe beside it:
// an ExternalSecret that makes no Secret and exists for ESO's Ready
// condition, which the custodian reads to say whether it has been set. The
// names, labels and paths are the app Composition's, so a requirement it
// declared is the same one when the app is rendered here. The values reach
// the chart through the component's sensitive Secret
// (component_integrations.go).

// suppliedPath is where the custodian stores one supplied credential.
func suppliedPath(tenant, app, name string) string {
	return secrets.CategoryPath(tenant, app, "supplied/"+name)
}

// suppliedRequirementName is the CredentialRequirement of one.
func suppliedRequirementName(tenant, app, name string) string {
	return fmt.Sprintf("app-%s-%s-%s", tenant, app, name)
}

// suppliedProviderLabel groups a tenant's requirements by the app that
// consumes them, and is what finds those of one app again.
const suppliedProviderLabel = "gentianos.io/credential-provider"

// ensureSuppliedRequirements declares each supplied credential of the
// component's profile with its probe, and removes those of the component
// the profile no longer declares.
func (r *ComponentReconciler) ensureSuppliedRequirements(ctx context.Context, comp *gentianov1alpha1.Component, profile *gentianov1alpha1.ComponentProfile, tenant string) error {
	wanted := map[string]bool{}
	if s := profile.Spec.Secrets; s != nil {
		for _, sc := range s.Supplied {
			if sc.Name == "" || !hasMappedField(sc) {
				continue
			}
			name := suppliedRequirementName(tenant, comp.Name, sc.Name)
			wanted[name] = true
			if err := r.ensureSuppliedRequirement(ctx, comp, tenant, name, sc); err != nil {
				return err
			}
		}
	}
	return r.dropSuppliedRequirements(ctx, comp, tenant, wanted)
}

func (r *ComponentReconciler) ensureSuppliedRequirement(ctx context.Context, comp *gentianov1alpha1.Component, tenant, name string, sc gentianov1alpha1.SuppliedSecret) error {
	path := suppliedPath(tenant, comp.Name, sc.Name)
	labels := map[string]string{
		"gentianos.io/credential-phase": "runtime",
		"gentianos.io/credential-scope": "tenant",
		suppliedProviderLabel:           comp.Name,
		tenantLabel:                     tenant,
		managedByLabel:                  managedByValue,
	}
	description := strings.TrimSpace(fmt.Sprintf("%s The app %s waits for it until it is set here.", strings.TrimSpace(sc.Description), comp.Name))
	display := sc.DisplayName
	if display == "" {
		display = sc.Name
	}
	var fields []gentianov1alpha1.CredentialField
	var data []interface{}
	for _, f := range sc.Fields {
		if f.Key == "" || f.ValuePath == "" {
			continue
		}
		format := f.Format
		if format == "" {
			format = "string"
		}
		fields = append(fields, gentianov1alpha1.CredentialField{Key: f.Key, Format: format, Secret: !f.Public, Example: f.Example})
		data = append(data, map[string]interface{}{
			"secretKey": f.Key,
			"remoteRef": map[string]interface{}{"key": path, "property": f.Key},
		})
	}
	desired := &gentianov1alpha1.CredentialRequirement{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels},
		Spec: gentianov1alpha1.CredentialRequirementSpec{
			DisplayName: display,
			Description: description,
			Phase:       "runtime",
			Scope:       "tenant",
			Tenant:      tenant,
			VaultPath:   path,
			Fields:      fields,
			Validate:    &gentianov1alpha1.CredentialValidation{Type: "noop"},
			ConsumedBy:  []gentianov1alpha1.CredentialConsumer{{Kind: "Component", Name: comp.Name}},
		},
	}
	existing := &gentianov1alpha1.CredentialRequirement{}
	err := r.Get(ctx, types.NamespacedName{Name: name}, existing)
	switch {
	case errors.IsNotFound(err):
		if err := r.Create(ctx, desired); err != nil && !errors.IsAlreadyExists(err) {
			return fmt.Errorf("declare the credential %s: %w", name, err)
		}
	case err != nil:
		return err
	case existing.Labels[managedByLabel] != managedByValue:
		// Declared by something else -- the app Composition, for an app it
		// delivered -- and left to it.
		return fmt.Errorf("the credential %s was declared by something else; it is left as it is", name)
	case !equality.Semantic.DeepEqual(existing.Spec, desired.Spec) || !covers(toInterfaceMap(existing.Labels), toInterfaceMap(labels)):
		existing.Spec = desired.Spec
		if existing.Labels == nil {
			existing.Labels = map[string]string{}
		}
		for k, v := range labels {
			existing.Labels[k] = v
		}
		if err := r.Update(ctx, existing); err != nil {
			return fmt.Errorf("update the credential %s: %w", name, err)
		}
	}

	probe := &unstructured.Unstructured{}
	probe.SetGroupVersionKind(externalSecretGVK)
	probe.SetName(credentialProbePrefix + name)
	probe.SetNamespace(meta.OperatorNamespace)
	probe.SetLabels(map[string]string{
		"gentianos.io/credential-requirement": name,
		"gentianos.io/credential-phase":       "runtime",
		"gentianos.io/credential-scope":       "tenant",
		suppliedProviderLabel:                 comp.Name,
		tenantLabel:                           tenant,
		managedByLabel:                        managedByValue,
	})
	spec := map[string]interface{}{
		"refreshInterval": "1h",
		"secretStoreRef":  secrets.StoreRef(secrets.KernelStore),
		"target":          map[string]interface{}{"creationPolicy": "None"},
		"data":            data,
	}
	have := &unstructured.Unstructured{}
	have.SetGroupVersionKind(externalSecretGVK)
	err = r.Get(ctx, types.NamespacedName{Name: probe.GetName(), Namespace: probe.GetNamespace()}, have)
	switch {
	case errors.IsNotFound(err):
		if err := unstructured.SetNestedField(probe.Object, spec, "spec"); err != nil {
			return err
		}
		if err := r.Create(ctx, probe); err != nil && !errors.IsAlreadyExists(err) {
			return fmt.Errorf("create the probe of %s: %w", name, err)
		}
	case err != nil:
		return err
	case have.GetLabels()[managedByLabel] != managedByValue:
		return fmt.Errorf("the probe of %s was made by something else; it is left as it is", name)
	case !covers(have.Object["spec"], spec):
		patch := client.MergeFrom(have.DeepCopy())
		if err := unstructured.SetNestedField(have.Object, spec, "spec"); err != nil {
			return err
		}
		if err := r.Patch(ctx, have, patch); err != nil {
			return fmt.Errorf("update the probe of %s: %w", name, err)
		}
	}
	return nil
}

// dropSuppliedRequirements removes the requirements and probes this
// reconciler declared for the component that are not wanted any more, and
// with wanted nil all of them. One the app Composition declared carries no
// managed-by label of this reconciler's and is left to it.
func (r *ComponentReconciler) dropSuppliedRequirements(ctx context.Context, comp *gentianov1alpha1.Component, tenant string, wanted map[string]bool) error {
	selector := client.MatchingLabels{suppliedProviderLabel: comp.Name, tenantLabel: tenant, managedByLabel: managedByValue}
	reqs := &gentianov1alpha1.CredentialRequirementList{}
	if err := r.List(ctx, reqs, selector); err != nil {
		return err
	}
	for i := range reqs.Items {
		if wanted[reqs.Items[i].Name] {
			continue
		}
		if err := r.Delete(ctx, &reqs.Items[i]); client.IgnoreNotFound(err) != nil {
			return err
		}
	}
	probes := &unstructured.UnstructuredList{}
	probes.SetGroupVersionKind(externalSecretGVK.GroupVersion().WithKind("ExternalSecretList"))
	if err := r.List(ctx, probes, client.InNamespace(meta.OperatorNamespace), selector); err != nil {
		return err
	}
	for i := range probes.Items {
		if wanted[strings.TrimPrefix(probes.Items[i].GetName(), credentialProbePrefix)] {
			continue
		}
		if err := r.Delete(ctx, &probes.Items[i]); client.IgnoreNotFound(err) != nil {
			return err
		}
	}
	return nil
}

func toInterfaceMap(in map[string]string) map[string]interface{} {
	out := make(map[string]interface{}, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// hasMappedField reports whether a supplied credential has a field the chart
// is told: one with nothing to tell is declared nowhere.
func hasMappedField(sc gentianov1alpha1.SuppliedSecret) bool {
	for _, f := range sc.Fields {
		if f.Key != "" && f.ValuePath != "" {
			return true
		}
	}
	return false
}
