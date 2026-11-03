/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package usage

import "testing"

// A stored sample's quantities are read as they were written, and a column
// that does not decode is an error rather than a sample of zero usage.
func TestASampleThatDoesNotDecodeIsAnError(t *testing.T) {
	good := []byte(`{"requests.cpu":"2"}`)
	for _, tc := range []struct {
		name               string
		hard, used, actual []byte
		ok                 bool
	}{
		{"as written", good, good, good, true},
		{"no live reading", good, good, nil, true},
		{"corrupt ceiling", []byte(`{"requests.cpu":`), good, nil, false},
		{"corrupt usage", good, []byte(`not json`), nil, false},
		{"corrupt live reading", good, good, []byte(`[1,2]`), false},
	} {
		var s Sample
		err := decodeQuantities(&s, tc.hard, tc.used, tc.actual)
		if (err == nil) != tc.ok {
			t.Errorf("%s: err = %v, want ok = %v", tc.name, err, tc.ok)
			continue
		}
		if tc.ok && (s.Hard["requests.cpu"] != "2" || s.Used["requests.cpu"] != "2") {
			t.Errorf("%s: decoded %+v", tc.name, s)
		}
	}
}
