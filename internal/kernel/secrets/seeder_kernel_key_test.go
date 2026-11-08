/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package secrets_test

import (
	"context"
	"testing"

	"github.com/gentian-org/gentian-os/internal/kernel/secrets"
)

// A kernel key is made once and kept: a second seed returns the stored one.
func TestAKernelKeyIsMadeOnceAndKept(t *testing.T) {
	ctx := context.Background()
	w := &deafStore{data: map[string]map[string]string{}, failAfter: 1 << 30}
	s := secrets.NewSeeder(w, secrets.NewDeriver(goldenMaster, goldenSalt))
	first, err := s.SeedKernelKey(ctx, "authz", "openfga-apps", "preshared_key")
	if err != nil || len(first) < 32 {
		t.Fatalf("key = %q, %v", first, err)
	}
	again, err := s.SeedKernelKey(ctx, "authz", "openfga-apps", "preshared_key")
	if err != nil || again != first {
		t.Fatalf("second seed = %q, %v; want the stored %q", again, err, first)
	}
	if got, _ := w.Get(ctx, "gentian-os/kernel/authz/openfga-apps"); got["preshared_key"] != first {
		t.Fatalf("stored = %v", got)
	}
}
