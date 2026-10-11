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
	"slices"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/gentian-org/gentian-os/api/bundle"
	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/backup"
	"github.com/gentian-org/gentian-os/internal/kernel/secrets"
)

// An app's own secrets, in a bundle and out of it.
//
// An app encrypts and signs what it stores with the secrets its profile had
// the platform make (spec.secrets.generated, an extension's appSecrets, and
// the keys under spec.secrets.derived). The data is in the bundle; without the secrets it was written
// with it is unreadable wherever those are not the ones in the vault: after
// a purge and a new install, in a tenant of another name, on another
// cluster. So an export puts them into the bundle, as one more artefact of
// the app, and a restore sets them before it puts the data back.
//
// Three rules hold throughout.
//
// The values are in nothing but the vault, the operator's memory and the
// encrypted artefact. The operator encrypts and decrypts them itself
// (backup.SealAppSecrets); no Job, no Secret, no ConfigMap and no status is
// handed one. A status lists names.
//
// A bundle says names and values, never where a value goes. A restore writes
// only at the app's own place in the tenant restored into
// (secrets.InternalPath, from that tenant, that app and the name), only for
// an app this restore restores, and only under a name the app's profile
// declares here. What the bundle holds under any other name is not written
// and is named in the result.
//
// A restore replaces the value the vault holds. That is the one exception to
// "the first value stored stays": the value there was made for an app
// installed anew, or for another tenant, and cannot read the bundle's data.
// The app is then handed the new value -- its Secrets read again, its Helm
// releases upgraded with it -- before its data is replaced, and is restarted
// when the data is in.

// restoreSecretsWait bounds the wait for replaced secrets to reach the app:
// its Secrets filled again by the secrets operator, and its Helm releases
// upgraded with them.
const restoreSecretsWait = 10 * time.Minute

// secretsRestoredAnnotation is written on a Helm release that is handed a
// replaced secret as a value, once the Secret it reads holds the new value:
// a change to the object is what makes the provider look at the release
// again now, and not at its next poll. Its value names the restore.
const secretsRestoredAnnotation = "gentianos.io/secrets-restored"

// secretsSchemaVersion is the bundle format an app's own secrets are in
// from.
const secretsSchemaVersion = 4

// forceSyncAnnotation is what the secrets operator re-reads an
// ExternalSecret on, ahead of its refresh interval.
const forceSyncAnnotation = "force-sync"

// BundleWriter is the object store as an export uses it for what the
// operator writes itself. *bundlestore.Store is one.
type BundleWriter interface {
	PutArtefact(ctx context.Context, ref gentianov1alpha1.BundleRef, path string, cipher []byte) error
}

// declaredSecret is one secret an app's profile declares.
type declaredSecret struct {
	// label is how it is named where names are listed: the name, or
	// "<extension>/<name>".
	label string
	// vaultApp is the app whose place in the vault holds it: the app, or
	// the extension's own (SidecarAppName).
	vaultApp string
	// extension is "" for the app's own.
	extension string
	name      string
	// derived says it is a key under spec.secrets.derived: kept at the
	// app's derived/ path, and delivered by the operator itself in the
	// Secret llm-credentials-<app>, not through an ExternalSecret.
	derived bool
}

// derivedLabel is how a key under spec.secrets.derived is named where names
// are listed. An extension's name has no colon in it.
func derivedLabel(key string) string { return "derived:" + key }

// path is where the secret is kept for a tenant: built here, from the
// tenant, the app and the name, and from nothing a bundle says.
func (d declaredSecret) path(tenantName string) string {
	if d.derived {
		return secrets.DerivedKeyPath(tenantName, d.vaultApp, d.name)
	}
	return secrets.InternalPath(tenantName, d.vaultApp, d.name)
}

// read and replace are the Seeder's, for the kind of secret this is.
func (d declaredSecret) read(ctx context.Context, s *secrets.Seeder, tenantName string) (string, bool, error) {
	if d.derived {
		return s.ReadDerivedKey(ctx, tenantName, d.vaultApp, d.name)
	}
	return s.ReadAppSecret(ctx, tenantName, d.vaultApp, d.name)
}

func (d declaredSecret) replace(ctx context.Context, s *secrets.Seeder, tenantName, value string) (bool, error) {
	if d.derived {
		return s.ReplaceDerivedKey(ctx, tenantName, d.vaultApp, d.name, value)
	}
	return s.ReplaceAppSecret(ctx, tenantName, d.vaultApp, d.name, value)
}

