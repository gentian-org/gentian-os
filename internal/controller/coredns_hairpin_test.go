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
          192.0.2.197 portal.platform.example.test
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
          192.0.2.36 portal.platform.example.test
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
