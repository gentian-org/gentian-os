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
	"net/url"
	"time"
)

// Change is one entry of the store's changelog.
type Change struct {
	Tuple     Tuple
	Write     bool // false is a delete
	Timestamp time.Time
}

// Changes returns a page of the store's changelog for one object type, from
// the continuation token of the previous page, and the token for the next.
// The last page's token is what to poll with: OpenFGA answers it with the
// changes since, which is how the bouncer learns of a revocation or a membership
// change without a push stream (networking.md §4).
func (c *OpenFGA) Changes(ctx context.Context, objectType, token string) ([]Change, string, error) {
	q := url.Values{"page_size": {"100"}}
	if objectType != "" {
		q.Set("type", objectType)
	}
	if token != "" {
		q.Set("continuation_token", token)
	}
	var page struct {
		Changes []struct {
			Key       Tuple     `json:"tuple_key"`
			Operation string    `json:"operation"`
			Timestamp time.Time `json:"timestamp"`
		} `json:"changes"`
		ContinuationToken string `json:"continuation_token"`
	}
	if err := c.get(ctx, "/stores/"+c.storeID+"/changes?"+q.Encode(), &page); err != nil {
		return nil, "", err
	}
	out := make([]Change, 0, len(page.Changes))
	for _, ch := range page.Changes {
		out = append(out, Change{
			Tuple:     Tuple{User: ch.Key.User, Relation: ch.Key.Relation, Object: ch.Key.Object},
			Write:     ch.Operation == "TUPLE_OPERATION_WRITE",
			Timestamp: ch.Timestamp,
		})
	}
	return out, page.ContinuationToken, nil
}