// declaredSecrets are the secrets a profile has the platform make for an
// app and for each of its extensions: the generated ones, and the keys
// declared under spec.secrets.derived.
func declaredSecrets(appName string, profile *gentianov1alpha1.ComponentProfile) []declaredSecret {
	if profile == nil {
		return nil
	}
	var out []declaredSecret
	for _, s := range profile.GeneratedSecrets() {
		if s.Name != "" {
			out = append(out, declaredSecret{label: s.Name, vaultApp: appName, name: s.Name})
		}
	}
	for _, ext := range profile.Spec.Extensions {
		if ext.Name == "" {
			continue
		}
		for _, s := range ext.AppSecrets {
			if s.Name != "" {
				out = append(out, declaredSecret{
					label:    backup.ExtensionSecretName(ext.Name, s.Name),
					vaultApp: gentianov1alpha1.SidecarAppName(appName, ext.Name), extension: ext.Name, name: s.Name,
				})
			}
		}
	}
	for _, k := range profile.DerivedSecrets() {
		if k.Key != "" {
			out = append(out, declaredSecret{label: derivedLabel(k.Key), vaultApp: appName, name: k.Key, derived: true})
		}
	}
	return out
}

// heldValue is the value a bundle's document holds for a declared secret.
func heldValue(held *bundle.AppSecrets, d declaredSecret) string {
	switch {
	case held == nil:
		return ""
	case d.derived:
		return held.Derived[d.name]
	case d.extension == "":
		return held.Secrets[d.name]
	}
	return held.Extensions[d.extension][d.name]
}

// hold puts a value into a bundle's document, where a secret of that kind
// goes.
func hold(doc *bundle.AppSecrets, d declaredSecret, value string) {
	switch {
	case d.derived:
		if doc.Derived == nil {
			doc.Derived = map[string]string{}
		}
		doc.Derived[d.name] = value
	case d.extension == "":
		if doc.Secrets == nil {
			doc.Secrets = map[string]string{}
		}
		doc.Secrets[d.name] = value
	default:
		if doc.Extensions == nil {
			doc.Extensions = map[string]map[string]string{}
		}
		if doc.Extensions[d.extension] == nil {
			doc.Extensions[d.extension] = map[string]string{}
		}
		doc.Extensions[d.extension][d.name] = value
	}
}

// heldNames are the names a bundle's document holds, as they are listed.
func heldNames(held *bundle.AppSecrets) []string {
	var out []string
	if held == nil {
		return nil
	}
	for name := range held.Secrets {
		out = append(out, name)
	}
	for ext, values := range held.Extensions {
		for name := range values {
			out = append(out, backup.ExtensionSecretName(ext, name))
		}
	}
	for key := range held.Derived {
		out = append(out, derivedLabel(key))
	}
	sort.Strings(out)
	return out
}

// secretsArtefact is the app's secrets artefact among the artefacts of a
// status entry, nil when there is none.
func secretsArtefact(artefacts []gentianov1alpha1.BundleArtefact) *gentianov1alpha1.BundleArtefact {
	for i := range artefacts {
		if artefacts[i].Kind == bundle.ArtefactSecrets {
			return &artefacts[i]
		}
	}
	return nil
}

// secretsReplaced reports whether a restore replaced one of the app's
// secrets.
func secretsReplaced(entry *gentianov1alpha1.AppExportStatus) bool {
	return entry != nil && entry.Secrets != nil && len(entry.Secrets.Replaced) > 0
}

// --- Export -----------------------------------------------------------------

