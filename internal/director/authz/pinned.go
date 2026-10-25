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
	"time"
)

// pinRetry is how long Pinned waits between looks while the model it was
// built with is not in the store yet.
var pinRetry = 5 * time.Second

// pinPages bounds how far back Pinned searches the store's models. The one
// it wants is the newest or close to it; a handful of pages is plenty.
const pinPages = 5

// ErrNotPinned is a reader started without the store and model the operator
// recorded. It is a refusal to start, never a cue to look the store up: a
// store found by its name is whichever store carries the name.
var ErrNotPinned = errors.New("authz: no pinned authorization store; the operator records it in the ConfigMap " + PinConfigMap)

// Pinned checks the store and model the operator recorded, without writing
// either: for a reader such as the ext-auth bouncer, which must decide
// against what the operator established and never establish anything of its
// own. Both o.StoreID and o.ModelID must be set.
//
// The model returned is the stored one equal to the embedded model.json,
// which is the recorded one except across an upgrade or a rollback. On an
// upgrade a reader starts alongside the operator and may read the record
// before the operator has written the new model; deciding against the
// recorded one would leave it on the old model until restarted. On a
// rollback the recorded model is the one the rolled-back binary does not
// know. So the recorded model is taken when it is this binary's, and
// otherwise the pinned store is searched for this binary's model -- never
// another store -- and Pinned waits for it, up to ctx's deadline.
func Pinned(ctx context.Context, o Options) (storeID, modelID string, err error) {
	if o.StoreID == "" || o.ModelID == "" {
		return "", "", ErrNotPinned
	}
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
		modelID, err = c.pinnedOnce(ctx, o, want)
		if err == nil {
			return o.StoreID, modelID, nil
		}
		if !logged {
			c.log.InfoContext(ctx, "waiting for the operator to write the authorization model this service was built with",
				"store", o.StoreID, "reason", err.Error())
			logged = true
		}
		select {
		case <-ctx.Done():
			return "", "", fmt.Errorf("%w (gave up waiting: %v)", err, ctx.Err())
		case <-time.After(pinRetry):
		}
	}
}

func (c *OpenFGA) pinnedOnce(ctx context.Context, o Options, want map[string]any) (string, error) {
	var recorded map[string]any
	if err := c.get(ctx, "/stores/"+o.StoreID+"/authorization-models/"+url.PathEscape(o.ModelID), &recorded); err == nil {
		if m, ok := recorded["authorization_model"].(map[string]any); ok && sameModel(m, want) {
			return o.ModelID, nil
		}
	} else if !isNotFound(err) {
		return "", fmt.Errorf("read model %s: %w", o.ModelID, err)
	}
	// OpenFGA lists a store's models newest first.
	token := ""
	for range pinPages {
		var page struct {
			Models            []map[string]any `json:"authorization_models"`
			ContinuationToken string           `json:"continuation_token"`
		}
		path := "/stores/" + o.StoreID + "/authorization-models?page_size=50"
		if token != "" {
			path += "&continuation_token=" + url.QueryEscape(token)
		}
		if err := c.get(ctx, path, &page); err != nil {
			return "", fmt.Errorf("read models: %w", err)
		}
		for _, m := range page.Models {
			if sameModel(m, want) {
				id, _ := m["id"].(string)
				return id, nil
			}
		}
		if token = page.ContinuationToken; token == "" {
			break
		}
	}
	return "", fmt.Errorf("authz: store %s does not hold the model this service was built with yet; the operator writes it", o.StoreID)
}
