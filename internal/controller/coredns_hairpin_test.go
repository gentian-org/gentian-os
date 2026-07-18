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

package controller

import (
	"strings"
	"testing"
)

func TestPatchHairpinCorefile_UpdatesKernelHostsPreservesMail(t *testing.T) {
	t.Parallel()

	corefile := `.:53 {
    hosts {
      # BEGIN gentian-hairpin
          10.152.183.197 desk.gentian.org
          10.152.183.197 id.desk.gentian.org
          10.152.183.139 mail.desk.gentian.org
          # END gentian-hairpin
          fallthrough
    }
}`

	patched, changed := patchHairpinCorefile(corefile, "10.152.183.36", "desk.gentian.org", nil)
	if !changed {
		t.Fatal("expected hairpin patch to change Corefile")
	}
	if strings.Contains(patched, "10.152.183.197") {
		t.Fatalf("expected edge proxy IP to be replaced, got:\n%s", patched)
	}
	if !strings.Contains(patched, "10.152.183.36 id.desk.gentian.org") {
		t.Fatalf("expected id host to point at Envoy IP, got:\n%s", patched)
	}
	if !strings.Contains(patched, "10.152.183.139 mail.desk.gentian.org") {
		t.Fatalf("expected mail host to be preserved, got:\n%s", patched)
	}
}

func TestPatchHairpinCorefile_Idempotent(t *testing.T) {
	t.Parallel()

	var b strings.Builder
	b.WriteString(hairpinBeginMarker + "\n")
	for _, host := range sortedHairpinHosts("desk.gentian.org") {
		b.WriteString("          10.152.183.36 ")
		b.WriteString(host)
		b.WriteByte('\n')
	}
	b.WriteString("          " + hairpinEndMarker)
	corefile := b.String()

	_, changed := patchHairpinCorefile(corefile, "10.152.183.36", "desk.gentian.org", nil)
	if changed {
		t.Fatal("expected no change when hairpin already correct for managed hosts")
	}
}

func TestPatchHairpinCorefile_InsertsMissingHosts(t *testing.T) {
	t.Parallel()

	corefile := `# BEGIN gentian-hairpin
          10.152.183.197 portal.desk.gentian.org
          # END gentian-hairpin`

	patched, changed := patchHairpinCorefile(corefile, "10.152.183.36", "desk.gentian.org", nil)
	if !changed {
		t.Fatal("expected missing kernel hosts to be inserted")
	}
	if !strings.Contains(patched, "10.152.183.36 id.desk.gentian.org") {
		t.Fatalf("expected missing id host to be added, got:\n%s", patched)
	}
}

func TestPatchHairpinCorefile_AddsTenantAppHosts(t *testing.T) {
	t.Parallel()

	corefile := `# BEGIN gentian-hairpin
          10.152.183.36 portal.desk.gentian.org
          # END gentian-hairpin`

	tenantHosts := map[string]struct{}{
		"cloud.demo.desk.gentian.org":     {},
		"collabora.demo.desk.gentian.org": {},
	}
	patched, changed := patchHairpinCorefile(corefile, "10.152.183.36", "desk.gentian.org", tenantHosts)
	if !changed {
		t.Fatal("expected tenant app hosts to be inserted")
	}
	if !strings.Contains(patched, "10.152.183.36 cloud.demo.desk.gentian.org") {
		t.Fatalf("expected cloud host in hairpin block, got:\n%s", patched)
	}
	if !strings.Contains(patched, "10.152.183.36 collabora.demo.desk.gentian.org") {
		t.Fatalf("expected collabora host in hairpin block, got:\n%s", patched)
	}
}
