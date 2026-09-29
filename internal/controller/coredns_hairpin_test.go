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
          192.0.2.197 platform.example.test
          192.0.2.197 id.platform.example.test
          192.0.2.139 mail.platform.example.test
          # END gentian-hairpin
          fallthrough
    }
}`

	patched, changed := patchHairpinCorefile(corefile, "192.0.2.36", "platform.example.test", nil)
	if !changed {
		t.Fatal("expected hairpin patch to change Corefile")
	}
	if strings.Contains(patched, "192.0.2.197") {
		t.Fatalf("expected edge proxy IP to be replaced, got:\n%s", patched)
	}
	if !strings.Contains(patched, "192.0.2.36 id.platform.example.test") {
		t.Fatalf("expected id host to point at Envoy IP, got:\n%s", patched)
	}
	if !strings.Contains(patched, "192.0.2.139 mail.platform.example.test") {
		t.Fatalf("expected mail host to be preserved, got:\n%s", patched)
	}
}

func TestPatchHairpinCorefile_Idempotent(t *testing.T) {
	t.Parallel()

	var b strings.Builder
	b.WriteString(hairpinBeginMarker + "\n")
	for _, host := range sortedHairpinHosts("platform.example.test") {
		b.WriteString("          192.0.2.36 ")
		b.WriteString(host)
		b.WriteByte('\n')
	}
	b.WriteString("          " + hairpinEndMarker)
	corefile := b.String()

	_, changed := patchHairpinCorefile(corefile, "192.0.2.36", "platform.example.test", nil)
	if changed {
		t.Fatal("expected no change when hairpin already correct for managed hosts")
	}
}

func TestPatchHairpinCorefile_InsertsMissingHosts(t *testing.T) {
	t.Parallel()

	corefile := `# BEGIN gentian-hairpin
          192.0.2.197 console.platform.example.test
          # END gentian-hairpin`

	patched, changed := patchHairpinCorefile(corefile, "192.0.2.36", "platform.example.test", nil)
	if !changed {
		t.Fatal("expected missing kernel hosts to be inserted")
	}
	if !strings.Contains(patched, "192.0.2.36 id.platform.example.test") {
		t.Fatalf("expected missing id host to be added, got:\n%s", patched)
	}
}

func TestPatchHairpinCorefile_AddsTenantAppHosts(t *testing.T) {
	t.Parallel()

	corefile := `# BEGIN gentian-hairpin
          192.0.2.36 console.platform.example.test
          # END gentian-hairpin`

	tenantHosts := map[string]struct{}{
		"cloud.demo.platform.example.test":     {},
		"collabora.demo.platform.example.test": {},
	}
	patched, changed := patchHairpinCorefile(corefile, "192.0.2.36", "platform.example.test", tenantHosts)
	if !changed {
		t.Fatal("expected tenant app hosts to be inserted")
	}
	if !strings.Contains(patched, "192.0.2.36 cloud.demo.platform.example.test") {
		t.Fatalf("expected cloud host in hairpin block, got:\n%s", patched)
	}
	if !strings.Contains(patched, "192.0.2.36 collabora.demo.platform.example.test") {
		t.Fatalf("expected collabora host in hairpin block, got:\n%s", patched)
	}
}

// A managed CoreDNS ships a stock Corefile with no hosts plugin. That used to
// return (corefile, false), which ensureCoreDNSHairpin read as "already
// converged" -- so the hairpin silently never applied and every server-side
// fetch to a tenant host left the cluster for the load balancer's public IP.
func TestPatchHairpinCorefile_CreatesHostsBlockWhenAbsent(t *testing.T) {
	t.Parallel()

	corefile := `.:53 {
    errors
    health {
        lameduck 5s
    }
    ready
    kubernetes cluster.local in-addr.arpa ip6.arpa {
        pods insecure
        fallthrough in-addr.arpa ip6.arpa
        ttl 30
    }
    prometheus 0.0.0.0:9153
    forward . /etc/resolv.conf
    cache 30
    loop
    reload
    loadbalance
}`

	tenantHosts := map[string]struct{}{
		"cloud.demo.platform.example.test": {},
	}
	patched, changed := patchHairpinCorefile(corefile, "192.0.2.36", "platform.example.test", tenantHosts)
	if !changed {
		t.Fatalf("expected a hosts block to be created, got:\n%s", patched)
	}
	if !strings.Contains(patched, "hosts {") {
		t.Fatalf("expected a hosts plugin block, got:\n%s", patched)
	}
	// Without fallthrough the hosts plugin answers authoritatively for names it
	// does not know and the rest of cluster DNS stops resolving.
	if !strings.Contains(patched, "fallthrough\n    }") {
		t.Fatalf("expected fallthrough inside the hosts block, got:\n%s", patched)
	}
	// A block created from scratch must already carry the tenant app hosts.
	if !strings.Contains(patched, "192.0.2.36 cloud.demo.platform.example.test") {
		t.Fatalf("expected tenant host in the new block, got:\n%s", patched)
	}
	if !strings.Contains(patched, "192.0.2.36 id.platform.example.test") {
		t.Fatalf("expected kernel hosts in the new block, got:\n%s", patched)
	}
	if strings.Index(patched, "hosts {") > strings.Index(patched, "kubernetes ") {
		t.Fatalf("expected hosts block before the kubernetes directive, got:\n%s", patched)
	}
	// The kubernetes plugin's own indented brace must not be mistaken for the
	// server block's.
	if !strings.Contains(patched, "fallthrough in-addr.arpa ip6.arpa") {
		t.Fatalf("expected the kubernetes block to survive intact, got:\n%s", patched)
	}
}

