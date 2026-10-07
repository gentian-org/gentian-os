/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package schemacheck

import (
	"sync"
	"time"
)

// DefaultSourceTTL is how long the director believes an answer from the
// operator. Long enough that a burst of writes asks once, short enough that
// a definition repaired a moment ago stops refusing writes almost at once --
// and that a definition gone stale is acted on before much is written.
const DefaultSourceTTL = 15 * time.Second

// Cached remembers an answer for ttl. Only an answer is remembered: a failure
// to get one is asked again every time, so that the operator coming back is
// noticed by the next write and not after a wait.
func Cached(source Source, ttl time.Duration, now func() time.Time) Source {
	if now == nil {
		now = time.Now
	}
	var (
		mu    sync.Mutex
		held  Report
		until time.Time
	)
	return func() (Report, error) {
		mu.Lock()
		defer mu.Unlock()
		if now().Before(until) {
			return held, nil
		}
		report, err := source()
		if err != nil {
			return Report{}, err
		}
		held, until = report, now().Add(ttl)
		return report, nil
	}
}
