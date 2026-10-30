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
	"encoding/json"
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
// What a profile maps from a vault path -- its database key by key
// (valueMapping.database), a contract's endpoint, user and password
// (valueMapping.integrations) -- reaches its chart through one
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
	// documents are keys of the Secret templated from the others, each a
	// values document of its own; valuesFrom are the release's optional
	// reads of them.
	documents  map[string]interface{}
	valuesFrom []interface{}
}

// optional adds a value the chart may go without: written into the Secret
// as a values document of its own when it is not empty, and read by the
// release as optional valuesFrom. An empty value -- mail on a cluster
// without the relay's credentials -- is then simply not set, where a set
// entry naming it would keep the whole release from installing. raw writes
// the value as it is (a port) rather than as a JSON string.
func (v *sensitiveValues) optional(comp *gentianov1alpha1.Component, valuePath, secretKey string, raw bool) {
	if valuePath == "" {
		return
	}
	head, tail := "", ""
	for _, part := range splitDots(valuePath) {
		quoted, _ := json.Marshal(part)
		head += "{" + string(quoted) + ":"
		tail += "}"
	}
	value := " . | toJson "
	if raw {
		value = " . "
	}
	doc := "values-" + secretKey
	if v.documents == nil {
		v.documents = map[string]interface{}{}
	}
	v.documents[doc] = fmt.Sprintf("{{ with index . %q }}%s{{%s}}%s{{ end }}", secretKey, head, value, tail)
	v.valuesFrom = append(v.valuesFrom, map[string]interface{}{"secretKeyRef": map[string]interface{}{
		"name": sensitiveValuesName(comp), "namespace": comp.Namespace, "key": doc, "optional": true,
	}})
}

func (v *sensitiveValues) add(comp *gentianov1alpha1.Component, valueKey, secretKey, path, property string) {
	if valueKey == "" {
		return
	}
	v.read(secretKey, path, property)
	v.tell(comp, valueKey, secretKey)
}

// read adds a key of the Secret, once, and where its value is.
func (v *sensitiveValues) read(secretKey, path, property string) {
	for _, d := range v.data {
		if d.(map[string]interface{})["secretKey"] == secretKey {
			return
		}
	}
	v.data = append(v.data, map[string]interface{}{
		"secretKey": secretKey,
		"remoteRef": map[string]interface{}{"key": path, "property": property},
	})
}

// tell adds a set entry: the chart value valueKey from a key of the Secret.
func (v *sensitiveValues) tell(comp *gentianov1alpha1.Component, valueKey, secretKey string) {
	if valueKey == "" {
		return
	}
	v.set = append(v.set, map[string]interface{}{
		"name": valueKey,
		"valueFrom": map[string]interface{}{"secretKeyRef": map[string]interface{}{
			"name": sensitiveValuesName(comp), "namespace": comp.Namespace, "key": secretKey,
		}},
	})
}

