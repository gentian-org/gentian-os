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

package credentialmgr

import (
	"context"
	"strconv"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	ctrl "sigs.k8s.io/controller-runtime"
)

// forceSyncAnnotation is what External Secrets watches to re-read a value ahead
// of its refreshInterval. Any change to the object triggers a reconcile; this
// is the conventional key, so an operator reading the object sees why it moved.
const forceSyncAnnotation = "force-sync"

// refreshConsumers tells every ExternalSecret that reads a vault path to read it
// again, now.
//
// Storing a credential and having it take effect are different events, and the
// gap between them is a refreshInterval — an hour on this platform. Everything
// in between reports success: the console shows the credential set, the
// CredentialRequirement's probe is satisfied, the BackupPolicy accepts. The
// workloads keep authenticating with the previous value.
//
// That is not theoretical and the failure it produces is badly misleading. A
// backup destination's keys were replaced in the provider and supplied here; the
// cluster went on using the deleted ones for as long as the interval had left,
// and the export failed with "capture did not succeed after 3 attempts",
// naming an app that had nothing to do with it. Nothing anywhere said "the
// value you just set is not the one in use".
//
// Best effort, deliberately. The credential is already stored by the time this
// runs, so a failure here means it takes effect within the refresh interval
// rather than immediately — which is exactly the old behaviour. Reporting the
// write as failed because the nudge failed would turn a slow success into a
// visible error and invite someone to store it twice.
func (s *Server) refreshConsumers(ctx context.Context, vaultPath string) {
	log := ctrl.Log.WithName("credentialmgr")
	if s.Client == nil || vaultPath == "" {
		return
	}

	list := &unstructured.UnstructuredList{}
	// The List kind, from the same GVK the rest of this package reads one by.
	list.SetGroupVersionKind(externalSecretGVK.GroupVersion().WithKind(
		externalSecretGVK.Kind + "List"))
	if err := s.Client.List(ctx, list); err != nil {
		log.Error(err, "cannot list ExternalSecrets to refresh a stored credential",
			"path", vaultPath)
		return
	}

	stamp := strconv.FormatInt(time.Now().UTC().Unix(), 10)
	nudged := 0
	for i := range list.Items {
		item := &list.Items[i]
		if !readsVaultPath(item, vaultPath) {
			continue
		}
		annotations := item.GetAnnotations()
		if annotations == nil {
			annotations = map[string]string{}
		}
		annotations[forceSyncAnnotation] = stamp
		item.SetAnnotations(annotations)
		if err := s.Client.Update(ctx, item); err != nil {
			log.Error(err, "cannot refresh an ExternalSecret after storing a credential",
				"path", vaultPath, "externalSecret", item.GetName(),
				"namespace", item.GetNamespace())
			continue
		}
		nudged++
	}
	if nudged > 0 {
		log.Info("refreshed the secrets reading a stored credential",
			"path", vaultPath, "externalSecrets", nudged)
	}
}

// readsVaultPath reports whether an ExternalSecret sources anything from a path.
//
// Both shapes are checked. spec.data names one property at a time and is what
// this platform writes; spec.dataFrom pulls a whole path and is what a chart may
// use. Missing either would leave a credential that looks refreshed and is not.
func readsVaultPath(es *unstructured.Unstructured, vaultPath string) bool {
	data, _, _ := unstructured.NestedSlice(es.Object, "spec", "data")
	for _, raw := range data {
		entry, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		ref, ok := entry["remoteRef"].(map[string]any)
		if ok && ref["key"] == vaultPath {
			return true
		}
	}

	from, _, _ := unstructured.NestedSlice(es.Object, "spec", "dataFrom")
	for _, raw := range from {
		entry, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		for _, field := range []string{"extract", "find"} {
			ref, ok := entry[field].(map[string]any)
			if ok && ref["key"] == vaultPath {
				return true
			}
		}
	}
	return false
}
