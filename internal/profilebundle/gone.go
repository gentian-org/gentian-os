/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package profilebundle

import (
	"context"
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	runtimeschema "k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// CatalogueApplicationPrefix is what the name of the Argo CD Application that
// applies the catalogue directory begins with (kernel/appsets/raw/11b-catalogue.yaml).
const CatalogueApplicationPrefix = "gentian-catalogue-"

// ApplicationGVK is Argo CD's Application.
var ApplicationGVK = runtimeschema.GroupVersionKind{Group: "argoproj.io", Version: "v1alpha1", Kind: "Application"}

// GoneFromCatalogue names the ComponentProfiles the catalogue's Application
// reports as on the cluster and no longer in the catalogue directory: it
// marks them as requiring pruning, which it never does itself. A profile
// that left git that way -- retired, or renamed by its catalogue and replaced
// by the installer -- is still an object, and still says whatever it said
// about where it is placed.
//
// An error when no such Application can be read: then nothing is known to
// be gone.
func GoneFromCatalogue(ctx context.Context, reader client.Reader, namespace string) (map[string]bool, error) {
	apps := &unstructured.UnstructuredList{}
	apps.SetGroupVersionKind(ApplicationGVK.GroupVersion().WithKind("ApplicationList"))
	if err := reader.List(ctx, apps, client.InNamespace(namespace)); err != nil {
		return nil, fmt.Errorf("list Argo CD Applications in %s: %w", namespace, err)
	}
	gone, seen := map[string]bool{}, false
	for i := range apps.Items {
		if !strings.HasPrefix(apps.Items[i].GetName(), CatalogueApplicationPrefix) {
			continue
		}
		seen = true
		for name := range PrunableProfiles(&apps.Items[i]) {
			gone[name] = true
		}
	}
	if !seen {
		return nil, fmt.Errorf("no Argo CD Application %s* in %s", CatalogueApplicationPrefix, namespace)
	}
	return gone, nil
}

// PrunableProfiles names the ComponentProfiles one Application marks as
// requiring pruning.
func PrunableProfiles(app *unstructured.Unstructured) map[string]bool {
	out := map[string]bool{}
	resources, _, _ := unstructured.NestedSlice(app.Object, "status", "resources")
	for _, raw := range resources {
		r, _ := raw.(map[string]any)
		if r["group"] != gentianov1alpha1.GroupVersion.Group || r["kind"] != KindProfile || r["requiresPruning"] != true {
			continue
		}
		if name, _ := r["name"].(string); name != "" {
			out[name] = true
		}
	}
	return out
}