// captureAppSecrets puts an app's own secrets into the bundle and returns
// the artefact, nil when the app has none stored.
//
// Every secret the profile declares that the vault holds; one it declares
// and the vault does not hold was never made, and is not in the bundle. A
// path that cannot be read is an error: a bundle that left a secret out
// because the vault was away would bring data back unreadable and say
// nothing.
func (r *TenantExportReconciler) captureAppSecrets(
	ctx context.Context,
	export *gentianov1alpha1.TenantExport,
	tenant *gentianov1alpha1.Tenant,
	appName string,
	profile *gentianov1alpha1.ComponentProfile,
	encryption backup.Encryption,
) (*gentianov1alpha1.BundleArtefact, *gentianov1alpha1.AppSecretsStatus, error) {
	declared := declaredSecrets(appName, profile)
	if len(declared) == 0 || r.Reconciler == nil || r.Reconciler.Seeder == nil {
		return nil, nil, nil
	}
	doc := &bundle.AppSecrets{App: appName}
	var names []string
	for _, d := range declared {
		value, found, err := d.read(ctx, r.Reconciler.Seeder, tenant.Name)
		if err != nil {
			return nil, nil, err
		}
		if !found {
			continue
		}
		hold(doc, d, value)
		names = append(names, d.label)
	}
	if doc.Empty() {
		return nil, nil, nil
	}
	if r.Bundles == nil || export.Status.Bundle == nil {
		return nil, nil, fmt.Errorf("%s has secrets of its own and the operator has no way to write them into the bundle", appName)
	}
	passphrase := ""
	if encryption.Mode == gentianov1alpha1.ExportEncryptionPassphrase {
		value, err := r.exportPassphrase(ctx, export)
		if err != nil {
			return nil, nil, err
		}
		passphrase = value
	}
	cipher, err := backup.SealAppSecrets(encryption, passphrase, doc)
	if err != nil {
		return nil, nil, fmt.Errorf("%s: %w", appName, err)
	}
	path := backup.SecretsArtefact(appName)
	if err := r.Bundles.PutArtefact(ctx, *export.Status.Bundle, path, cipher); err != nil {
		return nil, nil, fmt.Errorf("%s: write its secrets into the bundle: %w", appName, err)
	}
	sort.Strings(names)
	return &gentianov1alpha1.BundleArtefact{Kind: bundle.ArtefactSecrets, Name: appName, Path: path},
		&gentianov1alpha1.AppSecretsStatus{Names: names}, nil
}

// exportPassphrase reads the passphrase an export in passphrase mode names,
// from the Secret the requester put in the tenant's namespace.
func (r *TenantExportReconciler) exportPassphrase(ctx context.Context, export *gentianov1alpha1.TenantExport) (string, error) {
	spec := export.Spec.Encryption
	if spec == nil || spec.PassphraseSecretRef == nil {
		return "", fmt.Errorf("mode: passphrase requires spec.encryption.passphraseSecretRef")
	}
	source := &corev1.Secret{}
	if err := r.Get(ctx, types.NamespacedName{Name: spec.PassphraseSecretRef.Name, Namespace: export.Namespace}, source); err != nil {
		return "", fmt.Errorf("passphrase Secret %q in %s: %w", spec.PassphraseSecretRef.Name, export.Namespace, err)
	}
	value := string(source.Data[spec.PassphraseKey()])
	if value == "" {
		return "", fmt.Errorf("passphrase Secret %q has no non-empty key %q", spec.PassphraseSecretRef.Name, spec.PassphraseKey())
	}
	return value, nil
}

// withAppSecrets adds an app's secrets to what was captured for it, when it
// has data in the bundle: secrets with no data to read are of no use there.
func (r *TenantExportReconciler) withAppSecrets(
	ctx context.Context,
	export *gentianov1alpha1.TenantExport,
	tenant *gentianov1alpha1.Tenant,
	appName string,
	profile *gentianov1alpha1.ComponentProfile,
	encryption backup.Encryption,
	entry *gentianov1alpha1.AppExportStatus,
) error {
	if len(entry.Artefacts) == 0 {
		return nil
	}
	artefact, status, err := r.captureAppSecrets(ctx, export, tenant, appName, profile, encryption)
	if err != nil || artefact == nil {
		return err
	}
	entry.Artefacts = append(entry.Artefacts, *artefact)
	entry.Stores = append(entry.Stores, bundle.ArtefactSecrets)
	entry.Secrets = status
	return nil
}

// --- Restore ----------------------------------------------------------------

