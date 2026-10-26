/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package custodian

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
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

	reader := consumer("kernel-authentication", "backup-destination-corp", path)
	probeES := consumer("kernel-control", "credreq-backup-destination-corp", path)
	bulk := bulkConsumer("kernel-authentication", "chart-style-reader", path)
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
		{"kernel-authentication", "backup-destination-corp"},
		{"kernel-control", "credreq-backup-destination-corp"},
		{"kernel-authentication", "chart-style-reader"},
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

// probeAt is a probe whose Ready condition last changed at `since` and which
// the custodian nudged at `nudged` (zero: never).
func probeAt(name string, ready bool, since, nudged time.Time) *unstructured.Unstructured {
	u := probe(name, ready)
	conds, _, _ := unstructured.NestedSlice(u.Object, "status", "conditions")
	cond := conds[0].(map[string]any)
	cond["lastTransitionTime"] = since.UTC().Format(time.RFC3339)
	_ = unstructured.SetNestedSlice(u.Object, conds, "status", "conditions")
	if !nudged.IsZero() {
		u.SetAnnotations(map[string]string{
			forceSyncAnnotation: strconv.FormatInt(nudged.Unix(), 10),
		})
	}
	return u
}

// A value stored a moment ago is not "missing": ESO has been told to read it
// and has not answered. Reporting the probe's previous verdict told the person
// who had just supplied it that it was lost, for as long as a refresh interval.
func TestAFreshWriteIsPendingUntilTheProbeAnswers(t *testing.T) {
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	before := now.Add(-time.Hour)

	cases := []struct {
		name          string
		probe         *unstructured.Unstructured
		wantSatisfied bool
		wantPending   bool
	}{
		{"nudged, probe still on the old verdict",
			probeAt("p", false, before, now.Add(-10*time.Second)), false, true},
		{"nudged, ESO has confirmed",
			probeAt("p", true, now.Add(-5*time.Second), now.Add(-10*time.Second)), true, false},
		{"nudged, ESO has refused the new value",
			probeAt("p", false, now.Add(-5*time.Second), now.Add(-10*time.Second)), false, false},
		{"nudged too long ago: ESO's verdict stands",
			probeAt("p", false, before, now.Add(-pendingWindow-time.Second)), false, false},
		{"never nudged",
			probeAt("p", false, before, time.Time{}), false, false},
		{"nudged before ESO ever reported",
			func() *unstructured.Unstructured {
				u := probeAt("p", false, before, now.Add(-10*time.Second))
				unstructured.RemoveNestedField(u.Object, "status")
				return u
			}(), false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := &Catalogue{
				Client:         fake.NewClientBuilder().WithScheme(testScheme(t)).WithRuntimeObjects(tc.probe).Build(),
				ProbeNamespace: "kernel-control",
				Now:            func() time.Time { return now },
			}
			satisfied, pending, reason := c.probeStatus(context.Background(), "p")
			if satisfied != tc.wantSatisfied || pending != tc.wantPending {
				t.Fatalf("satisfied=%v pending=%v (reason %q), want satisfied=%v pending=%v",
					satisfied, pending, reason, tc.wantSatisfied, tc.wantPending)
			}
			if pending && satisfied {
				t.Fatal("pending and satisfied at once")
			}
		})
	}
}

// The nudge is a merge patch of one annotation. The role grants patch and not
// update, so an Update here would fail on the cluster while passing against a
// fake client; and an Update would rewrite fields this service does not own.
func TestRefreshPatchesOnlyTheAnnotation(t *testing.T) {
	const path = "gentian-os/kernel/mail/postfix"
	es := consumer("kernel-authentication", "keycloak-smtp-credentials", path)
	es.SetAnnotations(map[string]string{"owner.example/keep": "yes"})

	var patched, updated int
	s := &Server{Client: interceptor.NewClient(
		fake.NewClientBuilder().WithScheme(testScheme(t)).WithRuntimeObjects(es).Build(),
		interceptor.Funcs{
			Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, p client.Patch, opts ...client.PatchOption) error {
				patched++
				if p.Type() != types.MergePatchType {
					t.Errorf("patch type %s, want a merge patch", p.Type())
				}
				body, _ := p.Data(obj)
				if strings.Contains(string(body), "spec") || strings.Contains(string(body), "owner.example") {
					t.Errorf("the patch carries more than the force-sync annotation: %s", body)
				}
				return c.Patch(ctx, obj, p, opts...)
			},
			Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
				updated++
				return c.Update(ctx, obj, opts...)
			},
		})}

	s.refreshConsumers(context.Background(), path)

	if patched != 1 || updated != 0 {
		t.Fatalf("patched %d, updated %d; want one patch and no update", patched, updated)
	}
	if annotationOf(t, s, "kernel-authentication", "keycloak-smtp-credentials") == "" {
		t.Fatal("the consumer was not stamped")
	}
}
