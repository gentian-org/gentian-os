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

import "sort"

// Within reports whether every cookie and path of c is one of approved's.
func (c *SignInCookies) Within(approved *SignInCookies) bool {
	if c == nil {
		return true
	}
	if approved == nil {
		return false
	}
	return sameOrFewer(c.Names, approved.Names) && sameOrFewer(c.Paths, approved.Paths)
}

// Intersect is what both c and other name: the cookies a proxy passes are
// the ones the profile declares now and the approver approved, so a profile
// may narrow them without an approval and never widen them. Nil when either
// is nil or they share no name or no path.
func (c *SignInCookies) Intersect(other *SignInCookies) *SignInCookies {
	if c == nil || other == nil {
		return nil
	}
	out := &SignInCookies{Names: sharedSorted(c.Names, other.Names), Paths: sharedSorted(c.Paths, other.Paths)}
	if len(out.Names) == 0 || len(out.Paths) == 0 {
		return nil
	}
	return out
}

func sameOrFewer(a, b []string) bool {
	in := map[string]bool{}
	for _, v := range b {
		in[v] = true
	}
	for _, v := range a {
		if !in[v] {
			return false
		}
	}
	return true
}

func sharedSorted(a, b []string) []string {
	in := map[string]bool{}
	for _, v := range b {
		in[v] = true
	}
	var out []string
	for _, v := range a {
		if in[v] {
			out = append(out, v)
			in[v] = false
		}
	}
	sort.Strings(out)
	return out
}