// restoreAppSecrets sets an app's own secrets from the bundle and waits
// until the app has been handed them. ready says the restore of the app may
// go on; failure, when not empty, is why it cannot.
//
// It runs before the app is paused. The app's Helm releases are upgraded
// with the new values on the way, and an upgrade of a paused app would start
// it again in the middle of its restore.
func (r *TenantRestoreReconciler) restoreAppSecrets(
	ctx context.Context,
	restore *gentianov1alpha1.TenantRestore,
	tenant *gentianov1alpha1.Tenant,
	appName string,
	profile *gentianov1alpha1.ComponentProfile,
) (ready bool, failure string, err error) {
	entry := appStatus(&restore.Status.Apps, appName)
	found := secretsArtefact(entry.Artefacts)
	if found == nil {
		return true, "", nil
	}
	artefact := *found
	if entry.Secrets == nil {
		entry.Secrets = &gentianov1alpha1.AppSecretsStatus{}
	}
	st := entry.Secrets
	// Writing the status reads it back into the restore, and what pointed
	// into the old one no longer does: the entry is looked up again.
	persist := func() error {
		if err := r.persist(ctx, restore); err != nil {
			return err
		}
		entry = appStatus(&restore.Status.Apps, appName)
		if entry.Secrets == nil {
			entry.Secrets = &gentianov1alpha1.AppSecretsStatus{}
		}
		st = entry.Secrets
		return nil
	}
	if st.DeliveredAt != nil {
		return true, "", nil
	}
	if r.Tenant == nil || r.Tenant.Seeder == nil {
		return false, "the bundle holds the app's own secrets and this operator has no vault to set them in", nil
	}
	seeder := r.Tenant.Seeder
	declared := declaredSecrets(appName, profile)
	ns := backup.TenantNamespace(tenant)

	if st.AppliedAt == nil {
		if restore.Status.Bundle == nil {
			return false, "the restore records no bundle to read the app's secrets from", nil
		}
		key, err := r.bundleKey(ctx, restore)
		if err != nil {
			return false, fmt.Sprintf("the key to open the app's secrets with: %v", err), nil
		}
		held, err := r.Bundles.AppSecrets(ctx, *restore.Status.Bundle, artefact.Path, key)
		if err != nil {
			return false, fmt.Sprintf("its own secrets: %v", err), nil
		}
		if !st.Planned {
			// What will change, written down before anything does: were the
			// values replaced first and the record lost, the next pass would
			// find nothing to replace and hand the app nothing.
			st.Names, st.NotDeclared, st.NotHeld, st.Replaced = nil, nil, nil, nil
			declaredLabels := map[string]bool{}
			for _, d := range declared {
				declaredLabels[d.label] = true
				value := heldValue(held, d)
				if value == "" {
					st.NotHeld = append(st.NotHeld, d.label)
					continue
				}
				st.Names = append(st.Names, d.label)
				have, found, err := d.read(ctx, seeder, tenant.Name)
				if err != nil {
					return false, "", err
				}
				// Also where the vault holds the value and the app does
				// not yet: an earlier restore set it and ended before the
				// app was handed it. The app is handed it now.
				lacking := false
				if found && have == value && !entry.Retained {
					lacking, err = r.appLacks(ctx, ns, d.path(tenant.Name), value, directReaders(tenant.Name, []declaredSecret{d}, []string{d.label}))
					if err != nil {
						return false, "", err
					}
				}
				if !found || have != value || lacking {
					st.Replaced = append(st.Replaced, d.label)
				}
			}
			for _, name := range heldNames(held) {
				if !declaredLabels[name] {
					st.NotDeclared = append(st.NotDeclared, name)
				}
			}
			if !entry.Retained && len(st.Replaced) > 0 {
				releases, err := r.secretReleases(ctx, ns, replacedPaths(tenant.Name, declared, st.Replaced), directReaders(tenant.Name, declared, st.Replaced))
				if err != nil {
					return false, "", err
				}
				st.Releases = releases
			}
			st.Planned = true
			if err := persist(); err != nil {
				return false, "", err
			}
		}
		for _, d := range declared {
			if !slices.Contains(st.Replaced, d.label) {
				continue
			}
			value := heldValue(held, d)
			if value == "" {
				continue
			}
			if _, err := d.replace(ctx, seeder, tenant.Name, value); err != nil {
				return false, "", err
			}
			// A declared key reaches the app in a Secret the operator writes
			// itself; it is written now, not at the tenant's next pass.
			if d.derived && !entry.Retained {
				if err := r.Tenant.deliverDerivedKey(ctx, tenant, d.vaultApp, d.name, value); err != nil {
					return false, "", err
				}
			}
		}
		st.AppliedAt = ptrNow()
		// Nothing replaced: the app holds these values already. Nothing
		// running: an uninstalled app's are there for its next install.
		if len(st.Replaced) == 0 || entry.Retained {
			st.DeliveredAt = ptrNow()
			return true, "", persist()
		}
		if err := r.refreshExternalSecrets(ctx, ns, replacedPaths(tenant.Name, declared, st.Replaced), st.AppliedAt.UTC().Format(time.RFC3339)); err != nil {
			return false, "", err
		}
		entry.Phase = gentianov1alpha1.TenantExportPhaseRunning
		entry.Message = fmt.Sprintf("its secrets %s were set from the bundle; waiting for the app to be handed them", strings.Join(st.Replaced, ", "))
		return false, "", persist()
	}

	pending, err := r.secretsPending(ctx, restore, ns, tenant.Name, declared, st)
	if err != nil {
		return false, "", err
	}
	if len(pending) == 0 {
		st.DeliveredAt = ptrNow()
		return true, "", persist()
	}
	if time.Since(st.AppliedAt.Time) > restoreSecretsWait {
		return false, fmt.Sprintf("its secrets %s were set from the bundle and did not reach the app in %s (waiting on %s). "+
			"The vault holds the bundle's values; the app's data was not touched",
			strings.Join(st.Replaced, ", "), restoreSecretsWait, strings.Join(pending, ", ")), nil
	}
	entry.Message = "its secrets were set from the bundle; waiting on " + strings.Join(pending, ", ")
	return false, "", persist()
}

