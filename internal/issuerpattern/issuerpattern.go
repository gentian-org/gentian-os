/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

// Package issuerpattern matches an issuer against the Cluster claim's
// spec.partners.allowedIssuers, for the operator and the director alike.
package issuerpattern

import (
	"regexp"
	"strings"
	"sync"
)

// compiled holds each pattern's regular expression, compiled the first time
// the pattern is matched. The patterns come from the Cluster claim, a short
// list its administrator writes.
var compiled sync.Map // pattern -> *regexp.Regexp

// Allowed reports whether an issuer matches one of the patterns. A pattern
// matches the whole issuer; "*" matches any run of characters other than a
// newline, and every other character only itself, case included.
func Allowed(patterns []string, issuer string) bool {
	for _, p := range patterns {
		if compile(p).MatchString(issuer) {
			return true
		}
	}
	return false
}

func compile(pattern string) *regexp.Regexp {
	if re, ok := compiled.Load(pattern); ok {
		return re.(*regexp.Regexp)
	}
	parts := strings.Split(pattern, "*")
	for i := range parts {
		parts[i] = regexp.QuoteMeta(parts[i])
	}
	re, _ := compiled.LoadOrStore(pattern, regexp.MustCompile("^"+strings.Join(parts, ".*")+"$"))
	return re.(*regexp.Regexp)
}
