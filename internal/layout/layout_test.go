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
	text := string(raw)
	text = text[strings.Index(text, "kernel:"):]
	if i := strings.Index(text, "\nlabelled:"); i > 0 {
		text = text[:i]
	}
	entry := regexp.MustCompile(`- name: (\S+)\n\s+function: (\S+)`)
	var fromFile []string
	for _, m := range entry.FindAllStringSubmatch(text, -1) {
		fromFile = append(fromFile, m[1])
		if got := Kernel(Function(m[2])); got != m[1] {
			t.Errorf("function %s: file says %s, code says %s", m[2], m[1], got)
		}
	}
	if got, want := strings.Join(KernelNamespaces(), " "), strings.Join(fromFile, " "); got != want {
		t.Errorf("kernel namespaces:\n code: %s\n file: %s", got, want)
	}
}

func TestATenantNameFitsItsDMZ(t *testing.T) {
	long := strings.Repeat("a", MaxTenantName)
	if n := len(TenantDMZ(long)); n != 63 {
		t.Fatalf("tenant-%s-dmz is %d characters", long, n)
	}
}
