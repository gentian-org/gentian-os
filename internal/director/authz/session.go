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

	"github.com/gentian-org/gentian-os/internal/openfga"
)

// Change is one entry of the store's changelog.
type Change = openfga.Change

// Changes returns a page of the store's changelog for one object type, from
// the continuation token of the previous page, and the token for the next.
// The last page's token is what to poll with: OpenFGA answers it with the
// changes since, which is how the bouncer learns of a revocation or a membership
// change without a push stream (networking.md §4).
func (c *OpenFGA) Changes(ctx context.Context, objectType, token string) ([]Change, string, error) {
	return c.fga.Changes(ctx, c.storeID, objectType, token)
}
