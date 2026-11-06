/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package api

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gentian-org/gentian-os/internal/componentkeys/keystest"
)

func commissionEntry(tenant, component, hash string) string {
	return fmt.Sprintf(`{"tenant":%q,"component":%q,"keyHash":%q,"tenants":["demo","solo"]}`, tenant, component, hash)
}

func describeCommission(k *CommissionKey) string {
	if k == nil {
		return "none"
	}
	return fmt.Sprintf("%s/%s may demo=%v other=%v", k.Tenant, k.Component, slices.Contains(k.Tenants, "demo"), slices.Contains(k.Tenants, "other"))
}

// What the director does with the operator's list of commission keys,
// held to testdata/commission-keys.txt.
func TestCommissionKeysTranscript(t *testing.T) {
	var out []string
	for _, c := range keystest.Lists(commissionEntry) {
		keys, err := ParseCommissionKeys([]byte(c[1]))
		out = append(out, fmt.Sprintf("parse %s: %s, %d entries", c[0], keystest.Error(err), len(keys)))
		held := &CommissionKeys{}
		held.Set(keys)
		for _, k := range keystest.Keys {
			out = append(out, fmt.Sprintf("  find %q: %s", k, describeCommission(held.Find(k))))
		}
	}
	var none *CommissionKeys
	out = append(out, "nil list finds: "+describeCommission(none.Find("notary-key")))
	out = append(out, "unset list finds: "+describeCommission((&CommissionKeys{}).Find("notary-key")))

	held := &CommissionKeys{}
	out = append(out, keystest.Follow(t, func(ctx context.Context, path string, every time.Duration, log *slog.Logger) {
		held.Follow(ctx, path, every, log, CommissionKeysList)
	}, commissionEntry, func() string {
		var found []string
		for _, k := range keystest.Keys {
			found = append(found, describeCommission(held.Find(k)))
		}
		return strings.Join(found, "; ")
	})...)
	keystest.Golden(t, "commission-keys", out)
}
