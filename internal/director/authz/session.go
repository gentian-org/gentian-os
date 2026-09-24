/*
Copyright 2026 Gentian Organization.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
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
// changes since, which is how the shim learns of a revocation or a membership
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
