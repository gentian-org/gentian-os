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
	"regexp"
	"strings"

	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/apprender"
	"github.com/gentian-org/gentian-os/internal/credentialprobe"
	"github.com/gentian-org/gentian-os/internal/kernel/secrets"
	"github.com/gentian-org/gentian-os/internal/meta"
)

// The credentials of an action (spec.secrets.supplied of a profile of class
// action). An action that calls a service outside the cluster needs what
// only a person can provide. It is asked for as an app's is: a
// CredentialRequirement of the tenant, set in its Admin Console, kept in the
// vault under the action's own path. It reaches a run as files: one Secret
// per installed action in the tenant's namespace for runs, read through the
// tenant's own store, which the dispatcher's table names and a run's pod of
// that action mounts. The dispatcher reads no Secret, and neither does a pod.
//
// For an action a field's valuePath is the name its value has in the file:
// the file of a credential is <name>.env, a line NAME='value' per field,
// quoted as a shell reads it.

// runCredentialsLabel marks what is kept here for an action's credentials:
// the requirement, its probe and the ExternalSecret in the namespace for
// runs. Its value is the action.
const runCredentialsLabel = "gentianos.io/run-credentials"

// runCredentialsName is the Secret, in a tenant's namespace for runs, that
// holds the credentials of one installed action.
func runCredentialsName(action string) string { return "run-credentials-" + action }

var credentialVariable = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// actionCredentials are the credentials an action's profile asks a person
// for: the entries with a field.
func actionCredentials(profile *gentianov1alpha1.ComponentProfile) []gentianov1alpha1.SuppliedSecret {
	var out []gentianov1alpha1.SuppliedSecret
	for _, sc := range profile.SuppliedSecrets() {
		if sc.Name != "" && len(sc.Fields) > 0 {
			out = append(out, sc)
		}
	}
	return out
}

// actionCredentialsRefusal is why an action's credentials cannot be written
// as files, "" when they can: each field's valuePath is the name of a
// variable, once per file.
func actionCredentialsRefusal(profile *gentianov1alpha1.ComponentProfile) string {
	for _, sc := range actionCredentials(profile) {
		seen := map[string]bool{}
		for _, f := range sc.Fields {
			if !credentialVariable.MatchString(f.ValuePath) {
				return fmt.Sprintf("secrets.supplied[%s].fields[%s].valuePath is %q: for an action it is the name the value has in the credential's file, letters, digits and underscores",
					sc.Name, f.Key, f.ValuePath)
			}
			if seen[f.ValuePath] {
				return fmt.Sprintf("secrets.supplied[%s] names %s twice", sc.Name, f.ValuePath)
			}
			seen[f.ValuePath] = true
		}
	}
	return ""
}

// actionAwaits is what an installed action waits for before a run of it
// starts, "" when nothing: the credentials its profile asks a person for
// that are not set.
func actionAwaits(ctx context.Context, c client.Reader, tenant string, comp *gentianov1alpha1.Component, profile *gentianov1alpha1.ComponentProfile) string {
	if len(actionCredentials(profile)) == 0 {
		return ""
	}
	awaited := credentialprobe.Text(credentialprobe.AwaitedSupplied(ctx, c, tenant, comp.Name, profile))
	if awaited == "" {
		return ""
	}
	return "The action " + awaited + ". No run of it starts until then."
}

// runCredentialsSecret is the ExternalSecret that writes an action's
// credentials into the tenant's namespace for runs: a file per credential.
func runCredentialsSecret(tenant, namespace, action string, supplied []gentianov1alpha1.SuppliedSecret) *unstructured.Unstructured {
	files := map[string]any{}
	var data []any
	for i, sc := range supplied {
		path := secrets.CategoryPath(tenant, action, "supplied/"+sc.Name)
		var lines strings.Builder
		for j, f := range sc.Fields {
			key := fmt.Sprintf("c%df%d", i, j)
			data = append(data, secrets.RemoteRef(key, path, f.Key))
			// Single quotes, and a quote in the value closed, escaped
			// and opened again: nothing in a value is read as anything
			// but the value.
			fmt.Fprintf(&lines, "%s='{{ .%s | replace \"'\" \"'\\\\''\" }}'\n", f.ValuePath, key)
		}
		files[sc.Name+".env"] = lines.String()
	}
	name := runCredentialsName(action)
	es := secrets.ExternalSecret(name, namespace, secrets.TenantStore(tenant), map[string]any{
		"name":           name,
		"creationPolicy": "Owner",
		"template":       map[string]any{"engineVersion": "v2", "data": files},
	}, data)
	es.SetLabels(map[string]string{
		managedByLabel: managedByValue, tenantLabel: tenant, runCredentialsLabel: action,
	})
	return es
}

