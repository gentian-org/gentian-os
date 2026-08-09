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
