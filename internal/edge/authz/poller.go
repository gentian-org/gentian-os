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
