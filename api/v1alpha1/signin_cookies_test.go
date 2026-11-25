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
	"reflect"
	"testing"
)

func TestSignInCookiesWithinAndIntersect(t *testing.T) {
	set := func(names, paths []string) *SignInCookies { return &SignInCookies{Names: names, Paths: paths} }
	approved := set([]string{"a", "b"}, []string{"/x", "/y"})
	for _, tc := range []struct {
		what   string
		c      *SignInCookies
		within bool
		both   *SignInCookies
	}{
		{"the same", set([]string{"b", "a"}, []string{"/y", "/x"}), true, set([]string{"a", "b"}, []string{"/x", "/y"})},
		{"fewer", set([]string{"a"}, []string{"/x"}), true, set([]string{"a"}, []string{"/x"})},
		{"another name", set([]string{"a", "c"}, []string{"/x"}), false, set([]string{"a"}, []string{"/x"})},
		{"another path", set([]string{"a"}, []string{"/z"}), false, nil},
		{"none", nil, true, nil},
	} {
		if got := tc.c.Within(approved); got != tc.within {
			t.Errorf("%s: within = %v", tc.what, got)
		}
		if got := tc.c.Intersect(approved); !reflect.DeepEqual(got, tc.both) {
			t.Errorf("%s: intersect = %+v, want %+v", tc.what, got, tc.both)
		}
	}
	if set([]string{"a"}, []string{"/x"}).Within(nil) {
		t.Error("a set is within no approval")
	}
}
