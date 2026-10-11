/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package secrets

import (
	"context"
	"errors"
	"fmt"
)

// DerivedKeyPath is where the value of one key a profile declares under
// spec.secrets.derived is kept: below the app's own subtree, so it is purged
// with the app's other credentials, and one path per key, so a key a profile
// declares later is made without touching those that exist.
func DerivedKeyPath(tenant, app, key string) string {
	return fmt.Sprintf("gentian-os/tenants/%s/apps/%s/derived/%s", tenant, app, key)
}

// ErrNoMasterPassword says a value that has to be derived from the master
// password was not made, because the Seeder does not hold it.
var ErrNoMasterPassword = errors.New("the master password is not available")

// derivedKeyLength is the length of a declared key's value in hex characters:
// 256 bits, the most one derivation gives.
const derivedKeyLength = 64

// SeedDerivedKey returns the value of one key a profile declares under
// spec.secrets.derived, making and storing it the first time it is asked for.
//
// It follows the cluster's secret mode. In ModeDerived the value is computed
// from the master password and the salt, for this tenant, app and key; in
// ModeRandom it is drawn at random. Either way the first value stored at the
// path stays the path's value, and that one is returned.
//
// Unlike the other credentials, nothing stands in for a master password that
// is not there: in ModeDerived without one no value is made, and the error is
// ErrNoMasterPassword. A key that is already stored is returned whatever the
// Seeder holds.
func (s *Seeder) SeedDerivedKey(ctx context.Context, tenant, app, key string) (string, error) {
	path := DerivedKeyPath(tenant, app, key)
	fail := func(err error) (string, error) {
		return "", fmt.Errorf("seed derived key(%s/%s/%s): %w", tenant, app, key, err)
	}

	mode := ModeDerived
	if s.mode != nil {
		m, err := s.mode(ctx)
		if err != nil {
			return fail(fmt.Errorf("secret mode: %w", err))
		}
		mode = m
	}
	var value string
	switch mode {
	case ModeDerived:
		if s.d == nil || !s.d.HasMaster() {
			// What an earlier pass stored is still the key's value. A read
			// that fails and a path that holds nothing both mean there is
			// no value to hand on.
			if held, err := s.w.Get(ctx, path); err == nil && held["value"] != "" {
				return held["value"], nil
			}
			return fail(ErrNoMasterPassword)
		}
		value = s.d.Derive(path, "value", derivedKeyLength)
	case ModeRandom:
		value = randomHex(derivedKeyLength)
	default:
		return fail(fmt.Errorf("secret mode: %q is neither %q nor %q", mode, ModeDerived, ModeRandom))
	}

	if err := s.w.PutOnce(ctx, path, map[string]string{"value": value}); err != nil {
		return fail(err)
	}
	// Read back, and never hand on a value that was not: PutOnce leaves a
	// path alone that somebody else wrote in between.
	held, err := s.w.Get(ctx, path)
	if err != nil {
		return fail(fmt.Errorf("read back %s: %w", path, err))
	}
	if held["value"] == "" {
		return fail(fmt.Errorf("read back %s: nothing is stored", path))
	}
	return held["value"], nil
}
