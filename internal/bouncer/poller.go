/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package bouncer

import (
	"context"
	"log/slog"
	"time"
)

// Poll follows the store's changelog and calls onChange for every batch of
// entries after the point where it started. OpenFGA has no push stream, so
// the interval is the stated bound on how long a revoked right survives
// (networking.md §4). It returns when ctx ends.
func Poll(ctx context.Context, store Store, interval time.Duration, log *slog.Logger, onChange func(n int)) {
	// Fast-forward to the end: what happened before this replica started is
	// already reflected in the store it will ask.
	token := ""
	for {
		changes, next, err := store.Changes(ctx, "", token)
		if err != nil {
			log.WarnContext(ctx, "changelog unreachable at start; polling from the beginning", "error", err.Error())
			break
		}
		if next == "" || next == token || len(changes) == 0 {
			if next != "" {
				token = next
			}
			break
		}
		token = next
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		for {
			changes, next, err := store.Changes(ctx, "", token)
			if err != nil {
				log.WarnContext(ctx, "changelog unreachable; cached decisions stand until they expire", "error", err.Error())
				break
			}
			if len(changes) > 0 {
				onChange(len(changes))
			}
			if next == "" || next == token {
				break
			}
			token = next
			if len(changes) == 0 {
				break
			}
		}
	}
}
