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
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// consumer is an ExternalSecret reading one property of one vault path, which
// is the shape this platform writes.
func consumer(namespace, name, vaultPath string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(externalSecretGVK)
	u.SetNamespace(namespace)
	u.SetName(name)
	_ = unstructured.SetNestedSlice(u.Object, []any{
		map[string]any{
			"secretKey": "accessKey",
			"remoteRef": map[string]any{"key": vaultPath, "property": "accessKey"},
		},
	}, "spec", "data")
	return u
}

// bulkConsumer reads a whole path with dataFrom, the shape a chart may use.
func bulkConsumer(namespace, name, vaultPath string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(externalSecretGVK)
	u.SetNamespace(namespace)
	u.SetName(name)
	_ = unstructured.SetNestedSlice(u.Object, []any{
		map[string]any{"extract": map[string]any{"key": vaultPath}},
	}, "spec", "dataFrom")
	return u
}

func annotationOf(t *testing.T, s *Server, namespace, name string) string {
	t.Helper()
	got := &unstructured.Unstructured{}
	got.SetGroupVersionKind(externalSecretGVK)
	if err := s.Client.Get(context.Background(),
		types.NamespacedName{Namespace: namespace, Name: name}, got); err != nil {
		t.Fatalf("get %s/%s: %v", namespace, name, err)
	}
	return got.GetAnnotations()[forceSyncAnnotation]
}

// The gap this closes: a credential is stored, everything reports success, and
// the workloads keep using the previous value until a refreshInterval elapses.
// On corp that interval was an hour, and a backup started inside it failed
// against keys that had already been deleted at the provider.
func TestStoringACredentialRefreshesWhatReadsIt(t *testing.T) {
	const path = "gentian-os/tenants/corp/backup/destination"

	reader := consumer("platform-kernel", "backup-destination-corp", path)
	probeES := consumer("gentian-system", "credreq-backup-destination-corp", path)
	bulk := bulkConsumer("platform-kernel", "chart-style-reader", path)
	// A different credential entirely. Nudging this would wake workloads that
	// have no stake in the value that changed.
	unrelated := consumer("tenant-corp", "nextcloud-oidc", "gentian-os/tenants/corp/apps/nextcloud/oidc")

	scheme := testScheme(t)
	s := &Server{
		Client: fake.NewClientBuilder().WithScheme(scheme).
			WithRuntimeObjects(reader, probeES, bulk, unrelated).Build(),
	}

	s.refreshConsumers(context.Background(), path)

	for _, es := range []struct{ ns, name string }{
		{"platform-kernel", "backup-destination-corp"},
		{"gentian-system", "credreq-backup-destination-corp"},
		{"platform-kernel", "chart-style-reader"},
	} {
		if annotationOf(t, s, es.ns, es.name) == "" {
			t.Errorf("%s/%s was not refreshed; it reads the path that just changed, so it "+
				"would serve the old value until its refreshInterval elapsed", es.ns, es.name)
		}
	}

	if got := annotationOf(t, s, "tenant-corp", "nextcloud-oidc"); got != "" {
		t.Errorf("an ExternalSecret for an unrelated path was refreshed (%q); storing one "+
			"credential must not wake every workload on the cluster", got)
	}
}

// Storing the credential is the part that matters. If the nudge cannot happen —
// no client, or a cluster that will not answer — the value is still stored and
// still takes effect on the normal interval, so this must not panic or block.
func TestRefreshIsHarmlessWithoutAClient(t *testing.T) {
	s := &Server{}
	s.refreshConsumers(context.Background(), "gentian-os/kernel/mail/postfix")

	scheme := testScheme(t)
	s = &Server{Client: fake.NewClientBuilder().WithScheme(scheme).Build()}
	s.refreshConsumers(context.Background(), "")
}