// replacedPaths are the vault paths of the secrets a restore replaced.
func replacedPaths(tenantName string, declared []declaredSecret, replaced []string) map[string]bool {
	out := map[string]bool{}
	for _, d := range declared {
		if slices.Contains(replaced, d.label) {
			out[d.path(tenantName)] = true
		}
	}
	return out
}

// directReaders are the keys of the Secrets the operator writes itself from
// the vault, for the declared keys among the labels: llm-credentials-<app>,
// which holds each key the profile declares under spec.secrets.derived.
func directReaders(tenantName string, declared []declaredSecret, labels []string) []secretReader {
	var out []secretReader
	for _, d := range declared {
		if d.derived && slices.Contains(labels, d.label) {
			out = append(out, secretReader{target: modelCredentialsSecretName(d.vaultApp), key: d.name,
				path: d.path(tenantName), property: "value", direct: true})
		}
	}
	return out
}

// deliverDerivedKey writes one declared key into the app's Secret
// llm-credentials-<app>, where the operator delivers such keys, when that
// Secret is there and the operator's. Where it is not, the app has not been
// given model access yet, and the tenant's reconcile writes the Secret with
// what the vault holds.
func (r *TenantReconciler) deliverDerivedKey(ctx context.Context, tenant *gentianov1alpha1.Tenant, app, key, value string) error {
	secret := &corev1.Secret{}
	err := r.Get(ctx, types.NamespacedName{Name: modelCredentialsSecretName(app), Namespace: tenantNamespaceName(tenant)}, secret)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !ownsModelCredentials(secret, tenant.Name, app) || string(secret.Data[key]) == value {
		return nil
	}
	patch := client.MergeFrom(secret.DeepCopy())
	if secret.Data == nil {
		secret.Data = map[string][]byte{}
	}
	secret.Data[key] = []byte(value)
	return r.Patch(ctx, secret, patch)
}

// secretReader is one key of a Secret the secrets operator fills from a
// vault path.
type secretReader struct {
	externalSecret string
	target         string
	key            string
	path           string
	property       string
	// direct says the operator writes the Secret itself: no ExternalSecret
	// fills it, and where it is not there, nothing waits for it.
	direct bool
}

// secretReaders are the keys of Secrets in a namespace that are filled from
// one of the paths.
func (r *TenantRestoreReconciler) secretReaders(ctx context.Context, ns string, paths map[string]bool) ([]secretReader, []unstructured.Unstructured, error) {
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(externalSecretGVK.GroupVersion().WithKind(externalSecretGVK.Kind + "List"))
	if err := r.List(ctx, list, client.InNamespace(ns)); err != nil {
		return nil, nil, fmt.Errorf("list the ExternalSecrets in %s: %w", ns, err)
	}
	var readers []secretReader
	var objects []unstructured.Unstructured
	for i := range list.Items {
		es := &list.Items[i]
		target, _, _ := unstructured.NestedString(es.Object, "spec", "target", "name")
		if target == "" {
			target = es.GetName()
		}
		data, _, _ := unstructured.NestedSlice(es.Object, "spec", "data")
		reads := false
		for _, raw := range data {
			item, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			ref, ok := item["remoteRef"].(map[string]any)
			if !ok {
				continue
			}
			path, _ := ref["key"].(string)
			if !paths[path] {
				continue
			}
			key, _ := item["secretKey"].(string)
			property, _ := ref["property"].(string)
			readers = append(readers, secretReader{externalSecret: es.GetName(), target: target, key: key, path: path, property: property})
			reads = true
		}
		if reads {
			objects = append(objects, *es)
		}
	}
	return readers, objects, nil
}

