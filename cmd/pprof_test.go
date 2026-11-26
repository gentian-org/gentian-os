/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package main

import "testing"

func TestLoopbackOnly(t *testing.T) {
	for addr, ok := range map[string]bool{
		"":               true,
		"0":              true,
		"127.0.0.1:6060": true,
		"[::1]:6060":     true,
		"localhost:6060": true,
		":6060":          false,
		"0.0.0.0:6060":   false,
		"10.0.0.5:6060":  false,
		"6060":           false,
	} {
		if err := loopbackOnly(addr); (err == nil) != ok {
			t.Errorf("loopbackOnly(%q) = %v, want allowed=%v", addr, err, ok)
		}
	}
}