func TestPatchHairpinCorefile_CreatedBlockIsIdempotent(t *testing.T) {
	t.Parallel()

	corefile := ".:53 {\n    kubernetes cluster.local\n    forward . /etc/resolv.conf\n}"
	tenantHosts := map[string]struct{}{"cloud.demo.platform.example.test": {}}

	first, changed := patchHairpinCorefile(corefile, "192.0.2.36", "platform.example.test", tenantHosts)
	if !changed {
		t.Fatal("expected the first pass to create the block")
	}
	second, changed := patchHairpinCorefile(first, "192.0.2.36", "platform.example.test", tenantHosts)
	if changed {
		t.Fatalf("expected the second pass to be a no-op, got:\n%s", second)
	}
}

// No server block to place it in: that must be reported, not silently accepted.
func TestPatchHairpinCorefile_ReportsUnplaceableCorefile(t *testing.T) {
	t.Parallel()

	if _, changed := patchHairpinCorefile("# nothing here", "192.0.2.36", "platform.example.test", nil); changed {
		t.Fatal("expected no change when there is no kubernetes directive to anchor the hosts block to")
	}
}

// A host inside the markers that nothing wants any more is removed: the
// block used to keep every line it did not recognise, so a cluster carried
// the hostnames of every domain it had ever been installed under, each
// pinned to an address that might since belong to something else.
func TestPatchHairpinCorefile_RetiresHostsNothingWants(t *testing.T) {
	t.Parallel()

	corefile := `# BEGIN gentian-hairpin
          192.0.2.36 platform.example.test
          192.0.2.36 console.platform.example.test
          192.0.2.36 id.platform.example.test
          192.0.2.36 argocd.platform.example.test
          192.0.2.36 mail.platform.example.test
          198.51.100.7 old.previous.test
          198.51.100.7 console.previous.test
          198.51.100.7 mail.previous.test
          # END gentian-hairpin`

	patched, changed := patchHairpinCorefile(corefile, "192.0.2.36", "platform.example.test", nil)
	if !changed {
		t.Fatal("expected the stale hosts to be retired")
	}
	for _, gone := range []string{"old.previous.test", "console.previous.test", "mail.previous.test"} {
		if strings.Contains(patched, gone) {
			t.Errorf("%q survived:\n%s", gone, patched)
		}
	}
	// What this cluster wants is untouched, the mail host included.
	for _, kept := range []string{
		"192.0.2.36 platform.example.test",
		"192.0.2.36 console.platform.example.test",
		"192.0.2.36 id.platform.example.test",
		"192.0.2.36 argocd.platform.example.test",
		"192.0.2.36 mail.platform.example.test",
	} {
		if !strings.Contains(patched, kept) {
			t.Errorf("%q was dropped:\n%s", kept, patched)
		}
	}
	// And a second pass changes nothing.
	if _, changed := patchHairpinCorefile(patched, "192.0.2.36", "platform.example.test", nil); changed {
		t.Error("retiring stale hosts is not idempotent")
	}
}

// A tenant's app hostnames are retired with the tenant: they reach this
// function as the tenantHosts set, and a set that no longer names them is
// how a deleted tenant's overrides disappear.
func TestPatchHairpinCorefile_RetiresTenantHostsWithTheTenant(t *testing.T) {
	t.Parallel()

	corefile := `# BEGIN gentian-hairpin
          192.0.2.36 platform.example.test
          192.0.2.36 chat.demo.platform.example.test
          # END gentian-hairpin`

	patched, changed := patchHairpinCorefile(corefile, "192.0.2.36", "platform.example.test", nil)
	if !changed || strings.Contains(patched, "chat.demo.platform.example.test") {
		t.Fatalf("the deleted tenant's host survived:\n%s", patched)
	}
}
