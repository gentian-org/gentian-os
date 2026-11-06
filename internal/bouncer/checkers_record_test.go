/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package bouncer

import (
	"fmt"
	"strings"
	"testing"

	"github.com/gentian-org/gentian-os/internal/componentkeys"
	"github.com/gentian-org/gentian-os/internal/componentkeys/keystest"
)

func checkerEntry(tenant, component, hash string) string {
	return fmt.Sprintf(`{"tenant":%q,"component":%q,"keyHash":%q}`, tenant, component, hash)
}

func describeChecker(c *Checker) string {
	if c == nil {
		return "none"
	}
	if c.Audit != nil {
		return fmt.Sprintf("%s/%s audits %s/%s", c.Tenant, c.Component, c.Audit.Cluster, c.Audit.Realm)
	}
	return c.Tenant + "/" + c.Component
}

// What the bouncer does with the checkers of the operator's route table,
// held to testdata/checkers.txt.
func TestCheckersTranscript(t *testing.T) {
	audited := func(audit string) string {
		return `{"routes":[],"checkers":[{"tenant":"platform","component":"evidence","keyHash":"` + componentkeys.Hash("notary-key") + `","audit":` + audit + `}]}`
	}
	cases := [][2]string{}
	for _, c := range keystest.Lists(checkerEntry) {
		cases = append(cases, [2]string{c[0], strings.Replace(c[1], `{"keys":`, `{"routes":[],"checkers":`, 1)})
	}
	cases = append(cases,
		[2]string{"an audit", audited(`{"cluster":"main","realm":"kernel"}`)},
		[2]string{"an audit without a cluster", audited(`{"realm":"kernel"}`)},
		[2]string{"an audit with a slash in the cluster", audited(`{"cluster":"ma/in","realm":"kernel"}`)},
		[2]string{"an audit without a realm", audited(`{"cluster":"main"}`)},
		[2]string{"a shared key and a bad audit", `{"routes":[],"checkers":[` + checkerEntry("demo", "notary", componentkeys.Hash("a")) +
			`,{"tenant":"platform","component":"evidence","keyHash":"` + componentkeys.Hash("a") + `","audit":{}}]}`},
	)
	var out []string
	for _, c := range cases {
		table, err := ParseTable([]byte(c[1]))
		n := 0
		if table != nil {
			n = len(table.Checkers)
		}
		out = append(out, fmt.Sprintf("parse %s: %s, %d checkers", c[0], keystest.Error(err), n))
		for _, k := range keystest.Keys {
			out = append(out, fmt.Sprintf("  find %q: %s", k, describeChecker(table.checker(k))))
		}
	}
	keystest.Golden(t, "checkers", out)
}
