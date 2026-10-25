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
	"fmt"
	"log/slog"
	"sync"
)

// StoreIDs is what the arbiter recorded for one store: the store it created
// and the model it last wrote there, with the digest of the compiled model
// that model was written from.
type StoreIDs struct {
	Store  string `json:"store"`
	Model  string `json:"model,omitempty"`
	Digest string `json:"digest,omitempty"`
}

// Recorder keeps the ids the arbiter created, by the key of the store's
// entry (<tenant>.<component>). A store is never looked up by its name: the
// record is the only way back to it.
type Recorder interface {
	// Load is every record.
	Load(ctx context.Context) (map[string]StoreIDs, error)
	// Claim records a store created for key unless one is recorded already,
	// and returns the store that is recorded now. Two arbiters that created
	// a store for the same entry at once agree on one this way; the other
	// deletes its own.
	Claim(ctx context.Context, key string, ids StoreIDs) (StoreIDs, error)
	// Save records ids for key, over whatever was recorded.
	Save(ctx context.Context, key string, ids StoreIDs) error
	// Forget removes the record of key.
	Forget(ctx context.Context, key string) error
}

// Stores resolves an entry of the table to its store and model, creating
// the store on first sight and writing a new model when the entry's model
// is not the one written last.
type Stores struct {
	fga *openFGA
	rec Recorder
	log *slog.Logger

	mu     sync.Mutex
	loaded bool
	ids    map[string]StoreIDs
	locks  map[string]*sync.Mutex
}

func NewStores(fga *openFGA, rec Recorder, log *slog.Logger) *Stores {
	return &Stores{fga: fga, rec: rec, log: log, ids: map[string]StoreIDs{}, locks: map[string]*sync.Mutex{}}
}

// lock is the lock of one entry: two calls for one store that has none yet
// must not create two.
func (s *Stores) lock(ctx context.Context, key string) (func(), error) {
	s.mu.Lock()
	if !s.loaded {
		ids, err := s.rec.Load(ctx)
		if err != nil {
			s.mu.Unlock()
			return nil, fmt.Errorf("read the recorded stores: %w", err)
		}
		s.ids, s.loaded = ids, true
	}
	l, ok := s.locks[key]
	if !ok {
		l = &sync.Mutex{}
		s.locks[key] = l
	}
	s.mu.Unlock()
	l.Lock()
	return l.Unlock, nil
}

func (s *Stores) get(key string) (StoreIDs, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ids, ok := s.ids[key]
	return ids, ok
}

func (s *Stores) set(key string, ids StoreIDs) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ids[key] = ids
}

// Resolve is the store and model an entry is decided in. The store is
// created the first time the entry is seen, and recorded before anything is
// written to it; the model is written when the entry's compiled model has
// another digest than the one written last, and the tuples stay.
func (s *Stores) Resolve(ctx context.Context, e Entry) (StoreIDs, error) {
	key := e.recordKey()
	unlock, err := s.lock(ctx, key)
	if err != nil {
		return StoreIDs{}, err
	}
	defer unlock()

	ids, ok := s.get(key)
	if !ok {
		created, err := s.fga.createStore(ctx, e.StoreName())
		if err != nil {
			return StoreIDs{}, fmt.Errorf("create the store %s: %w", e.StoreName(), err)
		}
		ids, err = s.rec.Claim(ctx, key, StoreIDs{Store: created})
		if err != nil {
			// Not recorded is not created: a store nobody can find again
			// is removed rather than left to be made a second time.
			_ = s.fga.deleteStore(ctx, created)
			return StoreIDs{}, fmt.Errorf("record the store %s: %w", e.StoreName(), err)
		}
		if ids.Store != created {
			_ = s.fga.deleteStore(ctx, created)
		} else {
			s.log.InfoContext(ctx, "authorization store created", "store", e.StoreName(), "id", created)
		}
		s.set(key, ids)
	}

	digest := ModelDigest(e.Model)
	if ids.Model != "" && ids.Digest == digest {
		return ids, nil
	}
	model, err := s.fga.writeModel(ctx, ids.Store, e.Model)
	if err != nil {
		return StoreIDs{}, fmt.Errorf("write the model of %s: %w", e.StoreName(), err)
	}
	ids.Model, ids.Digest = model, digest
	if err := s.rec.Save(ctx, key, ids); err != nil {
		return StoreIDs{}, fmt.Errorf("record the model of %s: %w", e.StoreName(), err)
	}
	s.set(key, ids)
	s.log.InfoContext(ctx, "authorization model written", "store", e.StoreName(), "model", model)
	return ids, nil
}

// Purge deletes the store of tenant's component, with every tuple and model
// in it, and its record. A store that was never made is purged already.
func (s *Stores) Purge(ctx context.Context, tenant, component string) error {
	key := Entry{Tenant: tenant, Component: component}.recordKey()
	unlock, err := s.lock(ctx, key)
	if err != nil {
		return err
	}
	defer unlock()
	ids, ok := s.get(key)
	if !ok {
		return nil
	}
	if err := s.fga.deleteStore(ctx, ids.Store); err != nil {
		return fmt.Errorf("delete the store %s/%s: %w", tenant, component, err)
	}
	if err := s.rec.Forget(ctx, key); err != nil {
		return fmt.Errorf("forget the store %s/%s: %w", tenant, component, err)
	}
	s.mu.Lock()
	delete(s.ids, key)
	s.mu.Unlock()
	s.log.InfoContext(ctx, "authorization store purged", "store", tenant+"/"+component, "id", ids.Store)
	return nil
}

// Recorded is whether a store is recorded for tenant's component.
func (s *Stores) Recorded(ctx context.Context, tenant, component string) (bool, error) {
	key := Entry{Tenant: tenant, Component: component}.recordKey()
	unlock, err := s.lock(ctx, key)
	if err != nil {
		return false, err
	}
	defer unlock()
	_, ok := s.get(key)
	return ok, nil
}

// encodeIDs and decodeIDs are the record's form in a ConfigMap value.
func encodeIDs(ids StoreIDs) (string, error) {
	raw, err := json.Marshal(ids)
	return string(raw), err
}

func decodeIDs(v string) (StoreIDs, error) {
	var ids StoreIDs
	err := json.Unmarshal([]byte(v), &ids)
	if err == nil && ids.Store == "" {
		err = fmt.Errorf("a record without a store")
	}
	return ids, err
}