// appLacks reports whether a Secret that is filled from the path holds
// another value than the one given.
func (r *TenantRestoreReconciler) appLacks(ctx context.Context, ns, path, value string, direct []secretReader) (bool, error) {
	readers, _, err := r.secretReaders(ctx, ns, map[string]bool{path: true})
	if err != nil {
		return false, err
	}
	for _, rd := range append(readers, direct...) {
		if rd.property != "" && rd.property != "value" {
			continue
		}
		target := &corev1.Secret{}
		err := r.Get(ctx, types.NamespacedName{Name: rd.target, Namespace: ns}, target)
		if apierrors.IsNotFound(err) {
			if rd.direct {
				continue
			}
			return true, nil
		}
		if err != nil {
			return false, err
		}
		if string(target.Data[rd.key]) != value {
			return true, nil
		}
	}
	return false, nil
}

// refreshExternalSecrets tells the secrets operator to read again, now,
// every ExternalSecret of the namespace that reads one of the paths.
func (r *TenantRestoreReconciler) refreshExternalSecrets(ctx context.Context, ns string, paths map[string]bool, stamp string) error {
	_, objects, err := r.secretReaders(ctx, ns, paths)
	if err != nil {
		return err
	}
	for i := range objects {
		es := &objects[i]
		patch := client.MergeFrom(es.DeepCopy())
		annotations := es.GetAnnotations()
		if annotations == nil {
			annotations = map[string]string{}
		}
		annotations[forceSyncAnnotation] = stamp
		es.SetAnnotations(annotations)
		if err := r.Patch(ctx, es, patch); err != nil {
			return fmt.Errorf("refresh ExternalSecret %s/%s: %w", ns, es.GetName(), err)
		}
	}
	return nil
}

