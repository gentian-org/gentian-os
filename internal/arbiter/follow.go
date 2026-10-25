/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package arbiter

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"time"
)

// Follow keeps the server on the operator's table. The kubelet swaps the
// mounted file when the operator changes the ConfigMap; every interval the
// file is read again, and a table that reads differently and parses is put
// in force, with the stores it says to purge deleted. One that does not
// parse leaves the last good one in force: the operator writes only good
// ones, and a torn read is not a reason to refuse every key.
func Follow(ctx context.Context, path string, interval time.Duration, s *Server, log *slog.Logger) {
	var last []byte
	apply := func() {
		raw, err := os.ReadFile(path)
		if err != nil {
			// Not written yet: no component declares a store, and every
			// key is refused, which is the table's own answer.
			return
		}
		if bytes.Equal(raw, last) {
			return
		}
		t, err := ParseTable(raw)
		if err != nil {
			log.Warn("the table was not reloaded", "error", err.Error())
			return
		}
		s.SetTable(t)
		purged := true
		for _, p := range t.Purge {
			if err := s.stores.Purge(ctx, p.Tenant, p.Component); err != nil {
				log.Error("a store was not purged; tried again with the next look", "store", p.Tenant+"/"+p.Component, "error", err.Error())
				purged = false
			}
		}
		// A purge that failed is tried again: the table is read as new
		// until every purge it asks for has gone through.
		if purged {
			last = raw
		}
		log.Info("table in force", "stores", len(t.Stores), "purge", len(t.Purge))
	}
	apply()
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			apply()
		}
	}
}
