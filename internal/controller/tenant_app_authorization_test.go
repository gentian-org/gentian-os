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
	"reflect"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/gentian-org/gentian-os/api/bundle"
	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
	"github.com/gentian-org/gentian-os/internal/arbiter"
)

// fakeArbiter is the platform path: one store per tenant/app.
type fakeArbiter struct {
	stores map[string]*arbiter.StoreExport
	purged []string
}

func (f *fakeArbiter) Export(_ context.Context, tenant, component string) (*arbiter.StoreExport, error) {
	if s, ok := f.stores[tenant+"/"+component]; ok {
		return s, nil
	}
	return nil, arbiter.ErrNoStore
}

func (f *fakeArbiter) Restore(_ context.Context, tenant, component string, held *arbiter.StoreExport) error {
	f.stores[tenant+"/"+component] = held
	return nil
}

func (f *fakeArbiter) Purge(_ context.Context, tenant, component string) error {
	f.purged = append(f.purged, tenant+"/"+component)
	return nil
}

func (f *fakeArbiter) PurgeTenant(_ context.Context, tenant string) error {
	f.purged = append(f.purged, tenant)
	return nil
}

// An export carries the app's authorization store, encrypted, and a restore
// puts exactly that back, once, into the app's store in the tenant restored
// into; a tenant deleted with its data deletes its stores.
func TestAnAuthorizationStoreGoesIntoTheBundleAndComesBack(t *testing.T) {
	w := newSecretsWorld(t, nil)
	ctx := context.Background()
	model := json.RawMessage(`{"schema_version":"1.1","type_definitions":[{"type":"user"}]}`)
	held := &arbiter.StoreExport{Model: model, Tuples: []arbiter.Tuple{{User: "user:demo/u1", Relation: "owner", Object: "project:1"}}}
	fga := &fakeArbiter{stores: map[string]*arbiter.StoreExport{"demo/wiki": held}}
	w.er.Authorization = fga
	w.r.Authorization = fga
	for _, obj := range []*corev1.ConfigMap{
		{ObjectMeta: metav1.ObjectMeta{Name: arbiter.StoresConfigMap, Namespace: arbiter.Namespace}, Data: map[string]string{"demo.wiki": `{"store":"s1"}`}},
		{ObjectMeta: metav1.ObjectMeta{Name: systemEnginesConfigMap, Namespace: "kernel-control"}, Data: map[string]string{EngineAuthorization: engineWanted}},
	} {
		if err := w.c.Create(ctx, obj); err != nil {
			t.Fatal(err)
		}
	}

	entry := gentianov1alpha1.AppExportStatus{Name: "wiki"}
	if err := w.er.withAppAuthorization(ctx, w.export, planTenant("demo", "wiki"), "wiki", passphraseEncryption, &entry); err != nil {
		t.Fatal(err)
	}
	a := artefactOf(entry.Artefacts, bundle.ArtefactAuthorization)
	if a == nil || a.Path != "authorization/wiki.json" || entry.Authorization == nil || entry.Authorization.Tuples != 1 {
		t.Fatalf("entry = %+v", entry)
	}
	for path, cipher := range w.bundles.objects {
		if strings.HasSuffix(path, a.Path) && strings.Contains(string(cipher), "project:1") {
			t.Fatal("the store is in the bundle in the clear")
		}
	}
	// Another app, with no store recorded, has none in the bundle.
	plain := gentianov1alpha1.AppExportStatus{Name: "drive"}
	if err := w.er.withAppAuthorization(ctx, w.export, planTenant("demo", "drive"), "drive", passphraseEncryption, &plain); err != nil || len(plain.Artefacts) != 0 {
		t.Fatalf("drive: %+v %v", plain, err)
	}

	delete(fga.stores, "demo/wiki")
	restore := w.restore(t)
	restore.Status.Bundle = restore.Spec.Bundle
	restore.Status.Apps = []gentianov1alpha1.AppExportStatus{{Name: "wiki", Artefacts: []gentianov1alpha1.BundleArtefact{*a}}}
	ready, failure, err := w.r.restoreAppAuthorization(ctx, restore, planTenant("demo"), "wiki")
	if !ready || failure != "" || err != nil {
		t.Fatalf("restore: %v %q %v", ready, failure, err)
	}
	back := fga.stores["demo/wiki"]
	if back == nil || !reflect.DeepEqual(back.Tuples, held.Tuples) || string(back.Model) != string(model) {
		t.Fatalf("put back %+v", back)
	}
	if restore.Status.Apps[0].Authorization == nil || restore.Status.Apps[0].Authorization.RestoredAt == nil {
		t.Fatalf("status %+v", restore.Status.Apps[0])
	}
	// Once: a second pass does not put it back over what the app wrote since.
	fga.stores["demo/wiki"] = &arbiter.StoreExport{Model: model}
	if ready, _, _ := w.r.restoreAppAuthorization(ctx, restore, planTenant("demo"), "wiki"); !ready || len(fga.stores["demo/wiki"].Tuples) != 0 {
		t.Fatal("the store was put back twice")
	}

	tenant := planTenant("demo", "wiki")
	tenant.Spec.DeletionPolicy = gentianov1alpha1.DeletionPolicyDelete
	tr := &TenantReconciler{Client: w.c, Authorization: fga}
	if err := tr.deleteAuthorizationStores(ctx, tenant); err != nil || len(fga.purged) != 1 || fga.purged[0] != "demo" {
		t.Fatalf("deletion: %v %v", fga.purged, err)
	}
}
