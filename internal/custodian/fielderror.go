/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package custodian

import "strings"

// FieldError attributes one validation failure to a field of the credential's
// declared schema, so a form can render it against the field an operator
// needs to fix instead of a message they have to map back themselves.
//
// Field is empty for a failure that does not belong to one field — an
// unreachable endpoint, for instance, is a configuration problem rather than
// something typed wrong.
type FieldError struct {
	Field   string `json:"field"`
	Message string `json:"message"`
}

func (e FieldError) Error() string { return e.Field + ": " + e.Message }

// FieldErrors is one or more FieldError, collected rather than returned on
// the first failure so a form can flag every offending field from a single
// submission instead of one at a time.
type FieldErrors []FieldError

func (e FieldErrors) Error() string {
	msgs := make([]string, len(e))
	for i, fe := range e {
		msgs[i] = fe.Error()
	}
	return strings.Join(msgs, "; ")
}