// secretReleases are the Helm releases that are handed, as a value, a key of
// a Secret filled from one of the paths, each with the revision it is at.
func (r *TenantRestoreReconciler) secretReleases(ctx context.Context, ns string, paths map[string]bool, direct []secretReader) ([]gentianov1alpha1.SecretsRelease, error) {
	readers, _, err := r.secretReaders(ctx, ns, paths)
	readers = append(readers, direct...)
	if err != nil || len(readers) == 0 {
		return nil, err
	}
	reads := func(ref map[string]any) bool {
		name, _ := ref["name"].(string)
		namespace, _ := ref["namespace"].(string)
		key, _ := ref["key"].(string)
		if namespace != ns {
			return false
		}
		return slices.ContainsFunc(readers, func(rd secretReader) bool {
			return rd.target == name && (key == "" || rd.key == key)
		})
	}
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(helmReleaseGVK.GroupVersion().WithKind(helmReleaseGVK.Kind + "List"))
	if err := r.List(ctx, list); err != nil {
		return nil, fmt.Errorf("list the Helm releases: %w", err)
	}
	var out []gentianov1alpha1.SecretsRelease
	for i := range list.Items {
		release := &list.Items[i]
		taken := false
		for _, field := range []string{"set", "valuesFrom"} {
			items, _, _ := unstructured.NestedSlice(release.Object, "spec", "forProvider", field)
			for _, raw := range items {
				item, ok := raw.(map[string]any)
				if !ok {
					continue
				}
				// set[].valueFrom.secretKeyRef, valuesFrom[].secretKeyRef
				if from, ok := item["valueFrom"].(map[string]any); ok {
					item = from
				}
				if ref, ok := item["secretKeyRef"].(map[string]any); ok && reads(ref) {
					taken = true
				}
			}
		}
		if taken {
			out = append(out, gentianov1alpha1.SecretsRelease{Name: release.GetName(), Revision: releaseRevision(release)})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// releaseRevision is the revision a Helm release is at, by the provider.
func releaseRevision(release *unstructured.Unstructured) int64 {
	revision, _, _ := unstructured.NestedInt64(release.Object, "status", "atProvider", "revision")
	return revision
}

// secretsPending names what has not been handed the replaced secrets yet:
// first the Secrets the secrets operator fills from them, compared key by
// key with what the vault holds; then, once those are in, the Helm releases
// that take one as a value, each of which has to have been upgraded since.
func (r *TenantRestoreReconciler) secretsPending(
	ctx context.Context,
	restore *gentianov1alpha1.TenantRestore,
	ns, tenantName string,
	declared []declaredSecret,
	st *gentianov1alpha1.AppSecretsStatus,
) ([]string, error) {
	paths := replacedPaths(tenantName, declared, st.Replaced)
	readers, _, err := r.secretReaders(ctx, ns, paths)
	if err != nil {
		return nil, err
	}
	readers = append(readers, directReaders(tenantName, declared, st.Replaced)...)
	stored := map[string]map[string]string{}
	var pending []string
	for _, rd := range readers {
		record, read := stored[rd.path]
		if !read {
			record, err = r.Tenant.Seeder.Read(ctx, rd.path)
			if err != nil {
				return nil, fmt.Errorf("read %s back: %w", rd.path, err)
			}
			stored[rd.path] = record
		}
		property := rd.property
		if property == "" {
			property = "value"
		}
		target := &corev1.Secret{}
		err := r.Get(ctx, types.NamespacedName{Name: rd.target, Namespace: ns}, target)
		if err != nil && !apierrors.IsNotFound(err) {
			return nil, err
		}
		if err != nil && rd.direct {
			continue
		}
		if err != nil || string(target.Data[rd.key]) != record[property] {
			name := "Secret " + rd.target
			if !slices.Contains(pending, name) {
				pending = append(pending, name)
			}
		}
	}
	if len(pending) > 0 {
		return pending, nil
	}

	mark := restore.Name + "@" + st.AppliedAt.UTC().Format(time.RFC3339)
	for _, rec := range st.Releases {
		release := &unstructured.Unstructured{}
		release.SetGroupVersionKind(helmReleaseGVK)
		if err := r.Get(ctx, types.NamespacedName{Name: rec.Name}, release); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return nil, err
		}
		if releaseRevision(release) > rec.Revision && crossplaneObjectReady(release) {
			continue
		}
		pending = append(pending, "release "+rec.Name)
		if release.GetAnnotations()[secretsRestoredAnnotation] == mark {
			continue
		}
		patch := client.MergeFrom(release.DeepCopy())
		annotations := release.GetAnnotations()
		if annotations == nil {
			annotations = map[string]string{}
		}
		annotations[secretsRestoredAnnotation] = mark
		release.SetAnnotations(annotations)
		if err := r.Patch(ctx, release, patch); err != nil {
			return nil, fmt.Errorf("have release %s read its values again: %w", rec.Name, err)
		}
	}
	return pending, nil
}

// secretsNote is what a restore's result says of an app's own secrets, from
// what was done with them.
func secretsNote(entry *gentianov1alpha1.AppExportStatus) string {
	if entry == nil || entry.Secrets == nil || entry.Secrets.AppliedAt == nil {
		return ""
	}
	st := entry.Secrets
	var parts []string
	switch {
	case len(st.Replaced) > 0 && entry.Retained:
		parts = append(parts, "its own secrets "+strings.Join(st.Replaced, ", ")+" were set to the bundle's, for its next install")
	case len(st.Replaced) > 0:
		parts = append(parts, "its own secrets "+strings.Join(st.Replaced, ", ")+" were replaced by the bundle's, and the app was restarted with them")
	case len(st.Names) > 0:
		parts = append(parts, "its own secrets are the bundle's already; none was changed")
	}
	if len(st.NotHeld) > 0 {
		parts = append(parts, "the bundle holds no value for "+strings.Join(st.NotHeld, ", ")+", which the app's profile here declares: left as it is")
	}
	if len(st.NotDeclared) > 0 {
		parts = append(parts, "the bundle also holds "+strings.Join(st.NotDeclared, ", ")+", which the app's profile here does not declare: not written")
	}
	return strings.Join(parts, "; ")
}
