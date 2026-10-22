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
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"time"
)

// lookupRetry is how long Lookup waits between looks while the model it was
// built with is not in the store yet.
var lookupRetry = 5 * time.Second

// lookupPages bounds how far back Lookup searches the store's models. The
// one it wants is the newest or close to it; a handful of pages is plenty.
const lookupPages = 5

// Lookup finds the store and the model this binary was built with, without
// writing either: for a reader such as the ext-auth bouncer, which must
// decide against what the operator established and never establish anything
// of its own.
//
// The model is the stored one equal to the embedded model.json, not the
// newest. On an upgrade a reader starts alongside the operator and may look
// before the operator has written the new model; taking the newest would
// leave it deciding against the old model until restarted. On a rollback the
// newest is the one the rolled-back binary does not know. Until the matching
// model is there Lookup waits, up to ctx's deadline.
//
// A store or model id set in o is a pin and is returned as given.
func Lookup(ctx context.Context, o Options) (storeID, modelID string, err error) {
	c, err := newClient(o)
	if err != nil {
		return "", "", err
	}
	var want map[string]any
	if err := json.Unmarshal(modelV1, &want); err != nil {
		return "", "", fmt.Errorf("embedded model: %w", err)
	}
	logged := false
	for {
		storeID, modelID, err = c.lookupOnce(ctx, o, want)
		if err == nil {
			return storeID, modelID, nil
		}
		if !logged {
			c.log.InfoContext(ctx, "waiting for the operator to write the authorization model this service was built with",
				"reason", err.Error())
			logged = true
		}
		select {
		case <-ctx.Done():
			return "", "", fmt.Errorf("%w (gave up waiting: %v)", err, ctx.Err())
		case <-time.After(lookupRetry):
		}
	}
}

func (c *OpenFGA) lookupOnce(ctx context.Context, o Options, want map[string]any) (string, string, error) {
	storeID := o.StoreID
	if storeID == "" {
		found, err := c.storesNamed(ctx, StoreName)
		if err != nil {
			return "", "", err
		}
		if len(found) == 0 {
			return "", "", errors.New("authz: no authorization store yet; the operator creates it")
		}
		sort.Slice(found, func(i, j int) bool { return found[i].CreatedAt.Before(found[j].CreatedAt) })
		storeID = found[0].ID
	}
	if o.ModelID != "" {
		return storeID, o.ModelID, nil
	}
	// OpenFGA lists a store's models newest first.
	token := ""
	for range lookupPages {
		var page struct {
			Models            []map[string]any `json:"authorization_models"`
			ContinuationToken string           `json:"continuation_token"`
		}
		path := "/stores/" + storeID + "/authorization-models?page_size=50"
		if token != "" {
			path += "&continuation_token=" + url.QueryEscape(token)
		}
		if err := c.get(ctx, path, &page); err != nil {
			return "", "", fmt.Errorf("read models: %w", err)
		}
		for _, m := range page.Models {
			if sameModel(m, want) {
				id, _ := m["id"].(string)
				return storeID, id, nil
			}
		}
		if token = page.ContinuationToken; token == "" {
			break
		}
	}
	return "", "", fmt.Errorf("authz: store %s does not hold the model this service was built with yet; the operator writes it", storeID)
}
