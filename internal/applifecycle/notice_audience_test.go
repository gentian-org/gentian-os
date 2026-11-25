/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package applifecycle

import (
	"encoding/json"
	"strings"
	"testing"
)

// An audience that names groups is refused, not read as "everyone".
func TestANoticeAudienceNamingGroupsIsRefused(t *testing.T) {
	_, err := decodeAudience(json.RawMessage(`{"scope":"tenant","groups":["finance"]}`))
	if err == nil || !strings.Contains(err.Error(), "groups") {
		t.Fatalf("an audience naming groups gave %v, want a refusal naming groups", err)
	}
}

func TestANoticeAudienceByPositionsIsRead(t *testing.T) {
	a, err := decodeAudience(json.RawMessage(`{"scope":"tenant","positions":["finance"]}`))
	if err != nil || a == nil || len(a.Positions) != 1 || a.Positions[0] != "finance" {
		t.Fatalf("got %+v, %v; want positions [finance]", a, err)
	}
	if a, err := decodeAudience(nil); a != nil || err != nil {
		t.Fatalf("no audience gave %+v, %v; want none", a, err)
	}
}
