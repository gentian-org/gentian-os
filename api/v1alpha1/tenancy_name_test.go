/*
Copyright The Gentian OS Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.

SPDX-License-Identifier: Apache-2.0
*/

package v1alpha1

import (
	"strings"
	"testing"
)

func TestATenantNameIsADNSLabelOfAtMost38(t *testing.T) {
	for name, ok := range map[string]bool{
		"demo":                        true,
		"a":                           true,
		"a1-b2":                       true,
		strings.Repeat("a", 38):       true,
		strings.Repeat("a", 39):       false,
		"":                            false,
		"-demo":                       false,
		"demo-":                       false,
		"Demo":                        false,
		"de_mo":                       false,
		"de.mo":                       false,
		strings.Repeat("a", 37) + "-": false,
	} {
		if got := ValidTenantName(name); got != ok {
			t.Errorf("ValidTenantName(%q) = %v, want %v", name, got, ok)
		}
	}
	if MaxTenantNameLength != 38 {
		t.Fatalf("MaxTenantNameLength = %d", MaxTenantNameLength)
	}
}
