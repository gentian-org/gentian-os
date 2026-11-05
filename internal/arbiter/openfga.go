/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package arbiter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/gentian-org/gentian-os/internal/openfga"
)

// openFGA is the app instance of OpenFGA. The arbiter is the only holder of
// its credential, and it names a store in every path from the key a caller
// presented, never from anything the caller sent.
type openFGA struct {
	*openfga.Client
}

// NewOpenFGA is a client of the app instance at baseURL, with its preshared
// key.
func NewOpenFGA(baseURL, token string) *openFGA {
	return &openFGA{openfga.New(baseURL, token, &http.Client{Timeout: 15 * time.Second})}
}

// fgaTuple is a tuple as OpenFGA's API spells it.
type fgaTuple = openfga.Tuple

func toFGA(t Tuple) fgaTuple { return fgaTuple(t) }

// createStore makes a store and returns its id.
func (c *openFGA) createStore(ctx context.Context, name string) (string, error) {
	id, err := c.CreateStore(ctx, name)
	if err != nil {
		return "", err
	}
	if id == "" {
		return "", fmt.Errorf("openfga created store %q without an id", name)
	}
	return id, nil
}

// deleteStore removes a store with every tuple and model in it. A store
// that is not there is gone already.
func (c *openFGA) deleteStore(ctx context.Context, id string) error {
	err := c.DeleteStore(ctx, id)
	if openfga.IsStatus(err, http.StatusNotFound) {
		return nil
	}
	return err
}

// writeModel writes a model to a store and returns its id. Models are
// immutable and versioned: every write is a new one, and the store's tuples
// stay.
func (c *openFGA) writeModel(ctx context.Context, storeID string, model json.RawMessage) (string, error) {
	id, err := c.WriteModel(ctx, storeID, model)
	if err != nil {
		return "", err
	}
	if id == "" {
		return "", errors.New("openfga wrote a model without an id")
	}
	return id, nil
}

// writeTuples writes and deletes tuples for the platform, in transactions of
// at most MaxWriteTuples, the way the platform's own writes are made: a tuple
// that is there already is not an error, nor is one that is gone already. A
// component's own writes are not made this way (Server.write): a component
// is told what OpenFGA says.
func (c *openFGA) writeTuples(ctx context.Context, storeID, model string, writes, deletes []Tuple) error {
	return c.Write(ctx, storeID, fgaTuples(writes), fgaTuples(deletes),
		openfga.WriteOptions{Model: model, OnDuplicate: "ignore", OnMissing: "ignore"})
}

func fgaTuples(tuples []Tuple) []fgaTuple {
	out := make([]fgaTuple, 0, len(tuples))
	for _, t := range tuples {
		out = append(out, toFGA(t))
	}
	return out
}

// readAll is every tuple of a store matching a partial tuple, all pages.
func (c *openFGA) readAll(ctx context.Context, storeID string, filter fgaTuple) ([]Tuple, error) {
	o := openfga.ReadOptions{PageSize: MaxPageSize, StopAtEmptyPage: true}
	if filter != (fgaTuple{}) {
		o.Filter = &filter
	}
	got, err := c.Read(ctx, storeID, o)
	if err != nil {
		return nil, err
	}
	var out []Tuple
	for _, t := range got {
		out = append(out, Tuple(t))
	}
	return out, nil
}

// readModel is one model of a store, as OpenFGA holds it.
func (c *openFGA) readModel(ctx context.Context, storeID, model string) (json.RawMessage, error) {
	return c.ReadModel(ctx, storeID, model)
}

// storePath is a path under one store.
func storePath(storeID, rest string) string { return "/stores/" + storeID + rest }