// componentSensitiveValues is what a profile maps from the tenant's store,
// in a fixed order: the app's own secrets, its database, then each contract
// by name.
func componentSensitiveValues(comp *gentianov1alpha1.Component, profile *gentianov1alpha1.ComponentProfile, tenant string) sensitiveValues {
	var out sensitiveValues
	// The app's own secrets: each generated one the tenant seeded
	// (seedAppPrerequisites), and each field of a credential a person
	// supplies, which the Secret does not have until every field is there
	// -- so the release waits for the person.
	if s := profile.Spec.Secrets; s != nil {
		for _, g := range s.Generated {
			if g.Name == "" || g.ValuePath == "" {
				continue
			}
			out.add(comp, g.ValuePath, "internal-"+g.Name, secrets.CategoryPath(tenant, comp.Name, "internal/"+g.Name), "value")
		}
		for _, sc := range s.Supplied {
			for _, f := range sc.Fields {
				if f.Key == "" || f.ValuePath == "" {
					continue
				}
				out.add(comp, f.ValuePath, "supplied-"+sc.Name+"-"+f.Key, suppliedPath(tenant, comp.Name, sc.Name), f.Key)
			}
		}
	}
	m := profile.Spec.Package.ValueMapping
	if m == nil {
		return out
	}
	// The database the tenant provisioned for this app, key by key -- for a
	// chart that reads host, port and the rest as values rather than a
	// Secret by name. A read replica is the same database here: the names
	// are the ones the app Composition gives them.
	if db := m.Database; db != nil && !operatorPlaced(profile) {
		path := secrets.CategoryPath(tenant, comp.Name, "database")
		out.add(comp, db.HostKey, "db-host", path, "host")
		out.add(comp, db.PortKey, "db-port", path, "port")
		if db.NameKey != "" || db.ReadNameKey != "" {
			out.read("db-name", path, "name")
		}
		out.tell(comp, db.NameKey, "db-name")
		out.tell(comp, db.ReadNameKey, "db-name")
		if db.UserKey != "" || db.ReadUserKey != "" {
			out.read("db-user", path, "user")
		}
		out.tell(comp, db.UserKey, "db-user")
		out.tell(comp, db.ReadUserKey, "db-user")
		out.add(comp, db.PasswordKey, "db-password", path, "password")
	}
	// Object storage, the cache and mail, each read where the tenant's
	// stores wrote it for this app.
	if s3 := m.S3; s3 != nil {
		path := secrets.CategoryPath(tenant, comp.Name, "s3")
		out.add(comp, s3.EndpointKey, "s3-endpoint", path, "endpoint")
		out.add(comp, s3.BucketKey, "s3-bucket", path, "bucket")
		out.add(comp, s3.AccessKeyKey, "s3-access-key", path, "access-key")
		out.add(comp, s3.SecretKeyKey, "s3-secret-key", path, "secret-key")
		out.add(comp, s3.RegionKey, "s3-region", path, "region")
	}
	if cache := m.Cache; cache != nil {
		path := secrets.CategoryPath(tenant, comp.Name, "cache")
		out.add(comp, cache.HostKey, "cache-host", path, "host")
		out.add(comp, cache.PortKey, "cache-port", path, "port")
		out.add(comp, cache.UserKey, "cache-user", path, "user")
		out.add(comp, cache.PasswordKey, "cache-password", path, "password")
	}
	if smtp := m.SMTP; smtp != nil {
		path := secrets.CategoryPath(tenant, comp.Name, "smtp")
		for _, k := range []struct {
			valuePath, key, property string
			raw                      bool
		}{
			{smtp.HostKey, "smtp-host", "host", false},
			{smtp.PortKey, "smtp-port", "port", true},
			{smtp.UserKey, "smtp-user", "user", false},
			{smtp.PasswordKey, "smtp-password", "password", false},
		} {
			if k.valuePath == "" {
				continue
			}
			out.read(k.key, path, k.property)
			out.optional(comp, k.valuePath, k.key, k.raw)
		}
	}
	if imap := m.IMAP; imap != nil {
		path := secrets.CategoryPath(tenant, comp.Name, "imap")
		out.add(comp, imap.HostKey, "imap-host", path, "host")
		out.add(comp, imap.PortKey, "imap-port", path, "port")
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
	if len(v.documents) > 0 {
		spec["target"].(map[string]interface{})["template"] = map[string]interface{}{
			"engineVersion": "v2",
			"mergePolicy":   "Merge",
			"data":          v.documents,
		}
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
	// A template the profile no longer asks for goes too: covers reads only
	// what is written here, and would leave it.
	_, hasTemplate, _ := unstructured.NestedFieldNoCopy(existing.Object, "spec", "target", "template")
	if !covers(existing.Object["spec"], spec) || (hasTemplate && len(v.documents) == 0) {
		patch := client.MergeFrom(existing.DeepCopy())
		if err := unstructured.SetNestedField(existing.Object, spec, "spec"); err != nil {
			return false, err
		}
		if len(v.documents) == 0 {
			unstructured.RemoveNestedField(existing.Object, "spec", "target", "template")
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
