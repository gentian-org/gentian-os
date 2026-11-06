/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package registrar

import (
	"fmt"
	"strings"
	"testing"

	"github.com/gentian-org/gentian-os/internal/componentkeys/keystest"
)

func vouchingEntry(tenant, component, hash string) string {
	return fmt.Sprintf(`{"tenant":%q,"component":%q,"keyHash":%q}`, tenant, component, hash)
}

func describeVouching(k *VouchingKey) string {
	if k == nil {
		return "none"
	}
	return k.Tenant + "/" + k.Component
}

// What the registrar does with the operator's list of vouching keys, held
// to testdata/vouching-keys.txt.
func TestVouchingKeysTranscript(t *testing.T) {
	var out []string
	for _, c := range keystest.Lists(vouchingEntry) {
		keys, err := ParseVouchingKeys([]byte(c[1]))
		out = append(out, fmt.Sprintf("parse %s: %s, %d entries", c[0], keystest.Error(err), len(keys)))
		held := &VouchingKeys{}
		held.Set(keys)
		out = append(out, fmt.Sprintf("  len %d", held.Len()))
		for _, k := range keystest.Keys {
			out = append(out, fmt.Sprintf("  find %q: %s", k, describeVouching(held.holder(k))))
		}
	}
	var none *VouchingKeys
	out = append(out, fmt.Sprintf("nil list: len %d, finds %s", none.Len(), describeVouching(none.holder("notary-key"))))
	unset := &VouchingKeys{}
	out = append(out, fmt.Sprintf("unset list: len %d, finds %s", unset.Len(), describeVouching(unset.holder("notary-key"))))

	held := &VouchingKeys{}
	out = append(out, keystest.Follow(t, held.Follow, vouchingEntry, func() string {
		found := []string{fmt.Sprintf("len %d", held.Len())}
		for _, k := range keystest.Keys {
			found = append(found, describeVouching(held.holder(k)))
		}
		return strings.Join(found, "; ")
	})...)
	keystest.Golden(t, "vouching-keys", out)
}
