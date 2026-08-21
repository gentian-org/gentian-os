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

package credentialmgr

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
