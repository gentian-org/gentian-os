/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package authz

import "strings"

// UserSubject formats an OpenFGA user id (user:<id>).
func UserSubject(id string) string {
	id = strings.TrimSpace(id)
	if id == "" {
		return "user:"
	}
	if strings.Contains(id, ":") {
		return id
	}
	return "user:" + id
}

// ObjectRef formats an OpenFGA object reference (type:id).
func ObjectRef(objectType, id string) string {
	return objectType + ":" + id
}
