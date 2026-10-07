/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package api

import "time"

// SetPurgePoll shortens the purge watcher's interval for a test.
func SetPurgePoll(d time.Duration) (restore func()) {
	old := purgePoll
	purgePoll = d
	return func() { purgePoll = old }
}

// SetImportPoll shortens the import watcher's interval for a test.
func SetImportPoll(d time.Duration) (restore func()) {
	old := importPoll
	importPoll = d
	return func() { importPoll = old }
}

// SetResiduePoll shortens the wait between two requests to delete a profile
// that has left git.
func SetResiduePoll(d time.Duration) (restore func()) {
	old := residuePoll.Swap(int64(d))
	return func() { residuePoll.Store(old) }
}