// ensureUnstructured creates desired or brings what is there to its spec and
// labels, on a real difference only.
func ensureUnstructured(ctx context.Context, c client.Client, desired *unstructured.Unstructured) error {
	return ensureObject(ctx, c, desired, func(existing *unstructured.Unstructured) bool {
		if covers(existing.Object["spec"], desired.Object["spec"]) && equality.Semantic.DeepEqual(existing.GetLabels(), desired.GetLabels()) {
			return false
		}
		existing.Object["spec"] = desired.Object["spec"]
		existing.SetLabels(desired.GetLabels())
		return true
	})
}

// pruneUnstructured deletes, of one kind and where the selector looks, what
// carries runCredentialsLabel and is not kept.
func pruneUnstructured(ctx context.Context, c client.Client, list *unstructured.UnstructuredList, keep map[string]bool, in ...client.ListOption) error {
	if err := c.List(ctx, list, append(in, client.HasLabels{runCredentialsLabel})...); err != nil {
		return err
	}
	for i := range list.Items {
		if keep[list.Items[i].GetName()] {
			continue
		}
		if err := c.Delete(ctx, &list.Items[i]); client.IgnoreNotFound(err) != nil {
			return err
		}
	}
	return nil
}

func externalSecretList() *unstructured.UnstructuredList {
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(secrets.ExternalSecretGVK.GroupVersion().WithKind(secrets.ExternalSecretGVK.Kind + "List"))
	return list
}

// ensureActionRequirements asks for the credentials of every installed
// action, Ready or not: an action is not Ready before they are set. What is
// asked for an action that is gone is taken away.
func (r *RunsReconciler) ensureActionRequirements(ctx context.Context, gathered map[string]*tenantRuns) error {
	requirements, probes := map[string]bool{}, map[string]bool{}
	for _, runs := range gathered {
		for _, asking := range runs.asking {
			action := asking.comp.Name
			waits := fmt.Sprintf("No run of the action %s starts until it is set here.", action)
			consumer := gentianov1alpha1.CredentialConsumer{Kind: "Component", Name: action}
			for _, sc := range actionCredentials(asking.profile) {
				for _, obj := range apprender.SuppliedRequirement(runs.tenant.Name, action, meta.OperatorNamespace, consumer, sc, waits) {
					labels := obj.GetLabels()
					labels[managedByLabel], labels[runCredentialsLabel] = managedByValue, action
					obj.SetLabels(labels)
					if obj.GetKind() == secrets.ExternalSecretGVK.Kind {
						probes[obj.GetName()] = true
					} else {
						requirements[obj.GetName()] = true
					}
					if err := ensureUnstructured(ctx, r.Client, obj); err != nil {
						return fmt.Errorf("ensure %s %s: %w", obj.GetKind(), obj.GetName(), err)
					}
				}
			}
		}
	}
	asked := &unstructured.UnstructuredList{}
	asked.SetGroupVersionKind(gentianov1alpha1.GroupVersion.WithKind("CredentialRequirementList"))
	if err := pruneUnstructured(ctx, r.Client, asked, requirements); err != nil {
		return err
	}
	return pruneUnstructured(ctx, r.Client, externalSecretList(), probes, client.InNamespace(meta.OperatorNamespace))
}

// ensureRunCredentials keeps, in a tenant's namespace for runs, the Secret
// of each Ready action that asks for credentials, and no other.
func (r *RunsReconciler) ensureRunCredentials(ctx context.Context, runs *tenantRuns, namespace string) error {
	keep := map[string]bool{}
	for _, action := range runs.actions {
		if action.runnable.Credentials == "" {
			continue
		}
		keep[action.runnable.Credentials] = true
		desired := runCredentialsSecret(runs.tenant.Name, namespace, action.comp.Name, actionCredentials(action.profile))
		if err := ensureUnstructured(ctx, r.Client, desired); err != nil {
			return fmt.Errorf("ensure ExternalSecret %s: %w", desired.GetName(), err)
		}
	}
	return pruneUnstructured(ctx, r.Client, externalSecretList(), keep, client.InNamespace(namespace))
}

// credentialsOf is the Secret the dispatcher's table names for an action,
// "" for one that asks for none.
func credentialsOf(comp *gentianov1alpha1.Component, profile *gentianov1alpha1.ComponentProfile) string {
	if len(actionCredentials(profile)) == 0 {
		return ""
	}
	return runCredentialsName(comp.Name)
}
