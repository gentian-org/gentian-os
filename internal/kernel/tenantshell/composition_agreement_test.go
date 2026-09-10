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

package tenantshell

import (
	"os"
	"sort"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"sigs.k8s.io/yaml"

	gentianov1alpha1 "github.com/gentian-org/gentian-os/api/v1alpha1"
)

// TestResourceListMirrorsTheComposition holds ResourceListFromQuotas to the
// quota the Composition actually renders.
//
// The doc comment on ResourceListFromQuotas explains why the mapping exists
// twice and asks whoever changes one to change the other. That request went
// unmet once already — requests.cpu and requests.memory were added here and not
// to the Composition — and the cost was not a broken mirror but a tenant paying
// for reserved capacity the cluster was never told to reserve.
//
// The golden render fixture sets every field of TenantQuotas, so the key set it
// produces is the whole contract: what the mirror emits for the same input has
// to match it exactly.
func TestResourceListMirrorsTheComposition(t *testing.T) {
	rendered := renderedQuotaKeys(t)

	// The same quantities the render fixture feeds the Composition. Values do
	// not matter here — the keys are what drifted.
	q := &gentianov1alpha1.TenantQuotas{MaxApps: 10, MaxPods: 100}
	for target, value := range map[**resource.Quantity]string{
		&q.Storage:        "50Gi",
		&q.CPU:            "8",
		&q.Memory:         "8Gi",
		&q.RequestsCPU:    "2",
		&q.RequestsMemory: "4Gi",
	} {
		parsed := resource.MustParse(value)
		*target = &parsed
	}

	mirrored := map[string]struct{}{}
	for key := range ResourceListFromQuotas(q) {
		mirrored[string(key)] = struct{}{}
	}

	for key := range rendered {
		if _, ok := mirrored[key]; !ok {
			t.Errorf("the Composition renders %q and ResourceListFromQuotas does not; "+
				"the downgrade guard in internal/resourceplan compares against a key "+
				"the cluster is not enforcing", key)
		}
	}
	for key := range mirrored {
		if _, ok := rendered[key]; !ok {
			t.Errorf("ResourceListFromQuotas emits %q and the Composition renders no "+
				"such key; the cluster obeys the Composition, so this quota does not "+
				"exist outside Go", key)
		}
	}

	// maxApps is the one field of TenantQuotas that is deliberately not a quota
	// key: it is the Tenant webhook's policy limit on how many apps may be
	// installed, not a quantity any ResourceQuota can hold.
	if _, ok := mirrored["maxApps"]; ok {
		t.Error("ResourceListFromQuotas emits maxApps; it is a webhook policy " +
			"limit, and ResourceQuota has no such resource")
	}
}

// renderedQuotaKeys pulls spec.hard out of the tenant-quota ResourceQuota in the
// golden render output.
func renderedQuotaKeys(t *testing.T) map[string]struct{} {
	t.Helper()
	raw, err := os.ReadFile("../../../crossplane/tests/unit/render/tenant-default/expected.yaml")
	if err != nil {
		t.Skipf("render golden not readable from here: %v", err)
	}

	for _, doc := range strings.Split(string(raw), "\n---\n") {
		var obj struct {
			Spec struct {
				ForProvider struct {
					Manifest struct {
						Kind     string `json:"kind"`
						Metadata struct {
							Name string `json:"name"`
						} `json:"metadata"`
						Spec corev1.ResourceQuotaSpec `json:"spec"`
					} `json:"manifest"`
				} `json:"forProvider"`
			} `json:"spec"`
		}
		if err := yaml.Unmarshal([]byte(doc), &obj); err != nil {
			continue // not every document in the golden is a provider-kubernetes Object
		}
		m := obj.Spec.ForProvider.Manifest
		if m.Kind != "ResourceQuota" || m.Metadata.Name != "tenant-quota" {
			continue
		}
		keys := make(map[string]struct{}, len(m.Spec.Hard))
		for key := range m.Spec.Hard {
			keys[string(key)] = struct{}{}
		}
		if len(keys) == 0 {
			t.Fatal("the golden render composes tenant-quota with no hard limits")
		}
		return keys
	}

	names := make([]string, 0)
	for _, doc := range strings.Split(string(raw), "\n---\n") {
		if i := strings.Index(doc, "name: "); i >= 0 {
			names = append(names, strings.SplitN(doc[i+6:], "\n", 2)[0])
		}
	}
	sort.Strings(names)
	t.Fatalf("no tenant-quota ResourceQuota in the golden render; it composes %v", names)
	return nil
}
