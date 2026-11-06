/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package arbiter

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/gentian-org/gentian-os/internal/componentkeys/keystest"
)

func storeEntry(tenant, component, hash string) string {
	return fmt.Sprintf(`{"tenant":%q,"component":%q,"keyHash":%q,"model":{"schema_version":"1.1"},"realm":"r"}`, tenant, component, hash)
}

// What the arbiter reads of the operator's table, and the table as it is
// written again, held to testdata/table.txt.
func TestTableTranscript(t *testing.T) {
	var out []string
	for _, c := range keystest.Lists(storeEntry) {
		table, err := ParseTable([]byte(strings.Replace(c[1], `{"keys":`, `{"stores":`, 1)))
		out = append(out, fmt.Sprintf("parse %s: %s", c[0], keystest.Error(err)))
		if table != nil {
			raw, err := json.Marshal(table)
			if err != nil {
				t.Fatal(err)
			}
			out = append(out, "  "+string(raw))
		}
	}
	keystest.Golden(t, "table", out)
}
