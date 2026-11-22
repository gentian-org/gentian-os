/*
Copyright The Gentian OS Authors.

This Source Code Form is subject to the terms of the Mozilla Public
License, v. 2.0. If a copy of the MPL was not distributed with this
file, You can obtain one at https://mozilla.org/MPL/2.0/.

SPDX-License-Identifier: MPL-2.0
*/

package openfga

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// A check reads the store as it is unless the caller asked for the fastest
// answer: OpenFGA's caches serve only a request that says so.
func TestACheckIsFreshUnlessAskedForTheFastestAnswer(t *testing.T) {
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Consistency string `json:"consistency"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		got = append(got, body.Consistency)
		if r.URL.Path == "/stores/s/batch-check" {
			_, _ = w.Write([]byte(`{"result":{"0":{"allowed":true}}}`))
			return
		}
		_, _ = w.Write([]byte(`{"allowed":true}`))
	}))
	defer srv.Close()
	c := New(srv.URL, "", srv.Client())
	ctx := context.Background()
	if _, err := c.Check(ctx, "s", "m", "user:a", "user", "app:t/x"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.BatchCheck(ctx, "s", "m", []Tuple{{User: "user:a", Relation: "user", Object: "app:t/x"}}); err != nil {
		t.Fatal(err)
	}
	fast := WithFastAnswer(ctx)
	if _, err := c.Check(fast, "s", "m", "user:a", "user", "app:t/x"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.BatchCheck(fast, "s", "m", []Tuple{{User: "user:a", Relation: "user", Object: "app:t/x"}}); err != nil {
		t.Fatal(err)
	}
	want := []string{"HIGHER_CONSISTENCY", "HIGHER_CONSISTENCY", "MINIMIZE_LATENCY", "MINIMIZE_LATENCY"}
	for i := range want {
		if i >= len(got) || got[i] != want[i] {
			t.Fatalf("consistency asked = %v, want %v", got, want)
		}
	}
}
