/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package authz

import (
	"context"
	"fmt"
)

// AppEntries reads what the store still holds on one app of tenant: who was
// given a role on it. An uninstalled app keeps these, so that installing it
// again opens it to the people it was open to; a purge removes them
// (RemoveAppEntries), or a later app of the same name would be handed to
// whoever had the old one.
func AppEntries(ctx context.Context, s Store, tenant, profile string) ([]Tuple, error) {
	if !ValidName(tenant) || !ValidName(profile) {
		return nil, fmt.Errorf("%w: %q", ErrInvalidID, tenant+"/"+profile)
	}
	object := App(tenant, profile)
	have, err := s.Read(ctx, Tuple{Object: object})
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", object, err)
	}
	out := make([]Tuple, 0, len(have))
	for _, t := range have {
		out = append(out, plain(t))
	}
	return out, nil
}

// RemoveAppEntries removes everything the store holds on one app of tenant
// and answers how many entries that was.
func RemoveAppEntries(ctx context.Context, s Store, tenant, profile string) (int, error) {
	entries, err := AppEntries(ctx, s, tenant, profile)
	if err != nil || len(entries) == 0 {
		return 0, err
	}
	if err := s.Write(ctx, nil, entries); err != nil {
		return 0, fmt.Errorf("remove what is held on %s: %w", App(tenant, profile), err)
	}
	return len(entries), nil
}
