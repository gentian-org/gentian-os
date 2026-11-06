/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

// Package envvar reads a process's settings from its environment.
package envvar

import "os"

// Or is the environment variable name, or def when it is unset or empty.
// Empty and unset are treated alike deliberately: a Helm value rendered to ""
// is how an unset chart value reaches a container, and treating that as a
// deliberate empty string means a default that silently stops applying.
func Or(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}
