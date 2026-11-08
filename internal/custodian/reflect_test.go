/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package custodian

import "reflect"

// fieldExists reports whether a struct has a field of the given name. Used to
// assert the ABSENCE of fields — the design constraints in this package are
// about what the types cannot hold.
func fieldExists(v any, name string) bool {
	t := reflect.TypeOf(v)
	if t.Kind() != reflect.Struct {
		return false
	}
	_, ok := t.FieldByName(name)
	return ok
}
