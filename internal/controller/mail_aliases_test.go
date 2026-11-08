/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package controller

import (
	"strings"
	"testing"
)

// A configured alias is written only when Postfix would ever ask about it (its
// domain is hosted) and only to an address that has a reader (off the
// cluster). Everything else is refused with a reason, never written half-right.
func TestParseMailAliases(t *testing.T) {
	t.Parallel()

	hosted := map[string]bool{
		"gentian.cloud":        true,
		"finnor.gentian.cloud": true,
	}

	byDomain, refused := parseMailAliases(
		" hello@gentian.cloud=someone@example.net , Sales@Finnor.gentian.cloud=sales@example.net,"+
			"hello@gentian.cloud=other@example.net,"+ // a duplicate address keeps the first target
			"info@elsewhere.example=someone@example.net,"+ // not hosted
			"ops@gentian.cloud=admin@gentian.cloud,"+ // a target on the cluster
			"nonsense,"+ // no pair
			"=someone@example.net,"+ // no address
			"hello@gentian.cloud=,"+ // no target
			"bare=someone@example.net", // not an address
		hosted,
	)

	if got := len(byDomain["gentian.cloud"]); got != 1 {
		t.Fatalf("gentian.cloud aliases = %d, want 1: %+v", got, byDomain["gentian.cloud"])
	}
	if a := byDomain["gentian.cloud"][0]; a.address != "hello@gentian.cloud" || a.to != "someone@example.net" {
		t.Errorf("gentian.cloud alias = %+v, want hello@gentian.cloud -> someone@example.net", a)
	}
	if got := byDomain["finnor.gentian.cloud"]; len(got) != 1 || got[0].address != "sales@finnor.gentian.cloud" || got[0].to != "sales@example.net" {
		t.Errorf("finnor aliases = %+v, want the address lower-cased and the target as written", got)
	}
	if len(byDomain) != 2 {
		t.Errorf("domains with aliases = %v, want exactly the two hosted ones", byDomain)
	}

	wantRefused := []string{
		"does not accept mail for elsewhere.example",
		"the target is inside a domain this cluster hosts",
		`"nonsense" is not address=target`,
		`"=someone@example.net" is not address=target`,
		`"hello@gentian.cloud=" is not address=target`,
		"the address is not an e-mail address",
	}
	if len(refused) != len(wantRefused) {
		t.Fatalf("refused %d entries, want %d:\n%s", len(refused), len(wantRefused), strings.Join(refused, "\n"))
	}
	for i, want := range wantRefused {
		if !strings.Contains(refused[i], want) {
			t.Errorf("refused[%d] = %q, want it to say %q", i, refused[i], want)
		}
	}
}

func TestParseMailAliasesEmpty(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{"", " ", ",", " , "} {
		byDomain, refused := parseMailAliases(raw, map[string]bool{"gentian.cloud": true})
		if len(byDomain) != 0 || len(refused) != 0 {
			t.Errorf("parseMailAliases(%q) = %v, %v; want nothing and no complaint", raw, byDomain, refused)
		}
	}
}
