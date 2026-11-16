/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package authz_test

import (
	"context"
	"testing"

	"github.com/gentian-org/gentian-os/internal/director/authz"
	"github.com/gentian-org/gentian-os/internal/director/directortest"
)

// A batch says what the same questions asked one at a time say. Under
// `make test-director-contract` the batch is OpenFGA's own batch-check
// against model v1 and its fixture; with the table it is the one-at-a-time
// fallback.
func TestABatchAnswersAsSingleChecksDo(t *testing.T) {
	cluster := authz.Cluster(directortest.Cluster)
	facts := directortest.Table{
		"user:gentian/sam security_officer " + cluster:  true,
		"user:gentian/sam can_approve " + cluster:       true,
		"user:gentian/bea break_glass " + cluster:       true,
		"user:gentian/bea can_set_admission " + cluster: true,
	}
	c := directortest.Checker(t, facts)
	var qs []authz.Question
	for _, u := range []string{"user:gentian/sam", "user:gentian/bea", "user:gentian/nobody"} {
		for _, rel := range []string{"security_officer", "can_approve", "break_glass", "can_set_admission"} {
			qs = append(qs, authz.Question{User: u, Relation: rel, Object: cluster})
		}
	}
	ctx := context.Background()
	got, err := authz.CheckAll(ctx, c, "contract", qs)
	if err != nil {
		t.Fatal(err)
	}
	allowed := 0
	for i, q := range qs {
		want, err := c.Check(ctx, "contract", q.User, q.Relation, q.Object)
		if err != nil {
			t.Fatal(err)
		}
		if got[i] != want {
			t.Errorf("%s %s %s: batch %v, single %v", q.User, q.Relation, q.Object, got[i], want)
		}
		if want {
			allowed++
		}
	}
	if allowed == 0 || allowed == len(qs) {
		t.Errorf("%d of %d allowed: the questions do not tell a batch from a constant", allowed, len(qs))
	}
}
