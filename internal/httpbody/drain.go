/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

// Package httpbody holds what a handler that refuses a streamed request body
// part-way needs to get its answer to the sender.
package httpbody

import (
	"io"
	"net/http"
	"time"
)

// DrainWindow is how long Drain reads what is left of a refused body: long
// enough for a bundle sent inside the cluster to finish arriving, short
// enough that a sender that keeps sending cannot hold the handler.
const DrainWindow = 10 * time.Minute

// Drain reads and discards what is left of r's body, for at most window,
// before the handler answers.
//
// A handler that answers before the body has ended is not heard: Go's server
// reads at most 256 KiB of what is left and then closes the connection, and a
// sender still writing the body -- every HTTP/1 client sends the whole body
// before it reads the answer -- sees a connection reset instead of the
// status and the reason. Drained, the body ends, and the answer is read.
//
// It returns how many bytes it discarded and whether the body ended within
// the window; past it, the answer is written anyway and is likely lost.
func Drain(w http.ResponseWriter, r *http.Request, window time.Duration) (int64, bool) {
	rc := http.NewResponseController(w)
	_ = rc.SetReadDeadline(time.Now().Add(window))
	n, err := io.Copy(io.Discard, r.Body)
	_ = rc.SetReadDeadline(time.Time{})
	return n, err == nil
}
