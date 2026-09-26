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

package layout

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// The installer reads kernel/namespaces.yaml; this package is the same list in
// Go. They must say the same thing, in the same order.
func TestTheGoLayoutMatchesTheInstallersFile(t *testing.T) {
	raw, err := os.ReadFile("../../kernel/namespaces.yaml")
	if err != nil {
		t.Fatal(err)
	}
	// One section at a time. The slice used to run from "kernel:" to
	// "labelled:", which swallowed every section in between -- so the day a
	// system: section was added, its entries were read as kernel namespaces
	// and Kernel(Function("postgresql")) panicked on a file that was correct.
	kernel := section(t, string(raw), "kernel:")
	entry := regexp.MustCompile(`- name: (\S+)\n\s+function: (\S+)`)
	var fromFile []string
	for _, m := range entry.FindAllStringSubmatch(kernel, -1) {
		fromFile = append(fromFile, m[1])
		if got := Kernel(Function(m[2])); got != m[1] {
			t.Errorf("function %s: file says %s, code says %s", m[2], m[1], got)
		}
	}
	if got, want := strings.Join(KernelNamespaces(), " "), strings.Join(fromFile, " "); got != want {
		t.Errorf("kernel namespaces:\n code: %s\n file: %s", got, want)
	}

	// The system tier, the same way. The operator addresses these through
	// System(fn) and the Cluster composition creates them; a name the two
	// spell differently is a namespace nothing looks in.
	for _, m := range entry.FindAllStringSubmatch(section(t, string(raw), "system:"), -1) {
		if got := System(m[2]); got != m[1] {
			t.Errorf("system function %s: file says %s, code says %s", m[2], m[1], got)
		}
	}
}

// section returns one top-level block of the layout file: from its key to the
// next line that starts in column zero.
func section(t *testing.T, text, key string) string {
	t.Helper()
	i := strings.Index(text, key)
	if i < 0 {
		t.Fatalf("kernel/namespaces.yaml has no %s section", key)
	}
	rest := text[i+len(key):]
	next := regexp.MustCompile(`(?m)^[a-z]`).FindStringIndex(rest)
	if next == nil {
		return rest
	}
	return rest[:next[0]]
}

func TestATenantNameFitsItsDMZ(t *testing.T) {
	long := strings.Repeat("a", MaxTenantName)
	if n := len(TenantDMZ(long)); n != 63 {
		t.Fatalf("tenant-%s-dmz is %d characters", long, n)
	}
}
