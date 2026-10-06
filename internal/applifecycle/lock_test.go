/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package applifecycle

import (
	"sync"
	"testing"
	"time"
)

// Install, Uninstall and SetAddons must not interleave for one app: a reinstall
// that starts while a purge is still deleting loses its new volumes and secrets
// to the tail of the teardown.

func TestLockAppSerializesTheSameApp(t *testing.T) {
	t.Parallel()
	s := &Service{}

	held := make(chan struct{})
	release := s.lockApp("demo", "odoo-base-ce")
	defer func() {
		select {
		case <-held:
		default:
			release()
		}
	}()

	acquired := make(chan struct{})
	go func() {
		unlock := s.lockApp("demo", "odoo-base-ce")
		close(acquired)
		unlock()
	}()

	select {
	case <-acquired:
		t.Fatal("second caller acquired the lock while the first still held it")
	case <-time.After(50 * time.Millisecond):
	}

	release()
	close(held)

	select {
	case <-acquired:
	case <-time.After(2 * time.Second):
		t.Fatal("second caller never acquired the lock after release")
	}
}

func TestLockAppDoesNotSerializeDifferentApps(t *testing.T) {
	t.Parallel()
	s := &Service{}

	unlock := s.lockApp("demo", "odoo-base-ce")
	defer unlock()

	done := make(chan struct{})
	go func() {
		defer close(done)
		// Different profile, and the same profile in a different tenant: neither
		// shares state with the one held above, so neither may block.
		s.lockApp("demo", "nextcloud-base-ce")()
		s.lockApp("other", "odoo-base-ce")()
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("an unrelated app was blocked by this app's lock")
	}
}

func TestLockAppIsOneMutexPerKey(t *testing.T) {
	t.Parallel()
	s := &Service{}

	// LoadOrStore under contention must hand every caller the same mutex —
	// otherwise the lock silently stops serializing anything. Run with -race.
	const goroutines = 64
	var counter int
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for range goroutines {
		go func() {
			defer wg.Done()
			unlock := s.lockApp("demo", "odoo-base-ce")
			defer unlock()
			counter++
		}()
	}
	wg.Wait()

	if counter != goroutines {
		t.Fatalf("counter = %d, want %d", counter, goroutines)
	}
}
